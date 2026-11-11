// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"strings"
	"testing"

	agents "github.com/bogworks/synadia-agents.go"
)

// seedlessT is a session whose every probe stream was never sent because the
// endpoint needs a signed sender and the runner has no seed. Each stream also
// carries a terminated message, so a check that judged it instead of reporting
// the gap would pass or fail on it.
func seedlessT() *T {
	cfg := &Config{}
	cfg.defaults()
	ep := &Endpoint{Name: "prompt", Subject: "agents.prompt.x.y", Meta: map[string]any{agents.MetaAttachmentsOK: true}}
	in := &Instance{ID: "i1"}
	t := &T{cfg: cfg, Insts: []*Instance{in}, Inst: in, Prompt: ep, streams: map[string]*Stream{}, queryObs: map[string]*queryObs{}}
	for _, n := range []string{"plain", "json", "wsjson", "bracket", "att",
		"e-empty", "e-badjson", "e-noprompt", "e-emptyprompt", "e-att", "e-b64", "e-urlsafe", "e-ws", "e-nofilename"} {
		t.streams[n] = &Stream{Name: n, Term: 0, End: "needs-seed: x", NeedSeed: "supply --sender-seed so the runner can send requests it will serve", Msgs: []Msg{{}}}
	}
	return t
}

// TestSeededChecksReportConfigMissingThemselves: a check whose probe stream has
// NeedSeed returns configuration-missing on its own, before it judges the
// stream. The runner converts nothing; it only merges.
func TestSeededChecksReportConfigMissingThemselves(t *testing.T) {
	ids := []string{
		"5.1-01", "5.1-02", "5.1-05", "5.1-06", "5.2-01", "5.2-02",
		"5.3-01", "5.3-02", "5.3-03", "5.3-04", "5.3-05", "5.3-06", "5.6-01",
		"6-01", "6.2-01", "6.2-02", "6.2-04", "6.3-01", "6.3-04",
		"6.4-02", "6.4-03", "6.5-01", "6.5-02", "6.5-03", "6.5-04", "6.5-05",
		"9.1-01", "9.1-02", "9.1-03", "9.1-04", "9.1-06", "9.2-01", "9.2-03",
		"9.3-01", "9.3-02",
	}
	checks := builtinChecks()
	for _, id := range ids {
		// The ids are bare so the catalog's test-coverage scan does not take this
		// cross-cutting test for any one entry's fault test.
		id = "SAP-" + id
		fn := checks[id]
		if fn == nil {
			t.Errorf("%s has no registered check", id)
			continue
		}
		o := fn(context.Background(), seedlessT())
		if o.Result != ConfigMissing || !strings.Contains(o.Detail, "--sender-seed") {
			t.Errorf("%s returned %s (%s), want configuration-missing naming the seed", id, o.Result, o.Detail)
		}
	}
}

func TestSeedGate(t *testing.T) {
	ok := &Stream{Name: "a"}
	gap := &Stream{Name: "b", NeedSeed: "supply --sender-seed"}
	if _, g := seedGate(ok, nil); g {
		t.Error("a sent stream was gated")
	}
	if o, g := seedGate(ok, gap); !g || o.Result != ConfigMissing || o.Detail != "supply --sender-seed" {
		t.Errorf("a stream that was not sent must gate: %+v %v", o, g)
	}
}

// TestCachedOverStreamFailsEveryReader: a stream that hit a resource cap is
// noted again each time a later check reads it from the cache, so no check that
// judges it can pass on the truncated stream.
func TestCachedOverStreamFailsEveryReader(t *testing.T) {
	read := map[string]func(*T){
		"run":      func(t *T) { t.run(context.Background(), "plain") },
		"identity": func(t *T) { t.probeIdentity(context.Background(), "id-x", reqSpec{}) },
		"replay":   func(t *T) { t.pReplay(context.Background(), nil, "id-r") },
	}
	for name, f := range read {
		tt := seedlessT()
		for _, n := range []string{"plain", "id-x", "id-r-1", "id-r-2"} {
			tt.streams[n] = &Stream{Name: n, Over: "more than 1 messages"}
		}
		tt.curOver = nil
		f(tt)
		if len(tt.curOver) == 0 {
			t.Errorf("%s: a cached stream over its cap was read without noting it", name)
		}
	}
}
