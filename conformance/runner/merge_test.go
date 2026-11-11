// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

var allResults = []Result{Pass, Policy, NotApplicable, NotRun, ConfigMissing, Fail}

// TestWorseOrder checks the merge for every ordered pair of results: the worse
// one wins, a tie keeps the first, and the order is fail > configuration-missing
// > not-run > not-applicable > policy > pass.
func TestWorseOrder(t *testing.T) {
	for i, a := range allResults {
		for j, b := range allResults {
			got := worse(Outcome{a, "first"}, Outcome{b, "second"})
			want := a
			if j > i {
				want = b
			}
			if got.Result != want {
				t.Errorf("worse(%s, %s) = %s, want %s", a, b, got.Result, want)
			}
		}
	}
	// A result the table does not know ranks worst.
	if got := worse(Outcome{Fail, "x"}, Outcome{"mystery", "y"}); got.Result != "mystery" {
		t.Errorf("an unknown result must outrank fail, got %s", got.Result)
	}
}

func TestWorseDetails(t *testing.T) {
	got := worse(Outcome{Fail, "saw a signed status reply"}, Outcome{ConfigMissing, "supply --sender-seed"})
	if got.Result != Fail || !strings.HasPrefix(got.Detail, "saw a signed status reply") || !strings.Contains(got.Detail, "supply --sender-seed") {
		t.Errorf("fail must keep its evidence and note the gap: %+v", got)
	}
	// Order of arrival does not change what wins or what the detail leads with.
	got = worse(Outcome{ConfigMissing, "supply --sender-seed"}, Outcome{Fail, "saw a signed status reply"})
	if got.Result != Fail || !strings.HasPrefix(got.Detail, "saw a signed status reply") {
		t.Errorf("later fail must win and lead: %+v", got)
	}
	// Pass details are not carried behind a worse result.
	got = worse(Outcome{Fail, "bad"}, Outcome{Pass, "fine"})
	if got.Detail != "bad" {
		t.Errorf("a pass detail was appended to a fail: %q", got.Detail)
	}
	// A tie keeps the first recorded one.
	got = worse(Outcome{ConfigMissing, "one"}, Outcome{ConfigMissing, "two"})
	if got.Result != ConfigMissing || !strings.HasPrefix(got.Detail, "one") {
		t.Errorf("tie: %+v", got)
	}
}

// TestRecordedFailSurvives: once a Fail is recorded no later result, in any
// order of arrival or repetition, replaces it.
func TestRecordedFailSurvives(t *testing.T) {
	for _, later := range allResults {
		var r recorder
		r.add(Outcome{Fail, "observed"})
		r.add(Outcome{later, "later"})
		r.add(Outcome{Pass, "again"})
		r.add(Outcome{NotApplicable, "again"})
		r.add(Outcome{ConfigMissing, "again"})
		out, ok := r.result()
		if !ok || out.Result != Fail || !strings.HasPrefix(out.Detail, "observed") {
			t.Errorf("after fail then %s: %+v", later, out)
		}
	}
	// And a configuration-missing is not replaced by a pass or not-applicable.
	var r recorder
	r.add(Outcome{ConfigMissing, "gap"})
	r.add(Outcome{Pass, "p"})
	r.add(Outcome{NotApplicable, "n"})
	if out, _ := r.result(); out.Result != ConfigMissing {
		t.Errorf("configuration-missing was improved to %s", out.Result)
	}
}

