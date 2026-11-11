// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/identity"
)

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func registerExtras(add adder) {
	registerSession(add)
	registerInstances(add)
	registerLimits(add)
	registerQueries(add)

	notObserved := "needs an account layout (an export that inserts an account token, or an import that renames subjects) that a runner holding one NATS connection cannot set up; not implemented in this version"
	add(func(context.Context, *T) Outcome { return notRun("%s", notObserved) },
		"SAP-13.7.1-03", "SAP-13.7.1-07", "SAP-13.6.2-04")
}

// ---- session ------------------------------------------------------------------

func registerSession(add adder) {
	each := perEach(add)
	each(func(_ context.Context, t *T) Outcome {
		if !*t.cfg.SessionAware {
			return na("the operator says the harness is session-less")
		}
		s, ok := t.Inst.MetaString(agents.MetaSession)
		if !ok || s == "" {
			return fail("the harness is session-aware and metadata.session is not set")
		}
		return pass("metadata.session = %q", s)
	}, "SAP-3.2-06")

	each(func(_ context.Context, t *T) Outcome {
		if *t.cfg.SessionAware {
			return na("the operator says the harness is session-aware")
		}
		s, ok := t.Inst.MetaString(agents.MetaSession)
		if !ok || s == "default" {
			return pass("a session-less harness omits metadata.session or sets it to default")
		}
		return policy("a session-less harness sets metadata.session to %q", s)
	}, "SAP-3.2-07")

	add(func(ctx context.Context, t *T) Outcome {
		beats, bad := t.beatsOrFail(ctx)
		if bad != nil {
			return *bad
		}
		for i, b := range beats {
			obj := mustObj(b.Data)
			want, set := t.instFor(b.InstanceID).MetaString(agents.MetaSession)
			got, present := obj["session"]
			switch {
			case set && (!present || got != want):
				return fail("heartbeat %d: session is %v, metadata.session is %q", i, got, want)
			case !set && present:
				return fail("heartbeat %d carries session %v although metadata.session is not set", i, got)
			}
		}
		return pass("session is present on a heartbeat if and only if metadata.session is set")
	}, "SAP-8.3-03")
}

// ---- multiple instances ---------------------------------------------------------

func registerInstances(add adder) {
	add(func(_ context.Context, t *T) Outcome {
		if n := len(t.Insts); n < t.cfg.ExpectInstances {
			return fail("%d instance(s) registered, the operator started %d", n, t.cfg.ExpectInstances)
		}
		return pass("%d instances of one logical agent are registered at once", len(t.Insts))
	}, "SAP-3.4-01")

	add(func(_ context.Context, t *T) Outcome {
		if n := len(t.Insts); n < t.cfg.ExpectInstances {
			return fail("%d instance(s) registered, the operator started %d", n, t.cfg.ExpectInstances)
		}
		var problems []string
		for _, in := range t.Insts {
			for _, name := range []string{agents.EndpointPrompt, agents.EndpointStatus} {
				ep := in.Endpoint(name)
				switch {
				case ep == nil:
					problems = append(problems, fmt.Sprintf("instance %s has no %s endpoint", in.ID, name))
				case ep.Subject != t.Inst.Endpoint(name).Subject:
					problems = append(problems, fmt.Sprintf("instance %s serves %s on %s", in.ID, name, ep.Subject))
				case ep.QueueGroup != agents.QueueGroup:
					problems = append(problems, fmt.Sprintf("instance %s has queue group %q on %s", in.ID, ep.QueueGroup, name))
				}
			}
		}
		return verdict(problems, fmt.Sprintf("%d instances share identical endpoint subjects and the agents queue group; the load balancing itself is the NATS server's and cannot be told from outside", len(t.Insts)))
	}, "SAP-3.4-02")

	add(func(_ context.Context, t *T) Outcome {
		if len(t.Insts) < 2 {
			return fail("only %d instance registered", len(t.Insts))
		}
		var problems []string
		base := canonicalInstance(t.Inst)
		for _, in := range t.Insts[1:] {
			if c := canonicalInstance(in); c != base {
				problems = append(problems, fmt.Sprintf("instance %s differs from %s in its endpoints or metadata", in.ID, t.Inst.ID))
			}
		}
		return verdict(problems, "all instances expose identical endpoints and metadata, apart from per-instance identity keys")
	}, "SAP-3.4-04")
}

