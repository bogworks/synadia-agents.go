// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// specEnv names a copy of the pinned core-protocol.md that overrides
// defaultSpecPath. Either way the catalog is checked against the spec itself:
// hash, keyword coverage and paraphrase.
const specEnv = "SAP_SPEC_PATH"

// defaultSpecPath is where the pinned spec is expected, relative to this
// package. The upstream docs repository carries no license that permits
// redistribution, so the file is not committed: CI fetches it at the catalog's
// pinned commit (.github/workflows/go-sdk.yml, catalog job) and the catalog's
// spec.sha256 is verified by ValidateSpec.
const defaultSpecPath = "testdata/spec/core-protocol.md"

// requireSpecEnv makes a missing spec a failure and never a skip. The catalog
// job in .github/workflows/go-sdk.yml sets it after fetching the spec. It is a
// switch of its own, not CI or GITHUB_ACTIONS, because the workflow's test job
// also runs these tests under GITHUB_ACTIONS=true and does not fetch the spec.
const requireSpecEnv = "SAP_REQUIRE_SPEC"

func specRequired() bool { return os.Getenv(requireSpecEnv) == "true" }

// loadSpec reads the pinned spec: SAP_SPEC_PATH if set, else defaultSpecPath.
// A missing spec is fatal when SAP_REQUIRE_SPEC=true and a skip otherwise.
func loadSpec(t *testing.T) []byte {
	t.Helper()
	path, explicit := os.Getenv(specEnv), true
	if path == "" {
		path, explicit = defaultSpecPath, false
	}
	spec, err := os.ReadFile(path) //nolint:gosec // G703: the path is the operator's SAP_SPEC_PATH or the default spec location
	switch {
	case err == nil:
		return spec
	case !explicit && os.IsNotExist(err) && !specRequired():
		t.Skipf("spec coverage not checked: %s is missing; fetch the pinned core-protocol.md there or set %s", path, specEnv)
	}
	t.Fatalf("pinned spec %s: %v", path, err)
	return nil
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repository root %s has no go.mod: %v", root, err)
	}
	return root
}

func report(t *testing.T, errs []error) {
	t.Helper()
	const limit = 300
	for i, err := range errs {
		if i == limit {
			t.Errorf("... and %d more", len(errs)-limit)
			break
		}
		t.Error(err)
	}
}

// TestCatalog_WellFormed always enforces the catalog's structure: ids, lines,
// levels, roles, capabilities, reasons and duplicates, and checks the catalog
// against the pinned spec, so an obligation cannot be dropped unnoticed.
func TestCatalog_WellFormed(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	report(t, Validate(c))

	report(t, ValidateSpec(c, loadSpec(t)))
}

// TestCatalog_Complete checks that every obligation is covered: a status other
// than planned, tests that exist, name the obligation and contain an assertion,
// and a fault check or reviewed assertion note for every MUST and MUST NOT. It
// runs in every ordinary test run and fails on any planned entry.
func TestCatalog_Complete(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	tests, err := ScanTests(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	report(t, CheckCoverage(c, tests))
}

func TestCatalog_Summary(t *testing.T) {
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	s := c.Summarize()
	if s.Total != len(c.Entries) {
		t.Fatalf("total %d, entries %d", s.Total, len(c.Entries))
	}
	var sum int
	for _, n := range s.ByCap {
		sum += n
	}
	if sum != s.Total {
		t.Errorf("capabilities add to %d, want %d", sum, s.Total)
	}
}

// --- the checker's own tests: it must reject what it claims to reject ---

func fid(n int) string { return fmt.Sprintf("SAP-2-%02d", n) }

func goodEntry(n int) Entry {
	return Entry{
		ID: fid(n), Section: "2", SpecLine: 10 + n, Level: LevelShould, Role: RoleHost,
		Summary: fmt.Sprintf("a distinct paraphrase number %c", 'a'+n), Capability: CapBlackBox,
		Tests: []string{}, Status: StatusPlanned,
	}
}

func goodCatalog() *Catalog {
	return &Catalog{
		CatalogVersion: 1,
		Spec: Spec{
			Name: "n", Version: "0.3.0", Repo: "r", Path: "p",
			Commit: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64), Lines: 100,
		},
		Entries: []Entry{goodEntry(1), goodEntry(2)},
	}
}

