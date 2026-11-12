// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package hosttest is the private hook between package host and the protocol
// test fixtures (package agentstest and this module's tests). It carries the
// raw prompt handler, which bypasses envelope checks, the ack, framing and the
// terminator and so can build a noncompliant host. It is internal so that the
// raw handler is not part of the host package's public, compatibility-bound
// API: host exports no symbol for it, and no package-level state is involved.
package hosttest

import (
	"context"
	"errors"
	"time"

	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go/micro"
)

// RawHandler serves a prompt with no help from the host: it receives the raw
// micro request and the sender the gate classified, and publishes the whole
// reply stream itself (ack, chunks, error frames and terminator), or
// deliberately does not. An error it returns is answered as a 500 error frame
// and a terminator.
type RawHandler func(ctx context.Context, req micro.Request, sender *identity.Sender) error

// Hooks is the fixture-only part of the host's configuration. Package host
// embeds one in its config and exposes it through [Carrier].
type Hooks struct {
	// Raw replaces the prompt pipeline after sender admission: no envelope
	// decode, limit check, interceptor, ack, keep-alive, error mapping or
	// terminator is applied, and the handler passed to host.New is not called
	// and may be nil.
	Raw RawHandler

	// CloseTimeout replaces host.DefaultCloseTimeout for this host's Close,
	// so a test can bound Close in milliseconds. Zero means the default.
	CloseTimeout time.Duration
}

// Carrier is implemented by package host's unexported config.
type Carrier interface{ TestHooks() *Hooks }

// RawOption builds a host option (O is host.Option) that installs h as the
// raw prompt handler. The generic form lets this package name the option type
// without importing host, and lets host export nothing for it. A nil h makes
// host.New fail.
func RawOption[O ~func(C) error, C Carrier](h RawHandler) O {
	return func(c C) error {
		if h == nil {
			return errors.New("hosttest: nil raw prompt handler")
		}
		c.TestHooks().Raw = h
		return nil
	}
}

// CloseTimeoutOption builds a host option (O is host.Option) that sets the
// bound Close applies when it is given no deadline of its own, in place of
// host.DefaultCloseTimeout. A d that is not positive makes host.New fail.
func CloseTimeoutOption[O ~func(C) error, C Carrier](d time.Duration) O {
	return func(c C) error {
		if d <= 0 {
			return errors.New("hosttest: close timeout must be positive")
		}
		c.TestHooks().CloseTimeout = d
		return nil
	}
}
