// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestMutations_PatchPerFault checks the cheap half of the mutation harness:
// every fault_id of the catalog has conformance/mutations/<fault_id>.patch and
// every patch names a fault_id of the catalog. Applying the patches and
// running the linked tests is the release step `go run ./internal/cmd/mutate`
// (see README.md, "Mutation testing"); it is too slow for every test run.
func TestMutations_PatchPerFault(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	faults := map[string]bool{}
	for _, e := range c.Entries {
		if e.FaultID != "" {
			faults[e.FaultID] = true
		}
	}
	dir := filepath.Join(repoRoot(t), "conformance", "mutations")
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, f := range files {
		id, ok := strings.CutSuffix(f.Name(), ".patch")
		if !ok || f.IsDir() {
			t.Errorf("mutations/%s: only <fault_id>.patch files belong here", f.Name())
			continue
		}
		have[id] = true
		if !faults[id] {
			t.Errorf("mutations/%s names no fault_id of the catalog", f.Name())
		}
	}
	var missing []string
	for id := range faults {
		if !have[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	for _, id := range missing {
		t.Errorf("fault_id %s has no mutations/%s.patch", id, id)
	}
}

// TestMutations_PatchShape checks that each patch is a unified diff of
// non-test source: a patch that edited a test, or the catalog, could make a
// mutation look killed or survive for the wrong reason.
func TestMutations_PatchShape(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "conformance", "mutations")
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, f.Name())) //nolint:gosec // G304: a file of the repository's own mutations directory
		if err != nil {
			t.Fatal(err)
		}
		hunks, targets := 0, 0
		for _, line := range strings.Split(string(data), "\n") {
			switch {
			case strings.HasPrefix(line, "+++ "):
				targets++
				path, ok := strings.CutPrefix(line, "+++ b/")
				switch {
				case !ok:
					t.Errorf("%s: target %q is not a b/ path", f.Name(), line)
				case strings.HasSuffix(path, "_test.go"), strings.HasPrefix(path, "conformance/"), strings.HasPrefix(path, "testdata/"):
					t.Errorf("%s: a mutation must change SDK source, not %s", f.Name(), path)
				}
			case strings.HasPrefix(line, "@@ "):
				hunks++
			}
		}
		if targets == 0 || hunks == 0 {
			t.Errorf("%s: not a unified diff (files %d, hunks %d)", f.Name(), targets, hunks)
		}
	}
}
