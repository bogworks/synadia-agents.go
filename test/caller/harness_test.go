// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package caller_test runs the caller package against an embedded nats-server
// and a minimal hand-written fake SAP host built from raw NATS subscriptions.
// It deliberately does not use the host package: what the caller sees is what a
// peer wrote on the wire, including peers that misbehave.
package caller_test

import (
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

const identityTestdata = "../../identity/testdata"

type user struct{ pub, seed string }

// alice is a throwaway key pair of the identity fixtures (nkey-noaccounts.conf).
var alice = user{"UCDUW5V44EBDBIK2FL4CTNDBQFNGBEZVJHSZGQVKRHASN4AV4IWPB5NT", "SUAEJ6GDK6FSSB54LD45Q7W25AW7NUT7MVLBABIR5MIPFUTBW7ZNPK2KYE"}

func runServer(t *testing.T, opts *server.Options) *server.Server {
	t.Helper()
	opts.Host, opts.Port = "127.0.0.1", -1
	opts.NoLog, opts.NoSigs = true, true
	srv, err := server.NewServer(opts)
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

// startServer runs a server with no authentication.
func startServer(t *testing.T) *server.Server {
	t.Helper()
	return runServer(t, &server.Options{})
}

// startNkeyServer runs the nkey fixture server: one user, alice, in $G.
func startNkeyServer(t *testing.T) *server.Server {
	t.Helper()
	opts, err := server.ProcessConfigFile(filepath.Join(identityTestdata, "nkey-noaccounts.conf"))
	if err != nil {
		t.Fatal(err)
	}
	return runServer(t, opts)
}

func connect(t *testing.T, srv *server.Server, extra ...nats.Option) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(srv.ClientURL(), append([]nats.Option{nats.MaxReconnects(0)}, extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func connectAlice(t *testing.T, srv *server.Server) *nats.Conn {
	t.Helper()
	kp, err := nkeys.FromSeed([]byte(alice.seed))
	if err != nil {
		t.Fatal(err)
	}
	return connect(t, srv, nats.Nkey(alice.pub, kp.Sign))
}

func aliceSigner(t *testing.T) identity.Signer {
	t.Helper()
	s, err := identity.SignerFromSeed([]byte(alice.seed))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// host is a hand-written SAP host: it answers discovery, serves a prompt and a
// status endpoint through a script, and publishes heartbeats on demand. It
// shares nothing with the host package.
type host struct {
	t  *testing.T
	nc *nats.Conn

	id, agent, owner, name string
	promptSubject          string
	statusSubject          string
	maxPayload             string
	attachmentsOK          bool
	minTrust               string // empty: the key is not declared
	protocol               string
	extraMeta              map[string]string
	extraEndpoint          bool
	noStatusEndpoint       bool

	// script handles one prompt, on its own goroutine. Nil means the default:
	// ack, one response chunk echoing the prompt, terminator.
	script func(h *host, m *nats.Msg)
	// statusScript handles one status request; nil means a heartbeat reply.
	statusScript func(h *host, m *nats.Msg)

	prompts  chan *nats.Msg
	statuses chan *nats.Msg
	barriers chan struct{}
	infoReqs atomic.Int64
	wg       sync.WaitGroup
	subs     []*nats.Subscription
}

var hostSeq atomic.Int64

func newHost(t *testing.T, nc *nats.Conn, mods ...func(*host)) *host {
	t.Helper()
	n := hostSeq.Add(1)
	h := &host{
		t: t, nc: nc,
		id: fmt.Sprintf("HOST%04d%s", n, strings.ToUpper(nuid())), agent: "fake-agent", owner: "tester", name: fmt.Sprintf("n%d", n),
		maxPayload: "1MB", attachmentsOK: true, protocol: agents.ProtocolVersion,
		prompts: make(chan *nats.Msg, 1024), statuses: make(chan *nats.Msg, 64), barriers: make(chan struct{}, 2),
	}
	for _, m := range mods {
		m(h)
	}
	if h.promptSubject == "" {
		h.promptSubject = fmt.Sprintf("agents.prompt.%s.%s.%s", h.agent, h.owner, h.name)
	}
	if h.statusSubject == "" {
		h.statusSubject = fmt.Sprintf("agents.status.%s.%s.%s", h.agent, h.owner, h.name)
	}
	return h
}

func nuid() string { return nats.NewInbox()[len("_INBOX."):] }

func (h *host) info() agents.ServiceInfo {
	meta := agents.ServiceMetadata{Agent: h.agent, Owner: h.owner, ProtocolVersion: h.protocol}.Map()
	for k, v := range h.extraMeta {
		meta[k] = v
	}
	pm := agents.PromptEndpointMetadata{MaxPayload: 1, AttachmentsOK: h.attachmentsOK}.Map()
	pm[agents.MetaMaxPayload] = h.maxPayload
	if h.minTrust != "" {
		pm[agents.MetaMinSenderTrust] = h.minTrust
	}
	eps := []agents.EndpointInfo{{Name: "prompt", Subject: h.promptSubject, QueueGroup: "agents", Metadata: pm}}
	if !h.noStatusEndpoint {
		eps = append(eps, agents.EndpointInfo{Name: "status", Subject: h.statusSubject, QueueGroup: "agents"})
	}
	if h.extraEndpoint {
		eps = append(eps, agents.EndpointInfo{Name: "x-extra", Subject: "x.extra." + h.name, Metadata: map[string]string{"k": "v"}})
	}
	return agents.ServiceInfo{Name: "agents", ID: h.id, Version: "1.2.3", Description: "fake", Metadata: meta, Endpoints: eps}
}

func (h *host) heartbeat() agents.Heartbeat {
	hb := agents.Heartbeat{Agent: h.agent, Owner: h.owner, InstanceID: h.id, IntervalS: 30, ProtocolVersion: h.protocol}
	hb.Stamp(time.Now())
	return hb
}

// start subscribes and flushes; the host is stopped at cleanup.
func (h *host) start() *host {
	h.t.Helper()
	sub := func(subject string, cb nats.MsgHandler) {
		s, err := h.nc.Subscribe(subject, cb)
		if err != nil {
			h.t.Fatal(err)
		}
		h.subs = append(h.subs, s)
	}
	reply := func(m *nats.Msg, v any) {
		b, _ := json.Marshal(v)
		_ = m.Respond(b)
	}
	sub("$SRV.INFO.agents", func(m *nats.Msg) { h.infoReqs.Add(1); reply(m, h.info()) })
	sub("$SRV.INFO.agents."+h.id, func(m *nats.Msg) { h.infoReqs.Add(1); reply(m, h.info()) })
	ping := func(m *nats.Msg) {
		reply(m, agents.ServicePing{Name: "agents", ID: h.id, Version: "1.2.3"})
	}
	sub("$SRV.PING.agents", ping)
	sub("$SRV.PING.agents."+h.id, ping)
	sub(h.promptSubject, func(m *nats.Msg) {
		if m.Header.Get(barrierHeader) != "" {
			h.barriers <- struct{}{}
			return
		}
		h.prompts <- m
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			if h.script != nil {
				h.script(h, m)
				return
			}
			h.ack(m)
			h.text(m, "echo: "+h.promptText(m))
			h.end(m)
		}()
	})
	sub(h.statusSubject, func(m *nats.Msg) {
		if m.Header.Get(barrierHeader) != "" {
			h.barriers <- struct{}{}
			return
		}
		h.statuses <- m
		if h.statusScript != nil {
			h.statusScript(h, m)
			return
		}
		b, _ := agents.EncodeHeartbeat(h.heartbeat())
		_ = m.Respond(b)
	})
	if err := h.nc.Flush(); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() {
		for _, s := range h.subs {
			_ = s.Unsubscribe()
		}
		h.wg.Wait()
	})
	return h
}

func (h *host) promptText(m *nats.Msg) string {
	env, err := agents.DecodeEnvelope(m.Data)
	if err != nil {
		return "<undecodable>"
	}
	return env.Prompt
}

// publishHeartbeat publishes one heartbeat on the host's heartbeat subject.
func (h *host) publishHeartbeat(extra func(*nats.Msg)) {
	h.t.Helper()
	b, err := agents.EncodeHeartbeat(h.heartbeat())
	if err != nil {
		h.t.Fatal(err)
	}
	msg := &nats.Msg{Subject: fmt.Sprintf("agents.hb.%s.%s.%s", h.agent, h.owner, h.name), Data: b}
	if extra != nil {
		extra(msg)
	}
	if err := h.nc.PublishMsg(msg); err != nil {
		h.t.Fatal(err)
	}
}

func (h *host) raw(m *nats.Msg, data []byte) { _ = h.nc.Publish(m.Reply, data) }

func (h *host) chunk(m *nats.Msg, typ string, data any) {
	b, _ := json.Marshal(map[string]any{"type": typ, "data": data})
	h.raw(m, b)
}

func (h *host) ack(m *nats.Msg)            { h.raw(m, agents.AckChunk()) }
func (h *host) text(m *nats.Msg, s string) { h.chunk(m, "response", s) }
func (h *host) end(m *nats.Msg)            { _ = h.nc.PublishMsg(&nats.Msg{Subject: m.Reply}) }

// fail publishes an error-headered message (SAP §9.1).
func (h *host) fail(m *nats.Msg, code, desc string, body []byte) {
	_ = h.nc.PublishMsg(&nats.Msg{
		Subject: m.Reply, Data: body,
		Header: nats.Header{agents.HeaderServiceErrorCode: {code}, agents.HeaderServiceError: {desc}},
	})
}

// nextPrompt waits for one prompt request to arrive.
func (h *host) nextPrompt(t *testing.T) *nats.Msg {
	t.Helper()
	select {
	case m := <-h.prompts:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("the host received no prompt")
		return nil
	}
}

// barrierHeader marks the host's own synchronization messages, which its
// handlers swallow.
const barrierHeader = "X-Test-Barrier"

// barrier returns once every message that pub published before the call has
// been dispatched to the host's prompt and status handlers, so a following
// assertion that nothing arrived is a fact and not a guess. pub's flush proves
// the server has routed everything pub sent; the sentinels the host then
// publishes to its own subjects queue behind it, and a subscription dispatches
// in order.
func (h *host) barrier(t *testing.T, pub *nats.Conn) {
	t.Helper()
	if err := pub.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, subject := range []string{h.promptSubject, h.statusSubject} {
		msg := &nats.Msg{Subject: subject, Header: nats.Header{barrierHeader: {"1"}}}
		if err := h.nc.PublishMsg(msg); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case <-h.barriers:
		case <-time.After(5 * time.Second):
			t.Fatal("the host's barrier did not come back")
		}
	}
}

// promptsSeen is the number of prompts received so far.
func (h *host) promptsSeen() int { return len(h.prompts) }

// sniffer records every subject published on the connection's account after it
// was created, apart from server-internal traffic on $SYS and inboxes it owns.
type sniffer struct {
	mu       sync.Mutex
	subjects []string
	msgs     []*nats.Msg
}

func newSniffer(t *testing.T, nc *nats.Conn) *sniffer {
	t.Helper()
	s := &sniffer{}
	sub, err := nc.Subscribe(">", func(m *nats.Msg) {
		s.mu.Lock()
		s.subjects = append(s.subjects, m.Subject)
		s.msgs = append(s.msgs, m)
		s.mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *sniffer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.subjects...)
}

func (s *sniffer) on(subject string) []*nats.Msg {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*nats.Msg
	for _, m := range s.msgs {
		if m.Subject == subject {
			out = append(out, m)
		}
	}
	return out
}

// eventually polls cond for up to five seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// pull adapts a stream's sequence to next/stop calls; next returns io.EOF when
// the sequence has ended.
func pull(st *caller.Stream) (func() (caller.Event, error), func()) {
	next, stop := iter.Pull2(st.Events())
	return func() (caller.Event, error) {
		ev, err, ok := next()
		if !ok {
			return nil, io.EOF
		}
		return ev, err
	}, stop
}
