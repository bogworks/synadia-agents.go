// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/nats-io/nats.go"
)

func newClient(t *testing.T, nc *nats.Conn, opts ...caller.Option) *caller.Client {
	t.Helper()
	c, err := caller.New(nc, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestDiscover_ReadsRecordsAndKeepsUnknowns asserts that a caller discovers
// agents through $SRV.INFO.agents, drops services that are not agents, ignores
// unknown endpoints and metadata keys while keeping them in the record, and
// reads the instance name from the fifth token of the prompt subject.
//
// catalog: SAP-12-01 SAP-3.2-11 SAP-12-12
// catalog: SAP-12-10
// catalog: SAP-12-11
// catalog: SAP-4.3-01
// catalog: SAP-12-03
// catalog: SAP-3.2-09
func TestDiscover_ReadsRecordsAndKeepsUnknowns(t *testing.T) {
	srv := startServer(t)
	hnc, cnc := connect(t, srv), connect(t, srv)
	a := newHost(t, hnc, func(h *host) {
		h.extraMeta = map[string]string{"x-custom": "kept"}
		h.extraEndpoint = true
	}).start()
	b := newHost(t, hnc, func(h *host) { h.agent, h.owner = "other-agent", "someone" }).start()

	// A micro service that is not an agent also answers $SRV.INFO.agents here.
	if _, err := hnc.Subscribe("$SRV.INFO.agents", func(m *nats.Msg) {
		body, _ := json.Marshal(agents.ServiceInfo{Name: "agents", ID: "NOTANAGENT", Endpoints: []agents.EndpointInfo{{Name: "x", Subject: "x"}}})
		_ = m.Respond(body)
	}); err != nil {
		t.Fatal(err)
	}
	_ = hnc.Flush()

	sn := newSniffer(t, cnc)
	c := newClient(t, cnc)
	found, err := c.Discover(ctxT(t), caller.WithDiscoverTimeout(300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("found %d agents, want 2 (the non-agent record is dropped)", len(found))
	}
	byID := map[string]*caller.Agent{}
	for _, f := range found {
		byID[f.InstanceID()] = f
	}
	fa, fb := byID[a.id], byID[b.id]
	if fa == nil || fb == nil {
		t.Fatalf("missing agents: %v", byID)
	}
	info := fa.Info()
	if info.Metadata["x-custom"] != "kept" {
		t.Fatalf("an unknown metadata key was not kept: %v", info.Metadata)
	}
	if _, ok := info.Endpoint("x-extra"); !ok || len(info.Endpoints) != 3 {
		t.Fatalf("an unknown endpoint was not kept: %+v", info.Endpoints)
	}
	if info.Prompt.Name != "prompt" || fa.PromptSubject() != a.promptSubject || info.Name != a.name {
		t.Fatalf("prompt endpoint or name not read from the record: %+v", info)
	}

	// Only the two discovery subjects were used: no per-agent construction.
	for _, s := range sn.seen() {
		if s == agents.SubjectInfoAll || strings.HasPrefix(s, "_INBOX.") || s == agents.SubjectPingAll {
			continue
		}
		t.Fatalf("the caller published or received an unexpected subject %q during discovery", s)
	}
	if len(sn.on(agents.SubjectInfoAll)) != 1 {
		t.Fatalf("expected one $SRV.INFO.agents request, got %d", len(sn.on(agents.SubjectInfoAll)))
	}

	if fa.Client() != c || fa.MinSenderTrust() != "any" {
		t.Fatalf("handle accessors: %v %q", fa.Client(), fa.MinSenderTrust())
	}
	// Mutating the copy does not touch the handle.
	info.Metadata["x-custom"] = "changed"
	if fa.Info().Metadata["x-custom"] != "kept" {
		t.Fatal("Info returned shared state")
	}
}

// TestDiscover_NoResponders asserts that a discovery with nobody listening is
// an empty list at once, not an error and not a wait for the ceiling.
func TestDiscover_NoResponders(t *testing.T) {
	srv := startServer(t)
	c := newClient(t, connect(t, srv))
	start := time.Now()
	found, err := c.Discover(ctxT(t))
	if err != nil || len(found) != 0 {
		t.Fatalf("got %v, %v", found, err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("no responders took %s", time.Since(start))
	}
}

// TestDiscover_StartsTrackerBeforeThePing asserts that the caller subscribes to
// the heartbeat wildcard, and the subscription is in place at the server,
// before it sends its first discovery request: a host that heartbeats the
// instant it sees the request is never missed.
//
// catalog: SAP-8.5-01
// catalog: SAP-12-05
func TestDiscover_StartsTrackerBeforeThePing(t *testing.T) {
	srv := startServer(t)
	hnc, cnc := connect(t, srv), connect(t, srv)
	h := newHost(t, hnc).start()
	// On every discovery request the host heartbeats at once.
	if _, err := hnc.Subscribe("$SRV.INFO.agents", func(m *nats.Msg) { h.publishHeartbeat(nil) }); err != nil {
		t.Fatal(err)
	}
	_ = hnc.Flush()

	c := newClient(t, cnc)
	if c.Tracker().Started() {
		t.Fatal("the tracker started before any discovery")
	}
	if _, err := c.Discover(ctxT(t), caller.WithDiscoverTimeout(300*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the heartbeat sent at discovery time", func() bool {
		_, ok := c.Tracker().Liveness(h.id)
		return ok
	})
	if c.Tracker().Subject() != agents.SubjectHeartbeatAll {
		t.Fatalf("tracker subject %q", c.Tracker().Subject())
	}
}

// TestTracker_ListensWithoutLookup asserts that a caller can listen on
// agents.hb.*.*.* with no lookup first and sees each instance by the
// instance_id in the payload.
//
// catalog: SAP-2-14
func TestTracker_ListensWithoutLookup(t *testing.T) {
	srv := startServer(t)
	hnc, cnc := connect(t, srv), connect(t, srv)
	a, b := newHost(t, hnc), newHost(t, hnc)
	c := newClient(t, cnc)
	got := make(chan caller.Beat, 4)
	cancel := c.Tracker().OnAnyHeartbeat(func(b caller.Beat) { got <- b })
	defer cancel()
	if err := c.StartTracking(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	a.publishHeartbeat(func(m *nats.Msg) { m.Header = nats.Header{"X-Seen": {"yes"}} })
	b.publishHeartbeat(nil)
	seen := map[string]caller.Beat{}
	for len(seen) < 2 {
		select {
		case beat := <-got:
			seen[beat.Heartbeat.InstanceID] = beat
		case <-time.After(5 * time.Second):
			t.Fatal("heartbeats not received")
		}
	}
	if seen[a.id].Name != a.name || seen[a.id].Header().Get("X-Seen") != "yes" {
		t.Fatalf("name or header access: %+v", seen[a.id])
	}
	for _, id := range []string{a.id, b.id} {
		if l, ok := c.Tracker().Liveness(id); !ok || !l.Online || l.Interval != 30*time.Second {
			t.Fatalf("%s: %+v %v", id, l, ok)
		}
	}
}

// TestPing_ByInstanceAndAll asserts the on-demand reachability check of SAP
// §8.4 and its correlation with heartbeats by instance_id, and that a status
// reply is fed to the tracker as a heartbeat.
//
// catalog: SAP-8.4-01
// catalog: SAP-8.4-02
// catalog: SAP-8.7-08
func TestPing_ByInstanceAndAll(t *testing.T) {
	srv := startServer(t)
	hnc, cnc := connect(t, srv), connect(t, srv)
	a := newHost(t, hnc).start()
	b := newHost(t, hnc).start()
	c := newClient(t, cnc)
	ctx := ctxT(t)

	ok, err := c.Ping(ctx, a.id)
	if err != nil || !ok {
		t.Fatalf("ping a known instance: %v %v", ok, err)
	}
	ok, err = c.Ping(ctx, "NOSUCHINSTANCE")
	if err != nil || ok {
		t.Fatalf("ping an unknown instance: %v %v", ok, err)
	}
	pongs, err := c.PingAll(ctx, caller.WithDiscoverTimeout(300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, p := range pongs {
		ids = append(ids, p.ID)
	}
	slices.Sort(ids)
	want := []string{a.id, b.id}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatalf("PingAll ids %v, want %v", ids, want)
	}

	// Ping answers correlate with heartbeats by instance_id.
	a.publishHeartbeat(nil)
	eventually(t, "heartbeat", func() bool { _, ok := c.Tracker().Liveness(a.id); return ok })

	// A status reply is a just-arrived heartbeat.
	infos, err := c.ListAgents(ctx, caller.WithDiscoverTimeout(300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	var target *agents.AgentInfo
	for i := range infos {
		if infos[i].InstanceID == b.id {
			target = &infos[i]
		}
	}
	if target == nil {
		t.Fatal("instance b not listed")
	}
	if _, seen := c.Tracker().Liveness(b.id); seen {
		t.Fatal("b was already tracked")
	}
	ag, err := c.LookupInstance(ctx, b.id)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := ag.Status(ctx)
	if err != nil || hb.InstanceID != b.id {
		t.Fatalf("status: %+v %v", hb, err)
	}
	if l, seen := c.Tracker().Liveness(b.id); !seen || !l.Online {
		t.Fatalf("the status reply was not fed to the tracker: %+v %v", l, seen)
	}
}

// TestFind_NarrowsBroadDiscovery asserts that an identity tuple without an
// instance id discovers broadly and narrows client-side, and that a direct
// lookup by instance id works and fails cleanly for an unknown one.
//
// catalog: SAP-4.2-01
func TestFind_NarrowsBroadDiscovery(t *testing.T) {
	srv := startServer(t)
	hnc, cnc := connect(t, srv), connect(t, srv)
	a := newHost(t, hnc, func(h *host) { h.agent, h.owner, h.name = "alpha", "o1", "main" }).start()
	newHost(t, hnc, func(h *host) { h.agent, h.owner, h.name = "alpha", "o2", "main" }).start()
	newHost(t, hnc, func(h *host) { h.agent, h.owner, h.name = "beta", "o1", "main" }).start()
	c := newClient(t, cnc)
	ctx := ctxT(t)

	got, err := c.Find(ctx, caller.Filter{Agent: "alpha", Owner: "o1", Name: "main"}, caller.WithDiscoverTimeout(300*time.Millisecond))
	if err != nil || got.InstanceID() != a.id {
		t.Fatalf("Find: %v %v", got, err)
	}
	if _, err := c.Find(ctx, caller.Filter{Agent: "gamma"}, caller.WithDiscoverTimeout(200*time.Millisecond)); !errors.Is(err, caller.ErrAgentNotFound) {
		t.Fatalf("a missing agent: %v", err)
	}
	if _, err := c.LookupInstance(ctx, "NOSUCHINSTANCE"); !errors.Is(err, caller.ErrAgentNotFound) {
		t.Fatalf("lookup of an unknown instance: %v", err)
	}
	if _, err := c.LookupInstance(ctx, "bad.token"); !errors.Is(err, caller.ErrAgentNotFound) {
		t.Fatalf("lookup with an invalid id: %v", err)
	}
	all, err := c.Discover(ctx, caller.WithDiscoverTimeout(300*time.Millisecond), caller.WithFilter(caller.Filter{Agent: "alpha"}))
	if err != nil || len(all) != 2 {
		t.Fatalf("filtered discovery: %d %v", len(all), err)
	}
}

// TestPrompt_UsesTheDiscoveredSubject asserts that a prompt goes to the subject
// in the discovery record, verbatim, as one message, and that the caller builds
// no subject from identity: nothing is published on the default-pattern subject
// of an agent whose record names a different one.
//
// catalog: SAP-2-12
// catalog: SAP-2-13
// catalog: SAP-4-03
// catalog: SAP-5-01
func TestPrompt_UsesTheDiscoveredSubject(t *testing.T) {
	srv := startServer(t)
	hnc, cnc := connect(t, srv), connect(t, srv)
	h := newHost(t, hnc, func(h *host) { h.promptSubject = "custom.place.for.prompts" }).start()
	defaultSubject := "agents.prompt." + h.agent + "." + h.owner + "." + h.name
	sn := newSniffer(t, cnc)
	c := newClient(t, cnc)
	ctx := ctxT(t)
	found, err := c.Discover(ctx, caller.WithDiscoverTimeout(300*time.Millisecond))
	if err != nil || len(found) != 1 {
		t.Fatal(found, err)
	}
	st, err := found[0].PromptText(ctx, "hi")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, err := range st.Events() {
		if err != nil {
			t.Fatal(err)
		}
	}
	if st.Subject() != "custom.place.for.prompts" {
		t.Fatalf("stream subject %q", st.Subject())
	}
	if n := len(sn.on("custom.place.for.prompts")); n != 1 {
		t.Fatalf("%d messages on the discovered subject, want exactly 1", n)
	}
	if n := len(sn.on(defaultSubject)); n != 0 {
		t.Fatalf("%d messages on a subject built from identity", n)
	}
}
