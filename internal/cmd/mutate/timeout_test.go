// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunTargets_TimeoutIsNotAKill(t *testing.T) {
	f := fault{id: "f", targets: []target{{pkg: ".", tests: []string{"TestA"}}}}
	hang := func(string, target, time.Duration) ([]string, []string, error) {
		return nil, []string{"./:TestA"}, nil
	}
	killed, hung, err := runTargets("", f, time.Millisecond, hang)
	if err != nil || len(killed) != 0 || len(hung) != 1 {
		t.Fatalf("killed=%v hung=%v err=%v, want a hang and no kill", killed, hung, err)
	}
	// A hang next to a real failure is still a timeout, never a clean kill.
	both := func(string, target, time.Duration) ([]string, []string, error) {
		return []string{"./:TestB"}, []string{"./:TestA"}, nil
	}
	if _, h, _ := runTargets("", f, time.Millisecond, both); len(h) != 1 {
		t.Errorf("hung=%v, want the hang reported", h)
	}
	pass := func(string, target, time.Duration) ([]string, []string, error) { return nil, nil, nil }
	if _, _, err := runTargets("", f, time.Millisecond, pass); err == nil {
		t.Error("a surviving mutation returned no error")
	}

	c := report([]result{
		{id: "a", killedBy: []string{"x"}},
		{id: "b", hung: hung},
	}, time.Second)
	if c.killed != 1 || c.timeout != 1 || c.failed != 0 || c.bad() != 1 {
		t.Errorf("counts %+v, want 1 killed, 1 timeout, run failing", c)
	}
}

// TestRunTests_RealTimeout runs a real go test that sleeps past -timeout and
// requires it to be classified hung, not failed.
func TestRunTests_RealTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns go test")
	}
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module hang\n\ngo 1.21\n")
	write("hang_test.go", "package hang\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestHang(t *testing.T) { time.Sleep(time.Minute) }\n")
	failed, hung, err := runTests(dir, target{pkg: ".", tests: []string{"TestHang"}}, time.Second)
	if err != nil || len(failed) != 0 || len(hung) != 1 {
		t.Fatalf("failed=%v hung=%v err=%v, want one hung test", failed, hung, err)
	}
}

func TestFailure_DeadlineIsNotABuildError(t *testing.T) {
	err := failure("test/host", context.DeadlineExceeded, "", "go: downloading x v1")
	if err == nil || !strings.Contains(err.Error(), "not a build error") {
		t.Errorf("deadline: %v, want it named as the harness deadline", err)
	}
	err = failure("test/host", nil, "", "undefined: x")
	if err == nil || !strings.Contains(err.Error(), "build error?") || !strings.Contains(err.Error(), "undefined: x") {
		t.Errorf("real failure: %v, want a build error carrying the output", err)
	}
}
