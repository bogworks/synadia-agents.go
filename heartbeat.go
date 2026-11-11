// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// Heartbeat cadence defaults. See SAP §8.2.
const (
	// DefaultHeartbeatInterval is the recommended cadence, 30 seconds.
	DefaultHeartbeatInterval = 30 * time.Second

	// MinRecommendedHeartbeatInterval is the floor below which SAP says
	// heartbeats SHOULD NOT be sent on shared infrastructure. It is advice,
	// not a prohibition.
	MinRecommendedHeartbeatInterval = time.Second

	// OfflineThresholdFactor is the recommended offline threshold: 3 times
	// interval_s since the last observed heartbeat, per instance_id.
	OfflineThresholdFactor = 3

	// MaxHeartbeatInterval is the ceiling this SDK applies to a heartbeat
	// cadence: 24 hours. SAP §8.3 requires only interval_s > 0 and sets no
	// upper bound, so this is SDK policy. It bounds what [Heartbeat.Interval]
	// and [Heartbeat.OfflineAfter] return, so a hostile or absurd interval_s
	// cannot overflow a [time.Duration], and it is the longest cadence
	// [ValidateInterval] accepts. A received interval_s above it is not
	// rejected (SAP §8.3 tolerates it); it is read as the ceiling.
	MaxHeartbeatInterval = 24 * time.Hour
)

// Heartbeat is the payload of a heartbeat, published on agents.hb.{agent}.{owner}.{name},
// and of a status reply, which has exactly the same schema. The instance name
// is not in the payload; receivers read it from the 5th token of the subject.
// See SAP §8.3, §8.7.
//
// Unknown fields are kept in Extra and written back, as callers MUST tolerate
// them (SAP §8.3). When a received protocol_version or endpoints is malformed
// it is ignored, not fatal: the field is left unset and its raw value is kept
// in Extra (SAP §8.3).
type Heartbeat struct {
	// Agent matches metadata.agent. Required.
	Agent string
	// Owner matches metadata.owner. Required.
	Owner string
	// Session matches metadata.session; empty when the metadata field is unset.
	Session string
	// InstanceID is the micro service's per-instance id (the service id).
	// Required.
	InstanceID string
	// TS is the UTC ISO 8601 (RFC 3339) timestamp of publication, kept as the
	// sender wrote it so a relay round-trips it. Required.
	TS string
	// IntervalS is this instance's cadence in seconds. Required; > 0.
	IntervalS float64
	// ProtocolVersion matches metadata.protocol_version, when declared.
	ProtocolVersion string
	// Endpoints declares endpoints by name, "prompt" among them, each with the
	// subject and metadata copied verbatim from the registration. Nil when not
	// declared.
	Endpoints map[string]HeartbeatEndpoint
	// Extra holds unknown fields, verbatim.
	Extra map[string]json.RawMessage
}

// HeartbeatEndpoint is one declared endpoint of a heartbeat: its registered
// subject and its endpoint metadata (SAP §2.1). See SAP §8.3.
type HeartbeatEndpoint struct {
	Subject  string            `json:"subject"`
	Metadata map[string]string `json:"metadata"`
}

// MarshalJSON writes nil Metadata as {} and never as null: SAP §2.1 endpoint
// metadata is an object, and a receiver treats null as malformed. See SAP §8.3.
func (e HeartbeatEndpoint) MarshalJSON() ([]byte, error) {
	md := e.Metadata
	if md == nil {
		md = map[string]string{}
	}
	return json.Marshal(struct {
		Subject  string            `json:"subject"`
		Metadata map[string]string `json:"metadata"`
	}{e.Subject, md})
}

// Interval returns IntervalS as a duration. A value that is not positive (or
// is NaN) reads as 0, and one above [MaxHeartbeatInterval] reads as that
// ceiling (SDK policy; SAP §8.3 sets no upper bound), so the result never
// overflows.
func (h Heartbeat) Interval() time.Duration {
	switch {
	case !(h.IntervalS > 0):
		return 0
	case h.IntervalS >= MaxHeartbeatInterval.Seconds():
		return MaxHeartbeatInterval
	}
	return time.Duration(h.IntervalS * float64(time.Second))
}

