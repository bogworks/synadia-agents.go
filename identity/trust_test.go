// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMinSenderTrust_UnknownValueIsSigned(t *testing.T) {
	cases := []struct {
		name     string
		value    string
		declared bool
		want     MinSenderTrust
	}{
		{"not declared implies any", "", false, TrustAny},
		{"any", "any", true, TrustAny},
		{"signed", "signed", true, TrustSigned},
		{"unknown value", "verified-by-notary", true, TrustSigned},
		{"empty but declared", "", true, TrustSigned},
		{"different case", "ANY", true, TrustSigned},
		{"a future level", "attested", true, TrustSigned},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseMinSenderTrust(tc.value, tc.declared); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

// A caller that cannot sign fails locally, before publishing, against a signed
// endpoint (SAP §13.4, §13.9).
func TestCheckCanSend_LocalRefusal(t *testing.T) {
	cases := []struct {
		required MinSenderTrust
		canSign  bool
		refused  bool
	}{
		{TrustAny, false, false},
		{TrustAny, true, false},
		{TrustSigned, true, false},
		{TrustSigned, false, true},
		{ParseMinSenderTrust("never-heard-of-it", true), false, true},
	}
	for _, tc := range cases {
		err := CheckCanSend(tc.required, tc.canSign)
		if (err != nil) != tc.refused || (err != nil && !errors.Is(err, ErrCannotSign)) {
			t.Errorf("required=%s canSign=%v: %v", tc.required, tc.canSign, err)
		}
	}
}

// A §13 host declares min_sender_trust on prompt and nothing on status.
func TestPromptMetadata_DeclaresMinSenderTrust(t *testing.T) {
	for _, tr := range []MinSenderTrust{TrustAny, TrustSigned} {
		m := PromptMetadata(tr)
		if len(m) != 1 || m[MetadataKeyMinSenderTrust] != string(tr) || MetadataKeyMinSenderTrust != "min_sender_trust" {
			t.Errorf("got %v", m)
		}
	}
}

func TestHeaderSizes(t *testing.T) {
	id := newID(t, testAcct, alicePub)
	signer := newSigner(t, aliceSeed)
	for _, name := range []string{"", "claude-code", "ünïcode-名前"} {
		h, err := SignHeader(SignOptions{Signer: signer, ID: id, Name: name, Subject: subj, Payload: []byte(payload)})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := ExpectedHeaderWireBytes(id, name, subj, true), h.WireBytes(); got != want {
			t.Errorf("signed %q: expected %d, header is %d", name, got, want)
		}
		if h.WireBytes() > MaxHeaderWireBytes(subj, name) {
			t.Errorf("the bound %d is below the real size %d", MaxHeaderWireBytes(subj, name), h.WireBytes())
		}
		claim, _ := ClaimHeader(id, name)
		if got, want := ExpectedHeaderWireBytes(id, name, subj, false), claim.WireBytes(); got != want {
			t.Errorf("claim %q: expected %d, header is %d", name, got, want)
		}
	}
	// The framing is "NATS/1.0\r\nAgent-Sender: " + "\r\n\r\n".
	if HeaderFramingBytes != len("NATS/1.0\r\n")+len(HeaderName+": ")+len("\r\n\r\n") {
		t.Errorf("framing constant is %d", HeaderFramingBytes)
	}
}

func TestSignHeader_Refusals(t *testing.T) {
	signer := newSigner(t, aliceSeed)
	id := newID(t, testAcct, alicePub)
	base := SignOptions{Signer: signer, ID: id, Subject: subj}
	for name, mutate := range map[string]func(*SignOptions){
		"no signer":       func(o *SignOptions) { o.Signer = nil },
		"another user":    func(o *SignOptions) { o.ID = newID(t, testAcct, bobPub) },
		"zero id":         func(o *SignOptions) { o.ID = "" },
		"bad subject":     func(o *SignOptions) { o.Subject = "a b" },
		"empty subject":   func(o *SignOptions) { o.Subject = "" },
		"bad name":        func(o *SignOptions) { o.Name = "a\nb" },
		"bad nonce":       func(o *SignOptions) { o.Nonce = "a.b" },
		"bad ts":          func(o *SignOptions) { o.TS = "yesterday" },
		"over-long nonce": func(o *SignOptions) { o.Nonce = strings.Repeat("n", 65) },
	} {
		t.Run(name, func(t *testing.T) {
			o := base
			mutate(&o)
			if _, err := SignHeader(o); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	t.Run("fresh nonce and ts per call", func(t *testing.T) {
		now := func() time.Time { return testNow }
		a, _ := SignHeader(SignOptions{Signer: signer, ID: id, Subject: subj, Now: now})
		b, _ := SignHeader(SignOptions{Signer: signer, ID: id, Subject: subj, Now: now})
		if a.Nonce == b.Nonce || len(a.Nonce) != 22 || !ValidNonce(a.Nonce) {
			t.Fatalf("nonces %q %q", a.Nonce, b.Nonce)
		}
		if a.TS != "2026-08-28T12:00:00Z" {
			t.Fatalf("ts %q", a.TS)
		}
	})
	t.Run("empty payload hashes to the empty digest", func(t *testing.T) {
		in := string(SignedInput("A", "U", "s", "t", "n", nil))
		if !strings.Contains(in, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855") {
			t.Fatal(in)
		}
	})
}

func TestResolver_ReverseLookup(t *testing.T) {
	alice := newSigner(t, aliceSeed)
	id := newID(t, testAcct, alicePub)
	const prompt = "agents.prompt.demo.alice.example"
	meta, err := RegistrationMetadata(alice, id, "demo", "alice", prompt)
	if err != nil {
		t.Fatal(err)
	}
	good := Registration{Name: "demo", ID: "i1", PromptSubject: prompt,
		Metadata: map[string]string{"agent": "demo", "owner": "alice", MetadataKeyUserNKey: meta[MetadataKeyUserNKey],
			MetadataKeyAccount: meta[MetadataKeyAccount], MetadataKeyIDSig: meta[MetadataKeyIDSig]}}
	forged := good
	forged.ID = "i2"
	forged.PromptSubject = "agents.prompt.evil.mallory.example" // republished id_sig, other subject
	unsigned := Registration{Name: "x", Metadata: map[string]string{"agent": "demo", "owner": "alice",
		MetadataKeyUserNKey: bobPub, MetadataKeyAccount: testAcct}}

	clock := newFakeClock(testNow)
	var calls int
	list := func(context.Context) ([]Registration, error) {
		calls++
		return []Registration{forged, unsigned, good}, nil
	}
	r := NewSenderResolver(list, ResolverOptions{TTL: time.Minute, Now: clock.Now})
	ctx := context.Background()

	reg, ok, err := r.Resolve(ctx, id)
	if err != nil || !ok || reg.ID != "i1" || reg.PromptSubject != prompt {
		t.Fatalf("got %+v %v %v", reg, ok, err)
	}
	if _, ok, _ := r.Resolve(ctx, newID(t, testAcct, bobPub)); ok {
		t.Fatal("an unsigned registration resolved")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, the index must be cached", calls)
	}
	clock.Advance(2 * time.Minute)
	_, _, _ = r.Resolve(ctx, id)
	if calls != 2 {
		t.Fatalf("calls = %d after the TTL", calls)
	}
	r.Invalidate()
	_, _, _ = r.Resolve(ctx, id)
	if calls != 3 {
		t.Fatalf("calls = %d after Invalidate", calls)
	}
	if _, _, err := r.Resolve(ctx, "not an id"); !errors.Is(err, ErrInvalidAgentID) {
		t.Fatalf("got %v", err)
	}

	boom := errors.New("scatter-gather failed")
	r2 := NewSenderResolver(func(context.Context) ([]Registration, error) { return nil, boom }, ResolverOptions{})
	if _, _, err := r2.Resolve(ctx, id); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

func TestVerifyAgentID_Tampering(t *testing.T) {
	alice := newSigner(t, aliceSeed)
	id := newID(t, testAcct, alicePub)
	const prompt = "agents.prompt.demo.alice.example"
	m, _ := RegistrationMetadata(alice, id, "demo", "alice", prompt)
	m["agent"], m["owner"] = "demo", "alice"
	if !VerifyAgentID(m, prompt) {
		t.Fatal("good metadata does not verify")
	}
	for name, mutate := range map[string]func(map[string]string){
		"agent changed":   func(m map[string]string) { m["agent"] = "other" },
		"owner changed":   func(m map[string]string) { m["owner"] = "mallory" },
		"account changed": func(m map[string]string) { m["account"] = "OTHER" },
		"user changed":    func(m map[string]string) { m[MetadataKeyUserNKey] = bobPub },
		"id_sig missing":  func(m map[string]string) { delete(m, MetadataKeyIDSig) },
		"id_sig padded":   func(m map[string]string) { m[MetadataKeyIDSig] += "==" },
		"id_sig short":    func(m map[string]string) { m[MetadataKeyIDSig] = m[MetadataKeyIDSig][:40] },
		"user not a key":  func(m map[string]string) { m[MetadataKeyUserNKey] = "alice" },
	} {
		t.Run(name, func(t *testing.T) {
			c := map[string]string{}
			for k, v := range m {
				c[k] = v
			}
			mutate(c)
			if VerifyAgentID(c, prompt) {
				t.Fatal("verified")
			}
		})
	}
	// An Agent-Sender signature is not an id_sig, and the reverse.
	sig, _ := alice.Sign(SignedInput(testAcct, alicePub, prompt, "t", "n", nil))
	m2 := map[string]string{"agent": "demo", "owner": "alice", MetadataKeyUserNKey: alicePub, MetadataKeyAccount: testAcct,
		MetadataKeyIDSig: encodeBase64URL(sig)}
	if VerifyAgentID(m2, prompt) {
		t.Fatal("an Agent-Sender signature passed as an id_sig")
	}
}
