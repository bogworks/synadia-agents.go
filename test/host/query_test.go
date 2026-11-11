// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host_test

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/nats-io/nats.go"
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// nextQuery reads frames from sub until the query chunk arrives.
func nextQuery(t *testing.T, sub *nats.Subscription) agents.QueryChunk {
	t.Helper()
	for {
		m, err := sub.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatalf("no query chunk: %v", err)
		}
		c, err := agents.DecodeChunk(m.Data)
		if err != nil {
			continue
		}
		if q, ok := c.(agents.QueryChunk); ok {
			return q
		}
	}
}

// TestQuery_RoundTrip: the handler pauses the stream with a question, the
// caller answers once on the fresh reply subject, and the stream goes on.
//
// catalog: SAP-7-01 SAP-7.1-01 SAP-7.1-02 SAP-7.1-03 SAP-7.1-04 SAP-7.3-05
func TestQuery_RoundTrip(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		ans, err := w.Ask(ctx, "which one?", 5*time.Second, host.WithAskAttachments(agents.NewAttachment("opts.txt", []byte("a,b"))))
		if err != nil {
			return err
		}
		return w.Send(ctx, "you said "+ans.Prompt)
	})
	sub, reply := openStream(t, nc)
	if err := nc.PublishMsg(&nats.Msg{Subject: h.Subject().Prompt(), Reply: reply, Data: []byte("go")}); err != nil {
		t.Fatal(err)
	}
	q := nextQuery(t, sub)
	if q.Prompt != "which one?" || !uuidRE.MatchString(q.ID) {
		t.Errorf("query: %+v", q)
	}
	if err := agents.ValidatePublishSubject(q.ReplySubject); err != nil || q.ReplySubject == reply || q.ReplySubject == "" {
		t.Errorf("reply subject %q: %v", q.ReplySubject, err)
	}
	if len(q.Attachments) != 1 || q.Attachments[0].Filename != "opts.txt" {
		t.Errorf("attachments: %+v", q.Attachments)
	}
	// The stream is still open: nothing terminates it before the reply.
	if m, err := sub.NextMsg(150 * time.Millisecond); err == nil {
		t.Fatalf("a message before the reply: %q", m.Data)
	}
	// No acknowledgement of the reply is defined: the reply is published with a
	// reply subject of its own, and nothing comes back on it.
	ackSub, ackReply := openStream(t, nc)
	if err := nc.PublishMsg(&nats.Msg{Subject: q.ReplySubject, Reply: ackReply, Data: []byte(`{"prompt":"B"}`)}); err != nil {
		t.Fatal(err)
	}
	if m, err := ackSub.NextMsg(300 * time.Millisecond); err == nil {
		t.Fatalf("the host acknowledged a query reply: %q", m.Data)
	}
	s := collect(t, sub, 5*time.Second)
	if got := s.texts(t); len(got) != 1 || got[0] != "you said B" {
		t.Fatalf("got %v", got)
	}
	if !s.last().terminator() {
		t.Error("the stream did not end with the terminator after the query")
	}
}

