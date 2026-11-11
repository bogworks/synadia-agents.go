// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/nats-io/nkeys"
)

// ErrInvalidSeed is returned (wrapped) when key material is not a usable user
// seed or credentials file. The error text never contains the seed.
var ErrInvalidSeed = errors.New("identity: invalid user seed")

// ErrIdentityMismatch is returned (wrapped) when two sources of a connection's
// identity disagree, or when a seed does not belong to the JWT it came with.
// The connection's identity is then unknown. See SAP §13.4.
var ErrIdentityMismatch = errors.New("identity: identity sources disagree")

var userSeedRegexp = regexp.MustCompile(`^SU[A-Z2-7]{56}$`)

// Signer signs with a connection's user NKEY seed. The program supplies the
// seed (or a credentials file); the SDK reads key material from nowhere else.
// See SAP §13.6.
//
// Sign may be backed by an HSM or a KMS. Implementations must be safe for
// concurrent use.
type Signer interface {
	// PublicKey returns the user public NKEY ("U...") this signer signs for.
	PublicKey() string
	// Sign returns an ed25519 signature over data (the NKEY Sign operation).
	Sign(data []byte) ([]byte, error)
}

// JWTSigner is a [Signer] that also carries the user JWT of a credentials
// file. The JWT is a source of the connection's own identity (SAP §13.4).
type JWTSigner interface {
	Signer
	// JWT returns the user JWT.
	JWT() string
}

// SignerWiper is implemented by signers that can clear their key material.
type SignerWiper interface {
	// Wipe clears the key material. A later Sign fails.
	Wipe()
}

type nkeySigner struct {
	mu  sync.RWMutex
	kp  nkeys.KeyPair
	pub string
	jwt string
}

func (s *nkeySigner) PublicKey() string { return s.pub }

func (s *nkeySigner) JWT() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.jwt
}

func (s *nkeySigner) Sign(data []byte) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.kp == nil {
		return nil, errors.New("identity: signer has been wiped")
	}
	return s.kp.Sign(data)
}

// Wipe clears the key pair and the JWT.
func (s *nkeySigner) Wipe() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kp != nil {
		s.kp.Wipe()
		s.kp = nil
	}
	s.jwt = ""
}

// String and GoString print the public key only, so a signer never leaks a seed
// through a format verb or a log line.
func (s *nkeySigner) String() string   { return "SenderSigner(" + s.pub + ")" }
func (s *nkeySigner) GoString() string { return s.String() }

// MarshalJSON exposes the public key only.
func (s *nkeySigner) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		PublicKey string `json:"publicKey"`
	}{s.pub})
}

// SignerFromSeed builds a [Signer] from a user seed ("SU..."). Surrounding
// whitespace, for example a trailing newline from a seed file, is ignored, and
// a "-----BEGIN USER NKEY SEED-----" block is accepted. The seed is not
// retained and no error contains it. See SAP §13.2.1.
func SignerFromSeed(seed []byte) (Signer, error) {
	return signerFromSeedAndJWT(seed, "")
}

// SignerFromCreds builds a [JWTSigner] from the text of a NATS credentials
// file: the user JWT and the user seed. A seed that does not belong to the
// JWT's subject is rejected with [ErrIdentityMismatch]. See SAP §13.4.
func SignerFromCreds(creds []byte) (JWTSigner, error) {
	jwt, err := nkeys.ParseDecoratedJWT(creds)
	if err != nil || jwt == "" || jwt == string(creds) {
		return nil, fmt.Errorf("%w: credentials have no user JWT block", ErrInvalidSeed)
	}
	kp, err := nkeys.ParseDecoratedNKey(creds)
	if err != nil {
		return nil, fmt.Errorf("%w: credentials have no user seed block", ErrInvalidSeed)
	}
	seed, err := seedBytes(kp)
	if err != nil {
		return nil, fmt.Errorf("%w: credentials seed is unreadable", ErrInvalidSeed)
	}
	defer wipeBytes(seed)
	s, err := signerFromSeedAndJWT(seed, jwt)
	if err != nil {
		return nil, err
	}
	return s.(JWTSigner), nil //nolint:errcheck // invariant: the signer built here implements JWTSigner
}

