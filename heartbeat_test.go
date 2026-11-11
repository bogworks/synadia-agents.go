// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The payload of SAP:L486-L501 (§8.3), verbatim.
const sapHeartbeatExample = `{
  "agent": "claude-code",
  "owner": "aconnolly",
  "session": "synadia-com-2",
  "instance_id": "VMKS6MHK71PCPWGY38A7N5",
  "ts": "2026-04-21T14:23:01Z",
  "interval_s": 30,
  "protocol_version": "0.3",
  "endpoints": {
    "prompt": {
      "subject": "agents.prompt.claude-code.aconnolly.synadia-com-2",
      "metadata": { "max_payload": "1MB", "attachments_ok": "true", "min_sender_trust": "signed" }
    }
  }
}`

// REQ-005, SAP:L484-L513 (§8.3): decode the specification's own example.
func TestHeartbeat_DecodeSpecExample(t *testing.T) {
	h, err := DecodeHeartbeat([]byte(sapHeartbeatExample))
	if err != nil {
		t.Fatal(err)
	}
	want := Heartbeat{
		Agent: "claude-code", Owner: "aconnolly", Session: "synadia-com-2", InstanceID: "VMKS6MHK71PCPWGY38A7N5",
		TS: "2026-04-21T14:23:01Z", IntervalS: 30, ProtocolVersion: "0.3",
		Endpoints: map[string]HeartbeatEndpoint{"prompt": {
			Subject:  "agents.prompt.claude-code.aconnolly.synadia-com-2",
			Metadata: map[string]string{"max_payload": "1MB", "attachments_ok": "true", "min_sender_trust": "signed"}}},
	}
	if !reflect.DeepEqual(h, want) {
		t.Fatalf("%+v", h)
	}
	if h.Interval() != 30*time.Second || h.OfflineAfter() != 90*time.Second {
		t.Fatal(h.Interval(), h.OfflineAfter())
	}
	if tm, err := h.Time(); err != nil || tm.Year() != 2026 {
		t.Fatal(tm, err)
	}
	ep, ok := h.PromptEndpoint()
	if !ok || ep.Subject != want.Endpoints["prompt"].Subject {
		t.Fatal(ep, ok)
	}
	if l, err := ep.PromptLimits(); err != nil || l.MaxPayload != 1<<20 || l.AttachmentsOK == nil || !*l.AttachmentsOK {
		t.Fatalf("%+v", l)
	}
}

