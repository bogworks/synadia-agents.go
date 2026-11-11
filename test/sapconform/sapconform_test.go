// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package sapconform_test

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/bogworks/synadia-agents.go/conformance"
	"github.com/bogworks/synadia-agents.go/conformance/runner"
	"github.com/bogworks/synadia-agents.go/identity"
)

// fastConfig is a configuration tuned for fakes that answer at once.
func fastConfig(nc *nats.Conn) runner.Config {
	return runner.Config{
		Conn:                nc,
		DiscoverWait:        400 * time.Millisecond,
		HeartbeatWindow:     1500 * time.Millisecond,
		StreamTimeout:       1500 * time.Millisecond,
		PostTerminatorGrace: 200 * time.Millisecond,
		QueryUnansweredWait: 2 * time.Second,
		AckDeadline:         500 * time.Millisecond,
		Timing:              fastTiming(),
	}
}

// fastTiming is the runner's fixed waits shortened for in-process fakes that
// answer at once. FreshTSGap stays at its default: a heartbeat's ts has
// one-second precision, so a shorter gap would make SAP-8.7's freshness check
// vacuous.
func fastTiming() runner.Timing {
	return runner.Timing{
		FirstBeatInfoWait:   time.Second,
		InfoProbeTimeout:    time.Second,
		StatusTimeout:       2 * time.Second,
		PollInterval:        10 * time.Millisecond,
		DiscoverStallMin:    100 * time.Millisecond,
		AutoHeartbeatWindow: 2 * time.Second,
		AutoHeartbeatMin:    time.Second,
	}
}

type results map[string]runner.EntryResult

func runOnce(t *testing.T, cfg runner.Config) (*runner.Report, results) {
	t.Helper()
	r, err := runner.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rep, err := r.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Targets) != 1 {
		t.Fatalf("%d targets, want 1", len(rep.Targets))
	}
	res := results{}
	for _, e := range rep.Targets[0].Entries {
		res[e.ID] = e
	}
	return rep, res
}

func (r results) want(t *testing.T, result runner.Result, ids ...string) {
	t.Helper()
	for _, id := range ids {
		e, ok := r[id]
		if !ok {
			t.Errorf("%s: not in the report", id)
			continue
		}
		if e.Result != result {
			t.Errorf("%s: result %s (%s), want %s", id, e.Result, e.Detail, result)
		}
	}
}

