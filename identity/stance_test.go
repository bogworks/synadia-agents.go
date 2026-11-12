// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity_test

import (
	"testing"

	"github.com/bogworks/synadia-agents.go/identity"
)

func TestStance_Strings(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{identity.Unset.String(), "unset"},
		{identity.On.String(), "on"},
		{identity.Off.String(), "off"},
		{identity.Setting(9).String(), "Setting(9)"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
	if identity.Unset != 0 {
		t.Error("the zero value must mean unset")
	}
}

func TestStance_Defaults(t *testing.T) {
	for name, d := range map[string]identity.Setting{
		"SignedSenders": identity.DefaultSignedSenders, "VerifiedDiscovery": identity.DefaultVerifiedDiscovery,
		"HeartbeatVerification": identity.DefaultHeartbeatVerification, "Signing": identity.DefaultSigning,
		"RequireSigner": identity.DefaultRequireSigner,
	} {
		if d != identity.On && d != identity.Off {
			t.Errorf("the default of %s is %v, want on or off", name, d)
		}
	}
}

func TestStance_Resolved(t *testing.T) {
	zero := identity.Stance{}.Resolved()
	want := identity.Stance{
		SignedSenders:         identity.DefaultSignedSenders,
		VerifiedDiscovery:     identity.DefaultVerifiedDiscovery,
		HeartbeatVerification: identity.DefaultHeartbeatVerification,
		Signing:               identity.DefaultSigning,
		RequireSigner:         identity.DefaultRequireSigner,
	}
	if zero != want {
		t.Errorf("Resolved() of the zero value = %+v, want %+v", zero, want)
	}
	// A set check is kept, whichever way it points.
	got := identity.Stance{SignedSenders: identity.On, RequireSigner: identity.Off, Signing: identity.Setting(9)}.Resolved()
	if got.SignedSenders != identity.On || got.RequireSigner != identity.Off || got.Signing != 9 {
		t.Errorf("a set check changed: %+v", got)
	}
	if got.VerifiedDiscovery != identity.DefaultVerifiedDiscovery {
		t.Errorf("an unset check was not replaced: %+v", got)
	}
	if again := got.Resolved(); again != got {
		t.Error("Resolved is not idempotent")
	}
}
