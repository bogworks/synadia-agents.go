// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package stance resolves an [identity.Stance] into the checks a package
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
// did not come from an SDK default.
type Explicit struct {
	SignedSenders, VerifiedDiscovery, HeartbeatVerification, Signing, RequireSigner bool
}

// Effective is a resolved stance.
type Effective struct {
	// SignedSenders, VerifiedDiscovery, HeartbeatVerification, Signing and
	// RequireSigner are the checks, each from its setting or else from its
	// identity.DefaultX constant.
	SignedSenders, VerifiedDiscovery, HeartbeatVerification, Signing, RequireSigner bool

	// Explicit is true per check when its setting was On or Off.
	Explicit Explicit
}

// Resolve resolves s. It refuses a Setting outside the defined values.
func Resolve(s identity.Stance) (Effective, error) {
	for _, c := range []struct {
		name string
		v    identity.Setting
	}{
		{"SignedSenders", s.SignedSenders}, {"VerifiedDiscovery", s.VerifiedDiscovery},
		{"HeartbeatVerification", s.HeartbeatVerification}, {"Signing", s.Signing},
		{"RequireSigner", s.RequireSigner},
	} {
		if c.v > identity.Off {
			return Effective{}, fmt.Errorf("invalid security setting %d for %s", uint8(c.v), c.name)
		}
	}
	var e Effective
	e.SignedSenders, e.Explicit.SignedSenders = pick(s.SignedSenders, identity.DefaultSignedSenders)
	e.VerifiedDiscovery, e.Explicit.VerifiedDiscovery = pick(s.VerifiedDiscovery, identity.DefaultVerifiedDiscovery)
	e.HeartbeatVerification, e.Explicit.HeartbeatVerification = pick(s.HeartbeatVerification, identity.DefaultHeartbeatVerification)
	e.Signing, e.Explicit.Signing = pick(s.Signing, identity.DefaultSigning)
	e.RequireSigner, e.Explicit.RequireSigner = pick(s.RequireSigner, identity.DefaultRequireSigner)
	return e, nil
}

func pick(s, def identity.Setting) (on, explicit bool) {
	switch s {
	case identity.On:
		return true, true
	case identity.Off:
		return false, true
	}
	return def == identity.On, false
}

// Chosen reports whether s sets at least one check.
func Chosen(s identity.Stance) bool { return s != identity.Stance{} }

// WarnUnchosen logs once per once, that is once per process for a package's own
// Once, that no security setting was chosen and the SDK defaults apply. It logs
// only to log, a logger the program configured, and does nothing when log is
// nil. pkg names the calling package.
func WarnUnchosen(once *sync.Once, log *slog.Logger, pkg string) {
	if log == nil {
		return
	}
	once.Do(func() {
		log.Warn("no security settings were chosen; the SDK defaults apply, which may change; pass WithStance to choose them",
			"package", pkg)
	})
}