func (r results) ids(result runner.Result) []string {
	var out []string
	for id, e := range r {
		if e.Result == result {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// TestRegistry_ChecksAreTheCatalogsChecks asserts REQ-013: the runner has no list
// of its own. Every black-box host or both-role entry has a registered check, and
// every registered check names a catalog entry the runner will actually run.
func TestRegistry_ChecksAreTheCatalogsChecks(t *testing.T) {
	cat, err := conformance.Default()
	if err != nil {
		t.Fatal(err)
	}
	reg := map[string]bool{}
	for _, id := range runner.Registered() {
		reg[id] = true
	}
	var missing []string
	for _, e := range runner.HostEntries(cat, conformance.CapBlackBox) {
		if !reg[e.ID] {
			missing = append(missing, e.ID)
		}
	}
	if len(missing) > 0 {
		t.Errorf("black-box host entries without a registered check: %v", missing)
	}
	for id := range reg {
		e, ok := cat.ByID(id)
		switch {
		case !ok:
			t.Errorf("check registered for %s, which is not in the catalog", id)
		case e.Role != conformance.RoleHost && e.Role != conformance.RoleBoth:
			t.Errorf("check registered for %s, role %s", id, e.Role)
		case e.Capability != conformance.CapBlackBox && e.Capability != conformance.CapNeedsCfg:
			t.Errorf("check registered for %s, capability %s: the runner would not run it", id, e.Capability)
		}
	}
	nc := runner.HostEntries(cat, conformance.CapNeedsCfg)
	checked := 0
	for _, e := range nc {
		if reg[e.ID] {
			checked++
		}
	}
	t.Logf("registered checks %d; black-box host entries %d; needs-configuration host entries %d (%d with a check)",
		len(reg), len(runner.HostEntries(cat, conformance.CapBlackBox)), len(nc), checked)
}

// TestConforming_FakeHost: a minimal conforming fake passes every black-box MUST
// and MUST NOT it implements, and no MUST fails.
func TestConforming_FakeHost(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	newFake(t, connect(t, s), fakeOpts{})
	rep, res := runOnce(t, fastConfig(connect(t, s)))
	if !rep.OK() {
		for _, f := range rep.Failures() {
			t.Errorf("%s (%s, line %d): %s", f.ID, f.Level, f.SpecLine, f.Detail)
		}
	}
	for _, e := range res {
		if e.Capability == conformance.CapBlackBox && (e.Role == "host" || e.Role == "both") && e.Result == runner.Fail {
			t.Errorf("%s failed on a conforming fake (%s, %s): %s", e.ID, e.Level, e.Summary, e.Detail)
		}
		if e.Capability == conformance.CapBlackBox && e.Result == runner.NotRun && (e.Role == "host" || e.Role == "both") {
			t.Errorf("%s was not run: %s", e.ID, e.Detail)
		}
	}
	res.want(t, runner.Pass, conformingPass...)
	// Configuration-gated entries are configuration-missing without the config.
	res.want(t, runner.ConfigMissing, gatedIDs...)
	if res[conformingPass[0]].Capability != conformance.CapBlackBox {
		t.Error("capability must be the catalog's")
	}
	t.Logf("results (no configuration): pass %d, fail %d, policy %d, not-applicable %d, configuration-missing %d, not-run %d; duration %s",
		len(res.ids(runner.Pass)), len(res.ids(runner.Fail)), len(res.ids(runner.Policy)), len(res.ids(runner.NotApplicable)),
		len(res.ids(runner.ConfigMissing)), len(res.ids(runner.NotRun)), rep.Duration)
	t.Logf("not-applicable: %v", res.ids(runner.NotApplicable))
	t.Logf("policy: %v", res.ids(runner.Policy))
	if rep.Scope == "" || !strings.Contains(rep.Scope, "host obligations only") {
		t.Errorf("the report must state its scope, got %q", rep.Scope)
	}
}

// TestBroken_Fakes: each deliberately broken fake fails the catalog entry its
// fault breaks.
func TestBroken_Fakes(t *testing.T) {
	t.Parallel()
	for _, c := range brokenCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := startServer(t)
			newFake(t, connect(t, s), c.opts)
			cfg := fastConfig(connect(t, s))
			cfg.ExpectMinSenderTrust = c.trust
			rep, res := runOnce(t, cfg)
			res.want(t, runner.Fail, c.failID...)
			if rep.OK() {
				t.Errorf("the run passed although %v failed", c.failID)
			}
		})
	}
}

// TestHeartbeatNeverSent_NotReportedAsNotCheckable asserts AT-006: a host that
// publishes no heartbeat fails the heartbeat entries; none is left unchecked.
func TestHeartbeatNeverSent_NotReportedAsNotCheckable(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	newFake(t, connect(t, s), fakeOpts{NoHeartbeat: true})
	_, res := runOnce(t, fastConfig(connect(t, s)))
	for _, e := range res {
		if strings.HasPrefix(e.ID, heartbeatPayloadPrefix) && e.Capability == conformance.CapBlackBox && e.Result != runner.Fail {
			t.Errorf("%s: %s, want fail for a host with no heartbeat", e.ID, e.Result)
		}
	}
}

