// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package runner is the check engine of sapconform, the black-box conformance
// runner for Synadia Agents Protocol (SAP) 0.3 hosts. See REQ-013 and AT-006.
//
// The runner needs nothing of the system under test beyond NATS access. It
// loads the conformance catalog (package conformance), and for every host or
// both-role entry decides what to do from the entry's fixed capability:
//
//   - black-box: a check registered under the entry's id runs on every run;
//   - needs-configuration: the check runs when the operator supplied the
//     configuration it needs (a sender seed, the expected min_sender_trust and
//     so on), and otherwise the result is configuration-missing;
//   - sdk-internal, and every caller-only or operator entry: not-run, with the
//     reason.
//
// The capability is a property of the catalog entry and never changes from run
// to run. A run gives each checked entry one result: pass, fail or
// configuration-missing, plus policy (a host choice the spec leaves open),
// not-applicable (the behavior is not exercised by this host or this run) and
// not-run. The runner covers host obligations only and never claims caller
// conformance. The checks exercise the agent over the wire only.
//
// The runner never silently discards a responder. A reply on a discovery or
// probe subject that is malformed, off-spec, wrong-service, duplicated or
// unparseable becomes a target (a record with the wrong service name, or an
// instance that answers $SRV.PING but not $SRV.INFO, is checked like any other),
// a finding on the catalog entry it breaks (SAP-3.1-01 for the service name,
// SAP-4.1-01 for a malformed, repeated or capped INFO record, SAP-4-01 for ping,
// SAP-8.7-07 for a second status responder), or, when it cannot be attributed
// to an instance, a note that marks the run incomplete and fails both
// verdicts. The only filters that remain are not responders (timeouts, the
// operator's own selector, our own probes) and each says so where it stands.
// A check's result only gets worse, never better: fail > configuration-missing
// > not-run > not-applicable > policy > pass. Everything recorded for an entry
// (observations, a missing seed, a resource cap, each instance) merges through
// worse, so a failure the runner saw is never overwritten by a later gap, such
// as a probe that needed a seed. TestResultsAreRecordedThroughWorse rejects a
// direct write of a result.
//
// A stream chunk of an unknown type is reported, not failed: SAP tells callers
// to ignore unknown chunk types, so failing one would fail a conforming host.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/bogworks/synadia-agents.go/conformance"
	"github.com/bogworks/synadia-agents.go/identity"
)

// Version is the version of the runner, reported in every report.
const Version = "0.1.0"

// Scope is the statement every report carries about what it covers.
const Scope = "host obligations only: the runner exercises an SAP agent over NATS as a black box and makes no claim about caller conformance"

// Result is the outcome of one check in one run. It is separate from the
// entry's capability, which is fixed in the catalog.
type Result string

// The results of a run.
const (
	// Pass means the obligation was observed to hold.
	Pass Result = "pass"
	// Fail means the host was observed to break the obligation.
	Fail Result = "fail"
	// ConfigMissing means the entry needs configuration the operator did not
	// supply.
	ConfigMissing Result = "configuration-missing"
	// Policy means the host made a choice the spec leaves to it (a host-optional
	// 400, for example); it is reported and never counts as a failure.
	Policy Result = "policy"
	// NotApplicable means this host or this run does not exercise the
	// behavior (the agent never emits a query, the endpoint accepts
	// attachments so the refusal cannot be seen).
	NotApplicable Result = "not-applicable"
	// NotRun means the runner does not check the entry: it is not a host
	// obligation, it is observable only inside an SDK, or the check is not
	// implemented in this version.
	NotRun Result = "not-run"
)

// Outcome is what a check returns.
type Outcome struct {
	Result Result
	Detail string
}

// CheckFunc checks one catalog entry against one target. It must use only the
// wire.
type CheckFunc func(ctx context.Context, t *T) Outcome

