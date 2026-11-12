// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package ci holds guard tests over the repository's own CI configuration.
package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The repository's CI uses exactly two runner label sets and nothing else:
// [self-hosted, rh-linux] for build and test jobs, and [self-hosted, rh-gate]
// for the light gate jobs, which must never queue behind a test job.
const (
	linuxLabels = "[self-hosted rh-linux]"
	gateLabels  = "[self-hosted rh-gate]"
)

// gateJobs is the allowlist of jobs that may run on rh-gate: the pr-check aggregate and the two pull_request_target
// gate workflows (label-reset.yml, adr-lint.yml), which check out and run no pull request code.
var gateJobs = map[string]bool{"pr-check": true, "label-reset": true, "adr-lint": true}

// noPrCheck is the allowlist of workflow files that have no pr-check job: each is a single-job gate whose job is
// itself a required check (named in .mergify.yml), except release.yml, which runs on a tag push and gates no pull
// request. Every other workflow must have a pr-check that needs every job.
var noPrCheck = map[string]bool{"label-reset.yml": true, "adr-lint.yml": true, "release.yml": true}

// jobWrites is the allowlist of job-level permission grants above read, as "file/job/permission". The top-level
// permissions of every workflow stay read-only; label-reset edits labels and the release workflow
// creates a release and attests provenance; nothing else writes.
var jobWrites = map[string]bool{
	"label-reset.yml/label-reset/pull-requests": true,
	// release.yml (ADR 0002): the release job creates the GitHub release; the attest job signs build provenance.
	"release.yml/release/contents":    true,
	"release.yml/attest/id-token":     true,
	"release.yml/attest/attestations": true,
}

var shaPin = regexp.MustCompile(`@[0-9a-f]{40}$`)

type step struct {
	Uses string         `yaml:"uses"`
	With map[string]any `yaml:"with"`
}

type job struct {
	RunsOn      any    `yaml:"runs-on"`
	Needs       any    `yaml:"needs"`
	Steps       []step `yaml:"steps"`
	Uses        string `yaml:"uses"`
	Permissions any    `yaml:"permissions"`
	If          string `yaml:"if"`
}

type workflow struct {
	Permissions any            `yaml:"permissions"`
	Jobs        map[string]job `yaml:"jobs"`
}

// labelString renders a runs-on value as a list of strings, or "" when it is
// not a list of plain strings (a string, an expression, a mapping).
func labelString(v any) string {
	l, ok := v.([]any)
	if !ok {
		return ""
	}
	for _, e := range l {
		if _, ok := e.(string); !ok {
			return ""
		}
	}
	return fmt.Sprint(l)
}

