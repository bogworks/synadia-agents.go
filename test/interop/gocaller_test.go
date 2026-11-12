// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package interop_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/bogworks/synadia-agents.go/connect"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/bogworks/synadia-agents.go/test/interop"
)

// The Go caller against the official TypeScript and Python hosts (AT-003).

var gcHosts = []interop.Impl{interop.TypeScript, interop.Python}

// gcEach runs fn once per official host, in parallel.
func gcEach(t *testing.T, fn func(t *testing.T, e *interop.Env, impl interop.Impl)) {
	t.Helper()
	e := interop.Require(t)
	for _, impl := range gcHosts {
		t.Run(string(impl)+"Host", func(t *testing.T) {
			t.Parallel()
			fn(t, e, impl)
		})
	}
}

// gcClient connects to url (as the nkey user in seedFile when non-empty, able to
// sign when sign) and returns a caller client.
func gcClient(t *testing.T, url, seedFile string, sign bool, opts ...caller.Option) *caller.Client {
	t.Helper()
	var bopts []connect.BundleOption
	if sign {
		bopts = append(bopts, connect.WithSignedIdentity())
	}
	b, err := connect.ResolveBundle(context.Background(), connect.Source{URL: url, NKey: seedFile}, bopts...)
	if err != nil {
		t.Fatalf("resolve bundle: %v", err)
	}
	nc, err := nats.Connect(b.URL, b.Options...)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if b.Signer != nil {
		opts = append(opts, caller.WithIdentity(caller.IdentityOptions{Signer: b.Signer}))
	}
	c, err := caller.New(nc, opts...)
	if err != nil {
		t.Fatalf("caller.New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()); nc.Close(); b.Wipe() })
	return c
}

func gcCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func gcFind(t *testing.T, c *caller.Client) *caller.Agent {
	t.Helper()
	a, err := c.Find(gcCtx(t), caller.Filter{Agent: "interop", Owner: "interop", Name: "main"})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	return a
}

// gcResult is everything one stream yielded.
type gcResult struct {
	Text        string
	Chunks      int
	Acks        int
	Attachments []agents.Attachment
	Responses   []caller.Response
	Err         error
}

// gcDrain reads a stream to its end; onQuery answers any query.
func gcDrain(s *caller.Stream, onQuery func(*caller.Query) error) gcResult {
	var r gcResult
	var b strings.Builder
	for ev, err := range s.Events() {
		if err != nil {
			r.Err = err
			break
		}
		switch e := ev.(type) {
		case caller.Status:
			r.Acks++
		case caller.Response:
			r.Chunks++
			b.WriteString(e.Text)
			r.Attachments = append(r.Attachments, e.Attachments...)
			r.Responses = append(r.Responses, e)
		case *caller.Query:
			if onQuery != nil {
				if err := onQuery(e); err != nil {
					r.Err = err
					r.Text = b.String()
					return r
				}
			}
		}
	}
	r.Text = b.String()
	return r
}

func gcPrompt(t *testing.T, a *caller.Agent, text string, opts ...caller.PromptOption) gcResult {
	t.Helper()
	s, err := a.PromptText(gcCtx(t), text, opts...)
	if err != nil {
		t.Fatalf("PromptText: %v", err)
	}
	defer s.Close()
	r := gcDrain(s, nil)
	if r.Err == nil && !s.Terminated() {
		t.Errorf("stream ended without the terminator (Terminated() false)")
	}
	return r
}

func gcPromptLines(p *interop.Process) []interop.Line {
	var out []interop.Line
	for _, l := range p.Lines() {
		if l.Type() == "prompt" {
			out = append(out, l)
		}
	}
	return out
}

func gcSHA(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestGoCaller_Discovery(t *testing.T) {
	gcEach(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
		srv := interop.StartServer(t, e)
		host := e.StartHost(t, impl, srv.URL, interop.Peer{Mode: "echo"})
		c := gcClient(t, srv.URL, "", false)
		ctx := gcCtx(t)

		found, err := c.Discover(ctx)
		if err != nil || len(found) != 1 {
			t.Fatalf("Discover = %d agents, err %v", len(found), err)
		}
		a := found[0]
		info := a.Info()
		t.Logf("%s record: agent=%q owner=%q name=%q session=%q protocol_version=%q desc=%q metadata=%v", impl,
			info.Agent, info.Owner, info.Name, info.Session, info.ProtocolVersion, info.Description, info.Metadata)
		if a.PromptSubject() != host.Subject || host.Subject != "agents.prompt.interop.interop.main" {
			t.Errorf("prompt subject %q, host %q", a.PromptSubject(), host.Subject)
		}
		if info.Agent != "interop" || info.Owner != "interop" || info.Name != "main" {
			t.Errorf("identity = %q/%q/%q", info.Agent, info.Owner, info.Name)
		}
		if !strings.HasPrefix(info.ProtocolVersion, "0.3") {
			t.Errorf("protocol_version = %q, want 0.3.x", info.ProtocolVersion)
		}
		if err := a.CheckVersion(); err != nil {
			t.Errorf("default version policy refused the official host: %v", err)
		}
		if impl == interop.Python && info.Session == "" {
			t.Errorf("python host advertised no session")
		}
		lim, err := info.PromptLimits()
		if err != nil || lim.MaxPayload <= 0 || lim.AttachmentsOK == nil {
			t.Errorf("PromptLimits = %+v, %v", lim, err)
		}
		if info.Status == nil {
			t.Errorf("no status endpoint in the record")
		}
		if found2, err := c.Discover(ctx, caller.WithCompatibleOnly(), caller.WithFilter(caller.Filter{Agent: "interop"})); err != nil || len(found2) != 1 {
			t.Errorf("Discover(compatible, filter) = %d, %v", len(found2), err)
		}
		if _, err := c.Find(ctx, caller.Filter{Agent: "nope"}); !errors.Is(err, caller.ErrAgentNotFound) {
			t.Errorf("Find(nope) = %v, want ErrAgentNotFound", err)
		}
		ok, err := c.Ping(ctx, a.InstanceID())
		if err != nil || !ok {
			t.Errorf("Ping = %v, %v", ok, err)
		}
		pings, err := c.PingAll(ctx)
		if err != nil || len(pings) != 1 {
			t.Errorf("PingAll = %v, %v", pings, err)
		}
		if got, err := c.LookupInstance(ctx, a.InstanceID()); err != nil || got.PromptSubject() != host.Subject {
			t.Errorf("LookupInstance = %v, %v", got, err)
		}
	})
}

func TestGoCaller_Echo(t *testing.T) {
	gcEach(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
		srv := interop.StartServer(t, e)
		host := e.StartHost(t, impl, srv.URL, interop.Peer{Mode: "echo", Chunks: 4})
		c := gcClient(t, srv.URL, "", false)
		a := gcFind(t, c)
		const prompt = "hello from the Go caller, with multibyte: héllo 世界 \U0001F600"
		r := gcPrompt(t, a, prompt)
		if r.Err != nil {
			t.Fatalf("stream error: %v", r.Err)
		}
		if r.Text != prompt {
			t.Errorf("echoed %q, want %q", r.Text, prompt)
		}
		if r.Chunks < 2 {
			t.Errorf("%d response chunks, want the host's split (>1)", r.Chunks)
		}
		if r.Acks < 1 {
			t.Errorf("no ack seen")
		}
		l := host.WaitForType(t, 5*time.Second, "prompt")
		if l.Str("prompt") != prompt || l.Str("sender") != "" {
			t.Errorf("host saw %v", l)
		}
		if st := c.Stats(); st.DroppedReplies != 0 || st.MalformedChunks != 0 {
			t.Errorf("stats = %+v", st)
		}
	})
}

func TestGoCaller_Error(t *testing.T) {
	// SAP §9.2 codes.
	codes := []struct {
		code int
		desc string
		is   error
	}{
		{400, "bad request from host", agents.ErrBadRequest},
		{401, "unauthorized by host", agents.ErrUnauthorized},
		{403, "forbidden by host", agents.ErrForbidden},
		{404, "not found on host", agents.ErrNotFound},
		{409, "conflict on host", agents.ErrConflict},
		{429, "busy, retry later", agents.ErrRateLimited},
		{500, "internal host failure", agents.ErrInternal},
		{503, "interop refusal", nil},
	}
	e := interop.Require(t)
	for _, impl := range gcHosts {
		for _, tc := range codes {
			t.Run(fmt.Sprintf("%sHost_%d", impl, tc.code), func(t *testing.T) {
				t.Parallel()
				srv := interop.StartServer(t, e)
				e.StartHost(t, impl, srv.URL, interop.Peer{Mode: "error", ErrorCode: tc.code, ErrorDesc: tc.desc})
				c := gcClient(t, srv.URL, "", false)
				a := gcFind(t, c)
				s, err := a.PromptText(gcCtx(t), "anything")
				var r gcResult
				if err != nil {
					r.Err = err
				} else {
					defer s.Close()
					r = gcDrain(s, nil)
				}
				var se *agents.ServiceError
				if !errors.As(r.Err, &se) {
					t.Fatalf("error = %T %v, want *agents.ServiceError", r.Err, r.Err)
				}
				if se.Code != tc.code || !agents.IsCode(r.Err, tc.code) {
					t.Errorf("code = %d, want %d", se.Code, tc.code)
				}
				if tc.is != nil && !errors.Is(r.Err, tc.is) {
					t.Errorf("errors.Is(%v) false", tc.is)
				}
				if !strings.Contains(se.Description, tc.desc) {
					t.Errorf("description = %q, want to contain %q", se.Description, tc.desc)
				}
				if r.Text != "" {
					t.Errorf("an errored stream carried text %q", r.Text)
				}
			})
		}
	}
}

func TestGoCaller_Attachments(t *testing.T) {
	gcEach(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
		srv := interop.StartServer(t, e)
		host := e.StartHost(t, impl, srv.URL, interop.Peer{Mode: "attachments"})
		c := gcClient(t, srv.URL, "", false)
		a := gcFind(t, c)
		if ok, set := a.Info().Prompt.AttachmentsOK(); !set || !ok {
			t.Fatalf("attachments_ok = %v (set %v)", ok, set)
		}
		big := make([]byte, 200_000)
		_, _ = rand.Read(big)
		files := []struct {
			name string
			data []byte
		}{
			{"a.txt", []byte("alpha")},
			{"b.bin", []byte{0, 1, 2, 255, 254}},
			{"big.dat", big},
		}
		var atts []agents.Attachment
		for _, f := range files {
			atts = append(atts, agents.NewAttachment(f.name, f.data))
		}
		s, err := a.Prompt(gcCtx(t), agents.Envelope{Prompt: "look", Attachments: atts})
		if err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		defer s.Close()
		r := gcDrain(s, nil)
		if r.Err != nil {
			t.Fatalf("stream error: %v", r.Err)
		}
		if r.Text != "received 3" {
			t.Errorf("text = %q", r.Text)
		}
		// What the host decoded from the request.
		got := host.WaitForType(t, 5*time.Second, "prompt").List("attachments")
		if len(got) != len(files) {
			t.Fatalf("host saw %d attachments: %v", len(got), got)
		}
		for i, f := range files {
			if got[i].Str("filename") != f.name || got[i].Int("size") != len(f.data) || got[i].Str("sha256") != gcSHA(f.data) {
				t.Errorf("host's view of attachment %d = %v, sent %s %d %s", i, got[i], f.name, len(f.data), gcSHA(f.data))
			}
		}
		// What came back.
		if len(r.Attachments) != len(files) {
			t.Fatalf("echoed %d attachments", len(r.Attachments))
		}
		for i, f := range files {
			b, err := r.Attachments[i].Bytes()
			if err != nil || r.Attachments[i].Filename != f.name || !bytes.Equal(b, f.data) {
				t.Errorf("echoed attachment %d: name %q, %d bytes, err %v", i, r.Attachments[i].Filename, len(b), err)
			}
		}
		dir := t.TempDir()
		saved, err := r.Responses[0].SaveAttachments(dir, agents.SaveOptions{})
		if err != nil || len(saved) != len(files) {
			t.Fatalf("SaveAttachments = %v, %v", saved, err)
		}
		for i, f := range files {
			if saved[i].Path == "" || filepath.Dir(saved[i].Path) != dir {
				t.Errorf("saved[%d] = %+v", i, saved[i])
				continue
			}
			b, err := os.ReadFile(saved[i].Path)
			if err != nil || !bytes.Equal(b, f.data) || saved[i].Size != int64(len(f.data)) {
				t.Errorf("saved file %d differs (%v)", i, err)
			}
		}
	})
}