// Config configures a run. Conn is the only required field.
type Config struct {
	// Conn is a connection to the NATS system the agent is on.
	Conn *nats.Conn

	// Agent, Owner and Name select targets by subject token (or metadata
	// agent and owner). All empty means every agent that answers discovery.
	Agent, Owner, Name string

	// DiscoverWait bounds the discovery round (default 2s).
	DiscoverWait time.Duration
	// HeartbeatWindow is the heartbeat observation window. Zero means auto:
	// about two declared heartbeat intervals, between 3s and 70s, ending early
	// once every instance has been heard three times.
	HeartbeatWindow time.Duration
	// StreamTimeout is the inactivity timeout of one prompt stream (default 30s).
	StreamTimeout time.Duration
	// PostTerminatorGrace is how long to listen after the terminator for a
	// message that must not come (default 400ms).
	PostTerminatorGrace time.Duration
	// Timing holds the fixed waits that have no flag of their own. The zero
	// value of each field means its production default (see Timing.Defaults).
	Timing Timing
	// ProbePrompt is the text of the well-formed prompts the runner sends
	// (default "sapconform probe: reply with the word ok").
	ProbePrompt string
	// ClockSkew is how far a heartbeat's ts may differ from the local clock
	// before the runner calls it wrong (default 10s).
	ClockSkew time.Duration

	// The fields below are configuration that gates needs-configuration
	// entries. The zero value means not supplied.

	// ExpectMinSenderTrust is what the operator says the agent declares: "none"
	// (no sender-identity extension), "any" or "signed". It enables the
	// extension checks (SAP §13).
	ExpectMinSenderTrust string
	// SenderSigner signs requests as an identity the agent admits. SenderAccount
	// is its account; when empty the runner asks the connection
	// ($SYS.REQ.USER.INFO) and uses the answer if the connection's user is the
	// signer's user.
	SenderSigner  identity.Signer
	SenderAccount string
	// RefusedSigner signs as an identity the agent verifies but refuses.
	RefusedSigner identity.Signer
	// SessionAware says whether the harness is session-aware. Nil is not
	// supplied.
	SessionAware *bool
	// ExpectInstances is the number of instances of one logical agent the
	// operator started (2 or more enables the multiple-instance checks).
	ExpectInstances int
	// ProbeOversize enables the oversize request probe (it publishes a payload
	// just over the declared max_payload).
	ProbeOversize bool
	// QueryPrompt is a prompt the operator knows makes the agent emit a
	// mid-stream query. QueryUnansweredWait is how long to wait for the agent's
	// own timeout when the query goes unanswered (default 15s).
	QueryPrompt         string
	QueryUnansweredWait time.Duration
	// SlowPrompt is a prompt the operator knows makes the agent do slow work
	// before its first response chunk. AckDeadline is how soon the ack must
	// arrive (default 1s).
	SlowPrompt  string
	AckDeadline time.Duration

	// AnswerQuerySubjectPrefixes are the subject prefixes the runner may publish
	// a query answer under. The runner publishes with the operator's NATS
	// credentials to a subject the agent under test chose, so by default it only
	// observes queries and answers none. A query is answered when its
	// reply_subject starts with one of these prefixes; the checks that need an
	// answered query are otherwise configuration-missing.
	AnswerQuerySubjectPrefixes []string

	// Resource limits. The agent under test and everything else on the subject
	// space is untrusted traffic, so a run is bounded; zero means the default.

	// MaxDuration is the total deadline of the run (default 5m). When it
	// passes, the checks not yet run are not-run and the report is marked
	// incomplete.
	MaxDuration time.Duration
	// MaxStreamMessages and MaxStreamBytes cap what one prompt stream may
	// deliver, counting messages after the terminator (default 1000 and 4 MiB).
	// A stream over a cap is closed and the checks that used it fail.
	MaxStreamMessages int
	MaxStreamBytes    int
	// MaxDiscovery caps the replies read from one discovery round (default 256).
	MaxDiscovery int
	// MaxTargets caps the targets checked in a run (default 32); the rest are
	// listed in the report as skipped.
	MaxTargets int
	// MaxHeartbeats caps the heartbeats retained over a run (default 5000), and
	// MaxInfoProbes the INFO probes started because a heartbeat named a new
	// instance (default 64).
	MaxHeartbeats int
	MaxInfoProbes int

	// Logger receives progress. Nil is silent. Never given seeds or payloads.
	Logger *slog.Logger
}

