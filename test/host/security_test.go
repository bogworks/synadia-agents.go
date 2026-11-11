// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
)

// syncBuffer is a log sink safe for concurrent use.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func capturingLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// TestEndpoints_OverlapWithProtocolSubjectsRefused: an extra endpoint whose
// subject or pattern could match the host's prompt, status or heartbeat
// subject is a configuration error, so no extra handler sees a prompt that the
// sender gate has not classified.
func TestEndpoints_OverlapWithProtocolSubjectsRefused(t *testing.T) {
	_, nc := setup(t)
	subj := subjectOf(t, testName)
	cases := []struct {
		subject string
		refused bool
	}{
		{subj.Prompt(), true},
		{subj.Status(), true},
		{subj.Heartbeat(), true},
		{"agents.>", true},
		{"agents.prompt.>", true},
		{"agents.status.>", true},
		{"agents.*.test-agent.tester.t1", true},
		{"agents.prompt.*.*.*", true},
		{"agents.prompt.test-agent.>", true},
		{">", true},
		{"agents.prompt.other-agent.tester.t1", false},
		{"agents.prompt.test-agent.tester.t2", false},
		{"svc.custom", false},
		{"svc.>", false},
	}
	for _, tc := range cases {
		_, err := host.New(nc, subj, echo, host.WithEndpoint(host.Endpoint{
			Name: "extra", Subject: tc.subject, Handler: nopHandler()}))
		if tc.refused {
			if !errors.Is(err, host.ErrConfig) || !errors.Is(err, host.ErrEndpointOverlap) {
				t.Errorf("subject %q: want ErrConfig and ErrEndpointOverlap, got %v", tc.subject, err)
			}
		} else if err != nil {
			t.Errorf("subject %q: %v", tc.subject, err)
		}
	}
}

