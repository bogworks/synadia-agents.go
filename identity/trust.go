// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"errors"
	"fmt"
)

// MetadataKeyMinSenderTrust is the key of the prompt endpoint metadata that
// declares what the endpoint requires of the sender, next to max_payload and
// attachments_ok. The status endpoint declares nothing. See SAP §13.9.
const MetadataKeyMinSenderTrust = "min_sender_trust"

// Registration metadata keys of the agent ID, next to agent, owner and
// protocol_version. See SAP §13.10.
const (
	MetadataKeyUserNKey = "user_nkey"
	MetadataKeyAccount  = "account"
	MetadataKeyIDSig    = "id_sig"
)

// MinSenderTrust is what a prompt endpoint requires of the sender. See SAP
// §13.9.
type MinSenderTrust string

const (
	// TrustAny serves every request. It is the default, and what an endpoint
	// without the field implies.
	TrustAny MinSenderTrust = "any"
	// TrustSigned serves a request only when it is verified (SAP §13.7). The
	// receiver may still refuse the sender (403).
	TrustSigned MinSenderTrust = "signed"
)

// ParseMinSenderTrust interprets a declared min_sender_trust. declared is false
// when the metadata has no such key, which implies [TrustAny]. A caller that
// reads an unknown value MUST treat it as [TrustSigned], the strictest level it
// can satisfy on its own. See SAP §13.9.
func ParseMinSenderTrust(value string, declared bool) MinSenderTrust {
	if !declared {
		return TrustAny
	}
	switch MinSenderTrust(value) {
	case TrustAny:
		return TrustAny
	default:
		// "signed" and every unknown value, the empty string included.
		return TrustSigned
	}
}

// ErrCannotSign is the local refusal of SAP §13.4 and §13.9: the target
// endpoint requires a signed sender and the caller cannot sign, because its
// identity is none or unknown, or it holds no seed. The request is not
// published. Test with [errors.Is].
var ErrCannotSign = errors.New("identity: the endpoint requires a signed sender and this caller cannot sign")

// CheckCanSend returns nil when a caller may send to an endpoint that requires
// required. It returns an error wrapping [ErrCannotSign] when the endpoint
// requires signed and canSign is false. The caller fails the send locally,
// before publishing. See SAP §13.9.
func CheckCanSend(required MinSenderTrust, canSign bool) error {
	if required == TrustSigned && !canSign {
		return fmt.Errorf("%w (min_sender_trust is %q)", ErrCannotSign, required)
	}
	return nil
}

// PromptMetadata returns the identity key a host adds to its prompt endpoint
// metadata: min_sender_trust. A host with this chapter configured MUST declare
// it. See SAP §13.9.
func PromptMetadata(trust MinSenderTrust) map[string]string {
	return map[string]string{MetadataKeyMinSenderTrust: string(trust)}
}
