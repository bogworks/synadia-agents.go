// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
)

// beatPayload is a heartbeat or status reply payload, decoded for judgement.
type beatPayload struct {
	Obj map[string]any
}

func (b beatPayload) str(k string) (string, bool) { s, ok := b.Obj[k].(string); return s, ok }

// hbProblems lists how a payload departs from the SAP §8.3 schema.
func (t *T) hbProblems(data []byte, at time.Time) []string {
	obj, err := decodeObject(data)
	if err != nil {
		return []string{"payload is not a JSON object: " + err.Error()}
	}
	b := beatPayload{obj}
	var p []string
	for _, k := range []string{"agent", "owner", "instance_id"} {
		if s, ok := b.str(k); !ok || s == "" {
			p = append(p, k+" is missing or not a non-empty string")
		}
	}
	p = append(p, tsProblems(b, at, t.cfg.ClockSkew)...)
	p = append(p, intervalProblems(b)...)
	return p
}

func tsProblems(b beatPayload, at time.Time, skew time.Duration) []string {
	s, ok := b.str("ts")
	if !ok {
		return []string{"ts is missing or not a string"}
	}
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return []string{fmt.Sprintf("ts %q is not an ISO 8601 / RFC 3339 timestamp", s)}
	}
	if _, off := ts.Zone(); off != 0 {
		return []string{fmt.Sprintf("ts %q is not UTC", s)}
	}
	if d := at.Sub(ts); d > skew || d < -skew {
		return []string{fmt.Sprintf("ts %q is %s from the time it was received (allowed %s)", s, d.Round(time.Second), skew)}
	}
	return nil
}

func intervalProblems(b beatPayload) []string {
	v, ok := b.Obj["interval_s"]
	if !ok {
		return []string{"interval_s is missing"}
	}
	n, ok := v.(json.Number)
	if !ok {
		return []string{fmt.Sprintf("interval_s is %T, not a number", v)}
	}
	f, err := n.Float64()
	if err != nil || f <= 0 {
		return []string{fmt.Sprintf("interval_s %s is not greater than zero", n)}
	}
	return nil
}

func (t *T) beatsOrFail(ctx context.Context) ([]HB, *Outcome) {
	beats := t.heartbeats(ctx)
	if len(beats) == 0 {
		o := fail("no heartbeat was observed on agents.hb.*.*.* from this agent within the observation window")
		return nil, &o
	}
	return beats, nil
}

