// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/nats-io/nats.go"
)

// Event is one item of a response stream: a [Response], a [Status] or a
// *[Query]. The set is closed. Terminators, unknown chunk types and unknown
// status values are consumed by the SDK and not surfaced (SAP §6.2, §6.4).
type Event interface {
	// Header returns a copy of the headers the message arrived with: SAP §9
	// has callers inspect them on every message. Changing the copy changes
	// nothing in the SDK. See REQ-016.
	Header() nats.Header
	isEvent()
}

type meta struct{ hdr nats.Header }

func (m meta) Header() nats.Header { return cloneHeader(m.hdr) }
func (meta) isEvent()              {}

// Response is a response chunk: content from the agent, in either wire form
// (a bare string or {text, attachments}). A program concatenates Text in the
// order the events arrive. See SAP §6.3.
type Response struct {
	meta
	Text string
	// Attachments are the files the agent attached. A caller MAY ignore them.
	Attachments []agents.Attachment
	// Extra holds unknown fields of the data object, verbatim (SAP §6.6).
	Extra map[string]json.RawMessage
}

// SaveAttachments writes the response's attachments into dir with
// [agents.SaveAttachments], which sanitizes every file name and never
// overwrites or follows an existing file. It is a convenience; see
// [agents.SaveOptions].
func (r Response) SaveAttachments(dir string, opts agents.SaveOptions) ([]agents.SavedAttachment, error) {
	return agents.SaveAttachments(r.Attachments, dir, opts)
}

// Status is a status chunk. The SDK surfaces only the one value SAP defines,
// [agents.StatusAck]; every other value is silently ignored. The first message
// of a healthy stream is the ack, which also resets the inactivity timeout.
// See SAP §6.4.
type Status struct {
	meta
	// Value is [agents.StatusAck].
	Value string
}

// Query is a mid-stream question from the agent (SAP §7.1). The program answers
// it exactly once, with [Query.Reply] or [Query.ReplyEnvelope], from any
// goroutine. The stream stays open while it waits; the agent chooses its own
// reply timeout and the caller receives no signal when it passes (SAP §7.3).
type Query struct {
	meta
	// ID is the opaque correlation identifier.
	ID string
	// ReplySubject is the subject the one reply is published to.
	ReplySubject string
	// Prompt is the question.
	Prompt string
	// Attachments are the files that came with the question.
	Attachments []agents.Attachment
	// Extra holds unknown fields of the data object, verbatim.
	Extra map[string]json.RawMessage

	c       *Client
	policy  func(string) error // nil: the client's policy
	mu      sync.Mutex
	replied bool
}

// SaveAttachments writes the query's attachments into dir; see
// [Response.SaveAttachments].
func (q *Query) SaveAttachments(dir string, opts agents.SaveOptions) ([]agents.SavedAttachment, error) {
	return agents.SaveAttachments(q.Attachments, dir, opts)
}

// Reply answers the query with plain text, the shorthand of SAP §5.1. A text
// that begins with "{" would be read as JSON by the receiver, so it is sent as
// a JSON envelope instead. Only the first successful reply is sent; a second
// call returns a [*QueryAlreadyRepliedError] and publishes nothing. The
// agent-supplied reply subject is vetted by the query reply policy first
// ([WithQueryReplyPolicy]); a refusal returns a [*QueryReplyRefusedError] and
// publishes nothing.
func (q *Query) Reply(ctx context.Context, answer string) error {
	payload, err := agents.EncodePlainPrompt(answer)
	if err == nil {
		return q.send(ctx, payload)
	}
	if errors.Is(err, agents.ErrInvalidUTF8) || answer == "" {
		return err
	}
	return q.ReplyEnvelope(ctx, agents.Envelope{Prompt: answer})
}

// ReplyEnvelope answers the query with an envelope: a prompt and optional
// attachments (SAP §7.2). The envelope is validated and its size checked
// against the connection's limit before it is sent. Only the first successful
// reply is sent.
func (q *Query) ReplyEnvelope(ctx context.Context, e agents.Envelope) error {
	payload, err := e.Encode()
	if err != nil {
		return err
	}
	return q.send(ctx, payload)
}

func (q *Query) send(ctx context.Context, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := agents.ValidatePublishSubject(q.ReplySubject); err != nil {
		return errf("query reply subject", err)
	}
	pol := q.policy
	if pol == nil {
		pol = q.c.cfg.queryPolicy
	}
	if perr := pol(q.ReplySubject); perr != nil {
		q.c.log.Debug("refused a query reply subject")
		return &QueryReplyRefusedError{Subject: q.ReplySubject, Err: perr}
	}
	if limit := q.c.nc.MaxPayload(); limit > 0 && int64(len(payload)) > limit {
		return &agents.PayloadTooLargeError{Limit: limit, Actual: int64(len(payload)), Connection: true}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.replied {
		return &QueryAlreadyRepliedError{ID: q.ID}
	}
	if err := q.c.checkOpen(); err != nil {
		return err
	}
	if err := q.c.nc.Publish(q.ReplySubject, payload); err != nil {
		return errf("query reply", err)
	}
	q.replied = true
	return nil
}
