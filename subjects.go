// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"strings"
	"unicode"
)

// SubjectRoot is the fixed first token of every protocol subject, reserved for
// the protocol. See SAP §2.
const SubjectRoot = "agents"

// Verb is the endpoint discriminator, the second token of a protocol subject.
// The four verbs are reserved; agents MUST NOT use them for any other
// purpose. See SAP §2.
type Verb string

// The reserved verbs. See SAP §2.
const (
	VerbPrompt      Verb = "prompt"      // required prompt endpoint (§5, §6)
	VerbHeartbeat   Verb = "hb"          // liveness beacon, protocol-fixed (§8)
	VerbStatus      Verb = "status"      // on-demand status request/reply (§8.7)
	VerbAttachments Verb = "attachments" // reserved for a future endpoint (§5.5)
)

// Valid reports whether v is one of the four reserved verbs.
func (v Verb) Valid() bool {
	switch v {
	case VerbPrompt, VerbHeartbeat, VerbStatus, VerbAttachments:
		return true
	}
	return false
}

// Subjects of discovery and wildcard subscription. See SAP §2.4, §4, §8.5.
const (
	// SubjectPingAll is the general discovery subject: every agent instance
	// answers once. See SAP §4.
	SubjectPingAll = "$SRV.PING.agents"

	// SubjectInfoAll is the general info subject: every instance answers with
	// its full service info. See SAP §4.1.
	SubjectInfoAll = "$SRV.INFO.agents"

	// SubjectHeartbeatAll is the wildcard for every heartbeat. The heartbeat
	// subject is the one fixed endpoint subject. Callers SHOULD subscribe to
	// it before the first ping. See SAP §2, §8.5.
	SubjectHeartbeatAll = "agents.hb.*.*.*"

	// SubjectAll matches every protocol subject. See SAP §2.4.
	SubjectAll = "agents.>"
)

// AgentSubject names one agent instance: the owner and instance name, the
// agent identifier and the token used for it on the wire. Build one with
// [NewAgentSubject] or [ParseSubject], the only constructors; both validate
// the tokens, so every non-zero AgentSubject builds valid subjects. The zero
// value is empty: its subject methods return "" and [AgentSubject.Subject]
// fails. See SAP §2.
//
// A host builds its endpoint subjects from it. A caller MUST NOT address a
// prompt or status endpoint from it: it publishes to the subject the
// discovery record reports (SAP §2, §4.3). The heartbeat subject is the one
// that may be built from identity alone.
type AgentSubject struct {
	agent string
	owner string
	name  string
	// token is the 3rd subject token; always set on a non-zero value.
	token string
}

// Agent returns the canonical harness identifier (metadata.agent). When the
// subject was parsed, only the wire token is known, and Agent equals Token.
func (s AgentSubject) Agent() string { return s.agent }

// Owner returns the operator or account (metadata.owner), the 4th token.
func (s AgentSubject) Owner() string { return s.owner }

// Name returns the instance name, the 5th token, which lives only in the
// subject.
func (s AgentSubject) Name() string { return s.name }

// Token returns the 3rd subject token: the agent identifier unless an
// abbreviation is in use ("cc" for "claude-code"). See SAP §2.
func (s AgentSubject) Token() string { return s.token }

// IsZero reports whether s is the empty value, which names no instance.
func (s AgentSubject) IsZero() bool { return s.token == "" }

// SubjectOption customizes [NewAgentSubject].
type SubjectOption func(*subjectConfig)

type subjectConfig struct{ token string }

// WithSubjectToken sets the 3rd subject token apart from the agent identifier,
// for the conventional abbreviations (Appendix C of SAP: "cc", "oc"). Both are
// validated independently. See SAP §2.
func WithSubjectToken(token string) SubjectOption {
	return func(c *subjectConfig) { c.token = token }
}

