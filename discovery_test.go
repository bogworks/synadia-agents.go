// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"errors"
	"reflect"
	"testing"
)

const infoJSON = `{
 "type":"io.nats.micro.v1.info_response","name":"agents","id":"VMKS6MHK71PCPWGY38A7N5","version":"1.4.0",
 "description":"Claude Code","metadata":{"agent":"claude-code","owner":"aconnolly","session":"synadia-com-2","protocol_version":"0.3","x-extra":"kept"},
 "endpoints":[
  {"name":"prompt","subject":"agents.prompt.cc.aconnolly.synadia-com-2","queue_group":"agents","metadata":{"max_payload":"1MB","attachments_ok":"true"}},
  {"name":"status","subject":"agents.status.cc.aconnolly.synadia-com-2","queue_group":"agents"},
  {"name":"other","subject":"x.y"}]}`

// SAP:L226-L232 (§4.3): reading a discovery record.
//
// catalog: SAP-3.2-11 SAP-4.3-02
func TestDiscovery_NewAgentInfo(t *testing.T) {
	si, err := DecodeServiceInfo([]byte(infoJSON))
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAgentInfo(si)
	if err != nil {
		t.Fatal(err)
	}
	if a.InstanceID != "VMKS6MHK71PCPWGY38A7N5" || a.Agent != "claude-code" || a.Owner != "aconnolly" ||
		a.Session != "synadia-com-2" || a.Name != "synadia-com-2" || a.ProtocolVersion != "0.3" ||
		a.Version != "1.4.0" || a.Description != "Claude Code" || a.Metadata["x-extra"] != "kept" {
		t.Fatalf("%+v", a)
	}
	if a.Prompt.Subject != "agents.prompt.cc.aconnolly.synadia-com-2" || a.Prompt.QueueGroup != QueueGroup ||
		a.Status == nil || a.Status.Subject != "agents.status.cc.aconnolly.synadia-com-2" || len(a.Endpoints) != 3 {
		t.Fatalf("%+v", a)
	}
	if n, ok := a.Prompt.MaxPayload(); !ok || n != 1<<20 {
		t.Fatal(n, ok)
	}
	if v, ok := a.Prompt.AttachmentsOK(); !ok || !v {
		t.Fatal(v, ok)
	}
	if _, ok := a.Prompt.MinSenderTrust(); ok || a.SupportsSenderIdentity() {
		t.Fatal("sender identity detected on a plain agent")
	}
	if e, ok := a.Endpoint("other"); !ok || e.Subject != "x.y" {
		t.Fatal(e, ok)
	}
	if _, ok := a.Endpoint("nope"); ok {
		t.Fatal("phantom endpoint")
	}
	if err := a.CheckVersion(VersionDefault); err != nil {
		t.Fatal(err)
	}
	// The record is copied, not aliased.
	si.Metadata["agent"] = "changed"
	if a.Agent != "claude-code" || a.Metadata["agent"] != "claude-code" {
		t.Fatal("aliased")
	}
}

// SAP:L142-L147, L164-L167 (§3.1, §3.2), SAP:L229 (§4.3): what makes a record not an agent.
func TestDiscovery_NotAnAgent(t *testing.T) {
	good := func() ServiceInfo {
		return ServiceInfo{Name: "agents", ID: "i", Metadata: map[string]string{"agent": "a", "owner": "o", "protocol_version": "0.3"},
			Endpoints: []EndpointInfo{{Name: "prompt", Subject: "agents.prompt.a.o.n"}, {Name: "status", Subject: "agents.status.a.o.n"}}}
	}
	if _, err := NewAgentInfo(good()); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*ServiceInfo){
		"service name": func(s *ServiceInfo) { s.Name = "Synadia Agents" },
		"no agent":     func(s *ServiceInfo) { delete(s.Metadata, "agent") },
		"no owner":     func(s *ServiceInfo) { delete(s.Metadata, "owner") },
		"no version":   func(s *ServiceInfo) { delete(s.Metadata, "protocol_version") },
		"nil metadata": func(s *ServiceInfo) { s.Metadata = nil },
		"no prompt":    func(s *ServiceInfo) { s.Endpoints[0].Name = "chat" },
		"no endpoints": func(s *ServiceInfo) { s.Endpoints = nil },
		"no status":    func(s *ServiceInfo) { s.Endpoints = s.Endpoints[:1] }, // SAP:L541-L549
	} {
		s := good()
		mod(&s)
		if a, err := NewAgentInfo(s); a != nil || !errors.Is(err, ErrNotAgent) {
			t.Errorf("%s: %v %v", name, a, err)
		}
	}
}

