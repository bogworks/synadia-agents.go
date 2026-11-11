// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"errors"
	"testing"
)

// SAP:L643 (§11.1): callers compare only the MAJOR.MINOR prefix; qualifiers
// have no compatibility meaning.
//
// catalog: SAP-11.1-01
func TestVersion_ParseMajorMinorPrefix(t *testing.T) {
	cases := []struct {
		in   string
		want Version
		ok   bool
	}{
		{"0.3", Version{0, 3}, true},
		{"0.3.0", Version{0, 3}, true},
		{"0.3-draft", Version{0, 3}, true},
		{"0.3.0-rc.1", Version{0, 3}, true},
		{" 1.10 ", Version{1, 10}, true},
		{"0.3x", Version{}, false},
		{"3", Version{}, false},
		{"", Version{}, false},
		{".3", Version{}, false},
		{"a.b", Version{}, false},
		{"0.", Version{}, false},
		{"99999999999999999999.1", Version{}, false},
	}
	for _, c := range cases {
		got, err := ParseVersion(c.in)
		if (err == nil) != c.ok || (c.ok && got != c.want) {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v ok=%v", c.in, got, err, c.want, c.ok)
		}
		if err != nil && !errors.Is(err, ErrVersionMismatch) {
			t.Errorf("ParseVersion(%q) error does not match ErrVersionMismatch: %v", c.in, err)
		}
	}
}

// SAP:L647-L649 (§11.2): same MAJOR.MINOR, same MAJOR different MINOR, different MAJOR.
func TestVersion_Compare(t *testing.T) {
	sdk := Version{0, 3}
	for _, c := range []struct {
		v    Version
		want Compatibility
	}{
		{Version{0, 3}, Compatible},
		{Version{0, 2}, MinorDrift},
		{Version{0, 4}, MinorDrift},
		{Version{1, 3}, Incompatible},
	} {
		if got := c.v.Compare(sdk); got != c.want {
			t.Errorf("%v.Compare(%v) = %v, want %v", c.v, sdk, got, c.want)
		}
	}
}

// REQ-002, SAP:L651: the default pins the exact MAJOR.MINOR while 0.x.
func TestVersion_CheckProtocolVersionPolicies(t *testing.T) {
	cases := []struct {
		agent  string
		policy VersionPolicy
		ok     bool
	}{
		{"0.3", VersionDefault, true},
		{"0.3.7", VersionDefault, true},
		{"0.2", VersionDefault, false},
		{"0.4", VersionDefault, false},
		{"1.3", VersionDefault, false},
		{"0.3", VersionExact, true},
		{"0.2", VersionExact, false},
		{"0.2", VersionSameMajor, true},
		{"0.9", VersionSameMajor, true},
		{"1.0", VersionSameMajor, false},
		{"garbage", VersionSameMajor, false},
	}
	for _, c := range cases {
		err := CheckProtocolVersion(c.agent, c.policy)
		if (err == nil) != c.ok {
			t.Errorf("CheckProtocolVersion(%q, %v) = %v, want ok=%v", c.agent, c.policy, err, c.ok)
		}
		if err != nil {
			var ve *VersionError
			if !errors.As(err, &ve) || !errors.Is(err, ErrLocal) || !errors.Is(err, ErrVersionMismatch) {
				t.Errorf("error %v is not a local *VersionError", err)
			}
		}
	}
}

func TestVersion_Constants(t *testing.T) {
	if ProtocolVersion != "0.3" || SDKVersion().String() != ProtocolVersion {
		t.Fatalf("protocol constants disagree: %q %v", ProtocolVersion, SDKVersion())
	}
	if Compatible.String() != "compatible" || MinorDrift.String() != "minor-drift" || Incompatible.String() != "incompatible" {
		t.Fatal("Compatibility.String")
	}
}
