// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package agents holds what both sides of the Synadia Agents Protocol (SAP)
// 0.3 share: subjects, headers, the prompt envelope, the response stream's
// chunks, errors, heartbeats, attachments and the discovery record shapes. It
// needs no connection and imports only the standard library.
//
// The packages built on it do the networking. Package caller discovers agents,
// prompts them and reads the reply stream; package host registers an agent
// and serves prompts and status; package identity is the optional sender
// identity extension (SAP §13).
//
// # Caller
//
//	c, _ := caller.New(nc)                          // nc is a *nats.Conn
//	defer c.Close()
//	agent, _ := c.Find(ctx, caller.Filter{Agent: "echo"}) // $SRV.INFO.agents (SAP §4)
//	stream, _ := agent.PromptText(ctx, "hello")
//	defer stream.Close()
//	for ev, err := range stream.Events() {          // ack, response and query events
//	    if err != nil { ... }                       // *agents.ServiceError, a timeout, ...
//	    if r, ok := ev.(caller.Response); ok { fmt.Print(r.Text) }
//	}
//
// # Host
//
//	subject, _ := agents.NewAgentSubject("echo", "me", "default")
//	h, _ := host.New(nc, subject,
//	    func(ctx context.Context, req *host.Request, w *host.Responder) error {
//	        return w.Send(ctx, "echo: "+req.Envelope.Prompt) // the host acks first, ends last
//	    })
//	_ = h.Start(ctx)
//	defer h.Stop(ctx)
//
// The two meet in this package's types: the caller sends an [Envelope] and
// reads [Chunk] values; the host receives the [Envelope] ([DecodeEnvelope]),
// writes chunks ([EncodeChunk]) and failures as a [ServiceError]. Runnable
// programs are in the examples directory of the module.
//
// # Subjects
//
// Subjects are built and parsed here and never by hand: [AgentSubject], made
// only by [NewAgentSubject] or [ParseSubject] (both validate), builds the
// subjects of an instance, [FilterSubject]
// builds wildcard subscriptions. Only a host builds its prompt and status
// subjects from identity; a caller publishes to the subject in the discovery
// record (SAP §2, §4.3).
//
// # Tokens
//
// [ValidateToken] enforces SAP's MUSTs on a token and nothing stricter. SAP's
// SHOULDs come back as advice from [TokenWarnings] or, opt-in, as errors from
// [ValidateTokenStrict].
//
// # Errors
//
// The errors this package defines are typed or sentinel values for errors.Is
// and errors.As; other errors, such as an I/O failure from [SaveAttachments],
// are returned as the standard library reports them. A refusal made before
// anything is published matches [ErrLocal]; an error response from a peer is a
// [*ServiceError], tested by code with sentinels such as [ErrRateLimited], or
// with [CodeOf] and [IsCode]; a malformed received payload is a
// [*ProtocolError].
//
// # Versions
//
// The SDK implements SAP [SpecVersion] at [SpecCommit] and declares
// [ProtocolVersion]. [CheckProtocolVersion] compares only the MAJOR.MINOR
// prefix, and by default accepts only an exact match while the protocol is 0.x.
package agents