// SAP:L229-L230 (§4.3 step 3): the name is opaque when the subject is custom.
//
// catalog: SAP-4.3-02
func TestDiscovery_CustomPromptSubjectHasNoName(t *testing.T) {
	a, err := NewAgentInfo(ServiceInfo{Name: "agents", Metadata: map[string]string{"agent": "a", "owner": "o", "protocol_version": "0.3"},
		Endpoints: []EndpointInfo{{Name: "prompt", Subject: "custom.place"}, {Name: "status", Subject: "custom.status"}}})
	if err != nil || a.Name != "" || a.Prompt.Subject != "custom.place" || a.Status == nil {
		t.Fatal(a, err)
	}
}

// SAP:L171 (§3.2): unknown metadata keys are preserved. SAP:L525-L528: unknown ping/info fields are ignored.
func TestDiscovery_DecodeServiceRecords(t *testing.T) {
	if _, err := DecodeServiceInfo([]byte(`[]`)); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
	if _, err := DecodeServiceInfo([]byte(`nope`)); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
	p, err := DecodeServicePing([]byte(`{"type":"io.nats.micro.v1.ping_response","name":"agents","id":"X","version":"1","metadata":{"agent":"a"},"future":1}`))
	if err != nil || p.ID != "X" || p.Metadata["agent"] != "a" {
		t.Fatal(p, err)
	}
	if _, err := DecodeServicePing([]byte(`5`)); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
}

// SAP:L90-L92 (§2.1): both keys are mandatory; a missing or malformed one makes the endpoint unusable.
func TestDiscovery_EndpointCapabilities(t *testing.T) {
	cases := []struct {
		md      map[string]string
		wantMax int64
		wantAtt *bool
		wantKey string // "": valid; else the CapabilityError key
	}{
		{map[string]string{"max_payload": "512KB", "attachments_ok": "false"}, 512 << 10, boolp(false), ""},
		{map[string]string{"max_payload": "junk", "attachments_ok": "true"}, 0, nil, MetaMaxPayload},
		{map[string]string{"max_payload": "1MB", "attachments_ok": "yes"}, 0, nil, MetaAttachmentsOK},
		{map[string]string{"max_payload": "1MB"}, 0, nil, MetaAttachmentsOK},
		{map[string]string{"attachments_ok": "true"}, 0, nil, MetaMaxPayload},
		{map[string]string{}, 0, nil, MetaMaxPayload},
		{nil, 0, nil, MetaMaxPayload},
	}
	for _, c := range cases {
		l, err := EndpointInfo{Name: "prompt", Metadata: c.md}.PromptLimits()
		var ce *CapabilityError
		if c.wantKey != "" {
			if !errors.As(err, &ce) || ce.Key != c.wantKey || !errors.Is(err, ErrInvalidCapability) || !errors.Is(err, ErrLocal) {
				t.Errorf("%v: err %v", c.md, err)
			}
			continue
		}
		if err != nil || l.MaxPayload != c.wantMax || !reflect.DeepEqual(l.AttachmentsOK, c.wantAtt) {
			t.Errorf("%v: %+v %v", c.md, l, err)
		}
	}
}

func boolp(b bool) *bool { return &b }

// SAP:L1001 (§13.9): "any" is any; "signed" and every unknown value are signed; absent is absent.
func TestDiscovery_MinSenderTrust(t *testing.T) {
	for raw, want := range map[string]SenderTrust{"any": TrustAny, "signed": TrustSigned, "": TrustSigned, "Any": TrustSigned, "weird": TrustSigned} {
		got, ok := EndpointInfo{Metadata: map[string]string{MetaMinSenderTrust: raw}}.MinSenderTrust()
		if !ok || got != want {
			t.Errorf("%q: %v %v", raw, got, ok)
		}
	}
	if _, ok := (EndpointInfo{}).MinSenderTrust(); ok {
		t.Fatal("absent reported present")
	}
	if !(&AgentInfo{Prompt: EndpointInfo{Metadata: map[string]string{MetaMinSenderTrust: "any"}}}).SupportsSenderIdentity() {
		t.Fatal("not detected")
	}
}

// SAP:L149-L169 (§3.2), SAP:L89-L92 (§2.1): the metadata a host registers.
func TestDiscovery_HostMetadata(t *testing.T) {
	got := ServiceMetadata{Agent: "claude-code", Owner: "aconnolly", Session: "s"}.Map()
	want := map[string]string{"agent": "claude-code", "owner": "aconnolly", "session": "s", "protocol_version": "0.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	if _, ok := (ServiceMetadata{Agent: "a", Owner: "o"}).Map()["session"]; ok {
		t.Fatal("empty session written")
	}
	if (ServiceMetadata{Agent: "a", Owner: "o", ProtocolVersion: "0.4"}).Map()["protocol_version"] != "0.4" {
		t.Fatal("override")
	}
	pm := PromptEndpointMetadata{MaxPayload: 1 << 20, AttachmentsOK: true}
	if !reflect.DeepEqual(pm.Map(), map[string]string{"max_payload": "1MB", "attachments_ok": "true"}) {
		t.Fatal(pm.Map())
	}
	if l := pm.Limits(); l.MaxPayload != 1<<20 || !*l.AttachmentsOK {
		t.Fatal(l)
	}
	// What a host declares, a caller reads back unchanged.
	back, err := EndpointInfo{Metadata: PromptEndpointMetadata{MaxPayload: 3 << 10, AttachmentsOK: false}.Map()}.PromptLimits()
	if err != nil || back.MaxPayload != 3<<10 || *back.AttachmentsOK {
		t.Fatal(back)
	}
}

