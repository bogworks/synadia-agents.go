// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
)

// inboxPrefix is the reply-inbox prefix every SDK uses, so one NATS
// permission covers all of them (an SDK convention, not SAP).
const inboxPrefix = "_INBOX.agents."

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("host: crypto/rand failed: %v", err)) // never happens on supported platforms
	}
	return b
}

// newInbox returns a fresh reply subject under the SDK inbox prefix.
func newInbox() string { return inboxPrefix + hex.EncodeToString(randBytes(11)) }

// newUUID returns a random (version 4) UUID, the form SAP §7.1 recommends for a
// query id.
func newUUID() string {
	b := randBytes(16)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func natsHeader(h micro.Headers) nats.Header { return nats.Header(h) }
