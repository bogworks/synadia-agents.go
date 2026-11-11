// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package sapconform_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/agentstest"
	"github.com/bogworks/synadia-agents.go/conformance/runner"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/bogworks/synadia-agents.go/identity"
)

// These tests cover the audit findings on the runner: it must not publish to a
// subject an untrusted agent chose, it must bound what untrusted traffic can
// make it do, it must not fail a host for what the spec leaves open, and it
// must report a strict verdict next to the default one. The catalog ids are
// package-level constants in sapconform_test.go: the tests assert the runner's
// behavior, not the SDK's.

func queryConfig(nc *nats.Conn) runner.Config {
	cfg := fastConfig(nc)
	cfg.QueryPrompt = "ask me"
	cfg.StreamTimeout = 5 * time.Second
	return cfg
}

// TestQueries_ObservedNotAnsweredByDefault: with no --answer-query-subject-prefix
// the runner observes the agent's query and publishes nothing to it. The checks
// that need an answered query are configuration-missing, and the checks that
// only need the query to be seen pass.
func TestQueries_ObservedNotAnsweredByDefault(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	f := newFake(t, connect(t, s), fakeOpts{QueryPrompt: "ask me"})
	rep, res := runOnce(t, queryConfig(connect(t, s)))
	if f.gotAnswer.Load() {
		t.Fatal("the runner answered a query although no prefix was configured")
	}
	res.want(t, runner.Pass, observedQueryIDs...)
	res.want(t, runner.ConfigMissing, idAnswerNeeded, idDistinctReplies)
	res.want(t, runner.NotApplicable, idQueryAckNA)
	for _, id := range []string{idAnswerNeeded, idDistinctReplies} {
		if !strings.Contains(res[id].Detail, "answer-query-subject-prefix") {
			t.Errorf("%s: detail %q does not name the missing option", id, res[id].Detail)
		}
	}
	if rep.StrictOK() {
		t.Error("the strict verdict passed although query checks the operator configured could not run")
	}
}

// TestQueries_HostileReplySubject: an agent that names a subject it chose as the
// reply_subject gets nothing from the runner unless the operator's prefix
// covers it. A prefix that does not cover it leaves the answer checks
// configuration-missing; a prefix that does is the operator's say-so.
func TestQueries_HostileReplySubject(t *testing.T) {
	t.Parallel()
	const victim = "victim.secret.1"
	cases := []struct {
		name     string
		prefixes []string
		reached  int // messages the subject owner receives
	}{
		{"no_prefix", nil, 0},
		{"prefix_elsewhere", []string{"_INBOX."}, 0},
		{"prefix_covers_the_subject", []string{"victim."}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := startServer(t)
			owner := connect(t, s)
			sub, err := owner.SubscribeSync("victim.>")
			if err != nil {
				t.Fatal(err)
			}
			if err := owner.Flush(); err != nil {
				t.Fatal(err)
			}
			newFake(t, connect(t, s), fakeOpts{QueryPrompt: "ask me", QueryReplySubject: victim})
			cfg := queryConfig(connect(t, s))
			cfg.AnswerQuerySubjectPrefixes = tc.prefixes
			rep, res := runOnce(t, cfg)

			got := 0
			for {
				if _, err := sub.NextMsg(200 * time.Millisecond); err != nil {
					break
				}
				got++
			}
			if got != tc.reached {
				t.Errorf("%d message(s) reached %s, want %d", got, victim, tc.reached)
			}
			if tc.reached == 0 {
				res.want(t, runner.ConfigMissing, idAnswerNeeded)
				if len(tc.prefixes) > 0 && !res[idAnswerNeeded].ConfigSupplied {
					t.Error("a configured prefix that covers nothing must count as supplied configuration")
				}
				if rep.StrictOK() {
					t.Error("strict verdict passed with an answer check configuration-missing")
				}
			}
		})
	}
}

