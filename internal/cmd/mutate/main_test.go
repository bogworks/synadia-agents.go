// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
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
