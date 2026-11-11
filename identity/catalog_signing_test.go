// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

// catSignedInputTag is the first line the spec fixes for a sender signature.
const catSignedInputTag = "AGENT-SENDER-V1"

// catVerifySig checks sig (base64url, unpadded) over input with the public key,
// without using this package's verifier.
func catVerifySig(t *testing.T, user, sig string, input []byte) bool {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		t.Fatalf("sig %q is not unpadded base64url: %v", sig, err)
	}
	kp, err := nkeys.FromPublicKey(user)
	if err != nil {
		t.Fatal(err)
	}
	return kp.Verify(input, raw) == nil
}

// TestSignedInput_SevenLinesInOrder asserts that the signed input is the
// seven-line byte string of SAP §13.6.1: the tag, account, user, subject, ts,
// nonce and the payload hash in that order, each line ending in one LF.
//
// catalog: SAP-13.6.1-02
func TestSignedInput_SevenLinesInOrder(t *testing.T) {
	payload := []byte(`{"prompt":"hello"}`)
	sum := sha256.Sum256(payload)
	got := SignedInput("ACME", alicePub, "agents.prompt.a.o.n", "2026-08-28T12:00:00Z", "nonce-1", payload)
	want := "AGENT-SENDER-V1\nACME\n" + alicePub + "\nagents.prompt.a.o.n\n2026-08-28T12:00:00Z\nnonce-1\n" + hex.EncodeToString(sum[:]) + "\n"
	if string(got) != want {
		t.Fatalf("signed input\n got %q\nwant %q", got, want)
	}
	if n := bytes.Count(got, []byte{'\n'}); n != 7 || got[len(got)-1] != '\n' || bytes.Contains(got, []byte("\r")) || bytes.Contains(got, []byte("\n\n")) {
		t.Fatalf("not seven LF-terminated lines: %q", got)
	}
	if !strings.HasPrefix(string(got), catSignedInputTag+"\n") {
		t.Fatalf("first line is not the tag: %q", got)
	}
}

