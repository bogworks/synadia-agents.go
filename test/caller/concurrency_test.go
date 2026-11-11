// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/nats-io/nats.go"
)

// TestConcurrentPrompts_Isolation is the cross-prompt isolation test of
// REQ-019: many prompts run at once on one client and one connection, each is
// answered through the client's single multiplexed reply subscription, and no
// stream ever sees another's chunks.
func TestConcurrentPrompts_Isolation(t *testing.T) {
	const prompts, chunks = 64, 12
	f := setup(t, mod(func(h *host) {
		h.script = func(h *host, m *nats.Msg) {
			id := h.promptText(m)
			h.ack(m)
			for k := 0; k < chunks; k++ {
				time.Sleep(time.Duration(rand.IntN(3)) * time.Millisecond)
				h.text(m, fmt.Sprintf("%s#%02d;", id, k))
			}
			h.end(m)
		}
	}))
	subsBefore := f.cnc.NumSubscriptions()
	var wg sync.WaitGroup
	errs := make(chan error, prompts)
	for i := 0; i < prompts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("p%03d", i)
			st, err := f.a.PromptText(ctxT(t), id)
			if err != nil {
				errs <- err
				return
			}
			defer st.Close()
			var got strings.Builder
			for ev, err := range st.Events() {
				if err != nil {
					errs <- fmt.Errorf("%s: %w", id, err)
					return
				}
				if r, ok := ev.(caller.Response); ok {
					got.WriteString(r.Text)
				}
			}
			var want strings.Builder
			for k := 0; k < chunks; k++ {
				fmt.Fprintf(&want, "%s#%02d;", id, k)
			}
			if got.String() != want.String() || !st.Terminated() {
				errs <- fmt.Errorf("%s saw %q, want %q", id, got.String(), want.String())
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := f.cnc.NumSubscriptions(); n != subsBefore {
		t.Fatalf("subscriptions went from %d to %d: prompts must share the client's one reply subscription", subsBefore, n)
	}
	if s := f.c.Stats(); s.DroppedReplies != 0 || s.ActiveRoutes != 0 {
		t.Fatalf("stats %+v", s)
	}
}

// TestLateReply_DroppedCountedNeverMisdelivered is the late-reply test of
// REQ-019: a reply that arrives after its prompt timed out is dropped and
// counted, and is never delivered to another prompt on the same client, nor is
// a reply to a token the client never issued.
func TestLateReply_DroppedCountedNeverMisdelivered(t *testing.T) {
	var mu sync.Mutex
	var slowReply string
	released := make(chan struct{})
	f := setup(t, mod(func(h *host) {
		h.script = func(h *host, m *nats.Msg) {
			switch h.promptText(m) {
			case "slow":
				mu.Lock()
				slowReply = m.Reply
				mu.Unlock()
				h.ack(m)
				<-released // the reply comes long after the caller gave up
				h.text(m, "LATE-1")
				h.text(m, "LATE-2")
				h.end(m)
			default:
				h.ack(m)
				<-released
				time.Sleep(50 * time.Millisecond)
				h.text(m, "fast reply")
				h.end(m)
			}
		}
	}))
	slow, err := f.a.PromptText(ctxT(t), "slow", caller.WithPromptInactivityTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	// A second prompt is in flight on the same client while the late reply lands.
	other, err := f.a.PromptText(ctxT(t), "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collect(t, slow); err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("the slow prompt should time out, got %v", err)
	}
	mu.Lock()
	reply := slowReply
	mu.Unlock()
	if reply == "" {
		t.Fatal("the host did not see the slow prompt")
	}

	// A token the client never issued, under the same prefix.
	forged := reply[:strings.LastIndex(reply, ".")+1] + "neverissued"
	if err := f.cnc.Publish(forged, []byte(`{"type":"response","data":"FORGED"}`)); err != nil {
		t.Fatal(err)
	}
	close(released)
	// The late chunks of the timed-out prompt and the forged one: dropped, counted.
	eventually(t, "late replies to be counted", func() bool { return f.c.Stats().DroppedReplies >= 4 })
	evs, err := collect(t, other)
	if err != nil {
		t.Fatal(err)
	}
	if got := texts(evs); got != "fast reply" {
		t.Fatalf("the other prompt received %q: a late or forged reply was delivered elsewhere", got)
	}
}

// TestClient_CloseDrainsAndLeaksNothing asserts that Close ends the streams in
// flight with ErrClosed, refuses new work, drains every subscription and leaves
// no goroutine behind; and that Close is idempotent and does not close the
// connection. Leaks are asserted on what the client owns: its routes and its
// subscriptions.
func TestClient_CloseDrainsAndLeaksNothing(t *testing.T) {
	srv := startServer(t)
	hnc, cnc := connect(t, srv), connect(t, srv)
	h := newHost(t, hnc, func(h *host) {
		h.script = func(h *host, m *nats.Msg) {
			h.ack(m)
			if h.promptText(m) == "hang" {
				return
			}
			h.text(m, "done")
			h.end(m)
		}
	}).start()
	subsBase := cnc.NumSubscriptions()

	c, err := caller.New(cnc)
	if err != nil {
		t.Fatal(err)
	}
	ctx := ctxT(t)
	found, err := c.Discover(ctx, caller.WithDiscoverTimeout(200*time.Millisecond))
	if err != nil || len(found) != 1 {
		t.Fatal(found, err)
	}
	a := found[0]
	if _, err := a.Status(ctx); err != nil {
		t.Fatal(err)
	}
	// Finished, abandoned (never ranged), canceled and broken-off streams.
	st, _ := a.PromptText(ctx, "one")
	if _, err := collect(t, st); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PromptText(ctx, "abandoned"); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	if _, err := a.PromptText(cctx, "never ranged"); err != nil {
		t.Fatal(err)
	}
	cancel()
	br, _ := a.PromptText(ctx, "two")
	for range br.Events() {
		break
	}
	hang, err := a.PromptText(ctx, "hang")
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() {
		var last error
		for _, err := range hang.Events() {
			last = err
		}
		ended <- last
	}()
	time.Sleep(100 * time.Millisecond)

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		if !errors.Is(err, caller.ErrClosed) {
			t.Fatalf("the stream in flight ended with %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end the stream in flight")
	}
	if err := c.Close(); err != nil || !c.IsClosed() {
		t.Fatalf("second Close: %v", err)
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("Done is not closed")
	}
	if _, err := a.PromptText(ctx, "late"); !errors.Is(err, caller.ErrClosed) {
		t.Fatalf("prompt after Close: %v", err)
	}
	if _, err := c.Discover(ctx); !errors.Is(err, caller.ErrClosed) {
		t.Fatalf("discover after Close: %v", err)
	}
	if cnc.IsClosed() {
		t.Fatal("Close closed the caller's connection")
	}
	eventually(t, "every subscription to be drained", func() bool { return cnc.NumSubscriptions() == subsBase })
	// Caller-owned state only: no route is left and the one reply subscription
	// is gone (the process-wide goroutine count says nothing about whose
	// goroutine it is).
	if n := c.Stats().ActiveRoutes; n != 0 {
		t.Fatalf("%d routes left after Close", n)
	}
	_ = h
}

// TestClient_DrainWaitsForStreams asserts that Drain refuses new prompts,
// waits for a stream in flight to finish on its own and then closes; and that
// on its context's expiry it closes at once.
func TestClient_DrainWaitsForStreams(t *testing.T) {
	gate := make(chan struct{})
	f := setup(t, mod(func(h *host) {
		h.script = func(h *host, m *nats.Msg) {
			h.ack(m)
			<-gate
			h.text(m, "finished")
			h.end(m)
		}
	}))
	st, err := f.a.PromptText(ctxT(t), "go")
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan error, 1)
	go func() { drained <- f.c.Drain(ctxT(t)) }()
	eventually(t, "new prompts to be refused", func() bool {
		extra, err := f.a.PromptText(ctxT(t), "refused")
		if err == nil {
			_ = extra.Close() // drain had not started yet
		}
		return errors.Is(err, caller.ErrClosed)
	})
	select {
	case err := <-drained:
		t.Fatalf("Drain returned %v while a stream was in flight", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	evs, err := collect(t, st)
	if err != nil || texts(evs) != "finished" {
		t.Fatalf("the in-flight stream: %q %v", texts(evs), err)
	}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if !f.c.IsClosed() {
		t.Fatal("Drain did not close the client")
	}

	// Context expiry forces the close.
	srv := startServer(t)
	hnc, cnc := connect(t, srv), connect(t, srv)
	h := newHost(t, hnc, func(h *host) { h.script = func(h *host, m *nats.Msg) { h.ack(m) } }).start()
	c := newClient(t, cnc)
	a, err := c.LookupInstance(ctxT(t), h.id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.PromptText(ctxT(t), "stuck"); err != nil {
		t.Fatal(err)
	}
	dctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := c.Drain(dctx); !errors.Is(err, context.DeadlineExceeded) || !c.IsClosed() {
		t.Fatalf("Drain on expiry: %v closed=%v", err, c.IsClosed())
	}
}

// TestTracker_StopDrainsAndRestarts asserts the tracker's lifecycle on its own:
// Start is idempotent, Stop drains, and the subscription is gone and the
// tracker reports itself stopped.
func TestTracker_StopDrainsAndRestarts(t *testing.T) {
	srv := startServer(t)
	hnc, cnc := connect(t, srv), connect(t, srv)
	h := newHost(t, hnc)
	subs := cnc.NumSubscriptions()
	tr, err := caller.NewHeartbeatTracker(cnc)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := tr.Start(ctxT(t)); err != nil {
			t.Fatal(err)
		}
	}
	if cnc.NumSubscriptions() != subs+1 || !tr.Started() {
		t.Fatalf("Start twice made %d subscriptions", cnc.NumSubscriptions()-subs)
	}
	h.publishHeartbeat(nil)
	eventually(t, "the heartbeat", func() bool { _, ok := tr.Liveness(h.id); return ok })
	if err := tr.Stop(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if err := tr.Stop(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the subscription to go", func() bool { return cnc.NumSubscriptions() == subs })
	if tr.Started() {
		t.Fatal("still started")
	}
	if err := tr.Start(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	_ = tr.Stop(ctxT(t))
}
