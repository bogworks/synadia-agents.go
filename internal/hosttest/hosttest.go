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

// RawOption is set by package host when it is initialized: it builds the
// host.Option, returned as any, that installs a raw prompt handler. With it
// the handler passed to host.New may be nil. A nil f makes the option fail
// host.New.
var RawOption = func(RawHandler) any { panic("hosttest: package host is not linked") }
