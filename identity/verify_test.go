// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

const (
	subj    = "agents.prompt.demo.alice.example"
	payload = `{"prompt":"hello"}`
)

func TestVerifyLive_Absent(t *testing.T) {
	for name, msg := range map[string]Message{
		"no header at all":  {Subject: subj, Data: []byte(payload)},
		"empty header map":  {Subject: subj, Header: nats.Header{}},
		"unknown v":         {Subject: subj, Header: nats.Header{HeaderName: {`{"v":2,"account":"x","user":"y"}`}}},
		"unknown v garbage": {Subject: subj, Header: nats.Header{HeaderName: {`{"v":7}`}}},
		"v as a float 2.0":  {Subject: subj, Header: nats.Header{HeaderName: {`{"v":2.0}`}}},
		"other header name": {Subject: subj, Header: nats.Header{"agent-sender": {`{"v":1}`}}},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := VerifyLive(msg, liveOpts())
			if s != nil || err != nil {
				t.Fatalf("want absent (nil, nil), got %+v, %v", s, err)
			}
			if TrustOf(s) != TrustAbsent {
				t.Fatal("nil sender is not absent")
			}
		})
	}
}

func TestVerifyLive_Malformed400(t *testing.T) {
	good := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
	goodValue := good.Header[HeaderName][0]
	h, _ := ParseHeader(goodValue)
	acct, user := h.Account, h.User
	const sigOK = "NlnxA8iJk5QpVhPXXIpupBplgKpqeGKuHZVqlKMn-koXrvdnE-Er_qUMX1jkOYLEmRPR9DuthxlxZRAVFueEDw"
	body := func(extra string) string {
		return `{"v":1,"account":"` + acct + `","user":"` + user + `"` + extra + `}`
	}
	signed := func(sig string) string {
		return body(`,"sub":"` + subj + `","ts":"2026-08-28T12:00:00Z","nonce":"n1","sig":"` + sig + `"`)
	}
	cases := map[string]string{
		"not JSON":                `agent-sender`,
		"JSON array":              `[1]`,
		"JSON null":               `null`,
		"trailing garbage":        body("") + ` x`,
		"empty":                   ``,
		"missing v":               `{"account":"` + acct + `","user":"` + user + `"}`,
		"v is a string":           `{"v":"1","account":"` + acct + `","user":"` + user + `"}`,
		"v is true":               `{"v":true,"account":"` + acct + `","user":"` + user + `"}`,
		"missing account":         `{"v":1,"user":"` + user + `"}`,
		"missing user":            `{"v":1,"account":"` + acct + `"}`,
		"account is a number":     `{"v":1,"account":5,"user":"` + user + `"}`,
		"user is null":            `{"v":1,"account":"` + acct + `","user":null}`,
		"empty account":           `{"v":1,"account":"","user":"` + user + `"}`,
		"user not an nkey":        `{"v":1,"account":"` + acct + `","user":"alice"}`,
		"account with a dot":      `{"v":1,"account":"a.b","user":"` + user + `"}`,
		"name is a number":        body(`,"name":5`),
		"name with control char":  body(`,"name":"a\u0007b"`),
		"name too long":           body(`,"name":"` + strings.Repeat("x", 65) + `"`),
		"sub is not a subject":    body(`,"sub":"a b"`),
		"sub with empty token":    body(`,"sub":"a..b"`),
		"ts not RFC 3339":         body(`,"ts":"yesterday"`),
		"ts with an offset":       body(`,"ts":"2026-08-28T12:00:00+00:00"`),
		"ts impossible date":      body(`,"ts":"2026-13-45T12:00:00Z"`),
		"nonce with a dot":        body(`,"nonce":"a.b"`),
		"nonce too long":          body(`,"nonce":"` + strings.Repeat("n", 65) + `"`),
		"nonce empty":             body(`,"nonce":""`),
		"sig without sub":         body(`,"ts":"2026-08-28T12:00:00Z","nonce":"n1","sig":"` + sigOK + `"`),
		"sig without ts":          body(`,"sub":"` + subj + `","nonce":"n1","sig":"` + sigOK + `"`),
		"sig without nonce":       body(`,"sub":"` + subj + `","ts":"2026-08-28T12:00:00Z","sig":"` + sigOK + `"`),
		"sig is not a string":     body(`,"sub":"` + subj + `","ts":"2026-08-28T12:00:00Z","nonce":"n1","sig":7`),
		"sig with std alphabet +": signed(strings.Replace(sigOK, "-", "+", 1)),
		"sig with std alphabet /": signed(strings.Replace(sigOK, "_", "/", 1)),
		"sig with padding":        signed(sigOK + "=="),
		"sig with a newline":      signed(sigOK + `\n`),
		"sig with a CRLF inside":  signed(sigOK[:40] + `\r\n` + sigOK[40:]),
		"sig with a space":        signed(sigOK + " "),
		"sig too short":           signed(sigOK[:80]),
		"sig too long":            signed(sigOK + "AAAA"),
		"sig length 1 mod 4":      signed(sigOK[:85]),
		"sig empty string":        signed(""),
		"value over 2048 bytes":   body(`,"name":"` + strings.Repeat("x", 2100) + `"`),
		"invalid UTF-8":           "{\"v\":1,\"account\":\"" + acct + "\",\"user\":\"" + user + "\",\"name\":\"\xff\"}",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			s, err := VerifyLive(withHeader(good, value), liveOpts())
			if s != nil {
				t.Fatalf("a malformed header produced a sender: %+v", s)
			}
			e := asError(t, err)
			if e.Code != 400 || !errors.Is(err, ErrMalformedHeader) || e.Description != DescriptionMalformed {
				t.Fatalf("want 400 malformed, got %v", e)
			}
			if _, serr := VerifyStored(withHeader(good, value), StoredOptions{}); asError(t, serr).Code != 400 {
				t.Fatalf("stored mode: want 400, got %v", serr)
			}
		})
	}
	t.Run("two Agent-Sender values", func(t *testing.T) {
		m := good
		m.Header = nats.Header{HeaderName: {goodValue, goodValue}}
		_, err := VerifyLive(m, liveOpts())
		if asError(t, err).Code != 400 {
			t.Fatalf("got %v", err)
		}
	})
}

