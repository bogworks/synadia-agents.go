// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

func stanceSigner(t *testing.T) identity.Signer {
	t.Helper()
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	seed, err := kp.Seed()
	if err != nil {
		t.Fatal(err)
	}
	s, err := identity.SignerFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newStanceClient(t *testing.T, opts ...Option) (*Client, error) {
	t.Helper()
	c, err := New(&nats.Conn{}, opts...)
	if err == nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	return c, err
}

// TestWithStance_Client asserts F2, F3 as the client wires it, and the signer
// requirement: verified discovery makes an unadmitted agent untrusted, an
// explicit trust policy wins, the stance reaches the tracker, and signing on
// makes New fail without a signer unless RequireSigner is off.
func TestWithStance_Client(t *testing.T) {
	signer := stanceSigner(t)
	withSigner := WithIdentity(IdentityOptions{Signer: signer})
	own := TrustPolicy{Agents: []identity.AgentID{"acc.UOWN"}}
	st := func(s identity.Stance) Option { return WithStance(s) }
	on, off := identity.On, identity.Off
	tests := []struct {
		name      string
		opts      []Option
		wantErr   bool
		verified  bool // the client gates Prompt and Status
		ownPolicy bool // the explicit policy is the one in force
		beats     bool // the tracker verifies heartbeats
	}{
		{name: "no stance"},
		{name: "discovery unset is the default", opts: []Option{st(identity.Stance{Signing: off})}},
		{name: "discovery on", opts: []Option{st(identity.Stance{VerifiedDiscovery: on})}, verified: true},
		{name: "discovery off", opts: []Option{st(identity.Stance{VerifiedDiscovery: off})}},
		{name: "heartbeats unset is the default", opts: []Option{st(identity.Stance{Signing: off})}},
		{name: "heartbeats on", opts: []Option{st(identity.Stance{HeartbeatVerification: on})}, beats: true},
		{name: "heartbeats off", opts: []Option{st(identity.Stance{HeartbeatVerification: off})}},
		{name: "all on with a signer", opts: []Option{st(identity.Stance{VerifiedDiscovery: on, HeartbeatVerification: on, Signing: on}), withSigner}, verified: true, beats: true},
		{name: "signing on, signer", opts: []Option{st(identity.Stance{Signing: on}), withSigner}},
		{name: "signing on, no signer", opts: []Option{st(identity.Stance{Signing: on})}, wantErr: true},
		{name: "signing on, RequireSigner on, no signer", opts: []Option{st(identity.Stance{Signing: on, RequireSigner: on})}, wantErr: true},
		{name: "signing on, RequireSigner off, no signer", opts: []Option{st(identity.Stance{Signing: on, RequireSigner: off})}},
		{name: "signing off, RequireSigner on, no signer", opts: []Option{st(identity.Stance{Signing: off, RequireSigner: on})}},
		{name: "explicit policy beats discovery off", opts: []Option{st(identity.Stance{VerifiedDiscovery: off}), WithTrustPolicy(own)}, verified: true, ownPolicy: true},
		{name: "explicit policy beats discovery on", opts: []Option{st(identity.Stance{VerifiedDiscovery: on}), WithTrustPolicy(own)}, verified: true, ownPolicy: true},
		{name: "explicit policy beats discovery on, other order", opts: []Option{WithTrustPolicy(own), st(identity.Stance{VerifiedDiscovery: on})}, verified: true, ownPolicy: true},
		{name: "explicit verification beats heartbeats off", opts: []Option{st(identity.Stance{HeartbeatVerification: off}),
			WithTracker(WithHeartbeatVerification(identity.LiveOptions{Nonces: identity.NewNonceSet(identity.NonceSetOptions{})}, false))}, beats: true},
		{name: "explicit verification beats heartbeats off, other order", opts: []Option{
			WithTracker(WithHeartbeatVerification(identity.LiveOptions{Nonces: identity.NewNonceSet(identity.NonceSetOptions{})}, false)),
			st(identity.Stance{HeartbeatVerification: off})}, beats: true},
		{name: "tracker stance beats the client's", opts: []Option{st(identity.Stance{VerifiedDiscovery: on, HeartbeatVerification: on}),
			WithTracker(WithTrackerStance(identity.Stance{HeartbeatVerification: off}))}, verified: true},
		{name: "invalid discovery", opts: []Option{st(identity.Stance{VerifiedDiscovery: 3})}, wantErr: true},
		{name: "invalid signing", opts: []Option{st(identity.Stance{Signing: 3})}, wantErr: true},
		{name: "invalid RequireSigner", opts: []Option{st(identity.Stance{RequireSigner: 3})}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := newStanceClient(t, tc.opts...)
			if tc.wantErr {
				if err == nil || !strings.HasPrefix(err.Error(), "caller: ") {
					t.Fatalf("err = %v, want a caller: error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := c.cfg.trust != nil; got != tc.verified {
				t.Fatalf("a trust policy is in force = %v, want %v", got, tc.verified)
			}
			if tc.ownPolicy && (c.cfg.trust == nil || len(c.cfg.trust.Agents) != 1) {
				t.Fatal("the explicit policy was replaced")
			}
			if tc.verified && !tc.ownPolicy && !c.cfg.trust.trusts("any.UANY", &agents.AgentInfo{}) {
				t.Fatal("the stance's policy must trust any verified signer")
			}
			// An agent discovery did not admit is refused by Prompt and Status.
			a := newAgent(c, &agents.AgentInfo{}, false)
			_, perr := a.Prompt(context.Background(), agents.Envelope{Prompt: "x"})
			_, serr := a.Status(context.Background())
			if got := errors.Is(perr, ErrUntrustedAgent) && errors.Is(serr, ErrUntrustedAgent); got != tc.verified {
				t.Fatalf("Prompt %v, Status %v; refused as untrusted = %v, want %v", perr, serr, got, tc.verified)
			}
			if got := c.tracker.cfg.verify != nil; got != tc.beats {
				t.Fatalf("the tracker verifies heartbeats = %v, want %v", got, tc.beats)
			}
			if c.tracker.requiresSigned() {
				t.Fatal("the stance must not require signed heartbeats")
			}
		})
	}
}

// TestWithStance_ClientRequireSignerOffWarnsOnce asserts that signing on without
// a signer and RequireSigner off logs one warning to the configured logger, does
// and does not panic without a logger.
func TestWithStance_ClientRequireSignerOffWarnsOnce(t *testing.T) {
	s := WithStance(identity.Stance{Signing: identity.On, RequireSigner: identity.Off})
	var buf bytes.Buffer
	if _, err := newStanceClient(t, s, WithLogger(slog.New(slog.NewTextHandler(&buf, nil)))); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 || !strings.Contains(buf.String(), "no Signer") {
		t.Fatalf("%d warnings, want 1: %s", n, buf.String())
	}
	if _, err := newStanceClient(t, s); err != nil {
		t.Fatal(err)
	}
}

// TestWithStance_ClientWarnsOnlyToTheConfiguredLogger asserts that an unchosen
// stance logs one warning per process to a logger the program set, never when
// no logger was set, and not when a stance or a trust policy was chosen.
func TestWithStance_ClientWarnsOnlyToTheConfiguredLogger(t *testing.T) {
	logTo := func(buf *bytes.Buffer) Option {
		return WithLogger(slog.New(slog.NewTextHandler(buf, nil)))
	}
	warnings := func(buf *bytes.Buffer) int { return strings.Count(buf.String(), "no security settings were chosen") }

	t.Run("once, to the configured logger", func(t *testing.T) {
		resetStanceWarning()
		var buf bytes.Buffer
		for range 3 {
			if _, err := newStanceClient(t, logTo(&buf)); err != nil {
				t.Fatal(err)
			}
		}
		if n := warnings(&buf); n != 1 {
			t.Fatalf("%d warnings, want 1: %s", n, buf.String())
		}
		for _, want := range []string{"level=WARN", "SDK defaults", "WithStance"} {
			if !strings.Contains(buf.String(), want) {
				t.Errorf("warning lacks %q: %s", want, buf.String())
			}
		}
	})
	t.Run("no logger, no warning and the once is kept", func(t *testing.T) {
		resetStanceWarning()
		if _, err := newStanceClient(t); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if _, err := newStanceClient(t, logTo(&buf)); err != nil {
			t.Fatal(err)
		}
		if n := warnings(&buf); n != 1 {
			t.Fatalf("%d warnings, want 1", n)
		}
	})
	t.Run("never the default logger", func(t *testing.T) {
		resetStanceWarning()
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		t.Cleanup(func() { slog.SetDefault(prev) })
		if _, err := newStanceClient(t); err != nil {
			t.Fatal(err)
		}
		if buf.Len() != 0 {
			t.Fatalf("the default logger got %q", buf.String())
		}
	})
	for name, opts := range map[string][]Option{
		"stance chosen": {WithStance(identity.Stance{SignedSenders: identity.Off})},
		"trust policy":  {WithTrustPolicy(TrustPolicy{Agents: []identity.AgentID{"acc.UOWN"}})},
	} {
		t.Run(name+", no warning", func(t *testing.T) {
			resetStanceWarning()
			var buf bytes.Buffer
			if _, err := newStanceClient(t, append(opts, logTo(&buf))...); err != nil {
				t.Fatal(err)
			}
			if n := warnings(&buf); n != 0 {
				t.Fatalf("%d warnings: %s", n, buf.String())
			}
		})
	}
}

// TestWithTrackerStance_Heartbeats asserts F3 on a tracker: a heartbeat whose
// Agent-Sender header fails verification is discarded under strict and kept
// under relaxed, a verified one is kept under both, a heartbeat with no header
// is kept under both, and an explicit WithHeartbeatVerification wins.
func TestWithTrackerStance_Heartbeats(t *testing.T) {
	signer := stanceSigner(t)
	id, err := identity.NewAgentID("$G", signer.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	const subject = "agents.hb.a.o.n"
	beat := func(instance string, tamper bool, sign bool) *nats.Msg {
		payload := hbPayload(instance, "10", "")
		m := &nats.Msg{Subject: subject, Data: payload}
		if sign {
			h, err := identity.SignHeader(identity.SignOptions{Signer: signer, ID: id, Subject: subject, Payload: payload})
			if err != nil {
				t.Fatal(err)
			}
			m.Header = nats.Header{}
			h.Set(m.Header)
			if tamper {
				m.Data = append(append([]byte(nil), payload...), ' ')
			}
		}
		return m
	}
	live := WithHeartbeatVerification(identity.LiveOptions{Nonces: identity.NewNonceSet(identity.NonceSetOptions{})}, false)
	tests := []struct {
		name string
		opts []TrackerOption
		// whether the verified, the tampered and the unsigned beat are kept
		good, bad, bare bool
	}{
		{"no stance", nil, true, true, true},
		{"heartbeats unset is the default", []TrackerOption{WithTrackerStance(identity.Stance{Signing: identity.Off})}, true, true, true},
		{"heartbeats on", []TrackerOption{WithTrackerStance(identity.Stance{HeartbeatVerification: identity.On})}, true, false, true},
		{"heartbeats off", []TrackerOption{WithTrackerStance(identity.Stance{HeartbeatVerification: identity.Off})}, true, true, true},
		{"explicit verification beats heartbeats off", []TrackerOption{WithTrackerStance(identity.Stance{HeartbeatVerification: identity.Off}), live}, true, false, true},
		{"explicit verification, other order", []TrackerOption{live, WithTrackerStance(identity.Stance{HeartbeatVerification: identity.Off})}, true, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTestTracker(t, tc.opts...)
			tr.handle(beat("good", false, true))
			tr.handle(beat("bad", true, true))
			tr.handle(beat("bare", false, false))
			for id, want := range map[string]bool{"good": tc.good, "bad": tc.bad, "bare": tc.bare} {
				if _, ok := tr.Liveness(id); ok != want {
					t.Errorf("%s kept = %v, want %v", id, ok, want)
				}
			}
			if tr.requiresSigned() {
				t.Error("the tracker must not require signed heartbeats")
			}
			if !tc.bad && tr.Dropped() != 1 {
				t.Errorf("dropped %d, want 1", tr.Dropped())
			}
		})
	}
	for name, s := range map[string]identity.Stance{"heartbeats": {HeartbeatVerification: 3}, "signing": {Signing: 3}} {
		if _, err := NewHeartbeatTracker(new(nats.Conn), WithTrackerStance(s)); err == nil {
			t.Errorf("an invalid %s was accepted", name)
		}
	}
}
