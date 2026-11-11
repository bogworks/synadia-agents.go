// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogworks/synadia-agents.go/conformance"
)

// TestEveryHostMustHasACheck: every MUST and MUST NOT entry that a host can be
// probed for has a registered check, unless the catalog marks it explicitly
// (sdk-internal, not-applicable, or another role's obligation). A strict run
// fails on one that does not, so a skipped MUST cannot read as conforming.
func TestEveryHostMustHasACheck(t *testing.T) {
	cat, err := conformance.Load("../catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	checks := builtinChecks()
	for _, e := range cat.Entries {
		if unchecked(e, checks[e.ID] == nil) {
			t.Errorf("%s (%s, %s) is a host MUST with no registered check and no explicit marking in catalog.json", e.ID, e.Level, e.Capability)
		}
	}
}

func TestUnchecked(t *testing.T) {
	must := conformance.Entry{ID: "X", Level: conformance.LevelMust, Role: conformance.RoleHost, Capability: conformance.CapBlackBox}
	if !unchecked(must, true) || unchecked(must, false) {
		t.Error("a host MUST is unchecked exactly when no check is registered")
	}
	should := must
	should.Level = conformance.LevelShould
	sdk := must
	sdk.Capability = conformance.CapSDK
	op := must
	op.Role = conformance.RoleOperator
	for name, e := range map[string]conformance.Entry{"should": should, "sdk-internal": sdk, "operator": op} {
		if unchecked(e, true) {
			t.Errorf("%s: counted as unchecked", name)
		}
	}
	r := EntryResult{Result: NotRun, Level: conformance.LevelMust, Unchecked: true}
	if !r.IsStrictFailure() || r.IsFailure() {
		t.Error("an unchecked MUST must fail the strict verdict and not the default one")
	}
	rep := &Report{Targets: []TargetReport{{Entries: []EntryResult{r}}}}
	if !rep.OK() || rep.StrictOK() {
		t.Errorf("an unchecked MUST: default OK=%v (want true), strict OK=%v (want false)", rep.OK(), rep.StrictOK())
	}
}

// TestProbesAreSentThroughProbeAuth fails when a function sends on the NATS
// connection or calls collect without being on the list below. Every positive
// probe to a prompt endpoint must go through (*T).send, which signs it with
// probeAuth, so a signed-only endpoint serves it and a missing seed reports
// configuration-missing. The functions listed are the ones that send on purpose
// without that signature; each says why in its doc comment.
func TestProbesAreSentThroughProbeAuth(t *testing.T) {
	allowed := map[string]string{
		"send":          "signs through probeAuth",
		"collect":       "the transport send calls with the headers probeAuth built",
		"probeIdentity": "identity probes: unsigned, malformed or badly signed on purpose (SAP 13.7)",
		"statusObs":     "status is always answerable; unsigned on purpose (SAP 13.9)",
		"statusWith":    "status with a claimed, malformed or bad header on purpose (SAP 13.9)",
		"accountOf":     "a server request for the connection's identity, not a probe",
		"pingObs":       "discovery subjects carry no sender",
		"collectMany":   "discovery subjects carry no sender",
		"requestAll":    "status requests are unsigned on purpose (SAP 13.9); it reads every reply",
		"probeInfo":     "discovery subject, no sender",
		"queryRun":      "publishes the operator-approved answer to a query, not a request to the agent",
	}
	senders := map[string]bool{"Request": true, "RequestMsg": true, "RequestWithContext": true, "RequestMsgWithContext": true,
		"Publish": true, "PublishMsg": true, "PublishRequest": true, "collect": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !senders[sel.Sel.Name] {
					return true
				}
				if _, ok := allowed[fn.Name.Name]; !ok {
					t.Errorf("%s: %s calls %s; send positive probes through (*T).send so probeAuth signs them, or list the function here with the reason it is unsigned", fset.Position(call.Pos()), fn.Name.Name, sel.Sel.Name)
				}
				return true
			})
		}
	}
}