// NewAgentSubject validates the three identifying tokens against the SAP §2.2
// MUSTs (and no more) and returns the [AgentSubject]. The error is a
// [*TokenError]. SHOULD-level checks are not applied; see [TokenWarnings] and
// [ValidateTokenStrict].
func NewAgentSubject(agent, owner, name string, opts ...SubjectOption) (AgentSubject, error) {
	var cfg subjectConfig
	for _, o := range opts {
		o(&cfg)
	}
	if err := ValidateToken(RoleAgent, agent); err != nil {
		return AgentSubject{}, err
	}
	if err := ValidateToken(RoleOwner, owner); err != nil {
		return AgentSubject{}, err
	}
	if err := ValidateToken(RoleName, name); err != nil {
		return AgentSubject{}, err
	}
	token := cfg.token
	if token == "" {
		token = agent
	} else if token != agent {
		if err := ValidateToken(RoleSubjectToken, token); err != nil {
			return AgentSubject{}, err
		}
	}
	return AgentSubject{agent: agent, owner: owner, name: name, token: token}, nil
}

// subject builds the subject for a verb; "" for the zero value.
func (s AgentSubject) subject(v Verb) string {
	if s.IsZero() {
		return ""
	}
	return SubjectRoot + "." + string(v) + "." + s.token + "." + s.owner + "." + s.name
}

// Subject returns agents.{verb}.{token}.{owner}.{name}. It fails with a
// [*SubjectError] when v is not one of the four reserved verbs or s is the
// zero value. See SAP §2.
func (s AgentSubject) Subject(v Verb) (string, error) {
	if !v.Valid() {
		return "", &SubjectError{Subject: string(v), Reason: "unknown verb"}
	}
	if s.IsZero() {
		return "", &SubjectError{Reason: "zero AgentSubject"}
	}
	return s.subject(v), nil
}

// Prompt returns the default prompt subject, agents.prompt.{agent}.{owner}.{name}.
// See SAP §2.
func (s AgentSubject) Prompt() string { return s.subject(VerbPrompt) }

// Heartbeat returns the heartbeat subject, agents.hb.{agent}.{owner}.{name}.
// See SAP §8.1.
func (s AgentSubject) Heartbeat() string { return s.subject(VerbHeartbeat) }

// Status returns the default status subject,
// agents.status.{agent}.{owner}.{name}. See SAP §8.7.
func (s AgentSubject) Status() string { return s.subject(VerbStatus) }

// Attachments returns the default subject reserved for the future attachments
// endpoint. See SAP §5.5.
func (s AgentSubject) Attachments() string { return s.subject(VerbAttachments) }

// Warnings returns the SAP §2.2 SHOULDs the identifying tokens and the
// longest subject do not meet. It is advisory.
func (s AgentSubject) Warnings() []string {
	var w []string
	for _, t := range []struct{ role, tok string }{
		{RoleAgent, s.agent}, {RoleSubjectToken, s.token}, {RoleOwner, s.owner}, {RoleName, s.name},
	} {
		for _, m := range TokenWarnings(t.tok) {
			w = append(w, t.role+" "+`"`+t.tok+`": `+m)
		}
	}
	return append(w, SubjectWarnings(s.subject(VerbAttachments))...)
}

// ParseSubject parses agents.{verb}.{agent}.{owner}.{name} into its verb and an
// [AgentSubject]. The subject must have exactly five tokens, the root
// "agents", a reserved verb and tokens that pass [ValidateToken]. The agent
// token is the wire token, which may be an abbreviation of metadata.agent
// (SAP §2). Failures are a [*SubjectError].
func ParseSubject(subject string) (Verb, AgentSubject, error) {
	parts := strings.Split(subject, ".")
	if len(parts) != 5 || parts[0] != SubjectRoot {
		return "", AgentSubject{}, &SubjectError{Subject: subject, Reason: "want agents.{verb}.{agent}.{owner}.{name}"}
	}
	v := Verb(parts[1])
	if !v.Valid() {
		return "", AgentSubject{}, &SubjectError{Subject: subject, Reason: "unknown verb " + parts[1]}
	}
	s, err := NewAgentSubject(parts[2], parts[3], parts[4])
	if err != nil {
		return "", AgentSubject{}, &SubjectError{Subject: subject, Reason: err.Error()}
	}
	return v, s, nil
}