// Timing is every fixed wait in the runner that is not already a Config
// field (DiscoverWait, HeartbeatWindow, StreamTimeout, PostTerminatorGrace,
// QueryUnansweredWait and AckDeadline are the others). cmd/sapconform uses the
// zero value, which is Defaults(); tests against in-process fakes pass a
// shorter profile. A field at zero or below means its default.
type Timing struct {
	// FirstBeatInfoWait is how long SAP-8.2-05 waits for the service record to
	// have been answerable at an instance's first heartbeat (default 3s).
	FirstBeatInfoWait time.Duration
	// InfoProbeTimeout bounds the $SRV.INFO probe of a newly heard instance
	// (default 2s).
	InfoProbeTimeout time.Duration
	// StatusTimeout bounds one status request (default 5s).
	StatusTimeout time.Duration
	// FreshTSGap separates the two status requests that show ts is built per
	// request. ts has one-second precision, so it must stay above 1s unless the
	// host's ts is finer (default 1.1s).
	FreshTSGap time.Duration
	// PollInterval is the tick of the heartbeat and first-beat polling loops
	// (default 50ms).
	PollInterval time.Duration
	// DiscoverStallMin is the least quiet time that ends a discovery round
	// early (default 300ms); the round's stall is a quarter of its window, but
	// never less than this.
	DiscoverStallMin time.Duration
	// AutoHeartbeatWindow is the observation window when the status reply
	// declares no interval (default 5s). With a declared interval the window is
	// 2.2 intervals clamped to [AutoHeartbeatMin, AutoHeartbeatMax].
	AutoHeartbeatWindow time.Duration
	AutoHeartbeatMin    time.Duration
	AutoHeartbeatMax    time.Duration
}

// Defaults returns the production timing.
func (Timing) Defaults() Timing {
	return Timing{
		FirstBeatInfoWait:   3 * time.Second,
		InfoProbeTimeout:    2 * time.Second,
		StatusTimeout:       5 * time.Second,
		FreshTSGap:          1100 * time.Millisecond,
		PollInterval:        50 * time.Millisecond,
		DiscoverStallMin:    300 * time.Millisecond,
		AutoHeartbeatWindow: 5 * time.Second,
		AutoHeartbeatMin:    3 * time.Second,
		AutoHeartbeatMax:    70 * time.Second,
	}
}

func (t *Timing) fill() {
	d := Timing{}.Defaults()
	for _, p := range []struct {
		v   *time.Duration
		def time.Duration
	}{
		{&t.FirstBeatInfoWait, d.FirstBeatInfoWait}, {&t.InfoProbeTimeout, d.InfoProbeTimeout},
		{&t.StatusTimeout, d.StatusTimeout}, {&t.FreshTSGap, d.FreshTSGap},
		{&t.PollInterval, d.PollInterval}, {&t.DiscoverStallMin, d.DiscoverStallMin},
		{&t.AutoHeartbeatWindow, d.AutoHeartbeatWindow}, {&t.AutoHeartbeatMin, d.AutoHeartbeatMin},
		{&t.AutoHeartbeatMax, d.AutoHeartbeatMax},
	} {
		if *p.v <= 0 {
			*p.v = p.def
		}
	}
}

func (c *Config) defaults() {
	c.Timing.fill()
	if c.DiscoverWait <= 0 {
		c.DiscoverWait = 2 * time.Second
	}
	if c.StreamTimeout <= 0 {
		c.StreamTimeout = 30 * time.Second
	}
	if c.PostTerminatorGrace <= 0 {
		c.PostTerminatorGrace = 400 * time.Millisecond
	}
	if c.ProbePrompt == "" {
		c.ProbePrompt = "sapconform probe: reply with the word ok"
	}
	if c.ClockSkew <= 0 {
		c.ClockSkew = 10 * time.Second
	}
	if c.QueryUnansweredWait <= 0 {
		c.QueryUnansweredWait = 15 * time.Second
	}
	if c.AckDeadline <= 0 {
		c.AckDeadline = time.Second
	}
	setDefault := func(p *int, v int) {
		if *p <= 0 {
			*p = v
		}
	}
	if c.MaxDuration <= 0 {
		c.MaxDuration = 5 * time.Minute
	}
	setDefault(&c.MaxStreamMessages, 1000)
	setDefault(&c.MaxStreamBytes, 4<<20)
	setDefault(&c.MaxDiscovery, 256)
	setDefault(&c.MaxTargets, 32)
	setDefault(&c.MaxHeartbeats, 5000)
	setDefault(&c.MaxInfoProbes, 64)
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
}

// Supplied returns the configuration keys the operator supplied. The report
// lists them (never their values).
func (c *Config) Supplied() []string {
	var out []string
	for _, k := range allConfigKeys {
		if c.has(k) {
			out = append(out, k)
		}
	}
	return out
}

