// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package docs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// parityPath is the parity matrix (acceptance test AT-011).
const parityPath = root + "/docs/parity.md"

// Column positions of a feature row of the parity matrix.
const (
	colID         = 0
	colCounter    = 7 // proposed Go counterpart
	colGoTest     = 8
	parityColumns = 10
)

var (
	featureRow   = regexp.MustCompile(`^\| \d+\.\d+a? \|`)
	deviationRow = regexp.MustCompile(`^\| D-\d+ \|`)
	deviationID  = regexp.MustCompile(`\bD-\d+\b`)
	testName     = regexp.MustCompile(`^(?:([A-Za-z][\w/]*)\.)?((?:Test|Fuzz|Example|Benchmark)\w*)$`)
	backticked   = regexp.MustCompile("`([^`]*)`")
	qualified    = regexp.MustCompile(`\b(agents|caller|host|identity|tools|connect|agentstest)\.([A-Z]\w*)((?:\.[A-Z]\w*)?)`)
)

// splitCells splits one markdown table row on its unescaped pipes.
func splitCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line) && line[i+1] == '|':
			cur.WriteString(`\|`)
			i++
		case line[i] == '|':
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(line[i])
		}
	}
	return append(cells, strings.TrimSpace(cur.String()))
}

type parityDoc struct {
	rows       [][]string          // feature rows, split into cells
	deviations map[string]struct{} // D-ids of the deviations table
}

func readParity(t *testing.T) parityDoc {
	t.Helper()
	b, err := os.ReadFile(parityPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := parityDoc{deviations: map[string]struct{}{}}
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case featureRow.MatchString(line):
			cells := splitCells(line)
			if len(cells) != parityColumns {
				t.Errorf("row %q has %d cells, want %d", cells[colID], len(cells), parityColumns)
				continue
			}
			doc.rows = append(doc.rows, cells)
		case deviationRow.MatchString(line):
			doc.deviations[splitCells(line)[0]] = struct{}{}
		}
	}
	if len(doc.rows) < 300 || len(doc.deviations) == 0 {
		t.Fatalf("parsed %d feature rows and %d deviations; the table format changed", len(doc.rows), len(doc.deviations))
	}
	return doc
}

// testRef is a named test and the directory that must hold it.
type testRef struct{ dir, name string }

// parseTests reads the Go test cell: tokens separated by commas or
// semicolons, each `pkg.TestName` or a bare `TestName` that continues the
// previous package. Package "agents" is the repository root; every other
// package is its directory (`caller`, `test/caller`, ...). Prose is ignored.
func parseTests(cell string) []testRef {
	var refs []testRef
	pkg := ""
	for _, tok := range strings.FieldsFunc(cell, func(r rune) bool { return r == ',' || r == ';' }) {
		tok = strings.Trim(strings.TrimSpace(tok), "`")
		m := testName.FindStringSubmatch(tok)
		if m == nil {
			continue
		}
		if m[1] != "" {
			pkg = m[1]
		}
		dir := pkg
		if dir == "agents" {
			dir = "."
		}
		refs = append(refs, testRef{dir: dir, name: m[2]})
	}
	return refs
}

// testFuncs returns the names of the top-level funcs of dir's *_test.go files.
func testFuncs(t *testing.T, dir string, cache map[string]map[string]bool) map[string]bool {
	t.Helper()
	if got, ok := cache[dir]; ok {
		return got
	}
	out := map[string]bool{}
	cache[dir] = out
	files, _ := filepath.Glob(filepath.Join(root, dir, "*_test.go"))
	for _, p := range files {
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
				out[fd.Name.Name] = true
			}
		}
	}
	return out
}

func TestParity_EveryRowHasATestOrADeviation(t *testing.T) {
	doc := readParity(t)
	cache := map[string]map[string]bool{}
	for _, row := range doc.rows {
		id := row[colID]
		refs := parseTests(row[colGoTest])
		if len(refs) == 0 && !deviationID.MatchString(strings.Join(row, " ")) {
			t.Errorf("row %s: the Go test cell names no test (%q) and the row cites no D-id", id, row[colGoTest])
		}
		for _, r := range refs {
			if r.dir == "" {
				t.Errorf("row %s: test %s has no package", id, r.name)
				continue
			}
			if !testFuncs(t, r.dir, cache)[r.name] {
				t.Errorf("row %s: %s does not exist in %s", id, r.name, r.dir)
			}
		}
	}
}

