// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package ci holds guard tests over the repository's own CI configuration.
package ci

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
var gateJobs = map[string]bool{"pr-check": true, "changes": true, "label-reset": true, "adr-lint": true}

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
	Name string            `yaml:"name"`
	ID   string            `yaml:"id"`
	Uses string            `yaml:"uses"`
	With map[string]any    `yaml:"with"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`

	WorkingDirectory string `yaml:"working-directory"`
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
	On          any            `yaml:"on"`
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

// alwaysRun is the allowlist of jobs that do not wait on the changes job: independence is a plain grep over the tracked
// files (docs included, so a docs-only change must still pass it) and runs on every change.
var alwaysRun = map[string]bool{"independence": true}

// checkSkipDocs enforces the rule that CI does no work for a docs-only change and never skips a check. It applies to a
// workflow that has a pr-check: no top-level paths filter on any trigger (a required check that never reports stalls
// the merge queue); a changes job; every heavy job needs changes and is gated on its code output; pr-check runs
// always() and reads the changes result and output, so a skip passes only when changes said docs-only.
func checkSkipDocs(wf workflow, ids []string, bad func(string, ...any)) {
	if on, ok := wf.On.(map[string]any); ok {
		trig := make([]string, 0, len(on))
		for k := range on {
			trig = append(trig, k)
		}
		sort.Strings(trig)
		for _, k := range trig {
			if m, ok := on[k].(map[string]any); ok {
				for _, f := range []string{"paths", "paths-ignore"} {
					if _, has := m[f]; has {
						bad("trigger %s has a %s filter: skip work in a changes job instead, a required check must always report", k, f)
					}
				}
			}
		}
	}
	pc, ok := wf.Jobs["pr-check"]
	if !ok {
		return
	}
	ch, ok := wf.Jobs["changes"]
	if !ok {
		bad("no changes job")
		return
	}
	if len(ch.Steps) == 0 {
		bad("the changes job has no steps")
	}
	const gate = "needs.changes.outputs.code == "
	for _, id := range ids {
		if id == "pr-check" || id == "changes" || alwaysRun[id] {
			continue
		}
		j := wf.Jobs[id]
		have := false
		for _, n := range needsList(j.Needs) {
			have = have || n == "changes"
		}
		if !have {
			bad("job %s does not need changes", id)
		}
		if !strings.Contains(j.If, gate+"'true'") && (id != "docs-guard" || !strings.Contains(j.If, gate+"'false'")) {
			bad("job %s is not gated on the changes code output", id)
		}
	}
	if strings.ReplaceAll(pc.If, " ", "") != "always()" {
		bad("pr-check must run with if: always(), got %q", pc.If)
	}
	var env, run strings.Builder
	for _, s := range pc.Steps {
		run.WriteString(s.Run)
		for _, v := range s.Env {
			env.WriteString(v + "\n")
		}
	}
	if !strings.Contains(env.String(), "needs.changes.result") || !strings.Contains(env.String(), "needs.changes.outputs.code") {
		bad("pr-check does not read the changes result and code output")
	}
	if !strings.Contains(run.String(), "skipped") {
		bad("pr-check does not handle a skipped job")
	}
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
	checkSkipDocs(wf, ids, bad)
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
		b, err := os.ReadFile(f)
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
on:
  push:
    branches: [main]
  pull_request:
jobs:
  changes:
    runs-on: [self-hosted, rh-gate]
    steps:
      - run: "true"
  test:
    runs-on: [self-hosted, rh-linux]
    needs: changes
    if: needs.changes.outputs.code == 'true'
    steps:
      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262
      - uses: actions/setup-go@40f1582b2485089dde7abd97c1529aa768e1baff
        with:
          cache: false
  pr-check:
    needs: [changes, test]
    if: always()
    runs-on: [self-hosted, rh-gate]
    steps:
      - env:
          CHANGES: ${{ needs.changes.result }}
          CODE: ${{ needs.changes.outputs.code }}
        run: '[ "$CHANGES" = success ] && [ "$CODE" = false ] && echo skipped'
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
		{"pr-check missing a need", "needs: [changes, test]", "needs: [changes]", "does not need"},
		{"paths filter", "  pull_request:\n", "  pull_request:\n    paths: ['**.go']\n", "paths filter"},
		{"paths-ignore filter", "  pull_request:\n", "  pull_request:\n    paths-ignore: ['docs/**']\n", "paths-ignore filter"},
		{"heavy job without needs changes", "    needs: changes\n    if:", "    if:", "does not need changes"},
		{"heavy job not gated", "    if: needs.changes.outputs.code == 'true'\n", "", "not gated"},
		{"heavy job gated on the wrong value", "outputs.code == 'true'", "outputs.code == 'false'", "not gated"},
		{"pr-check without always", "    if: always()\n", "", "always()"},
		{"pr-check ignores the changes output", "          CODE: ${{ needs.changes.outputs.code }}\n", "", "code output"},
		{"pr-check ignores the changes result", "          CHANGES: ${{ needs.changes.result }}\n", "", "changes result"},
		{"pr-check does not handle skipped", "&& echo skipped", "", "skipped"},
		{"no changes job", "  changes:\n    runs-on: [self-hosted, rh-gate]\n    steps:\n      - run: \"true\"\n", "", "no changes job"},
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

// TestChangesScript runs .github/scripts/changes.sh, the docs-only classifier, on sample file lists.
func TestChangesScript(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	script, err := filepath.Abs("../../.github/scripts/changes.sh")
	if err != nil {
		t.Fatal(err)
	}
	pr := map[string]string{"EVENT_NAME": "pull_request", "HEAD_REF": "feature", "PR_AUTHOR": "alice", "HEAD_REPO": "o/r", "REPO": "o/r"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range pr {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	tests := []struct {
		name  string
		env   map[string]string
		files []string
		want  string
	}{
		{"readme", pr, []string{"README.md"}, "false"},
		{"docs tree", pr, []string{"docs/parity.md", "docs/decisions/0002-x.md", "docs/decisions/NEXT"}, "false"},
		{"nested markdown", pr, []string{"examples/host-echo/README.md", "cmd/sapconform/README.md", "conformance/README.md"}, "false"},
		{"repo docs files", pr, []string{"LICENSE", "CODE_OF_CONDUCT.md", ".gitignore", "CHANGELOG.md", "RELEASING.md", "CONTRIBUTING.md"}, "false"},
		{"go file", pr, []string{"host/host.go"}, "true"},
		{"go test file", pr, []string{"README.md", "bytes_test.go"}, "true"},
		{"go file under docs", pr, []string{"docs/gen.go"}, "true"},
		{"go.mod", pr, []string{"go.mod"}, "true"},
		{"go.sum in test", pr, []string{"test/go.sum"}, "true"},
		{"workflow", pr, []string{".github/workflows/go-sdk.yml"}, "true"},
		{"github script", pr, []string{".github/scripts/changes.sh"}, "true"},
		{"github markdown", pr, []string{".github/pull_request_template.md"}, "true"},
		{"mergify", pr, []string{".mergify.yml"}, "true"},
		{"golangci", pr, []string{".golangci.yml"}, "true"},
		{"testdata markdown", pr, []string{"testdata/attachments/SOURCE.md"}, "true"},
		{"testdata json", pr, []string{"testdata/attachments/save-names.json"}, "true"},
		{"nested testdata", pr, []string{"conformance/testdata/spec/core-protocol.md"}, "true"},
		{"test-fixtures", pr, []string{"test-fixtures/a.txt"}, "true"},
		{"catalog", pr, []string{"conformance/catalog.json"}, "true"},
		{"mutation patch", pr, []string{"conformance/mutations/F-X.patch"}, "true"},
		{"interop lock", pr, []string{"test/interop/locks/ts-client/package-lock.json"}, "true"},
		{"interop requirements", pr, []string{"test/interop/locks/py/requirements.txt"}, "true"},
		{"interop pin", pr, []string{"test/interop/PINNED"}, "true"},
		{"unknown file", pr, []string{"Makefile"}, "true"},
		{"docs plus code", pr, []string{"docs/parity.md", "attachments.go"}, "true"},
		{"empty diff", pr, nil, "true"},
		{"push to main", with("EVENT_NAME", "push"), []string{"README.md"}, "true"},
		{"merge group", with("EVENT_NAME", "merge_group"), []string{"README.md"}, "true"},
		{"workflow dispatch", with("EVENT_NAME", "workflow_dispatch"), []string{"README.md"}, "true"},
		{"mergify queue draft", with("HEAD_REF", "mergify/merge-queue/abc", "PR_AUTHOR", "mergify[bot]"), []string{"README.md"}, "true"},
		{"queue ref from another author is classified", with("HEAD_REF", "mergify/merge-queue/abc"), []string{"README.md"}, "false"},
		{"queue ref from a fork is classified", with("HEAD_REF", "mergify/merge-queue/abc", "PR_AUTHOR", "mergify[bot]", "HEAD_REPO", "x/r"), []string{"README.md"}, "false"},
		{"quoted path", pr, []string{`"docs/caf\303\251.md"`}, "true"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(bash, script)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
			for k, v := range tc.env {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
			cmd.Stdin = strings.NewReader(strings.Join(tc.files, "\n") + "\n")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != "code="+tc.want {
				t.Fatalf("got %q, want code=%s", got, tc.want)
			}
		})
	}
}

// docsReaders lists, by package directory, the tests that read documentation (a Markdown file, anything under docs/)
// and so can break on a docs-only change. An empty list means every test of the package. The docs-guard job of
// go-sdk.yml must run all of them.
//
// Why a package list and not a per-function scan: a test names its file through consts, helpers and joined path
// elements (parityPath, filepath.Join(root, "docs", "decisions")), and a table test such as adrlint's names dozens of
// *.md files that live only in memory. Tracing which function really reads the disk would be fragile. So the scan
// below finds the packages that mention documentation at all, and this list, which a person keeps, names the readers.
var docsReaders = map[string][]string{
	"internal/cmd/adrlint": {"TestRepositoryADRsPass"},
	"test/docs":            nil,
	// TestQuickstart_ReadmeMatchesExample arrives with the examples change; a -run on a name that does not exist
	// passes, so docs-guard names it before it lands.
	"test/examples": {"TestQuickstart_ReadmeMatchesExample"},
}

// docsExempt lists test files that mention a documentation-looking name without reading a documentation file, each
// with the reason. A new entry needs one.
var docsExempt = map[string]string{
	"attachments_test.go":       "builds a file name ending in .md for the safe-name check; reads testdata/, which counts as code",
	"test/ci/workflows_test.go": "the changes.sh table names sample files such as README.md; reads no documentation",
}

// docLiteral reports whether a string literal of a test names documentation: a Markdown file, docs, a docs/ path,
// LICENSE or .gitignore, outside a testdata or test-fixtures directory (changes.sh counts those as code).
func docLiteral(v string) bool {
	if strings.Contains(v, "testdata") || strings.Contains(v, "test-fixtures") {
		return false
	}
	return strings.HasSuffix(v, ".md") || v == "docs" || strings.HasPrefix(v, "docs/") || strings.Contains(v, "/docs/") ||
		v == "LICENSE" || v == ".gitignore"
}

// docsRun is one package of one `go test` line of the docs-guard job: its directory relative to the repository root,
// the line's -run expression ("" when none) and whether the package was a /... pattern.
type docsRun struct {
	dir     string
	run     string
	subtree bool
}

// docsGuardRuns lists every package the docs-guard job tests.
func docsGuardRuns(t *testing.T, wf workflow) []docsRun {
	t.Helper()
	j, ok := wf.Jobs["docs-guard"]
	if !ok {
		t.Fatal("go-sdk.yml has no docs-guard job")
	}
	var out []docsRun
	for _, s := range j.Steps {
		for _, line := range strings.Split(s.Run, "\n") {
			f := strings.Fields(line)
			if len(f) < 2 || f[0] != "go" || f[1] != "test" {
				continue
			}
			run := ""
			var pkgs []string
			for i := 2; i < len(f); i++ {
				switch {
				case f[i] == "-run" && i+1 < len(f):
					i++
					run = strings.Trim(f[i], `'"`)
				case strings.HasPrefix(f[i], "./"):
					pkgs = append(pkgs, f[i])
				}
			}
			for _, p := range pkgs {
				sub := strings.HasSuffix(p, "/...")
				d := path.Join(s.WorkingDirectory, strings.TrimSuffix(strings.TrimSuffix(p, "..."), "/"))
				out = append(out, docsRun{dir: d, run: run, subtree: sub})
			}
		}
	}
	return out
}

// docsPackages returns the package directories (relative to root, slash-separated) of the *_test.go files that carry
// a documentation literal, mapped to the files that do.
func docsPackages(t *testing.T, root string) map[string][]string {
	t.Helper()
	found := map[string][]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); p != root && (strings.HasPrefix(n, ".") || n == "testdata" || n == "test-fixtures" || n == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if _, ok := docsExempt[rel]; ok {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			return err
		}
		hit := false
		ast.Inspect(f, func(n ast.Node) bool {
			if l, ok := n.(*ast.BasicLit); ok && l.Kind == token.STRING {
				if v, err := strconv.Unquote(l.Value); err == nil && docLiteral(v) {
					hit = true
				}
			}
			return !hit
		})
		if hit {
			dir := path.Dir(rel)
			found[dir] = append(found[dir], rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestDocsLiteral(t *testing.T) {
	for v, want := range map[string]bool{
		"README.md": true, "../../README.md": true, "docs": true, "docs/parity.md": true, "../../docs/x": true, "LICENSE": true,
		"testdata/SOURCE.md": false, "conformance/testdata/spec/core-protocol.md": false, "main.go": false, "documents": false,
	} {
		if got := docLiteral(v); got != want {
			t.Errorf("docLiteral(%q) = %v, want %v", v, got, want)
		}
	}
}

// Every test that reads documentation runs in the docs-guard job, so a docs-only change, which skips the heavy jobs,
// still runs it. A test file that names a Markdown file or a docs/ path fails here unless its package is run by
// docs-guard (and, where docs-guard narrows the package with -run, the readers are matched), or is in docsExempt.
func TestDocsGuard_RunsEveryTestThatReadsDocs(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/go-sdk.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	runs := docsGuardRuns(t, wf)
	if len(runs) == 0 {
		t.Fatal("docs-guard runs no go test")
	}
	// covered reports whether the docs-guard runs of a package cover the named test ("" for the whole package).
	covered := func(dir, test string) bool {
		for _, r := range runs {
			inTree := r.subtree && (r.dir == "." || strings.HasPrefix(dir, r.dir+"/"))
			if r.dir != dir && !inTree {
				continue
			}
			if r.run == "" {
				return true
			}
			if test == "" {
				continue
			}
			if re, err := regexp.Compile(r.run); err == nil && re.MatchString(test) {
				return true
			}
		}
		return false
	}
	pkgs := docsPackages(t, "../..")
	if len(pkgs["test/docs"]) == 0 {
		t.Fatal("the scan found no documentation literal in test/docs; the scan is broken")
	}
	var dirs []string
	for d := range pkgs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		readers, ok := docsReaders[d]
		if !ok {
			t.Errorf("%s names documentation (%s) but is not in docsReaders: add it, with the tests that read the files, and run them in docs-guard, or exempt the file with a reason in docsExempt", d, strings.Join(pkgs[d], ", "))
			continue
		}
		if len(readers) == 0 && !covered(d, "") {
			t.Errorf("docs-guard does not run all of %s", d)
		}
		for _, r := range readers {
			if !covered(d, r) {
				t.Errorf("docs-guard does not run %s in %s", r, d)
			}
		}
	}
	for d := range docsReaders {
		if _, ok := pkgs[d]; !ok && d != "test/examples" {
			t.Errorf("docsReaders names %s, which has no documentation literal: remove it", d)
		}
	}
	for f, why := range docsExempt {
		if _, err := os.Stat(filepath.Join("../..", f)); err != nil {
			t.Errorf("docsExempt names %s (%s), which is gone: %v", f, why, err)
		}
	}
}

// classifyStep returns the run script of the changes job's classify step in a workflow file, or "" if it has none.
func classifyStep(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("../../.github/workflows", file))
	if err != nil {
		t.Fatal(err)
	}
	var wf workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	for _, s := range wf.Jobs["changes"].Steps {
		if s.ID == "classify" {
			return s.Run
		}
	}
	return ""
}

// TestChangesStep_ClassifierComesFromTheBase pins the trust boundary of the changes job: the pull request must not be
// able to rewrite the script that decides whether it is checked. For every workflow with a changes job it checks the
// step text, then runs the step itself in a throwaway repository.
func TestChangesStep_ClassifierComesFromTheBase(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	realScript, err := os.ReadFile("../../.github/scripts/changes.sh")
	if err != nil {
		t.Fatal(err)
	}
	const script = ".github/scripts/changes.sh"
	for _, file := range []string{"go-sdk.yml", "interop.yml"} {
		run := classifyStep(t, file)
		if run == "" {
			t.Fatalf("%s: the changes job has no classify step", file)
		}
		t.Run(file+"/text", func(t *testing.T) {
			for _, bad := range []string{"| " + script, "bash " + script, "sh " + script, "\n" + script} {
				if strings.Contains(run, bad) {
					t.Errorf("the step runs the classifier from the working tree (%q)", bad)
				}
			}
			if !strings.Contains(run, `git show "$BASE_SHA:`+script+`"`) {
				t.Error("the step does not read the classifier with git show \"$BASE_SHA:" + script + "\"")
			}
			if !strings.Contains(run, `grep -q '^\.github/'`) {
				t.Error("the step has no .github/ override")
			}
		})

		// repo builds a repository whose base commit holds baseScript (nil: none), and whose head commit adds the
		// named files. tree, when set, then overwrites the script in the working tree without committing it.
		type repoSpec struct {
			baseScript []byte
			headFiles  map[string]string
			tree       string
		}
		g := func(t *testing.T, dir string, args ...string) string {
			t.Helper()
			cmd := exec.Command(git, args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
				"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		write := func(t *testing.T, dir, name, content string) {
			t.Helper()
			p := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		// classify runs the step and returns what it appended to GITHUB_OUTPUT.
		classify := func(t *testing.T, event string, spec repoSpec) string {
			t.Helper()
			dir := t.TempDir()
			g(t, dir, "init", "-q")
			write(t, dir, "README.md", "base\n")
			if spec.baseScript != nil {
				write(t, dir, script, string(spec.baseScript))
			}
			g(t, dir, "add", "-A")
			g(t, dir, "commit", "-q", "-m", "base")
			base := g(t, dir, "rev-parse", "HEAD")
			for n, c := range spec.headFiles {
				write(t, dir, n, c)
			}
			g(t, dir, "add", "-A")
			g(t, dir, "commit", "-q", "-m", "head")
			head := g(t, dir, "rev-parse", "HEAD")
			if spec.tree != "" {
				write(t, dir, script, spec.tree)
			}
			tmp, out := t.TempDir(), filepath.Join(t.TempDir(), "out")
			cmd := exec.Command(bash, "-c", run)
			cmd.Dir = dir
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "EVENT_NAME=" + event, "HEAD_REF=feature", "PR_AUTHOR=alice",
				"HEAD_REPO=o/r", "REPO=o/r", "BASE_SHA=" + base, "HEAD_SHA=" + head, "RUNNER_TEMP=" + tmp, "GITHUB_OUTPUT=" + out}
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("step failed: %v: %s", err, b)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			return strings.TrimSpace(string(got))
		}
		saysFalse := []byte("#!/usr/bin/env bash\necho code=false\n")
		tests := []struct {
			name  string
			event string
			spec  repoSpec
			want  string
		}{
			{"docs-only change, real classifier on the base", "pull_request", repoSpec{realScript, map[string]string{"README.md": "new\n"}, ""}, "code=false"},
			{"code change, real classifier on the base", "pull_request", repoSpec{realScript, map[string]string{"a.go": "package a\n"}, ""}, "code=true"},
			{"base has no classifier", "pull_request", repoSpec{nil, map[string]string{"README.md": "new\n"}, ""}, "code=true"},
			{"a working-tree classifier is not run", "pull_request", repoSpec{realScript, map[string]string{"README.md": "new\n"}, "#!/usr/bin/env bash\necho code=true\n"}, "code=false"},
			{"pull request rewrites the classifier to say false", "pull_request", repoSpec{realScript, map[string]string{"README.md": "new\n", script: string(saysFalse)}, ""}, "code=true"},
			{"a .github change is code even when the base classifier says false", "pull_request", repoSpec{saysFalse, map[string]string{".github/x": "x\n"}, ""}, "code=true"},
			{"the base classifier's false is honored", "pull_request", repoSpec{saysFalse, map[string]string{"README.md": "new\n"}, ""}, "code=false"},
			{"a base classifier that fails is code", "pull_request", repoSpec{[]byte("#!/usr/bin/env bash\nexit 3\n"), map[string]string{"README.md": "new\n"}, ""}, "code=true"},
			{"not a pull request", "push", repoSpec{realScript, map[string]string{"README.md": "new\n"}, ""}, "code=true"},
		}
		for _, tc := range tests {
			t.Run(file+"/"+tc.name, func(t *testing.T) {
				if got := classify(t, tc.event, tc.spec); got != tc.want {
					t.Fatalf("GITHUB_OUTPUT = %q, want %q", got, tc.want)
				}
			})
		}
	}
}

// TestDocsGuard_NamedTestsMustRun: go test -run passes when no test matches, so every `go test -run '^Name$'` of the
// docs-guard job must be followed by a check that `--- PASS: Name` was printed.
func TestDocsGuard_NamedTestsMustRun(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/go-sdk.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	named := regexp.MustCompile(`-run '\^(\w+)\$'`)
	n := 0
	for _, s := range wf.Jobs["docs-guard"].Steps {
		for _, m := range named.FindAllStringSubmatch(s.Run, -1) {
			n++
			if !strings.Contains(s.Run, "-v") || !strings.Contains(s.Run, "grep -q '^--- PASS: "+m[1]+" '") {
				t.Errorf("docs-guard runs %s with -run but does not require `--- PASS: %s` in a -v output", m[1], m[1])
			}
		}
	}
	if n == 0 {
		t.Error("docs-guard names no test with -run")
	}
}

// TestDocsGuard_StepsFailWhenACallFails runs each docs-guard script under bash with a stub `go` first on PATH, so a
// failing `go test` (or a named test that never printed PASS) cannot be masked by the lines after it.
func TestDocsGuard_StepsFailWhenACallFails(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/go-sdk.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	// The stub prints `--- PASS: <name> ` for a -run call unless STUB_NO_PASS is set, and exits 1 for the call
	// STUB_FAIL names ("run" for a -run call, "docs" for the ./docs/... call).
	const stub = `#!/bin/sh
case "$*" in
*-run*)
  name=$(printf '%s' "$*" | sed -n "s/.*-run '\{0,1\}^\([A-Za-z0-9_]*\)\$.*/\1/p")
  [ -n "$STUB_NO_PASS" ] || echo "--- PASS: $name (0.00s)"
  [ "$STUB_FAIL" = run ] && exit 1
  ;;
*./docs/...*)
  [ "$STUB_FAIL" = docs ] && exit 1
  ;;
esac
exit 0
`
	cases := []struct {
		step    string
		name    string
		fail    string
		noPass  bool
		wantErr bool
	}{
		{"Docs tests (root module)", "ok", "", false, false},
		{"Docs tests (root module)", "run call fails but prints PASS", "run", false, true},
		{"Docs tests (root module)", "test missing", "", true, true},
		{"Docs tests (integration module)", "ok", "", false, false},
		{"Docs tests (integration module)", "docs call fails", "docs", false, true},
		{"Docs tests (integration module)", "run call fails but prints PASS", "run", false, true},
		{"Docs tests (integration module)", "test missing", "", true, true},
	}
	seen := map[string]bool{}
	for _, s := range wf.Jobs["docs-guard"].Steps {
		if !strings.HasPrefix(s.Name, "Docs tests") {
			continue
		}
		seen[s.Name] = true
		var first string
		for _, l := range strings.Split(s.Run, "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
				first = l
				break
			}
		}
		if first != "set -euo pipefail" {
			t.Errorf("%s: first command is %q, want `set -euo pipefail`", s.Name, first)
		}
		for _, c := range cases {
			if c.step != s.Name {
				continue
			}
			t.Run(s.Name+"/"+c.name, func(t *testing.T) {
				dir := t.TempDir()
				bin := filepath.Join(dir, "bin")
				if err := os.Mkdir(bin, 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(bin, "go"), []byte(stub), 0o700); err != nil { //nolint:gosec // the stub must be executable
					t.Fatal(err)
				}
				cmd := exec.Command("bash", "-c", s.Run)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "RUNNER_TEMP="+dir, "STUB_FAIL="+c.fail)
				if c.noPass {
					cmd.Env = append(cmd.Env, "STUB_NO_PASS=1")
				}
				out, err := cmd.CombinedOutput()
				if (err != nil) != c.wantErr {
					t.Errorf("exit error = %v, want failure %v\n%s", err, c.wantErr, out)
				}
			})
		}
	}
	for _, name := range []string{"Docs tests (root module)", "Docs tests (integration module)"} {
		if !seen[name] {
			t.Errorf("docs-guard has no step %q", name)
		}
	}
}