// OfflineAfter returns the recommended offline threshold for this instance:
// [OfflineThresholdFactor] times its interval. Because [Heartbeat.Interval] is
// capped at [MaxHeartbeatInterval], the product is bounded and never negative
// or overflowed. See SAP §8.2.
func (h Heartbeat) OfflineAfter() time.Duration { return OfflineThresholdFactor * h.Interval() }

// Time parses TS, which [DecodeHeartbeat] and [Heartbeat.Validate] guarantee
// is an RFC 3339 UTC timestamp.
func (h Heartbeat) Time() (time.Time, error) { return parseTS(h.TS) }

// parseTS parses a UTC ISO 8601 publication timestamp (SAP:L504-L511): RFC 3339
// with a zero offset ("Z" or "+00:00").
func parseTS(ts string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, err
	}
	if _, off := t.Zone(); off != 0 {
		return time.Time{}, fmt.Errorf("timestamp %q is not UTC", ts)
	}
	return t, nil
}

// Stamp sets TS to t in UTC, in the form of SAP's example ("2026-04-21T14:23:01Z").
func (h *Heartbeat) Stamp(t time.Time) { h.TS = t.UTC().Format("2006-01-02T15:04:05Z") }

// SetInterval sets IntervalS from d.
func (h *Heartbeat) SetInterval(d time.Duration) { h.IntervalS = d.Seconds() }

// Validate checks a heartbeat about to be sent: agent, owner and instance_id
// are not empty, ts is an RFC 3339 UTC timestamp (SAP:L504-L511) and
// interval_s is finite and > 0. See SAP §8.3.
func (h Heartbeat) Validate() error {
	switch {
	case h.Agent == "":
		return protoErr("heartbeat agent is empty")
	case h.Owner == "":
		return protoErr("heartbeat owner is empty")
	case h.InstanceID == "":
		return protoErr("heartbeat instance_id is empty")
	case h.TS == "":
		return protoErr("heartbeat ts is empty")
	case !validTS(h.TS):
		return protoErr("heartbeat ts is not an RFC 3339 UTC timestamp")
	case !(h.IntervalS > 0) || math.IsInf(h.IntervalS, 0) || math.IsNaN(h.IntervalS):
		return protoErr("heartbeat interval_s must be a finite number > 0")
	}
	return nil
}

func validTS(ts string) bool { _, err := parseTS(ts); return err == nil }

// ValidateInterval applies the SAP §8.2 advice to a configured cadence. It
// refuses a non-positive interval, an interval above [MaxHeartbeatInterval]
// (SDK policy; SAP §8.3 sets no upper bound), and an interval under one second
// unless allowSubSecond is set, the explicit opt-in (SDK policy: SAP says
// SHOULD NOT, not MUST NOT).
func ValidateInterval(d time.Duration, allowSubSecond bool) error {
	if d <= 0 {
		return protoErr("heartbeat interval must be positive")
	}
	if d > MaxHeartbeatInterval {
		return protoErr("heartbeat interval %s is above the SDK ceiling of %s", d, MaxHeartbeatInterval)
	}
	if d < MinRecommendedHeartbeatInterval && !allowSubSecond {
		return protoErr("heartbeat interval %s is under the recommended minimum of %s; opt in explicitly to use it", d, MinRecommendedHeartbeatInterval)
	}
	return nil
}

// EncodeHeartbeat validates and serializes a heartbeat or status reply. Extra
// fields are written after the known ones, and an extra never shadows a field
// the heartbeat declares. See SAP §8.3, §8.7.
func EncodeHeartbeat(h Heartbeat) ([]byte, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	fields := []field{
		{"agent", h.Agent, true},
		{"owner", h.Owner, true},
		{"session", h.Session, h.Session != ""},
		{"instance_id", h.InstanceID, true},
		{"ts", h.TS, true},
		{"interval_s", h.IntervalS, true},
		{"protocol_version", h.ProtocolVersion, h.ProtocolVersion != ""},
		{"endpoints", h.Endpoints, h.Endpoints != nil},
	}
	// A declared field wins over the same key in Extra; a malformed declaration
	// that DecodeHeartbeat kept in Extra is relayed as received.
	return marshalObject(fields, h.Extra, "agent", "owner", "session", "instance_id", "ts", "interval_s")
}

