// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller_test

import (
	"fmt"

	"github.com/nats-io/nats.go"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/bogworks/synadia-agents.go/identity"
)

// A client that discovers only agents whose id_sig verifies and whose signer is
// on a list. The example needs a NATS server, so it compiles but does not run.
func ExampleWithTrustPolicy() {
	var nc *nats.Conn
	trusted := []identity.AgentID{"ACCOUNT.USER"} // real IDs: identity.ParseAgentID

	c, err := caller.New(nc, caller.WithTrustPolicy(caller.TrustPolicy{
		Agents: trusted,
		// Accept runs as well, when set, and must not block.
		Accept: func(id identity.AgentID, info agents.AgentInfo) bool {
			return info.Owner == "example"
		},
	}))
	if err != nil {
		fmt.Println(err)
		return
	}
	defer c.Close()
}
