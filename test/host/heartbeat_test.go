// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/nats-io/nats.go"
)

func subscribeHeartbeats(t *testing.T, nc *nats.Conn) *nats.Subscription {
	t.Helper()
	sub, err := nc.SubscribeSync(agents.SubjectHeartbeatAll)
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	return sub
}

// TestHeartbeat_PayloadAndCadence: heartbeats arrive on the fixed subject at
// the configured cadence, with the §8.3 fields, no reply subject, and the
// instance id of the service.
//
// catalog: SAP-8-01 SAP-8-02 SAP-8.1-01 SAP-8.1-02 SAP-8.2-01 SAP-8.2-03 SAP-8.3-01 SAP-8.3-02
func TestHeartbeat_PayloadAndCadence(t *testing.T) {
	_, nc := setup(t)
	sub := subscribeHeartbeats(t, nc)
	h := newHost(t, nc, echo, host.WithHeartbeatInterval(100*time.Millisecond), host.WithSubSecondHeartbeats())
	var times []time.Time
	for range 6 {
		m, err := sub.NextMsg(2 * time.Second)
		if err != nil {
			t.Fatalf("beat %d: %v", len(times), err)
		}
		times = append(times, time.Now())
		if m.Subject != "agents.hb."+testAgent+"."+testOwner+"."+testName {
			t.Errorf("subject %q", m.Subject)
		}
		if m.Reply != "" {
			t.Errorf("a heartbeat carries a reply subject %q", m.Reply)
		}
		hb, err := agents.DecodeHeartbeat(m.Data)
		if err != nil {
			t.Fatal(err)
		}
		if hb.Agent != testAgent || hb.Owner != testOwner || hb.InstanceID != h.InstanceID() {
			t.Errorf("heartbeat %+v", hb)
		}
		if hb.IntervalS != 0.1 {
			t.Errorf("interval_s %v", hb.IntervalS)
		}
		if agents.InstanceNameFromSubject(m.Subject) != testName {
			t.Error("the instance name is not the 5th token")
		}
	}
	span := times[len(times)-1].Sub(times[0])
	if span < 350*time.Millisecond || span > 1200*time.Millisecond {
		t.Errorf("5 intervals of 100ms took %v", span)
	}
	if !agents.IsHeartbeatSubject("agents.hb." + testAgent + "." + testOwner + "." + testName) {
		t.Error("IsHeartbeatSubject")
	}
}

// TestHeartbeat_Fields checks each optional and required field of §8.3 against
// the registration.
//
// catalog: SAP-8.3-04 SAP-8.3-05 SAP-8.3-06 SAP-8.3-07 SAP-8.3-08 SAP-8.3-03
func TestHeartbeat_Fields(t *testing.T) {
	_, nc := setup(t)
	sub := subscribeHeartbeats(t, nc)
	h := newHost(t, nc, echo, host.WithSession("s1"), host.WithHeartbeatInterval(2*time.Second))
	m, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(m.Data, &raw); err != nil {
		t.Fatal(err)
	}
	hb, err := agents.DecodeHeartbeat(m.Data)
	if err != nil {
		t.Fatal(err)
	}
	si := info(t, nc, h.InstanceID())
	if hb.InstanceID != si.ID {
		t.Errorf("instance_id %q, service id %q", hb.InstanceID, si.ID)
	}
	ts, err := hb.Time()
	if err != nil || !strings.HasSuffix(hb.TS, "Z") || time.Since(ts) > 5*time.Second || time.Since(ts) < -5*time.Second {
		t.Errorf("ts %q: %v", hb.TS, err)
	}
	if hb.IntervalS != 2 || string(raw["interval_s"]) != "2" {
		t.Errorf("interval_s %s", raw["interval_s"])
	}
	if hb.ProtocolVersion != si.Metadata["protocol_version"] {
		t.Errorf("protocol_version %q", hb.ProtocolVersion)
	}
	if hb.Session != "s1" || si.Metadata["session"] != "s1" {
		t.Errorf("session %q / %q", hb.Session, si.Metadata["session"])
	}
	declared, ok := hb.PromptEndpoint()
	reg := endpointOf(t, si, "prompt")
	if !ok || declared.Subject != reg.Subject || len(declared.Metadata) != len(reg.Metadata) {
		t.Fatalf("declared %+v registered %+v", declared, reg)
	}
	for k, v := range reg.Metadata {
		if declared.Metadata[k] != v {
			t.Errorf("metadata %s: %q vs %q", k, declared.Metadata[k], v)
		}
	}
	// A session-less host leaves the field out entirely.
	h2 := newHostNamed(t, nc, "t2", echo, host.WithHeartbeatInterval(2*time.Second))
	for {
		m, err := sub.NextMsg(2 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(m.Subject, ".t2") {
			var raw2 map[string]json.RawMessage
			_ = json.Unmarshal(m.Data, &raw2)
			if _, has := raw2["session"]; has {
				t.Error("session present on a host with no session")
			}
			break
		}
	}
	_ = h2
}

// TestHeartbeat_ProtocolFixedSubject: the heartbeat subject is built from
// identity and has no queue group.
//
// catalog: SAP-3.3-03
func TestHeartbeat_ProtocolFixedSubject(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, echo)
	si := info(t, nc, h.InstanceID())
	for _, e := range si.Endpoints {
		if agents.IsHeartbeatSubject(e.Subject) || e.Name == "hb" {
			t.Errorf("the heartbeat is registered as endpoint %+v", e)
		}
	}
	// Extra endpoints cannot take the heartbeat subject.
	_, err := host.New(nc, subjectOf(t, testName), echo, host.WithEndpoint(host.Endpoint{
		Name: "beats", Subject: h.Subject().Heartbeat(), Handler: nopHandler()}))
	if err == nil {
		t.Error("an endpoint on the heartbeat subject was accepted")
	}
}