// A signature that is malformed base64 must be refused with 400, and must never
// pass because a decoder was lenient. Mutation: accept a malformed signature.
func TestVerifyLive_MalformedBase64SignatureIsRefused(t *testing.T) {
	good := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
	h, err := ParseHeader(good.Header[HeaderName][0])
	if err != nil {
		t.Fatal(err)
	}
	for name, sig := range map[string]string{
		"trailing newline (Go skips it)": h.Sig + "\n",
		"padded":                         h.Sig + "==",
		"standard alphabet":              h.Sig[:85] + "+",
	} {
		t.Run(name, func(t *testing.T) {
			// Encode would escape the newline; build the wire form by hand.
			value := strings.Replace(h.Encode(), h.Sig, strings.ReplaceAll(sig, "\n", `\n`), 1)
			s, verr := VerifyLive(withHeader(good, value), liveOpts())
			if s != nil || verr == nil {
				t.Fatalf("a malformed signature %q verified: %+v", sig, s)
			}
			if e := asError(t, verr); e.Code != 400 || !errors.Is(verr, ErrMalformedHeader) {
				t.Fatalf("malformed sig syntax must be 400 (malformed header), got %d: %v", e.Code, verr)
			}
		})
	}
	if _, err := decodeBase64URL(h.Sig + "\n"); err == nil {
		t.Error("decodeBase64URL accepts a trailing newline")
	}
	if _, err := decodeBase64URL(h.Sig + "="); err == nil {
		t.Error("decodeBase64URL accepts padding")
	}
}

// A well-formed signature that does not verify is 401, never 400: the header
// is syntactically fine, the sender just does not hold the key.
func TestVerifyLive_WellFormedWrongSignatureIsUnauthorized(t *testing.T) {
	good := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
	h, err := ParseHeader(good.Header[HeaderName][0])
	if err != nil {
		t.Fatal(err)
	}
	other := signedMsg(t, subj, subj, []byte("another payload"), testNow, "n1")
	oh, err := ParseHeader(other.Header[HeaderName][0])
	if err != nil {
		t.Fatal(err)
	}
	if oh.Sig == h.Sig {
		t.Fatal("test setup: signatures should differ")
	}
	forged := *h
	forged.Sig = oh.Sig // 64 bytes, valid base64url, signed over other input
	s, verr := VerifyLive(withHeader(good, forged.Encode()), liveOpts())
	if s != nil || verr == nil {
		t.Fatalf("a wrong signature verified: %+v", s)
	}
	if e := asError(t, verr); e.Code != 401 || !errors.Is(verr, ErrVerification) {
		t.Fatalf("want 401 verification failure, got %d: %v", e.Code, verr)
	}
}