func registerHeartbeat(add adder) {
	add(func(ctx context.Context, t *T) Outcome {
		beats, bad := t.beatsOrFail(ctx)
		if bad != nil {
			return *bad
		}
		return pass("%d heartbeat(s) observed", len(beats))
	}, "SAP-8-01")

	add(func(ctx context.Context, t *T) Outcome {
		beats, bad := t.beatsOrFail(ctx)
		if bad != nil {
			return *bad
		}
		for _, b := range beats {
			if b.Reply != "" {
				return fail("a heartbeat carries a reply subject (%s), so it asks for an answer", b.Reply)
			}
		}
		return pass("no heartbeat carries a reply subject")
	}, "SAP-8-02")

	add(func(ctx context.Context, t *T) Outcome {
		beats, bad := t.beatsOrFail(ctx)
		if bad != nil {
			return *bad
		}
		want := t.ExpectedHeartbeatSubject()
		for _, b := range beats {
			if !agents.IsHeartbeatSubject(b.Subject) {
				return fail("heartbeat subject %q is not agents.hb.{agent}.{owner}.{name}", b.Subject)
			}
			if want != "" && b.Subject != want {
				return fail("heartbeat subject %q, want %q (the tokens of the prompt subject)", b.Subject, want)
			}
		}
		return pass("heartbeats are on %s", beats[0].Subject)
	}, "SAP-2-06", "SAP-8.1-01")

	add(func(ctx context.Context, t *T) Outcome {
		beats, bad := t.beatsOrFail(ctx)
		if bad != nil {
			return *bad
		}
		ids := t.instanceIDs()
		got := map[string]bool{}
		for _, b := range beats {
			if !ids[b.InstanceID] {
				return fail("a heartbeat names instance %q, which is not a registered service id", b.InstanceID)
			}
			got[b.InstanceID] = true
		}
		if len(ids) > 1 && len(got) < len(ids) {
			return fail("%d of %d instances published a heartbeat", len(got), len(ids))
		}
		return pass("%d instance(s) published their own heartbeats, told apart by instance_id", len(got))
	}, "SAP-8.1-02")

	add(func(ctx context.Context, t *T) Outcome {
		beats, bad := t.beatsOrFail(ctx)
		if bad != nil {
			return *bad
		}
		var problems []string
		for _, b := range beats {
			problems = append(problems, intervalProblems(beatPayload{mustObj(b.Data)})...)
		}
		if len(problems) == 0 {
			iv := declaredInterval(beats[0].Data)
			return pass("interval_s %v (the recommended default is %v)", iv, agents.DefaultHeartbeatInterval.Seconds())
		}
		return verdict(uniqueStrings(problems), "")
	}, "SAP-8.2-01")

	add(chkCadence, "SAP-8.2-03")

	add(func(ctx context.Context, t *T) Outcome {
		beats, bad := t.beatsOrFail(ctx)
		if bad != nil {
			return *bad
		}
		first := map[string]bool{}
		for _, b := range beats {
			if first[b.InstanceID] {
				continue // one INFO probe per instance id; a repeated beat adds no responder
			}
			first[b.InstanceID] = true
			// The runner cannot see a host start. It can see that the service
			// record was answerable by the time the first heartbeat arrived.
			deadline := time.Now().Add(t.cfg.Timing.FirstBeatInfoWait)
			for {
				answered, known := t.infoAtFirstBeat(b.InstanceID)
				if known && answered {
					break
				}
				if time.Now().After(deadline) || ctx.Err() != nil {
					return fail("$SRV.INFO.agents.%s was not answerable when the instance's first heartbeat arrived", b.InstanceID)
				}
				time.Sleep(t.cfg.Timing.PollInterval)
			}
		}
		return pass("the service record was answerable at each instance's first observed heartbeat; the runner cannot observe the ordering at host start")
	}, "SAP-8.2-05")

	perBeat := func(what string, check func(t *T, b HB, obj map[string]any) string) CheckFunc {
		return func(ctx context.Context, t *T) Outcome {
			beats, bad := t.beatsOrFail(ctx)
			if bad != nil {
				return *bad
			}
			var problems []string
			for i, b := range beats {
				obj, err := decodeObject(b.Data)
				if err != nil {
					problems = append(problems, fmt.Sprintf("heartbeat %d: not a JSON object", i))
					continue
				}
				if p := check(t, b, obj); p != "" {
					problems = append(problems, fmt.Sprintf("heartbeat %d: %s", i, p))
				}
			}
			return verdict(uniqueStrings(problems), fmt.Sprintf("%s on all %d heartbeat(s)", what, len(beats)))
		}
	}
	metaEq := func(field, metaKey string) CheckFunc {
		return perBeat(field+" equals metadata."+metaKey, func(t *T, b HB, obj map[string]any) string {
			want, _ := t.instFor(b.InstanceID).MetaString(metaKey)
			got, ok := obj[field].(string)
			if !ok {
				return field + " is missing or not a string"
			}
			if got != want {
				return fmt.Sprintf("%s %q differs from metadata.%s %q", field, got, metaKey, want)
			}
			return ""
		})
	}
	add(metaEq("agent", agents.MetaAgent), "SAP-8.3-01")
	add(metaEq("owner", agents.MetaOwner), "SAP-8.3-02")
	add(perBeat("instance_id equals the service id", func(t *T, _ HB, obj map[string]any) string {
		id, _ := obj["instance_id"].(string)
		if !t.instanceIDs()[id] {
			return fmt.Sprintf("instance_id %q is not a registered service id", id)
		}
		return ""
	}), "SAP-8.3-04", "SAP-3.4-03")
	add(perBeat("ts is a UTC ISO 8601 timestamp near publication", func(t *T, b HB, obj map[string]any) string {
		return joinProblems(tsProblems(beatPayload{obj}, b.At, t.cfg.ClockSkew))
	}), "SAP-8.3-05")
	add(perBeat("interval_s is a number greater than zero", func(_ *T, _ HB, obj map[string]any) string {
		return joinProblems(intervalProblems(beatPayload{obj}))
	}), "SAP-8.3-06")
	add(perBeat("protocol_version, where present, matches the metadata", func(t *T, b HB, obj map[string]any) string {
		v, present := obj["protocol_version"]
		if !present {
			return ""
		}
		want, _ := t.instFor(b.InstanceID).MetaString(agents.MetaProtocolVersion)
		if s, ok := v.(string); !ok || s != want {
			return fmt.Sprintf("protocol_version %v differs from metadata.protocol_version %q", v, want)
		}
		return ""
	}), "SAP-8.3-07")

	add(func(ctx context.Context, t *T) Outcome {
		beats, bad := t.beatsOrFail(ctx)
		if bad != nil {
			return *bad
		}
		declared := 0
		var problems []string
		for i, b := range beats {
			obj, err := decodeObject(b.Data)
			if err != nil {
				continue // reported per beat as "not a JSON object" by SAP-8.3-01..07
			}
			eps, present := obj["endpoints"]
			if !present {
				continue // endpoints is optional in a heartbeat; absence hides no responder
			}
			declared++
			m, ok := eps.(map[string]any)
			if !ok {
				problems = append(problems, fmt.Sprintf("heartbeat %d: endpoints is not an object", i))
				continue
			}
			p, ok := m["prompt"].(map[string]any)
			if !ok {
				problems = append(problems, fmt.Sprintf("heartbeat %d: endpoints is declared without prompt", i))
				continue
			}
			if reg := t.instFor(b.InstanceID).Endpoint(agents.EndpointPrompt); reg != nil {
				if subj, _ := p["subject"].(string); subj != reg.Subject {
					problems = append(problems, fmt.Sprintf("heartbeat %d: prompt subject %q differs from the registration %q", i, subj, reg.Subject))
				}
				if !sameMeta(p["metadata"], reg.Meta) {
					problems = append(problems, fmt.Sprintf("heartbeat %d: prompt metadata differs from the registration", i))
				}
			}
		}
		if declared == 0 {
			return na("the agent's heartbeats declare no endpoints")
		}
		return verdict(uniqueStrings(problems), "the declared prompt endpoint repeats the registration verbatim")
	}, "SAP-8.3-08")

	registerStatus(add)
}

