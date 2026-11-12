// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bogworks/synadia-agents.go/host"
	"github.com/nats-io/nats.go"
)

// holdPolicy is a reply-subject policy that blocks the micro callback that
// calls it, for the reply subjects with its prefix, until released. The
// callback is therefore "queued" across a Stop, deterministically.
type holdPolicy struct {
	prefix  string
	entered chan struct{}
	release chan struct{}
}

func newHoldPolicy(prefix string) *holdPolicy {
	return &holdPolicy{prefix: prefix, entered: make(chan struct{}, 16), release: make(chan struct{})}
}

func (p *holdPolicy) check(subject string) error {
	if strings.HasPrefix(subject, p.prefix) {
		p.entered <- struct{}{}
		<-p.release
	}
	return nil
}

func (p *holdPolicy) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the callback did not enter the policy")
	}
}

func stopWithin(h *host.Host, d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return h.Shutdown(ctx)
}

// TestStop_CloseWaitsForHandlerAfterTimedOutStop (audit findings 1, 11): a Stop
// that hit its deadline returns ctx.Err(), but a later Stop or Close still waits
// for the handler that ignores its context, and nothing is complete before.
func TestStop_CloseWaitsForHandlerAfterTimedOutStop(t *testing.T) {
	_, nc := setup(t)
	started, release := make(chan struct{}), make(chan struct{})
	var running atomic.Int32
	h := newHost(t, nc, func(_ context.Context, _ *host.Request, _ *host.Responder) error {
		running.Add(1)
		defer running.Add(-1)
		close(started)
		<-release // deliberately ignores ctx
		return nil
	})
	open := closeOnce(t, release)
	p := fire(t, nc, h, "x")
	<-started

	if err := stopWithin(h, 100*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Stop: %v", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- h.Close() }()
	stopped := make(chan error, 1)
	go func() { stopped <- h.Shutdown(context.Background()) }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned with a handler running: %v", err)
	case err := <-stopped:
		t.Fatalf("a later Stop returned with a handler running: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := stopWithin(h, 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a later Stop with a deadline: %v", err)
	}
	open()
	for _, c := range []chan error{closed, stopped} {
		select {
		case err := <-c:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Stop or Close did not return after the handler exited")
		}
	}
	if running.Load() != 0 {
		t.Error("completion signaled with a handler running")
	}
	collect(t, p.sub, 5*time.Second)
}

