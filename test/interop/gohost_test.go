// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package interop_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bogworks/synadia-agents.go/host"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/bogworks/synadia-agents.go/test/interop"
)

// AT-003, Go host x official callers: the Go host serves the TypeScript and the
// Python client. Every test runs once per caller; set SAP_INTEROP_EVIDENCE to
// keep the callers' transcripts.

func TestGoHost_Discovery(t *testing.T) {
	eachCaller(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
		t.Helper()
		srv := interop.StartServer(t, e)
		g := startGoHost(t, srv.URL, "", false, hostMode{kind: "echo"})
		res := e.RunCaller(t, impl, srv.URL, interop.Peer{Agent: "interop", Owner: "interop", Name: "main"})
		mustExit0(t, res)
		d, _ := res.Find("discovered")
		list := d.List("agents")
		if len(list) != 1 {
			t.Fatalf("discovered %v", d)
		}
		a := list[0]
		if a.Str("agent") != "interop" || a.Str("owner") != "interop" || a.Str("name") != "main" ||
			a.Str("subject") != g.Subject || g.Subject != "agents.prompt.interop.interop.main" {
			t.Fatalf("discovered metadata %v, host subject %q", a, g.Subject)
		}
		if a.Str("protocol_version") != "0.3" {
			t.Errorf("protocol_version = %q, want 0.3", a.Str("protocol_version"))
		}
		if a.Str("min_sender_trust") != "any" {
			t.Errorf("min_sender_trust = %v, want any", a["min_sender_trust"])
		}
		if a["attachments_ok"] != true {
			t.Errorf("attachments_ok = %v, want true", a["attachments_ok"])
		}
		if a.Int("max_payload") <= 0 {
			t.Errorf("max_payload = %v, want a positive limit", a["max_payload"])
		}
		// A filter that matches nothing finds nothing (exit 2).
		res = e.RunCaller(t, impl, srv.URL, interop.Peer{Agent: "nobody"})
		if res.Exit != 2 {
			t.Errorf("non-matching filter: exit %d, want 2; lines %v", res.Exit, res.Lines)
		}
	})
}

func TestGoHost_Echo(t *testing.T) {
	eachCaller(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
		t.Helper()
		srv := interop.StartServer(t, e)
		g := startGoHost(t, srv.URL, "", false, hostMode{kind: "echo", chunks: 3})
		const prompt = "hello from the interop harness, with unicode: héllo ✓"
		res := e.RunCaller(t, impl, srv.URL, interop.Peer{Prompt: prompt})
		mustExit0(t, res)
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
		if s := g.waitPrompt(t, 0); s.Prompt != prompt || s.Sender != nil {
			t.Fatalf("host saw %+v", s)
		}
	})
}

func TestGoHost_Error(t *testing.T) {
	// Every code of SAP §9.2 the host can send.
	for _, c := range []struct {
		code int
		desc string
	}{
		{400, "bad request refusal"}, {401, "unauthorized refusal"}, {403, "forbidden refusal"},
		{404, "not found refusal"}, {409, "conflict refusal"}, {429, "busy refusal"}, {500, "internal refusal"},
	} {
		t.Run(strconv.Itoa(c.code), func(t *testing.T) {
			eachCaller(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
				t.Helper()
				srv := interop.StartServer(t, e)
				startGoHost(t, srv.URL, "", false, hostMode{kind: "error", errCode: c.code, errDesc: c.desc})
				res := e.RunCaller(t, impl, srv.URL, interop.Peer{Prompt: "anything"})
				mustExit0(t, res)
				l, ok := res.Find("error")
				if !ok {
					t.Fatalf("no error line: %v", res.Lines)
				}
				// TypeScript exposes code and description; Python flattens the error to text
				// ("service error <code>: <description>") and the peer parses the code from it.
				if l.Int("code") != c.code || !strings.Contains(l.Str("message"), c.desc) {
					t.Fatalf("error line = %v, want code %d containing %q", l, c.code, c.desc)
				}
				if _, done := res.Find("done"); done {
					t.Fatal("an errored stream also reported done")
				}
			})
		})
	}
}

func TestGoHost_HandlerErrorIs500(t *testing.T) {
	// A handler that returns a plain error is a 500 "handler error", its text never reaching the caller.
	eachCaller(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
		t.Helper()
		srv := interop.StartServer(t, e)
		startGoHost(t, srv.URL, "", false, hostMode{kind: "plainerror"})
		res := e.RunCaller(t, impl, srv.URL, interop.Peer{Prompt: "x"})
		mustExit0(t, res)
		l, _ := res.Find("error")
		if l.Int("code") != 500 || strings.Contains(l.Str("message"), "secret") {
			t.Fatalf("error line = %v", l)
		}
	})
}

