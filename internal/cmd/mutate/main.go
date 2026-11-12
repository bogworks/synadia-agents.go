// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Command mutate runs the mutation checks of the conformance catalog. Each
// catalog entry with a fault_id names a mutation: a fault the SDK must not
// have. conformance/mutations/<fault_id>.patch introduces that fault. For each
// fault, mutate copies the repository to a temporary directory (both modules),
// applies the patch to the copy, runs only the tests the catalog links to the
// fault, and requires at least one of them to fail. A patch that does not
// apply, one whose copy does not compile, and one every linked test survives
// are errors, and so is one whose linked tests ran into -timeout (a hang is
// not a kill); the exit status is non-zero if there is any.
//
// The tree itself is never modified.
//
// Usage, from the repository root:
//
//	go run ./internal/cmd/mutate [-only id,id] [-j N] [-timeout 2m]
//	go run ./internal/cmd/mutate -check
//
// -check only verifies that every patch applies to the current tree, without
// running any test; it exits non-zero naming each patch that does not.
//
// A full run is a release step (see RELEASING.md and conformance/README.md).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bogworks/synadia-agents.go/conformance"
)

// target is one go test invocation: the tests of one package.
type target struct {
	module string // "" for the root module, "test" for the test module
	pkg    string // package path relative to the module, "./host"
	tests  []string
}

// fault is one mutation: a fault id, the entries that name it, and the tests
// that must catch it.
type fault struct {
	id       string
	mutation string
	entries  []string
	targets  []target
}

// result is the outcome of one mutation.
type result struct {
	id       string
	killedBy []string // module/package:Test, the linked tests that failed
	hung     []string // module/package:Test, the linked tests that ran into -timeout
	err      error
	took     time.Duration
}

func main() {
	root := flag.String("root", "", "repository root (default: found from the working directory)")
	only := flag.String("only", "", "comma-separated fault ids to run (default: all)")
	jobs := flag.Int("j", max(1, runtime.NumCPU()/2), "mutations run in parallel")
	timeout := flag.Duration("timeout", 2*time.Minute, "go test -timeout for each package")
	check := flag.Bool("check", false, "only verify that every patch applies cleanly; run no tests")
	flag.Parse()
	run := run
	if *check {
		run = func(root, only string, _ int, _ time.Duration) error { return checkPatches(root, only) }
	}
	if err := run(*root, *only, *jobs, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "mutate:", err)
		os.Exit(1)
	}
}

func run(root, only string, jobs int, timeout time.Duration) error {
	if root == "" {
		var err error
		if root, err = findRoot(); err != nil {
			return err
		}
	}
	cat, err := conformance.Load(filepath.Join(root, "conformance", "catalog.json"))
	if err != nil {
		return err
	}
	tests, err := conformance.ScanTests(root)
	if err != nil {
		return err
	}
	faults, err := Faults(cat, tests)
	if err != nil {
		return err
	}
	if faults, err = selectFaults(faults, only); err != nil {
		return err
	}
	if err := baseline(root, faults, jobs, timeout); err != nil {
		return err
	}
	start := time.Now()
	results := runAll(root, faults, jobs, timeout)
	c := report(results, time.Since(start))
	if bad := c.bad(); bad > 0 {
		return fmt.Errorf("%d of %d mutations were not killed (%d timeout, %d survived or errored)", bad, len(results), c.timeout, c.failed)
	}
	return nil
}

// selectFaults keeps the faults named in only (comma-separated); all when only
// is empty.
func selectFaults(faults []fault, only string) ([]fault, error) {
	if only == "" {
		return faults, nil
	}
	want := map[string]bool{}
	for _, id := range strings.Split(only, ",") {
		want[strings.TrimSpace(id)] = true
	}
	var sel []fault
	for _, f := range faults {
		if want[f.id] {
			sel = append(sel, f)
			delete(want, f.id)
		}
	}
	for id := range want {
		return nil, fmt.Errorf("-only: no fault %q in the catalog", id)
	}
	return sel, nil
}

// checkPatches verifies that every fault's patch applies to the tree at root,
// without running any test. It returns an error naming each patch that does
// not.
func checkPatches(root, only string) error {
	if root == "" {
		var err error
		if root, err = findRoot(); err != nil {
			return err
		}
	}
	cat, err := conformance.Load(filepath.Join(root, "conformance", "catalog.json"))
	if err != nil {
		return err
	}
	tests, err := conformance.ScanTests(root)
	if err != nil {
		return err
	}
	faults, err := Faults(cat, tests)
	if err != nil {
		return err
	}
	if faults, err = selectFaults(faults, only); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "mutate-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := copyTree(root, tmp); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	var errs []error
	for _, f := range faults {
		if err := applyPatch(tmp, root, f.id, true); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.id, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%d of %d patches do not apply:\n%w", len(errs), len(faults), err)
	}
	fmt.Fprintf(os.Stderr, "mutate: all %d patches apply\n", len(faults))
	return nil
}