func TestVerifyLive_Claimed(t *testing.T) {
	id := newID(t, testAcct, alicePub)
	for _, name := range []string{"", "claude-code"} {
		h, err := ClaimHeader(id, name)
		if err != nil {
			t.Fatal(err)
		}
		s, err := VerifyLive(withHeader(Message{Subject: subj}, h.Encode()), liveOpts())
		if err != nil || s == nil {
			t.Fatalf("got %+v, %v", s, err)
		}
		if s.Trust != TrustClaimed || s.ID != "" || s.Claim.User != alicePub || s.Name != name {
			t.Errorf("got %+v", s)
		}
		if !strings.HasPrefix(s.Describe(), "claimed ") {
			t.Errorf("Describe: %q", s.Describe())
		}
	}
}

// catalog: SAP-13.7.1-02
func TestVerifyLive_Verified(t *testing.T) {
	msg := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
	s, err := VerifyLive(msg, liveOpts())
	if err != nil {
		t.Fatal(err)
	}
	if s.Trust != TrustVerified || s.ID != newID(t, testAcct, alicePub) || s.AccountAttested {
		t.Fatalf("got %+v", s)
	}
	if !strings.HasPrefix(s.Describe(), "verified ") {
		t.Errorf("Describe: %q", s.Describe())
	}
}

// catalog: SAP-13.7.1-01 SAP-13.6.1-06
func TestVerifyLive_Reject401(t *testing.T) {
	alice := newSigner(t, aliceSeed)
	id := newID(t, testAcct, alicePub)
	sign := func(sub, nonce string, ts time.Time, body string) Message {
		h, err := SignHeader(SignOptions{Signer: alice, ID: id, Subject: sub, Payload: []byte(body),
			TS: FormatTimestamp(ts), Nonce: nonce})
		if err != nil {
			t.Fatal(err)
		}
		return Message{Subject: subj, Data: []byte(body), Header: nats.Header{HeaderName: {h.Encode()}}}
	}
	cases := map[string]Message{
		"payload changed": func() Message {
			m := sign(subj, "n1", testNow, payload)
			m.Data = []byte(payload + " ")
			return m
		}(),
		"subject transplanted": func() Message {
			m := sign(subj, "n1", testNow, payload)
			m.Subject = "agents.prompt.other.bob.example"
			return m
		}(),
		"sub is a wildcard pattern": sign("agents.prompt.>", "n1", testNow, payload),
		"ts 31 s old":               sign(subj, "n1", testNow.Add(-31*time.Second), payload),
		"ts 31 s ahead":             sign(subj, "n1", testNow.Add(31*time.Second), payload),
		"ts one hour old":           sign(subj, "n1", testNow.Add(-time.Hour), payload),
		"account swapped under a valid signature": func() Message {
			m := sign(subj, "n1", testNow, payload)
			v := strings.Replace(m.Header[HeaderName][0], `"account":"ACME"`, `"account":"EVIL"`, 1)
			return withHeader(m, v)
		}(),
		"signed by another key": func() Message {
			h, err := SignHeader(SignOptions{Signer: newSigner(t, bobSeed), ID: newID(t, testAcct, bobPub),
				Subject: subj, Payload: []byte(payload), TS: FormatTimestamp(testNow), Nonce: "n1"})
			if err != nil {
				t.Fatal(err)
			}
			h.User = alicePub // claim to be alice
			return Message{Subject: subj, Data: []byte(payload), Header: nats.Header{HeaderName: {h.Encode()}}}
		}(),
		"nonce changed": func() Message {
			m := sign(subj, "n1", testNow, payload)
			return withHeader(m, strings.Replace(m.Header[HeaderName][0], `"nonce":"n1"`, `"nonce":"n2"`, 1))
		}(),
		"an id_sig-shaped signature (other tag)": func() Message {
			sig, err := SignAgentID(alice, id, "a", "b", subj)
			if err != nil {
				t.Fatal(err)
			}
			h := SenderHeader{V: 1, Account: testAcct, User: alicePub, Sub: subj, TS: FormatTimestamp(testNow), Nonce: "n1", Sig: sig}
			return Message{Subject: subj, Header: nats.Header{HeaderName: {h.Encode()}}}
		}(),
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			s, err := VerifyLive(msg, liveOpts())
			if s != nil {
				t.Fatalf("verified: %+v", s)
			}
			e := asError(t, err)
			if e.Code != 401 || !errors.Is(err, ErrVerification) || e.Description != DescriptionRejected {
				t.Fatalf("want 401, got %v", e)
			}
		})
	}
}

