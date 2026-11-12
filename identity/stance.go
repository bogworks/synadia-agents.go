// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import "fmt"

// Profile is a named bundle of the four identity checks of a [Stance]. The
// sender identity extension is optional in SAP (§13), so which profile applies
// is SDK policy. The zero value, [ProfileDefault], means none was chosen.
type Profile uint8

const (
	// ProfileDefault means the program chose no profile; the SDK applies
	// [DefaultProfile].
	ProfileDefault Profile = iota
	// Relaxed turns the four checks off: the behavior of v0.1.0, where every
	// identity check is opt-in.
	Relaxed
	// Strict turns the four checks on: a prompt endpoint requires a signed
	// sender, discovery is verified, heartbeats are verified and the program
	// signs.
	Strict
)

// String returns "default", "relaxed" or "strict".
func (p Profile) String() string {
	switch p {
	case ProfileDefault:
		return "default"
	case Relaxed:
		return "relaxed"
	case Strict:
		return "strict"
	}
	return fmt.Sprintf("Profile(%d)", uint8(p))
}

// Setting overrides one check of a [Profile]. The zero value, [Inherit], takes
// the check from the profile.
type Setting uint8

const (
	// Inherit takes the check from the profile.
	Inherit Setting = iota
	// On turns the check on whatever the profile says.
	On
	// Off turns the check off whatever the profile says.
	Off
)

// String returns "inherit", "on" or "off".
func (s Setting) String() string {
	switch s {
	case Inherit:
		return "inherit"
	case On:
		return "on"
	case Off:
		return "off"
	}
	return fmt.Sprintf("Setting(%d)", uint8(s))
}

// DefaultProfile is the profile that applies when the program chooses none.
// SDK policy. It is Relaxed, the behavior of v0.1.0. The default is still
// undecided, and this one line chooses it. A program that needs a profile for
// certain passes it in a [Stance].
const DefaultProfile = Relaxed

// Stance is the security stance of a program: a [Profile] and, for each check,
// an optional override. It is one type shared by host, caller, the heartbeat
// tracker and connect, each of which takes it through its own WithStance
// option (SDK policy; SAP §13 makes the extension optional). The zero value
// chooses nothing and overrides nothing. An option refuses a value outside the
// constants of this file.
//
// An explicit option of a package, such as host.WithMinSenderTrust,
// caller.WithTrustPolicy, caller.WithHeartbeatVerification or
// connect.WithSignedIdentity, always wins over the stance for its own check.
// The order of options does not matter.
type Stance struct {
	// Profile selects the defaults of the four checks. ProfileDefault applies
	// [DefaultProfile].
	Profile Profile

	// Signing is the separate signing override: the program signs its requests
	// and heartbeats, so it needs a signer. On a connect bundle it yields the
	// signer when the credentials hold a user seed (SAP §13.4, §13.6); without
	// a seed it yields none and no error, unless signing was asked for
	// explicitly. With signing on, caller.New and host.New fail when no
	// WithIdentity with a Signer was given. Strict turns it on and Relaxed off.
	// On under Relaxed gives signing without the other checks.
	Signing Setting

	// SignedSenders makes a host's prompt endpoint require a signed sender: its
	// min_sender_trust becomes "signed" (SAP §13.9). It is the host check.
	SignedSenders Setting

	// VerifiedDiscovery makes a client trust only agents whose id_sig verifies
	// (SAP §13.10), as a trust policy that accepts any verified signer does. It
	// gates Agent.Prompt and Agent.Status like an explicit trust policy. It is
	// the caller check.
	VerifiedDiscovery Setting

	// HeartbeatVerification makes a heartbeat tracker verify every heartbeat
	// that carries an Agent-Sender header and discard one that fails (SAP
	// §13.7, §13.11). A heartbeat with no header is kept. It is the tracker
	// check.
	HeartbeatVerification Setting
}
