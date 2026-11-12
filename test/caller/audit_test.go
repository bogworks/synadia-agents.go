// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/nats-io/nats.go"
)

// TestPrompt_PartialBufferOverridesKeepTheOtherBound asserts that
// WithPromptBuffer treats zero as "keep the client's bound" for each field on
// its own: a message-only override keeps the client's byte allowance, and a
// byte-only override keeps the client's message bound.
func TestPrompt_PartialBufferOverridesKeepTheOtherBound(t *testing.T) {
	t.Run("messages only keeps the byte bound", func(t *testing.T) {
		f := setup(t, mod(func(h *host) {
			h.script = func(h *host, m *nats.Msg) { h.text(m, "hello") } // no terminator
		}))
		st, err := f.a.PromptText(ctxT(t), "go", caller.WithPromptBuffer(1, 0))
		if err != nil {
			t.Fatal(err)
		}
		for ev, err := range st.Events() {
			if err != nil {
				t.Fatalf("a one-message override zeroed the byte bound: %v", err)
			}
			if r, ok := ev.(caller.Response); !ok || r.Text != "hello" {
				t.Fatalf("event %v", ev)
			}
			break
		}
	})
	t.Run("bytes only keeps the message bound", func(t *testing.T) {
		f := setup(t, mod(func(h *host) {
			h.script = func(h *host, m *nats.Msg) {
				h.ack(m)
				if h.promptText(m) == "flood" {
					for i := 0; i < 100; i++ {
						h.text(m, "x")
					}
				} else {
					h.text(m, "a")
					h.text(m, "b")
				}
				h.end(m)
			}
		}), caller.WithStreamBuffer(5, 0))
		st, err := f.a.PromptText(ctxT(t), "few", caller.WithPromptBuffer(0, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		evs, err := collect(t, st)
		if err != nil || texts(evs) != "ab" {
			t.Fatalf("a byte-only override zeroed the message bound: %q %v", texts(evs), err)
		}
		st, err = f.a.PromptText(ctxT(t), "flood", caller.WithPromptBuffer(0, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "the overflow", func() bool { return f.c.Stats().SlowConsumers == 1 })
		if _, err := collect(t, st); !errors.Is(err, caller.ErrSlowConsumer) {
			t.Fatalf("the client's message bound of 5 was not kept: %v", err)
		}
	})
	t.Run("negative is refused", func(t *testing.T) {
		f := setup(t, nil)
		if _, err := f.a.PromptText(ctxT(t), "go", caller.WithPromptBuffer(-1, 0)); err == nil {
			t.Fatal("accepted")
		}
	})
}

// TestStream_TimersStartAtPublication asserts that the inactivity and
// max-wait deadlines run from the publish, not from the first iteration: a
// stream nobody consumes releases its route when a deadline passes (SAP §6.6),
// and a consumer that comes late gets the timeout at once, not a fresh one.
//
// catalog: SAP-6.6-03
func TestStream_TimersStartAtPublication(t *testing.T) {
	hang := mod(func(h *host) { h.script = func(h *host, m *nats.Msg) { h.ack(m) } })
	t.Run("inactivity", func(t *testing.T) {
		f := setup(t, hang)
		st, err := f.a.PromptText(ctxT(t), "go", caller.WithPromptInactivityTimeout(300*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if n := f.c.Stats().ActiveRoutes; n != 1 {
			t.Fatalf("ActiveRoutes %d, want 1 while the stream is live", n)
		}
		eventually(t, "the unconsumed stream's route to be released", func() bool { return f.c.Stats().ActiveRoutes == 0 })
		start := time.Now()
		_, err = collect(t, st)
		var stalled *agents.StreamStalledError
		if !errors.As(err, &stalled) || stalled.Timeout != 300*time.Millisecond {
			t.Fatalf("a late consumer got %v, want the stall error", err)
		}
		if el := time.Since(start); el > 150*time.Millisecond {
			t.Fatalf("a late consumer waited %s: it was given a fresh timeout", el)
		}
		if st.Err() == nil {
			t.Fatal("Err is nil after the timeout")
		}
	})
	t.Run("max wait", func(t *testing.T) {
		f := setup(t, hang)
		st, err := f.a.PromptText(ctxT(t), "go", caller.WithPromptMaxWait(300*time.Millisecond), caller.WithPromptInactivityTimeout(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "the unconsumed stream's route to be released", func() bool { return f.c.Stats().ActiveRoutes == 0 })
		start := time.Now()
		_, err = collect(t, st)
		if !errors.Is(err, caller.ErrMaxWait) {
			t.Fatalf("a late consumer got %v, want the max-wait error", err)
		}
		if el := time.Since(start); el > 150*time.Millisecond {
			t.Fatalf("a late consumer waited %s: it was given a fresh timeout", el)
		}
	})
	t.Run("a finished stream read late still ends cleanly", func(t *testing.T) {
		f := setup(t, nil) // ack, echo, terminator
		st, err := f.a.PromptText(ctxT(t), "late", caller.WithPromptInactivityTimeout(200*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "the route to be released", func() bool { return f.c.Stats().ActiveRoutes == 0 })
		evs, err := collect(t, st)
		if err != nil || texts(evs) != "echo: late" || !st.Terminated() {
			t.Fatalf("what the agent sent before the deadline was lost: %q %v", texts(evs), err)
		}
	})
	t.Run("a live stream is kept alive by its arrivals, consumed or not", func(t *testing.T) {
		f := setup(t, mod(func(h *host) {
			h.script = func(h *host, m *nats.Msg) {
				h.ack(m)
				for i := 0; i < 6; i++ {
					time.Sleep(100 * time.Millisecond)
					h.text(m, "x")
				}
				h.end(m)
			}
		}))
		st, err := f.a.PromptText(ctxT(t), "go", caller.WithPromptInactivityTimeout(300*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(900 * time.Millisecond) // longer than the timeout, shorter than the gaps summed
		evs, err := collect(t, st)
		if err != nil || texts(evs) != "xxxxxx" {
			t.Fatalf("%q %v", texts(evs), err)
		}
	})
}

// TestStream_NothingYieldedAfterClose asserts that Close discards what is
// buffered: after it returns, the sequence yields no further item, even though
// replies are queued.
func TestStream_NothingYieldedAfterClose(t *testing.T) {
	const rounds, per = 25, 20
	sent := make(chan struct{}, rounds)
	f := setup(t, mod(func(h *host) {
		h.script = func(h *host, m *nats.Msg) {
			h.ack(m)
			for i := 0; i < per; i++ {
				h.text(m, "x")
			}
			_ = h.nc.Flush()
			sent <- struct{}{}
		}
	}))
	// Close and a queued reply are both ready when the loop comes round, and a
	// select picks at random between them, so one round proves little; many do.
	for round := 1; round <= rounds; round++ {
		st, err := f.a.PromptText(ctxT(t), "go")
		if err != nil {
			t.Fatal(err)
		}
		<-sent
		want := uint64(round * (per + 1))
		eventually(t, "the replies to arrive", func() bool { return f.cnc.Stats().InMsgs >= want })
		after, closed := 0, false
		for _, err := range st.Events() {
			if err != nil {
				t.Fatal(err)
			}
			if closed {
				after++
				continue
			}
			_ = st.Close()
			closed = true
		}
		if after != 0 {
			t.Fatalf("round %d: %d items were yielded after Close returned", round, after)
		}
		if st.Err() != nil {
			t.Fatalf("Close ended the stream with %v", st.Err())
		}
	}
}

// TestStream_HeaderBytesCountAgainstTheBuffer asserts that a reply's header
// bytes are part of its route's byte budget: many small bodies with large
// headers end the stream with a slow-consumer error as plain large bodies do.
func TestStream_HeaderBytesCountAgainstTheBuffer(t *testing.T) {
	sent := make(chan struct{})
	f := setup(t, mod(func(h *host) {
		h.script = func(h *host, m *nats.Msg) {
			for i := 0; i < 8; i++ {
				b, _ := json.Marshal(map[string]any{"type": "response", "data": "x"})
				_ = h.nc.PublishMsg(&nats.Msg{Subject: m.Reply, Data: b, Header: nats.Header{"X-Pad": {strings.Repeat("h", 1024)}}})
			}
			_ = h.nc.Flush()
			close(sent)
		}
	}), caller.WithStreamBuffer(0, 4<<10))
	st, err := f.a.PromptText(ctxT(t), "go")
	if err != nil {
		t.Fatal(err)
	}
	<-sent
	eventually(t, "the overflow", func() bool { return f.c.Stats().SlowConsumers == 1 })
	if _, err := collect(t, st); !errors.Is(err, caller.ErrSlowConsumer) {
		t.Fatalf("8 KiB of headers in 4 KiB of budget: %v", err)
	}
}

// TestPrompt_SizesTheCompletedMessage asserts that the local size check covers
// every header on the final message, those of interceptors and of the header
// hook included, and that the error reports the full attempted size.
//
// catalog: SAP-5.4-03
func TestPrompt_SizesTheCompletedMessage(t *testing.T) {
	big := func(n int) map[string]string { return map[string]string{"X-Big": strings.Repeat("a", n)} }
	env := agents.Envelope{Prompt: strings.Repeat("x", 800)}
	payload, err := env.EncodedSize()
	if err != nil {
		t.Fatal(err)
	}
	hook := func(n int) caller.Option {
		return caller.WithRequestHeaders(func(context.Context, *caller.PromptInfo) (map[string]string, error) { return big(n), nil })
	}
	t.Run("header hook against the endpoint's max_payload", func(t *testing.T) {
		f := setup(t, mod(func(h *host) { h.maxPayload = "2KB" }), hook(1500))
		err := refused(t, f, env, agents.ErrPayloadTooLarge)
		var pl *agents.PayloadTooLargeError
		if !errors.As(err, &pl) || pl.Connection || pl.Limit != 2048 || pl.Actual != int64(payload) || pl.HeaderBytes < 1500 {
			t.Fatalf("%+v", pl)
		}
	})
	t.Run("interceptor headers count too", func(t *testing.T) {
		f := setup(t, mod(func(h *host) { h.maxPayload = "2KB" }), caller.WithInterceptors(caller.InterceptorFunc(
			func(context.Context, *caller.PromptInfo) (*caller.Extras, error) {
				return &caller.Extras{Headers: big(1500)}, nil
			})))
		_ = refused(t, f, env, agents.ErrPayloadTooLarge)
	})
	t.Run("header hook against the connection's limit reports the whole message", func(t *testing.T) {
		f := setup(t, mod(func(h *host) { h.maxPayload = "8MB" }), hook(1<<20))
		err := refused(t, f, agents.Envelope{Prompt: "small"}, agents.ErrPayloadTooLarge)
		var pl *agents.PayloadTooLargeError
		if !errors.As(err, &pl) || !pl.Connection || pl.HeaderBytes < 1<<20 || pl.Limit != f.cnc.MaxPayload() {
			t.Fatalf("%+v", pl)
		}
	})
	t.Run("a message that fits is sent", func(t *testing.T) {
		f := setup(t, mod(func(h *host) { h.maxPayload = "2KB" }), hook(100))
		st, err := f.a.Prompt(ctxT(t), env)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := collect(t, st); err != nil {
			t.Fatal(err)
		}
	})
}

// TestDiscover_BoundsAcceptedRecords asserts that one discovery keeps at most
// a bounded number of records, so a responder that floods distinct instances
// cannot grow its result without limit.
func TestDiscover_BoundsAcceptedRecords(t *testing.T) {
	if caller.DefaultDiscoverMaxRecords != 10000 {
		t.Fatalf("default %d", caller.DefaultDiscoverMaxRecords)
	}
	srv := startServer(t)
	hnc, cnc := connect(t, srv), connect(t, srv)
	const flood = 60
	hosts := make([]*host, flood)
	for i := range hosts {
		hosts[i] = newHost(t, hnc)
	}
	if _, err := hnc.Subscribe("$SRV.INFO.agents", func(m *nats.Msg) {
		for _, h := range hosts {
			b, _ := json.Marshal(h.info())
			_ = m.Respond(b)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := hnc.Subscribe("$SRV.PING.agents", func(m *nats.Msg) {
		for _, h := range hosts {
			b, _ := json.Marshal(agents.ServicePing{Name: "agents", ID: h.id, Version: "1"})
			_ = m.Respond(b)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := hnc.Flush(); err != nil {
		t.Fatal(err)
	}
	c := newClient(t, cnc)
	win := caller.WithDiscoverTimeout(time.Second)
	got, err := c.Discover(ctxT(t), win, caller.WithDiscoverMaxRecords(7))
	if err != nil || len(got) != 7 {
		t.Fatalf("Discover kept %d records (%v), want 7", len(got), err)
	}
	pings, err := c.PingAll(ctxT(t), win, caller.WithDiscoverMaxRecords(7))
	if err != nil || len(pings) != 7 {
		t.Fatalf("PingAll kept %d answers (%v), want 7", len(pings), err)
	}
	all, err := c.Discover(ctxT(t), win)
	if err != nil || len(all) != flood {
		t.Fatalf("under the default bound %d of %d were kept (%v)", len(all), flood, err)
	}
}

// TestStatus_TimeoutOption asserts WithStatusTimeout's contract: zero keeps
// the default, a negative value is an error at call time that sends nothing.
func TestStatus_TimeoutOption(t *testing.T) {
	f := setup(t, nil)
	if _, err := f.a.Status(ctxT(t), caller.WithStatusTimeout(0)); err != nil {
		t.Fatalf("a zero timeout must keep the default: %v", err)
	}
	for _, d := range []time.Duration{-1, -time.Second} {
		_, err := f.a.Status(ctxT(t), caller.WithStatusTimeout(d))
		if err == nil || errors.Is(err, nats.ErrTimeout) {
			t.Fatalf("timeout %s: %v, want a validation error", d, err)
		}
	}
	f.h.barrier(t, f.cnc)
	if n := len(f.h.statuses); n != 1 {
		t.Fatalf("%d status requests reached the host, want only the one with the default timeout", n)
	}
	if _, err := f.a.Status(ctxT(t), caller.WithStatusTimeout(time.Second)); err != nil {
		t.Fatal(err)
	}
}