// TestReplySubject_DefaultPolicy: a prompt or status request whose reply subject
// is a system subject, a wildcard or another agent's protocol subject is
// dropped: nothing is published there, the handler never runs, and the host
// counts it. Ordinary inbox prefixes keep working.
func TestReplySubject_DefaultPolicy(t *testing.T) {
	_, nc := setup(t)
	var ran atomic.Int64
	logger, logs := capturingLogger()
	h := newHost(t, nc, func(ctx context.Context, req *host.Request, w *host.Responder) error {
		ran.Add(1)
		return w.Send(ctx, "ok")
	}, host.WithLogger(logger))

	victims := []string{"agents.prompt.victim.tester.v", "agents.status.victim.tester.v", "agents.hb.victim.tester.v", "$JS.victim"}
	var subs []*nats.Subscription
	for _, v := range victims {
		sub, err := nc.SubscribeSync(v)
		if err != nil {
			t.Fatal(err)
		}
		subs = append(subs, sub)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	refused := 0
	for _, v := range append(victims, "bad..subject", "wild.*") {
		for _, target := range []string{h.Subject().Prompt(), h.Subject().Status()} {
			// The client library rejects some subjects itself; those cannot reach a host.
			if err := nc.PublishMsg(&nats.Msg{Subject: target, Reply: v, Data: []byte("secret-payload")}); err == nil {
				refused++
			}
		}
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	settle(t, 3*time.Second, "refusals counted", func() bool { return h.Stats().RefusedReplies >= uint64(refused) })
	time.Sleep(100 * time.Millisecond)
	for i, sub := range subs {
		if m, err := sub.NextMsg(50 * time.Millisecond); err == nil {
			t.Errorf("host published to %q: %q", victims[i], m.Data)
		}
	}
	if ran.Load() != 0 || h.Stats().Prompts != 0 {
		t.Errorf("handler ran %d times for refused prompts", ran.Load())
	}
	if !strings.Contains(logs.String(), "level=WARN") || strings.Contains(logs.String(), "secret-payload") {
		t.Errorf("want a warn without the payload, got:\n%s", logs.String())
	}

	// A custom inbox prefix and the usual inbox both work.
	for _, reply := range []string{nats.NewInbox(), "my.own.inbox.prefix.1"} {
		sub, err := nc.SubscribeSync(reply)
		if err != nil {
			t.Fatal(err)
		}
		if err := nc.PublishMsg(&nats.Msg{Subject: h.Subject().Prompt(), Reply: reply, Data: []byte("hi")}); err != nil {
			t.Fatal(err)
		}
		s := collect(t, sub, 3*time.Second)
		if got := s.texts(t); len(got) != 1 || got[0] != "ok" {
			t.Errorf("reply %q: %v", reply, got)
		}
	}
	if _, hb := status(t, nc, h, nil, nil); hb.InstanceID == "" {
		t.Error("status broken")
	}
}

// TestReplySubject_CustomPolicy: WithReplySubjectPolicy replaces the default
// and a nil policy is a configuration error.
func TestReplySubject_CustomPolicy(t *testing.T) {
	_, nc := setup(t)
	if _, err := host.New(nc, subjectOf(t, testName), echo, host.WithReplySubjectPolicy(nil)); !errors.Is(err, host.ErrConfig) {
		t.Errorf("nil policy: %v", err)
	}
	h := newHost(t, nc, echo, host.WithReplySubjectPolicy(func(s string) error {
		if !strings.HasPrefix(s, "_INBOX.") {
			return errors.New("only _INBOX")
		}
		return nil
	}))
	if got := prompt(t, nc, h, "x").texts(t); len(got) != 1 {
		t.Errorf("inbox reply: %v", got)
	}
	other, err := nc.SubscribeSync("other.reply")
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.PublishMsg(&nats.Msg{Subject: h.Subject().Prompt(), Reply: "other.reply", Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	settle(t, 3*time.Second, "refusal counted", func() bool { return h.Stats().RefusedReplies == 1 })
	if m, err := other.NextMsg(100 * time.Millisecond); err == nil {
		t.Errorf("host answered a refused subject: %q", m.Data)
	}
}

// TestLifecycle_StartStopRace: Stop racing Start leaves no endpoint registered
// once both have returned, and never panics on a double close.
func TestLifecycle_StartStopRace(t *testing.T) {
	_, nc := setup(t)
	for i := range 150 {
		name := fmt.Sprintf("race%d", i)
		subj := subjectOf(t, name)
		extra := "svc.race." + name
		h, err := host.New(nc, subj, echo, host.WithoutKeepalive(), host.WithHeartbeatInterval(time.Hour),
			host.WithEndpoint(host.Endpoint{Name: "extra", Subject: extra, Handler: nopHandler()}))
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		start := make(chan struct{})
		go func() {
			defer wg.Done()
			<-start
			_ = h.Start(context.Background())
		}()
		go func() {
			defer wg.Done()
			<-start
			if i%3 == 0 {
				time.Sleep(time.Duration(i%7) * 100 * time.Microsecond)
			}
			if err := h.Stop(context.Background()); err != nil {
				t.Errorf("stop: %v", err)
			}
		}()
		close(start)
		wg.Wait()
		// Stop has returned; whatever order they ran in, nothing answers.
		if err := nc.Flush(); err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{subj.Status(), extra} {
			if _, err := nc.Request(s, nil, time.Second); !errors.Is(err, nats.ErrNoResponders) {
				t.Fatalf("iteration %d: %q answered after Stop returned: %v", i, s, err)
			}
		}
		if err := h.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := h.Start(context.Background()); !errors.Is(err, host.ErrStarted) {
			t.Fatalf("Start after Stop: %v", err)
		}
	}
}

// TestLifecycle_StartFailureThenStop: a Start that fails and a Stop after it
// do not close anything twice.
func TestLifecycle_StartFailureThenStop(t *testing.T) {
	_, nc := setup(t)
	h, err := host.New(nc, subjectOf(t, testName), echo)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = h.Start(ctx) // may fail on the canceled flush
	for range 3 {
		if err := h.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// TestInbound_SizeCheckedBeforeDecode: a payload over max_payload is answered
// 400 on its size alone, without being decoded (garbage that would fail the
// decode answers the size error), and the signed header's framed bytes count
// toward the limit.
func TestInbound_SizeCheckedBeforeDecode(t *testing.T) {
	_, nc := setup(t)
	const limit = 200
	h := newHost(t, nc, echo, host.WithMaxPayload(limit))
	s := rawPrompt(t, nc, h.Subject().Prompt(), bytes.Repeat([]byte("{"), limit+50), nil, 3*time.Second)
	se := s.errorFrame()
	if se == nil || se.Code != 400 || !strings.Contains(se.Description, "max_payload") {
		t.Fatalf("oversize garbage: %+v", se)
	}

	// A valid envelope exactly at the limit passes; with an Agent-Sender header
	// whose framed bytes push it over, it does not.
	pad := limit - len(`{"prompt":""}`)
	body := []byte(`{"prompt":"` + strings.Repeat("a", pad) + `"}`)
	if len(body) != limit {
		t.Fatalf("body %d", len(body))
	}
	if got := rawPrompt(t, nc, h.Subject().Prompt(), body, nil, 3*time.Second); got.errorFrame() != nil {
		t.Errorf("payload at the limit refused: %v", got.errorFrame())
	}
	hdr := nats.Header{}
	hdr.Set("Agent-Sender", `{"v":1,"user":"UXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"}`)
	over := rawPrompt(t, nc, h.Subject().Prompt(), body, hdr, 3*time.Second).errorFrame()
	if over == nil || over.Code != 400 || !strings.Contains(over.Description, "max_payload") {
		t.Errorf("payload plus header bytes over the limit: %+v", over)
	}
	// Checks off: no size enforcement.
	off := newHostNamed(t, nc, "off", echo, host.WithMaxPayload(limit), host.WithInboundChecks(false))
	if got := rawPrompt(t, nc, off.Subject().Prompt(), append([]byte(nil), body...), hdr, 3*time.Second); got.errorFrame() != nil && got.errorFrame().Code == 400 && strings.Contains(got.errorFrame().Description, "max_payload") {
		t.Error("size enforced with inbound checks off")
	}
}

// flakySigner signs until failing is set.
type flakySigner struct {
	identity.Signer
	failing atomic.Bool
}

func (f *flakySigner) Sign(data []byte) ([]byte, error) {
	if f.failing.Load() {
		return nil, errors.New("hsm unavailable")
	}
	return f.Signer.Sign(data)
}

// TestHeartbeat_SigningFailureSkipsBeat: a signer that fails costs the host
// that beat, which is not published unsigned, is counted and logged; beats
// resume signed when the signer recovers.
func TestHeartbeat_SigningFailureSkipsBeat(t *testing.T) {
	env := startNkeyServer(t)
	nc := env.conn(t, carol)
	obs := env.conn(t, alice)
	sub := subscribeHeartbeats(t, obs)
	signer := &flakySigner{Signer: signerOf(t, carol)}
	logger, logs := capturingLogger()
	h := newHost(t, nc, echo, host.WithIdentity(host.IdentityOptions{Signer: signer}), host.WithLogger(logger),
		host.WithHeartbeatInterval(40*time.Millisecond), host.WithSubSecondHeartbeats())
	if _, err := sub.NextMsg(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	signer.failing.Store(true)
	settle(t, 3*time.Second, "a signing failure counted", func() bool { return h.Stats().HeartbeatSignFailures >= 2 })
	// Drain what was published before the failure took hold, then expect only signed beats.
	for {
		m, err := sub.NextMsg(150 * time.Millisecond)
		if err != nil {
			break
		}
		if _, has := m.Header["Agent-Sender"]; !has {
			t.Fatalf("unsigned heartbeat published: %q", m.Data)
		}
	}
	if m, err := sub.NextMsg(250 * time.Millisecond); err == nil {
		t.Fatalf("heartbeat published while the signer fails (header %v)", m.Header)
	}
	if !strings.Contains(logs.String(), "heartbeat signing failed") {
		t.Errorf("failure not logged:\n%s", logs.String())
	}
	signer.failing.Store(false)
	m, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("beats did not resume: %v", err)
	}
	if _, has := m.Header["Agent-Sender"]; !has {
		t.Error("resumed beat is unsigned")
	}
}

// TestHandlerError_ProtocolReasonStaysInLog: a handler's ProtocolError reason
// never reaches the caller; it goes to the logger.
func TestHandlerError_ProtocolReasonStaysInLog(t *testing.T) {
	_, nc := setup(t)
	logger, logs := capturingLogger()
	h := newHost(t, nc, func(context.Context, *host.Request, *host.Responder) error {
		return fmt.Errorf("parse /etc/secret/token: %w", &agents.ProtocolError{Reason: "token abc123 at /etc/secret"})
	}, host.WithLogger(logger))
	s := prompt(t, nc, h, "x")
	se := s.errorFrame()
	if se == nil || se.Code != 400 || se.Description != "malformed request" {
		t.Fatalf("error frame %+v", se)
	}
	for _, f := range s.Frames {
		if strings.Contains(string(f.Data), "abc123") || strings.Contains(fmt.Sprint(f.Header), "abc123") {
			t.Error("reason reached the caller")
		}
	}
	if !strings.Contains(logs.String(), "abc123") {
		t.Errorf("reason not logged:\n%s", logs.String())
	}
}