func TestGoCaller_Query(t *testing.T) {
	gcEach(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
		srv := interop.StartServer(t, e)
		host := e.StartHost(t, impl, srv.URL, interop.Peer{Mode: "query"})
		c := gcClient(t, srv.URL, "", false)
		a := gcFind(t, c)
		ctx := gcCtx(t)
		s, err := a.PromptText(ctx, "plan it")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		var asked string
		r := gcDrain(s, func(q *caller.Query) error {
			asked = q.Prompt
			err := q.Reply(ctx, "go ahead")
			if err == nil {
				if err2 := q.Reply(ctx, "again"); !errors.Is(err2, caller.ErrQueryAlreadyReplied) {
					t.Errorf("second Reply = %v, want ErrQueryAlreadyReplied", err2)
				}
			}
			return err
		})
		if r.Err != nil {
			t.Fatalf("stream error: %v", r.Err)
		}
		if asked != "proceed?" {
			t.Errorf("query prompt = %q", asked)
		}
		if r.Text != "thinkingreply: go ahead" {
			t.Errorf("text = %q", r.Text)
		}
		if l := host.WaitForType(t, 5*time.Second, "query_reply"); l.Str("prompt") != "go ahead" {
			t.Errorf("host got reply %v", l)
		}
		if !s.Terminated() {
			t.Error("not terminated")
		}
	})
}

