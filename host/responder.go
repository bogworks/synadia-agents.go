// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
)

// DefaultListenProbeTimeout is how long [Responder.CallerListening] waits for
// a no-responders answer when no timeout is given.
const DefaultListenProbeTimeout = 500 * time.Millisecond

// Responder writes the reply stream of one prompt (SAP §6). The host owns the
// ack, the keep-alives, the error frame and the terminator: the handler sends
// content and returns. It serializes its sends and is safe for concurrent use;
// once the terminator is out every call returns [ErrStreamClosed], so nothing
// is ever published after it (SAP §6.5).
type Responder struct {
	host *Host
	req  micro.Request

	mu     sync.Mutex
	done   bool
	kaStop chan struct{}
	kaDone chan struct{}
}

func newResponder(h *Host, req micro.Request) *Responder {
	return &Responder{host: h, req: req, kaStop: make(chan struct{})}
}

// Send publishes a response chunk with the given text, in the bare-string
// form of SAP §6.3.
func (w *Responder) Send(ctx context.Context, text string) error {
	return w.SendChunk(ctx, agents.ResponseChunk{Text: text})
}

// SendAttachments publishes a response chunk with text and attachments, in the
// object form of SAP §6.3. The attachments are checked as SAP §5.2 requires.
func (w *Responder) SendAttachments(ctx context.Context, text string, atts ...agents.Attachment) error {
	for i, a := range atts {
		if err := a.Validate(); err != nil {
			var ae *agents.AttachmentError
			if errors.As(err, &ae) {
				ae.Index = i
			}
			return err
		}
	}
	return w.SendChunk(ctx, agents.ResponseChunk{Text: text, Attachments: atts})
}

// SendChunk publishes one chunk. A status chunk is refused with
// [ErrStatusChunk]: the host sends the ack and the keep-alives (SAP §6.4). A
// chunk too large for the connection fails with nats.ErrMaxPayload. It returns
// ctx.Err() when ctx is done and [ErrStreamClosed] after the terminator.
func (w *Responder) SendChunk(ctx context.Context, c agents.Chunk) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := c.(agents.StatusChunk); ok {
		return ErrStatusChunk
	}
	data, err := agents.EncodeChunk(c)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.publishLocked(data)
}

func (w *Responder) publishLocked(data []byte) error {
	if w.done {
		return ErrStreamClosed
	}
	return w.req.Respond(data)
}

// ack publishes the leading ack (SAP §6.4).
func (w *Responder) ack() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.publishLocked(agents.AckChunk())
}

// startKeepalive sends a keep-alive ack every d until the stream ends.
func (w *Responder) startKeepalive(d time.Duration) {
	w.kaDone = make(chan struct{})
	go func() {
		defer close(w.kaDone)
		t := time.NewTicker(d)
		defer t.Stop()
		for {
			select {
			case <-w.kaStop:
				return
			case <-t.C:
				w.mu.Lock()
				if w.done {
					w.mu.Unlock()
					return
				}
				_ = w.req.Respond(agents.AckChunk())
				w.mu.Unlock()
			}
		}
	}()
}

// sendError publishes an error-headered message (SAP §9.1) when the stream is
// still open. The terminator follows from finish.
func (w *Responder) sendError(se *agents.ServiceError) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done {
		return
	}
	w.host.noteError(se)
	body, err := se.Payload()
	if err != nil {
		body = nil
	}
	_ = w.req.Respond(body, micro.WithHeaders(se.Headers()))
}

// finish publishes the terminator once and stops the keep-alive.
func (w *Responder) finish() {
	w.mu.Lock()
	if w.done {
		w.mu.Unlock()
		return
	}
	w.done = true
	close(w.kaStop)
	_ = w.req.Respond(nil)
	w.mu.Unlock()
	if w.kaDone != nil {
		<-w.kaDone
	}
}

// AskOption configures [Responder.Ask].
type AskOption func(*askConfig)

type askConfig struct{ attachments []agents.Attachment }

// WithAskAttachments adds attachments to the query chunk, merged after those
// of the envelope. See SAP §7.1.
func WithAskAttachments(a ...agents.Attachment) AskOption {
	return func(c *askConfig) { c.attachments = append(c.attachments, a...) }
}