// SignerFromSeedAndJWT is [SignerFromSeed] with a user JWT, for a program that
// holds the seed and the JWT apart. The JWT must name the seed's public key as
// its subject, or the call fails with [ErrIdentityMismatch].
func SignerFromSeedAndJWT(seed []byte, jwt string) (JWTSigner, error) {
	s, err := signerFromSeedAndJWT(seed, jwt)
	if err != nil {
		return nil, err
	}
	return s.(JWTSigner), nil //nolint:errcheck // invariant: the signer built here implements JWTSigner
}

// wipeBytes zeroes b. Go gives no stronger guarantee: the runtime may have
// copied a buffer before this runs, and a string cannot be cleared.
func wipeBytes(b []byte) { clear(b) }

// seedBytes returns a private copy of kp's seed and wipes kp. The caller must
// wipeBytes the copy. (kp.Wipe overwrites kp's own buffer, so the copy is made
// first.)
func seedBytes(kp nkeys.KeyPair) ([]byte, error) {
	defer kp.Wipe()
	s, err := kp.Seed()
	if err != nil {
		return nil, err
	}
	return bytes.Clone(s), nil
}

// seedFromDecorated returns a private copy of the seed in a decorated block
// ("-----BEGIN USER NKEY SEED-----"). The caller must wipeBytes it.
func seedFromDecorated(block []byte) ([]byte, error) {
	kp, err := nkeys.ParseDecoratedNKey(block)
	if err != nil {
		return nil, err
	}
	return seedBytes(kp)
}

func signerFromSeedAndJWT(seed []byte, jwt string) (Signer, error) {
	text := bytes.TrimSpace(seed)
	if bytes.Contains(text, []byte("BEGIN")) {
		s, err := seedFromDecorated(text)
		if err != nil {
			return nil, fmt.Errorf("%w: no user seed block", ErrInvalidSeed)
		}
		defer wipeBytes(s)
		text = s
	}
	if !userSeedRegexp.Match(text) {
		return nil, fmt.Errorf("%w: expected a user seed (SU and 56 base32 characters)", ErrInvalidSeed)
	}
	kp, err := nkeys.FromSeed(text) // copies text; the copy is the key pair's own
	if err != nil {
		return nil, fmt.Errorf("%w: checksum or prefix is wrong", ErrInvalidSeed)
	}
	pub, err := kp.PublicKey()
	if err != nil || !strings.HasPrefix(pub, "U") {
		kp.Wipe()
		return nil, fmt.Errorf("%w: not a user seed", ErrInvalidSeed)
	}
	if jwt != "" {
		id, jerr := IdentityFromJWT(jwt)
		if jerr != nil {
			kp.Wipe()
			return nil, jerr
		}
		if id.User() != pub {
			kp.Wipe()
			return nil, fmt.Errorf("%w: the seed derives %s but the JWT names %s", ErrIdentityMismatch, pub, id.User())
		}
	}
	return &nkeySigner{kp: kp, pub: pub, jwt: jwt}, nil
}

// IdentityFromJWT returns the agent ID a user JWT carries: sub is the user
// public NKEY, and the account is nats.issuer_account when a signing key issued
// the JWT, else iss. The JWT signature is not checked: the server already
// authenticated it. See SAP §13.4.
func IdentityFromJWT(jwt string) (AgentID, error) { //nolint:revive // documented public name (docs/protocol-mapping.md), kept for parity with upstream identityFromJWT
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("identity: JWT does not have three parts")
	}
	raw, err := decodeBase64URL(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", fmt.Errorf("identity: JWT payload is not base64url")
	}
	if checkNoDuplicateKeys(raw) != nil {
		return "", fmt.Errorf("identity: JWT payload repeats a JSON member name")
	}
	var claims struct {
		Sub  string `json:"sub"`
		Iss  string `json:"iss"`
		NATS struct {
			IssuerAccount string `json:"issuer_account"`
		} `json:"nats"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", fmt.Errorf("identity: JWT payload is not a JSON object")
	}
	account := claims.Iss
	if claims.NATS.IssuerAccount != "" {
		account = claims.NATS.IssuerAccount
	}
	id, err := NewAgentID(account, claims.Sub)
	if err != nil {
		return "", fmt.Errorf("identity: JWT does not carry a usable (account, user) pair: %w", err)
	}
	return id, nil
}
