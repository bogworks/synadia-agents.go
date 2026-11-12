// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// fixture is a server, one running host and a client on its own connection.
type fixture struct {
	srv *server.Server
	h   *host
	c   *caller.Client
	a   *caller.Agent
	cnc *nats.Conn
}

func setup(t *testing.T, hostMods []func(*host), opts ...caller.Option) *fixture {
	t.Helper()
	srv := startServer(t)
	hnc, cnc := connect(t, srv), connect(t, srv)
	h := newHost(t, hnc, hostMods...).start()
	c := newClient(t, cnc, opts...)
	a, err := c.LookupInstance(ctxT(t), h.id)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{srv: srv, h: h, c: c, a: a, cnc: cnc}
}

func mod(f ...func(*host)) []func(*host) { return f }

// collect ranges over a stream and returns its events and the ending error.
func collect(t *testing.T, st *caller.Stream) ([]caller.Event, error) {
	t.Helper()
	var evs []caller.Event
	var last error
	for ev, err := range st.Events() {
		if err != nil {
			last = err
			continue
		}
		evs = append(evs, ev)
	}
	return evs, last
}

func texts(evs []caller.Event) string {
	var sb strings.Builder
	for _, e := range evs {
		if r, ok := e.(caller.Response); ok {
			sb.WriteString(r.Text)
		}
	}
	return sb.String()
}

