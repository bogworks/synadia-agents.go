// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
)

// Throwaway keys of the spec's own examples (SAP §13.3.3).
const (
	catSpecAccountKey = "AABYLMBR6Q2CDXTLGRQCFA2GP76BGCDF7NZF2OVHH4RQ7L3Y3TZWJDRL"
	catSpecUser       = "UAWW24XPLGOX3R3JF4OZEZZ6RUXMB55DSWJCEFFSUDFBCKJD4MSCMQYI"
)

// catBreakCRC returns key with its last character changed, which keeps the
// shape and breaks the CRC-16.
func catBreakCRC(t *testing.T, key string, valid func(string) bool) string {
	t.Helper()
	last := key[len(key)-1]
	repl := byte('A')
	if last == 'A' {
		repl = 'B'
	}
	bad := key[:len(key)-1] + string(repl)
	if valid(bad) {
		t.Fatalf("%s still passes the NKEY check after changing its last character", bad)
	}
	return bad
}

// TestAgentIDForm_BuildsAccountDotUser asserts that an agent ID carried as text
// is account, one dot and user, for every account form, and that the text
// parses back to the same halves.
//
// catalog: SAP-13.3-01
func TestAgentIDForm_BuildsAccountDotUser(t *testing.T) {
	for _, account := range []string{catSpecAccountKey, "ACME", "$G", "a-b_c9"} {
		for _, user := range []string{alicePub, catSpecUser} {
			id, err := NewAgentID(account, user)
			if err != nil {
				t.Fatalf("%s: %v", account, err)
			}
			if want := account + "." + user; string(id) != want || id.String() != want {
				t.Errorf("NewAgentID(%q) = %q, want %q", account, id, want)
			}
			if got, err := ParseAgentID(account + "." + user); err != nil || got != id || got.Account() != account || got.User() != user {
				t.Errorf("ParseAgentID round trip: %q, %v", got, err)
			}
			h := SenderHeader{V: HeaderVersion, Account: account, User: user}
			if got, err := h.ID(); err != nil || got != id {
				t.Errorf("SenderHeader.ID: %q, %v", got, err)
			}
		}
	}
}

// TestAgentIDForm_NeverChangesCase asserts that building or parsing an agent ID
// keeps the case it was given, and that a case-changed NKEY is refused rather
// than folded back.
//
// catalog: SAP-13.3-02
func TestAgentIDForm_NeverChangesCase(t *testing.T) {
	id, err := NewAgentID("AcMe", alicePub)
	if err != nil {
		t.Fatal(err)
	}
	if id.Account() != "AcMe" || id.User() != alicePub || string(id) != "AcMe."+alicePub {
		t.Fatalf("case was changed: %q", id)
	}
	lowered := strings.ToLower(catSpecAccountKey + "." + catSpecUser)
	if got, err := ParseAgentID(lowered); err == nil || got != "" {
		t.Fatalf("a lower-cased agent ID parsed: %q", got)
	}
	if _, err := ParseAgentID(catSpecAccountKey + "." + strings.ToLower(catSpecUser)); !errors.Is(err, ErrInvalidAgentID) {
		t.Fatalf("a lower-cased user parsed: %v", err)
	}
	if _, err := NewAgentID(catSpecAccountKey, strings.ToLower(catSpecUser)); !errors.Is(err, ErrInvalidAgentID) {
		t.Fatalf("a lower-cased user was accepted: %v", err)
	}
	up, err := NewAgentID("ACME", alicePub)
	if err != nil {
		t.Fatal(err)
	}
	low, err := NewAgentID("acme", alicePub)
	if err != nil {
		t.Fatal(err)
	}
	if up == low {
		t.Fatal("two accounts that differ only in case produced one agent ID")
	}
}

