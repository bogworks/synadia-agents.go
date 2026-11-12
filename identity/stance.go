// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import "fmt"

// Setting is the value of one check of a [Stance]. The zero value, [Unset],
// means the program did not choose, so the SDK default of that check applies.
type Setting uint8

const (
	// Unset means the program did not choose: the SDK default of the check
	// applies (see [DefaultSignedSenders] and the constants beside it).
	Unset Setting = iota
	// On turns the check on.
	On
	// Off turns the check off.
	Off
)

// String returns "unset", "on" or "off".
func (s Setting) String() string {
	switch s {
	case Unset:
		return "unset"
	case On:
		return "on"
	case Off:
		return "off"
	}
	return fmt.Sprintf("Setting(%d)", uint8(s))
}

// The SDK defaults, one per check. They apply to a check left [Unset]. SDK
// policy; the sender identity extension is optional in SAP (§13). The SDK never
// interprets a named profile: the program, or the deployment around it, decides its
// policy and passes concrete settings. These defaults may change in a later
// release, so a program that depends on one sets it.
const (
	// DefaultSignedSenders is the default of [Stance.SignedSenders]: off, so a
	// host serves unsigned senders.
	DefaultSignedSenders = Off
	// DefaultVerifiedDiscovery is the default of [Stance.VerifiedDiscovery]:
	// off, so a client trusts discovery replies unless given a trust policy.
	DefaultVerifiedDiscovery = Off
	// DefaultHeartbeatVerification is the default of
	// [Stance.HeartbeatVerification]: off, so a tracker verifies nothing.
	DefaultHeartbeatVerification = Off
	// DefaultSigning is the default of [Stance.Signing]: off, so the program
	// does not sign.
	DefaultSigning = Off
	// DefaultRequireSigner is the default of [Stance.RequireSigner]: on, so
	// host.New and caller.New fail when signing is on and no Signer was given.
	DefaultRequireSigner = On
)

// Stance is the security stance of a program: one [Setting] per check. It is
// one type shared by host, caller, the heartbeat tracker and connect, each of
// which takes it through its own WithStance option (SDK policy; SAP §13 makes
// the extension optional). The zero value sets nothing, so every default
// applies. An option refuses a value outside the constants of this file.
//
// An explicit option of a package, such as host.WithMinSenderTrust,
// caller.WithTrustPolicy, caller.WithHeartbeatVerification or
// connect.WithSignedIdentity, always wins over the stance for its own check.
// The order of options does not matter.
type Stance struct {
	// SignedSenders makes a host's prompt endpoint require a signed sender: its
	// min_sender_trust becomes "signed" (SAP §13.9). It is the host check.
	// Unset applies [DefaultSignedSenders].
	SignedSenders Setting

	// VerifiedDiscovery makes a client trust only agents whose id_sig verifies
	// (SAP §13.10), as a trust policy that accepts any verified signer does. It
	// gates Agent.Prompt and Agent.Status like an explicit trust policy. It is
	// the caller check. Unset applies [DefaultVerifiedDiscovery].
	VerifiedDiscovery Setting

	// HeartbeatVerification makes a heartbeat tracker verify every heartbeat
	// that carries an Agent-Sender header and discard one that fails (SAP
	// §13.7, §13.11). A heartbeat with no header is kept. It is the tracker
	// check. Unset applies [DefaultHeartbeatVerification].
	HeartbeatVerification Setting

	// Signing says the program signs its requests and heartbeats, so it needs a
	// signer. On a connect bundle, On yields the signer from the user seed of
	// the credentials (SAP §13.4, §13.6) and fails with connect.ErrNoUserSeed
	// when there is none, as connect.WithSignedIdentity does; Unset and Off
	// yield no signer. With signing on, caller.New and host.New need a
	// WithIdentity with a Signer (see RequireSigner). Unset applies
	// [DefaultSigning].
	Signing Setting

	// RequireSigner decides what happens when signing is on and WithIdentity
	// gave no Signer. On makes host.New and caller.New fail. Off lets them
	// succeed and log one warning to the logger set with WithLogger; a caller
	// then fails each request to an endpoint that requires signed senders with
	// [ErrCannotSign]. It has no effect when signing is off, and none on
	// connect. Unset applies [DefaultRequireSigner].
	RequireSigner Setting
}

// Resolved returns s with every Unset check replaced by its default, so a
// program can print the settings that apply. A value outside the constants is
// kept as it is.
func (s Stance) Resolved() Stance {
	return Stance{
		SignedSenders:         s.SignedSenders.or(DefaultSignedSenders),
		VerifiedDiscovery:     s.VerifiedDiscovery.or(DefaultVerifiedDiscovery),
		HeartbeatVerification: s.HeartbeatVerification.or(DefaultHeartbeatVerification),
		Signing:               s.Signing.or(DefaultSigning),
		RequireSigner:         s.RequireSigner.or(DefaultRequireSigner),
	}
}

func (s Setting) or(def Setting) Setting {
	if s == Unset {
		return def
	}
	return s
}