// resultWriters are the functions that may write a result or detail directly:
// worse combines two outcomes, runTarget copies the merged outcome into the
// entry. withInstanceCount may write the detail of a pass, and only that.
var (
	resultWriters = map[string]bool{"worse": true, "runTarget": true}
	detailWriters = map[string]bool{"withInstanceCount": true}
	// flowFuncs are the functions a check's returned outcome flows through on
	// its way into the recorder. In them the returned outcome is merged and never
	// reassigned.
	flowFuncs = map[string]bool{"runTarget": true, "perInstance": true, "decide": true, "safeRun": true}
	// mergeOnly are the flow functions that may never construct a result that
	// is better than fail: they merge outcomes, they do not make a downgrade.
	// decide is not here: it builds the outcomes of entries that have no check to
	// run (operator, caller, sdk, not-applicable, no check, missing gate).
	mergeOnly   = map[string]bool{"runTarget": true, "perInstance": true}
	betterCtors = map[string]bool{"pass": true, "na": true, "policy": true, "notRun": true}
	betterRes   = map[string]bool{"Pass": true, "Policy": true, "NotApplicable": true, "NotRun": true, "ConfigMissing": true}
)

// scanResultWrites reports every place in one parsed source file where a
// result is written outside the merge, or a check's returned result is
// rewritten:
//
//   - an assignment to a .Result or .Detail field outside the writers above, and
//     an EntryResult composite literal outside runTarget;
//   - in a flow function, a reassignment of a variable that was defined from a
//     call (the returned outcome must go straight into the recorder);
//   - in runTarget and perInstance, a call to pass, na, policy or notRun, or a
//     composite Outcome literal of a result other than fail: those functions
//     may only merge, never construct a downgrade.
func scanResultWrites(fset *token.FileSet, f *ast.File) []string {
	var out []string
	report := func(n ast.Node, format string, a ...any) {
		out = append(out, fset.Position(n.Pos()).String()+": "+fmt.Sprintf(format, a...))
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		name := fn.Name.Name
		fromCall := map[string]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for _, l := range x.Lhs {
					switch lhs := l.(type) {
					case *ast.SelectorExpr:
						switch {
						case lhs.Sel.Name != "Result" && lhs.Sel.Name != "Detail", resultWriters[name]:
						case lhs.Sel.Name == "Detail" && detailWriters[name]:
						default:
							report(x, "%s writes %s directly; record it through worse (t.observe, the recorder or perInstance)", name, lhs.Sel.Name)
						}
					case *ast.Ident:
						if flowFuncs[name] && x.Tok != token.DEFINE && lhs.Name != "_" && fromCall[lhs.Name] {
							report(x, "%s reassigns %s, which holds a returned outcome; merge it into the recorder instead", name, lhs.Name)
						}
					}
				}
				if flowFuncs[name] && x.Tok == token.DEFINE {
					for _, r := range x.Rhs {
						if _, isCall := r.(*ast.CallExpr); isCall {
							for _, l := range x.Lhs {
								if id, ok := l.(*ast.Ident); ok && id.Name != "_" {
									fromCall[id.Name] = true
								}
							}
						}
					}
				}
			case *ast.CompositeLit:
				if id, ok := x.Type.(*ast.Ident); ok {
					if id.Name == "EntryResult" && name != "runTarget" {
						report(x, "%s builds an EntryResult; only runTarget does, from the recorder", name)
					}
					if id.Name == "Outcome" && mergeOnly[name] && len(x.Elts) > 0 {
						if r, ok := x.Elts[0].(*ast.Ident); ok && betterRes[r.Name] {
							report(x, "%s builds a %s outcome; it may only merge outcomes, never construct a downgrade", name, r.Name)
						}
					}
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && mergeOnly[name] && betterCtors[id.Name] {
					report(x, "%s calls %s; it may only merge outcomes, never construct a downgrade", name, id.Name)
				}
			}
			return true
		})
	}
	return out
}

// TestResultsAreRecordedThroughWorse fails when code outside the merge writes a
// result or detail directly, or when a check's returned result is rewritten on
// its way into the recorder (see scanResultWrites). The rule is: a result only
// gets worse, with no exception.
func TestResultsAreRecordedThroughWorse(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	scanned := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && flowFuncs[fn.Name.Name] {
				scanned[fn.Name.Name] = true
			}
		}
		for _, v := range scanResultWrites(fset, f) {
			t.Error(v)
		}
	}
	// The scan is only worth anything if it still sees the functions it guards.
	for n := range flowFuncs {
		if !scanned[n] {
			t.Errorf("the scan did not find %s; rename it in flowFuncs", n)
		}
	}
}

