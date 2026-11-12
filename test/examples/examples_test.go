// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package examples_test builds the programs in examples/ and runs them against
// an embedded nats-server, so the examples cannot rot.
package examples_test

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

const pkgBase = "github.com/bogworks/synadia-agents.go/examples/"

func startServer(t *testing.T) *server.Server {
	t.Helper()
	srv, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats-server did not become ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

// build compiles one example into dir and returns the binary's path.
func build(t *testing.T, dir, name string) string {
	t.Helper()
	bin := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-o", bin, pkgBase+name) //nolint:gosec // the test runs the go tool or a binary it built, with arguments it chose
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", name, err, out)
	}
	return bin
}

// startHost runs a host example and waits for its "ready" line. It interrupts
// the process when the test ends.
func startHost(t *testing.T, bin string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...) //nolint:gosec // the test runs the go tool or a binary it built, with arguments it chose
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGINT)
		select {
		case err := <-done:
			t.Logf("example exited: %v", err)
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			t.Logf("example killed: %v", <-done)
		}
	})
	ready := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "ready ") {
				ready <- sc.Text()
				break
			}
		}
		for sc.Scan() { // drain so the host never blocks on a full pipe
		}
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("host exited before it was ready: %v\n%s", err, stderr.String())
	case <-time.After(15 * time.Second):
		t.Fatalf("host not ready\n%s", stderr.String())
	}
}

func runExample(t *testing.T, bin string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput() //nolint:gosec // the test runs the go tool or a binary it built, with arguments it chose
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", filepath.Base(bin), args, err, out)
	}
	return string(out)
}

func TestExamples_HostEchoCallerBasic(t *testing.T) {
	srv := startServer(t)
	dir := t.TempDir()
	hostBin := build(t, dir, "host-echo")
	callerBin := build(t, dir, "caller-basic")

	startHost(t, hostBin, "-url", srv.ClientURL())
	out := runExample(t, callerBin, "-url", srv.ClientURL(), "-agent", "echo", "-prompt", "ping")
	for _, want := range []string{"discovered 1 agent instance(s)", "status: instance=", "[ack]", "echo: ping"} {
		if !strings.Contains(out, want) {
			t.Errorf("caller-basic output lacks %q:\n%s", want, out)
		}
	}
}

func TestExamples_HostQueryCallerBasic(t *testing.T) {
	srv := startServer(t)
	dir := t.TempDir()
	hostBin := build(t, dir, "host-query")
	callerBin := build(t, dir, "caller-basic")

	startHost(t, hostBin, "-url", srv.ClientURL())
	out := runExample(t, callerBin, "-url", srv.ClientURL(), "-agent", "quiz", "-prompt", "job", "-answer", "yes")
	for _, want := range []string{"working on: job", "[question] continue? (yes/no) -> yes", "continuing"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestExamples_ExtensionPoints(t *testing.T) {
	srv := startServer(t)
	bin := build(t, t.TempDir(), "extension-points")
	out := runExample(t, bin, "-url", srv.ClientURL())
	for _, want := range []string{
		`advertises endpoint "tenants"`,
		`with the hook: alice/one saw tenant "acme"`,
		`with the hook: bob/two saw tenant "acme"`,
		"without it: refused 401",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestExamples_AllBuild(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"caller-basic", "host-echo", "host-query", "signed", "extension-points"} {
		build(t, dir, name)
	}
}
