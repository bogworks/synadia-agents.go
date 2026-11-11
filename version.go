// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// ProtocolVersion is the MAJOR.MINOR protocol version this SDK implements
	// and that a host declares as metadata.protocol_version. See SAP §3.2 and
	// §11.3.
	ProtocolVersion = "0.3"

	// ProtocolMajor is the MAJOR part of [ProtocolVersion]. See SAP §11.1.
	ProtocolMajor = 0

	// ProtocolMinor is the MINOR part of [ProtocolVersion]. See SAP §11.1.
	ProtocolMinor = 3

	// SpecVersion is the full version of the specification the SDK was built
	// against. See SAP §1.2.
	SpecVersion = "0.3.0"

	// SpecCommit is the commit of core-protocol.md in
	// synadia-ai/synadia-agent-sdk-docs that the SDK is pinned to.
	SpecCommit = "c48cea5f07965cc8fd4054e120459f1833b3ed2b"
)

// Version is a protocol version reduced to its MAJOR.MINOR prefix, the only
// part with compatibility meaning. See SAP §11.1.
type Version struct {
	Major int
	Minor int
}

// SDKVersion returns the protocol version this SDK implements. See SAP §11.3.
func SDKVersion() Version { return Version{Major: ProtocolMajor, Minor: ProtocolMinor} }

// String returns the MAJOR.MINOR form. See SAP §11.1.
func (v Version) String() string { return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) }

// ParseVersion parses a protocol version string. Only the MAJOR.MINOR prefix
// is read; patch and pre-release qualifiers ("0.3.0", "0.3-draft") are
// accepted and discarded, because they have no compatibility meaning. Callers
// MUST compare only the MAJOR.MINOR prefix. See SAP §11.1.
func ParseVersion(s string) (Version, error) {
	t := strings.TrimSpace(s)
	i := strings.IndexByte(t, '.')
	if i <= 0 {
		return Version{}, &VersionError{Agent: s, Reason: "expected MAJOR.MINOR"}
	}
	rest := t[i+1:]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j == 0 || (j < len(rest) && rest[j] != '.' && rest[j] != '-') {
		return Version{}, &VersionError{Agent: s, Reason: "expected MAJOR.MINOR"}
	}
	major, err1 := parseDigits(t[:i])
	minor, err2 := parseDigits(rest[:j])
	if err1 != nil || err2 != nil {
		return Version{}, &VersionError{Agent: s, Reason: "expected MAJOR.MINOR"}
	}
	return Version{Major: major, Minor: minor}, nil
}

func parseDigits(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("not a digit")
		}
	}
	return strconv.Atoi(s)
}

// Compatibility is how two protocol versions relate. See SAP §11.2.
type Compatibility int

const (
	// Compatible means the same MAJOR.MINOR: full interoperability.
	Compatible Compatibility = iota
	// MinorDrift means the same MAJOR and a different MINOR: callers SHOULD
	// treat the agent as compatible for the caller's MINOR feature set.
	MinorDrift
	// Incompatible means a different MAJOR: no interoperability guarantee.
	Incompatible
)

// String returns a short name for c.
func (c Compatibility) String() string {
	switch c {
	case Compatible:
		return "compatible"
	case MinorDrift:
		return "minor-drift"
	default:
		return "incompatible"
	}
}

// Compare reports how an agent's version v relates to the SDK's own. See SAP
// §11.2.
func (v Version) Compare(sdk Version) Compatibility {
	switch {
	case v.Major != sdk.Major:
		return Incompatible
	case v.Minor != sdk.Minor:
		return MinorDrift
	default:
		return Compatible
	}
}

// VersionPolicy decides which agent protocol versions a caller accepts.
//
// The policy is SDK policy, not a spec MUST: SAP §11.2 says callers SHOULD
// pin to an exact MAJOR.MINOR until 1.0 and otherwise treat the same MAJOR as
// compatible.
type VersionPolicy int

const (
	// VersionDefault is the SDK default: an exact MAJOR.MINOR match while the
	// protocol is 0.x, and the same MAJOR from 1.0 on. See SAP §11.2.
	VersionDefault VersionPolicy = iota
	// VersionExact accepts only the SDK's own MAJOR.MINOR.
	VersionExact
	// VersionSameMajor accepts any MINOR within the SDK's MAJOR.
	VersionSameMajor
)

// Accepts reports whether the policy accepts an agent whose version relates to
// the SDK's as c. See SAP §11.2.
func (p VersionPolicy) Accepts(c Compatibility) bool {
	exact := p == VersionExact || (p == VersionDefault && ProtocolMajor == 0)
	if exact {
		return c == Compatible
	}
	return c != Incompatible
}

// CheckProtocolVersion applies SAP §11.2 to an agent's declared
// metadata.protocol_version. It returns nil when the policy accepts the
// version and a *VersionError (matching [ErrVersionMismatch] and [ErrLocal])
// otherwise. A version that is not MAJOR.MINOR is refused too. See SAP §3.2,
// §11.1, §11.2.
func CheckProtocolVersion(agentVersion string, policy VersionPolicy) error {
	v, err := ParseVersion(agentVersion)
	if err != nil {
		return err
	}
	if policy.Accepts(v.Compare(SDKVersion())) {
		return nil
	}
	return &VersionError{Agent: agentVersion, Reason: "SDK implements protocol " + ProtocolVersion}
}
