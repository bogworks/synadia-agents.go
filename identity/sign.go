// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"crypto/rand"
	"errors"
	"fmt"
	"time"
)

// SignOptions are the inputs of [SignHeader].
type SignOptions struct {
	// Signer signs with the seed of the user in ID. Required.
	Signer Signer
	// ID is the caller's agent ID (SAP §13.4). Its user must be the signer's
	// public key. Required.
	ID AgentID
	// Name is an optional display name. It is outside the signature.
	Name string
	// Subject is the subject to sign: the one the caller publishes to, or the
	// exporter's subject behind an import the caller's own account renamed
	// (SAP §13.6.2). Required.
	Subject string
	// Payload is the raw message payload that is hashed. An empty payload, as
	// in a status request, hashes to the SHA-256 of zero bytes.
	Payload []byte
	// Now returns the current time. Nil means [time.Now]. Test hook.
	Now func() time.Time
	// TS overrides the timestamp (known-answer vectors). Empty means now,
	// second precision.
	TS string
	// Nonce overrides the nonce (known-answer vectors). Empty means a fresh
	// 22-character NUID-shaped nonce.
	Nonce string
}

// SignHeader builds and signs an Agent-Sender header for one request. ts and
// nonce are fresh unless overridden. The header is signed over the input of SAP
// §13.6.1. A caller attaches the result to every prompt and status request,
// and a host to every heartbeat (SAP §13.5, §13.11). It MUST NOT be attached to
// a reply (SAP §13.11).
func SignHeader(opts SignOptions) (SenderHeader, error) {
	if opts.Signer == nil {
		return SenderHeader{}, errors.New("identity: SignHeader needs a Signer")
	}
	if opts.ID.User() != opts.Signer.PublicKey() || opts.ID.User() == "" {
		return SenderHeader{}, fmt.Errorf("%w: the agent ID names a different user than the signer", ErrIdentityMismatch)
	}
	if _, err := ParseAgentID(string(opts.ID)); err != nil {
		return SenderHeader{}, err
	}
	if !validSubject(opts.Subject) {
		return SenderHeader{}, errors.New("identity: the subject to sign is not a NATS subject")
	}
	if opts.Name != "" && !ValidName(opts.Name) {
		return SenderHeader{}, errors.New("identity: the display name is not valid")
	}
	ts := opts.TS
	if ts == "" {
		now := time.Now
		if opts.Now != nil {
			now = opts.Now
		}
		ts = FormatTimestamp(now())
	} else if _, err := ParseTimestamp(ts); err != nil {
		return SenderHeader{}, err
	}
	nonce := opts.Nonce
	if nonce == "" {
		n, err := NewNonce()
		if err != nil {
			return SenderHeader{}, err
		}
		nonce = n
	} else if !ValidNonce(nonce) {
		return SenderHeader{}, errors.New("identity: the nonce must match [A-Za-z0-9_-]{1,64}")
	}

	h := SenderHeader{
		V: HeaderVersion, Account: opts.ID.Account(), User: opts.ID.User(), Name: opts.Name,
		Sub: opts.Subject, TS: ts, Nonce: nonce,
	}
	sig, err := opts.Signer.Sign(SignedInput(h.Account, h.User, h.Sub, h.TS, h.Nonce, opts.Payload))
	if err != nil {
		return SenderHeader{}, fmt.Errorf("identity: signing the Agent-Sender header: %w", err)
	}
	h.Sig = encodeBase64URL(sig)
	return h, nil
}

// ClaimHeader builds an unsigned claim: exactly v, account, user and an
// optional name. A caller MAY send one when it holds no seed (SAP §13.5). A
// claim is display-only: receivers grant nothing on it (SAP §13.1).
func ClaimHeader(id AgentID, name string) (SenderHeader, error) {
	if _, err := ParseAgentID(string(id)); err != nil {
		return SenderHeader{}, err
	}
	if name != "" && !ValidName(name) {
		return SenderHeader{}, errors.New("identity: the display name is not valid")
	}
	return SenderHeader{V: HeaderVersion, Account: id.Account(), User: id.User(), Name: name}, nil
}

const nonceAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// NewNonce returns a fresh nonce of 22 characters from [A-Za-z0-9], the shape
// of a NUID, which SAP §13.5 RECOMMENDS. The source is crypto/rand.
func NewNonce() (string, error) {
	var raw [nuidLength]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("identity: reading random bytes for a nonce: %w", err)
	}
	var out [nuidLength]byte
	for i, b := range raw {
		// 248 is the largest multiple of 62 below 256; fold the rest with a
		// second draw to keep the choice uniform.
		for b >= 248 {
			var one [1]byte
			if _, err := rand.Read(one[:]); err != nil {
				return "", fmt.Errorf("identity: reading random bytes for a nonce: %w", err)
			}
			b = one[0]
		}
		out[i] = nonceAlphabet[int(b)%len(nonceAlphabet)]
	}
	return string(out[:]), nil
}
