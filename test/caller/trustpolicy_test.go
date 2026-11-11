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
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

// trustFixture hosts four records: trusted (signed by alice), mallory (validly
// signed by another key), plain (unsigned) and redirected (signed by alice for
// another subject).
type trustFixture struct {
	srv                     *server.Server
	hnc, cnc                *nats.Conn
	trusted, mallory, plain *host
	aliceID, malloryID      identity.AgentID
}

func newTrustFixture(t *testing.T) *trustFixture {
	t.Helper()
	f := &trustFixture{srv: startNkeyServer(t)}
	f.hnc, f.cnc = connectAlice(t, f.srv), connectAlice(t, f.srv)
	f.aliceID = aliceID(t)
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := kp.Seed()
	pub, _ := kp.PublicKey()
	msigner, err := identity.SignerFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	if f.malloryID, err = identity.NewAgentID("$G", pub); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, signer identity.Signer, id identity.AgentID, signedSubject string) *host {
		return newHost(t, f.hnc, func(h *host) {
			h.agent, h.owner, h.name = "real", "tester", name
			if signer != nil {
				reg, err := identity.RegistrationMetadata(signer, id, "real", "tester", signedSubject)
				if err != nil {
					t.Fatal(err)
				}
				h.extraMeta = reg
			}
		}).start()
	}
	f.trusted = mk("trusted", aliceSigner(t), f.aliceID, "agents.prompt.real.tester.trusted")
	f.mallory = mk("mallory", msigner, f.malloryID, "agents.prompt.real.tester.mallory")
	f.plain = mk("plain", nil, "", "")
	mk("redirected", aliceSigner(t), f.aliceID, "agents.prompt.real.tester.elsewhere")
	return f
}

var trustWin = caller.WithDiscoverTimeout(400 * time.Millisecond)

