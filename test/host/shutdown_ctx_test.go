// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bogworks/synadia-agents.go/host"
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

// TestShutdown_CanceledContextDuringStartStillStops: a Shutdown with a
// canceled context that arrives while Start is still registering waits for
// Start's registration (bounded by Start's own context) and then stops what it
// registered; it never returns with the host left registered.
func TestShutdown_CanceledContextDuringStartStillStops(t *testing.T) {
	env := startNkeyServer(t)
	nc := env.conn(t, carol)
	const interval = 25 * time.Millisecond
	hb := subscribeHeartbeats(t, nc)
	bs := newBlockingSigner(signerOf(t, carol), 1)
	h, err := host.New(nc, subjectOf(t, testName), echo,
		host.WithHeartbeatInterval(interval), host.WithSubSecondHeartbeats(),
		host.WithIdentity(host.IdentityOptions{Signer: bs}))
	if err != nil {
		t.Fatal(err)
	}
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	startDone := make(chan error, 1)
	go func() { startDone <- h.Start(sctx) }()
	// The registration signature is free; the first beat's signature blocks,
	// so Start holds the lifecycle lock after it has registered.
	settle(t, 5*time.Second, "Start reaches the first beat", func() bool { return bs.calls.Load() >= 2 })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	shutDone := make(chan error, 1)
	go func() { shutDone <- h.Shutdown(ctx) }()
	select {
	case err := <-shutDone:
		t.Fatalf("Shutdown returned while Start was still registering: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(bs.release)
	if err := <-startDone; err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case err := <-shutDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after Start finished")
	}
	requireStoppedAfter(t, nc, hb, h, interval)
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