func canonicalInstance(in *Instance) string {
	meta := map[string]any{}
	for k, v := range in.Meta {
		switch k {
		case identity.MetadataKeyUserNKey, identity.MetadataKeyAccount, identity.MetadataKeyIDSig:
			continue // identity keys differ per instance by design; not a responder
		}
		meta[k] = v
	}
	eps := make([]map[string]any, 0, len(in.Endpoints))
	for _, e := range in.Endpoints {
		eps = append(eps, map[string]any{"n": e.Name, "s": e.Subject, "q": e.QueueGroup, "m": e.Meta})
	}
	sort.Slice(eps, func(i, j int) bool { return fmt.Sprint(eps[i]["n"]) < fmt.Sprint(eps[j]["n"]) })
	b, _ := json.Marshal(map[string]any{"meta": meta, "eps": eps})
	return string(b)
}

// ---- oversize and slow ----------------------------------------------------------

func registerLimits(add adder) {
	oversize := func(ctx context.Context, t *T) (*Stream, *Outcome) {
		if t.Prompt == nil {
			o := fail("no prompt endpoint")
			return nil, &o
		}
		mp, ok := t.Prompt.Meta[agents.MetaMaxPayload].(string)
		if !ok {
			o := fail("max_payload is missing or not a string")
			return nil, &o
		}
		n, err := agents.ParseSize(mp)
		if err != nil {
			o := fail("max_payload %q is malformed", mp)
			return nil, &o
		}
		if limit := int64(t.nc.MaxPayload()); n+1 > limit {
			o := na("the declared max_payload (%d bytes) is not below the server's own limit (%d), so a request over it cannot be sent", n, limit)
			return nil, &o
		}
		if s, ok := t.streams["oversize"]; ok {
			if s.NeedSeed != "" {
				o := Outcome{ConfigMissing, s.NeedSeed}
				return nil, &o
			}
			return t.reuse(s), nil
		}
		s := t.send(ctx, "oversize", bytes.Repeat([]byte("a"), int(n)+1), collectOpts{})
		t.streams["oversize"] = s
		if s.NeedSeed != "" {
			o := Outcome{ConfigMissing, s.NeedSeed}
			return nil, &o
		}
		return s, nil
	}
	add(func(ctx context.Context, t *T) Outcome {
		s, o := oversize(ctx, t)
		if o != nil {
			return *o
		}
		switch code := s.Code(); code {
		case "400":
			return pass("a request one byte over max_payload was answered 400")
		case "":
			// Server-side enforcement is optional (SAP:L288-L293), and the 400 row
			// of the error table says what the code is if the error is reported.
			return policy("the host does not report an error for a request over max_payload (%s); enforcing it is optional", s.Describe())
		default:
			return fail("a request over max_payload was answered with error %s, not 400", code)
		}
	}, "SAP-9.2-02")
	add(func(ctx context.Context, t *T) Outcome {
		s, o := oversize(ctx, t)
		if o != nil {
			return *o
		}
		if s.Code() == "400" {
			return pass("the host enforces max_payload itself and answers 400")
		}
		return policy("the host does not enforce max_payload itself (%s); the spec makes that optional", s.Describe())
	}, "SAP-5.4-04")

	// The obligation (SAP:L380) is about order: the ack is the first message,
	// before any response or query chunk. From outside, the runner can see that
	// order and when each message arrived; it cannot see when the host began
	// its work, and the spec fixes no duration. Timing is therefore a diagnostic
	// in the detail line (AckDeadline), never a verdict. The host-side proof that
	// the ack precedes slow work uses a controlled slow-work fixture in the
	// tests, not this check.
	add(func(ctx context.Context, t *T) Outcome {
		if t.Prompt == nil {
			return fail("no prompt endpoint")
		}
		s := t.send(ctx, "slow", []byte(t.cfg.SlowPrompt), collectOpts{})
		if s.NeedSeed != "" {
			return Outcome{ConfigMissing, s.NeedSeed}
		}
		if len(s.Msgs) == 0 || !isAck(s.Msgs[0]) {
			return fail("the first message of a slow request is not the ack: %s", s.Describe())
		}
		ackAfter := s.FirstLatency().Round(time.Millisecond)
		note := ""
		for i, m := range s.Msgs {
			if i > 0 && !m.IsTerminator() {
				note = fmt.Sprintf("; the first content followed after %s", m.At.Sub(s.Sent).Round(time.Millisecond))
				break
			}
		}
		diag := ""
		if s.FirstLatency() > t.cfg.AckDeadline {
			diag = fmt.Sprintf(" (diagnostic only: that is over the %s --ack-deadline; the spec fixes no duration, and the runner cannot see when the host began its work)", t.cfg.AckDeadline)
		}
		return pass("the ack was the first message, %s after the request%s%s", ackAfter, note, diag)
	}, "SAP-6.4-04")
}