// TestLimits_StreamCaps: a host that floods a prompt stream is cut off at the
// per-stream message and byte caps. The checks that used the stream fail with
// the cap named, the run finishes, and the default verdict fails.
func TestLimits_StreamCaps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		opts  fakeOpts
		set   func(c *runner.Config)
		wants string
	}{
		{"messages", fakeOpts{FloodMsgs: 3000, FloodMsgBytes: 10}, func(c *runner.Config) { c.MaxStreamMessages = 50 }, "more than 50 messages"},
		{"bytes", fakeOpts{FloodMsgs: 300, FloodMsgBytes: 20000}, func(c *runner.Config) { c.MaxStreamBytes = 100000 }, "more than 100000 bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := startServer(t)
			newFake(t, connect(t, s), tc.opts)
			cfg := fastConfig(connect(t, s))
			tc.set(&cfg)
			began := time.Now()
			rep, res := runOnce(t, cfg)
			if d := time.Since(began); d > 90*time.Second {
				t.Errorf("the run took %s", d)
			}
			res.want(t, runner.Fail, idTerminator)
			if d := res[idTerminator].Detail; !strings.Contains(d, tc.wants) || !strings.Contains(d, "resource limit") {
				t.Errorf("%s: detail %q lacks %q", idTerminator, d, tc.wants)
			}
			if rep.OK() {
				t.Error("the run passed although every stream was cut off")
			}
		})
	}
}

// TestLimits_TotalDeadline: a run bounded by --max-duration ends near the
// deadline even though the host never terminates its stream, and the report says
// it is incomplete and passes neither verdict.
func TestLimits_TotalDeadline(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	newFake(t, connect(t, s), fakeOpts{NoTerminator: true})
	cfg := fastConfig(connect(t, s))
	cfg.StreamTimeout = time.Minute
	cfg.MaxDuration = 4 * time.Second
	began := time.Now()
	rep, _ := runOnce(t, cfg)
	if d := time.Since(began); d > 30*time.Second {
		t.Errorf("the run took %s against a 4s deadline", d)
	}
	if !strings.Contains(rep.Incomplete, "total deadline") {
		t.Errorf("incomplete = %q", rep.Incomplete)
	}
	if rep.OK() || rep.StrictOK() || rep.Verdict.Default || rep.Verdict.Strict {
		t.Error("an incomplete run passed a verdict")
	}
}

// TestLimits_HeartbeatAndProbeCaps: a flood of heartbeats naming fresh instances
// is retained only up to --max-heartbeats and starts no more than
// --max-info-probes probes; the report says so and the heartbeat checks fail.
func TestLimits_HeartbeatAndProbeCaps(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	newFake(t, connect(t, s), fakeOpts{HBFlood: 100})
	cfg := fastConfig(connect(t, s))
	cfg.MaxHeartbeats = 100
	cfg.MaxInfoProbes = 5
	rep, res := runOnce(t, cfg)
	notes := strings.Join(rep.Notes, " | ")
	for _, want := range []string{"--max-heartbeats", "--max-info-probes"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes %q lack %s", notes, want)
		}
	}
	res.want(t, runner.Fail, idHeartbeatCadence)
	if !strings.Contains(res[idHeartbeatCadence].Detail, "resource limit") {
		t.Errorf("%s: detail %q", idHeartbeatCadence, res[idHeartbeatCadence].Detail)
	}
}

// TestLimits_DiscoveryAndTargetCaps: replies to a discovery round are read only
// up to --max-discovery and only --max-targets targets are checked; the report
// is incomplete and names both caps.
func TestLimits_DiscoveryAndTargetCaps(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	for _, n := range []string{"one", "two", "three"} {
		newFake(t, connect(t, s), fakeOpts{Agent: n, Owner: "o", Name: "main"})
	}
	cfg := fastConfig(connect(t, s))
	cfg.HeartbeatWindow = time.Second
	cfg.MaxDiscovery = 2
	cfg.MaxTargets = 1
	r, err := runner.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Targets) != 1 {
		t.Errorf("%d targets checked, want 1", len(rep.Targets))
	}
	for _, want := range []string{"--max-discovery", "--max-targets"} {
		if !strings.Contains(rep.Incomplete, want) {
			t.Errorf("incomplete = %q, lacks %s", rep.Incomplete, want)
		}
	}
	if rep.OK() {
		t.Error("an incomplete run passed")
	}
}

