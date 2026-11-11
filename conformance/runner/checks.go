// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"fmt"
)

// adder registers a check under one or more catalog ids.
type adder func(f CheckFunc, ids ...string)

// builtinChecks returns the registry: catalog id to check. Several ids may
// share one check when the spec states one observable behavior in more than
// one place (the catalog's same_as entries).
func builtinChecks() map[string]CheckFunc {
	m := map[string]CheckFunc{}
	add := func(f CheckFunc, ids ...string) {
		for _, id := range ids {
			if _, dup := m[id]; dup {
				panic("sapconform: duplicate check for " + id)
			}
			m[id] = f
		}
	}
	registerInfo(add)
	registerPrompt(add)
	registerStream(add)
	registerHeartbeat(add)
	registerIdentity(add)
	registerExtras(add)
	return m
}

func pass(f string, a ...any) Outcome { return Outcome{Pass, fmt.Sprintf(f, a...)} }
func fail(f string, a ...any) Outcome { return Outcome{Fail, fmt.Sprintf(f, a...)} }
func policy(f string, a ...any) Outcome {
	return Outcome{Policy, fmt.Sprintf(f, a...)}
}
func na(f string, a ...any) Outcome { return Outcome{NotApplicable, fmt.Sprintf(f, a...)} }
func notRun(f string, a ...any) Outcome {
	return Outcome{NotRun, fmt.Sprintf(f, a...)}
}

// verdict turns a list of problems into fail, or pass with the note.
func verdict(problems []string, okNote string) Outcome {
	if len(problems) > 0 {
		if len(problems) > 4 {
			problems = append(problems[:4], fmt.Sprintf("and %d more", len(problems)-4))
		}
		return fail("%s", joinProblems(problems))
	}
	return pass("%s", okNote)
}

func joinProblems(p []string) string {
	out := ""
	for i, s := range p {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}

// perEach wraps an adder so every check it registers runs against every
// instance of the target and fails if any instance fails. Use it only for
// checks that judge one instance's own record (INFO, metadata, endpoints,
// identity fields); a check that sends prompts to the shared queue subject is
// subject-level and runs once per target.
func perEach(add adder) adder {
	return func(f CheckFunc, ids ...string) { add(perInstance(f), ids...) }
}

// perInstance runs f once for each instance of the target, with t.Inst, the
// endpoints and the per-instance observations pointed at that instance. The
// outcomes merge with worse: any failing instance fails the check, and the
// detail names each failing instance (the first four, then a count). Otherwise
// the worst outcome among the instances is reported.
func perInstance(f CheckFunc) CheckFunc {
	return func(ctx context.Context, t *T) Outcome {
		if len(t.Insts) < 2 {
			return f(ctx, t)
		}
		restore := t.saveInstance()
		defer restore()
		var fails []string
		var rest recorder
		for _, in := range t.Insts {
			t.useInstance(in)
			o := f(ctx, t)
			if o.Result == Fail {
				fails = append(fails, fmt.Sprintf("instance %s: %s", in.ID, o.Detail))
				continue
			}
			rest.add(o)
		}
		worst, _ := rest.result()
		if len(fails) > 0 {
			if len(fails) > 4 {
				fails = append(fails[:4], fmt.Sprintf("and %d more instance(s) fail", len(fails)-4))
			}
			return worse(Outcome{Fail, joinProblems(fails)}, worst)
		}
		return withInstanceCount(worst, len(t.Insts))
	}
}

// withInstanceCount notes in a pass detail that every instance passed. It
// writes only the detail of a pass; the result is never touched, and
// TestResultsAreRecordedThroughWorse allows no other write of a detail here.
func withInstanceCount(o Outcome, n int) Outcome {
	if o.Result == Pass {
		o.Detail = fmt.Sprintf("%s (all %d instances)", o.Detail, n)
	}
	return o
}