// applyPatch applies (or, with checkOnly, only checks) the patch of fault id
// from the repository at root to the copy in dir.
func applyPatch(dir, root, id string, checkOnly bool) error {
	patch := filepath.Join(root, "conformance", "mutations", id+".patch")
	if _, err := os.Stat(patch); err != nil {
		return fmt.Errorf("no patch: %w", err)
	}
	args := []string{"apply", "--whitespace=nowarn"}
	if checkOnly {
		args = append(args, "--check")
	}
	cmd := exec.Command("git", append(args, patch)...) //nolint:gosec // G204: the patch path is a file of the repository being tested
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("patch does not apply: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// baseline runs every linked test once on an unmutated copy. A test that
// already fails there would "kill" every mutation it is linked to, so the run
// stops instead.
func baseline(root string, faults []fault, jobs int, timeout time.Duration) error {
	var all []target
	for _, f := range faults {
		for _, t := range f.targets {
			for _, name := range t.tests {
				all = addTest(all, target{module: t.module, pkg: t.pkg, tests: []string{name}})
			}
		}
	}
	tmp, err := os.MkdirTemp("", "mutate-baseline-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := copyTree(root, tmp); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	if err := warm(tmp, all); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "baseline: running the linked tests of %d packages on the unmutated tree\n", len(all))
	var wg sync.WaitGroup
	errs := make([]error, len(all))
	sem := make(chan struct{}, jobs)
	for i, t := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			failed, hung, err := runTests(tmp, t, timeout)
			switch {
			case err != nil:
				errs[i] = err
			case len(hung) > 0:
				errs[i] = fmt.Errorf("linked tests hang without any mutation: %s", strings.Join(hung, ", "))
			case len(failed) > 0:
				errs[i] = fmt.Errorf("linked tests fail without any mutation: %s", strings.Join(failed, ", "))
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// warm fills the module and build caches before any test is timed: it
// downloads the modules of every module a target lives in, then compiles each
// target's test binary without running a test. On a runner with cold caches
// the downloads and the first compile are slow, and they must not count
// against a test's time limit.
func warm(dir string, targets []target) error {
	seen := map[string]bool{}
	for _, t := range targets {
		if seen[t.module+"\x00"+t.pkg] {
			continue
		}
		seen[t.module+"\x00"+t.pkg] = true
		mod := filepath.Join(dir, t.module)
		if !seen[mod] {
			seen[mod] = true
			if out, err := exec.Command("go", "-C", mod, "mod", "download").CombinedOutput(); err != nil { //nolint:gosec // G204: fixed arguments
				return fmt.Errorf("go mod download in %s: %w: %s", mod, err, tail(string(out)))
			}
		}
		if out, err := exec.Command("go", "-C", mod, "test", "-count=1", "-run", "^$", t.pkg).CombinedOutput(); err != nil { //nolint:gosec // G204: names come from the repository's own catalog
			return fmt.Errorf("%s: the test package does not build: %w: %s", filepath.Join(t.module, t.pkg), err, tail(string(out)))
		}
	}
	return nil
}

// failure explains a go test run that failed without a failing test. A run the
// harness itself cut off at its deadline is not a build error: it says so and
// names the deadline, so a slow runner is not mistaken for a broken build.
func failure(where string, ctxErr error, log, stderr string) error {
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return fmt.Errorf("%s: go test was stopped at the harness deadline, not a build error: %s %s", where, tail(log), tail(stderr))
	}
	return fmt.Errorf("%s: go test failed without a failing test (build error?): %s %s", where, tail(log), tail(stderr))
}

// findRoot walks up from the working directory to the directory holding
// conformance/catalog.json.
func findRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "conformance", "catalog.json")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("repository root not found; run from inside the repository or pass -root")
		}
		dir = parent
	}
}