// DecodeHeartbeat parses a heartbeat or status reply. The required fields are
// non-empty strings agent, owner and instance_id (null is not a string), an
// RFC 3339 UTC string ts (kept as sent) and a finite number interval_s > 0;
// otherwise it returns a [*ProtocolError], and a listener ignores that
// heartbeat. A malformed optional protocol_version or endpoints is ignored,
// not fatal, and kept in Extra. An empty session reads as unset. Unknown
// fields are kept in Extra. See SAP §8.3, §8.7.
func DecodeHeartbeat(data []byte) (Heartbeat, error) {
	obj, err := splitObject(data)
	if err != nil {
		return Heartbeat{}, &ProtocolError{Reason: "heartbeat must be a JSON object", Err: err}
	}
	var h Heartbeat
	for _, f := range []struct {
		key string
		dst *string
	}{{"agent", &h.Agent}, {"owner", &h.Owner}, {"instance_id", &h.InstanceID}, {"ts", &h.TS}} {
		v, ok := stringField(obj, f.key)
		if !ok {
			return Heartbeat{}, &ProtocolError{Reason: fmt.Sprintf("heartbeat missing string `%s`", f.key)}
		}
		if v == "" {
			return Heartbeat{}, &ProtocolError{Reason: fmt.Sprintf("heartbeat `%s` is empty", f.key)}
		}
		*f.dst = v
	}
	if !validTS(h.TS) {
		return Heartbeat{}, &ProtocolError{Reason: "heartbeat `ts` is not an RFC 3339 UTC timestamp"}
	}
	var interval float64
	raw, ok := obj["interval_s"]
	if !ok || json.Unmarshal(raw, &interval) != nil || !(interval > 0) || math.IsInf(interval, 0) || math.IsNaN(interval) {
		return Heartbeat{}, &ProtocolError{Reason: "heartbeat `interval_s` must be a finite number > 0"}
	}
	h.IntervalS = interval
	known := []string{"agent", "owner", "instance_id", "ts", "interval_s", "session"}
	if s, ok := stringField(obj, "session"); ok {
		h.Session = s // an empty session reads as unset
	}
	if s, ok := stringField(obj, "protocol_version"); ok && s != "" {
		h.ProtocolVersion = s
		known = append(known, "protocol_version")
	}
	if raw, present := obj["endpoints"]; present {
		if eps, ok := decodeHeartbeatEndpoints(raw); ok {
			h.Endpoints = eps
			known = append(known, "endpoints")
		}
	}
	h.Extra = withoutKeys(obj, known...)
	return h, nil
}

// decodeHeartbeatEndpoints reads the endpoints object; one malformed endpoint
// makes the whole declaration malformed.
func decodeHeartbeatEndpoints(raw json.RawMessage) (map[string]HeartbeatEndpoint, bool) {
	eps, err := splitObject(raw)
	if err != nil {
		return nil, false
	}
	out := make(map[string]HeartbeatEndpoint, len(eps))
	for name, v := range eps {
		e, err := splitObject(v)
		if err != nil {
			return nil, false
		}
		subject, ok := stringField(e, "subject")
		if !ok || subject == "" {
			return nil, false
		}
		md := map[string]string{}
		// Absent and null metadata both mean "none": an empty map.
		if mraw, present := e["metadata"]; present && string(bytes.TrimSpace(mraw)) != "null" {
			raws, err := splitObject(mraw)
			if err != nil {
				return nil, false
			}
			for k, v := range raws {
				str, ok := rawString(v)
				if !ok {
					return nil, false
				}
				md[k] = str
			}
		}
		out[name] = HeartbeatEndpoint{Subject: subject, Metadata: md}
	}
	return out, true
}
