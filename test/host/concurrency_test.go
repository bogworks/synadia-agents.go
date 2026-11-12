// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/nats-io/nats.go"
)

// gatedHandler blocks every call until release is closed and counts the calls
// running at once.
type gatedHandler struct {
	release chan struct{}
	running atomic.Int32
	peak    atomic.Int32
	started chan struct{}
}

func newGated() *gatedHandler {
	return &gatedHandler{release: make(chan struct{}), started: make(chan struct{}, 1000)}
}

func (g *gatedHandler) handle(ctx context.Context, req *host.Request, w *host.Responder) error {
	n := g.running.Add(1)
	defer g.running.Add(-1)
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			break
		}
	}
	g.started <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return w.Send(ctx, req.Envelope.Prompt)
}

func (g *gatedHandler) waitStarted(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-g.started:
		case <-time.After(5 * time.Second):
			t.Fatal("handlers did not start")
		}
	}
}

type pending struct {
	sub *nats.Subscription
}

func fire(t *testing.T, nc *nats.Conn, h *host.Host, text string) pending {
	t.Helper()
	sub, reply := openStream(t, nc)
	if err := nc.PublishMsg(&nats.Msg{Subject: h.Subject().Prompt(), Reply: reply, Data: []byte(text)}); err != nil {
		t.Fatal(err)
	}
	return pending{sub}
}

// TestConcurrency_ServesPromptsAtOnce: a handler runs once per request, on its
// own goroutine, so prompts are served concurrently up to the limit.
func TestConcurrency_ServesPromptsAtOnce(t *testing.T) {
	_, nc := setup(t)
	g := newGated()
	h := newHost(t, nc, g.handle)
	const n = 20
	ps := make([]pending, n)
	for i := range ps {
		ps[i] = fire(t, nc, h, fmt.Sprintf("p%d", i))
	}
	g.waitStarted(t, n)
	if g.running.Load() != n {
		t.Fatalf("running %d, want %d at once", g.running.Load(), n)
	}
	if s := h.Stats(); s.InFlight != n {
		t.Errorf("in flight %d", s.InFlight)
	}
	close(g.release)
	for i, p := range ps {
		s := collect(t, p.sub, 5*time.Second)
		if got := s.texts(t); len(got) != 1 || got[0] != fmt.Sprintf("p%d", i) {
			t.Errorf("prompt %d got %v", i, got)
		}
	}
}

// TestConcurrency_QueuePolicy: with every slot busy under the default policy,
// further prompts wait, none is refused, and they run as slots free.
func TestConcurrency_QueuePolicy(t *testing.T) {
	_, nc := setup(t)
	g := newGated()
	h := newHost(t, nc, g.handle, host.WithMaxConcurrentPrompts(2))
	ps := make([]pending, 5)
	for i := range ps {
		ps[i] = fire(t, nc, h, fmt.Sprintf("q%d", i))
	}
	g.waitStarted(t, 2)
	time.Sleep(200 * time.Millisecond)
	if g.running.Load() != 2 {
		t.Fatalf("running %d, limit is 2", g.running.Load())
	}
	// Only the two served prompts have been acked.
	acked := 0
	for _, p := range ps {
		if _, err := p.sub.NextMsg(20 * time.Millisecond); err == nil {
			acked++
		}
	}
	if acked != 2 {
		t.Errorf("%d prompts acked while the limit is 2", acked)
	}
	close(g.release)
	done := 0
	for _, p := range ps {
		// Skip what was already read above.
		for {
			m, err := p.sub.NextMsg(5 * time.Second)
			if err != nil {
				t.Fatalf("a queued prompt was never served: %v", err)
			}
			if (frame{Data: m.Data, Header: m.Header}).terminator() {
				done++
				break
			}
		}
	}
	if done != 5 || g.peak.Load() > 2 {
		t.Errorf("done %d, peak concurrency %d", done, g.peak.Load())
	}
	if h.Stats().RejectedBusy != 0 {
		t.Error("the queue policy refused a prompt")
	}
}