func sameMeta(got any, want map[string]any) bool {
	m, ok := got.(map[string]any)
	if !ok {
		return len(want) == 0
	}
	if len(m) != len(want) {
		return false
	}
	for k, v := range want {
		if fmt.Sprint(m[k]) != fmt.Sprint(v) {
			return false
		}
	}
	return true
}

func chkCadence(ctx context.Context, t *T) Outcome {
	beats, bad := t.beatsOrFail(ctx)
	if bad != nil {
		return *bad
	}
	byID := map[string][]HB{}
	for _, b := range beats {
		byID[b.InstanceID] = append(byID[b.InstanceID], b)
	}
	var problems []string
	judged := 0
	for id, bs := range byID {
		if len(bs) < 2 {
			continue // fewer than two beats: not enough to judge cadence; the beats themselves are judged by SAP-8.3-*
		}
		iv := declaredInterval(bs[0].Data)
		if iv <= 0 {
			continue // a bad interval_s is reported by SAP-8.3-06
		}
		judged++
		var total time.Duration
		for i := 1; i < len(bs); i++ {
			total += bs[i].At.Sub(bs[i-1].At)
		}
		mean := (total / time.Duration(len(bs)-1)).Seconds()
		if mean > iv*1.5+0.5 || mean < iv*0.5-0.2 {
			problems = append(problems, fmt.Sprintf("instance %s declares interval_s %v but beats every %.2fs on average", id, iv, mean))
		}
	}
	if judged == 0 {
		return na("fewer than two heartbeats were observed from any instance in the window; raise --heartbeat-window (the agent declares interval_s %v)", declaredInterval(beats[0].Data))
	}
	return verdict(problems, fmt.Sprintf("the cadence of %d instance(s) matches its interval_s", judged))
}

