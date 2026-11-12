// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/bogworks/synadia-agents.go/conformance"
	"github.com/bogworks/synadia-agents.go/conformance/runner"
	"github.com/bogworks/synadia-agents.go/connect"
	"github.com/bogworks/synadia-agents.go/identity"
)

// Exit codes.
const (
	exitOK      = 0 // no MUST or MUST NOT entry failed (with --strict: nothing of any level failed)
	exitFailed  = 1 // a MUST or MUST NOT entry failed (with --strict: any entry failed), or the run was incomplete
	exitProblem = 2 // the run could not happen: usage, connection, no target
)

// fileConfig is the documented configuration file (--config). Every field is
// also a flag, and a flag overrides the file. Seeds are named by file path and
// are never printed.
type fileConfig struct {
	URL      string `json:"url,omitempty"`
	Context  string `json:"context,omitempty"`
	Creds    string `json:"creds,omitempty"`
	NKey     string `json:"nkey,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Owner    string `json:"owner,omitempty"`
	Name     string `json:"name,omitempty"`
	JSON     bool   `json:"json,omitempty"`
	Verbose  bool   `json:"verbose,omitempty"`
	Strict   bool   `json:"strict,omitempty"`
	Timeouts struct {
		Discover        string `json:"discover,omitempty"`
		HeartbeatWindow string `json:"heartbeat_window,omitempty"`
		Stream          string `json:"stream,omitempty"`
		QueryUnanswered string `json:"query_unanswered_wait,omitempty"`
		Ack             string `json:"ack_deadline,omitempty"`
		MaxDuration     string `json:"max_duration,omitempty"`
	} `json:"timeouts,omitempty"`
	Limits struct {
		StreamMessages int `json:"stream_messages,omitempty"`
		StreamBytes    int `json:"stream_bytes,omitempty"`
		Discovery      int `json:"discovery,omitempty"`
		Targets        int `json:"targets,omitempty"`
		Heartbeats     int `json:"heartbeats,omitempty"`
		InfoProbes     int `json:"info_probes,omitempty"`
	} `json:"limits,omitempty"`
	AnswerQuerySubjectPrefixes []string `json:"answer_query_subject_prefixes,omitempty"`
	ProbePrompt                string   `json:"probe_prompt,omitempty"`
	ExpectMinSenderTrust       string   `json:"expect_min_sender_trust,omitempty"`
	SenderSeed                 string   `json:"sender_seed,omitempty"`
	SenderFromConnection       bool     `json:"sender_from_connection,omitempty"`
	SenderAccount              string   `json:"sender_account,omitempty"`
	RefusedSeed                string   `json:"refused_seed,omitempty"`
	SessionAware               *bool    `json:"session_aware,omitempty"`
	ExpectInstances            int      `json:"expect_instances,omitempty"`
	ProbeOversize              bool     `json:"probe_oversize,omitempty"`
	QueryPrompt                string   `json:"query_prompt,omitempty"`
	SlowPrompt                 string   `json:"slow_prompt,omitempty"`
}

const usage = `sapconform: check an SAP 0.3 agent (host) over NATS, as a black box.

usage: sapconform [flags]

Connection (one of --context or --url; creds and nkey are optional):
  --context NAME        a NATS CLI context ("current" for the selected one)
  --url URL             NATS server URL (default $NATS_URL, else nats://127.0.0.1:4222)
  --creds FILE          credentials file
  --nkey FILE           user seed file

Target (none of these means every agent that answers discovery):
  --agent TOKEN  --owner TOKEN  --name TOKEN

Output:
  --json                one JSON report on stdout
  --strict              exit 1 on any failure of any level, or on missing configuration
                        for an entry you supplied configuration for
  -v                    list every entry in the human summary
  --config FILE         a JSON file holding any of these settings (flags win)

Observation:
  --discover-wait D     discovery round (default 2s)
  --heartbeat-window D  heartbeat observation window (default: automatic)
  --stream-timeout D    inactivity timeout of one prompt stream (default 30s)
  --probe-prompt TEXT   the prompt text of the well-formed probes

Limits (the agent and the subject space are untrusted traffic):
  --max-duration D      total deadline of the run (default 5m); an unfinished run is incomplete
  --max-stream-messages N   messages one prompt stream may deliver (default 1000)
  --max-stream-bytes N      bytes one prompt stream may deliver (default 4194304)
  --max-discovery N     discovery replies read per round (default 256)
  --max-targets N       targets checked per run (default 32)
  --max-heartbeats N    heartbeats retained per run (default 5000)
  --max-info-probes N   INFO probes started from heartbeats per run (default 64)

Configuration that enables needs-configuration entries:
  --expect-min-sender-trust none|any|signed   what the agent declares (SAP 13)
  --sender-seed FILE    user seed of an identity the agent admits
  --sender-from-connection   sign with the connection's own seed
  --sender-account NAME account of the sender (default: asked of the server)
  --refused-seed FILE   user seed of an identity the agent verifies and refuses
  --session-aware[=false]    whether the harness is session-aware
  --expect-instances N  instances of one logical agent that are running
  --probe-oversize      send a request one byte over the declared max_payload
  --query-prompt TEXT   a prompt that makes the agent ask a mid-stream query
  --answer-query-subject-prefix P   let the runner answer a query whose reply_subject
                        starts with P (repeatable). Without it queries are observed and
                        never answered: the agent chooses the subject and the runner
                        publishes with your credentials
  --query-unanswered-wait D   how long to wait for the agent's own query timeout (default 15s)
  --slow-prompt TEXT    a prompt that makes the agent do slow work first
  --ack-deadline D      how soon the ack must come on the slow prompt (default 1s)

Exit status: 0 when no MUST or MUST NOT entry failed, 1 when one did or the run
was incomplete, 2 when the run could not happen. With --strict, 0 means nothing
of any level failed and no entry you supplied configuration for is
configuration-missing. The JSON report carries both verdicts.
`

// Main runs the command and returns its exit status.
func Main(args []string, stdout, stderr io.Writer) int {
	cfg, err := loadConfigFile(args)
	if err != nil {
		fmt.Fprintf(stderr, "sapconform: %v\n", err)
		return exitProblem
	}
	fs := flag.NewFlagSet("sapconform", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }

	var (
		cfgPath    string
		showVer    bool
		discover   = fs.String("discover-wait", cfg.Timeouts.Discover, "")
		hbWindow   = fs.String("heartbeat-window", cfg.Timeouts.HeartbeatWindow, "")
		streamT    = fs.String("stream-timeout", cfg.Timeouts.Stream, "")
		queryWait  = fs.String("query-unanswered-wait", cfg.Timeouts.QueryUnanswered, "")
		ackDead    = fs.String("ack-deadline", cfg.Timeouts.Ack, "")
		maxDur     = fs.String("max-duration", cfg.Timeouts.MaxDuration, "")
		answers    = stringList(cfg.AnswerQuerySubjectPrefixes)
		session    = fs.Bool("session-aware", false, "")
		sessionSet bool
	)
	if cfg.SessionAware != nil {
		*session = *cfg.SessionAware
	}
	fs.StringVar(&cfgPath, "config", "", "")
	fs.BoolVar(&showVer, "version", false, "")
	fs.StringVar(&cfg.URL, "url", cfg.URL, "")
	fs.StringVar(&cfg.Context, "context", cfg.Context, "")
	fs.StringVar(&cfg.Creds, "creds", cfg.Creds, "")
	fs.StringVar(&cfg.NKey, "nkey", cfg.NKey, "")
	fs.StringVar(&cfg.Agent, "agent", cfg.Agent, "")
	fs.StringVar(&cfg.Owner, "owner", cfg.Owner, "")
	fs.StringVar(&cfg.Name, "name", cfg.Name, "")
	fs.BoolVar(&cfg.JSON, "json", cfg.JSON, "")
	fs.BoolVar(&cfg.Verbose, "v", cfg.Verbose, "")
	fs.BoolVar(&cfg.Strict, "strict", cfg.Strict, "")
	fs.IntVar(&cfg.Limits.StreamMessages, "max-stream-messages", cfg.Limits.StreamMessages, "")
	fs.IntVar(&cfg.Limits.StreamBytes, "max-stream-bytes", cfg.Limits.StreamBytes, "")
	fs.IntVar(&cfg.Limits.Discovery, "max-discovery", cfg.Limits.Discovery, "")
	fs.IntVar(&cfg.Limits.Targets, "max-targets", cfg.Limits.Targets, "")
	fs.IntVar(&cfg.Limits.Heartbeats, "max-heartbeats", cfg.Limits.Heartbeats, "")
	fs.IntVar(&cfg.Limits.InfoProbes, "max-info-probes", cfg.Limits.InfoProbes, "")
	fs.Var(&answers, "answer-query-subject-prefix", "")
	fs.StringVar(&cfg.ProbePrompt, "probe-prompt", cfg.ProbePrompt, "")
	fs.StringVar(&cfg.ExpectMinSenderTrust, "expect-min-sender-trust", cfg.ExpectMinSenderTrust, "")
	fs.StringVar(&cfg.SenderSeed, "sender-seed", cfg.SenderSeed, "")
	fs.BoolVar(&cfg.SenderFromConnection, "sender-from-connection", cfg.SenderFromConnection, "")
	fs.StringVar(&cfg.SenderAccount, "sender-account", cfg.SenderAccount, "")
	fs.StringVar(&cfg.RefusedSeed, "refused-seed", cfg.RefusedSeed, "")
	fs.IntVar(&cfg.ExpectInstances, "expect-instances", cfg.ExpectInstances, "")
	fs.BoolVar(&cfg.ProbeOversize, "probe-oversize", cfg.ProbeOversize, "")
	fs.StringVar(&cfg.QueryPrompt, "query-prompt", cfg.QueryPrompt, "")
	fs.StringVar(&cfg.SlowPrompt, "slow-prompt", cfg.SlowPrompt, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitProblem
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "session-aware" {
			sessionSet = true
		}
	})
	if showVer {
		fmt.Fprintf(stdout, "sapconform %s\n", runner.Version)
		return exitOK
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "sapconform: unexpected argument %q\n", fs.Arg(0))
		return exitProblem
	}
	if sessionSet || cfg.SessionAware != nil {
		v := *session
		cfg.SessionAware = &v
	}
	_ = cfgPath
	cfg.AnswerQuerySubjectPrefixes = answers

	rc, closeFn, err := buildRunnerConfig(cfg, discover, hbWindow, streamT, queryWait, ackDead, maxDur)
	if err != nil {
		fmt.Fprintf(stderr, "sapconform: %v\n", err)
		return exitProblem
	}
	defer closeFn()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	r, err := runner.New(rc)
	if err != nil {
		fmt.Fprintf(stderr, "sapconform: %v\n", err)
		return exitProblem
	}
	rep, err := r.Run(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return exitProblem
	}
	if cfg.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(stderr, "sapconform: %v\n", err)
			return exitProblem
		}
	} else {
		writeSummary(stdout, rep, r.Catalog(), cfg.Verbose)
	}
	if cfg.Strict && !rep.StrictOK() {
		return exitFailed
	}
	if !rep.OK() {
		return exitFailed
	}
	return exitOK
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// loadConfigFile finds --config in args and reads it. A flag later overrides a
// value the file set, because the flags are defined with the file's values as
// their defaults.
func loadConfigFile(args []string) (fileConfig, error) {
	var cfg fileConfig
	path := ""
	for i, a := range args {
		a = strings.TrimLeft(a, "-")
		if v, ok := strings.CutPrefix(a, "config="); ok {
			path = v
		} else if a == "config" && i+1 < len(args) {
			path = args[i+1]
		}
	}
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304/G703: the config path is the operator's --config argument
	if err != nil {
		return cfg, fmt.Errorf("config file: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("config file %s: %w", path, err)
	}
	return cfg, nil
}

func parseDur(name, v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("--%s: %w", name, err)
	}
	return d, nil
}

func readSigner(path string) (identity.Signer, error) {
	seed, err := os.ReadFile(path) //nolint:gosec // G304: the seed path is the operator's own flag
	if err != nil {
		return nil, fmt.Errorf("seed file: %w", err)
	}
	s, err := identity.SignerFromSeed(seed)
	if err != nil {
		return nil, fmt.Errorf("seed file %s: %w", path, err)
	}
	return s, nil
}

func buildRunnerConfig(c fileConfig, discover, hb, stream, qwait, ack, maxDur *string) (runner.Config, func(), error) {
	var rc runner.Config
	noop := func() {}
	var err error
	if rc.DiscoverWait, err = parseDur("discover-wait", *discover); err != nil {
		return rc, noop, err
	}
	if rc.HeartbeatWindow, err = parseDur("heartbeat-window", *hb); err != nil {
		return rc, noop, err
	}
	if rc.StreamTimeout, err = parseDur("stream-timeout", *stream); err != nil {
		return rc, noop, err
	}
	if rc.QueryUnansweredWait, err = parseDur("query-unanswered-wait", *qwait); err != nil {
		return rc, noop, err
	}
	if rc.AckDeadline, err = parseDur("ack-deadline", *ack); err != nil {
		return rc, noop, err
	}
	if rc.MaxDuration, err = parseDur("max-duration", *maxDur); err != nil {
		return rc, noop, err
	}
	for name, v := range map[string]int{
		"max-stream-messages": c.Limits.StreamMessages, "max-stream-bytes": c.Limits.StreamBytes,
		"max-discovery": c.Limits.Discovery, "max-targets": c.Limits.Targets,
		"max-heartbeats": c.Limits.Heartbeats, "max-info-probes": c.Limits.InfoProbes,
	} {
		if v < 0 {
			return rc, noop, fmt.Errorf("--%s must not be negative", name)
		}
	}
	rc.MaxStreamMessages, rc.MaxStreamBytes = c.Limits.StreamMessages, c.Limits.StreamBytes
	rc.MaxDiscovery, rc.MaxTargets = c.Limits.Discovery, c.Limits.Targets
	rc.MaxHeartbeats, rc.MaxInfoProbes = c.Limits.Heartbeats, c.Limits.InfoProbes
	for _, p := range c.AnswerQuerySubjectPrefixes {
		if p == "" || strings.ContainsAny(p, " \t\r\n*>") {
			return rc, noop, fmt.Errorf("--answer-query-subject-prefix %q must be a non-empty subject prefix without wildcards or spaces", p)
		}
	}
	rc.AnswerQuerySubjectPrefixes = c.AnswerQuerySubjectPrefixes
	switch c.ExpectMinSenderTrust {
	case "", "none", "any", "signed":
	default:
		return rc, noop, fmt.Errorf("--expect-min-sender-trust must be none, any or signed, not %q", c.ExpectMinSenderTrust)
	}
	if c.ExpectInstances < 0 {
		return rc, noop, errors.New("--expect-instances must not be negative")
	}
	rc.Agent, rc.Owner, rc.Name = c.Agent, c.Owner, c.Name
	rc.ProbePrompt = c.ProbePrompt
	rc.ExpectMinSenderTrust = c.ExpectMinSenderTrust
	rc.SenderAccount = c.SenderAccount
	rc.SessionAware = c.SessionAware
	rc.ExpectInstances = c.ExpectInstances
	rc.ProbeOversize = c.ProbeOversize
	rc.QueryPrompt = c.QueryPrompt
	rc.SlowPrompt = c.SlowPrompt

	if c.SenderSeed != "" {
		if rc.SenderSigner, err = readSigner(c.SenderSeed); err != nil {
			return rc, noop, err
		}
	}
	if c.RefusedSeed != "" {
		if rc.RefusedSigner, err = readSigner(c.RefusedSeed); err != nil {
			return rc, noop, err
		}
	}

	src := connect.Source{Context: c.Context, URL: c.URL, Creds: c.Creds, NKey: c.NKey}
	if src.Context == "" && src.URL == "" {
		src.URL = os.Getenv("NATS_URL")
		if src.URL == "" {
			src.URL = nats.DefaultURL
		}
	}
	var opts []connect.BundleOption
	if c.SenderFromConnection && rc.SenderSigner == nil {
		opts = append(opts, connect.WithSignedIdentity())
	}
	bundle, err := connect.ResolveBundle(context.Background(), src, opts...)
	if err != nil {
		return rc, noop, fmt.Errorf("connection: %w", err)
	}
	if c.SenderFromConnection && rc.SenderSigner == nil {
		rc.SenderSigner = bundle.Signer
	}
	nc, err := nats.Connect(bundle.URL, append(bundle.Options, nats.Name("sapconform"))...)
	if err != nil {
		bundle.Wipe()
		return rc, noop, fmt.Errorf("connection: %w", err)
	}
	rc.Conn = nc
	return rc, func() { nc.Close(); bundle.Wipe() }, nil
}

// writeSummary prints the human report.
func writeSummary(w io.Writer, rep *runner.Report, _ *conformance.Catalog, verbose bool) {
	fmt.Fprintf(w, "sapconform %s against SAP %s (spec commit %s, catalog v%d)\n", rep.ToolVersion, rep.SpecVersion, rep.SpecCommit[:12], rep.CatalogVersion)
	fmt.Fprintf(w, "scope: %s\n", rep.Scope)
	if len(rep.Supplied) > 0 {
		fmt.Fprintf(w, "configuration supplied: %s\n", strings.Join(rep.Supplied, ", "))
	} else {
		fmt.Fprintf(w, "configuration supplied: none\n")
	}
	for _, t := range rep.Targets {
		fmt.Fprintf(w, "\ntarget %s (agent %q, owner %q, %d instance(s))\n", orNone(t.Target.PromptSubject), t.Target.Agent, t.Target.Owner, len(t.Target.InstanceIDs))
		byCap := map[string]map[runner.Result]int{}
		for _, e := range t.Entries {
			if e.Role != conformance.RoleHost && e.Role != conformance.RoleBoth {
				continue
			}
			if byCap[e.Capability] == nil {
				byCap[e.Capability] = map[runner.Result]int{}
			}
			byCap[e.Capability][e.Result]++
		}
		for _, capab := range []string{conformance.CapBlackBox, conformance.CapNeedsCfg, conformance.CapSDK, conformance.CapNA} {
			if m := byCap[capab]; len(m) > 0 {
				fmt.Fprintf(w, "  host entries, %-19s %s\n", capab+":", counts(m))
			}
		}
		fmt.Fprintf(w, "  all catalog entries by result: %s\n", counts(t.Counts))
		order := map[runner.Result]int{runner.Fail: 0, runner.ConfigMissing: 1, runner.Policy: 2, runner.NotApplicable: 3, runner.NotRun: 4, runner.Pass: 5}
		entries := append([]runner.EntryResult(nil), t.Entries...)
		sort.SliceStable(entries, func(i, j int) bool { return order[entries[i].Result] < order[entries[j].Result] })
		for _, e := range entries {
			if e.Role != conformance.RoleHost && e.Role != conformance.RoleBoth {
				continue
			}
			if !verbose && (e.Result == runner.Pass || e.Result == runner.NotRun) {
				continue
			}
			mark := ""
			if e.IsFailure() {
				mark = " ***"
			}
			fmt.Fprintf(w, "  %-22s %-5s %-11s %-8s L%-5d %s%s\n", e.Result, shortLevel(e.Level), e.Capability, e.ID, e.SpecLine, trunc(e.Detail, 110), mark)
		}
	}
	fails := rep.Failures()
	fmt.Fprintln(w)
	for _, n := range rep.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
	if rep.Incomplete != "" {
		fmt.Fprintf(w, "INCOMPLETE: %s\n", rep.Incomplete)
	}
	switch {
	case rep.Incomplete != "":
		fmt.Fprintf(w, "result: the run is incomplete, so it passes no verdict (run took %s)\n", rep.Duration)
	case rep.OK():
		fmt.Fprintf(w, "result: no MUST or MUST NOT entry failed (run took %s)\n", rep.Duration)
	default:
		ids := make([]string, 0, len(fails))
		for _, f := range fails {
			ids = append(ids, f.ID)
		}
		if len(ids) == 0 {
			fmt.Fprintf(w, "result: no target was checked\n")
		} else {
			fmt.Fprintf(w, "result: %d MUST or MUST NOT entr(ies) failed: %s\n", len(ids), strings.Join(ids, ", "))
		}
	}
	if rep.Incomplete == "" && len(rep.Targets) > 0 {
		if v := rep.Verdict; v.Strict {
			fmt.Fprintf(w, "strict: ok (no entry of any level failed, and no entry you supplied configuration for is configuration-missing)\n")
		} else {
			fmt.Fprintf(w, "strict: not ok, %d entr(ies): %s (pass --strict to exit non-zero on these)\n", len(v.StrictFailed), strings.Join(v.StrictFailed, ", "))
		}
	}
}

func orNone(s string) string {
	if s == "" {
		return "(no prompt endpoint)"
	}
	return s
}

func shortLevel(l string) string {
	switch l {
	case "MUST NOT":
		return "M-NOT"
	case "SHOULD NOT":
		return "S-NOT"
	}
	return l
}

func counts(m map[runner.Result]int) string {
	var parts []string
	for _, r := range []runner.Result{runner.Pass, runner.Fail, runner.Policy, runner.ConfigMissing, runner.NotApplicable, runner.NotRun} {
		if m[r] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", m[r], r))
		}
	}
	return strings.Join(parts, ", ")
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
