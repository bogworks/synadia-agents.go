// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package sapconform_test

import (
	"strings"
	"testing"

	"github.com/bogworks/synadia-agents.go/conformance/runner"
)

// instanceLevelIDs are runner entries that judge one instance's own record.
// They are package-level so the runner's tests are not linked as SDK coverage.
var instanceLevelIDs = []string{"SAP-3.2-02", "SAP-2-03"}

// Instances that share a prompt subject are one target, but an instance-level
// check judges each instance's own record: a later bad instance must fail it,
// and the detail must name that instance.
func TestSharedSubject_EveryInstanceIsChecked(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	good := newFake(t, connect(t, s), fakeOpts{})
	bad := newFake(t, connect(t, s), fakeOpts{NoOwnerMeta: true})
	rep, res := runOnce(t, fastConfig(connect(t, s)))
	if rep.OK() {
		t.Fatal("the run passed although one instance has no metadata.owner")
	}
	for _, id := range instanceLevelIDs {
		e := res[id]
		if e.Result != runner.Fail {
			t.Errorf("%s: %s (%s), want fail", id, e.Result, e.Detail)
			continue
		}
		if !strings.Contains(e.Detail, bad.id) {
			t.Errorf("%s: detail %q does not name the bad instance %s", id, e.Detail, bad.id)
		}
		if strings.Contains(e.Detail, good.id) {
			t.Errorf("%s: detail %q names the conforming instance %s", id, e.Detail, good.id)
		}
	}
}

func TestSharedSubject_TwoGoodInstancesPass(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	newFake(t, connect(t, s), fakeOpts{})
	newFake(t, connect(t, s), fakeOpts{})
	rep, res := runOnce(t, fastConfig(connect(t, s)))
	if !rep.OK() {
		for _, f := range rep.Failures() {
			t.Errorf("%s: %s", f.ID, f.Detail)
		}
	}
	res.want(t, runner.Pass, instanceLevelIDs...)
	if d := res[instanceLevelIDs[0]].Detail; !strings.Contains(d, "all 2 instances") {
		t.Errorf("detail %q does not say both instances passed", d)
	}
}