// TestAgentIDForm_RefusesWhitespaceQuotesAndOtherSeparators asserts that the
// canonical text has no whitespace, no quotes and no separator but one dot.
//
// catalog: SAP-13.3-03
func TestAgentIDForm_RefusesWhitespaceQuotesAndOtherSeparators(t *testing.T) {
	base := "ACME." + alicePub
	if _, err := ParseAgentID(base); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"leading space":       " " + base,
		"trailing space":      base + " ",
		"trailing newline":    base + "\n",
		"leading tab":         "\t" + base,
		"space around dot":    "ACME . " + alicePub,
		"space in the middle": "AC ME." + alicePub,
		"double quotes":       `"` + base + `"`,
		"single quotes":       "'" + base + "'",
		"quote in account":    `AC"ME.` + alicePub,
		"colon":               "ACME:" + alicePub,
		"slash":               "ACME/" + alicePub,
		"hyphen":              "ACME-" + alicePub,
		"underscore":          "ACME_" + alicePub,
		"comma":               "ACME," + alicePub,
		"two dots":            "ACME.." + alicePub,
		"three tokens":        base + ".extra",
	}
	for name, text := range bad {
		if got, err := ParseAgentID(text); !errors.Is(err, ErrInvalidAgentID) || got != "" {
			t.Errorf("%s: %q parsed to %q, %v", name, text, got, err)
		}
	}
	for _, half := range [][2]string{{"AC ME", alicePub}, {`AC"ME`, alicePub}, {"ACME", " " + alicePub}, {"ACME", alicePub + "\n"}, {"ACME.", alicePub}} {
		if _, err := NewAgentID(half[0], half[1]); !errors.Is(err, ErrInvalidAgentID) {
			t.Errorf("NewAgentID(%q, %q): %v", half[0], half[1], err)
		}
	}
}

// TestAgentIDForm_EqualityIsByteEquality asserts that two agent IDs are equal
// exactly when their text is byte-equal: no case folding, no trimming, and the
// value works as a map key.
//
// catalog: SAP-13.3-04
func TestAgentIDForm_EqualityIsByteEquality(t *testing.T) {
	a, err := NewAgentID("ACME", alicePub)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseAgentID("ACME." + alicePub)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("the same text gave unequal IDs: %q %q", a, b)
	}
	others := []struct {
		name          string
		account, user string
	}{
		{"account differs in case", "Acme", alicePub},
		{"other account", "APP", alicePub},
		{"other user", "ACME", bobPub},
		{"global account", "$G", alicePub},
	}
	set := map[AgentID]string{a: "a"}
	for _, o := range others {
		id, err := NewAgentID(o.account, o.user)
		if err != nil {
			t.Fatal(err)
		}
		if id == a {
			t.Errorf("%s: unequal text compared equal", o.name)
		}
		set[id] = o.name
	}
	if len(set) != 1+len(others) {
		t.Errorf("map keys collapsed: %d entries", len(set))
	}
	if set[b] != "a" {
		t.Error("an equal ID did not find its map entry")
	}
}

// TestAgentIDShape_AcceptsExactlyTheRegex asserts that the parser accepts the
// strings of the SAP §13.3 regular expression that also pass the NKEY check,
// and no others.
//
// catalog: SAP-13.3-05
func TestAgentIDShape_AcceptsExactlyTheRegex(t *testing.T) {
	spec := regexp.MustCompile(`^(A[A-Z2-7]{55}|[A-Za-z0-9_-]+|\$G)\.U[A-Z2-7]{55}$`)
	u := catSpecUser
	badUser := catBreakCRC(t, u, nkeys.IsValidPublicUserKey)
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"nkey account", catSpecAccountKey + "." + u, true},
		{"named account", "ACME." + u, true},
		{"lower-case named account", "acme." + u, true},
		{"global account", "$G." + u, true},
		{"one-character account", "a." + u, true},
		{"tokens swapped", u + "." + catSpecAccountKey, false},
		{"colon separator", catSpecAccountKey + ":" + u, false},
		{"lower-cased", strings.ToLower(catSpecAccountKey + "." + u), false},
		{"user truncated", catSpecAccountKey + "." + u[:55], false},
		{"user too long", catSpecAccountKey + "." + u + "A", false},
		{"three tokens", catSpecAccountKey + "." + u + ".extra", false},
		{"user alone", u, false},
		{"account with a space", "acme corp." + u, false},
		{"account with a star", "ac*me." + u, false},
		{"account with a greater-than", "acme>." + u, false},
		{"dollar name that is not $G", "$H." + u, false},
		{"$G with a suffix", "$GA." + u, false},
		{"empty account", "." + u, false},
		{"empty user", "ACME.", false},
		{"empty string", "", false},
		{"only a dot", ".", false},
		{"user with a bad CRC", "ACME." + badUser, false},
		{"user with an invalid base32 digit", "ACME." + u[:20] + "1" + u[21:], false},
		{"user with the account prefix", "ACME.A" + u[1:], false},
		{"account NKEY with a bad CRC", catBreakCRC(t, catSpecAccountKey, nkeys.IsValidPublicAccountKey) + "." + u, false},
	}
	for _, tc := range cases {
		id, err := ParseAgentID(tc.text)
		if (err == nil) != tc.want {
			t.Errorf("%s: ParseAgentID(%q) error %v, want accept=%v", tc.name, tc.text, err, tc.want)
			continue
		}
		if err != nil {
			if !errors.Is(err, ErrInvalidAgentID) || id != "" {
				t.Errorf("%s: %q, %v", tc.name, id, err)
			}
			continue
		}
		if string(id) != tc.text {
			t.Errorf("%s: parsed %q from %q", tc.name, id, tc.text)
		}
		// An accepted string is one the regular expression matches.
		if !spec.MatchString(tc.text) {
			t.Errorf("%s: accepted a string the spec expression refuses", tc.name)
		}
	}
	// A string that fails the expression is refused whatever the NKEY check says.
	for _, text := range []string{"ACME.U", "ACME" + "." + u + "\n", "A.B.C"} {
		if spec.MatchString(text) {
			t.Fatalf("test bug: %q matches the expression", text)
		}
		if _, err := ParseAgentID(text); err == nil {
			t.Errorf("%q was accepted", text)
		}
	}
}