func TestVerifyLive_WindowBoundaries(t *testing.T) {
	for name, tc := range map[string]struct {
		offset time.Duration
		ok     bool
	}{
		"exactly 30 s old":   {-30 * time.Second, true},
		"exactly 30 s ahead": {30 * time.Second, true},
		"now":                {0, true},
		"31 s old":           {-31 * time.Second, false},
		"31 s ahead":         {31 * time.Second, false},
	} {
		t.Run(name, func(t *testing.T) {
			msg := signedMsg(t, subj, subj, []byte(payload), testNow.Add(tc.offset), "n1")
			_, err := VerifyLive(msg, liveOpts())
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v, err=%v", tc.ok, err)
			}
		})
	}
	t.Run("a custom window", func(t *testing.T) {
		msg := signedMsg(t, subj, subj, []byte(payload), testNow.Add(-5*time.Second), "n1")
		o := liveOpts()
		o.ReplayWindow = 2 * time.Second
		if _, err := VerifyLive(msg, o); err == nil {
			t.Fatal("5 s old passed a 2 s window")
		}
	})
	t.Run("fractional seconds are accepted", func(t *testing.T) {
		h, _ := SignHeader(SignOptions{Signer: newSigner(t, aliceSeed), ID: newID(t, testAcct, alicePub), Subject: subj,
			Payload: []byte(payload), TS: "2026-08-28T12:00:00.250Z", Nonce: "n1"})
		msg := Message{Subject: subj, Data: []byte(payload), Header: nats.Header{HeaderName: {h.Encode()}}}
		if _, err := VerifyLive(msg, liveOpts()); err != nil {
			t.Fatal(err)
		}
	})
}

// Mutation: skip the timestamp window in live mode.
func TestVerifyLive_TimestampWindowIsEnforced(t *testing.T) {
	old := signedMsg(t, subj, subj, []byte(payload), testNow.Add(-24*time.Hour), "n1")
	s, err := VerifyLive(old, liveOpts())
	if s != nil || asError(t, err).Code != 401 {
		t.Fatalf("a day-old live request verified: %+v %v", s, err)
	}
	future := signedMsg(t, subj, subj, []byte(payload), testNow.Add(24*time.Hour), "n2")
	s, err = VerifyLive(future, liveOpts())
	if s != nil || asError(t, err).Code != 401 {
		t.Fatalf("a day-ahead live request verified: %+v %v", s, err)
	}
}

// Mutation: accept a replayed nonce.
func TestVerifyLive_ReplayedNonceIsRefused(t *testing.T) {
	msg := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
	o := liveOpts()
	if _, err := VerifyLive(msg, o); err != nil {
		t.Fatal(err)
	}
	s, err := VerifyLive(msg, o)
	if s != nil || asError(t, err).Code != 401 {
		t.Fatalf("a replayed nonce verified: %+v %v", s, err)
	}
	// A different user with the same nonce is a different pair.
	other := signedMsg(t, subj, subj, []byte(payload), testNow, "n9")
	if _, err := VerifyLive(other, o); err != nil {
		t.Fatalf("an unrelated nonce was refused: %v", err)
	}
}

