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

// TestResolveBundle_Stance asserts F4 and the signing override: a bundle yields
// the signer under a stance that turns signing on and the source holds a seed,
// yields none without error when signing came only from the profile and there is
// no seed, and fails with ErrNoUserSeed when signing was asked for explicitly.
func TestResolveBundle_Stance(t *testing.T) {
	e := newEnv(t)
	u := newUser(t)
	nk := e.write(filepath.Join(e.home, "u.nk"), u.seed)
	e.context("seed", map[string]any{"url": serverURL, "nkey": nk})
	e.context("token", map[string]any{"url": serverURL, "token": "T"})

	strict := identity.Stance{Profile: identity.Strict}
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
		{"relaxed, seed", "seed", identity.Stance{Profile: identity.Relaxed}, nil, false, false, false},
		{"strict, seed", "seed", strict, nil, true, false, false},
		{"strict, no seed", "token", strict, nil, false, false, false},
		{"relaxed signing on, seed", "seed", identity.Stance{Profile: identity.Relaxed, Signing: identity.On}, nil, true, false, false},
		{"relaxed signing on, no seed", "token", identity.Stance{Profile: identity.Relaxed, Signing: identity.On}, nil, false, true, false},
		{"default profile signing on, seed", "seed", identity.Stance{Signing: identity.On}, nil, true, false, false},
		{"strict signing off, seed", "seed", identity.Stance{Profile: identity.Strict, Signing: identity.Off}, nil, false, false, false},
		{"strict signing off, no seed", "token", identity.Stance{Profile: identity.Strict, Signing: identity.Off}, nil, false, false, false},
		{"strict other checks off, seed", "seed", identity.Stance{Profile: identity.Strict, SignedSenders: identity.Off, VerifiedDiscovery: identity.Off, HeartbeatVerification: identity.Off}, nil, true, false, false},
		{"explicit option beats relaxed, seed", "seed", identity.Stance{Profile: identity.Relaxed}, []connect.BundleOption{connect.WithSignedIdentity()}, true, false, false},
		{"explicit option beats signing off, seed", "seed", identity.Stance{Profile: identity.Strict, Signing: identity.Off}, []connect.BundleOption{connect.WithSignedIdentity()}, true, false, false},
		{"explicit option under strict, no seed", "token", strict, []connect.BundleOption{connect.WithSignedIdentity()}, false, true, false},
		{"invalid profile", "seed", identity.Stance{Profile: 9}, nil, false, false, true},
		{"invalid setting", "seed", identity.Stance{Signing: 9}, nil, false, false, true},
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
	t.Run("url source, strict, no seed", func(t *testing.T) {
		b, err := connect.ResolveBundle(t.Context(), connect.Source{URL: serverURL}, connect.WithStance(strict))
		if err != nil || b.Signer != nil {
			t.Fatalf("signer = %v, err = %v", b.Signer, err)
		}
	})
}
