// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package interop_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bogworks/synadia-agents.go/test/interop"
)

// The self-test runs the official SDKs against each other through the harness,
// before any Go code is involved: it proves the plumbing (server, peers, JSON
// lines, signing) so a failure in a later Go x official run points at the Go side.

type pair struct{ caller, host interop.Impl }

var pairs = []pair{
	{interop.TypeScript, interop.Python},
	{interop.Python, interop.TypeScript},
}

func (p pair) name() string { return fmt.Sprintf("%sCaller_%sHost", p.caller, p.host) }

func eachPair(t *testing.T, fn func(t *testing.T, e *interop.Env, p pair)) {
	t.Helper()
	e := interop.Require(t)
	for _, p := range pairs {
		t.Run(p.name(), func(t *testing.T) {
			t.Parallel()
			fn(t, e, p)
		})
	}
}

func TestSelf_Discover_Status_Heartbeat(t *testing.T) {
	eachPair(t, func(t *testing.T, e *interop.Env, p pair) {
		t.Helper()
		srv := interop.StartServer(t, e)
		host := e.StartHost(t, p.host, srv.URL, interop.Peer{Mode: "echo", Heartbeat: time.Second})
		res := e.RunCaller(t, p.caller, srv.URL, interop.Peer{Agent: "interop", Status: true, WatchHeartbeat: 2500 * time.Millisecond})
		if res.Exit != 0 {
			t.Fatalf("caller exit %d; stderr:\n%s", res.Exit, res.Stderr())
		}
		d, _ := res.Find("discovered")
		agents := d.List("agents")
		if len(agents) != 1 || agents[0].Str("subject") != host.Subject || host.Subject != "agents.prompt.interop.interop.main" {
			t.Fatalf("discovered %v, host subject %q", agents, host.Subject)
		}
		if got := len(res.All("heartbeat")); got < 2 {
			t.Fatalf("saw %d heartbeats in 2.5s at a 1s interval, want at least 2", got)
		}
		if l, _ := res.Find("liveness"); !l.Bool("online") {
			t.Fatalf("liveness = %v", l)
		}
		if s, ok := res.Find("status_reply"); !ok || s.Str("agent") != "interop" {
			t.Fatalf("status_reply = %v", s)
		}
	})
}

func TestSelf_Echo(t *testing.T) {
	eachPair(t, func(t *testing.T, e *interop.Env, p pair) {
		t.Helper()
		srv := interop.StartServer(t, e)
		host := e.StartHost(t, p.host, srv.URL, interop.Peer{Mode: "echo", Chunks: 3})
		const prompt = "hello from the interop harness"
		res := e.RunCaller(t, p.caller, srv.URL, interop.Peer{Prompt: prompt})
		if res.Exit != 0 {
			t.Fatalf("caller exit %d; stderr:\n%s", res.Exit, res.Stderr())
		}
		if res.Text() != prompt {
			t.Fatalf("echoed %q, want %q", res.Text(), prompt)
		}
		if n := len(res.All("response")); n != 3 {
			t.Fatalf("%d response chunks, want 3", n)
		}
		if st := res.All("status"); len(st) == 0 || st[0].Str("status") != "ack" {
			t.Fatalf("first status = %v, want a leading ack", st)
		}
		if _, ok := res.Find("done"); !ok {
			t.Fatalf("no done line: %v", res.Lines)
		}
		if l := host.WaitForType(t, 5*time.Second, "prompt"); l.Str("prompt") != prompt || l.Str("sender") != "" {
			t.Fatalf("host saw %v", l)
		}
	})
}

func TestSelf_Error(t *testing.T) {
	eachPair(t, func(t *testing.T, e *interop.Env, p pair) {
		t.Helper()
		srv := interop.StartServer(t, e)
		e.StartHost(t, p.host, srv.URL, interop.Peer{Mode: "error", ErrorCode: 503, ErrorDesc: "interop refusal"})
		res := e.RunCaller(t, p.caller, srv.URL, interop.Peer{Prompt: "anything"})
		if res.Exit != 0 {
			t.Fatalf("caller exit %d; stderr:\n%s", res.Exit, res.Stderr())
		}
		l, ok := res.Find("error")
		if !ok || l.Int("code") != 503 || !strings.Contains(l.Str("message"), "interop refusal") {
			t.Fatalf("error line = %v (all: %v)", l, res.Lines)
		}
		if _, done := res.Find("done"); done {
			t.Fatal("an errored stream also reported done")
		}
	})
}