// Configuration keys. Each gates the needs-configuration entries that list it.
const (
	KeyTrust    = "expect-min-sender-trust"
	KeySeed     = "sender-seed"
	KeyRefused  = "refused-seed"
	KeySession  = "session-aware"
	KeyInst     = "expect-instances"
	KeyOversize = "probe-oversize"
	KeyQuery    = "query-prompt"
	KeySlow     = "slow-prompt"
	KeyAnswer   = "answer-query-subject-prefix"
)

var allConfigKeys = []string{KeyTrust, KeySeed, KeyRefused, KeySession, KeyInst, KeyOversize, KeyQuery, KeySlow, KeyAnswer}

func (c *Config) has(key string) bool {
	switch key {
	case KeyTrust:
		return c.ExpectMinSenderTrust != ""
	case KeySeed:
		return c.SenderSigner != nil
	case KeyRefused:
		return c.RefusedSigner != nil
	case KeySession:
		return c.SessionAware != nil
	case KeyInst:
		return c.ExpectInstances >= 2
	case KeyOversize:
		return c.ProbeOversize
	case KeyQuery:
		return c.QueryPrompt != ""
	case KeySlow:
		return c.SlowPrompt != ""
	case KeyAnswer:
		return len(c.AnswerQuerySubjectPrefixes) > 0
	}
	return false
}

// EntryResult is one catalog entry's line in a report.
type EntryResult struct {
	ID         string  `json:"id"`
	Section    string  `json:"section"`
	SpecLine   int     `json:"spec_line"`
	Level      string  `json:"level"`
	Role       string  `json:"role"`
	Capability string  `json:"capability"`
	Summary    string  `json:"summary"`
	Result     Result  `json:"result"`
	Detail     string  `json:"detail,omitempty"`
	DurationMS float64 `json:"duration_ms"`
	// ConfigSupplied is set on a configuration-missing result when the operator
	// supplied some or all of the configuration the entry needs (the check could
	// not run with it, or part is still missing). The strict verdict fails on
	// such a result; an entry whose configuration was never supplied does not.
	ConfigSupplied bool `json:"config_supplied,omitempty"`
	// Untested is set on a not-applicable result of a MUST or MUST NOT entry
	// whose configuration the operator supplied: the operator asked for the
	// check and it could not run. The strict verdict fails on such a result.
	Untested bool `json:"untested,omitempty"`
	// Unchecked is set on a not-run result of a MUST or MUST NOT entry that a
	// host can be checked for (not sdk-internal, not-applicable, and not an
	// operator or caller obligation) but for which this version of the runner
	// has no check that ran. The strict verdict fails on such a result: a MUST the
	// runner silently skipped must not read as conforming. The default verdict
	// reports it as not-run.
	Unchecked bool `json:"unchecked,omitempty"`
}

// IsFailure reports whether this result makes the run fail: a fail of a MUST
// or MUST NOT entry.
func (e EntryResult) IsFailure() bool {
	return (e.Result == Fail || !knownResult(e.Result)) && (e.Level == conformance.LevelMust || e.Level == conformance.LevelMustNot)
}

// IsStrictFailure reports whether this result makes the strict verdict fail: a
// fail of any level, a MUST with no registered check, or a configuration-missing for an entry the operator
// supplied configuration for.
func (e EntryResult) IsStrictFailure() bool {
	return e.Result == Fail || !knownResult(e.Result) || e.Untested || e.Unchecked || (e.Result == ConfigMissing && e.ConfigSupplied)
}

// TargetInfo identifies the agent a TargetReport is about.
type TargetInfo struct {
	PromptSubject string   `json:"prompt_subject"`
	Agent         string   `json:"agent"`
	Owner         string   `json:"owner"`
	Name          string   `json:"name,omitempty"`
	InstanceIDs   []string `json:"instance_ids"`
}

// TargetReport is the result list for one target.
type TargetReport struct {
	Target  TargetInfo     `json:"target"`
	Entries []EntryResult  `json:"entries"`
	Counts  map[Result]int `json:"counts"`
}

