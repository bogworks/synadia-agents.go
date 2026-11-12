// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity_test

import (
	"testing"

	"github.com/bogworks/synadia-agents.go/identity"
)

func TestStance_Strings(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{identity.ProfileDefault.String(), "default"},
		{identity.Relaxed.String(), "relaxed"},
		{identity.Strict.String(), "strict"},
		{identity.Profile(9).String(), "Profile(9)"},
		{identity.Inherit.String(), "inherit"},
		{identity.On.String(), "on"},
		{identity.Off.String(), "off"},
		{identity.Setting(9).String(), "Setting(9)"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
	if identity.ProfileDefault != 0 || identity.Inherit != 0 {
		t.Error("the zero values must mean not chosen and inherit")
	}
}