func TestValidate_Table(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(c *Catalog)
		want   string // substring of an expected error, "" for none
	}{
		{"good", func(_ *Catalog) {}, ""},
		{"duplicate id", func(c *Catalog) { c.Entries[1].ID = c.Entries[0].ID }, "duplicate id"},
		{"bad id", func(c *Catalog) { c.Entries[0].ID = "X-1" }, "does not match"},
		{"id section differs", func(c *Catalog) { c.Entries[0].Section = "3" }, "differs from section"},
		{"line zero", func(c *Catalog) { c.Entries[0].SpecLine = 0 }, "outside"},
		{"line past end", func(c *Catalog) { c.Entries[0].SpecLine = 101 }, "outside"},
		{"bad level", func(c *Catalog) { c.Entries[0].Level = "must" }, "level"},
		{"bad role", func(c *Catalog) { c.Entries[0].Role = "client" }, "role"},
		{"bad capability", func(c *Catalog) { c.Entries[0].Capability = "checkable" }, "capability"},
		{"bad status", func(c *Catalog) { c.Entries[0].Status = "done" }, "status"},
		{"empty summary", func(c *Catalog) { c.Entries[0].Summary = "" }, "summary is empty"},
		{"long summary", func(c *Catalog) { c.Entries[0].Summary = strings.Repeat("x", MaxSummaryLen+1) }, "paraphrase is at most"},
		{"na without reason", func(c *Catalog) {
			c.Entries[0].Capability, c.Entries[0].Status = CapNA, StatusNA
		}, "not_applicable_reason"},
		{"reason without na", func(c *Catalog) { c.Entries[0].NotApplicableReason = "x" }, "not_applicable_reason"},
		{"na with tests", func(c *Catalog) {
			e := &c.Entries[0]
			e.Capability, e.Status, e.NotApplicableReason, e.Tests = CapNA, StatusNA, "r", []string{"TestA"}
		}, "no tests"},
		{"covered without tests", func(c *Catalog) { c.Entries[0].Status = StatusCovered }, "covered needs tests"},
		{"fault without mutation", func(c *Catalog) { c.Entries[0].FaultID = "F-X" }, "go together"},
		{"bad fault id", func(c *Catalog) { c.Entries[0].FaultID, c.Entries[0].Mutation = "x", "m" }, "fault_id"},
		{"bad test name", func(c *Catalog) { c.Entries[0].Tests = []string{"notATest"} }, "not TestXxx"},
		{"test listed twice", func(c *Catalog) { c.Entries[0].Tests = []string{"TestA", "TestA"} }, "twice"},
		{"same_as missing", func(c *Catalog) { c.Entries[0].SameAs = "nope" }, "names no entry"},
		{"same_as self", func(c *Catalog) { c.Entries[0].SameAs = c.Entries[0].ID }, "itself"},
		{"same_as chain", func(c *Catalog) {
			c.Entries = append(c.Entries, goodEntry(3))
			c.Entries[1].SameAs = c.Entries[2].ID
			c.Entries[0].SameAs = c.Entries[1].ID
		}, "chains"},
		{"duplicate obligation", func(c *Catalog) {
			d := goodEntry(2)
			d.ID = fid(3)
			d.SpecLine, d.Summary = c.Entries[0].SpecLine, "  "+strings.ToUpper(c.Entries[0].Summary)
			c.Entries = append(c.Entries, d)
		}, "duplicate obligation"},
		{"bad hash", func(c *Catalog) { c.Spec.SHA256 = "zz" }, "sha256"},
		{"no entries", func(c *Catalog) { c.Entries = nil }, "no entries"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := goodCatalog()
			tc.mutate(c)
			errs := Validate(c)
			if tc.want == "" {
				if len(errs) != 0 {
					t.Fatalf("unexpected errors: %v", errs)
				}
				return
			}
			for _, err := range errs {
				if strings.Contains(err.Error(), tc.want) {
					return
				}
			}
			t.Fatalf("want an error containing %q, got %v", tc.want, errs)
		})
	}
}

func TestParse_RejectsUnknownField(t *testing.T) {
	_, err := Parse([]byte(`{"catalog_version":1,"entries":[],"surprise":1}`))
	if err == nil {
		t.Fatal("want an error for an unknown field")
	}
}

