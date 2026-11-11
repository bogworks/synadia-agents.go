// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// The throwaway users of testdata/keys.json. Test-only.
const (
	aliceSeed = "SUAEJ6GDK6FSSB54LD45Q7W25AW7NUT7MVLBABIR5MIPFUTBW7ZNPK2KYE"
	alicePub  = "UCDUW5V44EBDBIK2FL4CTNDBQFNGBEZVJHSZGQVKRHASN4AV4IWPB5NT"
	bobSeed   = "SUAA37BA6WVMWPX27LE3AREHH5ELK2K5OYDECALTSZBJ7CDMW4AOC3GNAI"
	bobPub    = "UDLPRVYPNSPIPYH4RVGKEHP2AAG4J2CN4GQ3IBTKC6ZDV6SR7ONG27D4"
	testAcct  = "ACME"
)

var testNow = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

func fixedNow() time.Time { return testNow }

func newSigner(t *testing.T, seed string) Signer {
	t.Helper()
	s, err := SignerFromSeed([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newID(t *testing.T, account, user string) AgentID {
	t.Helper()
	id, err := NewAgentID(account, user)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// signedMsg builds a message signed by alice for subject, with the given ts
// and nonce, arriving on arrival.
func signedMsg(t *testing.T, subject, arrival string, payload []byte, ts time.Time, nonce string) Message {
	t.Helper()
	h, err := SignHeader(SignOptions{
		Signer: newSigner(t, aliceSeed), ID: newID(t, testAcct, alicePub), Subject: subject,
		Payload: payload, TS: FormatTimestamp(ts), Nonce: nonce,
	})
	if err != nil {
		t.Fatal(err)
	}
	return Message{Subject: arrival, Data: payload, Header: nats.Header{HeaderName: {h.Encode()}}}
}

func withHeader(m Message, value string) Message {
	m.Header = nats.Header{HeaderName: {value}}
	return m
}

// liveOpts returns live options with a fresh replay set on the fixed clock.
func liveOpts() LiveOptions {
	return LiveOptions{Now: fixedNow, Nonces: NewNonceSet(NonceSetOptions{Now: fixedNow})}
}

func asError(t *testing.T, err error) *Error {
	t.Helper()
	var e *Error
	if err == nil {
		t.Fatal("expected an *Error, got nil")
	}
	if !errors.As(err, &e) {
		t.Fatalf("expected an *Error, got %T: %v", err, err)
	}
	return e
}
