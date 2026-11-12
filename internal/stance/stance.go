// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package stance resolves an [identity.Stance] into the four checks a package
// applies. It holds all of the resolution logic: host, caller and connect only
// read the [Effective] result. SDK policy.
package stance

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/bogworks/synadia-agents.go/identity"
)

// Explicit says which checks the stance set itself with On or Off, and so which
// did not come from the profile.
type Explicit struct {
	Signing, SignedSenders, VerifiedDiscovery, HeartbeatVerification bool
}

// Effective is a resolved stance.
type Effective struct {
	// Profile is the profile in force: the chosen one, or identity.DefaultProfile.
	Profile identity.Profile
	// Chosen is true when the program set Profile, false when the default applies.
	Chosen bool

	// Signing, SignedSenders, VerifiedDiscovery and HeartbeatVerification are the
	// four checks, each from its setting or else from the profile.
	Signing, SignedSenders, VerifiedDiscovery, HeartbeatVerification bool

	// Explicit is true per check when its setting was On or Off.
	Explicit Explicit
}

// Resolve resolves s. It refuses a Profile or a Setting outside the defined
// values.
func Resolve(s identity.Stance) (Effective, error) {
	if s.Profile > identity.Strict {
		return Effective{}, fmt.Errorf("invalid security profile %d", uint8(s.Profile))
	}
	for _, c := range []struct {
		name string
		v    identity.Setting
	}{
		{"Signing", s.Signing}, {"SignedSenders", s.SignedSenders},
		{"VerifiedDiscovery", s.VerifiedDiscovery}, {"HeartbeatVerification", s.HeartbeatVerification},
	} {
		if c.v > identity.Off {
			return Effective{}, fmt.Errorf("invalid security setting %d for %s", uint8(c.v), c.name)
		}
	}
	e := Effective{Profile: s.Profile, Chosen: s.Profile != identity.ProfileDefault}
	if !e.Chosen {
		e.Profile = identity.DefaultProfile
	}
	strict := e.Profile == identity.Strict
	e.Signing, e.Explicit.Signing = pick(s.Signing, strict)
	e.SignedSenders, e.Explicit.SignedSenders = pick(s.SignedSenders, strict)
	e.VerifiedDiscovery, e.Explicit.VerifiedDiscovery = pick(s.VerifiedDiscovery, strict)
	e.HeartbeatVerification, e.Explicit.HeartbeatVerification = pick(s.HeartbeatVerification, strict)
	return e, nil
}

func pick(s identity.Setting, profile bool) (on, explicit bool) {
	switch s {
	case identity.On:
		return true, true
	case identity.Off:
		return false, true
	}
	return profile, false
}

// WarnUnchosen logs once per once, that is once per process for a package's own
// Once, that no stance was chosen and the default profile applies. It logs only
// to log, a logger the program configured, and does nothing when log is nil.
// pkg names the calling package.
func WarnUnchosen(once *sync.Once, log *slog.Logger, pkg string, e Effective) {
	if log == nil {
		return
	}
	once.Do(func() {
		log.Warn("no security stance was chosen; the default profile applies, which may change; pass WithStance to choose one",
			"package", pkg, "profile", e.Profile.String())
	})
}
