// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"testing"
)

// TestVerifySub_IsComparedNeverMatchedAsAPattern asserts that sub is compared
// with the arrival subject as a string: a wildcard sub that would match the
// arrival subject as a NATS pattern is refused in every configuration.
//
// catalog: SAP-13.7.1-01
func TestVerifySub_IsComparedNeverMatchedAsAPattern(t *testing.T) {
	const arrival = "svc.ACME.prompt"
	rename := func(a string) (string, bool) { return "agents.hb.x", a == arrival }
	configs := map[string]SubjectRules{
		"no rules":          {},
		"token position":    {AccountTokenPosition: 2},
		"renamed import":    {RenamedSubject: rename},
		"position + rename": {AccountTokenPosition: 2, RenamedSubject: rename},
	}
	for name, rules := range configs {
		for _, sub := range []string{"svc.*.prompt", "svc.>", ">", "*.*.*", "svc.ACME.*", "svc.ACME.>", "*"} {
			msg := signedMsg(t, sub, arrival, []byte(payload), testNow, "n1")
			o := liveOpts()
			o.SubjectRules = rules
			if s, err := VerifyLive(msg, o); s != nil || asError(t, err).Code != 401 {
				t.Errorf("%s: sub %q verified against arrival %q: %+v %v", name, sub, arrival, s, err)
			}
			if s, err := VerifyStored(msg, StoredOptions{SubjectRules: rules}); s != nil || asError(t, err).Code != 401 {
				t.Errorf("%s (stored): sub %q verified: %+v %v", name, sub, s, err)
			}
		}
	}
	// The control: the equal subject verifies under the same rules.
	ok := signedMsg(t, arrival, arrival, []byte(payload), testNow, "n2")
	if _, err := VerifyLive(ok, liveOpts()); err != nil {
		t.Fatalf("the equal subject was refused: %v", err)
	}
}

// TestVerifySub_AccountTokenMustEqualAccountInEveryForm asserts that, with an
// account_token_position configured, the arrival subject's token at that
// position must equal the header's account whichever form makes sub acceptable:
// (a) sub equals the arrival subject, (b) sub is the arrival subject with the
// token removed, (c) sub is the subject a renamed import maps the arrival
// subject back to.
//
// catalog: SAP-13.7.1-06
func TestVerifySub_AccountTokenMustEqualAccountInEveryForm(t *testing.T) {
	const published = "agents.hb.demo.alice.example"
	rename := func(arrival string) (string, bool) {
		if arrival == "local.ACME.hb" || arrival == "local.EVIL.hb" {
			return published, true
		}
		return "", false
	}
	rules := SubjectRules{AccountTokenPosition: 2, RenamedSubject: rename}
	cases := []struct {
		name    string
		sub     string
		arrival string
		ok      bool
	}{
		{"(a) token is the account", "svc.ACME.prompt", "svc.ACME.prompt", true},
		{"(a) token is another account", "svc.EVIL.prompt", "svc.EVIL.prompt", false},
		{"(b) token is the account", "svc.prompt", "svc.ACME.prompt", true},
		{"(b) token is another account", "svc.prompt", "svc.EVIL.prompt", false},
		{"(c) token is the account", published, "local.ACME.hb", true},
		{"(c) token is another account", published, "local.EVIL.hb", false},
	}
	for i, tc := range cases {
		msg := signedMsg(t, tc.sub, tc.arrival, []byte(payload), testNow, "n"+string(rune('a'+i)))
		o := liveOpts()
		o.SubjectRules = rules
		_, lerr := VerifyLive(msg, o)
		_, serr := VerifyStored(msg, StoredOptions{SubjectRules: rules})
		for mode, err := range map[string]error{"live": lerr, "stored": serr} {
			if (err == nil) != tc.ok {
				t.Errorf("%s, %s: ok=%v, err=%v", tc.name, mode, tc.ok, err)
			} else if err != nil && asError(t, err).Code != 401 {
				t.Errorf("%s, %s: want 401, got %v", tc.name, mode, err)
			}
		}
	}
}

// TestVerifyRequestInfo_IsOnlyAClaim asserts that a Nats-Request-Info stamp is
// never proof: by itself it makes no sender, beside an unsigned header it stays
// a claim with no agent ID, and beside a signed header it changes neither who
// the sender is nor whether the signature verifies.
//
// catalog: SAP-13.1-02
func TestVerifyRequestInfo_IsOnlyAClaim(t *testing.T) {
	stampAlice := `{"acc":"` + testAcct + `","user":"` + alicePub + `","rtt":5}`
	for _, attested := range []bool{false, true} {
		o := liveOpts()
		o.OperatorAttested = attested

		// A stamp and no Agent-Sender header: the message has no sender.
		bare := Message{Subject: subj, Data: []byte(payload)}
		bare.Header = map[string][]string{RequestInfoHeader: {stampAlice}}
		if s, err := VerifyLive(bare, o); s != nil || err != nil || TrustOf(s) != TrustAbsent {
			t.Errorf("attested=%v: a stamp alone gave %+v, %v", attested, s, err)
		}

		// A stamp beside an unsigned claim: still only a claim, with no ID.
		claim, err := ClaimHeader(newID(t, testAcct, alicePub), "alice")
		if err != nil {
			t.Fatal(err)
		}
		claimed := Message{Subject: subj, Data: []byte(payload), Header: map[string][]string{
			HeaderName: {claim.Encode()}, RequestInfoHeader: {stampAlice},
		}}
		s, err := VerifyLive(claimed, o)
		if err != nil || TrustOf(s) != TrustClaimed || s.ID != "" {
			t.Errorf("attested=%v: claim beside a stamp: %+v, %v", attested, s, err)
		}
	}

	// A signed header by alice beside a stamp that names bob: the verified
	// identity is the signer's, and the stamp is not used unless the operator
	// attests it.
	msg := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
	msg.Header[RequestInfoHeader] = []string{`{"acc":"OTHER","user":"` + bobPub + `"}`}
	s, err := VerifyLive(msg, liveOpts())
	if err != nil || TrustOf(s) != TrustVerified || s.ID != newID(t, testAcct, alicePub) || s.AccountAttested {
		t.Errorf("a stamp changed a verified sender: %+v, %v", s, err)
	}
	// The stamp cannot rescue a bad signature either.
	bad := signedMsg(t, subj, subj, []byte(payload), testNow, "n2")
	bad.Data = []byte(payload + " ")
	bad.Header[RequestInfoHeader] = []string{`{"acc":"` + testAcct + `","user":"` + alicePub + `"}`}
	if s, err := VerifyLive(bad, liveOpts()); s != nil || asError(t, err).Code != 401 {
		t.Errorf("a stamp rescued a tampered request: %+v, %v", s, err)
	}
}
