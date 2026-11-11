// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// MaxClaims is the number of obligations a single test may name before the
// checker flags it for review. A test that claims many obligations tends to
// prove indexing, not testing.
const MaxClaims = 8

// TestFunc is a Go test function found in a *_test.go file.
type TestFunc struct {
	Name string   // for example TestSubject_PromptSubjectFormat
	Dir  string   // slash-separated directory relative to the scan root
	File string   // slash-separated path relative to the scan root
	IDs  []string // obligation ids named in the function's doc comment or body
	// Asserts reports whether the function can fail a test: its body, or a
	// helper it calls in the same directory (followed to any depth by name),
	// calls t.Error, t.Errorf, t.Fatal, t.Fatalf, t.Fail or t.FailNow, or a
	// require or assert function. A test that only names an id, logs or skips
	// does not assert anything.
	Asserts bool
}

// ScanTests parses every *_test.go file under root and returns the test
// functions found, which are functions named TestXxx taking one *testing.T. It
// reads across module boundaries, so tests in nested modules such as test/ are
// found. Directories named .git, vendor and testdata are skipped.
func ScanTests(root string) ([]TestFunc, error) {
	var out []TestFunc
	helpers := map[string]map[string]*funcInfo{} // dir -> function name -> info
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path) //nolint:gosec // G304: path comes from walking the caller's source tree
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("conformance: parse %s: %w", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		dir := filepath.ToSlash(filepath.Dir(rel))
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			recordFunc(helpers, dir, fn)
			if !isTestFunc(fn) {
				continue
			}
			start := fn.Pos()
			if fn.Doc != nil {
				start = fn.Doc.Pos()
			}
			body := string(src[fset.Position(start).Offset:fset.Position(fn.End()).Offset])
			out = append(out, TestFunc{
				Name: fn.Name.Name,
				Dir:  filepath.ToSlash(filepath.Dir(rel)),
				File: rel,
				IDs:  uniqueIDs(body),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Asserts = reaches(helpers[out[i].Dir], out[i].Name)
	}
	return out, nil
}

// funcInfo is what the assertion check needs of a function: whether it calls
// a failing method itself, and the names of the functions and methods it calls.
type funcInfo struct {
	direct bool
	calls  map[string]bool
}

func recordFunc(all map[string]map[string]*funcInfo, dir string, fn *ast.FuncDecl) {
	if all[dir] == nil {
		all[dir] = map[string]*funcInfo{}
	}
	info := all[dir][fn.Name.Name]
	if info == nil {
		info = &funcInfo{calls: map[string]bool{}}
		all[dir][fn.Name.Name] = info
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.Ident:
			info.calls[f.Name] = true
		case *ast.SelectorExpr:
			info.calls[f.Sel.Name] = true
			if isFailCall(f, len(call.Args)) {
				info.direct = true
			}
		}
		return true
	})
}

// isFailCall reports whether sel(args) looks like a testing failure call or a
// require or assert call. err.Error() and fmt.Errorf are not.
func isFailCall(sel *ast.SelectorExpr, nargs int) bool {
	recv, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	switch recv.Name {
	case "require", "assert":
		return true
	case "fmt", "errors", "log", "slog", "os":
		return false
	}
	switch sel.Sel.Name {
	case "Error", "Errorf", "Fatal", "Fatalf":
		return nargs >= 1
	case "Fail", "FailNow":
		return nargs == 0
	}
	return false
}

// reaches reports whether the named function, or one it calls, fails tests.
func reaches(funcs map[string]*funcInfo, name string) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(n string) bool {
		if seen[n] {
			return false
		}
		seen[n] = true
		fi := funcs[n]
		if fi == nil {
			return false
		}
		if fi.direct {
			return true
		}
		for c := range fi.calls {
			if walk(c) {
				return true
			}
		}
		return false
	}
	return walk(name)
}

// noteNamesTest reports whether the entry's assertion note names one of its
// linked tests as file_test.go:TestName.
func noteNamesTest(e Entry, tests []TestFunc) bool {
	for _, name := range e.Tests {
		for _, t := range resolve(name, tests) {
			if strings.Contains(e.AssertionNote, path.Base(t.File)+":"+t.Name) {
				return true
			}
		}
	}
	return false
}

func isTestFunc(fn *ast.FuncDecl) bool {
	if fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
		return false
	}
	if rest := strings.TrimPrefix(fn.Name.Name, "Test"); rest != "" && rest[0] >= 'a' && rest[0] <= 'z' {
		return false
	}
	ps := fn.Type.Params
	if ps == nil || len(ps.List) != 1 || len(ps.List[0].Names) > 1 {
		return false
	}
	star, ok := ps.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "T" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "testing"
}