// TestStream_ChunksInOrder asserts that a caller streams chunks in publication
// order, accepts both the string and the object form of a response, tolerates
// unknown fields in data objects, silently ignores unknown chunk types and
// unknown status values, concatenates response text in order and ends at the
// zero-byte terminator.
//
// catalog: SAP-6.3-03
// catalog: SAP-6.3-05
// catalog: SAP-6.2-03
// catalog: SAP-6.4-05
// catalog: SAP-6.6-06
// catalog: SAP-6.6-07
// catalog: SAP-12-08
// catalog: SAP-12-09
func TestStream_ChunksInOrder(t *testing.T) {
	f := setup(t, mod(func(h *host) {
		h.script = func(h *host, m *nats.Msg) {
			h.ack(m)
			h.chunk(m, "status", "pondering") // unknown status value
			h.chunk(m, "telemetry", map[string]any{"cpu": 0.5})
			h.text(m, "Hello, ")
			h.chunk(m, "response", map[string]any{"text": "wor", "future_field": []int{1, 2}})
			h.chunk(m, "response", map[string]any{"text": "ld", "attachments": []map[string]any{{"filename": "a.txt", "content": "aGk="}}})
			h.end(m)
		}
	}))
	st, err := f.a.PromptText(ctxT(t), "go")
	if err != nil {
		t.Fatal(err)
	}
	evs, err := collect(t, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 {
		t.Fatalf("got %d events, want ack and three responses: %#v", len(evs), evs)
	}
	if s, ok := evs[0].(caller.Status); !ok || s.Value != agents.StatusAck {
		t.Fatalf("first event %#v", evs[0])
	}
	if got := texts(evs); got != "Hello, world" {
		t.Fatalf("text %q", got)
	}
	mid := evs[2].(caller.Response)
	if _, ok := mid.Extra["future_field"]; !ok {
		t.Fatalf("an unknown field was not tolerated and kept: %v", mid.Extra)
	}
	last := evs[3].(caller.Response)
	if len(last.Attachments) != 1 || last.Attachments[0].Filename != "a.txt" {
		t.Fatalf("attachments %+v", last.Attachments)
	}
	if !st.Terminated() || st.Err() != nil {
		t.Fatalf("terminated=%v err=%v", st.Terminated(), st.Err())
	}
	if f.c.Stats().DroppedReplies != 0 || f.c.Stats().MalformedChunks != 0 {
		t.Fatalf("stats %+v", f.c.Stats())
	}
}

// TestStream_HeaderAccess asserts that every event exposes a copy of the
// headers its message arrived with.
func TestStream_HeaderAccess(t *testing.T) {
	f := setup(t, mod(func(h *host) {
		h.script = func(h *host, m *nats.Msg) {
			b, _ := agents.EncodeChunk(agents.ResponseChunk{Text: "x"})
			_ = h.nc.PublishMsg(&nats.Msg{Subject: m.Reply, Data: b, Header: nats.Header{"X-Trace": {"t-1"}}})
			h.end(m)
		}
	}))
	st, err := f.a.PromptText(ctxT(t), "go")
	if err != nil {
		t.Fatal(err)
	}
	evs, err := collect(t, st)
	if err != nil || len(evs) != 1 {
		t.Fatal(evs, err)
	}
	h := evs[0].Header()
	if h.Get("X-Trace") != "t-1" {
		t.Fatalf("headers %v", h)
	}
	h.Set("X-Trace", "changed")
	if evs[0].Header().Get("X-Trace") != "t-1" {
		t.Fatal("Header returned shared state")
	}
}

// TestStream_ErrorHeadersBeforeBody asserts that the error headers are read on
// every message before its body, that an error body without a message or with
// no JSON falls back to the header description, that unknown body fields are
// tolerated, and that the terminator after an error is consumed.
//
// catalog: SAP-12-07
// catalog: SAP-9.1-05
// catalog: SAP-9.1-07
// catalog: SAP-9.1-08
func TestStream_ErrorHeadersBeforeBody(t *testing.T) {
	cases := []struct {
		name     string
		before   bool // a response chunk precedes the error
		code     string
		desc     string
		body     string
		wantCode int
		wantMsg  string
		check    func(*testing.T, *agents.ServiceError)
	}{
		{"json body with message and unknown fields", false, "429", "Too Many", `{"error":"rate_limited","message":"slow down","retry_after_s":30}`, 429, "slow down",
			func(t *testing.T, se *agents.ServiceError) {
				t.Helper()
				if se.Body == nil || se.Body.Error != "rate_limited" || string(se.Body.Extra["retry_after_s"]) != "30" {
					t.Fatalf("body %+v", se.Body)
				}
			}},
		{"json body without a message", false, "400", "bad envelope", `{"error":"bad_request"}`, 400, "bad envelope", nil},
		{"empty body", false, "500", "internal exploded", ``, 500, "internal exploded", nil},
		{"body that is not json", false, "409", "busy right now", `<html>nope</html>`, 409, "busy right now", nil},
		{"body that looks like a chunk", false, "403", "forbidden", `{"type":"response","data":"sneaky"}`, 403, "forbidden", nil},
		{"after content", true, "500", "mid-stream failure", ``, 500, "mid-stream failure", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, mod(func(h *host) {
				h.script = func(h *host, m *nats.Msg) {
					h.ack(m)
					if tc.before {
						h.text(m, "partial")
					}
					h.fail(m, tc.code, tc.desc, []byte(tc.body))
					h.end(m)
				}
			}))
			st, err := f.a.PromptText(ctxT(t), "go")
			if err != nil {
				t.Fatal(err)
			}
			evs, err := collect(t, st)
			var se *agents.ServiceError
			if !errors.As(err, &se) {
				t.Fatalf("error %v is not a *agents.ServiceError", err)
			}
			if se.Code != tc.wantCode || se.Message() != tc.wantMsg {
				t.Fatalf("code %d message %q", se.Code, se.Message())
			}
			if !agents.IsCode(err, tc.wantCode) || errors.Is(err, agents.ErrLocal) {
				t.Fatalf("a remote error must match its code and not ErrLocal: %v", err)
			}
			for _, e := range evs {
				if r, ok := e.(caller.Response); ok && r.Text == "sneaky" {
					t.Fatal("an error-headered message was read as a chunk")
				}
			}
			if tc.before && texts(evs) != "partial" {
				t.Fatalf("content before the error was lost: %q", texts(evs))
			}
			if tc.check != nil {
				tc.check(t, se)
			}
			// The terminator that follows an error is consumed, not counted late.
			if err := f.cnc.Flush(); err != nil {
				t.Fatal(err)
			}
			if d := f.c.Stats().DroppedReplies; d != 0 {
				t.Fatalf("%d replies counted as late; the trailing terminator should have been consumed", d)
			}
			if !st.Terminated() {
				t.Fatal("the terminator after the error was not observed")
			}
		})
	}
}