// SAP:L504-L513 (§8.3): required fields, and interval_s > 0.
func TestHeartbeat_DecodeRequiredFields(t *testing.T) {
	ok := `{"agent":"a","owner":"o","instance_id":"i","ts":"2026-04-21T14:23:01Z","interval_s":1}`
	if _, err := DecodeHeartbeat([]byte(ok)); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]string{
		"not json":        `x`,
		"array":           `[]`,
		"no agent":        `{"owner":"o","instance_id":"i","ts":"2026-04-21T14:23:01Z","interval_s":1}`,
		"no owner":        `{"agent":"a","instance_id":"i","ts":"2026-04-21T14:23:01Z","interval_s":1}`,
		"no instance":     `{"agent":"a","owner":"o","ts":"2026-04-21T14:23:01Z","interval_s":1}`,
		"no ts":           `{"agent":"a","owner":"o","instance_id":"i","interval_s":1}`,
		"no interval":     `{"agent":"a","owner":"o","instance_id":"i","ts":"2026-04-21T14:23:01Z"}`,
		"interval string": `{"agent":"a","owner":"o","instance_id":"i","ts":"2026-04-21T14:23:01Z","interval_s":"30"}`,
		"interval zero":   `{"agent":"a","owner":"o","instance_id":"i","ts":"2026-04-21T14:23:01Z","interval_s":0}`,
		"interval neg":    `{"agent":"a","owner":"o","instance_id":"i","ts":"2026-04-21T14:23:01Z","interval_s":-1}`,
		"agent number":    `{"agent":1,"owner":"o","instance_id":"i","ts":"2026-04-21T14:23:01Z","interval_s":1}`,
	} {
		if _, err := DecodeHeartbeat([]byte(in)); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// SAP:L517 (§8.3): a malformed optional field is ignored and the heartbeat kept.
func TestHeartbeat_MalformedOptionalFieldsIgnored(t *testing.T) {
	base := `"agent":"a","owner":"o","instance_id":"i","ts":"2026-04-21T14:23:01Z","interval_s":5`
	for name, in := range map[string]string{
		"protocol_version number":  `{` + base + `,"protocol_version":3}`,
		"protocol_version empty":   `{` + base + `,"protocol_version":""}`,
		"endpoints array":          `{` + base + `,"endpoints":[]}`,
		"endpoint not object":      `{` + base + `,"endpoints":{"prompt":"x"}}`,
		"endpoint no subject":      `{` + base + `,"endpoints":{"prompt":{"metadata":{}}}}`,
		"endpoint empty subject":   `{` + base + `,"endpoints":{"prompt":{"subject":""}}}`,
		"endpoint metadata number": `{` + base + `,"endpoints":{"prompt":{"subject":"s","metadata":{"k":1}}}}`,
		"endpoint metadata array":  `{` + base + `,"endpoints":{"prompt":{"subject":"s","metadata":[]}}}`,
	} {
		h, err := DecodeHeartbeat([]byte(in))
		if err != nil {
			t.Errorf("%s: heartbeat dropped: %v", name, err)
			continue
		}
		if h.ProtocolVersion != "" || h.Endpoints != nil {
			t.Errorf("%s: malformed field kept: %+v", name, h)
		}
	}
	// A missing endpoint metadata is fine.
	h, _ := DecodeHeartbeat([]byte(`{` + base + `,"endpoints":{"prompt":{"subject":"s"}}}`))
	if h.Endpoints["prompt"].Subject != "s" {
		t.Fatalf("%+v", h)
	}
}

// SAP:L519 (§8.3): unknown fields are tolerated; a malformed declaration is relayed as received.
func TestHeartbeat_UnknownFieldsPreserved(t *testing.T) {
	in := `{"agent":"a","owner":"o","instance_id":"i","ts":"2026-04-21T14:23:01Z","interval_s":5,"queue_depth":3,"endpoints":"junk","session":""}`
	h, err := DecodeHeartbeat([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if h.Session != "" || string(h.Extra["queue_depth"]) != "3" || string(h.Extra["endpoints"]) != `"junk"` {
		t.Fatalf("%+v", h)
	}
	out, err := EncodeHeartbeat(h)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"queue_depth":3`) || !strings.Contains(string(out), `"endpoints":"junk"`) || strings.Contains(string(out), "session") {
		t.Fatal(string(out))
	}
}

// SAP:L488-L501 (§8.3): encode then decode is the identity, with session only when set.
func TestHeartbeat_EncodeRoundTrip(t *testing.T) {
	h := Heartbeat{Agent: "a", Owner: "o", InstanceID: "i", IntervalS: 30, ProtocolVersion: ProtocolVersion,
		Endpoints: map[string]HeartbeatEndpoint{"prompt": {Subject: "agents.prompt.a.o.n", Metadata: PromptEndpointMetadata{MaxPayload: 1 << 20, AttachmentsOK: true}.Map()}},
		Extra:     map[string]json.RawMessage{"agent": json.RawMessage(`"shadow"`), "x": json.RawMessage(`1`)},
	}
	h.Stamp(time.Date(2026, 4, 21, 14, 23, 1, 0, time.FixedZone("x", 3600)))
	if h.TS != "2026-04-21T13:23:01Z" {
		t.Fatal(h.TS)
	}
	b, err := EncodeHeartbeat(h)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "session") || strings.Contains(string(b), "shadow") {
		t.Fatal(string(b))
	}
	back, err := DecodeHeartbeat(b)
	if err != nil {
		t.Fatal(err)
	}
	delete(h.Extra, "agent")
	if !reflect.DeepEqual(back, h) {
		t.Fatalf("\n%+v\n%+v", back, h)
	}
	h.Session = "s1"
	b, _ = EncodeHeartbeat(h)
	if back, _ := DecodeHeartbeat(b); back.Session != "s1" {
		t.Fatal("session lost")
	}
	// An optional declaration stays out of the payload when unset.
	bare, _ := EncodeHeartbeat(Heartbeat{Agent: "a", Owner: "o", InstanceID: "i", TS: "2026-04-21T14:23:01Z", IntervalS: 1})
	if string(bare) != `{"agent":"a","owner":"o","instance_id":"i","ts":"2026-04-21T14:23:01Z","interval_s":1}` {
		t.Fatal(string(bare))
	}
}

// SAP:L504-L511 (§8.3): a heartbeat is validated before it is sent.
func TestHeartbeat_EncodeValidates(t *testing.T) {
	ok := Heartbeat{Agent: "a", Owner: "o", InstanceID: "i", TS: "2026-04-21T14:23:01Z", IntervalS: 1}
	for name, mod := range map[string]func(*Heartbeat){
		"agent": func(h *Heartbeat) { h.Agent = "" }, "owner": func(h *Heartbeat) { h.Owner = "" },
		"instance": func(h *Heartbeat) { h.InstanceID = "" }, "ts": func(h *Heartbeat) { h.TS = "" },
		"zero interval": func(h *Heartbeat) { h.IntervalS = 0 }, "negative": func(h *Heartbeat) { h.IntervalS = -1 },
	} {
		h := ok
		mod(&h)
		if _, err := EncodeHeartbeat(h); err == nil || h.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	h := ok
	h.SetInterval(1500 * time.Millisecond)
	if h.IntervalS != 1.5 {
		t.Fatal(h.IntervalS)
	}
}

// REQ-005, SAP:L478 (§8.2): 30 s default; under 1 s only by explicit opt-in; 3x offline threshold.
func TestHeartbeat_IntervalPolicy(t *testing.T) {
	if DefaultHeartbeatInterval != 30*time.Second || OfflineThresholdFactor != 3 {
		t.Fatal("defaults")
	}
	cases := []struct {
		d     time.Duration
		allow bool
		ok    bool
	}{
		{30 * time.Second, false, true}, {time.Second, false, true},
		{999 * time.Millisecond, false, false}, {999 * time.Millisecond, true, true},
		{0, true, false}, {-time.Second, true, false},
	}
	for _, c := range cases {
		if err := ValidateInterval(c.d, c.allow); (err == nil) != c.ok {
			t.Errorf("%v allow=%v: %v", c.d, c.allow, err)
		}
	}
}

// SDK policy (SAP §8.3 sets no ceiling): Interval and OfflineAfter never overflow.
func TestHeartbeat_IntervalClamp(t *testing.T) {
	for _, c := range []struct {
		s         float64
		iv, after time.Duration
	}{
		{30, 30 * time.Second, 90 * time.Second},
		{86400, MaxHeartbeatInterval, 3 * MaxHeartbeatInterval},
		{1e300, MaxHeartbeatInterval, 3 * MaxHeartbeatInterval},
		{math.Inf(1), MaxHeartbeatInterval, 3 * MaxHeartbeatInterval},
		{9.3e9, MaxHeartbeatInterval, 3 * MaxHeartbeatInterval},
		{0, 0, 0}, {-5, 0, 0}, {math.NaN(), 0, 0}, {math.Inf(-1), 0, 0},
	} {
		h := Heartbeat{IntervalS: c.s}
		if h.Interval() != c.iv || h.OfflineAfter() != c.after {
			t.Errorf("IntervalS=%v: Interval=%v OfflineAfter=%v", c.s, h.Interval(), h.OfflineAfter())
		}
	}
	if err := ValidateInterval(MaxHeartbeatInterval, false); err != nil {
		t.Error(err)
	}
	if err := ValidateInterval(MaxHeartbeatInterval+time.Second, false); err == nil {
		t.Error("interval above the ceiling accepted")
	}
	// A receiver tolerates a large interval_s and reads it as the ceiling.
	h, err := DecodeHeartbeat([]byte(`{"agent":"a","owner":"o","instance_id":"i","ts":"2026-04-21T14:23:01Z","interval_s":1e300}`))
	if err != nil || h.OfflineAfter() != 3*MaxHeartbeatInterval {
		t.Fatalf("%v %v", h.OfflineAfter(), err)
	}
}

// SAP §2.1/§8.3: endpoint metadata is an object; nil encodes as {}, null decodes as none.
func TestHeartbeat_EndpointMetadataNilAndNull(t *testing.T) {
	b, err := EncodeHeartbeat(Heartbeat{Agent: "a", Owner: "o", InstanceID: "i", TS: "2026-04-21T14:23:01Z", IntervalS: 5,
		Endpoints: map[string]HeartbeatEndpoint{"prompt": {Subject: "s"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"metadata":{}`) || strings.Contains(string(b), "null") {
		t.Fatalf("nil metadata encoded as %s", b)
	}
	in := `{"agent":"a","owner":"o","instance_id":"i","ts":"2026-04-21T14:23:01Z","interval_s":5,"endpoints":{"prompt":{"subject":"s","metadata":null}}}`
	h, err := DecodeHeartbeat([]byte(in))
	if err != nil || h.Endpoints["prompt"].Subject != "s" || h.Endpoints["prompt"].Metadata == nil {
		t.Fatalf("null metadata dropped the endpoints: %+v %v", h, err)
	}
}
