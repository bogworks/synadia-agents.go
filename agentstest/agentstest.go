// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package agentstest provides a reference agent for tests: a spec-compliant
// SAP counterparty built on package host that a program can run beside the
// code under test, in the way the TypeScript SDK ships its ReferenceAgent.
//
// By default the reference agent echoes the prompt back as one response chunk
// through the full host pipeline: one ack, the chunk, the terminator. It
// heartbeats every second, the floor SAP recommends, so a test that waits for
// a heartbeat does not wait long. A test that needs a stream a compliant host
// would never produce (no ack, a missing terminator, a malformed chunk) uses
// [WithRawHandler], which hands it the raw request.
//
//	subj, _ := agents.NewAgentSubject("echo", "tests", "one")
//	ra, _ := agentstest.NewReferenceAgent(nc, subj)
//	_ = ra.Start(ctx)
//	defer ra.Stop(ctx)
//
// The package is for tests; it makes no promise of production fitness.
package agentstest

import (
	"context"
	"errors"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/bogworks/synadia-agents.go/internal/hosttest"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
)

// DefaultHeartbeatInterval is the reference agent's heartbeat cadence, the
// shortest SAP recommends (SAP §8.2). Override it with [WithHost].
const DefaultHeartbeatInterval = time.Second

// RawHandler serves a prompt with the raw micro request and the classified
// sender (nil for the absent class), as the TypeScript
// ReferenceAgentPromptHandler does: the host sends no ack, chunk, error frame
// or terminator for it. An error it returns is answered as a 500 error frame
// and a terminator.
type RawHandler func(req micro.Request, sender *identity.Sender) error

type refConfig struct {
	handler host.PromptHandler
	raw     RawHandler
	host    []host.Option
}

// ReferenceOption configures a [ReferenceAgent].
type ReferenceOption func(*refConfig)

// WithHandler replaces the echo with a handler served through the full host
// pipeline (decode, admission, ack, error mapping, terminator).
func WithHandler(h host.PromptHandler) ReferenceOption {
	return func(c *refConfig) { c.handler = h }
}

// WithRawHandler serves prompts with h and no help from the host. See
// [RawHandler].
func WithRawHandler(h RawHandler) ReferenceOption {
	return func(c *refConfig) { c.raw = h }
}

// WithHost passes options to the underlying [host.Host], after the reference
// agent's own defaults, so they win.
func WithHost(opts ...host.Option) ReferenceOption {
	return func(c *refConfig) { c.host = append(c.host, opts...) }
}

// ReferenceAgent is a spec-compliant agent for tests. It wraps a [host.Host].
type ReferenceAgent struct {
	h *host.Host
}

// echoHandler answers with the prompt text.
func echoHandler(ctx context.Context, req *host.Request, w *host.Responder) error {
	return w.Send(ctx, req.Envelope.Prompt)
}

// NewReferenceAgent builds a reference agent for subject on nc. Nothing is
// registered until [ReferenceAgent.Start].
func NewReferenceAgent(nc *nats.Conn, subject agents.AgentSubject, opts ...ReferenceOption) (*ReferenceAgent, error) {
	var c refConfig
	for _, o := range opts {
		o(&c)
	}
	handler := c.handler
	if handler == nil {
		handler = echoHandler
	}
	hopts := []host.Option{
		host.WithHeartbeatInterval(DefaultHeartbeatInterval),
		host.WithDescription(subject.Agent() + " reference agent"),
	}
	if c.raw != nil {
		raw := c.raw
		opt, ok := hosttest.RawOption(hosttest.RawHandler(func(_ context.Context, req micro.Request, s *identity.Sender) error {
			return raw(req, s)
		})).(host.Option)
		if !ok {
			return nil, errors.New("agentstest: the host package has no raw handler hook")
		}
		hopts = append(hopts, opt)
	}
	h, err := host.New(nc, subject, handler, append(hopts, c.host...)...)
	if err != nil {
		return nil, err
	}
	return &ReferenceAgent{h: h}, nil
}

// Start registers the agent and publishes its first heartbeat.
func (r *ReferenceAgent) Start(ctx context.Context) error { return r.h.Start(ctx) }

// Stop stops the agent without closing the connection.
func (r *ReferenceAgent) Stop(ctx context.Context) error { return r.h.Stop(ctx) }

// InstanceID returns the service id, the heartbeat's instance_id; empty before
// Start.
func (r *ReferenceAgent) InstanceID() string { return r.h.InstanceID() }

// PromptSubject returns the prompt endpoint's subject.
func (r *ReferenceAgent) PromptSubject() string { return r.h.Subject().Prompt() }

// HeartbeatSubject returns the heartbeat subject.
func (r *ReferenceAgent) HeartbeatSubject() string { return r.h.Subject().Heartbeat() }

// StatusSubject returns the status endpoint's subject.
func (r *ReferenceAgent) StatusSubject() string { return r.h.Subject().Status() }

// SenderGate returns the gate that classifies the agent's requests.
func (r *ReferenceAgent) SenderGate() *identity.Gate { return r.h.Gate() }

// Identity returns the agent ID the agent registered, if any.
func (r *ReferenceAgent) Identity() (identity.AgentID, bool) { return r.h.Identity() }

// MinSenderTrust returns the min_sender_trust the agent advertises.
func (r *ReferenceAgent) MinSenderTrust() identity.MinSenderTrust { return r.h.MinSenderTrust() }

// Host returns the underlying host, for the options and accessors this
// package does not repeat.
func (r *ReferenceAgent) Host() *host.Host { return r.h }
