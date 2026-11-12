// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bogworks/synadia-agents.go/host"
	"github.com/bogworks/synadia-agents.go/internal/hosttest"
	"github.com/nats-io/nats.go"
)

// requireStoppedAfter fails unless the host's prompt subject has no responders
// and no heartbeat is published for several intervals.
func requireStoppedAfter(t *testing.T, nc *nats.Conn, hb *nats.Subscription, h *host.Host, interval time.Duration) {
	t.Helper()
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	// Drop any beat published before the stop completed.
	for {
		if _, err := hb.NextMsg(0); err != nil {
			break
		}
	}
	if _, err := nc.Request(h.Subject().Prompt(), nil, time.Second); !errors.Is(err, nats.ErrNoResponders) {
		t.Fatalf("the prompt subject still has a responder after Shutdown returned: %v", err)
	}
	if m, err := hb.NextMsg(4 * interval); err == nil {
		t.Fatalf("a heartbeat was published after Shutdown returned: %s", m.Subject)
	}
}

// TestShutdown_CanceledContextStillStops: the context bounds only the wait for
// the handlers; a Shutdown whose context is already done still unregisters the
// service and stops the heartbeat before it returns.
func TestShutdown_CanceledContextStillStops(t *testing.T) {
	_, nc := setup(t)
	const interval = 25 * time.Millisecond
	hb := subscribeHeartbeats(t, nc)
	h := newHost(t, nc, echo, host.WithHeartbeatInterval(interval), host.WithSubSecondHeartbeats())
	if _, err := hb.NextMsg(2 * time.Second); err != nil {
		t.Fatalf("no heartbeat before Shutdown: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.Shutdown(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown: %v", err)
	}
	requireStoppedAfter(t, nc, hb, h, interval)
	if err := h.Close(); err != nil {
		t.Fatalf("Close after Shutdown: %v", err)
	}
}

// startBlocked starts a host whose first beat's signature blocks, so Start
// holds the lifecycle lock after it has registered. It returns the host, the
// signer (close its release to unblock Start), Start's result channel and the
// heartbeat subscription.
func startBlocked(t *testing.T, interval time.Duration, extra ...host.Option) (*host.Host, *blockingSigner, chan error, *nats.Conn, *nats.Subscription) {
	t.Helper()
	env := startNkeyServer(t)
	nc := env.conn(t, carol)
	hb := subscribeHeartbeats(t, nc)
	bs := newBlockingSigner(signerOf(t, carol), 1)
	opts := append([]host.Option{host.WithHeartbeatInterval(interval), host.WithSubSecondHeartbeats(),
		host.WithIdentity(host.IdentityOptions{Signer: bs})}, extra...)
	h, err := host.New(nc, subjectOf(t, testName), echo, opts...)
	if err != nil {
		t.Fatal(err)
	}
	sctx, scancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(scancel)
	startDone := make(chan error, 1)
	go func() { startDone <- h.Start(sctx) }()
	// The registration signature is free; the first beat's signature blocks.
	settle(t, 5*time.Second, "Start reaches the first beat", func() bool { return bs.calls.Load() >= 2 })
	return h, bs, startDone, nc, hb
}

// TestShutdown_CanceledContextDuringStartStillStops: a Shutdown with a
// canceled context that arrives while Start is blocked returns at once with the
// context error, and the stop still happens: Start sees the request, tears down
// what it registered, starts no heartbeat and returns ErrStopped.
func TestShutdown_CanceledContextDuringStartStillStops(t *testing.T) {
	const interval = 25 * time.Millisecond
	h, bs, startDone, nc, hb := startBlocked(t, interval)
	released := false
	defer func() {
		if !released {
			close(bs.release)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	begin := time.Now()
	err := h.Shutdown(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown during a blocked Start: %v, want an error wrapping context.Canceled", err)
	}
	if d := time.Since(begin); d > 500*time.Millisecond {
		t.Fatalf("Shutdown took %v while Start was blocked; the context must bound it", d)
	}
	select {
	case err := <-startDone:
		t.Fatalf("Start returned while its signer was blocked: %v", err)
	default:
	}
	close(bs.release)
	released = true
	select {
	case err := <-startDone:
		if !errors.Is(err, host.ErrStopped) {
			t.Fatalf("Start after a stop was requested: %v, want ErrStopped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after its signer was released")
	}
	requireStoppedAfter(t, nc, hb, h, interval)
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := h.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

// TestClose_ReturnsWithinBoundDuringBlockedStart: Close, like Shutdown, is
// bounded while Start is blocked (the close timeout is shortened through the
// internal test hook); the stop still happens when Start finishes.
func TestClose_ReturnsWithinBoundDuringBlockedStart(t *testing.T) {
	const interval = 25 * time.Millisecond
	h, bs, startDone, nc, hb := startBlockedWithCloseTimeout(t, interval, 200*time.Millisecond)
	released := false
	defer func() {
		if !released {
			close(bs.release)
		}
	}()
	begin := time.Now()
	err := h.Close()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close during a blocked Start: %v, want an error wrapping context.DeadlineExceeded", err)
	}
	if d := time.Since(begin); d > 2*time.Second {
		t.Fatalf("Close took %v with a 200ms bound", d)
	}
	close(bs.release)
	released = true
	if err := <-startDone; !errors.Is(err, host.ErrStopped) {
		t.Fatalf("Start: %v, want ErrStopped", err)
	}
	requireStoppedAfter(t, nc, hb, h, interval)
	if err := h.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func startBlockedWithCloseTimeout(t *testing.T, interval, d time.Duration) (*host.Host, *blockingSigner, chan error, *nats.Conn, *nats.Subscription) {
	t.Helper()
	return startBlocked(t, interval, hosttest.CloseTimeoutOption[host.Option](d))
}