func needsList(v any) []string {
	switch n := v.(type) {
	case string:
		return []string{n}
	case []any:
		var out []string
		for _, e := range n {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// checkWorkflow returns every violation in one workflow file (name is its base name).
func checkWorkflow(name, text string) []string {
	var wf workflow
	if err := yaml.Unmarshal([]byte(text), &wf); err != nil {
		return []string{fmt.Sprintf("%s: not valid YAML: %v", name, err)}
	}
	var out []string
	bad := func(format string, a ...any) {
		out = append(out, name+": "+fmt.Sprintf(format, a...))
	}

	// Top-level permissions must exist and grant nothing beyond read.
	switch p := wf.Permissions.(type) {
	case nil:
		bad("no top-level permissions")
	case string:
		if p != "read-all" {
			bad("permissions %q is not read-only", p)
		}
	case map[string]any:
		for k, v := range p {
			if v != "read" && v != "none" {
				bad("permission %s: %v grants more than read", k, v)
			}
		}
	default:
		bad("permissions has an unexpected form")
	}

	ids := make([]string, 0, len(wf.Jobs))
	for id := range wf.Jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		j := wf.Jobs[id]
		switch p := j.Permissions.(type) {
		case nil:
		case map[string]any:
			keys := make([]string, 0, len(p))
			for k := range p {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if v := p[k]; v != "read" && v != "none" && !jobWrites[name+"/"+id+"/"+k] {
					bad("job %s: permission %s: %v grants more than read and is not in the allowlist", id, k, v)
				}
			}
		default:
			bad("job %s: permissions has an unexpected form (a map is required)", id)
		}
		labels := labelString(j.RunsOn)
		if labels != linuxLabels && labels != gateLabels {
			bad("job %s: runs-on %v is not [self-hosted, rh-linux] or [self-hosted, rh-gate]", id, j.RunsOn)
		}
		if labels == gateLabels && !gateJobs[id] {
			bad("job %s runs on rh-gate but is not in the gate allowlist", id)
		}
		for _, s := range j.Steps {
			if s.Uses != "" && !shaPin.MatchString(s.Uses) {
				bad("job %s: %q is not pinned to a 40-hex SHA", id, s.Uses)
			}
			if strings.HasPrefix(s.Uses, "actions/setup-go@") && s.With["cache"] != false {
				bad("job %s: actions/setup-go lacks cache: false", id)
			}
		}
		if j.Uses != "" && !shaPin.MatchString(j.Uses) && !strings.HasPrefix(j.Uses, "./") {
			bad("job %s: reusable workflow %q is not pinned to a 40-hex SHA", id, j.Uses)
		}
	}

	// pr-check is the single required check and must need every other job. A workflow on the noPrCheck allowlist is a
	// single-job gate with no pr-check; it must still hold exactly the job it is allowed to be.
	if pc, ok := wf.Jobs["pr-check"]; !ok {
		if !noPrCheck[name] {
			bad("no pr-check job")
		}
	} else {
		have := map[string]bool{}
		for _, n := range needsList(pc.Needs) {
			have[n] = true
		}
		for _, id := range ids {
			if id != "pr-check" && !have[id] {
				bad("pr-check does not need job %s", id)
			}
		}
	}
	return out
}

// workflowFiles lists every workflow file in dir: GitHub Actions runs both
// .yml and .yaml, so the guard must inspect both.
func workflowFiles(dir string) ([]string, error) {
	var all []string
	for _, ext := range []string{"*.yml", "*.yaml"} {
		f, err := filepath.Glob(filepath.Join(dir, ext))
		if err != nil {
			return nil, err
		}
		all = append(all, f...)
	}
	return all, nil
}

func TestWorkflows_GuardInspectsYAMLExtension(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.yml", "b.yaml", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := workflowFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[filepath.Base(f)] = true
	}
	if len(got) != 2 || !got["a.yml"] || !got["b.yaml"] {
		t.Fatalf("workflowFiles = %v, want a.yml and b.yaml only", got)
	}
	// A .yaml workflow gets the same checks as a .yml one.
	if v := checkWorkflow("b.yaml", strings.Replace(goodWorkflow, "contents: read", "contents: write", 1)); len(v) == 0 {
		t.Fatal("a .yaml workflow with write permissions was accepted")
	}
}

func TestWorkflows_Guard(t *testing.T) {
	files, err := workflowFiles("../../.github/workflows")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no workflow files found")
	}
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // the test reads a fixture or temp path it built itself
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range checkWorkflow(filepath.Base(f), string(b)) {
			t.Error(v)
		}
	}
}

const goodWorkflow = `
permissions:
  contents: read
jobs:
  test:
    runs-on: [self-hosted, rh-linux]
    steps:
      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262
      - uses: actions/setup-go@40f1582b2485089dde7abd97c1529aa768e1baff
        with:
          cache: false
  pr-check:
    needs: [test]
    runs-on: [self-hosted, rh-gate]
    steps:
      - run: "true"
`