// Stored verification: authorship only. Mutation: apply the window in stored mode.
func TestVerifyStored_AuthorshipOnly(t *testing.T) {
	t.Run("a record from years ago verifies", func(t *testing.T) {
		msg := signedMsg(t, subj, subj, []byte(payload), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), "n1")
		s, err := VerifyStored(msg, StoredOptions{})
		if err != nil || s == nil || s.Trust != TrustVerified {
			t.Fatalf("got %+v, %v", s, err)
		}
		if _, err := VerifyLive(msg, liveOpts()); err == nil {
			t.Fatal("the same record verified live")
		}
	})
	t.Run("a record from the future verifies", func(t *testing.T) {
		msg := signedMsg(t, subj, subj, []byte(payload), time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), "n1")
		if s, err := VerifyStored(msg, StoredOptions{}); err != nil || s == nil {
			t.Fatalf("got %+v, %v", s, err)
		}
	})
	t.Run("a redelivered record verifies again", func(t *testing.T) {
		msg := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
		for i := 0; i < 3; i++ {
			if s, err := VerifyStored(msg, StoredOptions{}); err != nil || s == nil || s.Trust != TrustVerified {
				t.Fatalf("delivery %d: %+v, %v", i, s, err)
			}
		}
	})
	t.Run("a bad signature fails", func(t *testing.T) {
		msg := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
		msg.Data = []byte("tampered")
		if s, err := VerifyStored(msg, StoredOptions{}); s != nil || asError(t, err).Code != 401 {
			t.Fatalf("got %+v, %v", s, err)
		}
	})
	t.Run("a sub mismatch fails", func(t *testing.T) {
		msg := signedMsg(t, subj, "agents.prompt.elsewhere.bob.example", []byte(payload), testNow, "n1")
		if s, err := VerifyStored(msg, StoredOptions{}); s != nil || asError(t, err).Code != 401 {
			t.Fatalf("got %+v, %v", s, err)
		}
	})
	t.Run("malformed and unsigned records classify as live", func(t *testing.T) {
		if _, err := VerifyStored(Message{Subject: subj, Header: nats.Header{HeaderName: {"{"}}}, StoredOptions{}); asError(t, err).Code != 400 {
			t.Fatal("want 400")
		}
		if s, err := VerifyStored(Message{Subject: subj}, StoredOptions{}); s != nil || err != nil {
			t.Fatal("want absent")
		}
	})
}

// The inserted account token (SAP §13.7.1 (b)). Mutation: ignore it.
//
// catalog: SAP-13.7.1-03 SAP-13.7.1-05 SAP-13.7.1-06 SAP-13.7.1-07
func TestVerify_AccountTokenPosition(t *testing.T) {
	const bare, stamped = "svc.prompt", "svc.ACME.prompt"
	cases := []struct {
		name     string
		sign     string // what the caller signs
		arrival  string
		position int
		ok       bool
	}{
		{"sign the token-bearing subject, arrives as is", stamped, stamped, 2, true},
		{"sign the local name, server inserts the token", bare, stamped, 2, true},
		{"the token is another account's", stamped, "svc.EVIL.prompt", 2, false},
		{"the token is another account's, signed bare", bare, "svc.EVIL.prompt", 2, false},
		{"the position is beyond the subject", bare, stamped, 9, false},
		{"sub is neither the arrival nor the stripped subject", "svc.other", stamped, 2, false},
		{"position 1 (the token is the first one)", "ACME.svc.prompt", "ACME.svc.prompt", 1, true},
		{"no position configured: the bare name is not the arrival subject", bare, stamped, 0, false},
		{"no position configured: equal subjects", stamped, stamped, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := signedMsg(t, tc.sign, tc.arrival, []byte(payload), testNow, "n1")
			r := SubjectRules{AccountTokenPosition: tc.position}
			o := liveOpts()
			o.SubjectRules = r
			_, lerr := VerifyLive(msg, o)
			_, serr := VerifyStored(msg, StoredOptions{SubjectRules: r})
			for mode, err := range map[string]error{"live": lerr, "stored": serr} {
				if (err == nil) != tc.ok {
					t.Errorf("%s: ok=%v, err=%v", mode, tc.ok, err)
				} else if err != nil && asError(t, err).Code != 401 {
					t.Errorf("%s: want 401, got %v", mode, err)
				}
			}
		})
	}
	t.Run("a negative position is a programming error", func(t *testing.T) {
		msg := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
		o := liveOpts()
		o.AccountTokenPosition = -1
		if _, err := VerifyLive(msg, o); err == nil {
			t.Fatal("accepted")
		}
	})
}

