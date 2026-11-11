// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller

import (
	"context"
	"errors"
	"testing"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/identity"
)

// TestAgent_UnadmittedRefusedUnderTrustPolicy asserts that under a trust policy
// an Agent that discovery did not admit is refused by Prompt and Status before
// anything is published, and that without a policy the same agent is not.
func TestAgent_UnadmittedRefusedUnderTrustPolicy(t *testing.T) {
	info := &agents.AgentInfo{}
	c := &Client{cfg: config{trust: &TrustPolicy{Accept: func(identity.AgentID, agents.AgentInfo) bool { return true }}}}
	a := newAgent(c, info, false)
	if _, err := a.Prompt(context.Background(), agents.Envelope{Prompt: "x"}); !errors.Is(err, ErrUntrustedAgent) {
		t.Fatalf("Prompt: %v", err)
	}
	if _, err := a.Status(context.Background()); !errors.Is(err, ErrUntrustedAgent) {
		t.Fatalf("Status: %v", err)
	}
	if err := newAgent(c, info, true).checkTrusted(); err != nil {
		t.Fatal(err)
	}
	if err := newAgent(&Client{}, info, false).checkTrusted(); err != nil {
		t.Fatal(err)
	}
}

// TestStatusBound asserts that a status subject is bound only when it is the
// default-form prompt subject with the verb swapped.
func TestStatusBound(t *testing.T) {
	mk := func(prompt, status string) *agents.AgentInfo {
		i := &agents.AgentInfo{Prompt: agents.EndpointInfo{Subject: prompt}}
		if status != "" {
			i.Status = &agents.EndpointInfo{Subject: status}
		}
		return i
	}
	for name, tc := range map[string]struct {
		info *agents.AgentInfo
		want bool
	}{
		"swapped":          {mk("agents.prompt.a.o.n", "agents.status.a.o.n"), true},
		"other name":       {mk("agents.prompt.a.o.n", "agents.status.a.o.evil"), false},
		"other owner":      {mk("agents.prompt.a.o.n", "agents.status.a.x.n"), false},
		"foreign subject":  {mk("agents.prompt.a.o.n", "x.status"), false},
		"no status":        {mk("agents.prompt.a.o.n", ""), false},
		"custom prompt":    {mk("custom.prompt", "agents.status.a.o.n"), false},
		"status is prompt": {mk("agents.prompt.a.o.n", "agents.prompt.a.o.n"), false},
	} {
		if got := statusBound(tc.info); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}
