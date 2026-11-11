// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/nats-io/nats.go"
)

// TestPrompt_DecodesBothForms: a plain text payload and a JSON envelope both
// reach the handler as an envelope, after leading whitespace is skipped, and
// the reply goes to the request's reply subject.
//
// catalog: SAP-5.1-01 SAP-5.1-02 SAP-5.3-01 SAP-5.3-02 SAP-5.3-05 SAP-6-01
func TestPrompt_DecodesBothForms(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, echo)
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"plain text", "hello there", "echo: hello there"},
		{"plain text with a space and no brace", "  spaced", "echo:   spaced"},
		{"json envelope", `{"prompt":"hi"}`, "echo: hi"},
		{"leading space tab cr lf then json", " \t\r\n{\"prompt\":\"ws\"}", "echo: ws"},
		{"a plain prompt may contain braces later", "say {\"prompt\":1}", `echo: say {"prompt":1}`},
		{"unicode plain text", "héllo ✓", "echo: héllo ✓"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := rawPrompt(t, nc, h.Subject().Prompt(), []byte(tc.payload), nil, 5*time.Second)
			got := s.texts(t)
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPrompt_UnknownFieldsTolerated: unknown top-level and attachment fields do
// not fail the request, and the handler sees them (SAP §5.6).
//
// catalog: SAP-5.6-01
func TestPrompt_UnknownFieldsTolerated(t *testing.T) {
	_, nc := setup(t)
	var seen atomic.Value
	h := newHost(t, nc, func(ctx context.Context, req *host.Request, w *host.Responder) error {
		seen.Store(req.Envelope)
		return w.Send(ctx, "ok")
	})
	payload := `{"prompt":"p","trace":{"id":7},"attachments":[{"filename":"a.txt","content":"aGk=","mime":"text/plain"}]}`
	s := rawPrompt(t, nc, h.Subject().Prompt(), []byte(payload), nil, 5*time.Second)
	if s.errorFrame() != nil || len(s.texts(t)) != 1 {
		t.Fatalf("request failed: %+v", s.errorFrame())
	}
	env := seen.Load().(agents.Envelope)
	if string(env.Extra["trace"]) != `{"id":7}` {
		t.Errorf("extra: %v", env.Extra)
	}
	if len(env.Attachments) != 1 || string(env.Attachments[0].Extra["mime"]) != `"text/plain"` {
		t.Errorf("attachment extra: %+v", env.Attachments)
	}
}

// TestPrompt_MalformedIs400: a malformed request is answered 400, an error
// frame then the terminator, with no ack and without calling the handler.
//
// catalog: SAP-5.3-03 SAP-5.3-04 SAP-5.3-06 SAP-9.2-01 SAP-5.1-04 SAP-9.1-01 SAP-6.5-02 SAP-6.5-04
func TestPrompt_MalformedIs400(t *testing.T) {
	_, nc := setup(t)
	var calls atomic.Int32
	counting := func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		calls.Add(1)
		return w.Send(ctx, "ok")
	}
	h := newHost(t, nc, counting)
	noAtt := newHostNamed(t, nc, "noatt", counting, host.WithAttachmentsOK(false))
	cases := []struct {
		name    string
		h       *host.Host
		payload string
	}{
		{"zero bytes", h, ""},
		{"invalid json", h, `{"prompt":`},
		{"no prompt field", h, `{"text":"x"}`},
		{"prompt not a string", h, `{"prompt":3}`},
		{"prompt null", h, `{"prompt":null}`},
		{"empty prompt", h, `{"prompt":""}`},
		{"attachments not an array", h, `{"prompt":"x","attachments":"a"}`},
		{"attachment without content", h, `{"prompt":"x","attachments":[{"filename":"a"}]}`},
		{"url-safe base64", h, `{"prompt":"x","attachments":[{"filename":"a","content":"_-8="}]}`},
		{"unpadded base64", h, `{"prompt":"x","attachments":[{"filename":"a","content":"aGk"}]}`},
		{"whitespace in base64", h, `{"prompt":"x","attachments":[{"filename":"a","content":"aG k="}]}`},
		{"invalid base64", h, `{"prompt":"x","attachments":[{"filename":"a","content":"!!!!"}]}`},
		{"invalid utf-8 plain text", h, "bad \xff\xfe"},
		{"attachments to attachments_ok false", noAtt, `{"prompt":"x","attachments":[{"filename":"a","content":"aGk="}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := calls.Load()
			s := rawPrompt(t, nc, tc.h.Subject().Prompt(), []byte(tc.payload), nil, 5*time.Second)
			if len(s.Frames) != 2 {
				t.Fatalf("want error frame + terminator, got %d frames", len(s.Frames))
			}
			se := s.Frames[0].serviceError()
			if se == nil || se.Code != 400 || !errors.Is(se, agents.ErrBadRequest) {
				t.Fatalf("first frame: %+v", se)
			}
			if got := s.Frames[0].Header.Get("Nats-Service-Error-Code"); got != "400" {
				t.Errorf("code header %q", got)
			}
			if se.Description == "" || strings.ContainsAny(se.Description, "\r\n") {
				t.Errorf("description %q", se.Description)
			}
			if !s.last().terminator() || len(s.last().Header) != 0 {
				t.Errorf("last frame is not a headerless zero-byte terminator")
			}
			if s.acks() != 0 {
				t.Error("an ack was sent for a refused request")
			}
			if calls.Load() != before {
				t.Error("handler was called")
			}
		})
	}
}

// TestPrompt_OversizedPayload: the host refuses a payload over its advertised
// max_payload with 400 (SAP §5.4 MAY; SDK policy, default on) and can be told
// not to.
//
// catalog: SAP-5.4-04 SAP-9.2-02
func TestPrompt_OversizedPayload(t *testing.T) {
	_, nc := setup(t)
	big := []byte(strings.Repeat("x", 600))
	h := newHost(t, nc, echo, host.WithMaxPayload(512))
	s := rawPrompt(t, nc, h.Subject().Prompt(), big, nil, 5*time.Second)
	if se := s.errorFrame(); se == nil || se.Code != 400 {
		t.Fatalf("want 400, got %+v", se)
	}
	if got := rawPrompt(t, nc, h.Subject().Prompt(), []byte(strings.Repeat("x", 500)), nil, 5*time.Second).texts(t); len(got) != 1 {
		t.Error("a payload under the limit was refused")
	}
	off := newHostNamed(t, nc, "off", echo, host.WithMaxPayload(512), host.WithInboundChecks(false))
	if got := rawPrompt(t, nc, off.Subject().Prompt(), big, nil, 5*time.Second).texts(t); len(got) != 1 {
		t.Error("checks were not turned off")
	}
}

// TestStream_AckFirstThenChunksThenTerminator: exactly one ack first, then the
// handler's chunks in order, then the zero-byte header-less terminator.
//
// catalog: SAP-6.4-02 SAP-6.4-03 SAP-6.5-01 SAP-6.5-05 SAP-6.3-04 SAP-6.6-01 SAP-6.2-02 SAP-6.2-01
func TestStream_AckFirstThenChunksThenTerminator(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		for _, s := range []string{"one", "two", "three"} {
			if err := w.Send(ctx, s); err != nil {
				return err
			}
		}
		return w.SendAttachments(ctx, "four", agents.NewAttachment("a.txt", []byte("hi")))
	})
	s := prompt(t, nc, h, "go")
	if n := len(s.Frames); n != 6 {
		t.Fatalf("want ack + 4 chunks + terminator, got %d frames", n)
	}
	if !s.Frames[0].isAck() || s.acks() != 1 {
		t.Fatalf("the ack is not first and alone (acks=%d)", s.acks())
	}
	if !s.last().terminator() || len(s.last().Header) != 0 || len(s.last().Data) != 0 {
		t.Fatal("last frame is not the terminator")
	}
	for i, f := range s.Frames[:5] {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(f.Data, &obj); err != nil {
			t.Fatalf("frame %d is not a JSON object: %q", i, f.Data)
		}
		var typ string
		if json.Unmarshal(obj["type"], &typ) != nil || obj["data"] == nil {
			t.Fatalf("frame %d lacks type or data: %q", i, f.Data)
		}
		if typ != "status" && typ != "response" && typ != "query" {
			t.Fatalf("frame %d type %q", i, typ)
		}
	}
	if got := strings.Join(s.texts(t), ","); got != "one,two,three,four" {
		t.Errorf("order: %s", got)
	}
	// The bare-string form of §6.3, and the object form with attachments.
	if string(s.Frames[1].Data) != `{"type":"response","data":"one"}` {
		t.Errorf("frame 1: %s", s.Frames[1].Data)
	}
	last := s.Frames[4].chunk(t).(agents.ResponseChunk)
	if len(last.Attachments) != 1 {
		t.Errorf("attachments: %+v", last)
	}
	if b, _ := last.Attachments[0].Bytes(); string(b) != "hi" {
		t.Errorf("attachment content %q", b)
	}
}

// TestStream_ResponseChunkForms: a response chunk's data is a bare string, or
// an object with a string text field (SAP §6.3).
//
// catalog: SAP-6.3-01
func TestStream_ResponseChunkForms(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		if err := w.Send(ctx, "bare"); err != nil {
			return err
		}
		return w.SendChunk(ctx, agents.ResponseChunk{Text: "object", Extra: map[string]json.RawMessage{"k": json.RawMessage(`1`)}})
	})
	s := prompt(t, nc, h, "x")
	var forms []string
	for _, f := range s.Frames[1 : len(s.Frames)-1] {
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(f.Data, &env); err != nil {
			t.Fatal(err)
		}
		var str string
		var obj struct {
			Text *string `json:"text"`
		}
		switch {
		case json.Unmarshal(env.Data, &str) == nil:
			forms = append(forms, "string:"+str)
		case json.Unmarshal(env.Data, &obj) == nil && obj.Text != nil:
			forms = append(forms, "object:"+*obj.Text)
		default:
			t.Fatalf("data is neither: %s", env.Data)
		}
	}
	if got := strings.Join(forms, ","); got != "string:bare,object:object" {
		t.Fatalf("forms %s", got)
	}
}

// TestStream_CallerGoneDoesNotCancelHandler: the host is not told when a
// caller stops listening, and the handler runs to completion (SAP §6.7).
//
// catalog: SAP-6.7-02
func TestStream_CallerGoneDoesNotCancelHandler(t *testing.T) {
	srv, nc := setup(t)
	started := make(chan struct{})
	finish := make(chan struct{})
	result := make(chan error, 1)
	h := newHost(t, nc, func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		close(started)
		<-finish
		err := ctx.Err()
		if sendErr := w.Send(ctx, "work done"); err == nil {
			err = sendErr
		}
		result <- err
		return nil
	})
	caller := connect(t, srv)
	sub, reply := openStream(t, caller)
	if err := caller.PublishMsg(&nats.Msg{Subject: h.Subject().Prompt(), Reply: reply, Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	<-started
	_ = sub.Unsubscribe()
	caller.Close()
	time.Sleep(200 * time.Millisecond)
	close(finish)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("the handler was disturbed by the caller leaving: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not complete")
	}
}

// TestStream_SingleChunkIsValid: one response chunk then the terminator, and a
// handler that sends nothing, are both valid streams.
//
// catalog: SAP-6.5-05
func TestStream_SingleChunkIsValid(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, echo)
	s := prompt(t, nc, h, "x")
	if len(s.Frames) != 3 || !s.Frames[0].isAck() || !s.last().terminator() {
		t.Fatalf("frames: %d", len(s.Frames))
	}
	quiet := newHostNamed(t, nc, "quiet", func(context.Context, *host.Request, *host.Responder) error { return nil })
	s = prompt(t, nc, quiet, "x")
	if len(s.Frames) != 2 || !s.Frames[0].isAck() || !s.last().terminator() {
		t.Fatalf("quiet stream: %d frames", len(s.Frames))
	}
}

// TestStream_AckPrecedesSlowWork: the ack goes out before the handler's slow
// work (a model call), not after it.
//
// catalog: SAP-6.4-04
func TestStream_AckPrecedesSlowWork(t *testing.T) {
	_, nc := setup(t)
	release := make(chan struct{})
	var finished atomic.Bool
	h := newHost(t, nc, func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		<-release
		finished.Store(true)
		return w.Send(ctx, "late")
	})
	open := closeOnce(t, release)
	sub, reply := openStream(t, nc)
	if err := nc.PublishMsg(&nats.Msg{Subject: h.Subject().Prompt(), Reply: reply, Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	m, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !(frame{Data: m.Data, Header: m.Header}).isAck() || finished.Load() {
		t.Fatal("the first message is not an ack that preceded the work")
	}
	open()
	s := collect(t, sub, 5*time.Second)
	if got := s.texts(t); len(got) != 1 || got[0] != "late" {
		t.Fatalf("got %v", got)
	}
}

// TestStream_NothingAfterTerminator: after the handler returns, a late send
// fails and nothing more appears on the reply subject.
//
// catalog: SAP-6.5-03 SAP-9.3-02
func TestStream_NothingAfterTerminator(t *testing.T) {
	_, nc := setup(t)
	late := make(chan error, 1)
	h := newHost(t, nc, func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		go func() {
			time.Sleep(150 * time.Millisecond)
			late <- w.Send(context.Background(), "too late")
		}()
		return w.Send(ctx, "first")
	})
	sub, reply := openStream(t, nc)
	if err := nc.PublishMsg(&nats.Msg{Subject: h.Subject().Prompt(), Reply: reply, Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	s := collect(t, sub, 5*time.Second)
	if len(s.Frames) != 3 {
		t.Fatalf("frames: %d", len(s.Frames))
	}
	select {
	case err := <-late:
		if !errors.Is(err, host.ErrStreamClosed) {
			t.Fatalf("late send: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("late send never returned")
	}
	if m, err := sub.NextMsg(300 * time.Millisecond); err == nil {
		t.Fatalf("a message followed the terminator: %q", m.Data)
	}
}

// TestStream_ErrorAfterChunks: chunks, then an error-headered message, then the
// terminator; the error is carried in headers and not in a chunk.
//
// catalog: SAP-6.5-04 SAP-6.2-04 SAP-9.1-01 SAP-6.5-02
func TestStream_ErrorAfterChunks(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		_ = w.Send(ctx, "partial")
		return host.Reject(409, "conflict with state")
	})
	s := prompt(t, nc, h, "x")
	if len(s.Frames) != 4 {
		t.Fatalf("want ack, chunk, error, terminator; got %d", len(s.Frames))
	}
	if !s.Frames[0].isAck() || s.Frames[1].serviceError() != nil || s.Frames[2].serviceError() == nil || !s.Frames[3].terminator() {
		t.Fatal("wrong frame order")
	}
	se := s.Frames[2].serviceError()
	if se.Code != 409 || se.Description != "conflict with state" || !errors.Is(se, agents.ErrConflict) {
		t.Fatalf("error: %+v", se)
	}
	if s.Frames[2].terminator() {
		t.Error("the error frame is itself a terminator")
	}
	if got := s.Frames[2].Header.Get("Nats-Service-Error"); got != "conflict with state" {
		t.Errorf("description header %q", got)
	}
}

// TestHandlerError_Mapping: a handler error becomes a §9 frame; only §9.2 codes
// go out and the text of a plain error never reaches the caller.
//
// catalog: SAP-9.2-03 SAP-9.1-01
func TestHandlerError_Mapping(t *testing.T) {
	_, nc := setup(t)
	secret := errors.New("database password is hunter2")
	cases := []struct {
		name     string
		err      error
		panics   bool
		wantCode int
		wantDesc string
	}{
		{"plain error is a generic 500", secret, false, 500, "handler error"},
		{"wrapped plain error", errors.Join(secret, errors.New("x")), false, 500, "handler error"},
		{"panic is a generic 500", nil, true, 500, "handler error"},
		{"protocol error is 400 with a fixed description", &agents.ProtocolError{Reason: "bad shape"}, false, 400, "malformed request"},
		{"wrapped protocol sentinel is 400", errors.Join(agents.ErrProtocol), false, 400, "malformed request"},
		{"reject 400", host.Reject(400, "bad"), false, 400, "bad"},
		{"reject 401", host.Reject(401, "who"), false, 401, "who"},
		{"reject 403", host.Reject(403, "no"), false, 403, "no"},
		{"reject 404", host.Reject(404, "gone"), false, 404, "gone"},
		{"reject 409", host.Reject(409, "busy state"), false, 409, "busy state"},
		{"reject 429", host.Reject(429, "slow down"), false, 429, "slow down"},
		{"reject 500", host.Reject(500, "boom"), false, 500, "boom"},
		{"a code outside the table goes out as 500", &host.RejectError{Code: 418, Description: "teapot"}, false, 500, "teapot"},
		{"multi-line description is flattened", host.Reject(400, "line one\nline two\r\nthree"), false, 400, ""},
		{"long description is capped", host.Reject(400, strings.Repeat("é", 300)), false, 400, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHostNamed(t, nc, "m"+strings.ReplaceAll(tc.name[:3], " ", ""), func(context.Context, *host.Request, *host.Responder) error {
				if tc.panics {
					panic("kaboom hunter2")
				}
				return tc.err
			})
			s := prompt(t, nc, h, "x")
			se := s.errorFrame()
			if se == nil {
				t.Fatal("no error frame")
			}
			if se.Code != tc.wantCode {
				t.Errorf("code %d, want %d", se.Code, tc.wantCode)
			}
			if tc.wantDesc != "" && se.Description != tc.wantDesc {
				t.Errorf("description %q, want %q", se.Description, tc.wantDesc)
			}
			if strings.Contains(se.Description, "hunter2") || strings.Contains(string(s.last().Data), "hunter2") {
				t.Error("the error text reached the caller")
			}
			if strings.ContainsAny(se.Description, "\r\n") || len(se.Description) > 200 {
				t.Errorf("description not sanitized: %d bytes", len(se.Description))
			}
			if !s.last().terminator() || s.last().serviceError() != nil {
				t.Error("no terminator after the error")
			}
		})
	}
}

// TestReject_OnlyTaxonomyCodes: Reject refuses a code outside SAP §9.2.
//
// catalog: SAP-9.2-03
func TestReject_OnlyTaxonomyCodes(t *testing.T) {
	for _, c := range []int{400, 401, 403, 404, 409, 429, 500} {
		err := host.Reject(c, "x")
		var re *host.RejectError
		if !errors.As(err, &re) || re.Code != c || !errors.Is(err, host.ErrRejected) {
			t.Errorf("code %d: %v", c, err)
		}
		if !agents.IsCode(err, c) {
			t.Errorf("code %d does not match its sentinel", c)
		}
	}
	for _, c := range []int{0, 200, 402, 418, 499, 502, 503, 599} {
		if err := host.Reject(c, "x"); !errors.Is(err, host.ErrInvalidCode) {
			t.Errorf("code %d accepted: %v", c, err)
		}
	}
}

// TestErrorBody: an error frame carries an optional JSON body with a string
// error code and extra fields (SAP §9.1), and an empty body otherwise.
//
// catalog: SAP-9.1-02 SAP-9.1-03 SAP-9.1-04 SAP-9.1-06
func TestErrorBody(t *testing.T) {
	_, nc := setup(t)
	empty := newHost(t, nc, func(context.Context, *host.Request, *host.Responder) error { return host.Reject(400, "plain") })
	s := prompt(t, nc, empty, "x")
	if f := s.Frames[1]; len(f.Data) != 0 || f.serviceError() == nil || f.serviceError().Body != nil {
		t.Errorf("expected an empty error body: %q", f.Data)
	}
	body := agents.ErrorBody{
		Error: "quota_exceeded", Message: "five per hour",
		Extra: map[string]json.RawMessage{"retry_after_s": json.RawMessage("3600")},
	}
	rich := newHostNamed(t, nc, "rich", func(context.Context, *host.Request, *host.Responder) error {
		return host.RejectWithBody(429, "quota", body)
	})
	s = prompt(t, nc, rich, "x")
	se := s.errorFrame()
	if se == nil || se.Body == nil {
		t.Fatalf("no body: %+v", se)
	}
	if se.Body.Error != "quota_exceeded" || se.Message() != "five per hour" || string(se.Body.Extra["retry_after_s"]) != "3600" {
		t.Errorf("body: %+v", se.Body)
	}
	if se.Code != 429 || se.Description != "quota" {
		t.Errorf("headers: %d %q", se.Code, se.Description)
	}
}

// TestKeepalive: while a handler works the host sends further acks at the
// keep-alive cadence, and none after the terminator. They are SDK policy; with
// keep-alive off (WithoutKeepalive) the stream carries one ack.
func TestKeepalive(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		select {
		case <-time.After(400 * time.Millisecond):
		case <-ctx.Done():
		}
		return w.Send(ctx, "done")
	}, host.WithKeepalive(50*time.Millisecond))
	sub, reply := openStream(t, nc)
	if err := nc.PublishMsg(&nats.Msg{Subject: h.Subject().Prompt(), Reply: reply, Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	s := collect(t, sub, 5*time.Second)
	if s.acks() < 4 {
		t.Errorf("only %d acks in 400ms at a 50ms keep-alive", s.acks())
	}
	for i, f := range s.Frames[:s.acks()] {
		if !f.isAck() {
			t.Errorf("frame %d: acks do not come first", i)
		}
	}
	if m, err := sub.NextMsg(250 * time.Millisecond); err == nil {
		t.Fatalf("keep-alive after the terminator: %q", m.Data)
	}

	off := newHostNamed(t, nc, "off", func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		time.Sleep(150 * time.Millisecond)
		return w.Send(ctx, "x")
	}, host.WithoutKeepalive())
	if n := prompt(t, nc, off, "x").acks(); n != 1 {
		t.Errorf("acks without keep-alive: %d", n)
	}
}

// TestKeepalive_ZeroDisables: WithKeepalive(0) is the same as WithoutKeepalive,
// so a long handler's stream carries exactly one ack (SAP:L380).
func TestKeepalive_ZeroDisables(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		time.Sleep(300 * time.Millisecond)
		return w.Send(ctx, "x")
	}, host.WithKeepalive(10*time.Millisecond), host.WithKeepalive(0))
	if n := prompt(t, nc, h, "x").acks(); n != 1 {
		t.Errorf("acks with WithKeepalive(0): %d, want exactly 1", n)
	}
}

// TestResponder_RefusesStatusChunksAndBadAttachments: the host owns acks.
func TestResponder_RefusesStatusChunksAndBadAttachments(t *testing.T) {
	_, nc := setup(t)
	errs := make(chan []error, 1)
	h := newHost(t, nc, func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		var out []error
		out = append(out, w.SendChunk(ctx, agents.StatusChunk{Status: "ack"}))
		out = append(out, w.SendAttachments(ctx, "t", agents.Attachment{Filename: "a", Content: "!!"}))
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		out = append(out, w.Send(cctx, "x"))
		errs <- out
		return nil
	})
	s := prompt(t, nc, h, "x")
	got := <-errs
	if !errors.Is(got[0], host.ErrStatusChunk) {
		t.Errorf("status chunk: %v", got[0])
	}
	if !errors.Is(got[1], agents.ErrInvalidAttachment) {
		t.Errorf("bad attachment: %v", got[1])
	}
	if !errors.Is(got[2], context.Canceled) {
		t.Errorf("canceled context: %v", got[2])
	}
	if len(s.Frames) != 2 {
		t.Errorf("refused sends reached the wire: %d frames", len(s.Frames))
	}
}

// TestRequest_SeenByHandler: the handler gets the envelope, subject and a clone
// of the received headers (REQ-016).
func TestRequest_SeenByHandler(t *testing.T) {
	_, nc := setup(t)
	got := make(chan *host.Request, 1)
	var n atomic.Int32
	h := newHost(t, nc, func(ctx context.Context, req *host.Request, w *host.Responder) error {
		if n.Add(1) == 1 {
			req.Header.Set("X-Mutated", "yes")
		}
		got <- req
		return nil
	})
	hdr := nats.Header{}
	hdr.Set("X-Trace", "abc")
	hdr.Add("X-Multi", "1")
	hdr.Add("X-Multi", "2")
	rawPrompt(t, nc, h.Subject().Prompt(), []byte("x"), hdr, 5*time.Second)
	req := <-got
	if req.Subject != h.Subject().Prompt() || req.Header.Get("X-Trace") != "abc" || len(req.Header.Values("X-Multi")) != 2 {
		t.Fatalf("request: %+v", req)
	}
	if req.Sender != nil {
		t.Error("a sender for an unsigned request")
	}
	if _, ok, err := req.ResolveSender(context.Background()); ok || err != nil {
		t.Errorf("ResolveSender for an absent sender: %v %v", ok, err)
	}
	// A second request sees fresh headers, unaffected by the mutation.
	hdr2 := nats.Header{}
	hdr2.Set("X-Trace", "def")
	rawPrompt(t, nc, h.Subject().Prompt(), []byte("y"), hdr2, 5*time.Second)
	if r2 := <-got; r2.Header.Get("X-Mutated") != "" || r2.Header.Get("X-Trace") != "def" {
		t.Error("a mutation leaked across requests")
	}
}

// TestPrompt_WithoutReplySubjectIsDropped: a request with no reply subject gets
// no stream and does not call the handler.
func TestPrompt_WithoutReplySubjectIsDropped(t *testing.T) {
	_, nc := setup(t)
	var calls atomic.Int32
	h := newHost(t, nc, func(context.Context, *host.Request, *host.Responder) error { calls.Add(1); return nil })
	if err := nc.Publish(h.Subject().Prompt(), []byte("x")); err != nil {
		t.Fatal(err)
	}
	prompt(t, nc, h, "sync point")
	if calls.Load() != 1 {
		t.Errorf("handler calls: %d", calls.Load())
	}
}

// TestAttachments_RoundTrip: attachments in a request arrive decoded, with
// their authoritative file names.
func TestAttachments_RoundTrip(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, func(ctx context.Context, req *host.Request, w *host.Responder) error {
		var names []string
		for _, a := range req.Envelope.Attachments {
			b, err := a.Bytes()
			if err != nil {
				return err
			}
			names = append(names, a.Filename+"="+string(b))
		}
		return w.Send(ctx, strings.Join(names, ";"))
	})
	payload := jsonPrompt(t, map[string]any{"prompt": "p", "attachments": []map[string]string{
		{"filename": "a.txt", "content": base64.StdEncoding.EncodeToString([]byte("alpha"))},
		{"filename": "../b.bin", "content": base64.StdEncoding.EncodeToString([]byte("beta"))},
	}})
	s := rawPrompt(t, nc, h.Subject().Prompt(), payload, nil, 5*time.Second)
	if got := s.texts(t); len(got) != 1 || got[0] != "a.txt=alpha;../b.bin=beta" {
		t.Fatalf("got %v", got)
	}
}
