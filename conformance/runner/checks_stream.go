// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	agents "github.com/bogworks/synadia-agents.go"
)

// servedProbes are the well-formed prompts. The attachment probe joins them
// only when the endpoint declares attachments_ok true.
func (t *T) servedProbes() []string {
	names := []string{"plain", "json", "wsjson", "bracket"}
	if ok, known := t.attachmentsOK(); known && ok {
		names = append(names, "att")
	}
	return names
}

// errorProbes are the malformed prompts the spec says to refuse with 400.
func (t *T) errorProbes() []string {
	names := []string{"e-empty", "e-badjson", "e-noprompt", "e-emptyprompt"}
	if ok, known := t.attachmentsOK(); known {
		if ok {
			names = append(names, "e-b64", "e-urlsafe", "e-ws", "e-nofilename")
		} else {
			names = append(names, "e-att")
		}
	}
	return names
}

func (t *T) runAll(ctx context.Context, names []string) []*Stream {
	out := make([]*Stream, 0, len(names))
	for _, n := range names {
		out = append(out, t.run(ctx, n))
	}
	return out
}

func registerPrompt(add adder) {
	served := func(name, what string) CheckFunc {
		return func(ctx context.Context, t *T) Outcome {
			s := t.run(ctx, name)
			if o, gap := seedGate(s); gap {
				return o
			}
			if !s.Served() {
				return fail("%s was not served: %s", what, s.Describe())
			}
			return pass("%s was served", what)
		}
	}
	add(served("plain", "a plain UTF-8 prompt"), "SAP-5.1-01")
	add(served("json", "a JSON envelope with a prompt"), "SAP-5.1-02", "SAP-5.3-02", "SAP-5.1-05", "SAP-5.1-06")
	add(served("wsjson", "a JSON envelope after leading whitespace"), "SAP-5.3-01")
	add(served("bracket", "a text prompt that starts with a bracket"), "SAP-5.3-05")
	add(chkUnknownFieldsTolerated, "SAP-5.6-01")

	must400 := func(name, what string) CheckFunc {
		return func(ctx context.Context, t *T) Outcome {
			s := t.run(ctx, name)
			if o, gap := seedGate(s); gap {
				return o
			}
			if s.Code() == "400" {
				return pass("%s was answered 400", what)
			}
			return fail("%s was not answered 400: %s", what, s.Describe())
		}
	}
	add(must400("e-empty", "a zero-byte payload"), "SAP-5.3-06")
	add(must400("e-badjson", "unparseable JSON"), "SAP-5.3-03")
	add(must400("e-noprompt", "a JSON object without a prompt"), "SAP-5.3-04")

	// Where the spec does not oblige the host to refuse, a host that serves the
	// request has made a policy choice and is not failing.
	optional400 := func(name, what string) CheckFunc {
		return func(ctx context.Context, t *T) Outcome {
			if ok, known := t.attachmentsOK(); name != "e-emptyprompt" && (!known || !ok) {
				return na("the endpoint does not accept attachments, so %s cannot be isolated from the attachments refusal", what)
			}
			s := t.run(ctx, name)
			if o, gap := seedGate(s); gap {
				return o
			}
			switch {
			case s.Code() == "400":
				return pass("%s was answered 400", what)
			case s.Served():
				return policy("the host served %s; the spec obliges the sender, not the host, to avoid it", what)
			case s.Code() != "":
				return policy("the host refused %s with %s, not 400; the spec obliges the sender, not the host, to avoid it (see the base64 and envelope entries for the 400 rule)", what, s.Code())
			}
			return fail("%s drew %s", what, s.Describe())
		}
	}
	add(optional400("e-nofilename", "an attachment without a filename"), "SAP-5.2-01")

	add(func(ctx context.Context, t *T) Outcome {
		ok, known := t.attachmentsOK()
		if !known {
			return na("attachments_ok is missing or malformed (see SAP-2.1-04)")
		}
		if ok {
			return na("the endpoint declares attachments_ok true, so it accepts attachments")
		}
		s := t.run(ctx, "e-att")
		if o, gap := seedGate(s); gap {
			return o
		}
		if s.Code() == "400" {
			return pass("a non-empty attachments array was answered 400")
		}
		return fail("a non-empty attachments array on an attachments_ok false endpoint drew %s, not 400", s.Describe())
	}, "SAP-5.1-04")

	add(func(ctx context.Context, t *T) Outcome {
		ok, known := t.attachmentsOK()
		if !known || !ok {
			return na("the endpoint does not declare attachments_ok true")
		}
		s := t.run(ctx, "e-b64")
		if o, gap := seedGate(s); gap {
			return o
		}
		if s.Code() == "400" {
			return pass("invalid base64 in an attachment was answered 400")
		}
		return fail("invalid base64 in an attachment drew %s, not 400", s.Describe())
	}, "SAP-9.2-01")

	add(func(ctx context.Context, t *T) Outcome {
		ok, known := t.attachmentsOK()
		if !known || !ok {
			return na("the endpoint does not declare attachments_ok true")
		}
		s := t.run(ctx, "att")
		if o, gap := seedGate(s); gap {
			return o
		}
		if !s.Served() {
			return fail("a valid standard-alphabet base64 attachment was not served: %s", s.Describe())
		}
		return pass("a valid standard-alphabet base64 attachment, with an unknown attachment field, was served")
	}, "SAP-5.2-02")
}