// Faults groups the catalog's fault checks by fault id and resolves each
// linked test to the package that holds it. Entries that share a fault id
// share its mutation; their tests are united.
func Faults(c *conformance.Catalog, tests []conformance.TestFunc) ([]fault, error) {
	byID := map[string]*fault{}
	var order []string
	for _, e := range c.Entries {
		if e.FaultID == "" {
			continue
		}
		f := byID[e.FaultID]
		if f == nil {
			f = &fault{id: e.FaultID, mutation: e.Mutation}
			byID[e.FaultID] = f
			order = append(order, e.FaultID)
		} else if f.mutation != e.Mutation {
			return nil, fmt.Errorf("fault %s: entries %v and %s describe different mutations", e.FaultID, f.entries, e.ID)
		}
		f.entries = append(f.entries, e.ID)
		for _, name := range e.Tests {
			ts, err := resolve(name, tests)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", e.ID, err)
			}
			for _, t := range ts {
				f.targets = addTest(f.targets, t)
			}
		}
	}
	sort.Strings(order)
	out := make([]fault, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

func addTest(ts []target, t target) []target {
	for i := range ts {
		if ts[i].module == t.module && ts[i].pkg == t.pkg {
			for _, n := range ts[i].tests {
				if n == t.tests[0] {
					return ts
				}
			}
			ts[i].tests = append(ts[i].tests, t.tests[0])
			return ts
		}
	}
	return append(ts, t)
}

// resolve maps a catalog test name (TestX, or dir:TestX with dir relative to
// the repository root) to the packages that hold it.
func resolve(name string, tests []conformance.TestFunc) ([]target, error) {
	dir, fn := "", name
	if i := strings.LastIndex(name, ":"); i >= 0 {
		dir, fn = filepath.ToSlash(filepath.Clean(name[:i])), name[i+1:]
	}
	var out []target
	for _, t := range tests {
		if t.Name != fn || (dir != "" && t.Dir != dir) {
			continue
		}
		tg := target{pkg: "./" + t.Dir, tests: []string{fn}}
		switch {
		case t.Dir == "." || t.Dir == "":
			tg.pkg = "."
		case t.Dir == "test":
			tg.module, tg.pkg = "test", "."
		case strings.HasPrefix(t.Dir, "test/"):
			tg.module, tg.pkg = "test", "./"+strings.TrimPrefix(t.Dir, "test/")
		}
		out = append(out, tg)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("linked test %q not found", name)
	}
	return out, nil
}

func runAll(root string, faults []fault, jobs int, timeout time.Duration) []result {
	results := make([]result, len(faults))
	sem := make(chan struct{}, max(1, jobs))
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := range faults {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			start := time.Now()
			killed, hung, err := mutate(root, faults[i], timeout)
			results[i] = result{id: faults[i].id, killedBy: killed, hung: hung, err: err, took: time.Since(start)}
			mu.Lock()
			fmt.Fprintln(os.Stderr, line(results[i]))
			mu.Unlock()
		}()
	}
	wg.Wait()
	return results
}

func line(r result) string {
	if r.err != nil {
		return fmt.Sprintf("ERROR    %-34s %6.1fs  %v", r.id, r.took.Seconds(), r.err)
	}
	if len(r.hung) > 0 {
		return fmt.Sprintf("TIMEOUT  %-34s %6.1fs  %s", r.id, r.took.Seconds(), strings.Join(r.hung, ", "))
	}
	return fmt.Sprintf("killed   %-34s %6.1fs  %s", r.id, r.took.Seconds(), strings.Join(r.killedBy, ", "))
}

// counts is the summary of a run. A timeout is not a kill: a hang proves the
// mutation changed behavior, not that a test asserts the fault.
type counts struct{ killed, timeout, failed int }

// bad is the number of mutations that make the run fail.
func (c counts) bad() int { return c.timeout + c.failed }

func report(results []result, total time.Duration) counts {
	var c counts
	fmt.Println("fault_id\tstatus\tkilled by\tseconds")
	for _, r := range results {
		var status, by string
		switch {
		case r.err != nil:
			status, by = "ERROR", r.err.Error()
			c.failed++
		case len(r.hung) > 0:
			status, by = "timeout", strings.Join(r.hung, ", ")
			c.timeout++
		default:
			status, by = "killed", strings.Join(r.killedBy, ", ")
			c.killed++
		}
		fmt.Printf("%s\t%s\t%s\t%.1f\n", r.id, status, by, r.took.Seconds())
	}
	fmt.Printf("%d mutations, %d killed, %d timeout, %d survived or errored, %.0fs\n", len(results), c.killed, c.timeout, c.failed, total.Seconds())
	return c
}

