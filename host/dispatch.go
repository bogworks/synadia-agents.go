// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go/micro"
)

// Wire descriptions of the host's own refusals. They are generic by design.
const (
	descBusy      = "agent busy"
	descStopping  = "service stopping"
	descHandler   = "handler error"
	descStatus    = "status handler error"
	descMalformed = "malformed request"
	descOversize  = "request exceeds max_payload"
)

// enter admits one more prompt into the in-flight set unless the host is
// stopping.
func (h *Host) enter() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state == stateStopping || h.state == stateStopped {
		return false
	}
	h.inflight.Add(1)
	h.active.Add(1)
	return true
}

func (h *Host) leave() {
	h.active.Add(-1)
	h.inflight.Done()
}

// replyAllowed vets the caller-supplied reply subject with the configured
// policy. A request without one, or with one the policy refuses, is dropped:
// there is no safe place to answer. A refusal is counted and logged at warn
// without the payload (SDK policy).
func (h *Host) replyAllowed(req micro.Request, kind string) bool {
	reply := req.Reply()
	if reply == "" {
		h.log.Debug("request without a reply subject dropped", "kind", kind)
		return false
	}
	if err := h.checkReply(reply); err != nil {
		h.refusedReply.Add(1)
		h.log.Warn("request dropped: reply subject refused", "kind", kind, "reply_subject", truncate(reply, 128), "reason", truncate(err.Error(), 256))
		return false
	}
	return true
}

// checkReply runs the policy; a panic in it is a refusal.
func (h *Host) checkReply(reply string) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("reply subject policy panicked")
		}
	}()
	return h.cfg.replyPolicy(reply)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// refuse answers a request without serving it: an error frame, then the
// terminator (SAP §6.5, §9). No ack is sent.
func (h *Host) refuse(req micro.Request, code int, desc string) {
	if req.Reply() == "" {
		return
	}
	r := newResponder(h, req)
	r.sendError(agents.NewServiceError(code, desc))
	r.finish()
}

// onPrompt is the micro callback of the prompt endpoint. It runs on the
// subscription's goroutine and only takes a slot; the work runs on its own
// goroutine.
func (h *Host) onPrompt(req micro.Request) {
	if !h.cbEnter() {
		return
	}
	defer h.cbLeave()
	if !h.replyAllowed(req, "prompt") {
		return
	}
	if !h.enter() {
		h.refuse(req, agents.CodeInternal, descStopping)
		return
	}
	select {
	case h.sem <- struct{}{}:
	default:
		if h.cfg.busy == BusyReject429 {
			h.rejectedBusy.Add(1)
			h.leave()
			h.refuse(req, agents.CodeRateLimited, descBusy)
			return
		}
		select {
		case h.sem <- struct{}{}:
		case <-h.stopCh:
			h.leave()
			h.refuse(req, agents.CodeInternal, descStopping)
			return
		}
	}
	h.prompts.Add(1)
	start := time.Now()
	go func() {
		defer h.leave()
		defer func() { <-h.sem }()
		defer h.notePrompt(start)
		if h.cfg.hooks.Raw != nil {
			h.serveRaw(req)
			return
		}
		h.servePrompt(req)
	}()
}

// servePrompt runs the dispatch order of a prompt: envelope 400, limits 400,
// sender admission (400, 401, 403, 500), interceptors, ack, handler, error
// frame, terminator.
func (h *Host) servePrompt(req micro.Request) {
	w := newResponder(h, req)
	defer w.finish()

	data := req.Data()
	// The size is checked on the raw bytes, before any decoding allocates for
	// an oversize request (SAP §5.4: the endpoint limit counts the payload plus
	// the Agent-Sender header's framed bytes, §13.5.1).
	if h.cfg.inboundChecks {
		if err := h.limits.CheckSize(len(data), senderHeaderBytes(req.Headers()), 0); err != nil {
			w.sendError(agents.NewServiceError(agents.CodeBadRequest, descOversize))
			return
		}
	}
	env, err := agents.DecodeEnvelope(data)
	if err != nil {
		w.sendError(serviceErrorOf(err, agents.CodeBadRequest, descMalformed))
		return
	}
	if h.cfg.inboundChecks {
		if err := h.limits.CheckInbound(len(data), env); err != nil {
			w.sendError(serviceErrorOf(err, agents.CodeBadRequest, descMalformed))
			return
		}
	}

	hdr := cloneHeader(nil)
	if rh := req.Headers(); rh != nil {
		hdr = cloneHeader(map[string][]string(rh))
	}
	info := &RequestInfo{Envelope: env, Subject: req.Subject(), Header: hdr}

	ctx, cancel := context.WithCancel(h.base)
	defer cancel()
	adm := h.admit(context.WithValue(ctx, infoKey{}, info), req, data)
	if adm.Err != nil {
		w.sendError(agents.NewServiceError(adm.Err.Code, adm.Err.Description))
		return
	}
	info.Sender = adm.Sender
	request := &Request{
		Envelope: env, Sender: adm.Sender, Subject: req.Subject(),
		Header: cloneHeader(hdr), host: h,
	}

	var started, answered atomic.Bool
	serve := func(c context.Context) error {
		if started.Swap(true) {
			return ErrNextCalledTwice
		}
		// The host's own cancellation reaches the handler even when an
		// interceptor passed a context of its own.
		hctx, hcancel := context.WithCancel(c)
		defer hcancel()
		defer context.AfterFunc(h.base, hcancel)()
		if err := w.ack(); err != nil {
			h.log.Debug("ack not published")
		}
		if h.cfg.keepalive > 0 {
			w.startKeepalive(h.cfg.keepalive)
		}
		err := h.callHandler(hctx, request, w)
		if err == nil {
			answered.Store(true)
		}
		return err
	}
	err = h.runIntercepted(ctx, info, serve)
	if err == nil && !started.Load() {
		err = ErrNextNotCalled
	}
	if err == nil {
		return
	}
	if answered.Load() {
		h.log.Error("request interceptor failed after the handler completed; the reply is kept")
		return
	}
	se := h.failure(err)
	w.sendError(se)
}

