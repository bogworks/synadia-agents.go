// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/nats-io/nats.go"
)

// offlineClient is a Client on a connection object that never connects: enough
// for the parts of the package that do no network I/O.
func offlineClient(t *testing.T, opts ...Option) *Client {
	t.Helper()
	c, err := New(new(nats.Conn), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestClient_DefaultInactivityTimeout asserts the default inactivity timeout of
// a stream is 60 seconds, the value SAP recommends, counted from the latest
// chunk.
//
// catalog: SAP-6.6-04
func TestClient_DefaultInactivityTimeout(t *testing.T) {
	c := offlineClient(t)
	if c.cfg.inactivity != 60*time.Second || agents.DefaultInactivityTimeout != 60*time.Second {
		t.Fatalf("default inactivity timeout %s", c.cfg.inactivity)
	}
	if c.cfg.maxWait != 10*time.Minute {
		t.Fatalf("default max wait %s", c.cfg.maxWait)
	}
}

func TestNew_ValidatesOptions(t *testing.T) {
	for name, opt := range map[string]Option{
		"zero inactivity":   WithInactivityTimeout(0),
		"negative max wait": WithMaxWait(-1),
		"bad inbox prefix":  WithInboxPrefix("a..b"),
		"wildcard inbox":    WithInboxPrefix("_INBOX.*"),
		"nil interceptor":   WithInterceptors(nil),
		"negative buffer":   WithStreamBuffer(-1, 0),
		"bad tracker slack": WithTracker(WithTrackerSlack(-2)),
		"bad display name":  WithIdentity(IdentityOptions{Name: "bad\x00name"}),
	} {
		if _, err := New(new(nats.Conn), opt); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	if _, err := New(nil); err == nil {
		t.Fatal("a nil connection was accepted")
	}
}

func TestReservedHeader(t *testing.T) {
	for _, n := range []string{"Agent-Sender", "AGENT-SENDER", "nats-service-error", "Nats-Service-Error-Code", "Nats-Request-Info", "nats-msg-id"} {
		if !ReservedHeader(n) {
			t.Errorf("%s should be reserved", n)
		}
	}
	for _, n := range []string{"X-App", "Traceparent", "Agent-Senders"} {
		if ReservedHeader(n) {
			t.Errorf("%s should be free", n)
		}
	}
	for _, bad := range []struct{ k, v string }{{"", "x"}, {"a b", "x"}, {"a:b", "x"}, {"a", "x\ny"}, {"a", "x\x00"}} {
		if !errors.Is(validHeader(bad.k, bad.v), ErrInvalidHeader) {
			t.Errorf("%q: %q was accepted", bad.k, bad.v)
		}
	}
}

// streamOn returns a stream on a route of c that the test feeds directly
// through the mux callback, as the subscription would.
func streamOn(ctx context.Context, t *testing.T, c *Client, msgs int, bytes int64) *Stream {
	t.Helper()
	s := &Stream{c: c, ctx: ctx, subject: "x", inactivity: time.Second, maxWait: time.Minute, closed: make(chan struct{}), expired: make(chan struct{})}
	r, err := c.mux.register(msgs, bytes, s.arrived, s.stopTimers)
	if err != nil {
		t.Fatal(err)
	}
	s.r = r
	s.startTimers()
	t.Cleanup(s.stopTimers)
	return s
}

func deliver(c *Client, s *Stream, data string, hdr nats.Header) {
	c.mux.deliver(&nats.Msg{Subject: s.r.subject, Data: []byte(data), Header: hdr})
}

func drain(s *Stream) (texts []string, err error) {
	for ev, e := range s.Events() {
		if e != nil {
			err = e
			continue
		}
		if r, ok := ev.(Response); ok {
			texts = append(texts, r.Text)
		}
	}
	return
}

// TestMux_RoutesByTokenAndCountsStrays covers the reply multiplexer without a
// network: replies reach only their own route, in order, and a reply with no
// route is dropped and counted.
func TestMux_RoutesByTokenAndCountsStrays(t *testing.T) {
	c := offlineClient(t)
	a := streamOn(context.Background(), t, c, 8, 1<<20)
	b := streamOn(context.Background(), t, c, 8, 1<<20)
	deliver(c, a, `{"type":"response","data":"a1"}`, nil)
	deliver(c, b, `{"type":"response","data":"b1"}`, nil)
	deliver(c, a, `{"type":"response","data":"a2"}`, nil)
	deliver(c, a, "", nil)
	deliver(c, b, "", nil)
	if got, err := drain(a); err != nil || strings.Join(got, ",") != "a1,a2" || !a.Terminated() {
		t.Fatalf("a: %v %v", got, err)
	}
	if got, err := drain(b); err != nil || strings.Join(got, ",") != "b1" {
		t.Fatalf("b: %v %v", got, err)
	}
	if c.Stats().DroppedReplies != 0 || c.Stats().ActiveRoutes != 0 {
		t.Fatalf("%+v", c.Stats())
	}
	// Strays: a finished route, a token never issued, a subject with extra tokens.
	deliver(c, a, `{"type":"response","data":"late"}`, nil)
	c.mux.deliver(&nats.Msg{Subject: c.mux.prefix + ".neverissued", Data: []byte("x")})
	c.mux.deliver(&nats.Msg{Subject: c.mux.prefix + ".a.b", Data: []byte("x")})
	c.mux.deliver(&nats.Msg{Subject: "_INBOX.other.token", Data: []byte("x")})
	if got := c.Stats().DroppedReplies; got != 4 {
		t.Fatalf("dropped %d, want 4", got)
	}
}

// TestStream_SlowConsumerKeepsOrder asserts that an overflow ends the stream
// after the items buffered before it, never with a gap in the middle.
func TestStream_SlowConsumerKeepsOrder(t *testing.T) {
	c := offlineClient(t)
	s := streamOn(context.Background(), t, c, 3, 1<<20)
	for _, w := range []string{"1", "2", "3", "4", "5"} {
		deliver(c, s, `{"type":"response","data":"`+w+`"}`, nil)
	}
	got, err := drain(s)
	var sc *SlowConsumerError
	if !errors.As(err, &sc) || strings.Join(got, "") != "123" {
		t.Fatalf("%v %v", got, err)
	}
	if c.Stats().SlowConsumers != 1 {
		t.Fatalf("%+v", c.Stats())
	}
}

// TestStream_ClientCloseWinsOverBufferedItems asserts that a closed client ends
// its streams at once with ErrClosed, ahead of anything still buffered.
func TestStream_ClientCloseWinsOverBufferedItems(t *testing.T) {
	c := offlineClient(t)
	s := streamOn(context.Background(), t, c, 8, 1<<20)
	deliver(c, s, `{"type":"response","data":"buffered"}`, nil)
	_ = c.Close()
	got, err := drain(s)
	if !errors.Is(err, ErrClosed) || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := c.mux.register(1, 1, nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("register after close: %v", err)
	}
}

// TestStream_ErrorSignalNeedsTheHeader asserts that only the code header makes
// a message an error: a body that merely looks like one is not, and the
// terminator is only a zero-byte message without headers.
func TestStream_ErrorSignalNeedsTheHeader(t *testing.T) {
	c := offlineClient(t)
	s := streamOn(context.Background(), t, c, 8, 1<<20)
	deliver(c, s, `{"error":"x","message":"looks like an error"}`, nil) // not a chunk: dropped
	deliver(c, s, "", nats.Header{"X-Other": {"1"}})                    // headers: not the terminator
	deliver(c, s, `{"type":"response","data":"ok"}`, nil)
	deliver(c, s, "", nil)
	got, err := drain(s)
	if err != nil || strings.Join(got, "") != "ok" || !s.Terminated() {
		t.Fatalf("%v %v", got, err)
	}
	if c.Stats().MalformedChunks != 2 {
		t.Fatalf("%+v", c.Stats())
	}
}

func TestAttachmentFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "report.txt")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := AttachmentFromFile(p)
	if err != nil || a.Filename != "report.txt" {
		t.Fatalf("a base name only: %+v %v", a, err)
	}
	if b, err := a.Bytes(); err != nil || string(b) != "hello" {
		t.Fatal(b, err)
	}
	if _, err := AttachmentFromFile(dir); !errors.Is(err, agents.ErrLocal) {
		t.Fatalf("a directory: %v", err)
	}
	if _, err := AttachmentFromFile(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing file")
	}
}
