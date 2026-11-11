// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import "strings"

// severity orders the results of one check from best to worst. A check's
// result can only get worse as evidence arrives, never better, so every place
// that records or combines a result goes through worse.
//
//	fail > configuration-missing > not-run > not-applicable > policy > pass
//
// Anything that means "this was not verified" ranks above pass and below fail,
// so a strict run is never made safer by a later gap in the check (a missing
// seed, a probe that did not apply) hiding something already observed.
// Report-level incompleteness (Report.Incomplete) is not a per-check result and
// has no rank here; it fails both verdicts on its own. A result this table does
// not know ranks as the worst, so a new state cannot weaken a verdict by
// accident.
var severity = map[Result]int{
	Pass: 0, Policy: 1, NotApplicable: 2, NotRun: 3, ConfigMissing: 4, Fail: 5,
}

// knownResult reports whether r is a result the runner defines. A result it
// does not know ranks above fail in worse and fails both verdicts, so a new or
// corrupt state can never read as conforming.
func knownResult(r Result) bool {
	_, ok := severity[r]
	return ok
}

func rank(r Result) int {
	if n, ok := severity[r]; ok {
		return n
	}
	return len(severity)
}

// worse returns the worse of two outcomes. On a tie the first one wins, so the
// earlier recorded result is kept deterministically. The winner keeps its own
// detail; when the other outcome is itself a gap or a failure (not-run or
// worse) and says something different, its detail is appended so the evidence
// of a fail is not lost behind a later configuration-missing, or the reverse.
func worse(a, b Outcome) Outcome {
	win, lose := a, b
	if rank(b.Result) > rank(a.Result) {
		win, lose = b, a
	}
	if rank(lose.Result) >= rank(NotRun) && lose.Detail != "" && !strings.Contains(win.Detail, lose.Detail) {
		if win.Detail == "" {
			win.Detail = lose.Detail
		} else {
			win.Detail += "; also " + string(lose.Result) + ": " + lose.Detail
		}
	}
	return win
}

// recorder holds the one result of a check being run: everything the check
// observed and everything the runner added is merged into it with worse. It is
// the only way an entry's result is set; TestResultsAreRecordedThroughWorse
// fails the build of a direct write.
type recorder struct {
	out Outcome
	set bool
}

// add merges o into the recorded result.
func (r *recorder) add(o Outcome) {
	if !r.set {
		r.out, r.set = o, true
		return
	}
	r.out = worse(r.out, o)
}

// result is the merged outcome, or ok false when nothing was recorded.
func (r *recorder) result() (Outcome, bool) { return r.out, r.set }