// ---- status (SAP §8.7) -------------------------------------------------------

func registerStatus(add adder) {
	needStatus := func(ctx context.Context, t *T) (*statusObs, *Outcome) {
		o := t.statusObs(ctx)
		if o.Err != nil {
			f := fail("the status endpoint did not answer: %v", o.Err)
			return o, &f
		}
		return o, nil
	}
	add(func(ctx context.Context, t *T) Outcome {
		if t.Status == nil {
			return fail("no status endpoint is registered")
		}
		if _, bad := needStatus(ctx, t); bad != nil {
			return *bad
		}
		return pass("the status endpoint answers on %s", t.Status.Subject)
	}, "SAP-8.7-01")

	add(func(ctx context.Context, t *T) Outcome {
		o, bad := needStatus(ctx, t)
		if bad != nil {
			return *bad
		}
		for i, r := range o.Replies {
			if _, isErr := r.Header[agents.HeaderServiceErrorCode]; isErr {
				return fail("request %d (body %q) was answered with an error", i, trunc(string(r.Sent), 30))
			}
			if p := t.hbProblems(r.Data, r.At); len(p) > 0 {
				return fail("request %d (body %q): %s", i, trunc(string(r.Sent), 30), joinProblems(p))
			}
		}
		return pass("a request with a non-empty body was answered like one without")
	}, "SAP-8.7-04")

	add(func(ctx context.Context, t *T) Outcome {
		o, bad := needStatus(ctx, t)
		if bad != nil {
			return *bad
		}
		if len(o.Replies) < 2 {
			return fail("fewer than two replies")
		}
		a, errA := decodeObject(o.Replies[0].Data)
		b, errB := decodeObject(o.Replies[1].Data)
		if errA != nil || errB != nil {
			return fail("a status reply is not a JSON object")
		}
		ta, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(a["ts"]))
		tb, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(b["ts"]))
		if !tb.After(ta) {
			return fail("ts did not advance between replies %s apart (%v then %v): the reply is not built fresh per request", o.Replies[1].At.Sub(o.Replies[0].At).Round(10*time.Millisecond), a["ts"], b["ts"])
		}
		if p := tsProblems(beatPayload{b}, o.Replies[1].At, t.cfg.ClockSkew); len(p) > 0 {
			return fail("%s", joinProblems(p))
		}
		return pass("ts advanced between two replies and is current")
	}, "SAP-8.7-06")

	add(func(ctx context.Context, t *T) Outcome {
		o, bad := needStatus(ctx, t)
		if bad != nil {
			return *bad
		}
		var problems []string
		if len(o.Extras) > 0 {
			problems = append(problems, fmt.Sprintf("%d additional status reply(ies) came back for the same request: a second responder is answering on %s", len(o.Extras), t.Status.Subject))
		}
		for i, r := range append(append([]Reply(nil), o.Replies...), o.Extras...) {
			problems = append(problems, t.hbProblems(r.Data, r.At)...)
			obj, _ := decodeObject(r.Data)
			repInst := t.instFor(fmt.Sprint(obj["instance_id"]))
			if want, _ := repInst.MetaString(agents.MetaAgent); obj["agent"] != want {
				problems = append(problems, fmt.Sprintf("reply %d: agent %v differs from metadata.agent", i, obj["agent"]))
			}
			if id, _ := obj["instance_id"].(string); !t.instanceIDs()[id] {
				problems = append(problems, fmt.Sprintf("reply %d: instance_id %q is not a registered service id", i, id))
			}
			if v, present := obj["protocol_version"]; present {
				if want, _ := repInst.MetaString(agents.MetaProtocolVersion); v != want {
					problems = append(problems, fmt.Sprintf("reply %d: protocol_version %v differs from the metadata", i, v))
				}
			}
		}
		return verdict(uniqueStrings(problems), "the status reply has the heartbeat schema")
	}, "SAP-8.7-07")
}

// ---- small helpers ------------------------------------------------------------

func mustObj(b []byte) map[string]any {
	o, err := decodeObject(b)
	if err != nil {
		return map[string]any{}
	}
	return o
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