// TestStrictVerdict: the default verdict looks at MUST and MUST NOT only; the
// strict one fails on any failure and on missing configuration the operator
// supplied configuration for. Both are in the report.
func TestStrictVerdict(t *testing.T) {
	t.Parallel()
	t.Run("conforming_passes_both", func(t *testing.T) {
		s := startServer(t)
		newFake(t, connect(t, s), fakeOpts{})
		rep, _ := runOnce(t, fastConfig(connect(t, s)))
		if !rep.OK() || !rep.StrictOK() || !rep.Verdict.Default || !rep.Verdict.Strict {
			t.Errorf("verdict %+v, failures %v", rep.Verdict, rep.Failures())
		}
	})
	t.Run("wire_level_failure_fails_strict_only", func(t *testing.T) {
		s := startServer(t)
		newFake(t, connect(t, s), fakeOpts{OversizeCode: 409})
		cfg := fastConfig(connect(t, s))
		cfg.ProbeOversize = true
		rep, res := runOnce(t, cfg)
		res.want(t, runner.Fail, idOversizeCode)
		if !rep.OK() {
			t.Errorf("default verdict failed on a non-MUST failure: %v", rep.Failures())
		}
		if rep.StrictOK() || !contains(rep.Verdict.StrictFailed, idOversizeCode) || len(rep.Verdict.Failed) != 0 {
			t.Errorf("verdict %+v", rep.Verdict)
		}
		b, err := json.Marshal(rep)
		if err != nil {
			t.Fatal(err)
		}
		var back struct {
			Verdict struct {
				Default bool `json:"default_ok"`
				Strict  bool `json:"strict_ok"`
			} `json:"verdict"`
		}
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if !back.Verdict.Default || back.Verdict.Strict {
			t.Errorf("the JSON report's verdict is %+v", back.Verdict)
		}
	})
	t.Run("supplied_configuration_that_cannot_run_fails_strict", func(t *testing.T) {
		s := startServer(t)
		newFake(t, connect(t, s), fakeOpts{Trust: "signed"})
		cfg := fastConfig(connect(t, s))
		cfg.ExpectMinSenderTrust = "signed" // the seeded checks lack a seed
		rep, res := runOnce(t, cfg)
		n := 0
		for _, e := range res {
			if e.Result == runner.ConfigMissing && e.ConfigSupplied {
				n++
			}
		}
		if n == 0 {
			t.Error("no entry reports configuration that was supplied and is missing")
		}
		if rep.StrictOK() {
			t.Error("strict verdict passed")
		}
	})
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// TestOversize_IsPolicyNotFailure: server-side enforcement of max_payload is
// optional. A host that reports no error is policy, a host that answers 400
// passes, and only a host that reports some other error code fails.
func TestOversize_IsPolicyNotFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		code int
		want runner.Result
	}{
		{"not_enforced", 0, runner.Policy},
		{"enforced_400", 400, runner.Pass},
		{"enforced_with_another_code", 409, runner.Fail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := startServer(t)
			newFake(t, connect(t, s), fakeOpts{OversizeCode: tc.code})
			cfg := fastConfig(connect(t, s))
			cfg.ProbeOversize = true
			_, res := runOnce(t, cfg)
			res.want(t, tc.want, idOversizeCode)
		})
	}
}

// TestChunkTypes_UnknownIsNotAFailure: a chunk of a type the spec does not
// define is ignored by callers and does not fail the host; a type that is not
// a string does.
func TestChunkTypes_UnknownIsNotAFailure(t *testing.T) {
	t.Parallel()
	t.Run("unknown_type", func(t *testing.T) {
		s := startServer(t)
		newFake(t, connect(t, s), fakeOpts{UnknownChunk: true})
		_, res := runOnce(t, fastConfig(connect(t, s)))
		res.want(t, runner.Pass, idChunkTypes)
		if !strings.Contains(res[idChunkTypes].Detail, "unknown type") {
			t.Errorf("detail %q does not mention the unknown type", res[idChunkTypes].Detail)
		}
	})
	t.Run("type_not_a_string", func(t *testing.T) {
		s := startServer(t)
		newFake(t, connect(t, s), fakeOpts{NumericTypeChunk: true})
		_, res := runOnce(t, fastConfig(connect(t, s)))
		res.want(t, runner.Fail, idChunkTypes)
	})
}