// TestResultScanCatchesRewrites feeds the scan small sources that rewrite a
// check's result and requires each to be caught, and a clean one to pass.
func TestResultScanCatchesRewrites(t *testing.T) {
	cases := []struct {
		name, body string
		want       int
	}{
		{"clean", `func runTarget() { o := chk(); rec.add(o); m, _ := rec.result(); _ = m }`, 0},
		{"field write", `func check() { o := chk(); o.Result = ConfigMissing }`, 1},
		{"detail write", `func check() { var o Outcome; o.Detail = "x" }`, 1},
		{"reassign", `func runTarget() { o := chk(); if x { o = other() }; rec.add(o) }`, 1},
		{"reassign tuple", `func runTarget() { o, s := chk(); o, _ = rec.result(); _ = s }`, 1},
		{"convert in perInstance", `func perInstance() { o := f(); if bad { o = worst }; rest.add(o) }`, 1},
		{"construct config-missing", `func runTarget() { rec.add(Outcome{ConfigMissing, "x"}) }`, 1},
		{"construct pass", `func perInstance() { rec.add(pass("x")) }`, 1},
		{"construct na", `func runTarget() { rec.add(na("x")) }`, 1},
		{"fail is fine", `func runTarget() { rec.add(Outcome{Fail, "x"}) }`, 0},
		{"entry literal", `func other() { _ = EntryResult{} }`, 1},
		{"detail writer may write detail", `func withInstanceCount() { var o Outcome; o.Detail = "x" }`, 0},
		{"detail writer may not write result", `func withInstanceCount() { var o Outcome; o.Result = Pass }`, 1},
		{"decide may build gates", `func decide() { rec.add(Outcome{ConfigMissing, "x"}) }`, 0},
		{"decide may not rewrite", `func decide() { o := safeRun(); o = Outcome{}; _ = o }`, 1},
	}
	for _, c := range cases {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "snippet.go", "package runner\n"+c.body, 0)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := scanResultWrites(fset, f); len(got) != c.want {
			t.Errorf("%s: %d violations %v, want %d", c.name, len(got), got, c.want)
		}
	}
}

// TestUnknownResultFailsVerdicts: a result the runner does not define is never
// read as conforming, at any level, in either verdict.
func TestUnknownResultFailsVerdicts(t *testing.T) {
	for _, level := range []string{"MUST", "SHOULD", "MAY"} {
		e := EntryResult{ID: "X", Level: level, Result: "mystery"}
		if !e.IsStrictFailure() {
			t.Errorf("%s: an unknown result passed the strict verdict", level)
		}
	}
	if e := (EntryResult{ID: "X", Level: "MUST", Result: "mystery"}); !e.IsFailure() {
		t.Error("an unknown result of a MUST passed the default verdict")
	}
	rep := &Report{Targets: []TargetReport{{Entries: []EntryResult{{ID: "X", Level: "MUST", Result: "mystery"}}}}}
	if rep.OK() || rep.StrictOK() {
		t.Error("a report holding an unknown result passed a verdict")
	}
}

// TestIncompleteNeverPasses: an incomplete run, or one with no target, passes
// neither verdict even when every entry passed.
func TestIncompleteNeverPasses(t *testing.T) {
	clean := TargetReport{Entries: []EntryResult{{ID: "X", Level: "MUST", Result: Pass}}}
	if r := (&Report{Targets: []TargetReport{clean}}); !r.OK() || !r.StrictOK() {
		t.Fatal("a complete clean run must pass")
	}
	if r := (&Report{Targets: []TargetReport{clean}, Incomplete: "cap"}); r.OK() || r.StrictOK() {
		t.Error("an incomplete run passed a verdict")
	}
	if r := (&Report{}); r.OK() || r.StrictOK() {
		t.Error("a run with no target passed a verdict")
	}
}