// ---- mid-stream queries -----------------------------------------------------------

type query struct {
	ID, ReplySubject, Prompt string
	Attachments              json.RawMessage
	problems                 []string
}

type queryObs struct {
	Stream  *Stream
	Queries []query
	// Answered is how many queries the runner answered; Unanswerable is how many
	// it did not, because no --answer-query-subject-prefix covered their
	// reply_subject.
	Answered     int
	Unanswerable int
	// Dropped counts query chunks past maxQueriesKept; the checks that read the
	// queries fail with it, so none is silently dropped.
	Dropped int
	// NeedSeed is why the query probe could not be sent: the endpoint needs a
	// signed sender and the runner has no usable seed.
	NeedSeed string
}

// answerable reports whether the operator allowed the runner to publish to a
// query's reply_subject. The agent chose that subject, and the runner publishes
// with the operator's credentials, so it needs the operator's say-so.
func (t *T) answerable(subject string) bool {
	if subject == "" || agents.ValidatePublishSubject(subject) != nil {
		return false
	}
	for _, p := range t.cfg.AnswerQuerySubjectPrefixes {
		if p != "" && strings.HasPrefix(subject, p) {
			return true
		}
	}
	return false
}

// queryRun runs the query prompt once and records the queries the agent asks.
// With answer set and at least one --answer-query-subject-prefix, it answers
// the queries whose reply_subject falls under a prefix; without a prefix there
// is only one kind of run, the one that answers nothing.
func (t *T) queryRun(ctx context.Context, answer bool) *queryObs {
	answer = answer && len(t.cfg.AnswerQuerySubjectPrefixes) > 0
	key := "query-unanswered"
	if answer {
		key = "query-answered"
	}
	if o, ok := t.queryObs[key]; ok {
		if o.Stream != nil && o.Stream.Over != "" {
			t.noteOver(o.Stream)
		}
		t.noteDropped(o)
		return o
	}
	obs := &queryObs{}
	t.queryObs[key] = obs
	if t.Prompt == nil {
		obs.Stream = &Stream{Name: key, Term: -1, End: "error"}
		return obs
	}
	timeout := t.cfg.StreamTimeout
	if !answer {
		timeout = t.cfg.QueryUnansweredWait
	}
	payload := []byte(t.cfg.QueryPrompt)
	obs.Stream = t.send(ctx, key, payload, collectOpts{
		timeout: timeout,
		onMsg: func(m *nats.Msg) {
			c, err := parseChunk(m.Data)
			if err != nil || c.Type != agents.ChunkQuery {
				// Not a query chunk: nothing to record here. Every message of the
				// stream is judged by SAP-6.2-01/02, so no responder goes unjudged.
				return
			}
			var q query
			var raw map[string]json.RawMessage
			if json.Unmarshal(c.Data, &raw) != nil {
				q.problems = append(q.problems, "data is not an object")
			} else {
				get := func(k string, dst *string) {
					v, ok := raw[k]
					if !ok || json.Unmarshal(v, dst) != nil {
						q.problems = append(q.problems, k+" is missing or not a string")
					}
				}
				get("id", &q.ID)
				get("reply_subject", &q.ReplySubject)
				get("prompt", &q.Prompt)
				q.Attachments = raw["attachments"]
			}
			if len(obs.Queries) < maxQueriesKept {
				obs.Queries = append(obs.Queries, q)
			} else {
				obs.Dropped++
			}
			switch {
			case !answer:
			case t.answerable(q.ReplySubject):
				_ = t.nc.PublishMsg(&nats.Msg{Subject: q.ReplySubject, Data: []byte("yes")})
				_ = t.nc.Flush()
				obs.Answered++
			default:
				obs.Unanswerable++
			}
		},
	})
	obs.NeedSeed = obs.Stream.NeedSeed
	t.noteDropped(obs)
	return obs
}