func TestValidateSpec_Table(t *testing.T) {
	spec := "line one\nThe agent MUST do the thing now and report each step it takes.\nAgents MUST NOT stop and SHOULD keep going.\n"
	pin := func(c *Catalog) {
		c.Spec.Lines = 3
		c.Spec.SHA256 = sumHex([]byte(spec))
	}
	mk := func() *Catalog {
		c := goodCatalog()
		pin(c)
		c.Entries = []Entry{
			{ID: fid(1), Section: "2", SpecLine: 2, Level: LevelMust, Role: RoleHost, Summary: "does the work", Capability: CapBlackBox, Status: StatusPlanned},
			{ID: fid(2), Section: "2", SpecLine: 3, Level: LevelMustNot, Role: RoleHost, Summary: "never halts", Capability: CapBlackBox, Status: StatusPlanned},
			{ID: fid(3), Section: "2", SpecLine: 3, Level: LevelShould, Role: RoleHost, Summary: "carries on", Capability: CapBlackBox, Status: StatusPlanned},
		}
		return c
	}
	if errs := ValidateSpec(mk(), []byte(spec)); len(errs) != 0 {
		t.Fatalf("good catalog rejected: %v", errs)
	}
	cases := []struct {
		name   string
		mutate func(c *Catalog) []byte
		want   string
	}{
		{"hash changed", func(_ *Catalog) []byte { return []byte(spec + "\n") }, "spec hash"},
		{"missing keyword entry", func(c *Catalog) []byte {
			c.Entries = c.Entries[:2]
			return []byte(spec)
		}, "SHOULD"},
		{"added MUST without entry", func(c *Catalog) []byte {
			s := spec + "Extra MUST appear here.\n"
			c.Spec.Lines, c.Spec.SHA256 = 4, sumHex([]byte(s))
			return []byte(s)
		}, "line 4"},
		{"wrong keyword at line", func(c *Catalog) []byte {
			c.Entries[0].Level = LevelMay
			return []byte(spec)
		}, "does not contain"},
		{"verbatim summary", func(c *Catalog) []byte {
			c.Entries[0].Summary = "the agent must do the thing now and report each step it takes, as written"
			return []byte(spec)
		}, "paraphrase"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mk()
			data := tc.mutate(c)
			for _, err := range ValidateSpec(c, data) {
				if strings.Contains(err.Error(), tc.want) {
					return
				}
			}
			t.Fatalf("want an error containing %q", tc.want)
		})
	}
}