// TestConcurrency_Reject429Policy: with every slot busy, a prompt is answered
// 429 and the terminator at once, with no ack and no handler call.
func TestConcurrency_Reject429Policy(t *testing.T) {
	_, nc := setup(t)
	g := newGated()
	h := newHost(t, nc, g.handle, host.WithMaxConcurrentPrompts(1), host.WithBusyPolicy(host.BusyReject429))
	first := fire(t, nc, h, "first")
	g.waitStarted(t, 1)
	s := rawPrompt(t, nc, h.Subject().Prompt(), []byte("second"), nil, 2*time.Second)
	se := s.errorFrame()
	if se == nil || se.Code != 429 || !errors.Is(se, agents.ErrRateLimited) {
		t.Fatalf("want 429, got %+v", se)
	}
	if len(s.Frames) != 2 || s.acks() != 0 || !s.last().terminator() {
		t.Errorf("shape: %d frames, %d acks", len(s.Frames), s.acks())
	}
	if h.Stats().RejectedBusy != 1 {
		t.Errorf("rejected %d", h.Stats().RejectedBusy)
	}
	close(g.release)
	if got := collect(t, first.sub, 5*time.Second).texts(t); len(got) != 1 || got[0] != "first" {
		t.Errorf("the first prompt: %v", got)
	}
	// The slot is free again: the host replies before it releases the slot, so
	// wait for that release rather than race it.
	awaitIdle(t, h)
	if got := prompt(t, nc, h, "third").texts(t); len(got) != 1 {
		t.Errorf("after release: %v", got)
	}
}

// TestConcurrency_StatusServedOutsideTheLimit: with every slot busy and
// prompts queued, status is still answered at once.
func TestConcurrency_StatusServedOutsideTheLimit(t *testing.T) {
	_, nc := setup(t)
	g := newGated()
	h := newHost(t, nc, g.handle, host.WithMaxConcurrentPrompts(1))
	fire(t, nc, h, "a")
	fire(t, nc, h, "b")
	fire(t, nc, h, "c")
	g.waitStarted(t, 1)
	start := time.Now()
	_, hb := status(t, nc, h, nil, nil)
	if hb.InstanceID != h.InstanceID() || time.Since(start) > time.Second {
		t.Errorf("status took %v", time.Since(start))
	}
	close(g.release)
}

// TestConcurrency_ManyPromptsRace: many concurrent prompts under -race get
// their own, uncrossed streams.
func TestConcurrency_ManyPromptsRace(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, func(ctx context.Context, req *host.Request, w *host.Responder) error {
		for i := range 3 {
			if err := w.Send(ctx, fmt.Sprintf("%s-%d", req.Envelope.Prompt, i)); err != nil {
				return err
			}
		}
		return nil
	}, host.WithMaxConcurrentPrompts(8))
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub, reply := openStream(t, nc)
			defer func() { _ = sub.Unsubscribe() }()
			p := fmt.Sprintf("n%d", i)
			if err := nc.PublishMsg(&nats.Msg{Subject: h.Subject().Prompt(), Reply: reply, Data: []byte(p)}); err != nil {
				t.Error(err)
				return
			}
			var texts []string
			for {
				m, err := sub.NextMsg(10 * time.Second)
				if err != nil {
					t.Errorf("%s: %v", p, err)
					return
				}
				f := frame{Data: m.Data, Header: m.Header}
				if f.terminator() {
					break
				}
				if c, err := agents.DecodeChunk(f.Data); err == nil {
					if r, ok := c.(agents.ResponseChunk); ok {
						texts = append(texts, r.Text)
					}
				}
			}
			want := []string{p + "-0", p + "-1", p + "-2"}
			if fmt.Sprint(texts) != fmt.Sprint(want) {
				t.Errorf("%s got %v", p, texts)
			}
		}()
	}
	wg.Wait()
	if s := awaitIdle(t, h); s.Prompts != 40 {
		t.Errorf("stats %+v", s)
	}
}

// TestConcurrency_ConcurrentSendsAreSerialised: sends from several goroutines
// never interleave inside a chunk and never follow the terminator.
func TestConcurrency_ConcurrentSendsAreSerialised(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range 10 {
					_ = w.Send(ctx, fmt.Sprintf("%d.%d", i, j))
				}
			}()
		}
		wg.Wait()
		return nil
	}, host.WithKeepalive(time.Millisecond))
	s := prompt(t, nc, h, "x")
	if got := len(s.texts(t)); got != 80 {
		t.Errorf("chunks %d", got)
	}
	if !s.last().terminator() {
		t.Error("terminator is not last")
	}
	for i, f := range s.Frames[:len(s.Frames)-1] {
		if _, err := agents.DecodeChunk(f.Data); err != nil {
			t.Fatalf("frame %d is garbled: %v", i, err)
		}
	}
}
