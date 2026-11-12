// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package hosttest is the private hook between package host and the protocol
// test fixtures (package agentstest and this module's tests). It carries the
// raw prompt handler, which bypasses envelope checks, the ack, framing and the
// terminator and so can build a noncompliant host. It is internal so that the
// raw handler is not part of the host package's public, compatibility-bound
// API.
package hosttest

import (
	"context"

	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go/micro"
)

// RawHandler serves a prompt with no help from the host: it receives the raw
// micro request and the sender the gate classified, and publishes the whole
// reply stream itself (ack, chunks, error frames and terminator), or
// deliberately does not. An error it returns is answered as a 500 error frame
// and a terminator.
type RawHandler func(ctx context.Context, req micro.Request, sender *identity.Sender) error

// Raw carries a [RawHandler] to host.WithRawPromptHandler. Only this package
// can build one (with [NewRaw]) and it is internal, so no code outside this
// module can call that option. The zero Raw holds no handler, and the option
// fails host.New with it.
type Raw struct{ h RawHandler }

// NewRaw wraps h; a nil h gives the zero Raw.
func NewRaw(h RawHandler) Raw { return Raw{h} }

// Handler returns the wrapped handler, or nil.
func (r Raw) Handler() RawHandler { return r.h }
