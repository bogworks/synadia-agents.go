// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"errors"
	"testing"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/nats-io/nats.go/micro"
)

// An extra endpoint may not sit on the instance's attachments subject either.
func TestCheckEndpoints_AttachmentsSubjectReserved(t *testing.T) {
	s, err := agents.NewAgentSubject("a", "o", "n")
	if err != nil {
		t.Fatal(err)
	}
	for _, subj := range []string{s.Attachments(), "agents.attachments.>"} {
		err := checkEndpoints([]Endpoint{{Name: "x", Subject: subj, Handler: micro.HandlerFunc(func(micro.Request) {})}}, s)
		if !errors.Is(err, ErrEndpointOverlap) {
			t.Errorf("%q: %v, want ErrEndpointOverlap", subj, err)
		}
	}
}

func TestSubjectsOverlap(t *testing.T) {
	const p = "agents.prompt.a.o.n"
	cases := []struct {
		a, b string
		want bool
	}{
		{p, p, true},
		{p, "agents.prompt.a.o.m", false},
		{p, "agents.prompt.>", true},
		{p, "agents.>", true},
		{p, ">", true},
		{p, "agents.prompt.*.o.n", true},
		{p, "agents.*.a.o.n", true},
		{p, "agents.*.*.*.*", true},
		{p, "agents.*.*.*", false},
		{p, "agents.prompt.a.o", false},
		{p, "agents.prompt.a.o.n.x", false},
		{p, "agents.prompt.a.o.n.>", false},
		{"agents.prompt.a.o.>", p, true},
		{"x.>", "x", false},
		{"x.>", "x.y", true},
		{"x.*", "x.>", true},
		{"x.*", "y.*", false},
		{"*", "x", true},
		{"*", "x.y", false},
		{">", ">", true},
	}
	for _, tc := range cases {
		if got := subjectsOverlap(tc.a, tc.b); got != tc.want {
			t.Errorf("subjectsOverlap(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := subjectsOverlap(tc.b, tc.a); got != tc.want {
			t.Errorf("subjectsOverlap(%q, %q) = %v, want %v (swapped)", tc.b, tc.a, got, tc.want)
		}
	}
}

func TestDefaultReplySubjectPolicy(t *testing.T) {
	cases := []struct {
		subject string
		refused bool
	}{
		{"_INBOX.abc123", false},
		{"_INBOX.agents.client.tok", false},
		{"my.custom.inbox.prefix.x", false},
		{"inbox", false},
		{"", true},
		{"$SYS.REQ.SERVER.PING", true},
		{"$JS.API.STREAM.LIST", true},
		{"$SRV.PING", true},
		{"$KV.bucket.key", true},
		{"$", true},
		{"_INBOX.*", true},
		{"_INBOX.>", true},
		{"_INBOX..x", true},
		{"_INBOX.a b", true},
		{"agents.prompt.victim.owner.n", true},
		{"agents.status.victim.owner.n", true},
		{"agents.hb.victim.owner.n", true},
		{"agents.prompt", true},
		{"agents.heartbeat.victim.owner.n", false},
		{"agents.callers.inbox.1", false},
		{"agents", false},
		{"agentsx.foo", false},
		{"x.agents.y", false},
	}
	for _, tc := range cases {
		err := DefaultReplySubjectPolicy(tc.subject)
		if tc.refused != (err != nil) {
			t.Errorf("%q: err = %v, refused want %v", tc.subject, err, tc.refused)
		}
		if err != nil && !errors.Is(err, ErrReplySubject) {
			t.Errorf("%q: error does not match ErrReplySubject: %v", tc.subject, err)
		}
	}
}
