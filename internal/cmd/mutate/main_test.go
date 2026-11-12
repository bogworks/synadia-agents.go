// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogworks/synadia-agents.go/conformance"
)

func TestResolve(t *testing.T) {
	tests := []conformance.TestFunc{
		{Name: "TestRoot", Dir: "."},
		{Name: "TestHost", Dir: "host"},
		{Name: "TestHost", Dir: "test/host"},
		{Name: "TestTop", Dir: "test"},
	}
	cases := []struct {
		name string
		want []target
	}{
		{"TestRoot", []target{{pkg: ".", tests: []string{"TestRoot"}}}},
		{"host:TestHost", []target{{pkg: "./host", tests: []string{"TestHost"}}}},
		{"test/host:TestHost", []target{{module: "test", pkg: "./host", tests: []string{"TestHost"}}}},
		{"TestHost", []target{
			{pkg: "./host", tests: []string{"TestHost"}},
			{module: "test", pkg: "./host", tests: []string{"TestHost"}},
		}},
		{"test:TestTop", []target{{module: "test", pkg: ".", tests: []string{"TestTop"}}}},
	}
	for _, tc := range cases {
		got, err := resolve(tc.name, tests)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i].module != tc.want[i].module || got[i].pkg != tc.want[i].pkg || got[i].tests[0] != tc.want[i].tests[0] {
				t.Errorf("%s: %+v, want %+v", tc.name, got[i], tc.want[i])
			}
		}
	}
	if _, err := resolve("TestMissing", tests); err == nil {
		t.Error("an unknown test resolved")
	}
}

// TestFaults_EveryFaultHasTests checks against the real catalog that every
// fault resolves to at least one existing linked test, so the release run
// cannot silently run nothing.
func TestFaults_EveryFaultHasTests(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := conformance.Load(filepath.Join(root, "conformance", "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	tests, err := conformance.ScanTests(root)
	if err != nil {
		t.Fatal(err)
	}
	faults, err := Faults(cat, tests)
	if err != nil {
		t.Fatal(err)
	}
	if len(faults) == 0 {
		t.Fatal("no faults in the catalog")
	}
	for _, f := range faults {
		if len(f.targets) == 0 {
			t.Errorf("%s: no linked test", f.id)
		}
	}
}

// TestCheckPatches checks that -check names a patch that does not apply, and
// passes when every patch applies.
func TestCheckPatches(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("conformance/catalog.json", `{"entries":[
{"id":"E1","fault_id":"F-ONE","mutation":"m","tests":["TestX"]},
{"id":"E2","fault_id":"F-TWO","mutation":"m","tests":["TestX"]}]}`)
	write("x_test.go", "package x\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n")
	write("a.txt", "one\ntwo\nthree\n")
	good := "--- a/a.txt\n+++ b/a.txt\n@@ -1,3 +1,3 @@\n one\n-two\n+2\n three\n"
	bad := "--- a/a.txt\n+++ b/a.txt\n@@ -1,3 +1,3 @@\n one\n-zwei\n+2\n three\n"
	write("conformance/mutations/F-ONE.patch", good)
	write("conformance/mutations/F-TWO.patch", good)
	if err := checkPatches(root, ""); err != nil {
		t.Fatalf("all patches apply: %v", err)
	}
	write("conformance/mutations/F-TWO.patch", bad)
	err := checkPatches(root, "")
	if err == nil || !strings.Contains(err.Error(), "F-TWO") || strings.Contains(err.Error(), "F-ONE") {
		t.Fatalf("want an error naming only F-TWO, got %v", err)
	}
	if err := checkPatches(root, "F-ONE"); err != nil {
		t.Fatalf("-only F-ONE: %v", err)
	}
}