func uniqueIDs(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range idScanRE.FindAllString(s, -1) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// resolve finds the test functions a catalog test name refers to. The name is
// TestXxx, matching any package, or dir:TestXxx, matching one directory.
func resolve(name string, tests []TestFunc) []TestFunc {
	dir, fn := "", name
	if i := strings.LastIndex(name, ":"); i >= 0 {
		dir, fn = name[:i], name[i+1:]
		dir = strings.TrimSuffix(filepath.ToSlash(filepath.Clean(dir)), "/")
	}
	var out []TestFunc
	for _, t := range tests {
		if t.Name == fn && (dir == "" || t.Dir == dir) {
			out = append(out, t)
		}
	}
	return out
}

// CheckCoverage verifies the catalog's links to tests. It returns every
// problem found:
//
//   - a status other than planned or not-applicable that lacks tests;
//   - a MUST or MUST NOT with neither a reviewed not-applicable reason nor
//     tests plus a fault check or an assertion note;
//   - a linked test that does not exist, or does not name the obligation id;
//   - a test that names an id the catalog lacks, or one that lists the test
//     as covering it (the link is checked both ways);
//   - a test that names more than MaxClaims obligations.
//
// Entries with status planned are reported as unfinished. Entries with status
// deferred are accepted: they carry a reason and a tracking note instead of
// tests. A linked test must contain an assertion, and the assertion note of an
// entry without a fault check must name a linked test and its file.
func CheckCoverage(c *Catalog, tests []TestFunc) []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	byID := map[string]Entry{}
	for _, e := range c.Entries {
		byID[e.ID] = e
	}
	// The effective tests of an entry are its own, or its target's.
	canon := func(e Entry) Entry {
		if e.SameAs != "" {
			if t, ok := byID[e.SameAs]; ok {
				return t
			}
		}
		return e
	}

	listed := map[string]map[string]bool{} // test key -> ids that list it
	key := func(t TestFunc) string { return t.Dir + ":" + t.Name }
	for _, e := range c.Entries {
		if e.Status == StatusPlanned {
			add("%s: still planned: cover it, mark it not-applicable with a reason, or defer it with a reason and a tracking note", e.ID)
			continue
		}
		if e.Capability == CapNA || e.Status == StatusDeferred {
			continue
		}
		own := canon(e)
		if len(own.Tests) == 0 {
			add("%s: no tests (or its same_as target %s has none)", e.ID, e.SameAs)
		}
		if e.IsStrict() && e.SameAs == "" {
			if len(e.Tests) == 0 {
				add("%s: %s without tests or a reviewed not-applicable reason", e.ID, e.Level)
			}
			if (e.FaultID == "" || e.Mutation == "") && e.AssertionNote == "" {
				add("%s: %s has neither a fault check nor a reviewed assertion note", e.ID, e.Level)
			}
			if e.FaultID == "" && e.AssertionNote != "" && !noteNamesTest(e, tests) {
				add("%s: assertion_note must name the asserting test and its file, as file_test.go:TestName, for one of its linked tests", e.ID)
			}
		}
		// A same_as entry is covered by its target's tests; those tests may name
		// it but need not.
		for _, name := range own.Tests {
			found := resolve(name, tests)
			if e.SameAs != "" {
				for _, t := range found {
					if listed[key(t)] == nil {
						listed[key(t)] = map[string]bool{}
					}
					listed[key(t)][e.ID] = true
				}
				continue
			}
			if len(found) == 0 {
				add("%s: linked test %s does not exist", e.ID, name)
				continue
			}
			ok := false
			for _, t := range found {
				if m := listed[key(t)]; m == nil {
					listed[key(t)] = map[string]bool{}
				}
				listed[key(t)][e.ID] = true
				for _, id := range t.IDs {
					if id == e.ID {
						ok = true
					}
				}
			}
			if !ok {
				add("%s: linked test %s does not name the obligation id", e.ID, name)
			}
			asserts := false
			for _, t := range found {
				asserts = asserts || t.Asserts
			}
			if !asserts {
				add("%s: linked test %s contains no assertion (t.Error, t.Fatal, require or a helper that calls them)", e.ID, name)
			}
		}
	}
	for _, t := range tests {
		if len(t.IDs) > MaxClaims {
			add("test %s (%s) names %d obligations; more than %d needs review", t.Name, t.File, len(t.IDs), MaxClaims)
		}
		for _, id := range t.IDs {
			e, ok := byID[id]
			if !ok {
				add("test %s (%s) names unknown obligation %s", t.Name, t.File, id)
				continue
			}
			if e.Capability == CapNA {
				add("test %s (%s) names not-applicable obligation %s", t.Name, t.File, id)
				continue
			}
			if !listed[key(t)][id] {
				add("test %s (%s) names %s, but the catalog does not list it for that entry", t.Name, t.File, id)
			}
		}
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	return errs
}
