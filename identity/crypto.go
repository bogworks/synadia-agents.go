// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"

	"github.com/nats-io/nkeys"
)

// signatureLen is the length of an ed25519 signature in bytes.
const signatureLen = 64

// errBase64 is returned by decodeBase64URL for any input outside base64url
// without padding.
var errBase64 = errors.New("identity: not base64url without padding")

// encodeBase64URL encodes b as base64url without padding (RFC 4648 §5), the
// encoding of every signature in SAP §13.
func encodeBase64URL(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeBase64URL decodes base64url without padding. It accepts the alphabet
// [A-Za-z0-9_-] only: no padding, no whitespace, no line breaks. Go's base64
// decoder silently skips CR and LF, so the alphabet is checked first.
func decodeBase64URL(s string) ([]byte, error) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return nil, errBase64
		}
	}
	if len(s)%4 == 1 {
		return nil, errBase64
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, errBase64
	}
	return b, nil
}

// decodeSignature decodes a base64url signature and requires 64 bytes.
func decodeSignature(s string) ([]byte, error) {
	b, err := decodeBase64URL(s)
	if err != nil {
		return nil, err
	}
	if len(b) != signatureLen {
		return nil, errors.New("identity: signature is not 64 bytes")
	}
	return b, nil
}

// sha256Hex returns the lowercase hex SHA-256 of data.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// verifyWithPublicKey checks an ed25519 signature against a public NKEY. An
// undecodable key verifies nothing.
func verifyWithPublicKey(publicKey string, input, sig []byte) bool {
	kp, err := nkeys.FromPublicKey(publicKey)
	if err != nil {
		return false
	}
	return kp.Verify(input, sig) == nil
}