// Report is the one report of a run.
type Report struct {
	Tool           string         `json:"tool"`
	ToolVersion    string         `json:"tool_version"`
	Scope          string         `json:"scope"`
	CatalogVersion int            `json:"catalog_version"`
	SpecVersion    string         `json:"spec_version"`
	SpecCommit     string         `json:"spec_commit"`
	StartedAt      time.Time      `json:"started_at"`
	Duration       string         `json:"duration"`
	Selector       Selector       `json:"selector"`
	Supplied       []string       `json:"configuration_supplied"`
	Targets        []TargetReport `json:"targets"`
	// Incomplete says why the run did not check everything it meant to: the
	// total deadline passed, the run was canceled, or a cap left targets out.
	// Empty for a run that finished. An incomplete run is neither OK nor
	// strictly OK.
	Incomplete string `json:"incomplete,omitempty"`
	// Notes are run-level observations, such as a heartbeat or INFO-probe cap
	// that was reached.
	Notes []string `json:"notes,omitempty"`
	// Verdict carries both verdicts of the run.
	Verdict Verdict `json:"verdict"`
}

// Verdict is the outcome of a run, in two strengths. Default is the historic
// verdict: no MUST or MUST NOT entry failed. Strict is stronger and is what
// automation should gate on: no entry of any level failed and no entry the
// operator supplied configuration for is configuration-missing. Neither is true
// for a run with no target or an incomplete run.
type Verdict struct {
	Default bool `json:"default_ok"`
	Strict  bool `json:"strict_ok"`
	// Failed lists the ids that fail the default verdict, and StrictFailed those
	// that fail the strict one (a superset), over all targets.
	Failed       []string `json:"failed,omitempty"`
	StrictFailed []string `json:"strict_failed,omitempty"`
}

// Selector echoes the target selection of a run.
type Selector struct {
	Agent string `json:"agent,omitempty"`
	Owner string `json:"owner,omitempty"`
	Name  string `json:"name,omitempty"`
	All   bool   `json:"discover_all"`
}

// OK reports the default verdict: no MUST or MUST NOT entry failed on any
// target. A run with no targets, or an incomplete run, is not OK. Failures of
// other levels and missing configuration do not change it; see StrictOK.
func (r *Report) OK() bool { return r.computeVerdict().Default }

// StrictOK reports the strict verdict: no entry of any level failed and no
// entry the operator supplied configuration for is configuration-missing. A run
// with no targets, or an incomplete run, is not strictly OK.
func (r *Report) StrictOK() bool { return r.computeVerdict().Strict }

func (r *Report) computeVerdict() Verdict {
	var v Verdict
	seen, seenStrict := map[string]bool{}, map[string]bool{}
	for _, t := range r.Targets {
		for _, e := range t.Entries {
			if e.IsFailure() && !seen[e.ID] {
				seen[e.ID] = true
				v.Failed = append(v.Failed, e.ID)
			}
			if e.IsStrictFailure() && !seenStrict[e.ID] {
				seenStrict[e.ID] = true
				v.StrictFailed = append(v.StrictFailed, e.ID)
			}
		}
	}
	complete := len(r.Targets) > 0 && r.Incomplete == ""
	v.Default = complete && len(v.Failed) == 0
	v.Strict = complete && len(v.StrictFailed) == 0
	return v
}

// Failures returns the entries that make the run fail, with their target.
func (r *Report) Failures() []EntryResult {
	var out []EntryResult
	for _, t := range r.Targets {
		for _, e := range t.Entries {
			if e.IsFailure() {
				out = append(out, e)
			}
		}
	}
	return out
}

// ErrNoTarget is returned when no agent matched the selection.
var ErrNoTarget = errors.New("sapconform: no agent answered discovery for the selection")

// Runner runs the catalog's checks against targets.
type Runner struct {
	cfg    Config
	cat    *conformance.Catalog
	checks map[string]CheckFunc
}

// New builds a runner over the embedded catalog.
func New(cfg Config) (*Runner, error) {
	cat, err := conformance.Parse(conformance.CatalogJSON())
	if err != nil {
		return nil, err
	}
	return NewWithCatalog(cfg, cat)
}

// NewWithCatalog builds a runner over a given catalog.
func NewWithCatalog(cfg Config, cat *conformance.Catalog) (*Runner, error) {
	if cfg.Conn == nil {
		return nil, errors.New("sapconform: Config.Conn is required")
	}
	cfg.defaults()
	return &Runner{cfg: cfg, cat: cat, checks: builtinChecks()}, nil
}

// Catalog returns the catalog the runner uses.
func (r *Runner) Catalog() *conformance.Catalog { return r.cat }