// TestStream_InactivityTimeout asserts the per-stream inactivity timeout: a
// stream that never terminates ends with a stall error after the timeout, an
// ack and every later chunk reset it, and nothing blocks past it.
//
// catalog: SAP-6.6-03
// catalog: SAP-6.6-05
// catalog: SAP-6.4-01
func TestStream_InactivityTimeout(t *testing.T) {
	t.Run("missing terminator", func(t *testing.T) {
		f := setup(t, mod(func(h *host) {
			h.script = func(h *host, m *nats.Msg) { h.ack(m); h.text(m, "partial") } // no terminator
		}))
		start := time.Now()
		st, err := f.a.PromptText(ctxT(t), "go", caller.WithPromptInactivityTimeout(300*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		evs, err := collect(t, st)
		var stalled *agents.StreamStalledError
		if !errors.As(err, &stalled) || !errors.Is(err, agents.ErrStreamStalled) || stalled.Timeout != 300*time.Millisecond {
			t.Fatalf("error %v", err)
		}
		if texts(evs) != "partial" || st.Terminated() {
			t.Fatalf("events %v terminated %v", evs, st.Terminated())
		}
		if el := time.Since(start); el < 300*time.Millisecond || el > 3*time.Second {
			t.Fatalf("the stream ended after %s", el)
		}
	})
	t.Run("first chunk later than the timeout", func(t *testing.T) {
		f := setup(t, mod(func(h *host) {
			h.script = func(h *host, m *nats.Msg) { time.Sleep(600 * time.Millisecond); h.ack(m); h.end(m) }
		}))
		st, err := f.a.PromptText(ctxT(t), "go", caller.WithPromptInactivityTimeout(200*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := collect(t, st); !errors.Is(err, agents.ErrStreamStalled) {
			t.Fatalf("error %v", err)
		}
	})
	t.Run("an ack and every chunk reset the timer", func(t *testing.T) {
		f := setup(t, mod(func(h *host) {
			h.script = func(h *host, m *nats.Msg) {
				h.ack(m)
				for i := 0; i < 5; i++ {
					time.Sleep(150 * time.Millisecond)
					h.text(m, "x")
				}
				h.end(m)
			}
		}))
		st, err := f.a.PromptText(ctxT(t), "go", caller.WithPromptInactivityTimeout(400*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		evs, err := collect(t, st)
		if err != nil || texts(evs) != "xxxxx" || !st.Terminated() {
			t.Fatalf("a live stream was cut: %q %v", texts(evs), err)
		}
	})
}

// TestStream_MaxWait asserts the stream ceiling that backs the inactivity
// timeout when a host keeps chattering without ever terminating.
func TestStream_MaxWait(t *testing.T) {
	f := setup(t, mod(func(h *host) {
		h.script = func(h *host, m *nats.Msg) {
			h.ack(m)
			for i := 0; i < 40; i++ {
				time.Sleep(25 * time.Millisecond)
				h.text(m, "x")
			}
		}
	}))
	st, err := f.a.PromptText(ctxT(t), "go", caller.WithPromptMaxWait(300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	_, err = collect(t, st)
	var mw *caller.MaxWaitError
	if !errors.As(err, &mw) || !errors.Is(err, caller.ErrMaxWait) {
		t.Fatalf("error %v", err)
	}
}

// TestStream_MalformedChunksAreDropped asserts that malformed peers do not break
// a stream: bad JSON, a known type with bad data, a body without a type, plain
// text on the response side, a null data field and a query with an unusable
// reply subject are each dropped and counted, and the stream carries on to its
// terminator.
func TestStream_MalformedChunksAreDropped(t *testing.T) {
	bad := []string{
		`{"type":"response","data":`, // truncated JSON
		`not json at all`,            // plain text on the response side
		`{"data":"no type"}`,         // no discriminator
		`{"type":"response"}`,        // data is required
		`{"type":"response","data":null}`,
		`{"type":"response","data":{"text":5}}`, // known type, bad data
		`{"type":"status","data":{"x":1}}`,      // status data must be a string
		`{"type":"query","data":{"id":"q"}}`,    // query without reply_subject and prompt
		`{"type":"query","data":{"id":"q","reply_subject":"a..b","prompt":"?"}}`, // unusable subject
		`[1,2,3]`,
	}
	f := setup(t, mod(func(h *host) {
		h.script = func(h *host, m *nats.Msg) {
			h.ack(m)
			for _, b := range bad {
				h.raw(m, []byte(b))
			}
			h.text(m, "survivor")
			h.end(m)
		}
	}))
	st, err := f.a.PromptText(ctxT(t), "go")
	if err != nil {
		t.Fatal(err)
	}
	evs, err := collect(t, st)
	if err != nil {
		t.Fatal(err)
	}
	if texts(evs) != "survivor" || !st.Terminated() {
		t.Fatalf("events %v", evs)
	}
	if got := f.c.Stats().MalformedChunks; got != uint64(len(bad)) {
		t.Fatalf("counted %d malformed chunks, want %d", got, len(bad))
	}
}

// TestStream_FloodEndsWithSlowConsumer asserts that a peer which floods a stream
// cannot grow it without bound: a stream whose buffer overflows ends with a
// slow-consumer error after the items buffered before the overflow, and a
// sibling stream on the same client is untouched. An oversize chunk trips the
// byte bound the same way.
func TestStream_FloodEndsWithSlowConsumer(t *testing.T) {
	t.Run("message bound", func(t *testing.T) {
		release := make(chan struct{})
		f := setup(t, mod(func(h *host) {
			h.script = func(h *host, m *nats.Msg) {
				if h.promptText(m) == "sibling" {
					h.ack(m)
					<-release
					h.text(m, "fine")
					h.end(m)
					return
				}
				h.ack(m)
				for i := 0; i < 100; i++ {
					h.text(m, "flood")
				}
				h.end(m)
			}
		}), caller.WithStreamBuffer(8, 0))
		sib, err := f.a.PromptText(ctxT(t), "sibling")
		if err != nil {
			t.Fatal(err)
		}
		st, err := f.a.PromptText(ctxT(t), "flood")
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "the overflow", func() bool { return f.c.Stats().SlowConsumers == 1 })
		evs, err := collect(t, st)
		var sc *caller.SlowConsumerError
		if !errors.As(err, &sc) || !errors.Is(err, caller.ErrSlowConsumer) {
			t.Fatalf("error %v", err)
		}
		if len(evs) == 0 || len(evs) > 8 {
			t.Fatalf("%d buffered events survived, want 1..8", len(evs))
		}
		close(release)
		sevs, err := collect(t, sib)
		if err != nil || texts(sevs) != "fine" {
			t.Fatalf("sibling stream: %q %v", texts(sevs), err)
		}
	})
	t.Run("byte bound", func(t *testing.T) {
		f := setup(t, mod(func(h *host) {
			h.script = func(h *host, m *nats.Msg) { h.ack(m); h.text(m, strings.Repeat("x", 8<<10)); h.end(m) }
		}), caller.WithStreamBuffer(0, 4<<10))
		st, err := f.a.PromptText(ctxT(t), "go")
		if err != nil {
			t.Fatal(err)
		}
		_, err = collect(t, st)
		if !errors.Is(err, caller.ErrSlowConsumer) {
			t.Fatalf("error %v", err)
		}
	})
}

// TestStream_CancelAndClose asserts cancellation by dropping the reply
// registration with nothing sent on the wire, the single-use rule, early exit
// from the loop, and that chunks after a cancel are dropped and counted.
//
// catalog: SAP-6.7-01
func TestStream_CancelAndClose(t *testing.T) {
	gate := make(chan struct{})
	f := setup(t, mod(func(h *host) {
		h.script = func(h *host, m *nats.Msg) {
			h.ack(m)
			<-gate
			h.text(m, "after the cancel")
			h.end(m)
		}
	}))
	sn := newSniffer(t, f.cnc)
	ctx, cancel := context.WithCancel(ctxT(t))
	st, err := f.a.PromptText(ctx, "go")
	if err != nil {
		t.Fatal(err)
	}
	next, stop := pull(st)
	defer stop()
	if ev, err := next(); err != nil || ev == nil {
		t.Fatalf("first event %v %v", ev, err)
	}
	cancel()
	if _, err := next(); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled stream ended with %v", err)
	}
	eventually(t, "the registration to drop", func() bool { return f.c.Stats().ActiveRoutes == 0 })
	published := len(sn.seen())
	close(gate)
	eventually(t, "the late chunks to be counted", func() bool { return f.c.Stats().DroppedReplies >= 2 })
	// After the cancel the caller put nothing on the wire: the only new
	// messages are the host's own chunks on the reply subject.
	for _, s := range sn.seen()[published:] {
		if !strings.HasPrefix(s, "_INBOX.agents.") {
			t.Fatalf("the caller published %q after a cancel", s)
		}
	}
	for _, m := range sn.msgs[published:] {
		if len(m.Header) > 0 || m.Reply != "" {
			t.Fatalf("unexpected message after the cancel: %+v", m)
		}
	}
	if st.Terminated() {
		t.Fatal("a canceled stream is not terminated")
	}
}

func TestStream_SingleUseAndEarlyExit(t *testing.T) {
	f := setup(t, mod(func(h *host) {
		h.script = func(h *host, m *nats.Msg) {
			h.ack(m)
			h.text(m, "one")
			h.text(m, "two")
			h.end(m)
		}
	}))
	st, err := f.a.PromptText(ctxT(t), "go")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for ev, err := range st.Events() {
		if err != nil {
			t.Fatal(err)
		}
		_ = ev
		n++
		break // leaving the loop closes the stream
	}
	if n != 1 {
		t.Fatal(n)
	}
	var got error
	for _, err := range st.Events() {
		got = err
	}
	if !errors.Is(got, caller.ErrStreamConsumed) {
		t.Fatalf("second Events: %v", got)
	}
	eventually(t, "the route to go", func() bool { return f.c.Stats().ActiveRoutes == 0 })
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestStream_CloseFromAnotherGoroutine asserts Close ends a ranging loop
// without error from another goroutine.
func TestStream_CloseFromAnotherGoroutine(t *testing.T) {
	f := setup(t, mod(func(h *host) { h.script = func(h *host, m *nats.Msg) { h.ack(m) } }))
	st, err := f.a.PromptText(ctxT(t), "go")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var last error
		for _, err := range st.Events() {
			last = err
		}
		done <- last
	}()
	time.Sleep(100 * time.Millisecond)
	_ = st.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close ended the loop with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end the loop")
	}
}

// TestStream_NoResponders asserts that a prompt nobody serves ends with the
// server's no-responders status instead of a wait for the inactivity timeout.
func TestStream_NoResponders(t *testing.T) {
	f := setup(t, nil)
	for _, s := range f.h.subs {
		if s.Subject == f.h.promptSubject {
			_ = s.Unsubscribe()
		}
	}
	_ = f.h.nc.Flush()
	start := time.Now()
	st, err := f.a.PromptText(ctxT(t), "go")
	if err != nil {
		t.Fatal(err)
	}
	_, err = collect(t, st)
	if !errors.Is(err, nats.ErrNoResponders) || time.Since(start) > 2*time.Second {
		t.Fatalf("error %v after %s", err, time.Since(start))
	}
}

// TestQuery_ExactlyOnceReply asserts that a mid-stream query takes exactly one
// reply: a plain-text reply, concurrent second attempts refused locally, one
// message published to the reply subject, and a text that begins with "{" sent
// as an envelope.
//
// catalog: SAP-7.2-01
func TestQuery_ExactlyOnceReply(t *testing.T) {
	var qsub *nats.Subscription
	answers := make(chan *nats.Msg, 8)
	inboxes := make(chan string, 1)
	f := setup(t, mod(func(h *host) {
		h.script = func(h *host, m *nats.Msg) {
			h.ack(m)
			inbox := nats.NewInbox()
			inboxes <- inbox
			var err error
			qsub, err = h.nc.Subscribe(inbox, func(a *nats.Msg) { answers <- a })
			if err != nil {
				return
			}
			_ = h.nc.Flush()
			h.chunk(m, "query", map[string]any{"id": "q-1", "reply_subject": inbox, "prompt": "Delete 200 files?", "extra": true})
			select {
			case a := <-answers:
				h.text(m, "answered: "+string(a.Data))
				answers <- a
			case <-time.After(5 * time.Second):
			}
			h.end(m)
		}
	}))
	st, err := f.a.PromptText(ctxT(t), "go")
	if err != nil {
		t.Fatal(err)
	}
	var evs []caller.Event
	var q *caller.Query
	for ev, err := range st.Events() {
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
		if qq, ok := ev.(*caller.Query); ok {
			q = qq
			if qq.ID != "q-1" || qq.Prompt != "Delete 200 files?" || qq.ReplySubject == "" {
				t.Fatalf("query %+v", qq)
			}
			if _, ok := qq.Extra["extra"]; !ok {
				t.Fatal("unknown query field not kept")
			}
			// Many concurrent replies: exactly one wins.
			errs := make(chan error, 8)
			for i := 0; i < 8; i++ {
				go func() { errs <- qq.Reply(ctxT(t), "yes") }()
			}
			ok, refused := 0, 0
			for i := 0; i < 8; i++ {
				switch err := <-errs; {
				case err == nil:
					ok++
				case errors.Is(err, caller.ErrQueryAlreadyReplied) && errors.Is(err, agents.ErrLocal):
					refused++
				default:
					t.Fatalf("reply: %v", err)
				}
			}
			if ok != 1 || refused != 7 {
				t.Fatalf("%d replies sent and %d refused, want 1 and 7", ok, refused)
			}
		}
	}
	if q == nil {
		t.Fatal("no query event")
	}
	if got := texts(evs); got != "answered: yes" {
		t.Fatalf("text %q", got)
	}
	// Everything published to the reply subject before this sentinel reaches
	// the subscription before it, whatever the caller did; so counting what is
	// there once the sentinel arrives is a fact, not a wait.
	if err := f.cnc.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.h.nc.Publish(<-inboxes, []byte("sentinel")); err != nil {
		t.Fatal(err)
	}
	n, sentinel := 0, false
	for deadline := time.After(5 * time.Second); !sentinel; {
		select {
		case a := <-answers:
			if string(a.Data) == "sentinel" {
				sentinel = true
			} else {
				n++
			}
		case <-deadline:
			t.Fatal("the sentinel never arrived")
		}
	}
	for len(answers) > 0 {
		a := <-answers
		if string(a.Data) != "sentinel" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d messages reached the reply subject, want exactly 1", n)
	}
	if err := q.ReplyEnvelope(ctxT(t), agents.Envelope{Prompt: "again"}); !errors.Is(err, caller.ErrQueryAlreadyReplied) {
		t.Fatalf("second reply by envelope: %v", err)
	}
	var qe *caller.QueryAlreadyRepliedError
	if err := q.Reply(ctxT(t), "again"); !errors.As(err, &qe) || qe.ID != "q-1" {
		t.Fatalf("typed error: %v", err)
	}
	_ = qsub
}

// TestQuery_ReplyForms asserts the two payload forms of a query reply (SAP §7.2).
func TestQuery_ReplyForms(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply func(*caller.Query) error
		want  func(*testing.T, []byte)
	}{
		{"plain text", func(q *caller.Query) error { return q.Reply(context.Background(), "yes") }, func(t *testing.T, b []byte) {
			t.Helper()
			if string(b) != "yes" {
				t.Fatalf("%q", b)
			}
		}},
		{"text that looks like json goes as an envelope", func(q *caller.Query) error { return q.Reply(context.Background(), `{"a":1}`) }, func(t *testing.T, b []byte) {
			t.Helper()
			env, err := agents.DecodeEnvelope(b)
			if err != nil || env.Prompt != `{"a":1}` || b[0] != '{' || !strings.Contains(string(b), `"prompt"`) {
				t.Fatalf("%q %v", b, err)
			}
		}},
		{"envelope with an attachment", func(q *caller.Query) error {
			return q.ReplyEnvelope(context.Background(), agents.Envelope{Prompt: "see", Attachments: []agents.Attachment{agents.NewAttachment("a.txt", []byte("hi"))}})
		}, func(t *testing.T, b []byte) {
			t.Helper()
			env, err := agents.DecodeEnvelope(b)
			if err != nil || len(env.Attachments) != 1 {
				t.Fatalf("%q %v", b, err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answers := make(chan *nats.Msg, 2)
			f := setup(t, mod(func(h *host) {
				h.script = func(h *host, m *nats.Msg) {
					h.ack(m)
					inbox := nats.NewInbox()
					s, _ := h.nc.Subscribe(inbox, func(a *nats.Msg) { answers <- a })
					_ = h.nc.Flush()
					h.chunk(m, "query", map[string]any{"id": "q", "reply_subject": inbox, "prompt": "?"})
					for i := 0; i < 600 && len(answers) == 0; i++ {
						time.Sleep(5 * time.Millisecond)
					}
					_ = s.Unsubscribe()
					h.end(m)
				}
			}))
			st, err := f.a.PromptText(ctxT(t), "go")
			if err != nil {
				t.Fatal(err)
			}
			for ev, err := range st.Events() {
				if err != nil {
					t.Fatal(err)
				}
				if q, ok := ev.(*caller.Query); ok {
					if err := tc.reply(q); err != nil {
						t.Fatal(err)
					}
				}
			}
			select {
			case a := <-answers:
				tc.want(t, a.Data)
			default:
				t.Fatal("no reply reached the host")
			}
		})
	}
}

// TestResponse_SaveAttachments asserts that received attachments are written
// safely: a hostile name cannot escape the directory.
func TestResponse_SaveAttachments(t *testing.T) {
	f := setup(t, mod(func(h *host) {
		h.script = func(h *host, m *nats.Msg) {
			h.ack(m)
			h.chunk(m, "response", map[string]any{"text": "files", "attachments": []map[string]any{
				{"filename": "../../escape.txt", "content": "aGk="},
				{"filename": "ok.txt", "content": "!!notbase64"},
			}})
			h.end(m)
		}
	}))
	st, err := f.a.PromptText(ctxT(t), "go")
	if err != nil {
		t.Fatal(err)
	}
	evs, err := collect(t, st)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "out")
	res, err := evs[1].(caller.Response).SaveAttachments(dir, agents.SaveOptions{})
	if err != nil || len(res) != 2 {
		t.Fatal(res, err)
	}
	if res[0].Path != filepath.Join(dir, "escape.txt") || res[1].Skipped != agents.SkipInvalidContent {
		t.Fatalf("%+v", res)
	}
	if b, err := os.ReadFile(res[0].Path); err != nil || string(b) != "hi" {
		t.Fatal(b, err)
	}
}

// TestStatus asserts the status request: a heartbeat-shaped reply, an error
// reply as a remote error, a reply that is no heartbeat as a protocol error and
// a silent host as a timeout.
func TestStatus(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		f := setup(t, nil)
		hb, err := f.a.Status(ctxT(t))
		if err != nil || hb.InstanceID != f.h.id || hb.IntervalS != 30 {
			t.Fatalf("%+v %v", hb, err)
		}
		m := <-f.h.statuses
		if len(m.Data) != 0 {
			t.Fatalf("the status request body is reserved and must be empty, got %q", m.Data)
		}
	})
	t.Run("error reply", func(t *testing.T) {
		f := setup(t, mod(func(h *host) {
			h.statusScript = func(h *host, m *nats.Msg) {
				_ = h.nc.PublishMsg(&nats.Msg{Subject: m.Reply, Header: nats.Header{agents.HeaderServiceErrorCode: {"500"}, agents.HeaderServiceError: {"broken"}}})
			}
		}))
		_, err := f.a.Status(ctxT(t))
		if !agents.IsCode(err, 500) {
			t.Fatal(err)
		}
	})
	t.Run("not a heartbeat", func(t *testing.T) {
		f := setup(t, mod(func(h *host) {
			h.statusScript = func(_ *host, m *nats.Msg) { _ = m.Respond([]byte(`{"hello":1}`)) }
		}))
		_, err := f.a.Status(ctxT(t))
		if !errors.Is(err, agents.ErrProtocol) {
			t.Fatal(err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		f := setup(t, mod(func(h *host) { h.statusScript = func(_ *host, _ *nats.Msg) {} }))
		_, err := f.a.Status(ctxT(t), caller.WithStatusTimeout(200*time.Millisecond))
		if !errors.Is(err, nats.ErrTimeout) {
			t.Fatal(err)
		}
	})
}

// TestQuery_ConcurrentQueries asserts that several queries may be in flight in
// one stream, each with its own reply subject, answered in any order.
func TestQuery_ConcurrentQueries(t *testing.T) {
	answers := make(chan string, 4)
	f := setup(t, mod(func(h *host) {
		h.script = func(h *host, m *nats.Msg) {
			h.ack(m)
			for _, id := range []string{"one", "two", "three"} {
				inbox := nats.NewInbox()
				_, _ = h.nc.Subscribe(inbox, func(a *nats.Msg) { answers <- id + "=" + string(a.Data) })
				_ = h.nc.Flush()
				h.chunk(m, "query", map[string]any{"id": id, "reply_subject": inbox, "prompt": "q " + id})
			}
			for i := 0; i < 600 && len(answers) < 3; i++ {
				time.Sleep(5 * time.Millisecond)
			}
			h.end(m)
		}
	}))
	st, err := f.a.PromptText(ctxT(t), "go")
	if err != nil {
		t.Fatal(err)
	}
	var queries []*caller.Query
	subjects := map[string]bool{}
	for ev, err := range st.Events() {
		if err != nil {
			t.Fatal(err)
		}
		if q, ok := ev.(*caller.Query); ok {
			queries = append(queries, q)
			subjects[q.ReplySubject] = true
			if len(queries) == 3 {
				for i := len(queries) - 1; i >= 0; i-- { // answer in reverse
					if err := queries[i].Reply(ctxT(t), "a-"+queries[i].ID); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	if len(queries) != 3 || len(subjects) != 3 {
		t.Fatalf("%d queries on %d reply subjects", len(queries), len(subjects))
	}
	got := map[string]bool{}
	for len(answers) > 0 {
		got[<-answers] = true
	}
	for _, id := range []string{"one", "two", "three"} {
		if !got[id+"=a-"+id] {
			t.Fatalf("answers %v", got)
		}
	}
}
