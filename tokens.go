// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"fmt"
	"strings"
)

// Token roles, used in error messages. See SAP §2.
const (
	RoleAgent        = "agent"
	RoleOwner        = "owner"
	RoleName         = "name"
	RoleSubjectToken = "subject token"
	RoleInstanceID   = "instance id"
)

const (
	// MaxRecommendedTokenLen is the longest token SAP recommends. See SAP §2.2.
	MaxRecommendedTokenLen = 63
	// MaxRecommendedSubjectLen is the bound a fully qualified subject SHOULD
	// stay under. See SAP §2.2.
	MaxRecommendedSubjectLen = 256
)

// ValidateToken enforces the MUSTs SAP puts on a subject token and nothing
// stricter: the token is not empty, does not begin with "$" (SAP §2.2), and
// obeys NATS subject naming: no space, tab, CR or LF, no ".", no NUL, and not
// the whole-token wildcards "*" or ">".
// SAP's SHOULDs are not checked here; see [TokenWarnings] and
// [ValidateTokenStrict]. The error is a [*TokenError] matching
// [ErrInvalidToken] and [ErrLocal]. See SAP §2.2.
//
// SAP §2.2 says only "MUST conform to NATS subject naming rules" and "MUST NOT
// begin with $" (SAP:L96-L98). NATS forbids space, tab, CR and LF, and treats
// "*" and ">" as wildcards only as a whole token, so "a*" is a legal literal
// token. The official SDKs reject more (TypeScript FORBIDDEN_CHARS =
// /[.*>\s\0]/ in client-sdk/typescript/src/subjects.ts; Python restricts
// agent and owner by regular expression); SAP does not say so, and this SDK
// does not tighten a SHOULD into a MUST (SDK policy).
func ValidateToken(role, token string) error {
	switch {
	case token == "":
		return &TokenError{Role: role, Token: token, Reason: "must not be empty"}
	case token[0] == '$':
		return &TokenError{Role: role, Token: token, Reason: "must not begin with '$'"}
	case token == "*" || token == ">":
		return &TokenError{Role: role, Token: token, Reason: "must not be the wildcard token '*' or '>'"}
	}
	if strings.ContainsAny(token, ". \t\r\n\x00") {
		return &TokenError{Role: role, Token: token, Reason: "must not contain '.', space, tab, CR, LF or NUL"}
	}
	return nil
}

// TokenWarnings returns the SAP §2.2 SHOULDs the token does not meet: the
// character set a-z 0-9 - _, and a length of 1 to 63 characters. It returns
// nil for a token within them. It does not check the MUSTs (see
// [ValidateToken]); the warnings are advisory and a program may ignore them.
func TokenWarnings(token string) []string {
	var w []string
	if n := len([]rune(token)); n < 1 || n > MaxRecommendedTokenLen {
		w = append(w, fmt.Sprintf("length %d is outside the recommended 1-%d characters", n, MaxRecommendedTokenLen))
	}
	for _, r := range token {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			w = append(w, "characters outside the recommended set a-z 0-9 - _")
			break
		}
	}
	return w
}

// IsRecommendedToken reports whether token is within SAP's SHOULD-level form
// (a-z 0-9 - _, 1 to 63 characters). See SAP §2.2.
func IsRecommendedToken(token string) bool { return len(TokenWarnings(token)) == 0 }

// ValidateTokenStrict is the opt-in strict mode: the MUSTs of [ValidateToken]
// and then the SHOULDs of [TokenWarnings]. A SHOULD failure is a [*TokenError]
// with Recommended set, matching [ErrTokenNotRecommended] and [ErrLocal]. See
// SAP §2.2.
func ValidateTokenStrict(role, token string) error {
	if err := ValidateToken(role, token); err != nil {
		return err
	}
	if w := TokenWarnings(token); len(w) > 0 {
		return &TokenError{Role: role, Token: token, Reason: strings.Join(w, "; "), Recommended: true}
	}
	return nil
}

// SubjectWarnings returns the SAP §2.2 SHOULD that a fully qualified subject
// stay under 256 characters, when it does not.
func SubjectWarnings(subject string) []string {
	if len(subject) >= MaxRecommendedSubjectLen {
		return []string{fmt.Sprintf("subject length %d should stay under %d characters", len(subject), MaxRecommendedSubjectLen)}
	}
	return nil
}