func chkUnknownFieldsTolerated(ctx context.Context, t *T) Outcome {
	var problems []string
	for _, n := range []string{"json", "att"} {
		if n == "att" {
			if ok, known := t.attachmentsOK(); !known || !ok {
				continue // the att probe is gated on attachments_ok; no reply is skipped
			}
		}
		s := t.run(ctx, n)
		if o, gap := seedGate(s); gap {
			return o
		}
		if !s.Served() {
			problems = append(problems, fmt.Sprintf("the %q probe, with unknown fields, drew %s", n, s.Describe()))
		}
	}
	return verdict(problems, "unknown top-level and attachment fields were tolerated")
}

// ---- stream shape -----------------------------------------------------------

// content returns the messages before the terminator, or all of them when
// there is none.
func (s *Stream) content() []Msg {
	end := len(s.Msgs)
	if s.Term >= 0 {
		end = s.Term
	}
	return s.Msgs[:end]
}

func isAck(m Msg) bool {
	c, err := parseChunk(m.Data)
	if err != nil || c.Type != agents.ChunkStatus {
		return false
	}
	var d string
	return json.Unmarshal(c.Data, &d) == nil && d == agents.StatusAck
}

// served returns the probes that were served end to end.
func (t *T) servedStreams(ctx context.Context) (all, ok []*Stream) {
	all = t.runAll(ctx, t.servedProbes())
	for _, s := range all {
		if s.Served() {
			ok = append(ok, s)
		}
	}
	return all, ok
}

// seedGate returns configuration-missing, and true, when any of the streams was
// never sent because the endpoint requires a signed sender and the runner has
// no usable seed. Every check that judges a probe stream calls it before it
// reads the stream, so a check never judges a probe that was not sent and the
// runner never has to rewrite a returned result.
func seedGate(streams ...*Stream) (Outcome, bool) {
	for _, s := range streams {
		if s != nil && s.NeedSeed != "" {
			return Outcome{ConfigMissing, s.NeedSeed}, true
		}
	}
	return Outcome{}, false
}

// errorGate is seedGate over every malformed probe, whether or not it drew an
// error response.
func (t *T) errorGate(ctx context.Context) (Outcome, bool) {
	return seedGate(t.runAll(ctx, t.errorProbes())...)
}

func (t *T) errorStreams(ctx context.Context) []*Stream {
	var out []*Stream
	for _, s := range t.runAll(ctx, t.errorProbes()) {
		if _, _, isErr := s.ErrorMsg(); isErr {
			out = append(out, s)
		}
	}
	return out
}

// perStream applies check to every probe that was sent, success or error. A
// stream that never reached a terminator is judged by what it did show.
func perStream(ctx context.Context, t *T, streams []*Stream, okNote string, check func(*Stream) []string) Outcome {
	if len(streams) == 0 {
		return fail("no stream to judge")
	}
	var problems []string
	for _, s := range streams {
		for _, p := range check(s) {
			problems = append(problems, s.Name+": "+p)
		}
	}
	return verdict(problems, okNote)
}