// TestQuery_ReplyFormsAndErrors: a reply is a plain text prompt or an
// envelope; one that is not either fails the Ask.
func TestQuery_ReplyFormsAndErrors(t *testing.T) {
	_, nc := setup(t)
	cases := []struct {
		name  string
		reply string
		want  string
		fail  bool
	}{
		{"plain text", "yes", "yes", false},
		{"envelope", `{"prompt":"maybe","x":1}`, "maybe", false},
		{"zero bytes", "", "", true},
		{"bad envelope", `{"nope":1}`, "", true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHostNamed(t, nc, "q"+string(rune('a'+i)), func(ctx context.Context, _ *host.Request, w *host.Responder) error {
				ans, err := w.Ask(ctx, "q?", 5*time.Second)
				if err != nil {
					return host.Reject(400, "bad reply")
				}
				return w.Send(ctx, ans.Prompt)
			})
			sub, reply := openStream(t, nc)
			if err := nc.PublishMsg(&nats.Msg{Subject: h.Subject().Prompt(), Reply: reply, Data: []byte("go")}); err != nil {
				t.Fatal(err)
			}
			q := nextQuery(t, sub)
			if err := nc.Publish(q.ReplySubject, []byte(tc.reply)); err != nil {
				t.Fatal(err)
			}
			s := collect(t, sub, 5*time.Second)
			if tc.fail {
				if se := s.errorFrame(); se == nil || se.Code != 400 {
					t.Fatalf("want 400, got %+v", se)
				}
				return
			}
			if got := s.texts(t); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

// TestQuery_Timeout: when the caller does not answer, Ask fails with
// ErrQueryTimeout and the handler decides: end the stream with an error, or go
// on with a default and give no signal.
//
// catalog: SAP-7.3-01 SAP-7.3-02
func TestQuery_Timeout(t *testing.T) {
	_, nc := setup(t)
	abort := newHost(t, nc, func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		if _, err := w.Ask(ctx, "anyone?", 100*time.Millisecond); errors.Is(err, host.ErrQueryTimeout) {
			return host.Reject(409, "no answer")
		}
		return nil
	})
	s := prompt(t, nc, abort, "x")
	if se := s.errorFrame(); se == nil || se.Code != 409 || !s.last().terminator() {
		t.Fatalf("abort path: %+v", se)
	}
	var timedOut bool
	cont := newHostNamed(t, nc, "cont", func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		_, err := w.Ask(ctx, "anyone?", 100*time.Millisecond)
		timedOut = errors.Is(err, host.ErrQueryTimeout)
		return w.Send(ctx, "default")
	})
	// A caller that ignores the query: it reads the stream and never answers.
	s = prompt(t, nc, cont, "x")
	if !timedOut || s.errorFrame() != nil {
		t.Fatalf("continue path: timedOut=%v err=%+v", timedOut, s.errorFrame())
	}
	var queries int
	for _, f := range s.Frames {
		if !f.terminator() && f.serviceError() == nil {
			if _, ok := f.chunk(t).(agents.QueryChunk); ok {
				queries++
			}
		}
	}
	if queries != 1 {
		t.Errorf("query chunks: %d", queries)
	}
	if got := s.texts(t); len(got) != 1 || got[0] != "default" {
		t.Errorf("got %v", got)
	}
}

// TestQuery_CancelledByContext: Ask returns at once when the handler's context
// ends, and unsubscribes its reply subject.
func TestQuery_CancelledByContext(t *testing.T) {
	_, nc := setup(t)
	res := make(chan error, 1)
	h := newHost(t, nc, func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		cctx, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
		defer cancel()
		_, err := w.Ask(cctx, "q", time.Minute)
		res <- err
		return nil
	})
	s := prompt(t, nc, h, "x")
	if err := <-res; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ask: %v", err)
	}
	if !s.last().terminator() {
		t.Fatal("no terminator")
	}
	if _, err := (&host.Responder{}).Ask(context.Background(), "q", 0); err == nil {
		t.Error("a zero timeout was accepted")
	}
}

// TestQuery_ConcurrentAsksHaveDistinctReplySubjects: several queries may be in
// flight in one stream, each with its own reply subject, and a query does not
// end the stream.
//
// catalog: SAP-7.3-03 SAP-7.3-04
func TestQuery_ConcurrentAsksHaveDistinctReplySubjects(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		var wg sync.WaitGroup
		answers := make([]string, 3)
		for i := range answers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ans, err := w.Ask(ctx, "q"+string(rune('0'+i)), 5*time.Second)
				if err == nil {
					answers[i] = ans.Prompt
				}
			}()
		}
		wg.Wait()
		return w.Send(ctx, answers[0]+answers[1]+answers[2])
	})
	sub, reply := openStream(t, nc)
	if err := nc.PublishMsg(&nats.Msg{Subject: h.Subject().Prompt(), Reply: reply, Data: []byte("go")}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for range 3 {
		q := nextQuery(t, sub)
		if seen[q.ReplySubject] {
			t.Fatalf("reply subject %q reused", q.ReplySubject)
		}
		seen[q.ReplySubject] = true
		// The answer echoes the query so the test knows which was which.
		if err := nc.Publish(q.ReplySubject, []byte("<"+q.Prompt+">")); err != nil {
			t.Fatal(err)
		}
	}
	s := collect(t, sub, 5*time.Second)
	if got := s.texts(t); len(got) != 1 || got[0] != "<q0><q1><q2>" {
		t.Fatalf("got %v", got)
	}
}

// TestCallerListening: a caller that subscribes to its reply subject is
// listening; one that is gone is not.
func TestCallerListening(t *testing.T) {
	srv, nc := setup(t)
	res := make(chan bool, 2)
	gate := make(chan struct{})
	h := newHost(t, nc, func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		ok, err := w.CallerListening(ctx, 300*time.Millisecond)
		if err != nil {
			return err
		}
		res <- ok
		<-gate
		ok, err = w.CallerListening(ctx, 300*time.Millisecond)
		if err != nil {
			return err
		}
		res <- ok
		return nil
	})
	caller := connect(t, srv)
	sub, reply := openStream(t, caller)
	if err := caller.PublishMsg(&nats.Msg{Subject: h.Subject().Prompt(), Reply: reply, Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-res:
		if !ok {
			t.Fatal("a listening caller looks gone")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no probe result")
	}
	_ = sub.Unsubscribe()
	if err := caller.Flush(); err != nil {
		t.Fatal(err)
	}
	close(gate)
	select {
	case ok := <-res:
		if ok {
			t.Fatal("a caller that stopped listening looks present")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no probe result")
	}
}
