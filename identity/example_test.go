// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity_test

import (
	"errors"
	"fmt"

	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nkeys"
)

func ExampleParseAgentID() {
	kp, err := nkeys.CreateUser()
	if err != nil {
		panic(err)
	}
	user, err := kp.PublicKey()
	if err != nil {
		panic(err)
	}
	id, err := identity.NewAgentID("$G", user)
	if err != nil {
		panic(err)
	}

	// The text form parses back to the same ID.
	parsed, err := identity.ParseAgentID(string(id))
	fmt.Println(parsed == id, parsed.Account(), err)

	// Anything but {account}.{user} is refused.
	_, err = identity.ParseAgentID("not-an-agent-id")
	fmt.Println(errors.Is(err, identity.ErrInvalidAgentID))
	// Output:
	// true $G <nil>
	// true
}

func ExampleStance() {
	// Each check is its own setting; the zero value is Unset, which takes the
	// SDK default of that check.
	s := identity.Stance{
		SignedSenders: identity.On,
		Signing:       identity.On,
	}
	fmt.Println(s.SignedSenders, s.Signing, s.HeartbeatVerification)
	// Output:
	// on on unset
}

func ExampleStance_Resolved() {
	// Resolved replaces every Unset check with its SDK default.
	s := identity.Stance{SignedSenders: identity.On}.Resolved()
	fmt.Println("signed senders:", s.SignedSenders)
	fmt.Println("verified discovery:", s.VerifiedDiscovery)
	fmt.Println("heartbeat verification:", s.HeartbeatVerification)
	fmt.Println("signing:", s.Signing)
	fmt.Println("require signer:", s.RequireSigner)
	// Output:
	// signed senders: on
	// verified discovery: off
	// heartbeat verification: off
	// signing: off
	// require signer: on
}