// Registered returns the ids that have a registered check, sorted.
func Registered() []string {
	m := builtinChecks()
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// HostEntries returns the catalog's host and both-role entries of a capability.
func HostEntries(cat *conformance.Catalog, capability string) []conformance.Entry {
	var out []conformance.Entry
	for _, e := range cat.Entries {
		if (e.Role == conformance.RoleHost || e.Role == conformance.RoleBoth) && e.Capability == capability {
			out = append(out, e)
		}
	}
	return out
}

// Run discovers the targets and checks each. It returns an error only when the
// run itself could not happen (no target, a dead connection); a failing agent
// is a Report, not an error. The run is bounded by Config.MaxDuration; a run
// that hits it, is canceled, or leaves targets out because of Config.MaxTargets
// returns a Report whose Incomplete field says so.
func (r *Runner) Run(ctx context.Context) (*Report, error) {
	start := time.Now()
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, r.cfg.MaxDuration)
	defer cancel()
	hb, err := startHeartbeats(r.cfg.Conn, r.cfg.MaxHeartbeats, r.cfg.MaxInfoProbes, r.cfg.Timing.InfoProbeTimeout)
	if err != nil {
		return nil, fmt.Errorf("sapconform: subscribe heartbeats: %w", err)
	}
	defer hb.stop()
	// Subscribe to heartbeats before the first discovery request (SAP §8.5).
	disc, err := discover(ctx, r.cfg.Conn, r.cfg.DiscoverWait, r.cfg.MaxDiscovery, r.cfg.Timing.DiscoverStallMin)
	if err != nil {
		return nil, fmt.Errorf("sapconform: discovery: %w", err)
	}
	malformed, truncated := disc.Malformed, disc.Truncated
	known := map[string]bool{}
	for _, in := range disc.Insts {
		known[in.ID] = true
	}
	targets := groupTargets(disc.Insts, r.cfg.Agent, r.cfg.Owner, r.cfg.Name)
	if len(targets) == 0 {
		if len(malformed) > 0 {
			return nil, fmt.Errorf("%w (%d malformed discovery replies could not be attributed to an agent)", ErrNoTarget, len(malformed))
		}
		return nil, ErrNoTarget
	}
	var incomplete []string
	if len(malformed) > 0 {
		shown := malformed
		if len(shown) > 3 {
			shown = shown[:3]
		}
		incomplete = append(incomplete, fmt.Sprintf("%d malformed discovery replies cannot be attributed to an instance, so no check could judge them (%s)", len(malformed), strings.Join(shown, "; ")))
	}
	if truncated {
		incomplete = append(incomplete, fmt.Sprintf("a discovery round stopped at %d replies (--max-discovery); responders beyond them were not seen", r.cfg.MaxDiscovery))
	}
	if len(targets) > r.cfg.MaxTargets {
		incomplete = append(incomplete, fmt.Sprintf("%d targets answered discovery and %d were checked (--max-targets); the rest were skipped", len(targets), r.cfg.MaxTargets))
		targets = targets[:r.cfg.MaxTargets]
	}
	rep := &Report{
		Tool: "sapconform", ToolVersion: Version, Scope: Scope,
		CatalogVersion: r.cat.CatalogVersion, SpecVersion: r.cat.Spec.Version, SpecCommit: r.cat.Spec.Commit,
		StartedAt: start.UTC(),
		Selector:  Selector{Agent: r.cfg.Agent, Owner: r.cfg.Owner, Name: r.cfg.Name, All: r.cfg.Agent == "" && r.cfg.Owner == "" && r.cfg.Name == ""},
		Supplied:  r.cfg.Supplied(),
	}
	for _, tg := range targets {
		t := newT(&r.cfg, tg, hb)
		rep.Targets = append(rep.Targets, r.runTarget(ctx, t))
	}
	switch {
	case parent.Err() != nil:
		incomplete = append(incomplete, "the run was canceled: "+parent.Err().Error())
	case ctx.Err() != nil:
		incomplete = append(incomplete, fmt.Sprintf("the run reached its total deadline of %s (--max-duration); checks after it were not run", r.cfg.MaxDuration))
	}
	if n := hb.capNotes(); n != "" {
		rep.Notes = append(rep.Notes, n)
		// Heartbeats or INFO probes beyond the caps went unjudged.
		incomplete = append(incomplete, "heartbeat caps were reached: "+n)
	}
	if rep.Selector.All {
		// With no selector every responder is in scope: a heartbeat from an
		// instance that never answered discovery is reported, not ignored. With a
		// selector, heartbeats of other agents are out of scope.
		if n := hb.strangers(known); n != "" {
			incomplete = append(incomplete, "heartbeats from unexpected publishers: "+n)
		}
	}
	rep.Incomplete = strings.Join(incomplete, "; ")
	rep.Duration = time.Since(start).Round(time.Millisecond).String()
	rep.Verdict = rep.computeVerdict()
	return rep, nil
}

