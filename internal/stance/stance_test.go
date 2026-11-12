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
		in  = identity.Inherit
		on  = identity.On
		off = identity.Off
	)
	tests := []struct {
		name   string
		s      identity.Stance
		chosen bool
		prof   identity.Profile
		// signing, senders, discovery, heartbeats
		want [4]bool
		expl [4]bool
	}{
		{"zero value is the default profile", identity.Stance{}, false, identity.DefaultProfile, [4]bool{}, [4]bool{}},
		{"relaxed", identity.Stance{Profile: identity.Relaxed}, true, identity.Relaxed, [4]bool{}, [4]bool{}},
		{"strict", identity.Stance{Profile: identity.Strict}, true, identity.Strict, [4]bool{true, true, true, true}, [4]bool{}},
		{"relaxed signing on", identity.Stance{Profile: identity.Relaxed, Signing: on}, true, identity.Relaxed, [4]bool{true, false, false, false}, [4]bool{true}},
		{"relaxed all on", identity.Stance{Profile: identity.Relaxed, Signing: on, SignedSenders: on, VerifiedDiscovery: on, HeartbeatVerification: on}, true, identity.Relaxed, [4]bool{true, true, true, true}, [4]bool{true, true, true, true}},
		{"strict signing off", identity.Stance{Profile: identity.Strict, Signing: off}, true, identity.Strict, [4]bool{false, true, true, true}, [4]bool{true}},
		{"strict senders off", identity.Stance{Profile: identity.Strict, SignedSenders: off}, true, identity.Strict, [4]bool{true, false, true, true}, [4]bool{false, true}},
		{"strict discovery off", identity.Stance{Profile: identity.Strict, VerifiedDiscovery: off}, true, identity.Strict, [4]bool{true, true, false, true}, [4]bool{false, false, true}},
		{"strict heartbeats off", identity.Stance{Profile: identity.Strict, HeartbeatVerification: off}, true, identity.Strict, [4]bool{true, true, true, false}, [4]bool{false, false, false, true}},
		{"inherit is the profile", identity.Stance{Profile: identity.Strict, Signing: in}, true, identity.Strict, [4]bool{true, true, true, true}, [4]bool{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, err := Resolve(tc.s)
			if err != nil {
				t.Fatal(err)
			}
			if e.Chosen != tc.chosen || e.Profile != tc.prof {
				t.Errorf("chosen %v profile %v, want %v %v", e.Chosen, e.Profile, tc.chosen, tc.prof)
			}
			if got := [4]bool{e.Signing, e.SignedSenders, e.VerifiedDiscovery, e.HeartbeatVerification}; got != tc.want {
				t.Errorf("checks %v, want %v", got, tc.want)
			}
			x := e.Explicit
			if got := [4]bool{x.Signing, x.SignedSenders, x.VerifiedDiscovery, x.HeartbeatVerification}; got != tc.expl {
				t.Errorf("explicit %v, want %v", got, tc.expl)
			}
		})
	}
}

func TestResolve_RefusesInvalidValues(t *testing.T) {
	for name, s := range map[string]identity.Stance{
		"profile":               {Profile: identity.Strict + 1},
		"signing":               {Signing: identity.Off + 1},
		"signed senders":        {SignedSenders: 7},
		"verified discovery":    {VerifiedDiscovery: 7},
		"heartbeat verificaton": {HeartbeatVerification: 7},
	} {
		if _, err := Resolve(s); err == nil {
			t.Errorf("%s: an invalid value was accepted", name)
		}
	}
}

func TestWarnUnchosen(t *testing.T) {
	e, _ := Resolve(identity.Stance{})
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	var once sync.Once
	WarnUnchosen(&once, nil, "pkg", e) // a nil logger neither warns nor spends the once
	if buf.Len() != 0 {
		t.Fatal(buf.String())
	}
	WarnUnchosen(&once, log, "pkg", e)
	WarnUnchosen(&once, log, "pkg", e)
	out := buf.String()
	if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "relaxed") || !strings.Contains(out, "WithStance") {
		t.Fatalf("log: %q", out)
	}
}