// TestAgentIDNKEY_UserAlwaysChecked asserts that the user half must pass the
// NKEY check (prefix byte and CRC-16), not only the shape check.
//
// catalog: SAP-13.3-06
func TestAgentIDNKEY_UserAlwaysChecked(t *testing.T) {
	bad := catBreakCRC(t, alicePub, nkeys.IsValidPublicUserKey)
	if !userKeyShape.MatchString(bad) {
		t.Fatal("test bug: the broken key lost its shape")
	}
	for _, account := range []string{"ACME", "$G", catSpecAccountKey} {
		if _, err := NewAgentID(account, bad); !errors.Is(err, ErrInvalidAgentID) {
			t.Errorf("NewAgentID(%q, bad CRC user): %v", account, err)
		}
		if _, err := ParseAgentID(account + "." + bad); !errors.Is(err, ErrInvalidAgentID) {
			t.Errorf("ParseAgentID(%q, bad CRC user): %v", account, err)
		}
	}
	// A well-formed user that is not a user key at all.
	if _, err := ParseAgentID("ACME.U" + strings.Repeat("A", 55)); !errors.Is(err, ErrInvalidAgentID) {
		t.Errorf("an all-A user was accepted: %v", err)
	}
	if _, err := ParseAgentID("ACME." + alicePub); err != nil {
		t.Fatalf("the control user is refused: %v", err)
	}
}

// TestAgentIDNKEY_AccountCheckedOnlyWhenAccountKeyShaped asserts that the NKEY
// check runs on the account exactly when it starts with A and is 56 characters
// long: such an account with a bad CRC is refused, and the same shapes one
// character off, or starting with another letter, are names and are accepted.
//
// catalog: SAP-13.3-07
func TestAgentIDNKEY_AccountCheckedOnlyWhenAccountKeyShaped(t *testing.T) {
	badAccount := catBreakCRC(t, catSpecAccountKey, nkeys.IsValidPublicAccountKey)
	for _, account := range []string{badAccount, "A" + strings.Repeat("A", 55)} {
		if len(account) != 56 || account[0] != 'A' {
			t.Fatalf("test bug: %q", account)
		}
		if _, err := NewAgentID(account, alicePub); !errors.Is(err, ErrInvalidAgentID) {
			t.Errorf("NewAgentID(%q): %v", account, err)
		}
		if _, err := ParseAgentID(account + "." + alicePub); !errors.Is(err, ErrInvalidAgentID) {
			t.Errorf("ParseAgentID(%q): %v", account, err)
		}
	}
	names := map[string]string{
		"56 characters starting with B":  "B" + strings.Repeat("A", 55),
		"56 characters starting with U":  "U" + strings.Repeat("A", 55),
		"57 characters starting with A":  "A" + strings.Repeat("A", 56),
		"55 characters starting with A":  "A" + strings.Repeat("A", 54),
		"a lower-case 56 character name": "a" + strings.Repeat("a", 55),
	}
	for name, account := range names {
		id, err := NewAgentID(account, alicePub)
		if err != nil || id.Account() != account {
			t.Errorf("%s: NewAgentID refused a name: %v", name, err)
		}
		if _, err := ParseAgentID(account + "." + alicePub); err != nil {
			t.Errorf("%s: ParseAgentID refused a name: %v", name, err)
		}
	}
	if _, err := NewAgentID(catSpecAccountKey, alicePub); err != nil {
		t.Fatalf("the control account key is refused: %v", err)
	}
}