// writeTests writes a package of fake test files into a temp tree and returns
// its root.
func writeTests(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func testSrc(names map[string][]string) string {
	var b strings.Builder
	b.WriteString("package p\n\nimport \"testing\"\n\n")
	for name, ids := range names {
		b.WriteString("// " + strings.Join(ids, " ") + "\nfunc " + name + "(t *testing.T) { t.Error(\"x\") }\n\n")
	}
	return b.String()
}

func coveredCatalog() *Catalog {
	c := goodCatalog()
	c.Entries = []Entry{
		{ID: fid(1), Section: "2", SpecLine: 11, Level: LevelMust, Role: RoleHost, Summary: "first", Capability: CapBlackBox,
			Tests: []string{"TestFirst"}, FaultID: "F-FIRST", Mutation: "break first", Status: StatusCovered},
		{ID: fid(2), Section: "2", SpecLine: 12, Level: LevelShould, Role: RoleHost, Summary: "second", Capability: CapSDK,
			Tests: []string{"sub:TestSecond"}, Status: StatusCovered},
		{ID: fid(3), Section: "2", SpecLine: 13, Level: LevelMay, Role: RoleOperator, Summary: "third", Capability: CapNA,
			NotApplicableReason: "deployment", Status: StatusNA},
		{ID: fid(4), Section: "2", SpecLine: 14, Level: LevelMust, Role: RoleHost, Summary: "fourth", Capability: CapBlackBox,
			SameAs: fid(1), Status: StatusCovered},
	}
	return c
}

func coveredTree(t *testing.T) (string, []TestFunc) {
	t.Helper()
	root := writeTests(t, map[string]string{
		"a_test.go":     testSrc(map[string][]string{"TestFirst": {fid(1), fid(4)}}),
		"sub/b_test.go": testSrc(map[string][]string{"TestSecond": {fid(2)}}),
		"c.go":          "package p\n",
	})
	tests, err := ScanTests(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, tests
}

func TestCheckCoverage_Good(t *testing.T) {
	_, tests := coveredTree(t)
	if errs := CheckCoverage(coveredCatalog(), tests); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestCheckCoverage_Mutations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(c *Catalog, tests *[]TestFunc)
		want   string
	}{
		{"named test removed", func(_ *Catalog, tests *[]TestFunc) { *tests = (*tests)[:0] }, "does not exist"},
		{"test does not name the id", func(_ *Catalog, tests *[]TestFunc) {
			for i := range *tests {
				(*tests)[i].IDs = nil
			}
		}, "does not name the obligation"},
		{"MUST without tests", func(c *Catalog, _ *[]TestFunc) { c.Entries[0].Tests = nil }, "without tests"},
		{"MUST without fault check", func(c *Catalog, _ *[]TestFunc) {
			c.Entries[0].FaultID, c.Entries[0].Mutation = "", ""
		}, "neither a fault check"},
		{"assertion note stands in", func(c *Catalog, _ *[]TestFunc) {
			c.Entries[0].FaultID, c.Entries[0].Mutation, c.Entries[0].AssertionNote = "", "", "a_test.go:TestFirst asserts X"
		}, ""},
		{"assertion note names no test", func(c *Catalog, _ *[]TestFunc) {
			c.Entries[0].FaultID, c.Entries[0].Mutation, c.Entries[0].AssertionNote = "", "", "asserts X somewhere"
		}, "must name the asserting test"},
		{"assertion note names the test without its file", func(c *Catalog, _ *[]TestFunc) {
			c.Entries[0].FaultID, c.Entries[0].Mutation, c.Entries[0].AssertionNote = "", "", "TestFirst asserts X"
		}, "must name the asserting test"},
		{"linked test asserts nothing", func(_ *Catalog, tests *[]TestFunc) {
			for i := range *tests {
				(*tests)[i].Asserts = false
			}
		}, "contains no assertion"},
		{"deferred entry is accepted", func(c *Catalog, tests *[]TestFunc) {
			e := &c.Entries[1]
			e.Status, e.Tests, e.DeferredReason, e.Tracking = StatusDeferred, nil, "needs an account layout", "issue 12"
			for i := range *tests {
				if (*tests)[i].Name == "TestSecond" {
					(*tests)[i].IDs = nil
				}
			}
		}, ""},
		{"planned entry", func(c *Catalog, _ *[]TestFunc) { c.Entries[1].Status = StatusPlanned }, "still planned"},
		{"test names unknown id", func(_ *Catalog, tests *[]TestFunc) {
			(*tests)[0].IDs = append((*tests)[0].IDs, fid(77))
		}, "unknown obligation"},
		{"test names an unlisted id", func(c *Catalog, tests *[]TestFunc) {
			c.Entries[3].SameAs = fid(1)
			for i := range *tests {
				if (*tests)[i].Name == "TestSecond" {
					(*tests)[i].IDs = append((*tests)[i].IDs, fid(4))
				}
			}
		}, "does not list it"},
		{"test names an na id", func(_ *Catalog, tests *[]TestFunc) {
			(*tests)[0].IDs = append((*tests)[0].IDs, fid(3))
		}, "not-applicable"},
		{"test claims too many", func(_ *Catalog, tests *[]TestFunc) {
			for i := range *tests {
				if (*tests)[i].Name == "TestFirst" {
					for n := 10; n < 10+MaxClaims; n++ {
						(*tests)[i].IDs = append((*tests)[i].IDs, fid(n))
					}
				}
			}
		}, "needs review"},
		{"same_as target has no tests", func(c *Catalog, _ *[]TestFunc) { c.Entries[0].Tests = nil }, "no tests"},
		{"wrong directory", func(c *Catalog, _ *[]TestFunc) { c.Entries[1].Tests = []string{"other:TestSecond"} }, "does not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, tests := coveredTree(t)
			c := coveredCatalog()
			tc.mutate(c, &tests)
			errs := CheckCoverage(c, tests)
			if tc.want == "" {
				if len(errs) != 0 {
					t.Fatalf("unexpected errors: %v", errs)
				}
				return
			}
			for _, err := range errs {
				if strings.Contains(err.Error(), tc.want) {
					return
				}
			}
			t.Fatalf("want an error containing %q, got %v", tc.want, errs)
		})
	}
}

