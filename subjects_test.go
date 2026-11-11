// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"errors"
	"testing"
)

// SAP:L63-L68 (§2), SAP:L107-L113 (§2.3): the verb-first subjects.
func TestSubject_PromptSubjectFormat(t *testing.T) {
	s, err := NewAgentSubject("claude-code", "aconnolly", "synadia-com-2")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		s.Prompt():      "agents.prompt.claude-code.aconnolly.synadia-com-2",
		s.Heartbeat():   "agents.hb.claude-code.aconnolly.synadia-com-2",
		s.Status():      "agents.status.claude-code.aconnolly.synadia-com-2",
		s.Attachments(): "agents.attachments.claude-code.aconnolly.synadia-com-2",
	}
	for got, w := range want {
		if got != w {
			t.Errorf("got %q want %q", got, w)
		}
	}
}

// SAP:L108 (§2.3): the abbreviation appears in the subject, the canonical name in metadata.
func TestSubject_AbbreviatedToken(t *testing.T) {
	s, err := NewAgentSubject("claude-code", "aconnolly", "synadia-com-2", WithSubjectToken("cc"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Prompt() != "agents.prompt.cc.aconnolly.synadia-com-2" || s.Agent() != "claude-code" {
		t.Fatalf("%+v %s", s, s.Prompt())
	}
	if _, err := NewAgentSubject("claude-code", "o", "n", WithSubjectToken("c.c")); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("bad subject token accepted: %v", err)
	}
	// Without the option the token is the agent.
	z, _ := NewAgentSubject("a", "o", "n")
	if z.Prompt() != "agents.prompt.a.o.n" || z.Token() != "a" {
		t.Fatal(z.Prompt())
	}
}

func TestSubject_NewAgentSubjectRejectsBadTokens(t *testing.T) {
	cases := [][3]string{{"", "o", "n"}, {"a", "$o", "n"}, {"a", "o", "n.m"}, {"a b", "o", "n"}}
	for _, c := range cases {
		if _, err := NewAgentSubject(c[0], c[1], c[2]); err == nil || !errors.Is(err, ErrLocal) {
			t.Errorf("%v accepted: %v", c, err)
		}
	}
}

// SAP:L61-L63 (§2): the reserved verbs, and parse as the inverse of build.
func TestSubject_ParseRoundTrip(t *testing.T) {
	s, _ := NewAgentSubject("pi", "mario", "workspace-1")
	for _, v := range []Verb{VerbPrompt, VerbHeartbeat, VerbStatus, VerbAttachments} {
		subj, serr := s.Subject(v)
		if serr != nil {
			t.Fatal(serr)
		}
		gv, gs, err := ParseSubject(subj)
		if err != nil || gv != v || gs.Agent() != "pi" || gs.Owner() != "mario" || gs.Name() != "workspace-1" || gs.Token() != "pi" {
			t.Errorf("%s: %v %v %+v", v, err, gv, gs)
		}
	}
	if _, err := ParseAgentSubject(s.Prompt(), VerbPrompt); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseAgentSubject(s.Prompt(), VerbStatus); !errors.Is(err, ErrInvalidSubject) {
		t.Fatalf("wrong verb: %v", err)
	}
}

func TestSubject_ParseRejects(t *testing.T) {
	for _, bad := range []string{
		"", "agents", "agents.prompt.a.o", "agents.prompt.a.o.n.x", "foo.prompt.a.o.n",
		"agents.bogus.a.o.n", "agents.prompt..o.n", "agents.prompt.a.$o.n", "agents.prompt.*.o.n",
		"agents.prompt.>.o.n", "agents.prompt.a.o.n m",
	} {
		_, _, err := ParseSubject(bad)
		var se *SubjectError
		if !errors.As(err, &se) || !errors.Is(err, ErrInvalidSubject) || !errors.Is(err, ErrLocal) {
			t.Errorf("ParseSubject(%q) = %v", bad, err)
		}
	}
}

// SAP:L66, L466-L472 (§8.1): the heartbeat subject is protocol-fixed.
func TestSubject_HeartbeatSubject(t *testing.T) {
	if !IsHeartbeatSubject("agents.hb.cc.o.n") || IsHeartbeatSubject("agents.prompt.cc.o.n") || IsHeartbeatSubject("agents.hb.cc.o") {
		t.Fatal("IsHeartbeatSubject")
	}
	if SubjectHeartbeatAll != "agents.hb.*.*.*" {
		t.Fatal(SubjectHeartbeatAll)
	}
}

// SAP:L196-L201 (§4): the two stable discovery subjects.
func TestSubject_DiscoverySubjects(t *testing.T) {
	if SubjectPingAll != "$SRV.PING.agents" || SubjectInfoAll != "$SRV.INFO.agents" {
		t.Fatal("constants")
	}
	got, err := InfoSubject("VMKS6MHK71PCPWGY38A7N5")
	if err != nil || got != "$SRV.INFO.agents.VMKS6MHK71PCPWGY38A7N5" {
		t.Fatal(got, err)
	}
	if id, ok := InstanceIDFromInfoSubject(got); !ok || id != "VMKS6MHK71PCPWGY38A7N5" {
		t.Fatal(id, ok)
	}
	if got, err := PingSubject("X"); err != nil || got != "$SRV.PING.agents.X" {
		t.Fatal(got, err)
	}
	for _, bad := range []string{"", "a.b", "a b", "*", ">"} {
		if _, err := InfoSubject(bad); err == nil {
			t.Errorf("InfoSubject(%q) accepted", bad)
		}
		if _, err := PingSubject(bad); err == nil {
			t.Errorf("PingSubject(%q) accepted", bad)
		}
	}
	for _, bad := range []string{"$SRV.INFO.agents", "$SRV.INFO.agents.", "$SRV.INFO.agents.a.b", "$SRV.PING.agents.a"} {
		if _, ok := InstanceIDFromInfoSubject(bad); ok {
			t.Errorf("InstanceIDFromInfoSubject(%q) ok", bad)
		}
	}
}

// SAP:L118-L127 (§2.4): wildcard subscriptions.
func TestSubject_FilterSubject(t *testing.T) {
	cases := []struct {
		verb          Verb
		a, o, n, want string
	}{
		{VerbPrompt, "", "", "", "agents.prompt.*.*.*"},
		{VerbPrompt, "", "aconnolly", "", "agents.prompt.*.aconnolly.*"},
		{VerbPrompt, "", "", "summarizer", "agents.prompt.*.*.summarizer"},
		{VerbHeartbeat, "cc", "", "", "agents.hb.cc.*.*"},
		{VerbStatus, "cc", "o", "n", "agents.status.cc.o.n"},
	}
	for _, c := range cases {
		got, err := FilterSubject(c.verb, c.a, c.o, c.n)
		if err != nil || got != c.want {
			t.Errorf("got %q, %v want %q", got, err, c.want)
		}
	}
	if _, err := FilterSubject("bogus", "", "", ""); err == nil {
		t.Error("bogus verb accepted")
	}
	if _, err := FilterSubject(VerbPrompt, "a.b", "", ""); err == nil {
		t.Error("dotted token accepted")
	}
	if got, _ := FilterSubject(VerbPrompt, "", "", ""); got != "agents.prompt.*.*.*" {
		t.Error(got)
	}
}

// SAP:L203, L229-L232 (§4.3): the instance name is the 5th token of a default-form subject.
func TestSubject_InstanceNameFromSubject(t *testing.T) {
	cases := map[string]string{
		"agents.prompt.cc.o.my-session": "my-session",
		"agents.status.cc.o.n":          "n",
		"custom.subject.here":           "",
		"agents.prompt.cc.o":            "",
		"agents.other.cc.o.n":           "",
		"agents.prompt.cc.o.n.extra":    "",
	}
	for in, want := range cases {
		if got := InstanceNameFromSubject(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

// SAP:L441, L457 (§7.1): a reply_subject is a fresh publishable subject.
func TestSubject_ValidatePublishSubject(t *testing.T) {
	for _, ok := range []string{"_INBOX.Xj7k9Q2pA", "a", "a.b.c", "agents.prompt.x.y.z"} {
		if err := ValidatePublishSubject(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "a..b", ".a", "a.", "a.*", "*", ">", "a.>", "a b", "a\x00"} {
		if err := ValidatePublishSubject(bad); !errors.Is(err, ErrInvalidSubject) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestSubject_Warnings(t *testing.T) {
	s, _ := NewAgentSubject("Claude", "o", "n")
	if len(s.Warnings()) == 0 {
		t.Fatal("expected a warning for uppercase")
	}
	s2, _ := NewAgentSubject("cc", "o", "n")
	if w := s2.Warnings(); w != nil {
		t.Fatal(w)
	}
	if !VerbHeartbeat.Valid() || Verb("x").Valid() {
		t.Fatal("Verb.Valid")
	}
}