// TestAgentIDForm_NoEmptyTokenAndNoZeroID asserts that neither half may be
// empty and that the zero value is not an agent ID.
//
// catalog: SAP-13.3-08
func TestAgentIDForm_NoEmptyTokenAndNoZeroID(t *testing.T) {
	for _, tc := range []struct{ account, user string }{{"", alicePub}, {"ACME", ""}, {"", ""}} {
		if id, err := NewAgentID(tc.account, tc.user); !errors.Is(err, ErrInvalidAgentID) || id != "" {
			t.Errorf("NewAgentID(%q, %q) = %q, %v", tc.account, tc.user, id, err)
		}
	}
	for _, text := range []string{"", ".", "ACME.", "." + alicePub} {
		if id, err := ParseAgentID(text); !errors.Is(err, ErrInvalidAgentID) || id != "" {
			t.Errorf("ParseAgentID(%q) = %q, %v", text, id, err)
		}
	}
	var zero AgentID
	if zero.Account() != "" || zero.User() != "" || zero.String() != "" {
		t.Errorf("the zero value has halves: %q %q", zero.Account(), zero.User())
	}
	if _, err := ParseAgentID(zero.String()); err == nil {
		t.Error("the zero agent ID parses")
	}
	if _, err := (SenderHeader{V: HeaderVersion}).ID(); err == nil {
		t.Error("a header with empty account and user yields an ID")
	}
}

// TestAgentIDAccount_NameForm asserts the grammar of an account without an
// NKEY: one token of letters, digits, underscore or hyphen, or the global
// account $G.
//
// catalog: SAP-13.3.1-01
func TestAgentIDAccount_NameForm(t *testing.T) {
	good := []string{"ACME", "acme", "a", "0", "a-b_c-9", "_", "-", "$G", strings.Repeat("x", 200)}
	for _, account := range good {
		if _, err := NewAgentID(account, alicePub); err != nil {
			t.Errorf("NewAgentID(%q): %v", account, err)
		}
		if _, err := ParseAgentID(account + "." + alicePub); err != nil {
			t.Errorf("ParseAgentID(%q): %v", account, err)
		}
	}
	bad := []string{"", "a.b", "a*", "*", "a>", ">", "a b", "a\tb", "a\nb", "$g", "$H", "$", "$GG", "a$", "$G$", "é", "a:b", "a/b", "a@b", `a"b`}
	for _, account := range bad {
		if _, err := NewAgentID(account, alicePub); !errors.Is(err, ErrInvalidAgentID) {
			t.Errorf("NewAgentID(%q): %v", account, err)
		}
		if strings.Contains(account, ".") {
			continue
		}
		if _, err := ParseAgentID(account + "." + alicePub); err == nil {
			t.Errorf("ParseAgentID(%q) accepted", account)
		}
	}
}

// TestAgentIDAccount_UnrepresentableNameGivesNoIdentity asserts that a server
// reporting an account name outside the grammar leaves the connection with no
// identity, never an invented one.
//
// catalog: SAP-13.3.1-02
func TestAgentIDAccount_UnrepresentableNameGivesNoIdentity(t *testing.T) {
	for _, account := range []string{"a.b", "a*", "a>", "acme corp", "a\tb", "a:b", "é"} {
		raw, err := json.Marshal(map[string]any{"data": map[string]any{"user": alicePub, "account": account}})
		if err != nil {
			t.Fatal(err)
		}
		id, err := IdentityFromUserInfoReply(raw)
		if !errors.Is(err, ErrNoIdentity) || id != "" {
			t.Errorf("account %q: %q, %v", account, id, err)
		}
	}
	raw := []byte(`{"data":{"user":"` + alicePub + `","account":"ACME"}}`)
	if id, err := IdentityFromUserInfoReply(raw); err != nil || id != AgentID("ACME."+alicePub) {
		t.Fatalf("the control account: %q, %v", id, err)
	}
}