// TestConfiguration_SignedEndpoint: with the configuration, the section 13
// entries run and a conforming signed endpoint passes them.
func TestConfiguration_SignedEndpoint(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	kp, _ := nkeys.CreateUser()
	seed, _ := kp.Seed()
	signer, err := identity.SignerFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	kp2, _ := nkeys.CreateUser()
	seed2, _ := kp2.Seed()
	refused, _ := identity.SignerFromSeed(seed2)
	refusedPub, _ := kp2.PublicKey()
	newFake(t, connect(t, s), fakeOpts{Trust: "signed", RefuseUsers: map[string]bool{refusedPub: true}})

	cfg := fastConfig(connect(t, s))
	cfg.ExpectMinSenderTrust = "signed"
	cfg.SenderSigner = signer
	cfg.SenderAccount = "$G"
	cfg.RefusedSigner = refused
	rep, res := runOnce(t, cfg)
	for _, f := range rep.Failures() {
		t.Errorf("%s (%s): %s", f.ID, f.Level, f.Detail)
	}
	res.want(t, runner.Pass, signedPass...)
	t.Logf("section 13 results with configuration: pass %d, fail %d, policy %d, not-applicable %d, configuration-missing %d, not-run %d",
		len(res.ids(runner.Pass)), len(res.ids(runner.Fail)), len(res.ids(runner.Policy)), len(res.ids(runner.NotApplicable)),
		len(res.ids(runner.ConfigMissing)), len(res.ids(runner.NotRun)))
	t.Logf("configuration-missing even with this configuration: %v", res.ids(runner.ConfigMissing))
}

// TestConfiguration_Queries: a host that asks a query is checked on the
// round trip; the unanswered path follows the host's own choice.
func TestConfiguration_Queries(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	newFake(t, connect(t, s), fakeOpts{QueryPrompt: "ask me"})
	cfg := fastConfig(connect(t, s))
	cfg.QueryPrompt = "ask me"
	cfg.AnswerQuerySubjectPrefixes = []string{"_INBOX."}
	cfg.StreamTimeout = 5 * time.Second
	rep, res := runOnce(t, cfg)
	for _, f := range rep.Failures() {
		t.Errorf("%s: %s", f.ID, f.Detail)
	}
	res.want(t, runner.Pass, queryPass...)
	res.want(t, runner.NotApplicable, queryNA...)
}

// TestConfiguration_QueryNotEmitted: a host that never queries is not-applicable
// for the query entries, not a failure.
func TestConfiguration_QueryNotEmitted(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	newFake(t, connect(t, s), fakeOpts{})
	cfg := fastConfig(connect(t, s))
	cfg.QueryPrompt = "does not ask"
	cfg.AnswerQuerySubjectPrefixes = []string{"_INBOX."}
	_, res := runOnce(t, cfg)
	res.want(t, runner.NotApplicable, queryNotEmitted...)
}

// TestTargetSelection: tokens select one agent among several; an unknown token
// is an error.
func TestTargetSelection(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	newFake(t, connect(t, s), fakeOpts{Agent: "alpha", Owner: "o1", Name: "one"})
	newFake(t, connect(t, s), fakeOpts{Agent: "beta", Owner: "o2", Name: "two"})
	cfg := fastConfig(connect(t, s))
	cfg.HeartbeatWindow = time.Second
	cfg.Agent = "beta"
	rep, _ := runOnce(t, cfg)
	if got := rep.Targets[0].Target.PromptSubject; got != "agents.prompt.beta.o2.two" {
		t.Errorf("selected %q", got)
	}
	cfg.Agent, cfg.Owner = "alpha", "wrong"
	r, err := runner.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background()); err == nil {
		t.Error("an unmatched selection must be an error")
	}
	cfg.Agent, cfg.Owner = "", ""
	cfg.HeartbeatWindow = time.Second
	r, _ = runner.New(cfg)
	rep2, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("discover-all: %v", err)
	}
	if len(rep2.Targets) != 2 {
		t.Errorf("discover-all: %d targets, want 2", len(rep2.Targets))
	}
}