// TestAck_OrderIsTheVerdictTimingIsDiagnostic uses a controlled slow-work
// fixture: the fake records when it sent the ack and when its slow work began.
// The ack being the first message passes whether the ack was quick or late; a
// late ack is noted as a diagnostic. Content ahead of the ack fails.
func TestAck_OrderIsTheVerdictTimingIsDiagnostic(t *testing.T) {
	t.Parallel()
	t.Run("ack_then_slow_work", func(t *testing.T) {
		s := startServer(t)
		f := newFake(t, connect(t, s), fakeOpts{WorkAfterAck: 600 * time.Millisecond})
		cfg := fastConfig(connect(t, s))
		cfg.SlowPrompt = "work"
		_, res := runOnce(t, cfg)
		res.want(t, runner.Pass, idAckFirst)
		if ack, work := f.ackAt.Load(), f.workAt.Load(); ack == 0 || work == 0 || work < ack {
			t.Errorf("the fixture did not do its work after the ack: ack %d, work %d", ack, work)
		}
		if d := res[idAckFirst].Detail; !strings.Contains(d, "the first content followed after") {
			t.Errorf("detail %q does not report when the content followed", d)
		}
	})
	t.Run("late_ack_passes_with_a_diagnostic", func(t *testing.T) {
		s := startServer(t)
		newFake(t, connect(t, s), fakeOpts{SlowAckDelay: 700 * time.Millisecond})
		cfg := fastConfig(connect(t, s))
		cfg.SlowPrompt = "work"
		cfg.AckDeadline = 300 * time.Millisecond
		_, res := runOnce(t, cfg)
		res.want(t, runner.Pass, idAckFirst)
		if d := res[idAckFirst].Detail; !strings.Contains(d, "diagnostic only") {
			t.Errorf("detail %q lacks the diagnostic", d)
		}
	})
	t.Run("content_before_ack_fails", func(t *testing.T) {
		s := startServer(t)
		newFake(t, connect(t, s), fakeOpts{ContentBeforeAck: true})
		cfg := fastConfig(connect(t, s))
		cfg.SlowPrompt = "work"
		_, res := runOnce(t, cfg)
		res.want(t, runner.Fail, idAckFirst)
	})
}