func TestGoHost_Attachments(t *testing.T) {
	eachCaller(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
		t.Helper()
		srv := interop.StartServer(t, e)
		g := startGoHost(t, srv.URL, "", false, hostMode{kind: "attachments"})
		atts := []interop.Attachment{{Filename: "a.txt", Data: []byte("alpha")}, {Filename: "b.bin", Data: []byte{0, 1, 2, 255}}}
		res := e.RunCaller(t, impl, srv.URL, interop.Peer{Prompt: "look", Attachments: atts})
		mustExit0(t, res)
		s := g.waitPrompt(t, 0)
		if len(s.Attachments) != 2 || s.Attachments[0] != (seenAttachment{"a.txt", 5, sum([]byte("alpha"))}) ||
			s.Attachments[1] != (seenAttachment{"b.bin", 4, sum([]byte{0, 1, 2, 255})}) {
			t.Fatalf("host saw attachments %+v", s.Attachments)
		}
		back, ok := res.Find("response")
		if !ok || back.Str("text") != "received 2" {
			t.Fatalf("response = %v", back)
		}
		echoed := back.List("attachments")
		if len(echoed) != 2 {
			t.Fatalf("echoed %v", echoed)
		}
		for i, a := range s.Attachments {
			if echoed[i].Str("filename") != a.Name || echoed[i].Int("size") != a.Size || echoed[i].Str("sha256") != a.SHA256 {
				t.Fatalf("attachment %d: caller got %v, sent %+v", i, echoed[i], a)
			}
		}
	})
}

func TestGoHost_Query(t *testing.T) {
	eachCaller(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
		t.Helper()
		srv := interop.StartServer(t, e)
		g := startGoHost(t, srv.URL, "", false, hostMode{kind: "query"})
		res := e.RunCaller(t, impl, srv.URL, interop.Peer{Prompt: "plan it", QueryAnswer: "go ahead"})
		mustExit0(t, res)
		if q, ok := res.Find("query"); !ok || q.Str("prompt") != "proceed?" {
			t.Fatalf("query = %v; lines %v", q, res.Lines)
		}
		if res.Text() != "thinkingreply: go ahead" {
			t.Fatalf("text = %q", res.Text())
		}
		if s := g.waitPrompt(t, 0); s.QueryReply != "go ahead" {
			t.Fatalf("host got reply %q", s.QueryReply)
		}
	})
}

func TestGoHost_SlowKeepAlive(t *testing.T) {
	eachCaller(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
		t.Helper()
		srv := interop.StartServer(t, e)
		startGoHost(t, srv.URL, "", false, hostMode{kind: "slow", slowDelay: 3 * time.Second}, host.WithKeepalive(200*time.Millisecond))
		// The peers use one value for both the inactivity timeout and the overall ceiling, so the
		// handler (3s) must finish inside it: this shows the caller sees the keep-alive acks and does
		// not time out, not that the acks alone reset the inactivity timer.
		res := e.RunCaller(t, impl, srv.URL, interop.Peer{Prompt: "wait for it", Timeout: 8 * time.Second})
		mustExit0(t, res)
		if _, bad := res.Find("error"); bad {
			t.Fatalf("caller errored: %v", res.Lines)
		}
		if acks := len(res.All("status")); acks < 5 {
			t.Fatalf("%d acks over a 3s handler at a 200ms cadence, want at least 5", acks)
		}
		if res.Text() != "wait for it" {
			t.Fatalf("text = %q", res.Text())
		}
	})
}

func TestGoHost_Status_Heartbeat(t *testing.T) {
	eachCaller(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
		t.Helper()
		srv := interop.StartServer(t, e)
		startGoHost(t, srv.URL, "", false, hostMode{kind: "echo"}, host.WithHeartbeatInterval(time.Second))
		res := e.RunCaller(t, impl, srv.URL, interop.Peer{Agent: "interop", Status: true, WatchHeartbeat: 2500 * time.Millisecond})
		mustExit0(t, res)
		if got := len(res.All("heartbeat")); got < 2 {
			t.Fatalf("saw %d heartbeats in 2.5s at a 1s interval, want at least 2", got)
		}
		for _, hb := range res.All("heartbeat") {
			if hb.Str("agent") != "interop" || hb.Str("owner") != "interop" || hb.Int("interval_s") != 1 {
				t.Fatalf("heartbeat = %v", hb)
			}
		}
		if l, _ := res.Find("liveness"); !l.Bool("online") {
			t.Fatalf("liveness = %v", l)
		}
		if s, ok := res.Find("status_reply"); !ok || s.Str("agent") != "interop" || s.Str("owner") != "interop" || s.Int("interval_s") != 1 {
			t.Fatalf("status_reply = %v", s)
		}
	})
}

