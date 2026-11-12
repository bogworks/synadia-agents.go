// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package agents holds the Synadia Agents Protocol (SAP) 0.3 wire types and
// rules that every side shares: subjects, headers, the prompt envelope, the
// response stream's chunks, errors, heartbeats, attachments and the discovery
// record shapes. It needs no connection and imports only the standard library.
//
// Start in the package for the job. Package host registers an agent and serves
// prompts and status. Package caller discovers agents, prompts them and reads
// the reply stream. Package tools exposes agents to a model as tool use.
// Package connect turns NATS CLI context files into connections. Package
// identity is the optional sender identity extension (SAP §13). Packages
// agentstest and conformance are for testing.
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
//	defer h.Close()
//
// The two meet in this package's types: the caller sends an [Envelope] and
// reads [Chunk] values; the host receives the [Envelope] ([DecodeEnvelope]),
// writes chunks ([EncodeChunk]) and failures as a [ServiceError]. Runnable
// programs are in the examples directory of the module.
//
// # Wire vocabulary
//
// Each group below is one concept, kept in the file or files named.
//
// Subjects and tokens (subjects.go, tokens.go; SAP §2). [AgentSubject], made
// only by [NewAgentSubject] or [ParseSubject] (both validate), builds the
// subjects of an instance. [FilterSubject] builds wildcard subscriptions.
// Only a host builds its prompt and status subjects from identity; a caller
// publishes to the subject in the discovery record (SAP §2, §4.3).
// [ValidateToken] enforces SAP's MUSTs on a token and nothing stricter. SAP's
// SHOULDs come back as advice from [TokenWarnings] or, opt-in, as errors from
// [ValidateTokenStrict].
//
// Requests (envelope.go, attachments.go, bytes.go; SAP §2.1, §5). [Envelope]
// is the prompt, encoded and decoded by [DecodeEnvelope] and
// [EncodePlainPrompt], with its size bounds in [PromptLimits]. [Attachment]
// is the wire form of a file; [SaveAttachments] writes received ones to disk.
// [ParseSize] and [FormatSize] read and write the max_payload sizes.
//
// Responses (chunks.go, headers.go, errors.go; SAP §5.4, §6, §9). [Chunk] is
// one reply-stream message, with [DecodeChunk] and [EncodeChunk]. [IsTerminator]
// and [IsErrorSignal] classify a message by its headers. A refusal made before
// anything is published matches [ErrLocal]; an error response from a peer is a
// [*ServiceError], tested by code with sentinels such as [ErrRateLimited], or
// with [CodeOf] and [IsCode]; a malformed received payload is a
// [*ProtocolError]. Other errors, such as an I/O failure from
// [SaveAttachments], are returned as the standard library reports them.
//
// Discovery and liveness (discovery.go, heartbeat.go; SAP §2.1, §3, §4, §8).
// [ServiceInfo] and [ServicePing] are the service records, decoded by
// [DecodeServiceInfo] and [DecodeServicePing] and turned into an [AgentInfo]
// by [NewAgentInfo]. [Heartbeat] is the liveness payload, with
// [EncodeHeartbeat], [DecodeHeartbeat] and [ValidateInterval].
//
// Versions (version.go; SAP §11). The SDK implements SAP [SpecVersion] at
// [SpecCommit] and declares [ProtocolVersion]. [CheckProtocolVersion] compares
// only the MAJOR.MINOR prefix, and by default accepts only an exact match
// while the protocol is 0.x.
//
// # Why one package
//
// The host and the caller must use the same encoders and decoders to read each
// other's bytes, so the codecs for both directions sit together. Splitting by
// concept would make each side import several small packages for one
// protocol. The official TypeScript and Python SDKs keep the same set in one
// top-level package. The files are named after the protocol concepts, so a
// reader finds one by the name of the SAP section.
package agents
