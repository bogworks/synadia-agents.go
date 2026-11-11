// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"errors"
	"strings"
	"testing"
)

// REQ-009, SAP:L96-L99 (§2.2): the MUSTs are NATS subject rules and no leading "$".
//
// catalog: SAP-2.2-01 SAP-2.2-02
func TestToken_MustRules(t *testing.T) {
	cases := []struct {
		tok string
		ok  bool
	}{
		{"claude-code", true},
		{"a", true},
		{"UPPER", true},                  // SHOULD only: not a MUST failure
		{"under_score.", false},          // dot
		{"caf" + "é", true},              // non-ASCII is a SHOULD, not a MUST
		{strings.Repeat("a", 200), true}, // length is a SHOULD
		{"", false},
		{"$SYS", false},
		{"a.b", false},
		{"a*", true}, // NATS: a wildcard only as a whole token
		{"a>", true},
		{"*", false},
		{">", false},
		{"a\rb", false},
		{"a\u00a0b", true}, // NBSP: not a NATS-forbidden character
		{"a b", false},
		{"a\tb", false},
		{"a\nb", false},
		{"a\x00b", false},
	}
	for _, c := range cases {
		err := ValidateToken(RoleName, c.tok)
		if (err == nil) != c.ok {
			t.Errorf("ValidateToken(%q) = %v, want ok=%v", c.tok, err, c.ok)
		}
		if err != nil {
			var te *TokenError
			if !errors.As(err, &te) || !errors.Is(err, ErrInvalidToken) || !errors.Is(err, ErrLocal) || errors.Is(err, ErrTokenNotRecommended) {
				t.Errorf("ValidateToken(%q) error shape wrong: %v", c.tok, err)
			}
		}
	}
}

// REQ-009, SAP:L99-L100: SHOULDs are advice, and strict mode is opt-in.
//
// catalog: SAP-2.2-03 SAP-2.2-04
func TestToken_ShouldRulesAreWarningsOrStrict(t *testing.T) {
	good := []string{"cc", "claude-code", "a_b-9", strings.Repeat("a", 63)}
	for _, g := range good {
		if w := TokenWarnings(g); w != nil || !IsRecommendedToken(g) {
			t.Errorf("%q: unexpected warnings %v", g, w)
		}
		if err := ValidateTokenStrict(RoleOwner, g); err != nil {
			t.Errorf("strict %q: %v", g, err)
		}
	}
	bad := []string{"UPPER", "café", strings.Repeat("a", 64), "a+b"}
	for _, b := range bad {
		if len(TokenWarnings(b)) == 0 || IsRecommendedToken(b) {
			t.Errorf("%q: expected warnings", b)
		}
		if err := ValidateToken(RoleOwner, b); err != nil {
			t.Errorf("non-strict %q must pass: %v", b, err)
		}
		err := ValidateTokenStrict(RoleOwner, b)
		if err == nil || !errors.Is(err, ErrTokenNotRecommended) || errors.Is(err, ErrInvalidToken) || !errors.Is(err, ErrLocal) {
			t.Errorf("strict %q: %v", b, err)
		}
	}
	// A MUST failure stays a MUST failure in strict mode.
	if err := ValidateTokenStrict(RoleOwner, "$x"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("strict $x: %v", err)
	}
	if got := TokenWarnings(""); len(got) == 0 {
		t.Error("empty token has no length warning")
	}
}

// SAP:L100 (§2.2): a fully qualified subject SHOULD stay under 256 characters.
//
// catalog: SAP-2.2-05
func TestToken_SubjectLengthWarning(t *testing.T) {
	if w := SubjectWarnings(strings.Repeat("a", 255)); w != nil {
		t.Errorf("255: %v", w)
	}
	if w := SubjectWarnings(strings.Repeat("a", 256)); len(w) != 1 {
		t.Errorf("256: %v", w)
	}
}
