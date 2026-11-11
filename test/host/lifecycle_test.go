// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bogworks/synadia-agents.go/host"
	"github.com/nats-io/nats.go"
)

// TestStop_WaitsForInFlightPrompts: Stop lets the handlers in flight finish,
// and their streams complete normally; new prompts are no longer served.
func TestStop_WaitsForInFlightPrompts(t *testing.T) {
	_, nc := setup(t)
	g := newGated()
	h := newHost(t, nc, g.handle)
	p := fire(t, nc, h, "finish me")
	g.waitStarted(t, 1)

	stopped := make(chan error, 1)
	go func() { stopped <- h.Stop(context.Background()) }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned with a handler in flight: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	// A prompt arriving now is not served: refused or unrouted.
	if _, err := nc.Request(h.Subject().Prompt(), []byte("late"), 300*time.Millisecond); err == nil {
		t.Log("a late prompt got a reply (an error frame is allowed)")
	}
	close(g.release)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the handler finished")
	}
	s := collect(t, p.sub, 5*time.Second)
	if got := s.texts(t); len(got) != 1 || got[0] != "finish me" || s.errorFrame() != nil {
		t.Fatalf("in-flight stream: %v %+v", got, s.errorFrame())
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Errorf("Close after Stop: %v", err)
	}
	if !h.Service().Stopped() {
		t.Error("the micro service is still running")
	}
	if nc.IsClosed() {
		t.Error("Stop closed the connection")
	}
}

// TestStop_CancelsHandlersAtDeadline: when ctx ends before the handlers do,
// Stop cancels their contexts and returns ctx.Err(); a canceled handler's
// stream still gets an error frame and the terminator.
func TestStop_CancelsHandlersAtDeadline(t *testing.T) {
	_, nc := setup(t)
	g := newGated()
	h := newHost(t, nc, g.handle)
	p := fire(t, nc, h, "x")
	g.waitStarted(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := h.Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop: %v", err)
	}
	s := collect(t, p.sub, 5*time.Second)
	se := s.errorFrame()
	if se == nil || se.Code != 500 || se.Description != "service stopping" || !s.last().terminator() {
		t.Fatalf("stream of a canceled handler: %+v", se)
	}
	settle(t, 5*time.Second, "handler exit", func() bool { return g.running.Load() == 0 })
	if err := h.Stop(context.Background()); err != nil {
		t.Errorf("Stop after forced stop: %v", err)
	}
}

// TestClose_CancelsAndWaits: Close cancels the handlers' contexts and returns
// once they have returned.
func TestClose_CancelsAndWaits(t *testing.T) {
	_, nc := setup(t)
	g := newGated()
	h := newHost(t, nc, g.handle)
	p := fire(t, nc, h, "x")
	g.waitStarted(t, 1)
	done := make(chan error, 1)
	go func() { done <- h.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	if g.running.Load() != 0 {
		t.Error("Close returned with a handler running")
	}
	if se := collect(t, p.sub, 5*time.Second).errorFrame(); se == nil || se.Code != 500 {
		t.Errorf("stream: %+v", se)
	}
	if err := h.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestStop_BeforeStartAndIdempotent: Stop and Close on a host that never
// started are no-ops that keep it from starting later.
func TestStop_BeforeStartAndIdempotent(t *testing.T) {
	_, nc := setup(t)
	h, err := host.New(nc, subjectOf(t, testName), echo)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(context.Background()); !errors.Is(err, host.ErrStarted) {
		t.Fatalf("Start after Stop: %v", err)
	}
}

// TestStop_ConcurrentCallersAllReturn: several Stop calls at once all return.
func TestStop_ConcurrentCallersAllReturn(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, echo)
	errs := make(chan error, 4)
	for range 4 {
		go func() { errs <- h.Stop(context.Background()) }()
	}
	for range 4 {
		select {
		case err := <-errs:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a Stop call hung")
		}
	}
}

// TestStop_NoGoroutineLeak: starting, serving prompts with keep-alives, queries
// and heartbeats, and stopping leaves no goroutine behind.
func TestStop_NoGoroutineLeak(t *testing.T) {
	srv := startServer(t)
	nc := connect(t, srv)
	caller := connect(t, srv)
	check := goroutineBaseline(t)
	var n atomic.Int32
	h, err := host.New(nc, subjectOf(t, testName), func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		n.Add(1)
		time.Sleep(40 * time.Millisecond)
		if _, err := w.Ask(ctx, "q", 30*time.Millisecond); err == nil {
			return errors.New("unexpected answer")
		}
		_, _ = w.CallerListening(ctx, 30*time.Millisecond)
		return w.Send(ctx, "x")
	}, host.WithKeepalive(10*time.Millisecond), host.WithHeartbeatInterval(20*time.Millisecond), host.WithSubSecondHeartbeats(),
		host.WithEndpoint(host.Endpoint{Name: "extra", Subject: "x.extra", Handler: nopHandler()}))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		sub, reply := openStream(t, caller)
		if err := caller.PublishMsg(&nats.Msg{Subject: h.Subject().Prompt(), Reply: reply, Data: []byte("x")}); err != nil {
			t.Fatal(err)
		}
		collect(t, sub, 5*time.Second)
		_ = sub.Unsubscribe()
	}
	status(t, caller, h, nil, nil)
	// One more, canceled mid-flight by Stop.
	sub, reply := openStream(t, caller)
	if err := caller.PublishMsg(&nats.Msg{Subject: h.Subject().Prompt(), Reply: reply, Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	settle(t, 2*time.Second, "sixth prompt", func() bool { return n.Load() == 6 })
	if len(ownGoroutines()) == 0 {
		t.Fatal("the leak check sees no SDK goroutine while the host runs")
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	collect(t, sub, 5*time.Second)
	_ = sub.Unsubscribe()
	check()
}

// TestStop_NoLeakWhenStartFails: a failed Start leaves nothing running.
func TestStop_NoLeakWhenStartFails(t *testing.T) {
	env := startNkeyServer(t)
	nc := env.conn(t, carol)
	check := goroutineBaseline(t)
	h, err := host.New(nc, subjectOf(t, testName), echo, host.WithIdentity(host.IdentityOptions{Signer: signerOf(t, alice)}))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(context.Background()); err == nil {
		t.Fatal("Start succeeded")
	}
	check()
}
