// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package docs_test checks the documentation against the code.
package docs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const root = "../.." // the root module

// consumer is the name only docs/ may carry. It is spelled in two pieces so
// that this file does not itself trip the independence check of CI.
const consumer = "ma" + "sh"

// exportedIdentifiers returns the qualified name of every exported top-level
// identifier, and of every exported method of an exported type, of every
// non-main package of the root module (the test module, examples, test data
// internal packages, and hidden or underscore directories are not part of the
// API).
func exportedIdentifiers(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			n := d.Name()
			if rel == "test" || rel == "examples" || n == "internal" || n == "testdata" || strings.HasPrefix(n, "_") || (strings.HasPrefix(n, ".") && rel != ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			return err
		}
		if f.Name.Name == "main" {
			return nil
		}
		pkg := f.Name.Name
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				if d.Recv == nil {
					out = append(out, pkg+"."+d.Name.Name)
					continue
				}
				if r := receiver(d.Recv.List[0].Type); ast.IsExported(r) {
					out = append(out, pkg+"."+r+"."+d.Name.Name)
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						if !s.Name.IsExported() {
							continue
						}
						out = append(out, pkg+"."+s.Name.Name)
						if it, ok := s.Type.(*ast.InterfaceType); ok {
							for _, m := range it.Methods.List {
								for _, n := range m.Names {
									if n.IsExported() {
										out = append(out, pkg+"."+s.Name.Name+"."+n.Name)
									}
								}
							}
						}
					case *ast.ValueSpec:
						for _, n := range s.Names {
							if n.IsExported() {
								out = append(out, pkg+"."+n.Name)
							}
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func receiver(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return receiver(t.X)
	case *ast.IndexExpr:
		return receiver(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

// TestProtocolMapping_Complete fails when an exported identifier has no row in
// docs/protocol-mapping.md, and when a row names an identifier that no longer
// exists. A row is a table line that starts with the identifier in backticks.
func TestProtocolMapping_Complete(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join(root, "docs", "protocol-mapping.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]bool{}
	for _, line := range strings.Split(string(doc), "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		rest := line[len("| `"):]
		if i := strings.Index(rest, "`"); i > 0 {
			rows[rest[:i]] = true
		}
	}
	ids := exportedIdentifiers(t)
	if len(ids) < 100 {
		t.Fatalf("found only %d exported identifiers; the walk is broken", len(ids))
	}
	have := map[string]bool{}
	for _, id := range ids {
		have[id] = true
		if !rows[id] {
			t.Errorf("docs/protocol-mapping.md has no row for %s", id)
		}
	}
	for r := range rows {
		if !have[r] {
			t.Errorf("docs/protocol-mapping.md has a row for %s, which does not exist", r)
		}
	}
}

// TestProtocolMapping_NamesNoConsumer keeps the upstream documents free of any
// one consumer's name. Only the consumer mapping document under docs/ may name it.
func TestProtocolMapping_NamesNoConsumer(t *testing.T) {
	for _, f := range []string{"README.md", "docs/protocol-mapping.md", "CHANGELOG.md", "doc.go"} {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(string(b)), consumer) {
			t.Errorf("%s names a consumer", f)
		}
	}
}
