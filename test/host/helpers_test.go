// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package host_test drives package host over a real NATS connection with raw
// requests and subscriptions, the way any SAP caller would. It runs against an
// embedded nats-server, and does not use package caller.
package host_test

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

const (
	testAgent = "test-agent"
	testOwner = "tester"
	testName  = "t1"
)

func startServer(t *testing.T) *server.Server {
	t.Helper()
	srv, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats-server did not become ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

func connect(t *testing.T, srv *server.Server, opts ...nats.Option) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(srv.ClientURL(), append([]nats.Option{nats.MaxReconnects(0)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func setup(t *testing.T) (*server.Server, *nats.Conn) {
	t.Helper()
	srv := startServer(t)
	return srv, connect(t, srv)
}

func subjectOf(t *testing.T, name string) agents.AgentSubject {
	t.Helper()
	s, err := agents.NewAgentSubject(testAgent, testOwner, name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func echo(ctx context.Context, req *host.Request, w *host.Responder) error {
	return w.Send(ctx, "echo: "+req.Envelope.Prompt)
}

// newHost builds a host with a short, opt-in heartbeat unless the options say
// otherwise, starts it and stops it when the test ends.
func newHost(t *testing.T, nc *nats.Conn, handler host.PromptHandler, opts ...host.Option) *host.Host {
	t.Helper()
	return newHostNamed(t, nc, testName, handler, opts...)
}

func newHostNamed(t *testing.T, nc *nats.Conn, name string, handler host.PromptHandler, opts ...host.Option) *host.Host {
	t.Helper()
	base := []host.Option{host.WithoutKeepalive(), host.WithHeartbeatInterval(time.Hour)}
	h, err := host.New(nc, subjectOf(t, name), handler, append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// frame is one message received on a reply subject.
type frame struct {
	Data   []byte
	Header nats.Header
}

func (f frame) terminator() bool { return agents.IsTerminator(f.Data, f.Header) }

func (f frame) chunk(t *testing.T) agents.Chunk {
	t.Helper()
	c, err := agents.DecodeChunk(f.Data)
	if err != nil {
		t.Fatalf("not a chunk: %q: %v", f.Data, err)
	}
	return c
}

func (f frame) isAck() bool {
	if agents.IsErrorSignal(f.Header) || len(f.Data) == 0 {
		return false
	}
	c, err := agents.DecodeChunk(f.Data)
	if err != nil {
		return false
	}
	s, ok := c.(agents.StatusChunk)
	return ok && s.Status == agents.StatusAck
}

func (f frame) serviceError() *agents.ServiceError {
	se, ok := agents.ParseServiceError(f.Header, f.Data)
	if !ok {
		return nil
	}
	return se
}

// stream is what a caller sees of one prompt.
type stream struct {
	Frames []frame
}

func (s stream) last() frame { return s.Frames[len(s.Frames)-1] }

// errorFrame returns the error-headered message, if any.
func (s stream) errorFrame() *agents.ServiceError {
	for _, f := range s.Frames {
		if se := f.serviceError(); se != nil {
			return se
		}
	}
	return nil
}

// texts returns the text of every response chunk.
func (s stream) texts(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, f := range s.Frames {
		if f.terminator() || f.serviceError() != nil {
			continue
		}
		if r, ok := f.chunk(t).(agents.ResponseChunk); ok {
			out = append(out, r.Text)
		}
	}
	return out
}

func (s stream) acks() int {
	n := 0
	for _, f := range s.Frames {
		if f.isAck() {
			n++
		}
	}
	return n
}

// rawPrompt publishes payload to subject with a reply inbox and collects every
// message up to and including the terminator. It fails the test when no
// terminator arrives within wait.
func rawPrompt(t *testing.T, nc *nats.Conn, subject string, payload []byte, hdr nats.Header, wait time.Duration) stream {
	t.Helper()
	sub, reply := openStream(t, nc)
	defer func() { _ = sub.Unsubscribe() }()
	if err := nc.PublishMsg(&nats.Msg{Subject: subject, Reply: reply, Data: payload, Header: hdr}); err != nil {
		t.Fatal(err)
	}
	return collect(t, sub, wait)
}

func openStream(t *testing.T, nc *nats.Conn) (*nats.Subscription, string) {
	t.Helper()
	reply := nats.NewInbox()
	sub, err := nc.SubscribeSync(reply)
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	return sub, reply
}

func collect(t *testing.T, sub *nats.Subscription, wait time.Duration) stream {
	t.Helper()
	var s stream
	deadline := time.Now().Add(wait)
	for {
		m, err := sub.NextMsg(time.Until(deadline))
		if err != nil {
			t.Fatalf("no terminator within %v (got %d frames): %v", wait, len(s.Frames), err)
		}
		f := frame{Data: m.Data, Header: m.Header}
		s.Frames = append(s.Frames, f)
		if f.terminator() {
			return s
		}
	}
}

func prompt(t *testing.T, nc *nats.Conn, h *host.Host, text string) stream {
	t.Helper()
	return rawPrompt(t, nc, h.Subject().Prompt(), []byte(text), nil, 5*time.Second)
}

func jsonPrompt(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// status asks the status endpoint and decodes the reply.
func status(t *testing.T, nc *nats.Conn, h *host.Host, body []byte, hdr nats.Header) (*nats.Msg, agents.Heartbeat) {
	t.Helper()
	m, err := nc.RequestMsg(&nats.Msg{Subject: h.Subject().Status(), Data: body, Header: hdr}, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if se, ok := agents.ParseServiceError(m.Header, m.Data); ok {
		t.Fatalf("status answered an error: %v", se)
	}
	hb, err := agents.DecodeHeartbeat(m.Data)
	if err != nil {
		t.Fatalf("status reply is not a heartbeat: %v", err)
	}
	return m, hb
}

// info returns the $SRV.INFO record of one instance.
func info(t *testing.T, nc *nats.Conn, id string) agents.ServiceInfo {
	t.Helper()
	subj, err := agents.InfoSubject(id)
	if err != nil {
		t.Fatal(err)
	}
	m, err := nc.Request(subj, nil, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	si, err := agents.DecodeServiceInfo(m.Data)
	if err != nil {
		t.Fatal(err)
	}
	return si
}

func endpointOf(t *testing.T, si agents.ServiceInfo, name string) agents.EndpointInfo {
	t.Helper()
	for _, e := range si.Endpoints {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no endpoint %q in %+v", name, si.Endpoints)
	return agents.EndpointInfo{}
}

// settle waits until cond holds, or fails the test.
func settle(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ownGoroutines returns the stacks of the goroutines that run code of package
// host or of nats.go/micro. The embedded nats-server starts and ends goroutines
// of its own, so a plain runtime.NumGoroutine comparison is not stable; the
// count is taken over the goroutines this SDK could leak.
func ownGoroutines() []string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var out []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "synadia-agents.go/host.") || strings.Contains(g, "nats.go/micro.") {
			out = append(out, g)
		}
	}
	return out
}

// goroutineBaseline returns a check that fails the test when goroutines of the
// SDK are still alive after the host stopped (with retries, no new dependency).
func goroutineBaseline(t *testing.T) func() {
	t.Helper()
	before := len(ownGoroutines())
	return func() {
		t.Helper()
		var left []string
		for i := 0; i < 100; i++ {
			if left = ownGoroutines(); len(left) <= before {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatalf("goroutine leak: %d SDK goroutines (%d before)\n%s", len(left), before, strings.Join(left, "\n\n"))
	}
}

// closeOnce returns a function that closes ch the first time it is called. It
// is also registered as a cleanup, so a handler blocked on ch is freed when an
// assertion fails before the test body closes it. Register it after the host
// is created: t.Cleanup is LIFO, so it then runs before the host's own Close,
// which waits for in-flight handlers.
func closeOnce(t *testing.T, ch chan struct{}) func() {
	t.Helper()
	var once sync.Once
	f := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(f)
	return f
}