func TestSelf_Query(t *testing.T) {
	eachPair(t, func(t *testing.T, e *interop.Env, p pair) {
		t.Helper()
		srv := interop.StartServer(t, e)
		host := e.StartHost(t, p.host, srv.URL, interop.Peer{Mode: "query"})
		res := e.RunCaller(t, p.caller, srv.URL, interop.Peer{Prompt: "plan it", QueryAnswer: "go ahead"})
		if res.Exit != 0 {
			t.Fatalf("caller exit %d; stderr:\n%s", res.Exit, res.Stderr())
		}
		if q, ok := res.Find("query"); !ok || q.Str("prompt") != "proceed?" {
			t.Fatalf("query = %v", q)
		}
		if res.Text() != "thinkingreply: go ahead" {
			t.Fatalf("text = %q", res.Text())
		}
		if l := host.WaitForType(t, 5*time.Second, "query_reply"); l.Str("prompt") != "go ahead" {
			t.Fatalf("host got reply %v", l)
		}
	})
}

func TestSelf_Attachments(t *testing.T) {
	eachPair(t, func(t *testing.T, e *interop.Env, p pair) {
		t.Helper()
		srv := interop.StartServer(t, e)
		host := e.StartHost(t, p.host, srv.URL, interop.Peer{Mode: "attachments"})
		atts := []interop.Attachment{{Filename: "a.txt", Data: []byte("alpha")}, {Filename: "b.bin", Data: []byte{0, 1, 2, 255}}}
		res := e.RunCaller(t, p.caller, srv.URL, interop.Peer{Prompt: "look", Attachments: atts})
		if res.Exit != 0 {
			t.Fatalf("caller exit %d; stderr:\n%s", res.Exit, res.Stderr())
		}
		got := host.WaitForType(t, 5*time.Second, "prompt").List("attachments")
		if len(got) != 2 || got[0].Str("filename") != "a.txt" || got[0].Int("size") != 5 || got[1].Int("size") != 4 {
			t.Fatalf("host saw attachments %v", got)
		}
		back, _ := res.Find("response")
		echoed := back.List("attachments")
		if len(echoed) != 2 || echoed[0].Str("sha256") != got[0].Str("sha256") || echoed[1].Str("sha256") != got[1].Str("sha256") {
			t.Fatalf("echoed %v, sent %v", echoed, got)
		}
	})
}

func TestSelf_SlowKeepAlive(t *testing.T) {
	eachPair(t, func(t *testing.T, e *interop.Env, p pair) {
		t.Helper()
		srv := interop.StartServer(t, e)
		e.StartHost(t, p.host, srv.URL, interop.Peer{Mode: "slow", SlowDelay: time.Second, KeepAlive: 200 * time.Millisecond})
		res := e.RunCaller(t, p.caller, srv.URL, interop.Peer{Prompt: "wait for it"})
		if res.Exit != 0 {
			t.Fatalf("caller exit %d; stderr:\n%s", res.Exit, res.Stderr())
		}
		if acks := len(res.All("status")); acks < 3 {
			t.Fatalf("%d acks over a 1s handler at a 200ms cadence, want at least 3 (leading plus keep-alives)", acks)
		}
		if res.Text() != "wait for it" {
			t.Fatalf("text = %q", res.Text())
		}
	})
}

func TestSelf_Signed(t *testing.T) {
	eachPair(t, func(t *testing.T, e *interop.Env, p pair) {
		t.Helper()
		srv := interop.StartServer(t, e, interop.WithFixtureConfig("nkey-noaccounts.conf"))
		seed := e.Seed(t, "alice")
		host := e.StartHost(t, p.host, srv.URL, interop.Peer{Mode: "signed", Seed: seed, Sign: true})
		// A signed caller is served and classified verified.
		res := e.RunCaller(t, p.caller, srv.URL, interop.Peer{Prompt: "signed hello", Seed: seed, Sign: true})
		if res.Exit != 0 {
			t.Fatalf("caller exit %d; stderr:\n%s", res.Exit, res.Stderr())
		}
		id, _ := res.Find("identity")
		if res.Text() != "signed hello" || id.Str("id") == "" {
			t.Fatalf("text %q identity %v; lines %v", res.Text(), id, res.Lines)
		}
		if s := host.WaitForType(t, 5*time.Second, "prompt").Str("sender"); !strings.Contains(s, id.Str("id")) || !strings.Contains(s, "verified") {
			t.Fatalf("host classified sender as %q, caller id %q", s, id.Str("id"))
		}
		// An unsigned caller is refused by a min_sender_trust=signed endpoint: the TypeScript
		// client sends it and the host answers 401; the Python client refuses locally, before
		// sending, with SenderSignatureRequiredError (upstream parity difference).
		res = e.RunCaller(t, p.caller, srv.URL, interop.Peer{Prompt: "anonymous", Seed: seed})
		if l, ok := res.Find("error"); !ok || (l.Int("code") != 401 && l.Str("name") != "SenderSignatureRequiredError") {
			t.Fatalf("unsigned caller: error line %v (all: %v)", l, res.Lines)
		}
	})
}