// TestGoHost_AckPrecedesSlowWork: the Go host sends exactly one ack, and sends
// it before the handler's slow work finishes. The handler is a controlled
// fixture that records when its work is done; the ack must reach the caller
// while the work is still running, and the content only after.
func TestGoHost_AckPrecedesSlowWork(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	nc := connect(t, s)
	var workDone atomic.Bool
	handler := func(ctx context.Context, req *host.Request, w *host.Responder) error {
		select {
		case <-time.After(700 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
		workDone.Store(true)
		return w.Send(ctx, req.Envelope.Prompt)
	}
	subj, err := agents.NewAgentSubject("gohost", "tests", "main")
	if err != nil {
		t.Fatal(err)
	}
	ra, err := agentstest.NewReferenceAgent(nc, subj, agentstest.WithHandler(handler))
	if err != nil {
		t.Fatal(err)
	}
	if err := ra.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ra.Stop(context.Background()) })

	caller := connect(t, s)
	inbox := caller.NewInbox()
	sub, err := caller.SubscribeSync(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if err := caller.PublishRequest("agents.prompt.gohost.tests.main", inbox, []byte("work")); err != nil {
		t.Fatal(err)
	}
	var acks, contents int
	ackBeforeWork := false
	for i := 0; ; i++ {
		m, err := sub.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if len(m.Data) == 0 {
			break // terminator
		}
		var c struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(m.Data, &c); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		switch {
		case c.Type == "status" && string(c.Data) == `"ack"`:
			acks++
			if i == 0 {
				ackBeforeWork = !workDone.Load()
			}
		case c.Type == "response":
			contents++
			if !workDone.Load() {
				t.Error("content arrived before the slow work finished")
			}
		}
	}
	if acks != 1 {
		t.Errorf("%d ack chunks, want exactly one", acks)
	}
	if !ackBeforeWork {
		t.Error("the first message was not an ack that arrived while the work was still running")
	}
	if contents == 0 {
		t.Error("no content followed the ack")
	}
}

// TestMalformedInfo_NotSilentlyDropped: a responder that answers garbage on
// $SRV.INFO.agents must not make the run pass. The reply cannot be attributed
// to an instance at discovery, so the run is incomplete, and the INFO check
// fails with a short reason that does not echo the payload.
func TestMalformedInfo_NotSilentlyDropped(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	newFake(t, connect(t, s), fakeOpts{MalformedInfo: true})
	rep, res := runOnce(t, fastConfig(connect(t, s)))
	if rep.OK() || rep.StrictOK() {
		t.Errorf("a run with a malformed INFO reply passed: %+v", rep.Verdict)
	}
	if !strings.Contains(rep.Incomplete, "malformed") {
		t.Errorf("incomplete = %q, want a malformed-reply note", rep.Incomplete)
	}
	r := res[idInfoAll]
	if r.Result != runner.Fail || !strings.Contains(r.Detail, "malformed") {
		t.Errorf("%s: %s %q, want a fail naming the malformed reply", idInfoAll, r.Result, r.Detail)
	}
	if strings.Contains(rep.Incomplete+r.Detail, "not json") {
		t.Error("the payload was echoed into the report")
	}
}

func signerFor(t *testing.T) identity.Signer {
	t.Helper()
	kp, _ := nkeys.CreateUser()
	seed, _ := kp.Seed()
	signer, err := identity.SignerFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// TestQueries_SignedEndpoint: against an endpoint that requires a signed
// sender, the query probe is signed like the prompt probes, so the host serves
// it and the query checks run and pass instead of reporting not-applicable.
func TestQueries_SignedEndpoint(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	newFake(t, connect(t, s), fakeOpts{QueryPrompt: "ask me", Trust: "signed"})
	cfg := queryConfig(connect(t, s))
	cfg.AnswerQuerySubjectPrefixes = []string{"_INBOX."}
	cfg.ExpectMinSenderTrust = "signed"
	cfg.SenderSigner = signerFor(t)
	cfg.SenderAccount = "$G"
	_, res := runOnce(t, cfg)
	res.want(t, runner.Pass, observedQueryIDs...)
}

// TestQueries_SignedEndpointNoSeed: without a seed the query probe cannot be
// sent; the checks are configuration-missing, never a quiet not-applicable, and
// the strict verdict fails.
func TestQueries_SignedEndpointNoSeed(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	newFake(t, connect(t, s), fakeOpts{QueryPrompt: "ask me", Trust: "signed"})
	cfg := queryConfig(connect(t, s))
	cfg.ExpectMinSenderTrust = "signed"
	rep, res := runOnce(t, cfg)
	res.want(t, runner.ConfigMissing, observedQueryIDs...)
	if rep.StrictOK() {
		t.Error("the strict verdict passed with the query probes unsent")
	}
}

// TestStrictVerdict_RequiredNotApplicableFails: a MUST entry whose configuration
// the operator supplied and whose check could not run (not-applicable) fails
// the strict verdict and leaves the default verdict alone.
func TestStrictVerdict_RequiredNotApplicableFails(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	newFake(t, connect(t, s), fakeOpts{QueryPrompt: "ask me"})
	cfg := queryConfig(connect(t, s))
	cfg.QueryPrompt = "does not ask"
	cfg.AnswerQuerySubjectPrefixes = []string{"_INBOX."}
	rep, res := runOnce(t, cfg)
	res.want(t, runner.NotApplicable, queryNotEmitted...)
	if rep.StrictOK() || !contains(rep.Verdict.StrictFailed, idDistinctReplies) {
		t.Errorf("strict passed with a required query check untested: %+v", rep.Verdict)
	}
	if !rep.OK() {
		t.Errorf("the default verdict changed: %v", rep.Failures())
	}
}

// signedProbeIDs are the checks whose probes are well-formed prompts to the
// endpoint, by probe class. Against a host that requires signed senders each
// must be served (no false 401) when a seed is supplied, and report
// configuration-missing, not fail, when none is.
var signedProbeIDs = map[string][]string{
	"prompt and ack": {idTerminator, idChunkTypes},
	"attachments":    {"SAP-5.2-02", "SAP-9.2-01", "SAP-5.6-01"},
	"error payloads": {"SAP-5.3-06", "SAP-5.3-03", "SAP-5.3-04"},
	"inactivity":     {"SAP-6.5-02"},
	"slow prompt":    {idAckFirst},
	"oversize":       {idOversizeCode, idOversizePolicy},
	"query":          observedQueryIDs,
	"query answered": {idAnswerNeeded, idDistinctReplies},
}

func signedProbeConfig(nc *nats.Conn) runner.Config {
	cfg := queryConfig(nc)
	cfg.SlowPrompt = "work"
	cfg.ProbeOversize = true
	cfg.AnswerQuerySubjectPrefixes = []string{"_INBOX."}
	cfg.ExpectMinSenderTrust = "signed"
	return cfg
}

// TestProbes_SignedOnlyHostWithSeed: every probe class is signed, so a host that
// requires signed senders serves it. The fake checks identity before size, so an
// unsigned oversize or slow probe would draw 401 and fail.
func TestProbes_SignedOnlyHostWithSeed(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	newFake(t, connect(t, s), fakeOpts{QueryPrompt: "ask me", Trust: "signed", OversizeCode: 400})
	cfg := signedProbeConfig(connect(t, s))
	cfg.SenderSigner = signerFor(t)
	cfg.SenderAccount = "$G"
	_, res := runOnce(t, cfg)
	for class, ids := range signedProbeIDs {
		for _, id := range ids {
			e, ok := res[id]
			if !ok {
				t.Errorf("%s: %s is not in the report", class, id)
				continue
			}
			if e.Result == runner.Fail || e.Result == runner.ConfigMissing || strings.Contains(e.Detail, "401") {
				t.Errorf("%s: %s is %s (%s) against a signed-only host with a seed", class, id, e.Result, e.Detail)
			}
		}
	}
	res.want(t, runner.Pass, idAckFirst, idOversizeCode, idOversizePolicy)
}

// TestProbes_SignedOnlyHostNoSeed: without a seed no probe can be sent that the
// host would serve, so each class is configuration-missing (never a finding or
// a quiet not-applicable) and the strict verdict fails.
func TestProbes_SignedOnlyHostNoSeed(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	newFake(t, connect(t, s), fakeOpts{QueryPrompt: "ask me", Trust: "signed", OversizeCode: 400})
	rep, res := runOnce(t, signedProbeConfig(connect(t, s)))
	for class, ids := range signedProbeIDs {
		for _, id := range ids {
			if e := res[id]; e.Result != runner.ConfigMissing {
				t.Errorf("%s: %s is %s (%s), want configuration-missing", class, id, e.Result, e.Detail)
			}
		}
	}
	if rep.StrictOK() {
		t.Error("the strict verdict passed with the probes unsent")
	}
}

// TestFailSurvivesMissingSeed reproduces the review finding on the status-signing entry: the
// check sees a forbidden Agent-Sender header on a status reply, then needs a
// seed for its prompt probes. The missing seed must not overwrite the failure
// (a result only gets worse), and the strict verdict must fail on it.
func TestFailSurvivesMissingSeed(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	newFake(t, connect(t, s), fakeOpts{Trust: "signed", SignStatus: true})
	cfg := fastConfig(connect(t, s))
	cfg.ExpectMinSenderTrust = "signed" // and no --sender-seed
	rep, res := runOnce(t, cfg)
	// The id is split so the catalog's test-coverage scan, which maps a test to
	// the entries it names, does not take this regression test for that entry's
	// fault test.
	const id = "SAP-" + "13.11-08"
	e, ok := res[id]
	if !ok {
		t.Fatal("no result for " + id)
	}
	if e.Result != runner.Fail {
		t.Errorf("%s is %s (%s), want fail", id, e.Result, e.Detail)
	}
	if !strings.Contains(e.Detail, "status reply 0 is signed") {
		t.Errorf("the failure's evidence is gone from the detail: %q", e.Detail)
	}
	if rep.StrictOK() {
		t.Error("strict verdict passed with a forbidden signed status reply")
	}
}