func (r *Runner) runTarget(ctx context.Context, t *T) TargetReport {
	tr := TargetReport{Target: t.info(), Counts: map[Result]int{}}
	for _, e := range r.cat.Entries {
		er := EntryResult{
			ID: e.ID, Section: e.Section, SpecLine: e.SpecLine, Level: e.Level, Role: e.Role,
			Capability: e.Capability, Summary: e.Summary,
		}
		began := time.Now()
		t.curOver = nil
		t.rec = recorder{}
		out, supplied := r.decide(ctx, t, e)
		t.rec.add(out)
		if len(t.curOver) > 0 {
			// The check judged traffic that hit a resource cap: it cannot pass.
			t.rec.add(Outcome{Fail, "resource limit: " + strings.Join(t.curOver, "; ")})
		}
		merged, _ := t.rec.result()
		er.Result, er.Detail = merged.Result, merged.Detail
		er.ConfigSupplied = merged.Result == ConfigMissing && supplied
		er.Unchecked = unchecked(e, merged.Result == NotRun)
		er.Untested = merged.Result == NotApplicable && supplied && e.IsStrict() && e.Capability == conformance.CapNeedsCfg
		er.DurationMS = float64(time.Since(began).Microseconds()) / 1000
		tr.Entries = append(tr.Entries, er)
		tr.Counts[er.Result]++
	}
	return tr
}

// unchecked reports whether a MUST or MUST NOT entry that applies to a host
// black-box probe was not run: it has no registered check, or its check is a
// stub that reports not-run. Entries the catalog explicitly
// marks as not host-checkable (sdk-internal, not-applicable) or as another
// role's obligation never count.
func unchecked(e conformance.Entry, notRun bool) bool {
	if !notRun || !e.IsStrict() {
		return false
	}
	switch {
	case e.Role == conformance.RoleOperator, e.Role == conformance.RoleCaller,
		e.Capability == conformance.CapSDK, e.Capability == conformance.CapNA:
		return false
	}
	return true
}

// decide runs or skips one entry. For a configuration-missing result it also
// says whether the operator supplied some of the configuration the entry needs.
func (r *Runner) decide(ctx context.Context, t *T, e conformance.Entry) (Outcome, bool) {
	switch {
	case e.Role == conformance.RoleOperator:
		return Outcome{NotRun, "deployment practice (operator role); not a host behavior"}, false
	case e.Role == conformance.RoleCaller:
		return Outcome{NotRun, "caller obligation; the runner covers host obligations only"}, false
	case e.Capability == conformance.CapSDK:
		return Outcome{NotRun, "observable only inside an SDK; nothing a NATS client can see"}, false
	case e.Capability == conformance.CapNA:
		return Outcome{NotApplicable, e.NotApplicableReason}, false
	}
	fn := r.checks[e.ID]
	if fn == nil {
		return Outcome{NotRun, "no check is registered for this entry in this version of the runner"}, false
	}
	if e.Capability == conformance.CapNeedsCfg {
		var missing []string
		anySupplied := false
		for _, k := range gates[e.ID] {
			if r.cfg.has(k) {
				anySupplied = true
			} else {
				missing = append(missing, "--"+k)
			}
		}
		if len(missing) > 0 {
			return Outcome{ConfigMissing, "supply " + strings.Join(missing, ", ")}, anySupplied
		}
	}
	out := safeRun(ctx, t, fn)
	// A check that reports missing configuration after its gates were met is
	// one the operator supplied configuration for.
	return out, true
}

func safeRun(ctx context.Context, t *T, fn CheckFunc) (out Outcome) {
	defer func() {
		if p := recover(); p != nil {
			out = Outcome{Fail, fmt.Sprintf("check panicked: %v", p)}
		}
	}()
	if err := ctx.Err(); err != nil {
		return Outcome{NotRun, "run canceled: " + err.Error()}
	}
	return fn(ctx, t)
}
