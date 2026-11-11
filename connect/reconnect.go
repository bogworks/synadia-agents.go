// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package connect

import (
	"time"

	"github.com/nats-io/nats.go"
)

// Reconnect defaults for agent runtimes (parity row 1.11; outside SAP,
// deviation D-2). They match the TypeScript SDK's AGENT_RECONNECT_DEFAULTS.
const (
	// DefaultReconnectWait is the wait between reconnect attempts.
	DefaultReconnectWait = 2 * time.Second
	// DefaultReconnectJitter is the random addition to that wait.
	DefaultReconnectJitter = 200 * time.Millisecond
	// DefaultReconnectJitterTLS is the jitter for TLS connections (the nats.go default).
	DefaultReconnectJitterTLS = time.Second
)

// ReconnectDefaults returns options that keep an agent reachable: never give
// up reconnecting (MaxReconnects -1), wait 2s plus up to 200ms of jitter
// between attempts, and keep retrying from the first attempt when the server
// is unreachable at startup.
//
// Difference from the TypeScript waitOnFirstConnect: with nats.go,
// RetryOnFailedConnect makes nats.Connect return at once with a connection in
// the RECONNECTING state, rather than block until the first success. Use
// nc.StatusChanged to learn when it is up. See parity row 1.11.
func ReconnectDefaults() []nats.Option {
	return []nats.Option{
		nats.MaxReconnects(-1),
		nats.ReconnectWait(DefaultReconnectWait),
		nats.ReconnectJitter(DefaultReconnectJitter, DefaultReconnectJitterTLS),
		nats.RetryOnFailedConnect(true),
	}
}

// WithAgentReconnectDefaults returns [ReconnectDefaults] followed by user. The
// options apply in order, so any setting in user overrides the default, and an
// explicit MaxReconnects(0) or RetryOnFailedConnect(false) survives. The
// result is a new slice; user is not modified. See parity row 1.12.
func WithAgentReconnectDefaults(user ...nats.Option) []nats.Option {
	return append(ReconnectDefaults(), user...)
}