// SAP §13.7.1 (c): an equality with a subject the receiver computes.
//
// catalog: SAP-13.7.1-04 SAP-13.7.1-05
func TestVerify_RenamedSubject(t *testing.T) {
	const hbPublished = "agents.hb.demo.alice.example"
	const hbArrival = "local.agents.hb.demo.alice.example"
	rename := func(arrival string) (string, bool) {
		if rest, ok := strings.CutPrefix(arrival, "local."); ok {
			return rest, true
		}
		return "", false
	}
	o := liveOpts()
	o.RenamedSubject = rename

	ok := signedMsg(t, hbPublished, hbArrival, []byte(payload), testNow, "n1")
	if _, err := VerifyLive(ok, o); err != nil {
		t.Fatalf("the renamed heartbeat did not verify: %v", err)
	}
	if _, err := VerifyLive(ok, liveOpts()); err == nil {
		t.Fatal("without the mapping the renamed subject verified")
	}
	// A signature over a prompt subject of the same tail shape is refused.
	other := signedMsg(t, "agents.prompt.demo.alice.example", hbArrival, []byte(payload), testNow, "n2")
	if _, err := VerifyLive(other, o); err == nil {
		t.Fatal("a signature over another subject of the same shape verified")
	}
}

// catalog: SAP-13.1-02
func TestVerifyLive_OperatorAttested(t *testing.T) {
	stamp := func(v string) Message {
		m := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
		m.Header[RequestInfoHeader] = []string{v}
		return m
	}
	o := liveOpts()
	o.OperatorAttested = true
	cases := map[string]struct {
		msg      Message
		ok       bool
		attested bool
	}{
		"acc agrees":                   {stamp(`{"acc":"ACME","rtt":5}`), true, true},
		"acc and user agree":           {stamp(`{"acc":"ACME","user":"` + alicePub + `"}`), true, true},
		"acc disagrees":                {stamp(`{"acc":"OTHER"}`), false, false},
		"user disagrees":               {stamp(`{"acc":"ACME","user":"` + bobPub + `"}`), false, false},
		"stamp not JSON":               {stamp(`nope`), false, false},
		"acc is a number":              {stamp(`{"acc":5}`), false, false},
		"no stamp, nothing to compare": {signedMsg(t, subj, subj, []byte(payload), testNow, "n1"), true, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			o := o
			o.Nonces = NewNonceSet(NonceSetOptions{Now: fixedNow}) // every case signs with nonce n1
			s, err := VerifyLive(tc.msg, o)
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v err=%v", tc.ok, err)
			}
			if err == nil && s.AccountAttested != tc.attested {
				t.Fatalf("AccountAttested=%v", s.AccountAttested)
			}
		})
	}
	t.Run("off by default the stamp is never read", func(t *testing.T) {
		s, err := VerifyLive(stamp(`{"acc":"OTHER"}`), liveOpts())
		if err != nil || s.AccountAttested {
			t.Fatalf("got %+v, %v", s, err)
		}
	})
	t.Run("an inserted token attests the account", func(t *testing.T) {
		msg := signedMsg(t, "svc.prompt", "svc.ACME.prompt", []byte(payload), testNow, "n3")
		oo := o
		oo.AccountTokenPosition = 2
		s, err := VerifyLive(msg, oo)
		if err != nil || !s.AccountAttested {
			t.Fatalf("got %+v, %v", s, err)
		}
	})
}

func TestParseHeader_RoundTripAndShape(t *testing.T) {
	msg := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
	value := msg.Header[HeaderName][0]
	h, err := ParseHeader(value)
	if err != nil || h == nil {
		t.Fatal(err)
	}
	if h.Encode() != value {
		t.Errorf("round trip\n got %s\nwant %s", h.Encode(), value)
	}
	for _, field := range []string{"v", "account", "user", "sub", "ts", "nonce", "sig"} {
		if !strings.Contains(value, `"`+field+`"`) {
			t.Errorf("missing %s", field)
		}
	}
	if strings.ContainsAny(value, "\r\n ") {
		t.Errorf("not compact: %q", value)
	}
	if strings.Contains(h.String(), h.Sig) || strings.Contains(h.String(), h.Nonce) {
		t.Error("String leaks the signature or nonce")
	}
	// Unknown fields are ignored.
	extra := strings.Replace(value, `{"v":1,`, `{"v":1,"future":{"x":[1]},`, 1)
	if h2, err := ParseHeader(extra); err != nil || h2 == nil || h2.Sig != h.Sig {
		t.Errorf("unknown field: %v", err)
	}
	// v as 1.0 is the number 1.
	if h3, err := ParseHeader(strings.Replace(value, `"v":1`, `"v":1.0`, 1)); err != nil || h3 == nil {
		t.Errorf("v:1.0: %v", err)
	}
}