// ParseAgentSubject is [ParseSubject] that also requires the given verb.
func ParseAgentSubject(subject string, verb Verb) (AgentSubject, error) {
	v, s, err := ParseSubject(subject)
	if err != nil {
		return AgentSubject{}, err
	}
	if v != verb {
		return AgentSubject{}, &SubjectError{Subject: subject, Reason: "verb is " + string(v) + ", want " + string(verb)}
	}
	return s, nil
}

// IsHeartbeatSubject reports whether subject is agents.hb.{agent}.{owner}.{name}.
// See SAP §8.1.
func IsHeartbeatSubject(subject string) bool {
	parts := strings.Split(subject, ".")
	return len(parts) == 5 && parts[0] == SubjectRoot && parts[1] == string(VerbHeartbeat)
}

// InstanceNameFromSubject returns the instance name, the 5th token, of an
// endpoint subject that follows the default pattern agents.{verb}.{agent}.{owner}.{name}
// with a reserved verb. It returns "" for any other subject, in which case the
// name is opaque to the caller. See SAP §4.3 step 3.
func InstanceNameFromSubject(subject string) string {
	parts := strings.Split(subject, ".")
	if len(parts) != 5 || parts[0] != SubjectRoot || !Verb(parts[1]).Valid() {
		return ""
	}
	return parts[4]
}

// FilterSubject builds a wildcard subject for subscribing to one verb across
// agents. An empty agent, owner or name becomes "*", so
// FilterSubject(VerbPrompt, "", "alice", "") is agents.prompt.*.alice.*. A
// non-empty value is validated like any token. See SAP §2.4.
func FilterSubject(verb Verb, agent, owner, name string) (string, error) {
	if !verb.Valid() {
		return "", &SubjectError{Subject: string(verb), Reason: "unknown verb"}
	}
	toks := [3]string{agent, owner, name}
	roles := [3]string{RoleAgent, RoleOwner, RoleName}
	for i, t := range toks {
		if t == "" {
			toks[i] = "*"
		} else if err := ValidateToken(roles[i], t); err != nil {
			return "", err
		}
	}
	return SubjectRoot + "." + string(verb) + "." + toks[0] + "." + toks[1] + "." + toks[2], nil
}

// InfoSubject returns $SRV.INFO.agents.{instance_id}, the direct lookup of one
// instance. See SAP §4, §4.2.
func InfoSubject(instanceID string) (string, error) {
	if err := ValidateToken(RoleInstanceID, instanceID); err != nil {
		return "", err
	}
	return SubjectInfoAll + "." + instanceID, nil
}

// PingSubject returns $SRV.PING.agents.{instance_id}, the micro service ping
// of one instance. SAP names only the general ping subject (§4); this one is
// the NATS micro convention. See SAP §8.4.
func PingSubject(instanceID string) (string, error) {
	if err := ValidateToken(RoleInstanceID, instanceID); err != nil {
		return "", err
	}
	return SubjectPingAll + "." + instanceID, nil
}

// InstanceIDFromInfoSubject returns the instance id of a
// $SRV.INFO.agents.{instance_id} subject.
func InstanceIDFromInfoSubject(subject string) (string, bool) {
	id, ok := strings.CutPrefix(subject, SubjectInfoAll+".")
	if !ok || ValidateToken(RoleInstanceID, id) != nil {
		return "", false
	}
	return id, true
}

// ValidatePublishSubject checks that s is syntactically fit to publish to: not
// empty, no empty token, no wildcard token, no whitespace or NUL. A caller
// applies it to a subject it receives from a peer, such as a query chunk's
// reply_subject, before publishing there. It does not restrict the subject's
// prefix. See SAP §7.1.
func ValidatePublishSubject(s string) error {
	if s == "" {
		return &SubjectError{Subject: s, Reason: "empty"}
	}
	for _, r := range s {
		if r == 0 || unicode.IsSpace(r) {
			return &SubjectError{Subject: s, Reason: "contains whitespace or NUL"}
		}
	}
	for _, tok := range strings.Split(s, ".") {
		switch tok {
		case "":
			return &SubjectError{Subject: s, Reason: "empty token"}
		case "*", ">":
			return &SubjectError{Subject: s, Reason: "wildcard in a publish subject"}
		}
	}
	return nil
}