// SAP:L142-L145, L180, L547 (§3.1, §3.3, §8.7): fixed names.
func TestDiscovery_Constants(t *testing.T) {
	if ServiceName != "agents" || QueueGroup != "agents" || EndpointPrompt != "prompt" ||
		EndpointStatus != "status" || EndpointAttachments != "attachments" {
		t.Fatal("constants")
	}
}

// SAP:L165 (§3.2): the owner metadata matches the 4th subject token. The agent
// token is only a SHOULD (SAP:L57-L76): it is not checked here.
func TestDiscovery_CheckSubjectConsistency(t *testing.T) {
	mk := func(agent, owner, prompt, status string) *AgentInfo {
		return &AgentInfo{Agent: agent, Owner: owner,
			Prompt: EndpointInfo{Name: "prompt", Subject: prompt},
			Status: &EndpointInfo{Name: "status", Subject: status}}
	}
	for name, tc := range map[string]struct {
		a  *AgentInfo
		ok bool
	}{
		"full form":        {mk("claude-code", "alice", "agents.prompt.claude-code.alice.n", "agents.status.claude-code.alice.n"), true},
		"abbreviation":     {mk("claude-code", "alice", "agents.prompt.cc.alice.n", "agents.status.cc.alice.n"), true},
		"openclaw oc":      {mk("openclaw", "alice", "agents.prompt.oc.alice.default", "agents.status.oc.alice.default"), true},
		"custom subject":   {mk("claude-code", "alice", "my.private.subject", "my.private.status"), true},
		"owner differs":    {mk("claude-code", "alice", "agents.prompt.cc.mallory.n", "agents.status.cc.alice.n"), false},
		"agent differs":    {mk("claude-code", "alice", "agents.prompt.hermes.alice.n", "agents.status.cc.alice.n"), true},
		"status owner":     {mk("claude-code", "alice", "agents.prompt.cc.alice.n", "agents.status.cc.mallory.n"), false},
		"not subsequence":  {mk("claude-code", "alice", "agents.prompt.cx.alice.n", "agents.status.cc.alice.n"), true},
		"wrong first rune": {mk("claude-code", "alice", "agents.prompt.lc.alice.n", "agents.status.cc.alice.n"), true},
	} {
		err := tc.a.CheckSubjectConsistency()
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrRecordInconsistent)) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// SAP:L57-L76 (§2): the agent token SHOULD be an abbreviation of metadata.agent.
// CheckAgentToken reports a departure as a diagnostic; it is not a consistency
// failure.
func TestDiscovery_CheckAgentToken(t *testing.T) {
	mk := func(agent, prompt, status string) *AgentInfo {
		return &AgentInfo{Agent: agent, Owner: "alice",
			Prompt: EndpointInfo{Name: "prompt", Subject: prompt},
			Status: &EndpointInfo{Name: "status", Subject: status}}
	}
	for name, tc := range map[string]struct {
		a  *AgentInfo
		ok bool
	}{
		"full form":        {mk("claude-code", "agents.prompt.claude-code.alice.n", "agents.status.claude-code.alice.n"), true},
		"abbreviation":     {mk("claude-code", "agents.prompt.cc.alice.n", "agents.status.cc.alice.n"), true},
		"custom subject":   {mk("claude-code", "my.private.subject", "my.private.status"), true},
		"agent differs":    {mk("claude-code", "agents.prompt.hermes.alice.n", "agents.status.cc.alice.n"), false},
		"status differs":   {mk("claude-code", "agents.prompt.cc.alice.n", "agents.status.hermes.alice.n"), false},
		"not subsequence":  {mk("claude-code", "agents.prompt.cx.alice.n", "agents.status.cc.alice.n"), false},
		"wrong first rune": {mk("claude-code", "agents.prompt.lc.alice.n", "agents.status.cc.alice.n"), false},
	} {
		err := tc.a.CheckAgentToken()
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrAgentTokenUnusual)) {
			t.Errorf("%s: %v", name, err)
		}
		if errors.Is(err, ErrRecordInconsistent) {
			t.Errorf("%s: an unusual token must not read as an inconsistent record", name)
		}
	}
}
