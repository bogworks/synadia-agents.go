// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller

import (
	"fmt"
	"strings"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/nats-io/nats.go"
)

func hbPayload(id string, interval string, extra string) []byte {
	return []byte(fmt.Sprintf(`{"agent":"a","owner":"o","instance_id":%q,"ts":"2026-01-01T00:00:00Z","interval_s":%s%s}`, id, interval, extra))
}

func newTestTracker(t *testing.T, opts ...TrackerOption) *HeartbeatTracker {
	t.Helper()
	tr, err := NewHeartbeatTracker(new(nats.Conn), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func feed(tr *HeartbeatTracker, subject string, payload []byte) {
	tr.handle(&nats.Msg{Subject: subject, Data: payload})
}

// TestTracker_OfflineThreshold asserts that an instance is online while its
// last heartbeat is at most three times its own interval old, the boundary
// included, and that the multiple is configurable.
//
// catalog: SAP-8.2-04
func TestTracker_OfflineThreshold(t *testing.T) {
	cases := []struct {
		name     string
		slack    float64
		interval string
		age      time.Duration
		online   bool
	}{
		{"fresh", 3, "10", 0, true},
		{"just under three intervals", 3, "10", 30*time.Second - time.Nanosecond, true},
		{"exactly three intervals", 3, "10", 30 * time.Second, true},
		{"just past three intervals", 3, "10", 30*time.Second + time.Nanosecond, false},
		{"fractional interval", 3, "0.5", 1500 * time.Millisecond, true},
		{"fractional interval, past", 3, "0.5", 1501 * time.Millisecond, false},
		{"custom slack of two", 2, "10", 20*time.Second + time.Nanosecond, false},
		{"custom slack of five", 5, "10", 50 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTestTracker(t, WithTrackerSlack(tc.slack))
			feed(tr, "agents.hb.a.o.n", hbPayload("i1", tc.interval, ""))
			l, ok := tr.Liveness("i1")
			if !ok {
				t.Fatal("not tracked")
			}
			l, _ = tr.Liveness("i1", l.LastSeen.Add(tc.age))
			if l.Online != tc.online {
				t.Fatalf("online=%v at age %s, want %v (interval %v)", l.Online, tc.age, tc.online, l.Interval)
			}
		})
	}
	if _, err := NewHeartbeatTracker(new(nats.Conn), WithTrackerSlack(0)); err == nil {
		t.Fatal("a zero slack was accepted")
	}
}

// TestTracker_PerInstanceAndNameFromSubject asserts that liveness is tracked per
// instance_id from the payload (two instances of one logical agent share a
// subject and stay distinct) and that the instance name comes from the fifth
// token of the heartbeat subject.
//
// catalog: SAP-12-13
// catalog: SAP-8.3-09
func TestTracker_PerInstanceAndNameFromSubject(t *testing.T) {
	tr := newTestTracker(t)
	feed(tr, "agents.hb.a.o.main", hbPayload("i1", "10", ""))
	feed(tr, "agents.hb.a.o.main", hbPayload("i2", "20", ""))
	l1, ok1 := tr.Liveness("i1")
	l2, ok2 := tr.Liveness("i2")
	if !ok1 || !ok2 || l1.Interval != 10*time.Second || l2.Interval != 20*time.Second {
		t.Fatalf("%+v %+v", l1, l2)
	}
	if l1.Name != "main" || l1.Agent != "a" || l1.Owner != "o" {
		t.Fatalf("%+v", l1)
	}
	if got := len(tr.Snapshot()); got != 2 {
		t.Fatalf("%d instances", got)
	}
	if _, ok := tr.Liveness("agents.hb.a.o.main"); ok {
		t.Fatal("the tracker keyed on the subject")
	}
}

// TestTracker_ToleratesUnknownAndMalformedOptionalFields asserts that unknown
// fields are tolerated and that a malformed optional field is ignored while the
// rest of the heartbeat is kept; a malformed required field drops the beat.
//
// catalog: SAP-8.3-11
// catalog: SAP-8.3-12
func TestTracker_ToleratesUnknownAndMalformedOptionalFields(t *testing.T) {
	tr := newTestTracker(t)
	feed(tr, "agents.hb.a.o.n", hbPayload("unknown", "10", `,"future":{"x":[1,2]}`))
	feed(tr, "agents.hb.a.o.n", hbPayload("badproto", "10", `,"protocol_version":5`))
	feed(tr, "agents.hb.a.o.n", hbPayload("badendpoints", "10", `,"endpoints":"nope"`))
	for _, id := range []string{"unknown", "badproto", "badendpoints"} {
		if _, ok := tr.Liveness(id); !ok {
			t.Fatalf("%s was dropped", id)
		}
	}
	if tr.Dropped() != 0 {
		t.Fatalf("dropped %d", tr.Dropped())
	}
	for name, payload := range map[string][]byte{
		"not json":        []byte("hello"),
		"no instance":     []byte(`{"agent":"a","owner":"o","ts":"2026-01-01T00:00:00Z","interval_s":10}`),
		"zero interval":   hbPayload("zero", "0", ""),
		"negative":        hbPayload("neg", "-1", ""),
		"string interval": hbPayload("str", `"10"`, ""),
		"too large":       append(hbPayload("big", "10", `,"pad":"`), []byte(strings.Repeat("x", maxHeartbeatBytes)+`"}`)...),
		"overlong id":     hbPayload(strings.Repeat("i", maxHeartbeatField+1), "10", ""),
	} {
		before := tr.Dropped()
		feed(tr, "agents.hb.a.o.n", payload)
		if tr.Dropped() != before+1 {
			t.Fatalf("%s: the heartbeat was not dropped", name)
		}
	}
	if len(tr.Snapshot()) != 3 {
		t.Fatalf("%d instances tracked", len(tr.Snapshot()))
	}
}

// TestTracker_ListenersAndBounds covers the listener contract and the bound on
// what a peer can make the tracker remember.
func TestTracker_ListenersAndBounds(t *testing.T) {
	t.Run("listeners run in order under recover and may unsubscribe themselves", func(t *testing.T) {
		tr := newTestTracker(t)
		var order []string
		var cancelSelf func()
		cancelSelf = tr.OnAnyHeartbeat(func(Beat) { order = append(order, "once"); cancelSelf() })
		tr.OnAnyHeartbeat(func(Beat) { order = append(order, "panics"); panic("listener bug") })
		tr.OnHeartbeat("i1", func(b Beat) { order = append(order, "scoped:"+b.Heartbeat.InstanceID) })
		tr.OnHeartbeat("other", func(Beat) { order = append(order, "wrong instance") })
		feed(tr, "agents.hb.a.o.n", hbPayload("i1", "10", ""))
		feed(tr, "agents.hb.a.o.n", hbPayload("i1", "10", ""))
		if got := strings.Join(order, ","); got != "once,panics,scoped:i1,panics,scoped:i1" {
			t.Fatalf("order %s", got)
		}
	})
	t.Run("a full table evicts an offline instance and otherwise ignores the newcomer", func(t *testing.T) {
		tr := newTestTracker(t, WithTrackerMaxInstances(2), WithTrackerSlack(1))
		feed(tr, "agents.hb.a.o.n", hbPayload("stale", "0.001", "")) // offline almost at once
		feed(tr, "agents.hb.a.o.n", hbPayload("live", "3600", ""))
		time.Sleep(10 * time.Millisecond)
		feed(tr, "agents.hb.a.o.n", hbPayload("new1", "3600", ""))
		if _, ok := tr.Liveness("stale"); ok {
			t.Fatal("the offline instance was kept")
		}
		if _, ok := tr.Liveness("new1"); !ok {
			t.Fatal("the newcomer was not admitted")
		}
		feed(tr, "agents.hb.a.o.n", hbPayload("new2", "3600", ""))
		if _, ok := tr.Liveness("new2"); ok {
			t.Fatal("a newcomer displaced a live instance")
		}
		if _, ok := tr.Liveness("live"); !ok {
			t.Fatal("a live instance was evicted")
		}
		// A live instance's own heartbeats keep working when the table is full.
		feed(tr, "agents.hb.a.o.n", hbPayload("live", "3600", ""))
	})
}

// TestTracker_IntervalCeiling asserts that a heartbeat claiming an interval_s
// above agents.MaxHeartbeatInterval is dropped (so it cannot hold a slot online for
// years or overflow a duration), that one at the ceiling is kept, and that a
// status reply is held to the same bound.
func TestTracker_IntervalCeiling(t *testing.T) {
	tr := newTestTracker(t)
	feed(tr, "agents.hb.a.o.n", hbPayload("ok", "86400", ""))
	if _, ok := tr.Liveness("ok"); !ok {
		t.Fatal("a heartbeat at the ceiling was dropped")
	}
	for name, iv := range map[string]string{"just over": "86401", "huge": "1e9", "overflow": "1e12"} {
		before := tr.Dropped()
		feed(tr, "agents.hb.a.o.n", hbPayload("big-"+name, iv, ""))
		if tr.Dropped() != before+1 {
			t.Fatalf("%s: not dropped", name)
		}
		if _, ok := tr.Liveness("big-" + name); ok {
			t.Fatalf("%s: tracked", name)
		}
	}
	tr.observe(agents.Heartbeat{Agent: "a", Owner: "o", InstanceID: "st", TS: "2026-01-01T00:00:00Z", IntervalS: 1e12}, "agents.status.a.o.n")
	if _, ok := tr.Liveness("st"); ok {
		t.Fatal("a status reply above the ceiling was tracked")
	}
}
