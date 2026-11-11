// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package sapconform_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bogworks/synadia-agents.go/conformance"
	"github.com/bogworks/synadia-agents.go/conformance/runner"
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// sapconformBinary builds the command once.
func sapconformBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "sapconform-bin")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "sapconform")
		out, err := exec.Command("go", "build", "-o", binPath, "github.com/bogworks/synadia-agents.go/cmd/sapconform").CombinedOutput()
		if err != nil {
			buildErr = errors.New(string(out))
		}
	})
	if buildErr != nil {
		t.Fatalf("build sapconform: %v", buildErr)
	}
	return binPath
}

func runBinary(t *testing.T, args ...string) (stdout string, exit int) {
	t.Helper()
	cmd := exec.Command(sapconformBinary(t), args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	exit = 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if exit == 2 {
		t.Logf("stderr: %s", errb.String())
	}
	return out.String(), exit
}

// TestCLI_JSONReportAndExitStatus: the command prints one JSON report that
// carries the tool version, the catalog's spec commit, the target and every
// catalog entry, and exits 0 only when no MUST or MUST NOT entry failed.
func TestCLI_JSONReportAndExitStatus(t *testing.T) {
	t.Parallel()
	cat, err := conformance.Default()
	if err != nil {
		t.Fatal(err)
	}
	flags := []string{"--json", "--discover-wait", "700ms", "--heartbeat-window", "2s", "--stream-timeout", "1500ms"}

	t.Run("conforming_exits_0", func(t *testing.T) {
		s := startServer(t)
		newFake(t, connect(t, s), fakeOpts{})
		out, exit := runBinary(t, append([]string{"--url", s.ClientURL()}, flags...)...)
		if exit != 0 {
			t.Fatalf("exit %d; report:\n%s", exit, out)
		}
		var rep runner.Report
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatal(err)
		}
		if rep.ToolVersion != runner.Version || rep.SpecCommit != cat.Spec.Commit || !strings.Contains(rep.Scope, "host obligations only") {
			t.Errorf("report header %q %q %q", rep.ToolVersion, rep.SpecCommit, rep.Scope)
		}
		if len(rep.Targets) != 1 || rep.Targets[0].Target.PromptSubject != "agents.prompt.fake.tester.main" {
			t.Fatalf("targets %+v", rep.Targets)
		}
		if got, want := len(rep.Targets[0].Entries), len(cat.Entries); got != want {
			t.Errorf("%d entries in the report, the catalog has %d", got, want)
		}
		for _, e := range rep.Targets[0].Entries {
			ce, ok := cat.ByID(e.ID)
			if !ok || ce.Capability != e.Capability {
				t.Errorf("%s: capability %q, catalog %q", e.ID, e.Capability, ce.Capability)
			}
		}
	})

	t.Run("broken_exits_1", func(t *testing.T) {
		s := startServer(t)
		newFake(t, connect(t, s), fakeOpts{NoAck: true})
		out, exit := runBinary(t, append([]string{"--url", s.ClientURL()}, flags...)...)
		if exit != 1 {
			t.Fatalf("exit %d, want 1; report:\n%s", exit, out)
		}
	})

	t.Run("no_agent_exits_2", func(t *testing.T) {
		s := startServer(t)
		_, exit := runBinary(t, "--url", s.ClientURL(), "--discover-wait", "300ms")
		if exit != 2 {
			t.Errorf("exit %d, want 2", exit)
		}
	})

	t.Run("config_file_and_human_summary", func(t *testing.T) {
		s := startServer(t)
		newFake(t, connect(t, s), fakeOpts{SessionAware: true})
		dir := t.TempDir()
		conf := filepath.Join(dir, "sapconform.json")
		body := `{"url": "` + s.ClientURL() + `", "session_aware": true, "agent": "fake",
			"timeouts": {"discover": "700ms", "heartbeat_window": "2s", "stream": "1500ms"}}`
		if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		out, exit := runBinary(t, "--config", conf, "-v")
		if exit != 0 {
			t.Fatalf("exit %d:\n%s", exit, out)
		}
		for _, want := range []string{"host obligations only", "configuration supplied: session-aware", idSessionSet, "no MUST or MUST NOT entry failed"} {
			if !strings.Contains(out, want) {
				t.Errorf("summary lacks %q:\n%s", want, out)
			}
		}
	})
}

// TestCLI_StrictVerdict: a failure of a level below MUST leaves the exit status
// at 0 and the JSON report's strict verdict false; --strict turns it into 1, and
// the report carries both verdicts either way.
func TestCLI_StrictVerdict(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	newFake(t, connect(t, s), fakeOpts{OversizeCode: 409})
	flags := []string{"--url", s.ClientURL(), "--json", "--probe-oversize", "--discover-wait", "700ms", "--heartbeat-window", "2s", "--stream-timeout", "1500ms"}
	verdict := func(out string) (def, strict bool) {
		var rep struct {
			Verdict struct {
				Default bool `json:"default_ok"`
				Strict  bool `json:"strict_ok"`
			} `json:"verdict"`
		}
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return rep.Verdict.Default, rep.Verdict.Strict
	}
	out, exit := runBinary(t, flags...)
	if exit != 0 {
		t.Fatalf("exit %d without --strict, want 0", exit)
	}
	if def, strict := verdict(out); !def || strict {
		t.Errorf("verdicts default=%v strict=%v, want true and false", def, strict)
	}
	out, exit = runBinary(t, append(flags, "--strict")...)
	if exit != 1 {
		t.Fatalf("exit %d with --strict, want 1", exit)
	}
	if def, strict := verdict(out); !def || strict {
		t.Errorf("verdicts default=%v strict=%v, want true and false", def, strict)
	}
}

// TestCLI_Limits: the limit flags parse, and --max-duration ends an unfinished
// run with exit 1 and an incomplete report.
func TestCLI_Limits(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	newFake(t, connect(t, s), fakeOpts{NoTerminator: true})
	out, exit := runBinary(t, "--url", s.ClientURL(), "--json", "--discover-wait", "700ms", "--heartbeat-window", "1s",
		"--stream-timeout", "1m", "--max-duration", "3s", "--max-stream-messages", "500", "--max-stream-bytes", "100000",
		"--max-discovery", "10", "--max-targets", "2", "--max-heartbeats", "500", "--max-info-probes", "8")
	if exit != 1 {
		t.Fatalf("exit %d, want 1", exit)
	}
	var rep runner.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rep.Incomplete, "total deadline") {
		t.Errorf("incomplete = %q", rep.Incomplete)
	}
	if _, exit := runBinary(t, "--max-targets", "-1"); exit != 2 {
		t.Errorf("a negative limit exited %d, want 2", exit)
	}
}

// TestCLI_AnswerPrefixValidation: a prefix with a wildcard is a usage error.
func TestCLI_AnswerPrefixValidation(t *testing.T) {
	t.Parallel()
	for _, p := range []string{"a.>", "*", "has space"} {
		if _, exit := runBinary(t, "--answer-query-subject-prefix", p); exit != 2 {
			t.Errorf("prefix %q exited %d, want 2", p, exit)
		}
	}
}