// needServed fails a check that cannot judge a success stream because the host
// served none of the well-formed probes.
func needServed(t *T, all []*Stream) (Outcome, bool) {
	var parts []string
	for _, s := range all {
		parts = append(parts, s.Name+": "+s.Describe())
	}
	return fail("no well-formed prompt was served (%s)", strings.Join(parts, ", ")), false
}

func registerStream(add adder) {
	// SAP-6-01: chunks reach the request's reply subject.
	add(func(ctx context.Context, t *T) Outcome {
		probes := t.runAll(ctx, t.servedProbes())
		if o, gap := seedGate(probes...); gap {
			return o
		}
		for _, s := range probes {
			if len(s.Msgs) > 0 {
				return pass("the host published %d message(s) to the request's reply subject", len(s.Msgs))
			}
		}
		return fail("nothing arrived on the request's reply subject")
	}, "SAP-6-01")

	add(func(ctx context.Context, t *T) Outcome {
		all, ok := t.servedStreams(ctx)
		if o, gap := seedGate(all...); gap {
			return o
		}
		if len(ok) == 0 {
			o, _ := needServed(t, all)
			return o
		}
		// The spec defines three types and tells callers to ignore any other
		// (SAP:L338, L343), so an unknown type is not a host failure. The check is
		// that type is a string and that a message of a known type is well formed.
		unknown := 0
		out := perStream(ctx, t, ok, "every chunk type is a string, and every message of a known type is well formed", func(s *Stream) []string {
			var p []string
			for i, m := range s.content() {
				c, err := parseChunk(m.Data)
				if err != nil {
					if strings.Contains(err.Error(), "type is not a string") {
						p = append(p, fmt.Sprintf("message %d: %v", i, err))
					}
					continue // other shape faults belong to SAP-6.2-02
				}
				var bad string
				switch c.Type {
				case agents.ChunkResponse:
					var str string
					var obj struct {
						Text *string `json:"text"`
					}
					if json.Unmarshal(c.Data, &str) != nil && (json.Unmarshal(c.Data, &obj) != nil || obj.Text == nil) {
						bad = "a response chunk's data is neither a string nor an object with a string text"
					}
				case agents.ChunkStatus:
					var str string
					if json.Unmarshal(c.Data, &str) != nil {
						bad = "a status chunk's data is not a string"
					}
				case agents.ChunkQuery:
					var obj map[string]json.RawMessage
					if json.Unmarshal(c.Data, &obj) != nil || obj == nil {
						bad = "a query chunk's data is not an object"
					}
				default:
					unknown++
				}
				if bad != "" {
					p = append(p, fmt.Sprintf("message %d: %s", i, bad))
				}
			}
			return p
		})
		if out.Result == Pass && unknown > 0 {
			out = pass("%s; %d message(s) of an unknown type were seen, which callers ignore and the spec does not forbid", out.Detail, unknown)
		}
		return out
	}, "SAP-6.2-01")

	add(func(ctx context.Context, t *T) Outcome {
		all, ok := t.servedStreams(ctx)
		if o, gap := seedGate(all...); gap {
			return o
		}
		if len(ok) == 0 {
			o, _ := needServed(t, all)
			return o
		}
		return perStream(ctx, t, ok, "every non-terminating message is a JSON object with type and data", func(s *Stream) []string {
			var p []string
			for i, m := range s.content() {
				if _, err := parseChunk(m.Data); err != nil {
					p = append(p, fmt.Sprintf("message %d (%q): %v", i, trunc(string(m.Data), 40), err))
				}
			}
			return p
		})
	}, "SAP-6.2-02")

	add(chkErrorsViaHeaders, "SAP-6.2-04")

	add(func(ctx context.Context, t *T) Outcome {
		all, ok := t.servedStreams(ctx)
		if o, gap := seedGate(all...); gap {
			return o
		}
		if len(ok) == 0 {
			o, _ := needServed(t, all)
			return o
		}
		return perStream(ctx, t, ok, "every response chunk's data is a string or an object with a string text", func(s *Stream) []string {
			var p []string
			for i, m := range s.content() {
				c, err := parseChunk(m.Data)
				if err != nil || c.Type != agents.ChunkResponse {
					continue // a malformed or non-response message is judged by SAP-6.2-01/02
				}
				var str string
				if json.Unmarshal(c.Data, &str) == nil {
					continue // a string chunk is valid; only other shapes are judged below
				}
				var obj struct {
					Text *string `json:"text"`
				}
				if json.Unmarshal(c.Data, &obj) != nil || obj.Text == nil {
					p = append(p, fmt.Sprintf("message %d: data is neither a string nor an object with a string text", i))
				}
			}
			return p
		})
	}, "SAP-6.3-01")

	add(func(ctx context.Context, t *T) Outcome {
		all, ok := t.servedStreams(ctx)
		if o, gap := seedGate(all...); gap {
			return o
		}
		for _, s := range ok {
			n := 0
			for _, m := range s.content() {
				if c, err := parseChunk(m.Data); err == nil && c.Type == agents.ChunkResponse {
					n++
				}
			}
			if n > 1 {
				return pass("a stream carried %d response chunks", n)
			}
		}
		return na("the host answered each probe with at most one response chunk")
	}, "SAP-6.3-04")

	add(func(ctx context.Context, t *T) Outcome {
		all, ok := t.servedStreams(ctx)
		if o, gap := seedGate(all...); gap {
			return o
		}
		if len(ok) == 0 {
			// Judge what arrived anyway: an ack that is not first is a failure even
			// when the stream never terminated.
			if o, bad := firstMessageProblems(all); bad {
				return o
			}
			o, _ := needServed(t, all)
			return o
		}
		return perStream(ctx, t, ok, "exactly one ack per stream", func(s *Stream) []string {
			n := 0
			for _, m := range s.content() {
				if isAck(m) {
					n++
				}
			}
			if n != 1 {
				return []string{fmt.Sprintf("%d ack status chunk(s), want exactly one", n)}
			}
			return nil
		})
	}, "SAP-6.4-02")

	add(func(ctx context.Context, t *T) Outcome {
		all, _ := t.servedStreams(ctx)
		if o, gap := seedGate(all...); gap {
			return o
		}
		var seen []*Stream
		for _, s := range all {
			if len(s.Msgs) > 0 && s.Code() == "" {
				seen = append(seen, s)
			}
		}
		if len(seen) == 0 {
			o, _ := needServed(t, all)
			return o
		}
		return perStream(ctx, t, seen, "the ack was the first message of every stream", func(s *Stream) []string {
			if m := s.Msgs[0]; !isAck(m) {
				return []string{fmt.Sprintf("the first message is %q, not the ack", trunc(string(m.Data), 50))}
			}
			return nil
		})
	}, "SAP-6.4-03")

	add(func(ctx context.Context, t *T) Outcome {
		all, _ := t.servedStreams(ctx)
		if o, gap := seedGate(all...); gap {
			return o
		}
		if o, gap := t.errorGate(ctx); gap {
			return o
		}
		errs := t.errorStreams(ctx)
		streams := append(append([]*Stream{}, all...), errs...)
		return perStream(ctx, t, streams, "every stream ends with a zero-byte message that has no headers", func(s *Stream) []string {
			if s.Term >= 0 {
				return nil
			}
			if s.End == "no-responders" {
				return []string{"no responders"}
			}
			if n := len(s.Msgs); n > 0 {
				last := s.Msgs[n-1]
				if len(last.Data) == 0 {
					return []string{"the last message has an empty body but carries headers, so it is not the terminator"}
				}
			}
			return []string{fmt.Sprintf("no terminator within the inactivity timeout (%d message(s) seen)", len(s.Msgs))}
		})
	}, "SAP-6.5-01")

	add(func(ctx context.Context, t *T) Outcome {
		all, ok := t.servedStreams(ctx)
		if o, gap := seedGate(all...); gap {
			return o
		}
		if o, gap := t.errorGate(ctx); gap {
			return o
		}
		errs := t.errorStreams(ctx)
		if len(ok) == 0 {
			o, _ := needServed(t, all)
			return o
		}
		var problems []string
		for _, s := range append(append([]*Stream{}, ok...), errs...) {
			if s.Term < 0 {
				problems = append(problems, s.Name+": no terminator")
			}
		}
		for _, s := range errs {
			if s.Term != len(s.Msgs)-1-s.After || s.Term < 1 {
				problems = append(problems, s.Name+": the errored stream does not end with the terminator")
			}
		}
		return verdict(problems, fmt.Sprintf("%d successful and %d errored stream(s) end with the same terminator", len(ok), len(errs)))
	}, "SAP-6.5-02")

	add(func(ctx context.Context, t *T) Outcome {
		all, _ := t.servedStreams(ctx)
		if o, gap := seedGate(all...); gap {
			return o
		}
		if o, gap := t.errorGate(ctx); gap {
			return o
		}
		streams := append(append([]*Stream{}, all...), t.errorStreams(ctx)...)
		return perStream(ctx, t, streams, "nothing was published after any terminator", func(s *Stream) []string {
			if s.After > 0 {
				return []string{fmt.Sprintf("%d message(s) arrived after the terminator", s.After)}
			}
			return nil
		})
	}, "SAP-6.5-03", "SAP-9.3-02")

	add(func(ctx context.Context, t *T) Outcome {
		if o, gap := t.errorGate(ctx); gap {
			return o
		}
		errs := t.errorStreams(ctx)
		if len(errs) == 0 {
			return na("no probe drew an error response")
		}
		return perStream(ctx, t, errs, "every error completion is an error-headered message then the terminator", func(s *Stream) []string {
			n := 0
			for _, m := range s.Msgs {
				if _, ok := m.ErrorCode(); ok {
					n++
				}
			}
			var p []string
			if n != 1 {
				p = append(p, fmt.Sprintf("%d error-headered messages, want one", n))
			}
			if s.Term < 1 {
				return append(p, "no terminator after the error message")
			}
			if prev := s.Msgs[s.Term-1]; func() bool { _, ok := prev.ErrorCode(); return !ok }() {
				p = append(p, "the message before the terminator is not the error-headered message")
			}
			return p
		})
	}, "SAP-6.5-04", "SAP-9.3-01")

	add(func(ctx context.Context, t *T) Outcome {
		all, ok := t.servedStreams(ctx)
		if o, gap := seedGate(all...); gap {
			return o
		}
		if len(ok) == 0 {
			o, _ := needServed(t, all)
			return o
		}
		return perStream(ctx, t, ok, "every served stream is chunks then the terminator", func(s *Stream) []string {
			if len(s.content()) == 0 {
				return []string{"the stream has no chunk before the terminator"}
			}
			return nil
		})
	}, "SAP-6.5-05")

	registerErrorShape(add)
}