// TestHeartbeat_AfterRegistration: the first beat is published only once the
// service is registered, so a caller that sees it can look the instance up.
//
// catalog: SAP-8.2-05
func TestHeartbeat_AfterRegistration(t *testing.T) {
	_, nc := setup(t)
	sub := subscribeHeartbeats(t, nc)
	h, err := host.New(nc, subjectOf(t, testName), echo, host.WithHeartbeatInterval(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	m, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	hb, _ := agents.DecodeHeartbeat(m.Data)
	si := info(t, nc, hb.InstanceID)
	if si.ID != hb.InstanceID {
		t.Fatal("the first heartbeat preceded the registration")
	}
	// A request to the prompt endpoint right after the first beat is served.
	if got := prompt(t, nc, h, "x").texts(t); len(got) != 1 {
		t.Fatal("endpoint not live at the first beat")
	}
}

// TestHeartbeat_NoGoingAwaySignal: stopping publishes nothing on any protocol
// subject.
func TestHeartbeat_NoGoingAwaySignal(t *testing.T) {
	_, nc := setup(t)
	all, err := nc.SubscribeSync(agents.SubjectAll)
	if err != nil {
		t.Fatal(err)
	}
	h := newHost(t, nc, echo, host.WithHeartbeatInterval(time.Hour))
	if _, err := all.NextMsg(2 * time.Second); err != nil { // the first beat
		t.Fatal(err)
	}
	for {
		if _, err := all.NextMsg(100 * time.Millisecond); err != nil {
			break
		}
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m, err := all.NextMsg(300 * time.Millisecond); err == nil {
		t.Fatalf("a message on stop: %s %q", m.Subject, m.Data)
	}
}

// TestHeartbeat_StopsAtStop: no beat after Stop returns.
func TestHeartbeat_StopsAtStop(t *testing.T) {
	_, nc := setup(t)
	sub := subscribeHeartbeats(t, nc)
	h := newHost(t, nc, echo, host.WithHeartbeatInterval(50*time.Millisecond), host.WithSubSecondHeartbeats())
	for range 2 {
		if _, err := sub.NextMsg(2 * time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := sub.NextMsg(20 * time.Millisecond); err != nil {
			break
		}
	}
	if m, err := sub.NextMsg(300 * time.Millisecond); err == nil {
		t.Fatalf("heartbeat after Stop: %q", m.Data)
	}
}

// TestHeartbeat_Extras: extras ride on every beat and status reply, are read
// per beat, and a bad provider costs the beat its extras, never the beat.
func TestHeartbeat_Extras(t *testing.T) {
	_, nc := setup(t)
	sub := subscribeHeartbeats(t, nc)
	var mu sync.Mutex
	var n int
	mode := "good"
	setMode := func(m string) { mu.Lock(); mode = m; mu.Unlock() }
	h := newHost(t, nc, echo, host.WithHeartbeatInterval(60*time.Millisecond), host.WithSubSecondHeartbeats(),
		host.WithHeartbeatExtras(func() map[string]any {
			mu.Lock()
			n++
			cur, depth := mode, n
			mu.Unlock()
			switch cur {
			case "panic":
				panic("boom")
			case "clash":
				return map[string]any{"agent": "forged"}
			case "unserializable":
				return map[string]any{"c": make(chan int)}
			}
			return map[string]any{"queue_depth": depth}
		}))
	next := func() map[string]json.RawMessage {
		m, err := sub.NextMsg(2 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(m.Data, &raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	a, b := next(), next()
	if a["queue_depth"] == nil || string(a["queue_depth"]) == string(b["queue_depth"]) {
		t.Errorf("extras not read per beat: %s %s", a["queue_depth"], b["queue_depth"])
	}
	_, hb := status(t, nc, h, nil, nil)
	if hb.Extra["queue_depth"] == nil {
		t.Error("no extras on the status reply")
	}
	for _, m := range []string{"panic", "clash", "unserializable"} {
		setMode(m)
		time.Sleep(150 * time.Millisecond)
		raw := next()
		if raw["queue_depth"] != nil || raw["c"] != nil || string(raw["agent"]) != `"`+testAgent+`"` {
			t.Errorf("%s: bad extras reached the wire: %v", m, raw)
		}
		if _, err := agents.DecodeHeartbeat(mustJSON(t, raw)); err != nil {
			t.Errorf("%s: the beat is not a heartbeat: %v", m, err)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestStatus_AlwaysAnswerable: the status endpoint ignores the request body,
// answers a freshly built heartbeat-shaped payload, and uses exactly the §8.3
// schema.
//
// catalog: SAP-8.7-01 SAP-8.7-02 SAP-8.7-03 SAP-8.7-04 SAP-8.7-06 SAP-8.7-07 SAP-8.7.1-01
func TestStatus_AlwaysAnswerable(t *testing.T) {
	_, nc := setup(t)
	sub := subscribeHeartbeats(t, nc)
	h := newHost(t, nc, echo, host.WithSession("s9"))
	beat, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	si := info(t, nc, h.InstanceID())
	st := endpointOf(t, si, "status")
	if st.Subject != "agents.status."+testAgent+"."+testOwner+"."+testName || st.QueueGroup != "agents" {
		t.Errorf("status endpoint %+v", st)
	}
	hbBeat, _ := agents.DecodeHeartbeat(beat.Data)
	var lastTS string
	for _, body := range [][]byte{nil, []byte("ignored"), []byte(`{"unknown":"request schema"}`), []byte("\xff\xfe"), make([]byte, 4096)} {
		m, hb := status(t, nc, h, body, nil)
		if hb.Agent != testAgent || hb.Owner != testOwner || hb.InstanceID != h.InstanceID() || hb.Session != "s9" {
			t.Errorf("status reply %+v", hb)
		}
		if hb.IntervalS != hbBeat.IntervalS || hb.ProtocolVersion != hbBeat.ProtocolVersion || len(hb.Endpoints) != len(hbBeat.Endpoints) {
			t.Errorf("status differs from the heartbeat: %+v vs %+v", hb, hbBeat)
		}
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(m.Data, &raw)
		for k := range raw {
			switch k {
			case "agent", "owner", "session", "instance_id", "ts", "interval_s", "protocol_version", "endpoints":
			default:
				t.Errorf("status reply has a field outside the heartbeat schema: %s", k)
			}
		}
		lastTS = hb.TS
	}
	// Built fresh per request.
	time.Sleep(1100 * time.Millisecond)
	if _, hb := status(t, nc, h, nil, nil); hb.TS == lastTS {
		t.Error("the status reply was not rebuilt")
	}
}

// TestStatus_StatsTick: the micro stats of the status endpoint count requests.
func TestStatus_StatsTick(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, echo)
	for range 3 {
		status(t, nc, h, nil, nil)
	}
	prompt(t, nc, h, "x")
	prompt(t, nc, h, "y")
	var statusReqs, promptReqs int
	settle(t, 2*time.Second, "stats", func() bool {
		for _, e := range h.Service().Stats().Endpoints {
			switch e.Name {
			case "status":
				statusReqs = e.NumRequests
			case "prompt":
				promptReqs = e.NumRequests
			}
		}
		return statusReqs == 3 && promptReqs == 2
	})
	// The reply (and its terminator) reaches the caller before the serving
	// goroutine leaves the in-flight set, so a caller that has just read the
	// last reply can still see InFlight 1 (and PromptsCompleted lagging).
	// Wait for the count to drain, then assert the exact settled counts.
	s := awaitIdle(t, h)
	if s.Prompts != 2 || s.InFlight != 0 {
		t.Errorf("stats %+v", s)
	}
}

// TestHeartbeatSigning_HelpersRefuseNilSigner covers the helper's misuse path.
func TestHeartbeatSigning_HelpersRefuseNilSigner(t *testing.T) {
	_, err := host.SignHeartbeat(context.Background(), host.SignHeartbeatOptions{})
	if err == nil || errors.Is(err, nil) {
		t.Fatal("a nil signer was accepted")
	}
}
