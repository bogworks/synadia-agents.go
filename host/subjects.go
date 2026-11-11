// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	agents "github.com/bogworks/synadia-agents.go"
)

// ErrReplySubject is the error of the default reply-subject policy and the
// sentinel a custom [ReplySubjectPolicy] should wrap. A prompt or status
// request whose reply subject the policy refuses is dropped unanswered
// (SDK policy, not SAP).
var ErrReplySubject = errors.New("host: reply subject refused")

// ReplySubjectPolicy decides whether the host may publish its replies to a
// subject a caller supplied. See [WithReplySubjectPolicy].
type ReplySubjectPolicy = func(subject string) error

// DefaultReplySubjectPolicy is the host-independent part of the policy a host
// uses unless [WithReplySubjectPolicy] says otherwise. It refuses a reply
// subject that is not a valid publish subject (empty, an empty token,
// whitespace, or a wildcard token), that starts with "$" (the $SYS, $JS, $SRV
// and $KV system namespaces), or that is shaped like a protocol subject: any
// "agents.prompt.", "agents.status." or "agents.hb." subject, to which a reply
// would be taken for a prompt, a status request or a heartbeat (a protocol
// loop or a forged heartbeat). Other subjects under "agents." and ordinary
// inbox prefixes are accepted, since SAP lets the caller choose its reply
// subject and delegates authorization to NATS (SAP §2, §6). A host's default
// also refuses its own instance's subjects. The error matches [ErrReplySubject].
// This is SDK policy.
func DefaultReplySubjectPolicy(subject string) error {
	if err := agents.ValidatePublishSubject(subject); err != nil {
		return fmt.Errorf("%w: %w", ErrReplySubject, err)
	}
	first, rest, _ := strings.Cut(subject, ".")
	second, _, _ := strings.Cut(rest, ".")
	switch {
	case strings.HasPrefix(first, "$"):
		return fmt.Errorf("%w: system namespace", ErrReplySubject)
	case first == agents.SubjectRoot && slices.Contains(protocolVerbTokens, second):
		return fmt.Errorf("%w: protocol subject", ErrReplySubject)
	}
	return nil
}

// protocolVerbTokens are the second tokens of the subjects a reply must not use.
var protocolVerbTokens = []string{"prompt", "status", "hb"}

// defaultPolicyFor is the default policy of a host: [DefaultReplySubjectPolicy]
// plus a refusal of the instance's own prompt, status, heartbeat and
// attachments subjects, and of any subject an extra endpoint's subject could
// match (the endpoint subject may hold wildcards), so a caller cannot make the
// host publish its replies into one of its own handlers.
func defaultPolicyFor(s agents.AgentSubject, eps []Endpoint) ReplySubjectPolicy {
	own := []string{s.Prompt(), s.Status(), s.Heartbeat(), s.Attachments()}
	extra := make([]string, 0, len(eps))
	for _, e := range eps {
		extra = append(extra, e.Subject)
	}
	return func(subject string) error {
		if err := DefaultReplySubjectPolicy(subject); err != nil {
			return err
		}
		if slices.Contains(own, subject) {
			return fmt.Errorf("%w: this instance's own subject", ErrReplySubject)
		}
		for _, e := range extra {
			if subjectsOverlap(subject, e) {
				return fmt.Errorf("%w: overlaps an extra endpoint's subject", ErrReplySubject)
			}
		}
		return nil
	}
}

// subjectsOverlap reports whether some subject could match both patterns, each
// a NATS subject that may hold "*" tokens and a final ">" token.
func subjectsOverlap(a, b string) bool {
	return overlap(strings.Split(a, "."), strings.Split(b, "."))
}

func overlap(a, b []string) bool {
	for {
		switch {
		case len(a) == 0 || len(b) == 0:
			return len(a) == 0 && len(b) == 0
		case a[0] == ">":
			// ">" matches one or more tokens.
			return len(b) >= 1
		case b[0] == ">":
			return len(a) >= 1
		case a[0] != "*" && b[0] != "*" && a[0] != b[0]:
			return false
		}
		a, b = a[1:], b[1:]
	}
}