func firstMessageProblems(all []*Stream) (Outcome, bool) {
	for _, s := range all {
		if len(s.Msgs) > 0 && s.Code() == "" && !isAck(s.Msgs[0]) {
			return fail("%s: the first message is %q, not an ack", s.Name, trunc(string(s.Msgs[0].Data), 50)), true
		}
	}
	return Outcome{}, false
}

func chkErrorsViaHeaders(ctx context.Context, t *T) Outcome {
	all, _ := t.servedStreams(ctx)
	if o, gap := seedGate(all...); gap {
		return o
	}
	errAll := t.runAll(ctx, t.errorProbes())
	if o, gap := seedGate(errAll...); gap {
		return o
	}
	streams := append(append([]*Stream{}, all...), errAll...)
	var problems []string
	for _, s := range streams {
		for i, m := range s.content() {
			if _, hasHdr := m.ErrorCode(); hasHdr {
				continue // a headered message is the error form; the typed-chunk form is what this check reports
			}
			if c, err := parseChunk(m.Data); err == nil && c.Type == "error" {
				problems = append(problems, fmt.Sprintf("%s: message %d is a typed error chunk", s.Name, i))
			}
		}
	}
	if len(t.errorStreams(ctx)) == 0 {
		problems = append(problems, "no probe drew an error response to inspect")
	}
	return verdict(problems, "errors arrive as headered messages, never as a typed chunk")
}