// TestStop_WaitsForQueuedPromptCallback (audit findings 2, 11): a prompt
// callback that has not finished when Stop is first called holds completion:
// Stop returns ctx.Err(), Close waits, and nothing is answered once Close has
// returned.
func TestStop_WaitsForQueuedPromptCallback(t *testing.T) {
	_, nc := setup(t)
	pol := newHoldPolicy("held.")
	h := newHost(t, nc, echo, host.WithReplySubjectPolicy(pol.check))
	open := closeOnce(t, pol.release)
	sub, err := nc.SubscribeSync("held.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := nc.PublishMsg(&nats.Msg{Subject: h.Subject().Prompt(), Reply: "held.1", Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	pol.waitEntered(t)

	if err := stopWithin(h, 100*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Stop: %v", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- h.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned with a callback in progress: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	open()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	// What the released callback answered was published before Close returned
	// (a stopping error and the terminator); drain it, then expect silence.
	for {
		if _, err := sub.NextMsg(300 * time.Millisecond); err != nil {
			break
		}
	}
	if _, err := nc.Request(h.Subject().Prompt(), []byte("late"), 300*time.Millisecond); err == nil {
		t.Error("a prompt was answered after Close returned")
	}
}

// TestStop_NoStatusReplyAfterShutdownBegins (audit finding 2): a status
// callback held across Stop does not answer once shutdown has begun.
func TestStop_NoStatusReplyAfterShutdownBegins(t *testing.T) {
	_, nc := setup(t)
	pol := newHoldPolicy("held.")
	h := newHost(t, nc, echo, host.WithReplySubjectPolicy(pol.check))
	open := closeOnce(t, pol.release)
	sub, err := nc.SubscribeSync("held.status")
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := nc.PublishMsg(&nats.Msg{Subject: h.Subject().Status(), Reply: "held.status"}); err != nil {
		t.Fatal(err)
	}
	pol.waitEntered(t)
	stopped := make(chan error, 1)
	go func() { stopped <- h.Shutdown(context.Background()) }()
	settle(t, 5*time.Second, "service stop", func() bool { return h.Service().Stopped() })
	open()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
	if m, err := sub.NextMsg(300 * time.Millisecond); err == nil {
		t.Errorf("status answered after shutdown began: %q", m.Data)
	}
}

// TestStart_FailureWaitsForInFlightPrompt (Codex H1): when Start fails after
// the prompt endpoint is registered and a prompt handler is already running,
// Start returns its error but neither Close nor a later Stop reports the host
// complete until that handler has returned.
func TestStart_FailureWaitsForInFlightPrompt(t *testing.T) {
	_, nc := setup(t)
	started, release := make(chan struct{}), make(chan struct{})
	var running atomic.Int32
	handler := func(_ context.Context, _ *host.Request, _ *host.Responder) error {
		running.Add(1)
		defer running.Add(-1)
		close(started)
		<-release // deliberately ignores ctx
		return nil
	}
	ctx, cancelStart := context.WithCancel(context.Background())
	defer cancelStart()
	var h *host.Host
	var once sync.Once
	var p pending
	// The first heartbeat is the last step of Start's registration; its extras
	// provider runs while the prompt endpoint is live. It delivers a prompt,
	// waits until the handler is running, then ends Start's context so the
	// first beat fails.
	extras := func() map[string]any {
		once.Do(func() {
			p = fire(t, nc, h, "x")
			select {
			case <-started:
			case <-time.After(5 * time.Second):
			}
			cancelStart()
		})
		return nil
	}
	var err error
	h, err = host.New(nc, subjectOf(t, testName), handler,
		host.WithoutKeepalive(), host.WithHeartbeatInterval(time.Hour), host.WithHeartbeatExtras(extras))
	if err != nil {
		t.Fatal(err)
	}
	open := closeOnce(t, release)
	t.Cleanup(func() { _ = h.Close() })

	if err := h.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start: %v, want context.Canceled", err)
	}
	select {
	case <-started:
	default:
		t.Fatal("the handler was not running when Start failed")
	}

	closed := make(chan error, 1)
	go func() { closed <- h.Close() }()
	stopped := make(chan error, 1)
	go func() { stopped <- h.Shutdown(context.Background()) }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned with a handler running: %v", err)
	case err := <-stopped:
		t.Fatalf("a later Stop returned with a handler running: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := stopWithin(h, 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a later Stop with a deadline: %v", err)
	}
	open()
	for _, c := range []chan error{closed, stopped} {
		select {
		case err := <-c:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Stop or Close did not return after the handler exited")
		}
	}
	if running.Load() != 0 {
		t.Error("completion signaled with a handler running")
	}
	collect(t, p.sub, 5*time.Second)
}

// TestShutdown_ReturnsContextErrorAtDeadlineAndLaterCloseWaits: Shutdown(ctx) with a
// handler that ignores its context returns the wrapped ctx error at the
// deadline; a later Close waits for the handler's real exit and then returns
// nil.
func TestShutdown_ReturnsContextErrorAtDeadlineAndLaterCloseWaits(t *testing.T) {
	_, nc := setup(t)
	started, release := make(chan struct{}), make(chan struct{})
	var running atomic.Int32
	h := newHost(t, nc, func(_ context.Context, _ *host.Request, _ *host.Responder) error {
		running.Add(1)
		defer running.Add(-1)
		close(started)
		<-release // deliberately ignores ctx
		return nil
	})
	open := closeOnce(t, release)
	p := fire(t, nc, h, "x")
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := h.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown at the deadline: %v, want context.DeadlineExceeded", err)
	}
	second := make(chan error, 1)
	go func() { second <- h.Close() }()
	select {
	case err := <-second:
		t.Fatalf("a second Close returned with a handler running: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	open()
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second Close did not return after the handler exited")
	}
	if running.Load() != 0 {
		t.Error("Close completed with a handler running")
	}
	collect(t, p.sub, 5*time.Second)
}
