// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/nats-io/nkeys"
)

// ErrInvalidAgentID is returned (wrapped) when an agent ID, or one of its two
// halves, does not meet SAP §13.3. Test with [errors.Is].
var ErrInvalidAgentID = errors.New("identity: invalid agent ID")

// agentIDRegexp is the shape check of SAP §13.3. The NKEY check is separate.
var agentIDRegexp = regexp.MustCompile(`^(A[A-Z2-7]{55}|[A-Za-z0-9_-]+|\$G)\.U[A-Z2-7]{55}$`)

var (
	userKeyShape    = regexp.MustCompile(`^U[A-Z2-7]{55}$`)
	accountNameForm = regexp.MustCompile(`^([A-Za-z0-9_-]+|\$G)$`)
)

// nkeyPublicLen is the length of a public NKEY (prefix byte, key and CRC
// encoded as base32). See SAP §13.3.
const nkeyPublicLen = 56

// MaxAccountLen is the size allowance for an account token in [MaxHeaderWireBytes],
// in bytes: 56 for an NKEY, a little more for a configured name. It is a sizing
// figure only: agent ID parsing imposes no length cap, because SAP §13.3.1
// accepts any nonempty account name of the stated grammar. A caller whose
// account name is longer sizes its header with [ExpectedHeaderWireBytes]. See
// SAP §13.3.1.
const MaxAccountLen = 64

// AgentID is the canonical text form "{account}.{user}" of a sender's identity.
// Two agent IDs are equal if and only if their text forms are byte-equal, so
// AgentID values compare with == and work as map keys. The zero value is not a
// valid ID: there is no zero agent ID. See SAP §13.2 and §13.3.
type AgentID string

// NewAgentID builds an agent ID from its two halves and validates both: user
// must be a user public NKEY, and account must be an account public NKEY, a
// configured account name matching [A-Za-z0-9_-]+, or "$G". The NKEY check
// (prefix byte and CRC) runs on user, and on account whenever it starts with
// "A" and is 56 characters long. See SAP §13.3.
func NewAgentID(account, user string) (AgentID, error) {
	if err := validateAccount(account); err != nil {
		return "", err
	}
	if err := validateUser(user); err != nil {
		return "", err
	}
	return AgentID(account + "." + user), nil
}

// ParseAgentID parses the canonical text form and nothing else. See SAP
// §13.3 and the parse fixtures of §13.3.3.
func ParseAgentID(text string) (AgentID, error) {
	if text == "" {
		return "", fmt.Errorf("%w: empty string", ErrInvalidAgentID)
	}
	if !agentIDRegexp.MatchString(text) {
		return "", fmt.Errorf("%w: expected {account}.{user} with a U... user public NKEY", ErrInvalidAgentID)
	}
	account, user, _ := strings.Cut(text, ".")
	return NewAgentID(account, user)
}

// Account returns the account half of the ID, or "" for the zero value.
func (id AgentID) Account() string {
	a, _, _ := strings.Cut(string(id), ".")
	return a
}

// User returns the user public NKEY half of the ID, or "" for the zero value.
func (id AgentID) User() string {
	_, u, _ := strings.Cut(string(id), ".")
	return u
}

// String returns the canonical text form.
func (id AgentID) String() string { return string(id) }

func isAccountKeyShaped(account string) bool {
	return len(account) == nkeyPublicLen && strings.HasPrefix(account, "A")
}

func validateUser(user string) error {
	if user == "" {
		return fmt.Errorf("%w: empty user token", ErrInvalidAgentID)
	}
	if !userKeyShape.MatchString(user) {
		return fmt.Errorf("%w: user token is not a user public NKEY (U and 55 base32 characters)", ErrInvalidAgentID)
	}
	if !nkeys.IsValidPublicUserKey(user) {
		return fmt.Errorf("%w: user key fails the NKEY check", ErrInvalidAgentID)
	}
	return nil
}

func validateAccount(account string) error {
	if account == "" {
		return fmt.Errorf("%w: empty account token", ErrInvalidAgentID)
	}
	if isAccountKeyShaped(account) {
		if !nkeys.IsValidPublicAccountKey(account) {
			return fmt.Errorf("%w: account key fails the NKEY check", ErrInvalidAgentID)
		}
		return nil
	}
	if !accountNameForm.MatchString(account) {
		return fmt.Errorf("%w: account token is neither an account public NKEY nor a name matching [A-Za-z0-9_-]+ or $G", ErrInvalidAgentID)
	}
	return nil
}