func TestGoHost_Signed(t *testing.T) {
	eachCaller(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
		t.Helper()
		srv := interop.StartServer(t, e, interop.WithFixtureConfig("nkey-noaccounts.conf"))
		seed := e.Seed(t, "alice")
		g := startGoHost(t, srv.URL, seed, true, hostMode{kind: "echo"}, host.WithMinSenderTrust(identity.TrustSigned))

		// Discovery advertises the requirement.
		res := e.RunCaller(t, impl, srv.URL, interop.Peer{Seed: seed, Sign: true})
		mustExit0(t, res)
		d, _ := res.Find("discovered")
		if l := d.List("agents"); len(l) != 1 || l[0].Str("min_sender_trust") != "signed" {
			t.Fatalf("discovered %v", d)
		}

		// A signed caller is served and classified verified.
		res = e.RunCaller(t, impl, srv.URL, interop.Peer{Prompt: "signed hello", Seed: seed, Sign: true})
		mustExit0(t, res)
		id, _ := res.Find("identity")
		if res.Text() != "signed hello" || id.Str("id") == "" {
			t.Fatalf("text %q identity %v; lines %v", res.Text(), id, res.Lines)
		}
		s := g.waitPrompt(t, 0)
		if s.Sender == nil || s.Sender.Trust != identity.TrustVerified || s.Sender.ID.String() != id.Str("id") {
			t.Fatalf("host classified sender as %+v, caller id %q", s.Sender, id.Str("id"))
		}
		if !strings.Contains(s.Sender.ID.String(), e.PublicKey(t, "alice")) {
			t.Fatalf("sender id %q does not carry alice's key", s.Sender.ID)
		}

		// An unsigned caller: the TypeScript client sends it and the host answers 401 on the wire;
		// the Python client refuses locally (SenderSignatureRequiredError), sending nothing.
		res = e.RunCaller(t, impl, srv.URL, interop.Peer{Prompt: "anonymous", Seed: seed})
		l, ok := res.Find("error")
		switch impl {
		case interop.TypeScript:
			if !ok || l.Int("code") != 401 {
				t.Fatalf("unsigned TS caller: error line %v (all: %v)", l, res.Lines)
			}
		case interop.Python:
			if !ok || l.Str("name") != "SenderSignatureRequiredError" {
				t.Fatalf("unsigned Python caller: error line %v (all: %v)", l, res.Lines)
			}
		}
		// The unsigned prompt never reaches the handler.
		time.Sleep(200 * time.Millisecond)
		if n := g.count(); n != 1 {
			t.Fatalf("handler ran %d times, want 1", n)
		}
	})
}

func TestGoHost_Signed_CrossAccount(t *testing.T) {
	// The host is carol in ACME, the caller bob in APP, which imports the agents.> and $SRV.> exports.
	eachCaller(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
		t.Helper()
		srv := interop.StartServer(t, e, interop.WithFixtureConfig("accounts.conf"))
		carol, bob := e.Seed(t, "carol"), e.Seed(t, "bob")
		g := startGoHost(t, srv.URL, carol, true, hostMode{kind: "echo"}, host.WithMinSenderTrust(identity.TrustSigned))
		res := e.RunCaller(t, impl, srv.URL, interop.Peer{Prompt: "across accounts", Seed: bob, Sign: true})
		mustExit0(t, res)
		if res.Text() != "across accounts" {
			t.Fatalf("text %q; lines %v", res.Text(), res.Lines)
		}
		id, _ := res.Find("identity")
		s := g.waitPrompt(t, 0)
		if s.Sender == nil || s.Sender.Trust != identity.TrustVerified || s.Sender.ID.String() != id.Str("id") {
			t.Fatalf("host classified sender as %+v, caller id %q", s.Sender, id.Str("id"))
		}
		if !strings.Contains(s.Sender.ID.String(), e.PublicKey(t, "bob")) {
			t.Fatalf("sender id %q does not carry bob's key", s.Sender.ID)
		}
	})
}
