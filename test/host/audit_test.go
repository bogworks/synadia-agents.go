// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
)

// blockingSigner signs normally for the first free calls (the registration
// signature), then blocks every call until release is closed.
type blockingSigner struct {
	identity.Signer
	free    int32
	calls   atomic.Int32
	release chan struct{}
}

func newBlockingSigner(s identity.Signer, free int32) *blockingSigner {
	return &blockingSigner{Signer: s, free: free, release: make(chan struct{})}
}

func (b *blockingSigner) Sign(data []byte) ([]byte, error) {
	if b.calls.Add(1) > b.free {
		<-b.release
	}
	return b.Signer.Sign(data)
}

// TestHeartbeat_StartHonoursContextWithBlockedSigner (audit finding 4): a
// signer that blocks during the first beat cannot hold Start past its context.
func TestHeartbeat_StartHonoursContextWithBlockedSigner(t *testing.T) {
	env := startNkeyServer(t)
	nc := env.conn(t, carol)
	bs := newBlockingSigner(signerOf(t, carol), 1)
	defer close(bs.release)
	h, err := host.New(nc, subjectOf(t, testName), echo, host.WithHeartbeatInterval(time.Hour),
		host.WithIdentity(host.IdentityOptions{Signer: bs}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	begin := time.Now()
	err = h.Start(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start: %v", err)
	}
	if time.Since(begin) > 3*time.Second {
		t.Errorf("Start held for %v", time.Since(begin))
	}
}

// TestHeartbeat_BlockedSignerSkipsBeatAndCloseDoesNotHang (audit finding 4):
// a signer that blocks past the signing timeout costs that beat (counted); the
// host stays up and Close returns while the signer is still blocked.
func TestHeartbeat_BlockedSignerSkipsBeatAndCloseDoesNotHang(t *testing.T) {
	env := startNkeyServer(t)
	nc := env.conn(t, carol)
	bs := newBlockingSigner(signerOf(t, carol), 1)
	h, err := host.New(nc, subjectOf(t, testName), echo,
		host.WithHeartbeatInterval(20*time.Millisecond), host.WithSubSecondHeartbeats(),
		host.WithHeartbeatSignTimeout(50*time.Millisecond),
		host.WithIdentity(host.IdentityOptions{Signer: bs}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Start(ctx); err != nil {
		t.Fatalf("a timed-out first beat must not fail Start: %v", err)
	}
	settle(t, 3*time.Second, "timeout counted", func() bool { return h.Stats().HeartbeatSignTimeouts >= 1 })
	if h.Stats().HeartbeatSignFailures < 1 {
		t.Error("a timeout is also a signing failure")
	}
	done := make(chan error, 1)
	go func() { done <- h.Close(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close hung on a blocked signer")
	}
	close(bs.release) // the abandoned goroutine ends
}

// TestKeepalive_OnByDefault (audit finding 3, reversed): a host that is not
// told otherwise matches the official hosts and keeps acking while a long
// handler runs. The default cadence is 30 seconds (asserted here on the
// constant, and on the default configuration in host_test.go); the recurrence
// is checked at a short configured cadence, so the test does not sleep 30s.
func TestKeepalive_OnByDefault(t *testing.T) {
	if host.DefaultKeepalive != 30*time.Second {
		t.Fatalf("DefaultKeepalive = %s, want 30s", host.DefaultKeepalive)
	}
	_, nc := setup(t)
	h, err := host.New(nc, subjectOf(t, testName), func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		time.Sleep(400 * time.Millisecond)
		return w.Send(ctx, "x")
	}, host.WithHeartbeatInterval(time.Hour), host.WithKeepalive(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	if n := prompt(t, nc, h, "x").acks(); n < 4 {
		t.Errorf("acks over a 400ms handler at a 50ms cadence: %d, want recurring acks", n)
	}
}

// TestReplyPolicy_PanicIsRefusal (audit finding 5): a panic in the policy is a
// counted refusal; the request is dropped and the host keeps serving.
func TestReplyPolicy_PanicIsRefusal(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, echo, host.WithReplySubjectPolicy(func(s string) error {
		if strings.HasPrefix(s, "boom.") {
			panic("policy bug")
		}
		return nil
	}))
	sub, err := nc.SubscribeSync("boom.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.PublishMsg(&nats.Msg{Subject: h.Subject().Prompt(), Reply: "boom.1", Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	settle(t, 3*time.Second, "refusal counted", func() bool { return h.Stats().RefusedReplies == 1 })
	if m, err := sub.NextMsg(100 * time.Millisecond); err == nil {
		t.Errorf("answered after a policy panic: %q", m.Data)
	}
	if got := prompt(t, nc, h, "ok").texts(t); len(got) != 1 {
		t.Errorf("host stopped serving: %v", got)
	}
}

// TestHandler_TypedNilRejectErrorIs500 (audit finding 6).
func TestHandler_TypedNilRejectErrorIs500(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, func(context.Context, *host.Request, *host.Responder) error {
		var rej *host.RejectError
		return rej
	})
	se := prompt(t, nc, h, "x").errorFrame()
	if se == nil || se.Code != 500 || se.Description != "handler error" {
		t.Fatalf("typed nil RejectError: %+v", se)
	}
	if got := prompt(t, nc, h, "y"); got.errorFrame() == nil || !got.last().terminator() {
		t.Error("host did not survive")
	}
}

// TestStats_PromptErrorsAndProcessingTime (audit finding 7): the host's own
// numbers count the errors sent for rejected prompts and the time slow prompts
// take, which micro's dispatch-only numbers do not.
func TestStats_PromptErrorsAndProcessingTime(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, func(ctx context.Context, req *host.Request, w *host.Responder) error {
		switch req.Envelope.Prompt {
		case "reject":
			return host.Reject(429, "slow down")
		case "slow":
			time.Sleep(150 * time.Millisecond)
		}
		return w.Send(ctx, "ok")
	})
	prompt(t, nc, h, "reject")
	prompt(t, nc, h, "slow")
	settle(t, 3*time.Second, "completion", func() bool { return h.Stats().PromptsCompleted == 2 })
	s := h.Stats()
	if s.PromptErrors != 1 || !strings.Contains(s.LastPromptError, "429") {
		t.Errorf("errors %d, last %q", s.PromptErrors, s.LastPromptError)
	}
	if s.PromptProcessingTime < 150*time.Millisecond {
		t.Errorf("processing time %v does not include the slow prompt", s.PromptProcessingTime)
	}
	// The same numbers reach the custom data of the micro stats.
	seen := false
	for _, e := range h.Service().Stats().Endpoints {
		if e.Name != agents.EndpointPrompt {
			continue
		}
		seen = true
		var data map[string]any
		if err := json.Unmarshal(e.Data, &data); err != nil {
			t.Fatal(err)
		}
		last, _ := data["last_error"].(string)
		pt, _ := data["processing_time_ns"].(float64)
		if data["errors"] != float64(1) || !strings.Contains(last, "429") || pt < 150e6 {
			t.Errorf("micro custom stats: %v", data)
		}
	}
	if !seen {
		t.Error("no prompt endpoint in the micro stats")
	}
}

// TestWithEndpoint_ClonesMetadata (audit finding 9): changing the map after New
// does not change what Start registers.
func TestWithEndpoint_ClonesMetadata(t *testing.T) {
	_, nc := setup(t)
	md := map[string]string{"k": "v"}
	h, err := host.New(nc, subjectOf(t, testName), echo, host.WithHeartbeatInterval(time.Hour), host.WithEndpoint(host.Endpoint{
		Name: "extra", Subject: "x.extra", Handler: nopHandler(), Metadata: md}))
	if err != nil {
		t.Fatal(err)
	}
	md["k"] = "changed"
	md["new"] = "x"
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	got := endpointOf(t, info(t, nc, h.InstanceID()), "extra").Metadata
	if len(got) != 1 || got["k"] != "v" {
		t.Errorf("registered metadata %v", got)
	}
}

// TestReplySubject_AgentsNamespaceOnlyLoops (audit finding 8): a reply subject
// under "agents." that cannot start a protocol loop is answered, for prompt and
// status alike; the host's own subjects and the prompt/status/hb shapes are not.
func TestReplySubject_AgentsNamespaceOnlyLoops(t *testing.T) {
	_, nc := setup(t)
	logger, logs := capturingLogger()
	h := newHost(t, nc, echo, host.WithLogger(logger))
	for _, reply := range []string{"agents.callers.me.inbox.1", "agents.reply.x"} {
		sub, err := nc.SubscribeSync(reply)
		if err != nil {
			t.Fatal(err)
		}
		if err := nc.Flush(); err != nil {
			t.Fatal(err)
		}
		if err := nc.PublishMsg(&nats.Msg{Subject: h.Subject().Prompt(), Reply: reply, Data: []byte("hi")}); err != nil {
			t.Fatal(err)
		}
		if got := collect(t, sub, 3*time.Second).texts(t); len(got) != 1 || got[0] != "echo: hi" {
			t.Errorf("prompt reply under %q: %v", reply, got)
		}
		if err := nc.PublishMsg(&nats.Msg{Subject: h.Subject().Status(), Reply: reply}); err != nil {
			t.Fatal(err)
		}
		m, err := sub.NextMsg(3 * time.Second)
		if err != nil {
			t.Fatalf("status reply under %q: %v", reply, err)
		}
		if _, err := agents.DecodeHeartbeat(m.Data); err != nil {
			t.Errorf("status reply: %v", err)
		}
	}
	// A refused status request is dropped and logged at warn.
	before := h.Stats().RefusedReplies
	own := []string{h.Subject().Prompt(), h.Subject().Status(), h.Subject().Heartbeat(), "agents.prompt.other.o.n", "agents.status.other.o.n"}
	for _, reply := range own {
		if err := nc.PublishMsg(&nats.Msg{Subject: h.Subject().Status(), Reply: reply}); err != nil {
			t.Fatal(err)
		}
	}
	settle(t, 3*time.Second, "refusals", func() bool { return h.Stats().RefusedReplies == before+uint64(len(own)) })
	if !strings.Contains(logs.String(), `level=WARN msg="request dropped: reply subject refused" kind=status`) {
		t.Errorf("no warn for a refused status request:\n%s", logs.String())
	}
}