// TestSignedInput_FieldsAreTheHeaderValues asserts that the signer builds the
// input from the header's own account, user, sub, ts and nonce, byte for byte:
// the signature in a signed header verifies over exactly those values, and over
// nothing else.
//
// catalog: SAP-13.6.1-03
func TestSignedInput_FieldsAreTheHeaderValues(t *testing.T) {
	payload := []byte("body")
	h, err := SignHeader(SignOptions{
		Signer: newSigner(t, aliceSeed), ID: newID(t, "Acme_1-x", alicePub), Subject: "Svc.Prompt.Mixed-Case_1",
		Payload: payload, TS: "2026-08-28T12:00:00.250Z", Nonce: "Nonce_A-9",
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.Account != "Acme_1-x" || h.User != alicePub || h.Sub != "Svc.Prompt.Mixed-Case_1" || h.TS != "2026-08-28T12:00:00.250Z" || h.Nonce != "Nonce_A-9" {
		t.Fatalf("header %+v", h)
	}
	if !catVerifySig(t, h.User, h.Sig, SignedInput(h.Account, h.User, h.Sub, h.TS, h.Nonce, payload)) {
		t.Fatal("the signature does not verify over the header's own values")
	}
	// Any other rendering of a field is a different input.
	for name, input := range map[string][]byte{
		"account lower-cased": SignedInput(strings.ToLower(h.Account), h.User, h.Sub, h.TS, h.Nonce, payload),
		"sub lower-cased":     SignedInput(h.Account, h.User, strings.ToLower(h.Sub), h.TS, h.Nonce, payload),
		"ts without fraction": SignedInput(h.Account, h.User, h.Sub, "2026-08-28T12:00:00Z", h.Nonce, payload),
		"nonce changed":       SignedInput(h.Account, h.User, h.Sub, h.TS, "Nonce_A-8", payload),
		"payload changed":     SignedInput(h.Account, h.User, h.Sub, h.TS, h.Nonce, []byte("body ")),
	} {
		if catVerifySig(t, h.User, h.Sig, input) {
			t.Errorf("%s: the signature also verifies over a different input", name)
		}
	}
	// The receiver rebuilds the same input from what it received.
	msg := Message{Subject: h.Sub, Data: payload, Header: nats.Header{HeaderName: {h.Encode()}}}
	if s, err := VerifyStored(msg, StoredOptions{}); err != nil || s == nil || s.Trust != TrustVerified {
		t.Fatalf("receiver: %+v %v", s, err)
	}
}

// TestSignedInput_EmptyPayloadHashesZeroBytes asserts that an empty payload
// hashes to the SHA-256 of zero bytes, for the input and for a signed status
// request.
//
// catalog: SAP-13.6.1-04
func TestSignedInput_EmptyPayloadHashesZeroBytes(t *testing.T) {
	const zero = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	for name, payload := range map[string][]byte{"nil": nil, "empty": {}} {
		in := string(SignedInput("ACME", alicePub, "agents.status.a.o.n", "2026-08-28T12:00:00Z", "n", payload))
		lines := strings.Split(strings.TrimSuffix(in, "\n"), "\n")
		if len(lines) != 7 || lines[6] != zero {
			t.Errorf("%s payload: last line %q, want %s", name, lines[len(lines)-1], zero)
		}
	}
	h, err := SignHeader(SignOptions{Signer: newSigner(t, aliceSeed), ID: newID(t, "ACME", alicePub),
		Subject: "agents.status.a.o.n", TS: "2026-08-28T12:00:00Z", Nonce: "n"})
	if err != nil {
		t.Fatal(err)
	}
	want := "AGENT-SENDER-V1\nACME\n" + alicePub + "\nagents.status.a.o.n\n2026-08-28T12:00:00Z\nn\n" + zero + "\n"
	if !catVerifySig(t, alicePub, h.Sig, []byte(want)) {
		t.Fatal("a header signed with no payload does not verify over the zero-byte hash")
	}
	// The hash is over the raw bytes: a payload that differs only in a trailing
	// newline hashes differently.
	a := SignedInput("A", alicePub, "s", "t", "n", []byte("x"))
	b := SignedInput("A", alicePub, "s", "t", "n", []byte("x\n"))
	if bytes.Equal(a, b) {
		t.Fatal("the hash ignores a trailing newline")
	}
}

// TestSignedInput_SignatureForm asserts that the signature is ed25519 over the
// signed input, base64url without padding (86 characters), and that the signed
// input itself is never put in the header.
//
// catalog: SAP-13.6.1-05
func TestSignedInput_SignatureForm(t *testing.T) {
	payload := []byte("p")
	h, err := SignHeader(SignOptions{Signer: newSigner(t, aliceSeed), ID: newID(t, "ACME", alicePub),
		Subject: "agents.prompt.a.o.n", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{86}$`).MatchString(h.Sig) || strings.ContainsAny(h.Sig, "=+/") {
		t.Fatalf("sig %q is not unpadded base64url of 64 bytes", h.Sig)
	}
	raw, err := base64.RawURLEncoding.DecodeString(h.Sig)
	if err != nil || len(raw) != 64 {
		t.Fatalf("sig decodes to %d bytes, %v", len(raw), err)
	}
	if !catVerifySig(t, h.User, h.Sig, SignedInput(h.Account, h.User, h.Sub, h.TS, h.Nonce, payload)) {
		t.Fatal("sig is not an ed25519 signature over the signed input")
	}
	sum := sha256.Sum256(payload)
	wire := h.Encode()
	if strings.Contains(wire, catSignedInputTag) || strings.ContainsAny(wire, "\n\r") || strings.Contains(wire, hex.EncodeToString(sum[:])) {
		t.Fatalf("the signed input leaked into the header: %s", wire)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(wire), &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"v", "account", "user", "sub", "ts", "nonce", "sig", "name"} {
		delete(doc, k)
	}
	if len(doc) != 0 {
		t.Fatalf("the header carries fields beyond the wire form: %v", doc)
	}
}

// TestSignedInput_TagSeparatesSenderAndIDSignatures asserts that the fixed
// first line keeps a sender signature and an id_sig from being interchangeable,
// in both directions, though one seed makes both.
//
// catalog: SAP-13.6.1-06
func TestSignedInput_TagSeparatesSenderAndIDSignatures(t *testing.T) {
	signer := newSigner(t, aliceSeed)
	id := newID(t, "ACME", alicePub)
	const subject = "agents.prompt.demo.alice.example"
	if !bytes.HasPrefix(SignedInput("ACME", alicePub, subject, "ts", "n", nil), []byte("AGENT-SENDER-V1\n")) ||
		!bytes.HasPrefix(AgentIDSignedInput(alicePub, "ACME", "a", "o", subject), []byte("AGENT-ID-V1\n")) {
		t.Fatal("the two signed inputs do not start with their own tags")
	}

	// id_sig used as a header sig.
	idSig, err := SignAgentID(signer, id, "demo", "alice", subject)
	if err != nil {
		t.Fatal(err)
	}
	h := SenderHeader{V: HeaderVersion, Account: "ACME", User: alicePub, Sub: subject, TS: FormatTimestamp(testNow), Nonce: "n1", Sig: idSig}
	msg := Message{Subject: subject, Header: nats.Header{HeaderName: {h.Encode()}}}
	if s, err := VerifyStored(msg, StoredOptions{}); s != nil || asError(t, err).Code != 401 {
		t.Fatalf("an id_sig passed as a sig: %+v %v", s, err)
	}

	// A sender sig used as an id_sig.
	sender, err := SignHeader(SignOptions{Signer: signer, ID: id, Subject: subject, Payload: nil, TS: FormatTimestamp(testNow), Nonce: "n2"})
	if err != nil {
		t.Fatal(err)
	}
	meta := map[string]string{MetadataKeyUserNKey: alicePub, MetadataKeyAccount: "ACME", "agent": "demo", "owner": "alice", MetadataKeyIDSig: sender.Sig}
	if VerifyAgentID(meta, subject) {
		t.Fatal("a sender sig passed as an id_sig")
	}
	meta[MetadataKeyIDSig] = idSig
	if !VerifyAgentID(meta, subject) {
		t.Fatal("the control id_sig does not verify")
	}

	// The tag is part of what the verifier checks: the same fields signed under
	// the other tag do not verify as a sender sig.
	input := SignedInput("ACME", alicePub, subject, h.TS, "n3", nil)
	swapped := append([]byte("AGENT-ID-V1"), input[len(catSignedInputTag):]...)
	sig, err := signer.Sign(swapped)
	if err != nil {
		t.Fatal(err)
	}
	h2 := SenderHeader{V: HeaderVersion, Account: "ACME", User: alicePub, Sub: subject, TS: h.TS, Nonce: "n3", Sig: encodeBase64URL(sig)}
	msg2 := Message{Subject: subject, Header: nats.Header{HeaderName: {h2.Encode()}}}
	if s, err := VerifyStored(msg2, StoredOptions{}); s != nil || asError(t, err).Code != 401 {
		t.Fatalf("a signature under the other tag verified: %+v %v", s, err)
	}
}

// TestSignedInput_KnownAnswerOperatorAccount reproduces the published
// known-answer sender signature of SAP Appendix B.13 byte for byte, with the
// inputs and expected values written out here and not read from a fixture
// file.
//
// catalog: SAP-B.13-01
func TestSignedInput_KnownAnswerOperatorAccount(t *testing.T) {
	const (
		seed    = "SUAEJ6GDK6FSSB54LD45Q7W25AW7NUT7MVLBABIR5MIPFUTBW7ZNPK2KYE"
		account = "AABYLMBR6Q2CDXTLGRQCFA2GP76BGCDF7NZF2OVHH4RQ7L3Y3TZWJDRL"
		user    = "UCDUW5V44EBDBIK2FL4CTNDBQFNGBEZVJHSZGQVKRHASN4AV4IWPB5NT"
		subject = "agents.prompt.demo-agent.alice.example"
		ts      = "2026-08-28T12:00:00Z"
		nonce   = "nonce-operator-account-signed"
		wantSig = "EPGc0tNF0ZQeBqXUduwUp3zdi33k1AKIXOX-PUPSeY9QtS4V3d2EZLq-fHuPuYkw6Cc3HdVn_jVI58Y_3YBZBw"
	)
	payload := []byte(`{"prompt":"hello"}`)
	wantInput := "AGENT-SENDER-V1\n" + account + "\n" + user + "\n" + subject + "\n" + ts + "\n" + nonce + "\n" +
		"8a44725210b9dcd4fefd9f0eca07b70ae45e69274a3105fb25eb426a2cf8bbf4\n"
	if got := string(SignedInput(account, user, subject, ts, nonce, payload)); got != wantInput {
		t.Fatalf("signed input\n got %q\nwant %q", got, wantInput)
	}
	id, err := NewAgentID(account, user)
	if err != nil || len(id) != 113 {
		t.Fatalf("agent ID %q: %v", id, err)
	}
	h, err := SignHeader(SignOptions{Signer: newSigner(t, seed), ID: id, Name: "claude-code", Subject: subject, Payload: payload, TS: ts, Nonce: nonce})
	if err != nil {
		t.Fatal(err)
	}
	if h.Sig != wantSig {
		t.Fatalf("sig\n got %s\nwant %s", h.Sig, wantSig)
	}
	if n := len(h.Encode()); n != 373 {
		t.Errorf("header value is %d bytes, the spec says 373", n)
	}
	if n := h.WireBytes(); n != 401 {
		t.Errorf("header block is %d bytes, the spec says 401", n)
	}
	msg := Message{Subject: subject, Data: payload, Header: nats.Header{HeaderName: {h.Encode()}}}
	if s, err := VerifyStored(msg, StoredOptions{}); err != nil || s == nil || s.ID != id {
		t.Fatalf("receiver: %+v %v", s, err)
	}
}
