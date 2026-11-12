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
// explicit trust policy wins, the stance reaches the tracker, and a stance that
// turns signing on makes New fail without a signer.
func TestWithStance_Client(t *testing.T) {
	signer := stanceSigner(t)
	withSigner := WithIdentity(IdentityOptions{Signer: signer})
	own := TrustPolicy{Agents: []identity.AgentID{"acc.UOWN"}}
	st := func(s identity.Stance) Option { return WithStance(s) }
	tests := []struct {
		name      string
		opts      []Option
		wantErr   bool
		verified  bool // the client gates Prompt and Status
		ownPolicy bool // the explicit policy is the one in force
		beats     bool // the tracker verifies heartbeats
	}{
		{name: "no stance"},
		{name: "relaxed", opts: []Option{st(identity.Stance{Profile: identity.Relaxed})}},
		{name: "strict", opts: []Option{st(identity.Stance{Profile: identity.Strict}), withSigner}, verified: true, beats: true},
		{name: "strict without a signer", opts: []Option{st(identity.Stance{Profile: identity.Strict})}, wantErr: true},
		{name: "strict, signing off, no signer", opts: []Option{st(identity.Stance{Profile: identity.Strict, Signing: identity.Off})}, verified: true, beats: true},
		{name: "strict, discovery off", opts: []Option{st(identity.Stance{Profile: identity.Strict, VerifiedDiscovery: identity.Off}), withSigner}, beats: true},
		{name: "strict, heartbeats off", opts: []Option{st(identity.Stance{Profile: identity.Strict, HeartbeatVerification: identity.Off}), withSigner}, verified: true},
		{name: "strict, senders off", opts: []Option{st(identity.Stance{Profile: identity.Strict, SignedSenders: identity.Off}), withSigner}, verified: true, beats: true},
		{name: "relaxed, signing on, signer", opts: []Option{st(identity.Stance{Profile: identity.Relaxed, Signing: identity.On}), withSigner}},
		{name: "relaxed, signing on, no signer", opts: []Option{st(identity.Stance{Profile: identity.Relaxed, Signing: identity.On})}, wantErr: true},
		{name: "relaxed, discovery on", opts: []Option{st(identity.Stance{Profile: identity.Relaxed, VerifiedDiscovery: identity.On})}, verified: true},
		{name: "relaxed, heartbeats on", opts: []Option{st(identity.Stance{Profile: identity.Relaxed, HeartbeatVerification: identity.On})}, beats: true},
		{name: "explicit policy beats strict", opts: []Option{st(identity.Stance{Profile: identity.Strict}), withSigner, WithTrustPolicy(own)}, verified: true, ownPolicy: true, beats: true},
		{name: "explicit policy beats strict, other order", opts: []Option{WithTrustPolicy(own), withSigner, st(identity.Stance{Profile: identity.Strict})}, verified: true, ownPolicy: true, beats: true},
		{name: "explicit policy beats discovery off", opts: []Option{st(identity.Stance{VerifiedDiscovery: identity.Off}), WithTrustPolicy(own)}, verified: true, ownPolicy: true},
		{name: "explicit verification beats heartbeats off", opts: []Option{st(identity.Stance{Profile: identity.Strict, HeartbeatVerification: identity.Off}), withSigner,
			WithTracker(WithHeartbeatVerification(identity.LiveOptions{Nonces: identity.NewNonceSet(identity.NonceSetOptions{})}, false))}, verified: true, beats: true},
		{name: "tracker stance beats the client's", opts: []Option{st(identity.Stance{Profile: identity.Strict}), withSigner,
			WithTracker(WithTrackerStance(identity.Stance{Profile: identity.Relaxed}))}, verified: true},
		{name: "invalid profile", opts: []Option{st(identity.Stance{Profile: 9})}, wantErr: true},
		{name: "invalid setting", opts: []Option{st(identity.Stance{Signing: 9})}, wantErr: true},
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

// TestWithStance_ClientWarnsOnlyToTheConfiguredLogger asserts that an unchosen
// stance logs one warning per process to a logger the program set, never when
// no logger was set, and not when a stance or a trust policy was chosen.
func TestWithStance_ClientWarnsOnlyToTheConfiguredLogger(t *testing.T) {
	logTo := func(buf *bytes.Buffer) Option {
		return WithLogger(slog.New(slog.NewTextHandler(buf, nil)))
	}
	warnings := func(buf *bytes.Buffer) int { return strings.Count(buf.String(), "no security stance was chosen") }

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
		for _, want := range []string{"level=WARN", "relaxed", "WithStance"} {
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
		"stance chosen": {WithStance(identity.Stance{Profile: identity.Relaxed})},
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
		{"relaxed", []TrackerOption{WithTrackerStance(identity.Stance{Profile: identity.Relaxed})}, true, true, true},
		{"strict", []TrackerOption{WithTrackerStance(identity.Stance{Profile: identity.Strict})}, true, false, true},
		{"strict, heartbeats off", []TrackerOption{WithTrackerStance(identity.Stance{Profile: identity.Strict, HeartbeatVerification: identity.Off})}, true, true, true},
		{"relaxed, heartbeats on", []TrackerOption{WithTrackerStance(identity.Stance{Profile: identity.Relaxed, HeartbeatVerification: identity.On})}, true, false, true},
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
	for name, s := range map[string]identity.Stance{"profile": {Profile: 9}, "setting": {HeartbeatVerification: 9}} {
		if _, err := NewHeartbeatTracker(new(nats.Conn), WithTrackerStance(s)); err == nil {
			t.Errorf("an invalid %s was accepted", name)
		}
	}
}