// mutate copies the tree, applies the patch and runs the linked tests.
func mutate(root string, f fault, timeout time.Duration) (killed, hung []string, err error) {
	tmp, err := os.MkdirTemp("", "mutate-"+f.id+"-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(tmp)
	if err := copyTree(root, tmp); err != nil {
		return nil, nil, fmt.Errorf("copy: %w", err)
	}
	if err := applyPatch(tmp, root, f.id, false); err != nil {
		return nil, nil, err
	}
	return runTargets(tmp, f, timeout, runTests)
}

// testRunner runs one target's linked tests in dir; runTests is the real one.
type testRunner func(dir string, t target, timeout time.Duration) (failed, hung []string, err error)

// runTargets runs the fault's linked tests with run. A mutation is killed only
// if a linked test failed and none hung; a hang is reported as hung, never as
// a kill.
func runTargets(dir string, f fault, timeout time.Duration, run testRunner) (killed, hung []string, err error) {
	for _, t := range f.targets {
		k, h, err := run(dir, t, timeout)
		if err != nil {
			return nil, nil, err
		}
		killed = append(killed, k...)
		hung = append(hung, h...)
	}
	if len(hung) > 0 {
		return killed, hung, nil
	}
	if len(killed) == 0 {
		return nil, nil, errors.New("survived: every linked test passed")
	}
	return killed, nil, nil
}

// runningRE matches a line of the "running tests:" list of a go test timeout
// panic.
var runningRE = regexp.MustCompile(`(?m)^\s+(Test\w+) \(`)

// goEvent is the part of a go test -json event that the harness reads.
type goEvent struct {
	Action string
	Test   string
	Output string
}

// runTests runs one package's linked tests and returns those that failed. A
// run that hits -timeout names the hung tests separately. A build failure is an error: a mutation that does not compile proves nothing.
func runTests(dir string, t target, timeout time.Duration) (killed, hung []string, err error) {
	re := "^(" + strings.Join(t.tests, "|") + ")$"
	ctx, cancel := context.WithTimeout(context.Background(), timeout+time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-json", "-count=1", "-timeout", timeout.String(), "-run", re, t.pkg) //nolint:gosec // G204: names come from the repository's own catalog
	cmd.Dir = dir
	if t.module != "" {
		cmd.Dir = filepath.Join(dir, t.module)
	}
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	runErr := cmd.Run()
	where := filepath.Join(t.module, t.pkg)
	ran := 0
	timedOut := false
	var log, all strings.Builder
	dec := json.NewDecoder(&out)
	for {
		var ev goEvent
		if err := dec.Decode(&ev); err != nil {
			break
		}
		top := ev.Test != "" && !strings.Contains(ev.Test, "/")
		if ev.Action == "output" {
			all.WriteString(ev.Output)
			if strings.Contains(ev.Output, "test timed out after") {
				timedOut = true
			}
		}
		switch {
		case top && ev.Action == "pass":
			ran++
		case top && ev.Action == "fail":
			ran++
			killed = append(killed, where+":"+ev.Test)
		case ev.Test == "" && ev.Action == "output":
			log.WriteString(ev.Output)
		}
	}
	if timedOut {
		// When the timeout panic names the linked tests that were running,
		// they are the ones the mutation hung.
		for _, m := range runningRE.FindAllStringSubmatch(all.String(), -1) {
			if slices.Contains(t.tests, m[1]) {
				hung = append(hung, where+":"+m[1])
			}
		}
		if len(hung) == 0 {
			hung = append(hung, where+":(timed out)")
		}
	}
	if runErr != nil && len(killed) == 0 && len(hung) == 0 {
		return nil, nil, failure(where, ctx.Err(), log.String(), stderr.String())
	}
	if runErr == nil && ran == 0 {
		return nil, nil, fmt.Errorf("%s: no linked test ran (%s)", where, re)
	}
	return killed, hung, nil
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 600 {
		s = "..." + s[len(s)-600:]
	}
	return strings.ReplaceAll(s, "\n", " | ")
}

// copyTree copies the repository without .git.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		dest := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(dest, 0o750)
		case d.Type().IsRegular():
			return copyFile(p, dest, d)
		case d.Type()&fs.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, dest) //nolint:gosec // G122: copying the caller's own tree into a private temporary directory
		}
		return nil
	})
}

func copyFile(src, dest string, d fs.DirEntry) error {
	in, err := os.Open(src) //nolint:gosec // G304: walking the caller's source tree
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := d.Info()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm()) //nolint:gosec // G304: a path under the temporary copy
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
