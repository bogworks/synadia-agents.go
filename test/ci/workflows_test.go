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
// itself a required check (named in .mergify.yml). Every other workflow must have a pr-check that needs every job.
var noPrCheck = map[string]bool{"label-reset.yml": true, "adr-lint.yml": true}

// jobWrites is the allowlist of job-level permission grants above read, as "file/job/permission". The top-level
// permissions of every workflow stay read-only; label-reset edits labels and nothing else.
var jobWrites = map[string]bool{"label-reset.yml/label-reset/pull-requests": true}

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
	t.Run("no pr-check job", func(t *testing.T) {
		bad := goodWorkflow[:strings.Index(goodWorkflow, "  pr-check:")]
		if v := checkWorkflow("bad", bad); len(v) == 0 {
			t.Fatal("workflow without pr-check accepted")
		}
	})
}