// noteDropped fails the running check when query chunks went unrecorded.
func (t *T) noteDropped(o *queryObs) {
	if o.Dropped > 0 {
		t.curOver = append(t.curOver, fmt.Sprintf("queries: %d query chunk(s) past the first %d were not recorded", o.Dropped, maxQueriesKept))
	}
}

// maxQueriesKept bounds the queries one stream may record; the stream's own
// message cap stops a flood before this matters.
const maxQueriesKept = 100

// needAnswer returns configuration-missing when a check needs the runner to have
// answered a query and it did not: the operator gave no prefix covering the
// agent's reply_subject.
func needAnswer(o *queryObs) *Outcome {
	if o.Answered > 0 || len(o.Queries) == 0 {
		return nil
	}
	r := Outcome{ConfigMissing, "the runner observes queries and answers none unless --answer-query-subject-prefix covers the agent's reply_subject; none did (" + fmt.Sprintf("%d unanswerable", o.Unanswerable) + ")"}
	return &r
}

func registerQueries(add adder) {
	noQuery := func(o *queryObs) *Outcome {
		if o.NeedSeed != "" {
			r := Outcome{ConfigMissing, o.NeedSeed}
			return &r
		}
		if len(o.Queries) == 0 {
			r := na("the agent emitted no query for --query-prompt (%s)", o.Stream.Describe())
			return &r
		}
		return nil
	}
	each := func(ok string, check func(q query) []string) CheckFunc {
		return func(ctx context.Context, t *T) Outcome {
			o := t.queryRun(ctx, true)
			if r := noQuery(o); r != nil {
				return *r
			}
			var problems []string
			for i, q := range o.Queries {
				for _, p := range check(q) {
					problems = append(problems, fmt.Sprintf("query %d: %s", i, p))
				}
			}
			return verdict(problems, fmt.Sprintf("%s (%d query chunk(s))", ok, len(o.Queries)))
		}
	}
	add(func(ctx context.Context, t *T) Outcome {
		o := t.queryRun(ctx, true)
		if r := noQuery(o); r != nil {
			return *r
		}
		return pass("the agent paused its stream with %d query chunk(s)", len(o.Queries))
	}, "SAP-7-01")
	add(each("every query has type query and string id, reply_subject and prompt", func(q query) []string { return q.problems }), "SAP-7.1-01")
	add(each("every query id is a UUID", func(q query) []string {
		if !uuidRE.MatchString(q.ID) {
			return []string{fmt.Sprintf("id %q is not a UUID", q.ID)}
		}
		return nil
	}), "SAP-7.1-02")
	add(func(ctx context.Context, t *T) Outcome {
		o := t.queryRun(ctx, true)
		if r := noQuery(o); r != nil {
			return *r
		}
		seen := map[string]bool{}
		var problems []string
		for i, q := range o.Queries {
			switch {
			case q.ReplySubject == "" || agents.ValidatePublishSubject(q.ReplySubject) != nil:
				problems = append(problems, fmt.Sprintf("query %d: reply_subject %q is not a subject to publish to", i, q.ReplySubject))
			case seen[q.ReplySubject]:
				problems = append(problems, fmt.Sprintf("query %d reuses reply_subject %q", i, q.ReplySubject))
			case q.ReplySubject == t.promptSubj():
				problems = append(problems, fmt.Sprintf("query %d: reply_subject is the prompt subject", i))
			}
			seen[q.ReplySubject] = true
		}
		return verdict(problems, "every query names a fresh subject; that the agent expects exactly one reply on it is not observable")
	}, "SAP-7.1-03")
	add(each("an attachments array on a query has the request shape", func(q query) []string {
		if len(q.Attachments) == 0 {
			return nil
		}
		var atts []struct {
			Filename *string `json:"filename"`
			Content  *string `json:"content"`
		}
		if json.Unmarshal(q.Attachments, &atts) != nil {
			return []string{"attachments is not an array of objects"}
		}
		var p []string
		for i, a := range atts {
			if a.Filename == nil || a.Content == nil {
				p = append(p, fmt.Sprintf("attachment %d lacks a string filename or content", i))
			} else if _, ok := agents.Base64DecodedSize(*a.Content); !ok {
				p = append(p, fmt.Sprintf("attachment %d content is not strict base64", i))
			}
		}
		return p
	}), "SAP-7.1-04")
	add(func(ctx context.Context, t *T) Outcome {
		o := t.queryRun(ctx, true)
		if r := noQuery(o); r != nil {
			return *r
		}
		if r := needAnswer(o); r != nil {
			return *r
		}
		if o.Stream.Term < 0 {
			return fail("after the query was answered the stream did not reach a terminator: %s", o.Stream.Describe())
		}
		return pass("the stream continued past the query and ended with the terminator")
	}, "SAP-7.3-05")
	add(func(ctx context.Context, t *T) Outcome {
		o := t.queryRun(ctx, true)
		if r := noQuery(o); r != nil {
			return *r
		}
		if len(o.Queries) < 2 {
			if r := needAnswer(o); r != nil {
				return *r
			}
			return na("the agent emitted one query, so distinct reply subjects cannot be compared")
		}
		seen := map[string]bool{}
		for _, q := range o.Queries {
			if seen[q.ReplySubject] {
				return fail("reply_subject %q is used by more than one query", q.ReplySubject)
			}
			seen[q.ReplySubject] = true
		}
		return pass("%d queries use %d distinct reply subjects", len(o.Queries), len(seen))
	}, "SAP-7.3-04")

	add(func(ctx context.Context, t *T) Outcome {
		o := t.queryRun(ctx, false)
		if r := noQuery(o); r != nil {
			return *r
		}
		if len(o.Queries) >= 2 {
			return pass("%d queries were in flight in one stream", len(o.Queries))
		}
		return na("the agent emitted one query before its own timeout")
	}, "SAP-7.3-03")

	unanswered := func(wantError bool) CheckFunc {
		return func(ctx context.Context, t *T) Outcome {
			o := t.queryRun(ctx, false)
			if r := noQuery(o); r != nil {
				return *r
			}
			s := o.Stream
			var errored, continued bool
			if _, idx, ok := s.ErrorMsg(); ok && s.Term > idx {
				errored = true
			}
			if !errored && s.Term >= 0 {
				for _, m := range s.Msgs {
					if c, err := parseChunk(m.Data); err == nil && c.Type == agents.ChunkResponse {
						continued = true
					}
				}
				continued = continued || s.Term >= 0
			}
			switch {
			case wantError && errored:
				return pass("the agent ended the stream with an error (%s) when the query went unanswered", s.Code())
			case !wantError && continued:
				return pass("the agent continued with a default when the query went unanswered")
			case errored || continued:
				return na("the agent chose the other option when the query went unanswered; both are permitted")
			}
			return na("the agent neither ended nor continued the stream within %s of the unanswered query", t.cfg.QueryUnansweredWait)
		}
	}
	add(unanswered(true), "SAP-7.3-01")
	add(unanswered(false), "SAP-7.3-02")
}
