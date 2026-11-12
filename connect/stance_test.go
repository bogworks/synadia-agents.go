// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package connect_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/bogworks/synadia-agents.go/connect"
	"github.com/bogworks/synadia-agents.go/identity"
)

// TestResolveBundle_Stance asserts F4 and the signing setting: a bundle yields
// the signer when the stance sets Signing On and the source holds a seed, fails
// with ErrNoUserSeed when it holds none, and yields no signer for Unset and Off.
func TestResolveBundle_Stance(t *testing.T) {
	e := newEnv(t)
	u := newUser(t)
	nk := e.write(filepath.Join(e.home, "u.nk"), u.seed)
	e.context("seed", map[string]any{"url": serverURL, "nkey": nk})
	e.context("token", map[string]any{"url": serverURL, "token": "T"})

	on, off := identity.On, identity.Off
	tests := []struct {
		name    string
		context string
		stance  identity.Stance
		extra   []connect.BundleOption
		signer  bool
		noSeed  bool // want an ErrNoUserSeed failure
		invalid bool // want an ErrContext failure
	}{
		{"no stance, seed", "seed", identity.Stance{}, nil, false, false, false},
		{"signing unset, seed", "seed", identity.Stance{SignedSenders: on}, nil, false, false, false},
		{"signing on, seed", "seed", identity.Stance{Signing: on}, nil, true, false, false},
		{"signing on, no seed", "token", identity.Stance{Signing: on}, nil, false, true, false},
		{"signing off, seed", "seed", identity.Stance{Signing: off}, nil, false, false, false},
		{"signing off, no seed", "token", identity.Stance{Signing: off}, nil, false, false, false},
		{"signing unset, no seed", "token", identity.Stance{VerifiedDiscovery: on}, nil, false, false, false},
		{"other checks on, signing unset, seed", "seed", identity.Stance{SignedSenders: on, VerifiedDiscovery: on, HeartbeatVerification: on}, nil, false, false, false},
		{"signing on, other checks off, seed", "seed", identity.Stance{Signing: on, SignedSenders: off, VerifiedDiscovery: off, HeartbeatVerification: off}, nil, true, false, false},
		{"RequireSigner off has no effect, no seed", "token", identity.Stance{Signing: on, RequireSigner: off}, nil, false, true, false},
		{"RequireSigner on has no effect, signing unset", "token", identity.Stance{RequireSigner: on}, nil, false, false, false},
		{"explicit option beats unset, seed", "seed", identity.Stance{}, []connect.BundleOption{connect.WithSignedIdentity()}, true, false, false},
		{"explicit option beats signing off, seed", "seed", identity.Stance{Signing: off}, []connect.BundleOption{connect.WithSignedIdentity()}, true, false, false},
		{"explicit option with signing off, no seed", "token", identity.Stance{Signing: off}, []connect.BundleOption{connect.WithSignedIdentity()}, false, true, false},
		{"invalid signing", "seed", identity.Stance{Signing: 3}, nil, false, false, true},
		{"invalid RequireSigner", "seed", identity.Stance{RequireSigner: 3}, nil, false, false, true},
		{"invalid setting", "seed", identity.Stance{SignedSenders: 3}, nil, false, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The order of the options must not matter.
			orders := [][]connect.BundleOption{
				append([]connect.BundleOption{connect.WithStance(tc.stance)}, tc.extra...),
				append(append([]connect.BundleOption{}, tc.extra...), connect.WithStance(tc.stance)),
			}
			for _, opts := range orders {
				b, err := connect.ResolveBundle(t.Context(), connect.Source{Context: tc.context}, opts...)
				switch {
				case tc.noSeed:
					_ = wantContextErr(t, err)
					if !errors.Is(err, connect.ErrNoUserSeed) {
						t.Fatalf("err = %v, want ErrNoUserSeed", err)
					}
					continue
				case tc.invalid:
					_ = wantContextErr(t, err)
					if errors.Is(err, connect.ErrNoUserSeed) {
						t.Fatalf("err = %v", err)
					}
					continue
				case err != nil:
					t.Fatal(err)
				}
				if got := b.Signer != nil; got != tc.signer {
					t.Fatalf("signer present = %v, want %v", got, tc.signer)
				}
				if tc.signer && b.Signer.PublicKey() != u.pub {
					t.Fatalf("signer key %q", b.Signer.PublicKey())
				}
				if want := map[bool]string{true: "connect.Bundle(signed)", false: "connect.Bundle(off)"}[tc.signer]; b.String() != want {
					t.Fatalf("String() = %q, want %q", b.String(), want)
				}
			}
		})
	}
	t.Run("url source, signing unset, no seed", func(t *testing.T) {
		b, err := connect.ResolveBundle(t.Context(), connect.Source{URL: serverURL}, connect.WithStance(identity.Stance{VerifiedDiscovery: on}))
		if err != nil || b.Signer != nil {
			t.Fatalf("signer = %v, err = %v", b.Signer, err)
		}
	})
}