// The catalog ids the tests expect, kept outside the test bodies: a test that
// names an id in its body is linked to it by the catalog checker, and these
// tests assert the runner's behavior, not the SDK's.
const heartbeatPayloadPrefix = "SAP-8.3-0"

var conformingPass = []string{"SAP-6.4-02", "SAP-6.4-03", "SAP-6.5-01", "SAP-6.5-03", "SAP-8-01", "SAP-8.7-07",
	"SAP-3.3-01", "SAP-5.3-06", "SAP-5.1-01", "SAP-4-01", "SAP-8.2-03"}

var gatedIDs = []string{"SAP-13.9-01", "SAP-13.7-07", "SAP-7.1-01", "SAP-3.2-06", "SAP-9.2-02"}

var signedPass = []string{"SAP-13.9-01", "SAP-13.9-03", "SAP-13.1-01", "SAP-13.7-07", "SAP-13.7-08", "SAP-13.5.1-06",
	"SAP-13.7-03", "SAP-13.7-04", "SAP-13.7-05", "SAP-13.7-06", "SAP-13.5-08", "SAP-13.7-13", "SAP-13.7-11",
	"SAP-13.9-06", "SAP-13.9-07", "SAP-13.7.1-01", "SAP-13.7.1-02"}

var queryPass = []string{"SAP-7-01", "SAP-7.1-01", "SAP-7.1-02", "SAP-7.1-03", "SAP-7.3-05", "SAP-7.3-02"}

var observedQueryIDs = []string{"SAP-7-01", "SAP-7.1-01", "SAP-7.1-02", "SAP-7.1-03"}

const (
	idSessionSet        = "SAP-3.2-06"
	idUnansweredDefault = "SAP-7.3-02"
	idAnswerNeeded      = "SAP-7.3-05"
	idDistinctReplies   = "SAP-7.3-04"
	idAckFirst          = "SAP-6.4-04"
	idOversizeCode      = "SAP-9.2-02"
	idOversizePolicy    = "SAP-5.4-04"
	idChunkTypes        = "SAP-6.2-01"
	idQueryAckNA        = "SAP-7.2-02"
	idTerminator        = "SAP-6.5-01"
	idHeartbeatCadence  = "SAP-8.3-01"
	idInfoAll           = "SAP-4.1-01"
)

var queryNA = []string{"SAP-7.3-01", "SAP-7.3-04"}
var queryNotEmitted = []string{"SAP-7-01", "SAP-7.1-01", "SAP-7.3-04"}

type brokenCase struct {
	name   string
	opts   fakeOpts
	trust  string // ExpectMinSenderTrust
	failID []string
}

var brokenCases = []brokenCase{
	{"no_terminator", fakeOpts{NoTerminator: true}, "", []string{"SAP-6.5-01"}},
	{"publishes_after_terminator", fakeOpts{AfterTerminator: true}, "", []string{"SAP-6.5-03", "SAP-9.3-02"}},
	{"no_ack", fakeOpts{NoAck: true}, "", []string{"SAP-6.4-02", "SAP-6.4-03"}},
	{"wrong_status_shape", fakeOpts{WrongStatusShape: true}, "", []string{"SAP-8.7-07"}},
	{"stale_status", fakeOpts{StatusStale: true}, "", []string{"SAP-8.7-06"}},
	{"missing_queue_group", fakeOpts{NoQueueGroup: true}, "", []string{"SAP-3.3-01", "SAP-3.3-02", "SAP-8.7-03"}},
	{"heartbeat_never_sent", fakeOpts{NoHeartbeat: true}, "", []string{"SAP-8-01", "SAP-8.3-01", "SAP-8.3-02", "SAP-8.3-05", "SAP-8.3-06", "SAP-8.2-03"}},
	{"unsigned_accepted_on_signed_endpoint", fakeOpts{Trust: "signed", AdmitUnsigned: true}, "signed", []string{"SAP-13.9-03", "SAP-13.1-01"}},
}