func TestWorkflows_GuardSelfTest(t *testing.T) {
	if v := checkWorkflow("good", goodWorkflow); len(v) != 0 {
		t.Fatalf("known-good workflow rejected: %v", v)
	}
	tests := []struct {
		name string
		from string
		to   string
		want string
	}{
		{"runs-on string", "runs-on: [self-hosted, rh-linux]", "runs-on: ubuntu-latest", "runs-on"},
		{"runs-on expression", "runs-on: [self-hosted, rh-linux]", "runs-on: ${{ matrix.os }}", "runs-on"},
		{"runs-on other label", "runs-on: [self-hosted, rh-linux]", "runs-on: [self-hosted, rh-other]", "runs-on"},
		{"runs-on extra label", "runs-on: [self-hosted, rh-linux]", "runs-on: [self-hosted, rh-linux, x64]", "runs-on"},
		{"gate job outside allowlist", "  test:\n    runs-on: [self-hosted, rh-linux]", "  test:\n    runs-on: [self-hosted, rh-gate]", "allowlist"},
		{"unpinned tag", "actions/checkout@11d5960a326750d5838078e36cf38b85af677262", "actions/checkout@v4", "pinned"},
		{"short sha", "actions/checkout@11d5960a326750d5838078e36cf38b85af677262", "actions/checkout@11d5960", "pinned"},
		{"setup-go without cache false", "cache: false", "cache: true", "cache: false"},
		{"pr-check missing a need", "needs: [test]", "needs: []", "does not need"},
		{"no permissions", "permissions:\n  contents: read\n", "", "permissions"},
		{"write permission", "contents: read", "contents: write", "grants more"},
		{"write-all", "permissions:\n  contents: read\n", "permissions: write-all\n", "not read-only"},
		{"job-level write", "  test:\n    runs-on: [self-hosted, rh-linux]", "  test:\n    permissions:\n      contents: write\n    runs-on: [self-hosted, rh-linux]", "not in the allowlist"},
		{"job-level write-all", "  test:\n    runs-on: [self-hosted, rh-linux]", "  test:\n    permissions: write-all\n    runs-on: [self-hosted, rh-linux]", "unexpected form"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(goodWorkflow, tc.from) {
				t.Fatalf("self-test is stale: %q not in the base workflow", tc.from)
			}
			v := checkWorkflow("bad", strings.Replace(goodWorkflow, tc.from, tc.to, 1))
			if len(v) == 0 {
				t.Fatal("bad workflow accepted")
			}
			if !strings.Contains(strings.Join(v, "\n"), tc.want) {
				t.Fatalf("violations %v do not mention %q", v, tc.want)
			}
		})
	}
	t.Run("single-job gate without pr-check", func(t *testing.T) {
		const gate = "permissions:\n  contents: read\njobs:\n  adr-lint:\n    runs-on: [self-hosted, rh-gate]\n    steps:\n      - run: \"true\"\n"
		if v := checkWorkflow("adr-lint.yml", gate); len(v) != 0 {
			t.Fatalf("allowlisted gate workflow rejected: %v", v)
		}
		if v := checkWorkflow("other.yml", gate); len(v) == 0 {
			t.Fatal("a workflow outside the allowlist without pr-check was accepted")
		}
		if v := checkWorkflow("adr-lint.yml", strings.Replace(gate, "adr-lint:", "other-job:", 1)); len(v) == 0 {
			t.Fatal("a gate job outside the rh-gate allowlist was accepted")
		}
	})
	t.Run("label-reset may hold pull-requests write, nothing else", func(t *testing.T) {
		const lr = "permissions:\n  contents: read\njobs:\n  label-reset:\n    permissions:\n      pull-requests: write\n      contents: read\n    runs-on: [self-hosted, rh-gate]\n    steps:\n      - run: \"true\"\n"
		if v := checkWorkflow("label-reset.yml", lr); len(v) != 0 {
			t.Fatalf("label-reset rejected: %v", v)
		}
		if v := checkWorkflow("adr-lint.yml", lr); len(v) == 0 {
			t.Fatal("pull-requests: write accepted outside label-reset.yml")
		}
		if v := checkWorkflow("label-reset.yml", strings.Replace(lr, "contents: read", "contents: write", 1)); len(v) == 0 {
			t.Fatal("contents: write accepted")
		}
	})
	t.Run("release.yml may write only in its release and attest jobs", func(t *testing.T) {
		const rel = "permissions:\n  contents: read\njobs:\n  attest:\n    permissions:\n      id-token: write\n      attestations: write\n      contents: read\n    runs-on: [self-hosted, rh-linux]\n    steps:\n      - run: \"true\"\n  release:\n    permissions:\n      contents: write\n    runs-on: [self-hosted, rh-linux]\n    steps:\n      - run: \"true\"\n"
		if v := checkWorkflow("release.yml", rel); len(v) != 0 {
			t.Fatalf("release.yml rejected: %v", v)
		}
		if v := checkWorkflow("go-sdk.yml", rel); len(v) == 0 {
			t.Fatal("release permissions accepted outside release.yml")
		}
		if v := checkWorkflow("release.yml", strings.Replace(rel, "  release:\n    permissions:\n      contents: write", "  release:\n    permissions:\n      id-token: write", 1)); len(v) == 0 {
			t.Fatal("id-token: write accepted in the release job")
		}
	})
	t.Run("no pr-check job", func(t *testing.T) {
		bad := goodWorkflow[:strings.Index(goodWorkflow, "  pr-check:")]
		if v := checkWorkflow("bad", bad); len(v) == 0 {
			t.Fatal("workflow without pr-check accepted")
		}
	})
}