func TestGoCaller_SlowKeepAlive(t *testing.T) {
	gcEach(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
		srv := interop.StartServer(t, e)
		e.StartHost(t, impl, srv.URL, interop.Peer{Mode: "slow", SlowDelay: 2 * time.Second, KeepAlive: 300 * time.Millisecond})
		// Inactivity timeout shorter than the handler delay, longer than the keep-alive cadence.
		c := gcClient(t, srv.URL, "", false, caller.WithInactivityTimeout(900*time.Millisecond))
		a := gcFind(t, c)
		r := gcPrompt(t, a, "wait for it")
		if r.Err != nil {
			t.Fatalf("stream error (inactivity timeout despite keep-alives?): %v", r.Err)
		}
		if r.Text != "wait for it" {
			t.Errorf("text = %q", r.Text)
		}
		if r.Acks < 3 {
			t.Errorf("%d acks over a 2s handler at 300ms cadence, want at least 3", r.Acks)
		}
	})
}

// Heartbeat stays at 1s: the official Python host cannot take a fractional interval (its
// heartbeat payload types interval_s as int; with 0.5 its status endpoint answers 500
// "status handler error" and its heartbeats say 1), so a sub-second cadence is untestable there.
func TestGoCaller_Status_Heartbeat(t *testing.T) {
	gcEach(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
		srv := interop.StartServer(t, e)
		host := e.StartHost(t, impl, srv.URL, interop.Peer{Mode: "echo", Heartbeat: time.Second})
		c := gcClient(t, srv.URL, "", false)
		ctx := gcCtx(t)
		a := gcFind(t, c)

		hb, err := a.Status(ctx)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		t.Logf("%s status reply: ts=%q interval_s=%v session=%q protocol_version=%q", impl, hb.TS, hb.IntervalS, hb.Session, hb.ProtocolVersion)
		if hb.Agent != "interop" || hb.Owner != "interop" || hb.InstanceID != a.InstanceID() || hb.IntervalS <= 0 {
			t.Errorf("status = %+v", hb)
		}
		if _, err := hb.Time(); err != nil {
			t.Errorf("status ts %q does not parse: %v", hb.TS, err)
		}
		if ep, ok := hb.PromptEndpoint(); !ok || ep.Subject != host.Subject {
			t.Errorf("status prompt endpoint = %+v, %v", ep, ok)
		}

		// Tracker: online while the host beats, offline after it stops.
		tr := c.Tracker()
		deadline := time.Now().Add(10 * time.Second)
		var live caller.Liveness
		for time.Now().Before(deadline) {
			if l, ok := tr.Liveness(a.InstanceID()); ok && l.Online {
				live = l
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !live.Online {
			t.Fatalf("tracker never saw the host online (dropped %d)", tr.Dropped())
		}
		if live.Agent != "interop" || live.Owner != "interop" || live.Name != "main" || live.Interval != time.Second {
			t.Errorf("liveness = %+v", live)
		}
		if tr.Dropped() != 0 {
			t.Errorf("tracker dropped %d heartbeats (official heartbeat rejected?)", tr.Dropped())
		}
		host.Stop()
		deadline = time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if l, ok := tr.Liveness(a.InstanceID()); ok && !l.Online {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Errorf("tracker still reports the stopped host online")
	})
}

func TestGoCaller_MaxPayload(t *testing.T) {
	gcEach(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
		srv := interop.StartServer(t, e)
		host := e.StartHost(t, impl, srv.URL, interop.Peer{Mode: "echo"})
		c := gcClient(t, srv.URL, "", false)
		a := gcFind(t, c)
		max, ok := a.Info().Prompt.MaxPayload()
		if !ok || max <= 0 {
			t.Fatalf("host declares no max_payload")
		}
		t.Logf("%s declares max_payload %d; connection max %d", impl, max, c.Connection().MaxPayload())
		_, err := a.PromptText(gcCtx(t), strings.Repeat("x", int(max)+1))
		if err == nil {
			t.Fatal("a prompt over max_payload was sent")
		}
		if !errors.Is(err, agents.ErrLocal) {
			t.Errorf("error %v does not match agents.ErrLocal", err)
		}
		t.Logf("refusal: %v", err)
		time.Sleep(500 * time.Millisecond)
		if n := len(gcPromptLines(host)); n != 0 {
			t.Errorf("host saw %d prompts after a local refusal", n)
		}
		// A prompt within limits still works afterwards.
		if r := gcPrompt(t, a, "fine"); r.Err != nil || r.Text != "fine" {
			t.Errorf("follow-up prompt: %q, %v", r.Text, r.Err)
		}
	})
}

func TestGoCaller_Signed(t *testing.T) {
	gcEach(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
		srv := interop.StartServer(t, e, interop.WithFixtureConfig("nkey-noaccounts.conf"))
		seed := e.Seed(t, "alice")
		host := e.StartHost(t, impl, srv.URL, interop.Peer{Mode: "signed", Seed: seed, Sign: true})

		c := gcClient(t, srv.URL, seed, true)
		ctx := gcCtx(t)
		a := gcFind(t, c)
		if a.MinSenderTrust() != identity.TrustSigned {
			t.Errorf("MinSenderTrust = %v, want signed", a.MinSenderTrust())
		}
		r := gcPrompt(t, a, "signed hello")
		if r.Err != nil || r.Text != "signed hello" {
			t.Fatalf("signed prompt: %q, %v", r.Text, r.Err)
		}
		id, err := c.SelfID(ctx)
		if err != nil {
			t.Fatalf("SelfID: %v", err)
		}
		if s := host.WaitForType(t, 5*time.Second, "prompt").Str("sender"); !strings.Contains(s, id.String()) || !strings.Contains(s, "verified") {
			t.Errorf("host classified sender as %q, caller id %q", s, id)
		}
		// Status is signed too and must be answered.
		if _, err := a.Status(ctx); err != nil {
			t.Errorf("signed Status: %v", err)
		}
		// The host's id_sig verifies on the Go side, and resolves to it (alice is both caller and host).
		reg, found, err := c.ResolveSender(ctx, id)
		if err != nil || !found {
			t.Errorf("ResolveSender(%s) = %+v, %v, %v", id, reg, found, err)
		}

		// No signer: refused locally, nothing sent.
		before := len(gcPromptLines(host))
		c2 := gcClient(t, srv.URL, seed, false)
		a2 := gcFind(t, c2)
		_, err = a2.PromptText(ctx, "anonymous")
		if !errors.Is(err, identity.ErrCannotSign) {
			t.Errorf("unsigned prompt error = %v, want identity.ErrCannotSign", err)
		}
		if !errors.Is(err, agents.ErrLocal) {
			t.Logf("note: ErrCannotSign refusal does not match agents.ErrLocal: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
		if n := len(gcPromptLines(host)); n != before {
			t.Errorf("host saw %d prompts after a local refusal, had %d", n, before)
		}
	})
}

func TestGoCaller_CrossAccount(t *testing.T) {
	gcEach(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
		srv := interop.StartServer(t, e, interop.WithFixtureConfig("accounts.conf"))
		// alice (ACME) hosts; bob (APP) imports the agents and $SRV exports with share: true.
		host := e.StartHost(t, impl, srv.URL, interop.Peer{Mode: "signed", Seed: e.Seed(t, "alice"), Sign: true})
		c := gcClient(t, srv.URL, e.Seed(t, "bob"), true)
		ctx := gcCtx(t)
		a := gcFind(t, c)
		r := gcPrompt(t, a, "across accounts")
		if r.Err != nil || r.Text != "across accounts" {
			t.Fatalf("cross-account prompt: %q, %v", r.Text, r.Err)
		}
		id, err := c.SelfID(ctx)
		if err != nil {
			t.Fatalf("SelfID: %v", err)
		}
		s := host.WaitForType(t, 5*time.Second, "prompt").Str("sender")
		t.Logf("%s host saw sender %q (caller id %s)", impl, s, id)
		if !strings.Contains(s, id.String()) || !strings.Contains(s, "verified") {
			t.Errorf("host classified sender as %q, caller id %q", s, id)
		}
	})
}

func TestGoCaller_Concurrent32(t *testing.T) {
	gcEach(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
		srv := interop.StartServer(t, e)
		e.StartHost(t, impl, srv.URL, interop.Peer{Mode: "echo", Chunks: 5})
		c := gcClient(t, srv.URL, "", false)
		a := gcFind(t, c)
		const n = 32
		var wg sync.WaitGroup
		errs := make([]string, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				want := fmt.Sprintf("prompt-%02d-%s", i, strings.Repeat(string(rune('a'+i%26)), 40+i))
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				defer cancel()
				s, err := a.PromptText(ctx, want)
				if err != nil {
					errs[i] = "prompt: " + err.Error()
					return
				}
				defer s.Close()
				r := gcDrain(s, nil)
				switch {
				case r.Err != nil:
					errs[i] = "stream: " + r.Err.Error()
				case r.Text != want:
					errs[i] = fmt.Sprintf("got %q, want %q", r.Text, want)
				case !s.Terminated():
					errs[i] = "not terminated"
				}
			}()
		}
		wg.Wait()
		for i, m := range errs {
			if m != "" {
				t.Errorf("prompt %d: %s", i, m)
			}
		}
		st := c.Stats()
		if st.DroppedReplies != 0 || st.MalformedChunks != 0 || st.SlowConsumers != 0 {
			t.Errorf("stats = %+v", st)
		}
		if st.ActiveRoutes != 0 {
			t.Errorf("%d routes still active", st.ActiveRoutes)
		}
	})
}
