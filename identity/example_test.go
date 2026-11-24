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