func TestParity_DeviationsMatchRows(t *testing.T) {
	doc := readParity(t)
	used := map[string]bool{}
	for _, row := range doc.rows {
		for _, d := range deviationID.FindAllString(strings.Join(row, " "), -1) {
			used[d] = true
			if _, ok := doc.deviations[d]; !ok {
				t.Errorf("row %s cites %s, which the deviations table does not define", row[colID], d)
			}
		}
	}
	var unused []string
	for d := range doc.deviations {
		if !used[d] {
			unused = append(unused, d)
		}
	}
	sort.Strings(unused)
	for _, d := range unused {
		t.Errorf("deviation %s is cited by no row", d)
	}
}

// apiSurface maps package name to the set of exported names it provides:
// "Name" for a top-level identifier, "Type.Member" for an exported method or
// struct field or interface method of an exported type. It covers every
// non-main package of the root module outside internal/ and the test module.
func apiSurface(t *testing.T) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	dirs, err := filepath.Glob(filepath.Join(root, "*"))
	if err != nil {
		t.Fatal(err)
	}
	dirs = append(dirs, root)
	for _, dir := range dirs {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		switch filepath.Base(dir) {
		case "internal", "test", "testdata", "examples", "cmd", "docs":
			continue
		}
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, p := range files {
			if strings.HasSuffix(p, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			if f.Name.Name == "main" {
				continue
			}
			set := out[f.Name.Name]
			if set == nil {
				set = map[string]bool{}
				out[f.Name.Name] = set
			}
			collect(f, set)
		}
	}
	return out
}

func collect(f *ast.File, set map[string]bool) {
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if !d.Name.IsExported() {
				continue
			}
			if d.Recv == nil {
				set[d.Name.Name] = true
				continue
			}
			if r := recvName(d.Recv.List[0].Type); ast.IsExported(r) {
				set[r+"."+d.Name.Name] = true
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					if !s.Name.IsExported() {
						continue
					}
					set[s.Name.Name] = true
					switch ty := s.Type.(type) {
					case *ast.StructType:
						for _, fl := range ty.Fields.List {
							for _, n := range fl.Names {
								if n.IsExported() {
									set[s.Name.Name+"."+n.Name] = true
								}
							}
						}
					case *ast.InterfaceType:
						for _, m := range ty.Methods.List {
							for _, n := range m.Names {
								if n.IsExported() {
									set[s.Name.Name+"."+n.Name] = true
								}
							}
						}
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n.IsExported() {
							set[n.Name] = true
						}
					}
				}
			}
		}
	}
}

func recvName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.StarExpr:
		return recvName(e.X)
	case *ast.IndexExpr:
		return recvName(e.X)
	case *ast.IndexListExpr:
		return recvName(e.X)
	case *ast.Ident:
		return e.Name
	}
	return ""
}

func TestParity_CounterpartsAreExported(t *testing.T) {
	doc := readParity(t)
	api := apiSurface(t)
	for _, pkg := range []string{"agents", "caller", "host", "identity", "tools", "connect", "agentstest"} {
		if len(api[pkg]) == 0 {
			t.Fatalf("package %s not found under %s: the check would pass vacuously", pkg, root)
		}
	}
	for _, row := range doc.rows {
		for _, span := range backticked.FindAllStringSubmatch(row[colCounter], -1) {
			for _, m := range qualified.FindAllStringSubmatch(span[1], -1) {
				pkg, name, member := m[1], m[2], m[3]
				if !api[pkg][name] {
					t.Errorf("row %s: %s.%s is not exported by package %s", row[colID], pkg, name, pkg)
					continue
				}
				// A member is checked only when the type declares it itself;
				// a promoted or embedded member is not an error.
				if member != "" && !api[pkg][name+member] && declaresMembers(api[pkg], name) {
					t.Errorf("row %s: %s.%s%s: %s has no such exported member", row[colID], pkg, name, member, name)
				}
			}
		}
	}
}

// declaresMembers reports whether any member of the type is known, so that a
// type with no known members (a function or a basic type) is not flagged.
func declaresMembers(set map[string]bool, typ string) bool {
	for k := range set {
		if strings.HasPrefix(k, typ+".") {
			return true
		}
	}
	return false
}