// adr-lint skips Mergify's merge-queue drafts, and only those: the job-level if needs the head ref prefix, the same-repo
// head and the Mergify bot as author, all three. Any one alone would let a pull request opt out of the check.
func TestADRLint_skipsOnlyMergifyQueueDrafts(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/adr-lint.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	cond := wf.Jobs["adr-lint"].If
	if !strings.HasPrefix(cond, "${{ !(") || !strings.HasSuffix(cond, ") }}") {
		t.Fatalf("the adr-lint if must be a negated group, got %q", cond)
	}
	for _, want := range []string{
		"startsWith(github.event.pull_request.head.ref, 'mergify/merge-queue/')",
		"github.event.pull_request.head.repo.full_name == github.repository",
		"github.event.pull_request.user.login == 'mergify[bot]'",
	} {
		if !strings.Contains(cond, want) {
			t.Errorf("the adr-lint if lacks %q: %s", want, cond)
		}
	}
	if n := strings.Count(cond, "&&"); n != 2 || strings.Contains(cond, "||") {
		t.Errorf("the three conditions must be joined by && only, got %q", cond)
	}
}

// The ruleset requires "Mergify Merge Protections", so .mergify.yml must carry a merge_protections rule that holds a
// merge outside the queue to the queue's PR-level review gates (approval, hold), scoped to main, with Mergify's own
// queue drafts exempt. It must carry no check condition: Mergify applies those to the queue draft's commit, where
// codex-review never runs, so the queue would stall.
func TestMergify_mergeProtectionsMirrorTheQueueGates(t *testing.T) {
	raw, err := os.ReadFile("../../.mergify.yml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MergeProtections []struct {
			Name              string `yaml:"name"`
			If                []any  `yaml:"if"`
			SuccessConditions []any  `yaml:"success_conditions"`
		} `yaml:"merge_protections"`
		Settings map[string]any `yaml:"merge_protections_settings"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Settings == nil {
		t.Error("merge_protections_settings is gone")
	}
	if len(cfg.MergeProtections) != 1 {
		t.Fatalf("want one merge_protections rule, got %d", len(cfg.MergeProtections))
	}
	r := cfg.MergeProtections[0]
	succ := fmt.Sprint(r.SuccessConditions)
	if !strings.Contains(fmt.Sprint(r.If), "base = main") {
		t.Errorf("the rule's if lacks base = main: %v", r.If)
	}
	for _, want := range []string{"label != hold", "approved-reviews-by = randybias", "label = lead-approved"} {
		if !strings.Contains(succ, want) {
			t.Errorf("the rule's success_conditions lack %q: %s", want, succ)
		}
	}
	var hasCheck func(c any) bool
	hasCheck = func(c any) bool {
		switch v := c.(type) {
		case string:
			return strings.HasPrefix(v, "check-")
		case []any:
			for _, x := range v {
				if hasCheck(x) {
					return true
				}
			}
		case map[string]any:
			for _, x := range v {
				if hasCheck(x) {
					return true
				}
			}
		}
		return false
	}
	if hasCheck(r.SuccessConditions) {
		t.Errorf("the rule's success_conditions carry a check condition (Mergify applies check conditions to the "+
			"queue draft, where codex-review never runs, so the queue stalls): %s", succ)
	}

	// The queue-draft exemption needs both the head ref and Mergify's bot as author; either alone is attacker-chosen
	// (the branch name) or absent, so only (queue head AND mergify[bot]) leaves the rule inactive.
	for _, c := range []struct {
		head, author string
		applies      bool
	}{
		{"mergify/merge-queue/x", "mergify[bot]", false},
		{"mergify/merge-queue/x", "someone", true},
		{"feature/x", "mergify[bot]", true},
		{"feature/x", "someone", true},
	} {
		attrs := map[string]string{"base": "main", "head": c.head, "author": c.author}
		if got := evalMergify(t, r.If, attrs); got != c.applies {
			t.Errorf("head %q author %q: rule applies = %v, want %v", c.head, c.author, got, c.applies)
		}
	}
}

// evalMergify evaluates a Mergify condition tree: a list is an AND, `and:`, `or:` and `not:` are maps, and a string is
// `[-]attr (=|!=|~=) value` where a leading `-` negates it.
func evalMergify(t *testing.T, c any, attrs map[string]string) bool {
	t.Helper()
	switch v := c.(type) {
	case []any:
		for _, x := range v {
			if !evalMergify(t, x, attrs) {
				return false
			}
		}
		return true
	case map[string]any:
		if len(v) != 1 {
			t.Fatalf("unsupported condition map %v", v)
		}
		for k, x := range v {
			switch k {
			case "not":
				return !evalMergify(t, x, attrs)
			case "and":
				return evalMergify(t, x, attrs)
			case "or":
				list, ok := x.([]any)
				if !ok {
					t.Fatalf("or needs a list, got %v", x)
				}
				for _, y := range list {
					if evalMergify(t, y, attrs) {
						return true
					}
				}
				return false
			}
			t.Fatalf("unsupported condition key %q", k)
		}
	case string:
		m := regexp.MustCompile(`^(-?)([a-z-]+) (~=|!=|=) (.+)$`).FindStringSubmatch(v)
		if m == nil {
			t.Fatalf("unsupported condition %q", v)
		}
		var res bool
		switch m[3] {
		case "~=":
			res = regexp.MustCompile(m[4]).MatchString(attrs[m[2]])
		case "=":
			res = attrs[m[2]] == m[4]
		case "!=":
			res = attrs[m[2]] != m[4]
		}
		return res != (m[1] == "-")
	}
	t.Fatalf("unsupported condition %v", c)
	return false
}

// The merge_protections rule's success_conditions are the queue's PR-level review gates (the approval group and the
// hold label), so changing one without the other fails; codex-review stays in queue_conditions only. Both are compared
// as parsed values, `-label = hold` and `label != hold` being the same condition.
func TestMergify_mergeProtectionsMatchQueueReviewGates(t *testing.T) {
	raw, err := os.ReadFile("../../.mergify.yml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MergeProtections []struct {
			SuccessConditions []any `yaml:"success_conditions"`
		} `yaml:"merge_protections"`
		QueueRules []struct {
			Name            string `yaml:"name"`
			QueueConditions []any  `yaml:"queue_conditions"`
		} `yaml:"queue_rules"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.MergeProtections) != 1 || len(cfg.QueueRules) == 0 || cfg.QueueRules[0].Name != "default" {
		t.Fatal("want one merge_protections rule and the default queue rule first")
	}
	norm := func(c any) string {
		if s, ok := c.(string); ok {
			if s == "-label = hold" {
				return "label != hold"
			}
			return s
		}
		m, ok := c.(map[string]any)
		if !ok {
			return fmt.Sprint(c)
		}
		list, _ := m["or"].([]any)
		var parts []string
		for _, x := range list {
			parts = append(parts, fmt.Sprint(x))
		}
		sort.Strings(parts)
		return "or(" + strings.Join(parts, ",") + ")"
	}
	gate := func(s string) bool {
		return s == "label != hold" || strings.HasPrefix(s, "or(")
	}
	var queue, rule []string
	for _, c := range cfg.QueueRules[0].QueueConditions {
		if n := norm(c); gate(n) {
			queue = append(queue, n)
		}
	}
	for _, c := range cfg.MergeProtections[0].SuccessConditions {
		rule = append(rule, norm(c))
	}
	sort.Strings(queue)
	sort.Strings(rule)
	if len(rule) != 2 || strings.Join(queue, "\n") != strings.Join(rule, "\n") {
		t.Errorf("merge_protections success_conditions %v differ from the queue's review gates %v", rule, queue)
	}
}