func TestValidate_Deferred(t *testing.T) {
	mk := func(mutate func(e *Entry)) []error {
		c := goodCatalog()
		e := &c.Entries[0]
		e.Status, e.DeferredReason, e.Tracking = StatusDeferred, "no account layout", "issue 12"
		mutate(e)
		return Validate(c)
	}
	if errs := mk(func(_ *Entry) {}); len(errs) != 0 {
		t.Fatalf("good deferred entry rejected: %v", errs)
	}
	for name, mutate := range map[string]func(e *Entry){
		"no reason":   func(e *Entry) { e.DeferredReason = "" },
		"no tracking": func(e *Entry) { e.Tracking = "" },
		"reason on a covered entry": func(e *Entry) {
			e.Status, e.Tests = StatusCovered, []string{"TestA"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, err := range mk(mutate) {
				if strings.Contains(err.Error(), "deferred_reason and tracking") {
					return
				}
			}
			t.Fatal("want a deferred_reason and tracking error")
		})
	}
}

// TestScanTests_Assertions: a test asserts when it, or a helper it calls in the
// same directory at any depth, calls a failing method. Naming an id, logging,
// skipping and err.Error() or fmt.Errorf calls assert nothing.
func TestScanTests_Assertions(t *testing.T) {
	root := writeTests(t, map[string]string{
		"a_test.go": `package p

import (
	"fmt"
	"fmt"
	"testing"
)

type res map[string]string

func (r res) want(t *testing.T, id string) {
	t.Helper()
	if r[id] == "" {
		t.Errorf("missing %s", id)
	}
}

func outer(t *testing.T) { inner(t) }
func inner(t *testing.T) { t.Fatal("no") }

func TestDirect(t *testing.T)    { t.Fatalf("x %d", 1) }
func TestSubtest(t *testing.T)   { t.Run("s", func(t *testing.T) { t.Error("x") }) }
func TestViaMethod(t *testing.T) { res{}.want(t, "a") }
func TestViaChain(t *testing.T)  { outer(t) }
func TestFailNow(t *testing.T)   { t.FailNow() }
func TestRequire(t *testing.T)   { require.NoError(t, nil) }

func TestIDOnly(t *testing.T)    { t.Log("covers nothing") }
func TestSkips(t *testing.T)     { t.Skip("later") }
func TestErrString(t *testing.T) { err := fmt.Errorf("x"); _ = err.Error() }
func TestCallsUnknown(t *testing.T) { elsewhere(t) }
func TestRecursive(t *testing.T) { TestRecursive(t) }
`,
	})
	tests, err := ScanTests(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"TestDirect": true, "TestSubtest": true, "TestViaMethod": true, "TestViaChain": true,
		"TestFailNow": true, "TestRequire": true,
		"TestIDOnly": false, "TestSkips": false, "TestErrString": false, "TestCallsUnknown": false, "TestRecursive": false,
	}
	for _, tf := range tests {
		w, ok := want[tf.Name]
		if !ok {
			t.Errorf("unexpected test %s", tf.Name)
			continue
		}
		if tf.Asserts != w {
			t.Errorf("%s: Asserts = %v, want %v", tf.Name, tf.Asserts, w)
		}
		delete(want, tf.Name)
	}
	for name := range want {
		t.Errorf("test %s not found", name)
	}
}

func TestScanTests_FindsOnlyRealTests(t *testing.T) {
	root := writeTests(t, map[string]string{
		"x_test.go": "package p\n\nimport \"testing\"\n\n" +
			"func TestReal(t *testing.T) {}\n" +
			"func Testlower(t *testing.T) {}\n" +
			"func TestWrongArg(t *testing.B) {}\n" +
			"func TestNoArg() {}\n" +
			"func helper(t *testing.T) {}\n" +
			"func (r R) TestMethod(t *testing.T) {}\n" +
			"type R struct{}\n",
		"nested/go.mod":      "module nested\n",
		"nested/y_test.go":   "package n\n\nimport \"testing\"\n\nfunc TestNested(t *testing.T) {}\n",
		"testdata/z_test.go": "package z\n\nimport \"testing\"\n\nfunc TestSkipped(t *testing.T) {}\n",
	})
	tests, err := ScanTests(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tf := range tests {
		got = append(got, tf.Dir+":"+tf.Name)
	}
	want := map[string]bool{".:TestReal": true, "nested:TestNested": true}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Fatalf("got %v", got)
		}
	}
}

func TestSharedRun(t *testing.T) {
	if got := sharedRun("alpha beta gamma delta", "x alpha beta gamma y"); got != 3 {
		t.Fatalf("got %d, want 3", got)
	}
	if got := sharedRun("", "a b"); got != 0 {
		t.Fatalf("got %d, want 0", got)
	}
}

func sumHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
