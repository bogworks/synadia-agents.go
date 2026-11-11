// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/bogworks/synadia-agents.go/identity"
)

// TestTrustPolicy_StatusSubjectBound asserts that a replayed genuine signed
// record whose status subject (or instance id) was substituted is dropped under
// a trust policy and counted, and that without a policy it is unchanged.
func TestTrustPolicy_StatusSubjectBound(t *testing.T) {
	srv := startNkeyServer(t)
	hnc, cnc := connectAlice(t, srv), connectAlice(t, srv)
	id := aliceID(t)
	signed := func(name string, mod func(*host)) *host {
		return newHost(t, hnc, func(h *host) {
			h.agent, h.owner, h.name = "real", "tester", name
			reg, err := identity.RegistrationMetadata(aliceSigner(t), id, "real", "tester", "agents.prompt.real.tester."+name)
			if err != nil {
				t.Fatal(err)
			}
			h.extraMeta = reg
			if mod != nil {
				mod(h)
			}
		}).start()
	}
	good := signed("good", nil)
	signed("swapped", func(h *host) { h.statusSubject = "agents.status.real.tester.evil" })
	signed("foreign", func(h *host) { h.statusSubject = "evil.status" })
	signed("badid", func(h *host) { h.id = "BAD.ID" })

	ctx := ctxT(t)
	c := newClient(t, cnc, caller.WithTrustPolicy(caller.TrustPolicy{Agents: []identity.AgentID{id}}))
	got, err := c.Discover(ctx, trustWin)
	if err != nil || len(got) != 1 || got[0].InstanceID() != good.id {
		t.Fatalf("%v %v", hostNames(got), err)
	}
	if n := c.Stats().RejectedRecords; n != 3 {
		t.Fatalf("RejectedRecords %d, want 3", n)
	}
	if _, err := c.Find(ctx, caller.Filter{Name: "swapped"}, trustWin); !errors.Is(err, caller.ErrAgentNotFound) {
		t.Fatalf("swapped status subject was returned: %v", err)
	}

	plain := newClient(t, cnc)
	all, err := plain.Discover(ctx, trustWin)
	if err != nil || len(all) < 3 {
		t.Fatalf("no trust policy: %v %v", hostNames(all), err)
	}
	if n := plain.Stats().RejectedRecords; n != 0 {
		t.Fatalf("no trust policy rejected %d", n)
	}
}

// TestFind_DoesNotMutateOptions asserts Find leaves the caller's variadic
// slice alone: concurrent calls sharing one slice with spare capacity are
// race-free (run with -race) and the slice is unchanged.
func TestFind_DoesNotMutateOptions(t *testing.T) {
	srv := startServer(t)
	nc := connect(t, srv)
	newHost(t, nc, func(h *host) { h.name = "solo" }).start()
	c := newClient(t, nc)
	opts := make([]caller.DiscoverOption, 1, 8)
	opts[0] = trustWin
	spare := opts[:2]
	ctx := ctxT(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Find(ctx, caller.Filter{Name: "solo"}, opts...); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if spare[1] != nil {
		t.Fatal("Find wrote into the caller's slice")
	}
}