// TestTrustPolicy_DefaultsVerifiedDiscovery asserts that a client with a trust
// policy returns only the record that verifies and whose signer is trusted,
// from Discover, Find, ListAgents and LookupInstance, counting the rest; and
// that a client without one is unchanged.
func TestTrustPolicy_DefaultsVerifiedDiscovery(t *testing.T) {
	f := newTrustFixture(t)
	ctx := ctxT(t)
	policy := caller.TrustPolicy{Agents: []identity.AgentID{f.aliceID}}

	t.Run("Discover", func(t *testing.T) {
		c := newClient(t, f.cnc, caller.WithTrustPolicy(policy))
		got, err := c.Discover(ctx, trustWin)
		if err != nil || len(got) != 1 || got[0].InstanceID() != f.trusted.id {
			t.Fatalf("%v %v", hostNames(got), err)
		}
		if n := c.Stats().RejectedRecords; n != 3 {
			t.Fatalf("RejectedRecords %d, want 3", n)
		}
	})
	t.Run("ListAgents", func(t *testing.T) {
		c := newClient(t, f.cnc, caller.WithTrustPolicy(policy))
		got, err := c.ListAgents(ctx, trustWin)
		if err != nil || len(got) != 1 || got[0].InstanceID != f.trusted.id {
			t.Fatalf("%v %v", got, err)
		}
	})
	t.Run("Find", func(t *testing.T) {
		c := newClient(t, f.cnc, caller.WithTrustPolicy(policy))
		if a, err := c.Find(ctx, caller.Filter{Owner: "tester"}, trustWin); err != nil || a.InstanceID() != f.trusted.id {
			t.Fatalf("trusted: %v", err)
		}
		for _, name := range []string{"plain", "mallory"} {
			if _, err := c.Find(ctx, caller.Filter{Name: name}, trustWin); !errors.Is(err, caller.ErrAgentNotFound) {
				t.Fatalf("%s: %v", name, err)
			}
		}
	})
	t.Run("LookupInstance", func(t *testing.T) {
		c := newClient(t, f.cnc, caller.WithTrustPolicy(policy))
		if a, err := c.LookupInstance(ctx, f.trusted.id, 500*time.Millisecond); err != nil || a.InstanceID() != f.trusted.id {
			t.Fatalf("trusted: %v", err)
		}
		for _, h := range []*host{f.plain, f.mallory} {
			if _, err := c.LookupInstance(ctx, h.id, 500*time.Millisecond); !errors.Is(err, caller.ErrAgentNotFound) {
				t.Fatalf("%s: %v", h.name, err)
			}
		}
		if n := c.Stats().RejectedRecords; n != 2 {
			t.Fatalf("RejectedRecords %d, want 2", n)
		}
	})
	t.Run("NoPolicyUnchanged", func(t *testing.T) {
		c := newClient(t, f.cnc)
		got, err := c.Discover(ctx, trustWin)
		if err != nil || len(got) != 4 {
			t.Fatalf("%v %v", hostNames(got), err)
		}
		if _, err := c.LookupInstance(ctx, f.plain.id, 500*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if n := c.Stats().RejectedRecords; n != 0 {
			t.Fatalf("RejectedRecords %d", n)
		}
	})
}

// TestTrustPolicy_PredicateAndNarrowing asserts the Accept predicate (given the
// record), that Agents and Accept both apply, that a per-call option narrows
// but cannot widen past the policy, and that an empty policy is refused.
func TestTrustPolicy_PredicateAndNarrowing(t *testing.T) {
	f := newTrustFixture(t)
	ctx := ctxT(t)

	onlyMallory := caller.TrustPolicy{Accept: func(id identity.AgentID, i agents.AgentInfo) bool {
		return id == f.malloryID && i.Name == "mallory"
	}}
	c := newClient(t, f.cnc, caller.WithTrustPolicy(onlyMallory))
	if got, err := c.Discover(ctx, trustWin); err != nil || len(got) != 1 || got[0].InstanceID() != f.mallory.id {
		t.Fatalf("predicate: %v %v", hostNames(got), err)
	}

	both := caller.TrustPolicy{Agents: []identity.AgentID{f.aliceID}, Accept: func(identity.AgentID, agents.AgentInfo) bool { return false }}
	if got, err := newClient(t, f.cnc, caller.WithTrustPolicy(both)).Discover(ctx, trustWin); err != nil || len(got) != 0 {
		t.Fatalf("both must hold: %v %v", hostNames(got), err)
	}

	anyVerified := caller.TrustPolicy{Accept: func(identity.AgentID, agents.AgentInfo) bool { return true }}
	c2 := newClient(t, f.cnc, caller.WithTrustPolicy(anyVerified))
	if got, err := c2.Discover(ctx, trustWin); err != nil || len(got) != 2 {
		t.Fatalf("any verified: %v %v", hostNames(got), err)
	}
	if got, err := c2.Discover(ctx, trustWin, caller.WithRequireVerifiedIdentity(f.aliceID)); err != nil || len(got) != 1 || got[0].InstanceID() != f.trusted.id {
		t.Fatalf("narrowed: %v %v", hostNames(got), err)
	}
	c3 := newClient(t, f.cnc, caller.WithTrustPolicy(caller.TrustPolicy{Agents: []identity.AgentID{f.aliceID}}))
	if got, err := c3.Discover(ctx, trustWin, caller.WithRequireVerifiedIdentityFunc(nil)); err != nil || len(got) != 1 {
		t.Fatalf("per-call widened the policy: %v %v", hostNames(got), err)
	}

	if _, err := caller.New(f.cnc, caller.WithTrustPolicy(caller.TrustPolicy{})); err == nil {
		t.Fatal("an empty policy was accepted")
	}
}

// TestTrustPolicy_AdmittedAgentCanBePrompted asserts an agent a policy client
// discovered is not refused by Prompt or Status, and that ErrUntrustedAgent is
// a local refusal.
func TestTrustPolicy_AdmittedAgentCanBePrompted(t *testing.T) {
	f := newTrustFixture(t)
	ctx := ctxT(t)
	c := newClient(t, f.cnc, caller.WithTrustPolicy(caller.TrustPolicy{Agents: []identity.AgentID{f.aliceID}}))
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
	if !errors.Is(caller.ErrUntrustedAgent, agents.ErrLocal) {
		t.Fatal("ErrUntrustedAgent must match agents.ErrLocal")
	}
}
