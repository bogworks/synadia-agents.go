// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller_test

import (
	"errors"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
)

// TestStance_VerifiedDiscovery asserts F2 end to end: under the strict profile
// discovery drops the record with no id_sig and the one signed for another
// subject, keeps the validly signed ones of any signer, and a lookup of the
// unsigned one fails; under relaxed every record is kept. An explicit trust
// policy wins over the stance, and Signing On makes New need a signer.
func TestStance_VerifiedDiscovery(t *testing.T) {
	f := newTrustFixture(t)
	ctx := ctxT(t)
	signer := withSigner(t, "")
	onlyAlice := caller.TrustPolicy{Agents: []identity.AgentID{f.aliceID}}

	tests := []struct {
		name     string
		opts     []caller.Option
		wantKept int
		rejected uint64
		plain    bool // a lookup of the unsigned record succeeds
	}{
		{"no stance", nil, 4, 0, true},
		{"relaxed", []caller.Option{caller.WithStance(identity.Stance{Profile: identity.Relaxed})}, 4, 0, true},
		{"strict", []caller.Option{caller.WithStance(identity.Stance{Profile: identity.Strict}), signer}, 2, 2, false},
		{"strict, signing off", []caller.Option{caller.WithStance(identity.Stance{Profile: identity.Strict, Signing: identity.Off})}, 2, 2, false},
		{"strict, discovery off", []caller.Option{caller.WithStance(identity.Stance{Profile: identity.Strict, VerifiedDiscovery: identity.Off}), signer}, 4, 0, true},
		{"strict, other checks off", []caller.Option{caller.WithStance(identity.Stance{Profile: identity.Strict, SignedSenders: identity.Off, HeartbeatVerification: identity.Off}), signer}, 2, 2, false},
		{"relaxed, discovery on", []caller.Option{caller.WithStance(identity.Stance{Profile: identity.Relaxed, VerifiedDiscovery: identity.On})}, 2, 2, false},
		{"explicit policy beats strict", []caller.Option{caller.WithStance(identity.Stance{Profile: identity.Strict}), signer, caller.WithTrustPolicy(onlyAlice)}, 1, 3, false},
		{"explicit policy beats discovery off", []caller.Option{caller.WithTrustPolicy(onlyAlice), caller.WithStance(identity.Stance{VerifiedDiscovery: identity.Off})}, 1, 3, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, f.cnc, tc.opts...)
			got, err := c.Discover(ctx, trustWin)
			if err != nil || len(got) != tc.wantKept {
				t.Fatalf("kept %v, want %d: %v", hostNames(got), tc.wantKept, err)
			}
			if n := c.Stats().RejectedRecords; n != tc.rejected {
				t.Errorf("RejectedRecords %d, want %d", n, tc.rejected)
			}
			_, err = c.LookupInstance(ctx, f.plain.id, 500*time.Millisecond)
			if tc.plain != (err == nil) || (!tc.plain && !errors.Is(err, caller.ErrAgentNotFound)) {
				t.Errorf("lookup of the unsigned record: %v", err)
			}
		})
	}

	t.Run("strict client prompts an admitted agent", func(t *testing.T) {
		c := newClient(t, f.cnc, caller.WithStance(identity.Stance{Profile: identity.Strict}), signer)
		a, err := c.Find(ctx, caller.Filter{Name: "trusted"}, trustWin)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.PromptText(ctx, "hi"); errors.Is(err, caller.ErrUntrustedAgent) {
			t.Fatalf("admitted agent refused: %v", err)
		}
		if _, err := a.Status(ctx); errors.Is(err, caller.ErrUntrustedAgent) {
			t.Fatalf("admitted agent refused status: %v", err)
		}
	})
	t.Run("signing needs a signer", func(t *testing.T) {
		for _, s := range []identity.Stance{{Profile: identity.Strict}, {Signing: identity.On}} {
			if _, err := caller.New(f.cnc, caller.WithStance(s)); err == nil {
				t.Errorf("%+v: New succeeded without a signer", s)
			}
		}
		for _, s := range []identity.Stance{{Profile: identity.Strict, Signing: identity.Off}, {Profile: identity.Relaxed}} {
			c, err := caller.New(f.cnc, caller.WithStance(s))
			if err != nil {
				t.Errorf("%+v: %v", s, err)
				continue
			}
			_ = c.Close()
		}
	})
}

// TestStance_HeartbeatVerification asserts F3 end to end: the client's tracker
// discards a heartbeat with a bad Agent-Sender header under the strict profile
// and keeps it under relaxed, and keeps a verified heartbeat and an unsigned one
// under both.
func TestStance_HeartbeatVerification(t *testing.T) {
	srv := startNkeyServer(t)
	hnc, cnc := connectAlice(t, srv), connectAlice(t, srv)
	signer := aliceSigner(t)
	good, forged, bare := newHost(t, hnc), newHost(t, hnc), newHost(t, hnc)

	publish := func(h *host, tamper bool, sign bool) {
		payload, err := agents.EncodeHeartbeat(h.heartbeat())
		if err != nil {
			t.Fatal(err)
		}
		subject := "agents.hb." + h.agent + "." + h.owner + "." + h.name
		msg := &nats.Msg{Subject: subject, Data: payload}
		if sign {
			sh, err := identity.SignHeader(identity.SignOptions{Signer: signer, ID: aliceID(t), Subject: subject, Payload: payload})
			if err != nil {
				t.Fatal(err)
			}
			msg.Header = nats.Header{}
			sh.Set(msg.Header)
			if tamper {
				msg.Data = append(append([]byte(nil), payload...), ' ')
			}
		}
		if err := hnc.PublishMsg(msg); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name       string
		stance     identity.Stance
		forgedKept bool
	}{
		{"relaxed", identity.Stance{Profile: identity.Relaxed}, true},
		{"strict", identity.Stance{Profile: identity.Strict}, false},
		{"strict, heartbeats off", identity.Stance{Profile: identity.Strict, HeartbeatVerification: identity.Off}, true},
		{"relaxed, heartbeats on", identity.Stance{Profile: identity.Relaxed, HeartbeatVerification: identity.On}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, cnc, caller.WithStance(tc.stance), withSigner(t, ""))
			tr := c.Tracker()
			if err := tr.Start(ctxT(t)); err != nil {
				t.Fatal(err)
			}
			publish(forged, true, true)
			publish(good, false, true)
			publish(bare, false, false)
			// Heartbeats are handled in order: once the last one published is
			// accounted for, every earlier one has been judged.
			eventually(t, "the heartbeats to be handled", func() bool { _, ok := tr.Liveness(bare.id); return ok })
			if _, ok := tr.Liveness(good.id); !ok {
				t.Error("a verified heartbeat was discarded")
			}
			if _, ok := tr.Liveness(forged.id); ok != tc.forgedKept {
				t.Errorf("the forged heartbeat kept = %v, want %v", ok, tc.forgedKept)
			}
			if !tc.forgedKept && tr.Dropped() != 1 {
				t.Errorf("dropped %d, want 1", tr.Dropped())
			}
		})
	}
}
