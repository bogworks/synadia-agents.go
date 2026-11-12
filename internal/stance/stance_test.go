// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package stance

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/bogworks/synadia-agents.go/identity"
)

func TestResolve(t *testing.T) {
	const (
		un  = identity.Unset
		on  = identity.On
		off = identity.Off
	)
	type checks [5]bool // senders, discovery, heartbeats, signing, require signer
	// Each check alone: unset takes the default, on and off say what they say.
	var tests = []struct {
		name string
		s    identity.Stance
		want checks
		expl checks
	}{
		{"zero value is every default", identity.Stance{}, checks{false, false, false, false, true}, checks{}},
		{"senders on", identity.Stance{SignedSenders: on}, checks{true, false, false, false, true}, checks{true}},
		{"senders off", identity.Stance{SignedSenders: off}, checks{false, false, false, false, true}, checks{true}},
		{"senders unset", identity.Stance{SignedSenders: un}, checks{false, false, false, false, true}, checks{}},
		{"discovery on", identity.Stance{VerifiedDiscovery: on}, checks{false, true, false, false, true}, checks{false, true}},
		{"discovery off", identity.Stance{VerifiedDiscovery: off}, checks{false, false, false, false, true}, checks{false, true}},
		{"heartbeats on", identity.Stance{HeartbeatVerification: on}, checks{false, false, true, false, true}, checks{false, false, true}},
		{"heartbeats off", identity.Stance{HeartbeatVerification: off}, checks{false, false, false, false, true}, checks{false, false, true}},
		{"signing on", identity.Stance{Signing: on}, checks{false, false, false, true, true}, checks{false, false, false, true}},
		{"signing off", identity.Stance{Signing: off}, checks{false, false, false, false, true}, checks{false, false, false, true}},
		{"require signer on", identity.Stance{RequireSigner: on}, checks{false, false, false, false, true}, checks{false, false, false, false, true}},
		{"require signer off", identity.Stance{RequireSigner: off}, checks{false, false, false, false, false}, checks{false, false, false, false, true}},
		{"all on", identity.Stance{SignedSenders: on, VerifiedDiscovery: on, HeartbeatVerification: on, Signing: on, RequireSigner: on}, checks{true, true, true, true, true}, checks{true, true, true, true, true}},
		{"all off", identity.Stance{SignedSenders: off, VerifiedDiscovery: off, HeartbeatVerification: off, Signing: off, RequireSigner: off}, checks{}, checks{true, true, true, true, true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, err := Resolve(tc.s)
			if err != nil {
				t.Fatal(err)
			}
			if got := (checks{e.SignedSenders, e.VerifiedDiscovery, e.HeartbeatVerification, e.Signing, e.RequireSigner}); got != tc.want {
				t.Errorf("checks %v, want %v", got, tc.want)
			}
			x := e.Explicit
			if got := (checks{x.SignedSenders, x.VerifiedDiscovery, x.HeartbeatVerification, x.Signing, x.RequireSigner}); got != tc.expl {
				t.Errorf("explicit %v, want %v", got, tc.expl)
			}
		})
	}
}

// TestResolve_AgreesWithResolved asserts that the exported defaults, the
// Resolved method and Resolve say the same thing.
func TestResolve_AgreesWithResolved(t *testing.T) {
	e, err := Resolve(identity.Stance{})
	if err != nil {
		t.Fatal(err)
	}
	r := identity.Stance{}.Resolved()
	for name, p := range map[string][2]any{
		"senders":    {e.SignedSenders, r.SignedSenders},
		"discovery":  {e.VerifiedDiscovery, r.VerifiedDiscovery},
		"heartbeats": {e.HeartbeatVerification, r.HeartbeatVerification},
		"signing":    {e.Signing, r.Signing},
		"require":    {e.RequireSigner, r.RequireSigner},
	} {
		if p[0] != (p[1] == identity.On) {
			t.Errorf("%s: Resolve says %v, Resolved says %v", name, p[0], p[1])
		}
	}
}

func TestResolve_RefusesInvalidValues(t *testing.T) {
	for name, s := range map[string]identity.Stance{
		"signing":               {Signing: identity.Off + 1},
		"signed senders":        {SignedSenders: 3},
		"verified discovery":    {VerifiedDiscovery: 3},
		"heartbeat verificaton": {HeartbeatVerification: 3},
		"require signer":        {RequireSigner: 3},
	} {
		if _, err := Resolve(s); err == nil {
			t.Errorf("%s: an invalid value was accepted", name)
		}
	}
}

func TestChosen(t *testing.T) {
	if Chosen(identity.Stance{}) {
		t.Error("the zero value chose nothing")
	}
	for _, s := range []identity.Stance{
		{SignedSenders: identity.Off}, {VerifiedDiscovery: identity.On}, {HeartbeatVerification: identity.On},
		{Signing: identity.Off}, {RequireSigner: identity.Off},
	} {
		if !Chosen(s) {
			t.Errorf("%+v chose a setting", s)
		}
	}
}

func TestWarnUnchosen(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	var once sync.Once
	WarnUnchosen(&once, nil, "pkg") // a nil logger neither warns nor spends the once
	if buf.Len() != 0 {
		t.Fatal(buf.String())
	}
	WarnUnchosen(&once, log, "pkg")
	WarnUnchosen(&once, log, "pkg")
	out := buf.String()
	if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "SDK defaults") || !strings.Contains(out, "WithStance") || !strings.Contains(out, "package=pkg") {
		t.Fatalf("log: %q", out)
	}
	if strings.Contains(out, "profile") {
		t.Fatalf("log mentions a profile: %q", out)
	}
}