// ---- error shape (SAP §9) ----------------------------------------------------

func registerErrorShape(add adder) {
	errorMsgs := func(ctx context.Context, t *T) []Msg {
		var out []Msg
		for _, s := range t.errorStreams(ctx) {
			if m, _, ok := s.ErrorMsg(); ok {
				out = append(out, m)
			}
		}
		return out
	}

	add(func(ctx context.Context, t *T) Outcome {
		if o, gap := t.errorGate(ctx); gap {
			return o
		}
		msgs := errorMsgs(ctx, t)
		if len(msgs) == 0 {
			return fail("no probe drew an error response to inspect")
		}
		var problems []string
		for i, m := range msgs {
			code, _ := m.ErrorCode()
			if n, err := strconv.Atoi(code); err != nil || strconv.Itoa(n) != code {
				problems = append(problems, fmt.Sprintf("error %d: code %q is not a numeric string", i, code))
			}
			if d := m.Header.Get(agents.HeaderServiceError); strings.TrimSpace(d) == "" {
				problems = append(problems, fmt.Sprintf("error %d: no %s description", i, agents.HeaderServiceError))
			}
		}
		return verdict(problems, fmt.Sprintf("%d error response(s) carry the code and description headers", len(msgs)))
	}, "SAP-9.1-01")

	bodies := func(ctx context.Context, t *T) (empty, jsonN int, objs []map[string]any, bad []string) {
		for _, m := range errorMsgs(ctx, t) {
			if len(m.Data) == 0 {
				empty++
				continue
			}
			o, err := decodeObject(m.Data)
			if err != nil {
				// Not JSON: callers fall back to the header, which the spec allows
				// them to do; nothing for the host to answer for.
				continue
			}
			jsonN++
			objs = append(objs, o)
		}
		return
	}

	add(func(ctx context.Context, t *T) Outcome {
		if o, gap := t.errorGate(ctx); gap {
			return o
		}
		empty, _, _, _ := bodies(ctx, t)
		if empty > 0 {
			return pass("%d error response(s) have an empty body", empty)
		}
		return na("the host's error responses all carry a body")
	}, "SAP-9.1-02")
	add(func(ctx context.Context, t *T) Outcome {
		if o, gap := t.errorGate(ctx); gap {
			return o
		}
		_, n, _, _ := bodies(ctx, t)
		if n > 0 {
			return pass("%d error response(s) carry a JSON object body", n)
		}
		return na("the host's error responses carry no JSON body")
	}, "SAP-9.1-03")
	add(func(ctx context.Context, t *T) Outcome {
		if o, gap := t.errorGate(ctx); gap {
			return o
		}
		_, _, objs, _ := bodies(ctx, t)
		if len(objs) == 0 {
			return na("no error response carried a JSON body")
		}
		var problems []string
		for i, o := range objs {
			if _, ok := o["error"].(string); !ok {
				problems = append(problems, fmt.Sprintf("JSON error body %d has no string error field", i))
			}
		}
		return verdict(problems, "every JSON error body has a string error field")
	}, "SAP-9.1-04")
	add(func(ctx context.Context, t *T) Outcome {
		if o, gap := t.errorGate(ctx); gap {
			return o
		}
		_, _, objs, _ := bodies(ctx, t)
		for _, o := range objs {
			for k := range o {
				if k != "error" && k != "message" {
					return pass("an error body carries the additional field %q", k)
				}
			}
		}
		return na("no error body carried fields beyond error and message")
	}, "SAP-9.1-06")

	add(func(ctx context.Context, t *T) Outcome {
		if o, gap := t.errorGate(ctx); gap {
			return o
		}
		msgs := errorMsgs(ctx, t)
		for _, s := range t.identityErrorStreams() {
			if m, _, ok := s.ErrorMsg(); ok {
				msgs = append(msgs, m)
			}
		}
		if len(msgs) == 0 {
			return fail("no probe drew an error response to inspect")
		}
		var problems []string
		for _, m := range msgs {
			code, _ := m.ErrorCode()
			if n, err := strconv.Atoi(code); err != nil || !agents.KnownCode(n) {
				problems = append(problems, fmt.Sprintf("code %q is not in the SAP taxonomy", code))
			}
		}
		return verdict(problems, fmt.Sprintf("all %d error code(s) observed are in the taxonomy", len(msgs)))
	}, "SAP-9.2-03")
}