// Ask puts a question to the caller in the middle of the stream and waits for
// one reply (SAP §7). It publishes a query chunk with a fresh reply subject,
// subscribed and flushed first, and returns the reply as an envelope (SAP
// §7.2). The stream stays open meanwhile, and several asks may be in flight,
// each with its own reply subject (SAP §7.3).
//
// It returns [ErrQueryTimeout] after timeout and ctx.Err() when ctx ends first;
// the handler then decides whether to end the stream with an error or go on
// with a default (SAP §7.3). It does not end the stream itself.
func (w *Responder) Ask(ctx context.Context, prompt string, timeout time.Duration, opts ...AskOption) (agents.Envelope, error) {
	return w.AskEnvelope(ctx, agents.Envelope{Prompt: prompt}, timeout, opts...)
}

// AskEnvelope is [Responder.Ask] with an envelope: its prompt and attachments
// form the query, and unknown fields are not sent.
func (w *Responder) AskEnvelope(ctx context.Context, e agents.Envelope, timeout time.Duration, opts ...AskOption) (agents.Envelope, error) {
	if timeout <= 0 {
		return agents.Envelope{}, fmt.Errorf("%w: ask timeout must be positive", ErrConfig)
	}
	var cfg askConfig
	for _, o := range opts {
		o(&cfg)
	}
	q := agents.QueryChunk{
		ID:           newUUID(),
		ReplySubject: newInbox(),
		Prompt:       e.Prompt,
		Attachments:  append(append([]agents.Attachment(nil), e.Attachments...), cfg.attachments...),
	}
	if len(q.Attachments) == 0 {
		q.Attachments = nil
	}
	if err := (agents.Envelope{Prompt: q.Prompt, Attachments: q.Attachments}).Validate(); err != nil {
		return agents.Envelope{}, err
	}
	data, err := agents.EncodeChunk(q)
	if err != nil {
		return agents.Envelope{}, err
	}
	if err := ctx.Err(); err != nil {
		return agents.Envelope{}, err
	}

	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	nc := w.host.nc
	sub, err := nc.SubscribeSync(q.ReplySubject)
	if err != nil {
		return agents.Envelope{}, err
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := sub.AutoUnsubscribe(1); err != nil {
		return agents.Envelope{}, err
	}
	if err := nc.FlushWithContext(tctx); err != nil {
		return agents.Envelope{}, askError(ctx, err)
	}
	w.mu.Lock()
	err = w.publishLocked(data)
	w.mu.Unlock()
	if err != nil {
		return agents.Envelope{}, err
	}
	m, err := sub.NextMsgWithContext(tctx)
	if err != nil {
		return agents.Envelope{}, askError(ctx, err)
	}
	reply, err := agents.DecodeEnvelope(m.Data)
	if err != nil {
		return agents.Envelope{}, fmt.Errorf("host: query reply: %w", err)
	}
	return reply, nil
}

func askError(parent context.Context, err error) error {
	if perr := parent.Err(); perr != nil {
		return perr
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, nats.ErrTimeout) {
		return ErrQueryTimeout
	}
	return err
}

// CallerListening probes whether the caller still listens on the reply
// subject, as the TypeScript host does: it publishes one keep-alive ack
// carrying a reply subject of its own, and when nobody subscribes to the
// caller's reply subject the server answers that subject with a no-responders
// status at once. It returns false then, and true after timeout (default
// [DefaultListenProbeTimeout]) without an answer. A best guess, not a promise:
// a caller that canceled its stream but kept its connection's shared inbox
// still looks present. SAP defines no cancellation (§6.7).
func (w *Responder) CallerListening(ctx context.Context, timeout time.Duration) (bool, error) {
	if timeout <= 0 {
		timeout = DefaultListenProbeTimeout
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	nc := w.host.nc
	probe := newInbox()
	sub, err := nc.SubscribeSync(probe)
	if err != nil {
		return false, err
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := sub.AutoUnsubscribe(1); err != nil {
		return false, err
	}
	if err := nc.FlushWithContext(tctx); err != nil {
		return true, askErrorListening(ctx, err)
	}
	w.mu.Lock()
	if w.done {
		w.mu.Unlock()
		return false, ErrStreamClosed
	}
	err = nc.PublishMsg(&nats.Msg{Subject: w.req.Reply(), Reply: probe, Data: agents.AckChunk()})
	w.mu.Unlock()
	if err != nil {
		return false, nil //nolint:nilerr // a connection that cannot publish reaches nobody
	}
	m, err := sub.NextMsgWithContext(tctx)
	if err != nil {
		if perr := ctx.Err(); perr != nil {
			return true, perr
		}
		if errors.Is(err, nats.ErrNoResponders) {
			return false, nil
		}
		return true, nil
	}
	if len(m.Data) == 0 && m.Header.Get("Status") == "503" {
		return false, nil
	}
	return true, nil
}

func askErrorListening(parent context.Context, err error) error {
	if perr := parent.Err(); perr != nil {
		return perr
	}
	return err
}
