// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host_test

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
)

// TestRegistration_ServiceRecord reads the instance's $SRV.INFO record.
//
// catalog: SAP-3-01 SAP-3.1-01 SAP-3.1-02 SAP-3.1-03 SAP-3.1-04 SAP-3.2-01
// catalog: SAP-3.2-02 SAP-3.2-03
func TestRegistration_ServiceRecord(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, echo, host.WithVersion("1.2.3"), host.WithDescription("a test agent"))
	si := info(t, nc, h.InstanceID())
	if si.Name != "agents" {
		t.Errorf("service name %q", si.Name)
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+`).MatchString(si.Version) || si.Version != "1.2.3" {
		t.Errorf("version %q", si.Version)
	}
	if si.Description != "a test agent" {
		t.Errorf("description %q", si.Description)
	}
	if si.Metadata == nil {
		t.Fatal("metadata is not an object")
	}
	if si.Metadata["agent"] != testAgent || si.Metadata["owner"] != testOwner {
		t.Errorf("metadata %v", si.Metadata)
	}
	if si.Metadata["protocol_version"] != "0.3" {
		t.Errorf("protocol_version %q", si.Metadata["protocol_version"])
	}
	if si.ID != h.InstanceID() || h.InstanceID() == "" {
		t.Errorf("id %q vs %q", si.ID, h.InstanceID())
	}
	ai, err := agents.NewAgentInfo(si)
	if err != nil {
		t.Fatalf("the record is not an agent: %v", err)
	}
	if ai.Name != testName {
		t.Errorf("instance name %q", ai.Name)
	}
}

// TestRegistration_DefaultDescription: the description defaults to
// "<agent> agent <name>" (SAP §3.1).
//
// catalog: SAP-3.1-03
func TestRegistration_DefaultDescription(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, echo)
	if got := info(t, nc, h.InstanceID()).Description; got != testAgent+" agent "+testName {
		t.Fatalf("description %q", got)
	}
}

// TestRegistration_Endpoints checks the endpoint names, subjects and queue
// groups of the record.
//
// catalog: SAP-2-04 SAP-2-05 SAP-2-07 SAP-2-09 SAP-2-10 SAP-3.3-01 SAP-3.3-02
func TestRegistration_Endpoints(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, echo)
	si := info(t, nc, h.InstanceID())
	for _, c := range []struct{ name, subject string }{
		{"prompt", "agents.prompt." + testAgent + "." + testOwner + "." + testName},
		{"status", "agents.status." + testAgent + "." + testOwner + "." + testName},
	} {
		e := endpointOf(t, si, c.name)
		if e.Subject != c.subject {
			t.Errorf("%s subject %q, want %q", c.name, e.Subject, c.subject)
		}
		if e.QueueGroup != "agents" {
			t.Errorf("%s queue group %q, want agents (set explicitly)", c.name, e.QueueGroup)
		}
		if verb, _, err := agents.ParseSubject(e.Subject); err != nil || string(verb) != e.Name {
			t.Errorf("endpoint %q is not named for its verb %q (%v)", e.Name, verb, err)
		}
	}
}

// TestRegistration_PromptMetadata checks the prompt endpoint's declaration, and
// that the status endpoint declares nothing.
//
// catalog: SAP-2.1-01 SAP-2.1-02 SAP-2.1-04 SAP-13.9-01 SAP-13.9-02 SAP-13.9-05 SAP-13.13-03
func TestRegistration_PromptMetadata(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, echo, host.WithMaxPayload(512<<10), host.WithAttachmentsOK(false))
	si := info(t, nc, h.InstanceID())
	p := endpointOf(t, si, "prompt")
	lim, err := p.PromptLimits()
	if err != nil {
		t.Fatal(err)
	}
	if lim.MaxPayload != 512<<10 || p.Metadata["max_payload"] != "512KB" {
		t.Errorf("max_payload %q (%d)", p.Metadata["max_payload"], lim.MaxPayload)
	}
	if lim.AttachmentsOK == nil || *lim.AttachmentsOK || p.Metadata["attachments_ok"] != "false" {
		t.Errorf("attachments_ok %q", p.Metadata["attachments_ok"])
	}
	if v, ok := p.Metadata["min_sender_trust"]; !ok || (v != "any" && v != "signed") {
		t.Errorf("min_sender_trust %q declared=%v", v, ok)
	}
	if got, _ := p.MinSenderTrust(); got != agents.TrustAny {
		t.Errorf("default trust %q", got)
	}
	if _, has := endpointOf(t, si, "status").Metadata["min_sender_trust"]; has {
		t.Error("status declares min_sender_trust")
	}
}

// TestRegistration_MaxPayloadDefaultsToConnection: without an option the
// advertised max_payload is the connection's, and a larger request is clamped
// down to it (SAP §2.1; TS default and clamp).
func TestRegistration_MaxPayloadDefaultsToConnection(t *testing.T) {
	_, nc := setup(t)
	want := agents.FormatSize(nc.MaxPayload())
	h := newHost(t, nc, echo)
	if got := h.PromptMetadata()["max_payload"]; got != want {
		t.Errorf("default max_payload %q, want %q", got, want)
	}
	h2 := newHostNamed(t, nc, "t2", echo, host.WithMaxPayload(nc.MaxPayload()*4))
	if got := h2.PromptMetadata()["max_payload"]; got != want {
		t.Errorf("clamped max_payload %q, want %q", got, want)
	}
}

// TestRegistration_SessionAndExtraMetadata: session only when set, extra
// metadata merged with the required keys winning, an abbreviated subject token.
//
// catalog: SAP-3.2-04 SAP-3.2-05 SAP-3.2-06 SAP-3.2-07 SAP-3.2-08 SAP-3.2-10 SAP-2-02 SAP-2-03
func TestRegistration_SessionAndExtraMetadata(t *testing.T) {
	_, nc := setup(t)
	plain := newHost(t, nc, echo)
	if _, has := info(t, nc, plain.InstanceID()).Metadata["session"]; has {
		t.Error("session advertised on a session-less host")
	}

	subj, err := agents.NewAgentSubject("claude-code", testOwner, "work", agents.WithSubjectToken("cc"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := host.New(nc, subj, echo, host.WithoutKeepalive(), host.WithHeartbeatInterval(time.Hour),
		host.WithSession("s-42"),
		host.WithExtraMetadata(map[string]string{"build": "abc", "agent": "forged", "owner": "forged", "protocol_version": "9.9", "id_sig": "x", "user_nkey": "x", "account": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	md := info(t, nc, h.InstanceID()).Metadata
	want := map[string]string{"agent": "claude-code", "owner": testOwner, "session": "s-42", "protocol_version": "0.3", "build": "abc"}
	for k, v := range want {
		if md[k] != v {
			t.Errorf("metadata[%s] = %q, want %q", k, md[k], v)
		}
	}
	for _, k := range []string{"id_sig", "user_nkey", "account"} {
		if _, has := md[k]; has {
			t.Errorf("metadata carries %s without a registered identity", k)
		}
	}
	// The wire token is the abbreviation, metadata.agent the canonical name.
	if p := endpointOf(t, info(t, nc, h.InstanceID()), "prompt").Subject; p != "agents.prompt.cc."+testOwner+".work" {
		t.Errorf("prompt subject %q", p)
	}
	if md["owner"] != subj.Owner() {
		t.Error("metadata.owner is not the owner token")
	}
}

// TestInstances_LoadBalanced: instances of one logical agent register at once,
// share endpoint subjects and metadata, differ in service id, and the queue
// group load-balances prompts across them.
//
// catalog: SAP-3.4-01 SAP-3.4-02 SAP-3.4-04
func TestInstances_LoadBalanced(t *testing.T) {
	_, nc := setup(t)
	var mu sync.Mutex
	served := map[string]int{}
	mk := func(tag string) *host.Host {
		return newHost(t, nc, func(ctx context.Context, _ *host.Request, w *host.Responder) error {
			mu.Lock()
			served[tag]++
			mu.Unlock()
			return w.Send(ctx, tag)
		})
	}
	a, b := mk("a"), mk("b")
	if a.InstanceID() == b.InstanceID() {
		t.Fatal("instances share a service id")
	}
	ia, ib := info(t, nc, a.InstanceID()), info(t, nc, b.InstanceID())
	for _, name := range []string{"prompt", "status"} {
		ea, eb := endpointOf(t, ia, name), endpointOf(t, ib, name)
		if ea.Subject != eb.Subject || ea.QueueGroup != eb.QueueGroup {
			t.Errorf("%s differs: %+v vs %+v", name, ea, eb)
		}
	}
	if ia.Metadata["agent"] != ib.Metadata["agent"] || ia.Metadata["owner"] != ib.Metadata["owner"] {
		t.Error("metadata differs")
	}
	for i := 0; i < 60; i++ {
		prompt(t, nc, a, "x")
	}
	mu.Lock()
	defer mu.Unlock()
	if served["a"] == 0 || served["b"] == 0 {
		t.Fatalf("queue group did not balance: %v", served)
	}
}

// TestDiscovery_PingAndInfo: every instance answers $SRV.PING.agents once and
// $SRV.INFO.agents with its full record.
//
// catalog: SAP-4-01 SAP-4-02 SAP-4.1-01
func TestDiscovery_PingAndInfo(t *testing.T) {
	_, nc := setup(t)
	a := newHost(t, nc, echo)
	b := newHostNamed(t, nc, "t2", echo)

	collectReplies := func(subject string) []*nats.Msg {
		sub, err := nc.SubscribeSync(nats.NewInbox())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = sub.Unsubscribe() }()
		if err := nc.Flush(); err != nil {
			t.Fatal(err)
		}
		if err := nc.PublishRequest(subject, sub.Subject, nil); err != nil {
			t.Fatal(err)
		}
		var out []*nats.Msg
		for {
			m, err := sub.NextMsg(400 * time.Millisecond)
			if err != nil {
				return out
			}
			out = append(out, m)
		}
	}
	ids := map[string]int{}
	for _, m := range collectReplies(agents.SubjectPingAll) {
		p, err := agents.DecodeServicePing(m.Data)
		if err != nil {
			t.Fatal(err)
		}
		ids[p.ID]++
	}
	if ids[a.InstanceID()] != 1 || ids[b.InstanceID()] != 1 || len(ids) != 2 {
		t.Errorf("ping answers: %v", ids)
	}
	got := map[string]agents.ServiceInfo{}
	for _, m := range collectReplies(agents.SubjectInfoAll) {
		si, err := agents.DecodeServiceInfo(m.Data)
		if err != nil {
			t.Fatal(err)
		}
		got[si.ID] = si
	}
	if len(got) != 2 {
		t.Fatalf("info answers: %d", len(got))
	}
	for _, h := range []*host.Host{a, b} {
		si := got[h.InstanceID()]
		ai, err := agents.NewAgentInfo(si)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ai.PromptLimits(); err != nil {
			t.Errorf("INFO record lacks capability metadata: %v", err)
		}
		if one := info(t, nc, h.InstanceID()); one.ID != si.ID || len(one.Endpoints) != len(si.Endpoints) {
			t.Error("direct INFO differs from the general one")
		}
	}
}

// TestInstanceID_IsServiceID: Host.InstanceID is the micro service id.
func TestInstanceID_IsServiceID(t *testing.T) {
	_, nc := setup(t)
	h, err := host.New(nc, subjectOf(t, testName), echo)
	if err != nil {
		t.Fatal(err)
	}
	if h.InstanceID() != "" || h.Service() != nil {
		t.Error("instance id or service before Start")
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	if h.Service().Info().ID != h.InstanceID() {
		t.Error("service id differs")
	}
	if err := h.Start(context.Background()); err == nil {
		t.Error("second Start succeeded")
	}
	if h.Subject().Prompt() != "agents.prompt."+testAgent+"."+testOwner+"."+testName {
		t.Error("subject")
	}
}

// TestNew_RejectsInvalidConfiguration: every validation of the constructors
// happens at New, before anything registers.
func TestNew_RejectsInvalidConfiguration(t *testing.T) {
	_, nc := setup(t)
	subj := subjectOf(t, testName)
	cases := []struct {
		name string
		nc   *nats.Conn
		subj agents.AgentSubject
		h    host.PromptHandler
		opts []host.Option
	}{
		{"nil connection", nil, subj, echo, nil},
		{"zero subject", nc, agents.AgentSubject{}, echo, nil},
		{"nil handler", nc, subj, nil, nil},
		{"zero heartbeat interval", nc, subj, echo, []host.Option{host.WithHeartbeatInterval(0)}},
		{"negative heartbeat interval", nc, subj, echo, []host.Option{host.WithHeartbeatInterval(-time.Second)}},
		{"sub-second heartbeat without opt-in", nc, subj, echo, []host.Option{host.WithHeartbeatInterval(500 * time.Millisecond)}},
		{"negative keepalive", nc, subj, echo, []host.Option{host.WithKeepalive(-1)}},
		{"unknown trust", nc, subj, echo, []host.Option{host.WithMinSenderTrust("maybe")}},
		{"zero replay window", nc, subj, echo, []host.Option{host.WithReplayWindow(0)}},
		{"zero concurrency", nc, subj, echo, []host.Option{host.WithMaxConcurrentPrompts(0)}},
		{"bad busy policy", nc, subj, echo, []host.Option{host.WithBusyPolicy(9)}},
		{"bad version", nc, subj, echo, []host.Option{host.WithVersion("one")}},
		{"zero max payload", nc, subj, echo, []host.Option{host.WithMaxPayload(0)}},
		{"zero nonces", nc, subj, echo, []host.Option{host.WithMaxNonces(0)}},
		{"endpoint named prompt", nc, subj, echo, []host.Option{host.WithEndpoint(host.Endpoint{Name: "prompt", Subject: "x.y", Handler: nopHandler()})}},
		{"endpoint named status", nc, subj, echo, []host.Option{host.WithEndpoint(host.Endpoint{Name: "status", Subject: "x.y", Handler: nopHandler()})}},
		{"endpoint named hb", nc, subj, echo, []host.Option{host.WithEndpoint(host.Endpoint{Name: "hb", Subject: "x.y", Handler: nopHandler()})}},
		{"endpoint named attachments", nc, subj, echo, []host.Option{host.WithEndpoint(host.Endpoint{Name: "attachments", Subject: "x.y", Handler: nopHandler()})}},
		{"endpoint repeated", nc, subj, echo, []host.Option{
			host.WithEndpoint(host.Endpoint{Name: "a", Subject: "x.y", Handler: nopHandler()}),
			host.WithEndpoint(host.Endpoint{Name: "a", Subject: "x.z", Handler: nopHandler()})}},
		{"endpoint without subject", nc, subj, echo, []host.Option{host.WithEndpoint(host.Endpoint{Name: "a", Handler: nopHandler()})}},
		{"endpoint without handler", nc, subj, echo, []host.Option{host.WithEndpoint(host.Endpoint{Name: "a", Subject: "x.y"})}},
		{"endpoint on the prompt subject", nc, subj, echo, []host.Option{host.WithEndpoint(host.Endpoint{Name: "a", Subject: subj.Prompt(), Handler: nopHandler()})}},
		{"nil interceptor", nc, subj, echo, []host.Option{host.WithInterceptors(nil)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, err := host.New(tc.nc, tc.subj, tc.h, tc.opts...)
			if err == nil {
				t.Fatalf("New accepted it: %v", h)
			}
			if tc.nc != nil && !strings.Contains(err.Error(), "host:") {
				t.Errorf("error %q does not name the package", err)
			}
		})
	}
}

// TestNew_SubSecondHeartbeatNeedsOptIn: an interval under one second is allowed
// only with the explicit opt-in (SAP §8.2 SHOULD NOT; SDK policy).
//
// catalog: SAP-8.2-02
func TestNew_SubSecondHeartbeatNeedsOptIn(t *testing.T) {
	_, nc := setup(t)
	subj := subjectOf(t, testName)
	if _, err := host.New(nc, subj, echo, host.WithHeartbeatInterval(100*time.Millisecond)); err == nil {
		t.Fatal("a sub-second interval was accepted without the opt-in")
	}
	if _, err := host.New(nc, subj, echo, host.WithHeartbeatInterval(100*time.Millisecond), host.WithSubSecondHeartbeats()); err != nil {
		t.Fatalf("opt-in refused: %v", err)
	}
	if _, err := host.New(nc, subj, echo, host.WithHeartbeatInterval(time.Second)); err != nil {
		t.Fatalf("one second refused: %v", err)
	}
}

func nopHandler() micro.Handler { return micro.HandlerFunc(func(micro.Request) {}) }

// TestDefaults pins the documented defaults.
func TestDefaults(t *testing.T) {
	if host.DefaultMaxConcurrentPrompts != 64 {
		t.Errorf("DefaultMaxConcurrentPrompts = %d", host.DefaultMaxConcurrentPrompts)
	}
	if host.DefaultHeartbeatInterval != 30*time.Second || host.DefaultKeepalive != 30*time.Second {
		t.Error("heartbeat or keepalive default")
	}
	if !host.DefaultAttachmentsOK || host.DefaultMinSenderTrust != identity.TrustAny {
		t.Error("attachments or trust default")
	}
}