// serveRaw admits the sender and hands the raw request to the raw handler,
// which owns the whole reply stream.
func (h *Host) serveRaw(req micro.Request) {
	ctx, cancel := context.WithCancel(h.base)
	defer cancel()
	data := req.Data()
	info := &RequestInfo{Subject: req.Subject(), Header: cloneHeader(natsHeader(req.Headers()))}
	adm := h.admit(context.WithValue(ctx, infoKey{}, info), req, data)
	if adm.Err != nil {
		w := newResponder(h, req)
		w.sendError(agents.NewServiceError(adm.Err.Code, adm.Err.Description))
		w.finish()
		return
	}
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				h.log.Error("raw prompt handler panicked")
				err = errors.New("raw handler panicked")
			}
		}()
		return h.cfg.hooks.Raw(ctx, req, adm.Sender)
	}()
	if err != nil {
		w := newResponder(h, req)
		w.sendError(h.failure(err))
		w.finish()
	}
}

// runIntercepted runs the chain and turns a panic in an interceptor into an
// error.
func (h *Host) runIntercepted(ctx context.Context, info *RequestInfo, serve func(context.Context) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			h.log.Error("request interceptor panicked")
			err = errors.New("interceptor panicked")
		}
	}()
	return runChain(ctx, h.cfg.interceptors, info, serve)
}

func (h *Host) callHandler(ctx context.Context, req *Request, w *Responder) (err error) {
	defer func() {
		if r := recover(); r != nil {
			h.log.Error("prompt handler panicked")
			err = errors.New("handler panicked")
		}
	}()
	return h.handler(ctx, req, w)
}

// admit classifies and admits the sender (SAP §13.7), recovering from a panic
// as a 500.
func (h *Host) admit(ctx context.Context, req micro.Request, data []byte) (adm identity.Admission) {
	defer func() {
		if r := recover(); r != nil {
			h.log.Error("sender admission panicked")
			adm = identity.Admission{Err: &identity.Error{Code: agents.CodeInternal, Description: identity.DescriptionServerError}}
		}
	}()
	return h.gate.AdmitPrompt(ctx, identity.Message{
		Subject: req.Subject(), Data: data, Header: natsHeader(req.Headers()),
	})
}

// failure maps a handler or interceptor error to the error frame it answers
// with. A plain error is a generic 500: its text is not the caller's business.
func (h *Host) failure(err error) *agents.ServiceError {
	var rej *RejectError
	if errors.As(err, &rej) && rej != nil {
		code := rej.Code
		if !agents.KnownCode(code) {
			code = agents.CodeInternal
		}
		se := agents.NewServiceError(code, nonEmpty(rej.Description, "request rejected"))
		se.Body = rej.Body
		h.log.Warn("prompt request refused", "code", code)
		return se
	}
	if errors.Is(err, agents.ErrProtocol) {
		// The caller sees a fixed text; the handler's reason is for the log.
		var pe *agents.ProtocolError
		if errors.As(err, &pe) && pe.Reason != "" {
			h.log.Warn("prompt request refused as malformed", "reason", pe.Reason)
		} else {
			h.log.Warn("prompt request refused as malformed")
		}
		return agents.NewServiceError(agents.CodeBadRequest, descMalformed)
	}
	if h.base.Err() != nil && errors.Is(err, context.Canceled) {
		return agents.NewServiceError(agents.CodeInternal, descStopping)
	}
	h.log.Error("prompt handler failed")
	return agents.NewServiceError(agents.CodeInternal, descHandler)
}

// serviceErrorOf returns the *agents.ServiceError in err, or one of the
// fallback code and description. The host answers only §9.2 codes.
func serviceErrorOf(err error, code int, desc string) *agents.ServiceError {
	var se *agents.ServiceError
	if errors.As(err, &se) && agents.KnownCode(se.Code) {
		return se
	}
	return agents.NewServiceError(code, desc)
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// onStatus serves the status endpoint inline: it classifies the sender so the
// host knows who probed it, never refuses, ignores the request body and
// replies with a freshly built heartbeat-shaped payload, never signed
// (SAP §8.7, §13.9, §13.11).
func (h *Host) onStatus(req micro.Request) {
	if !h.cbEnter() {
		return
	}
	defer h.cbLeave()
	if !h.replyAllowed(req, "status") || !h.running() {
		return
	}
	func() {
		defer func() { _ = recover() }()
		res := h.gate.ClassifyStatus(identity.Message{
			Subject: req.Subject(), Data: req.Data(), Header: natsHeader(req.Headers()),
		})
		h.log.Debug("status request", "sender", res.Sender.Describe())
	}()
	_, data, err := h.buildHeartbeat()
	if err != nil {
		h.log.Error("status handler failed")
		_ = req.Error("500", descStatus, nil)
		return
	}
	if err := req.Respond(data); err != nil {
		h.log.Debug("status reply not published")
	}
}

// senderHeaderBytes returns the framed size the Agent-Sender header adds to
// the message, zero when there is none (SAP §13.5.1).
func senderHeaderBytes(h micro.Headers) int {
	vals := h.Values(identity.HeaderName)
	if len(vals) == 0 {
		return 0
	}
	return identity.HeaderFramingBytes + len(vals[0])
}
