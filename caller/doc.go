// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package caller is the caller side of the Synadia Agents Protocol (SAP) 0.3:
// it discovers agents, prompts them, reads their reply streams and tracks their
// heartbeats.
//
//	c, _ := caller.New(nc)                      // nc is a *nats.Conn
//	defer c.Close(context.Background())
//	found, _ := c.Discover(ctx)                 // $SRV.INFO.agents (SAP §4.1)
//	stream, _ := found[0].PromptText(ctx, "hello")
//	defer stream.Close()
//	for ev, err := range stream.Events() {      // ack, response and query events
//		if err != nil { ... }                   // *agents.ServiceError, timeout, ...
//		if r, ok := ev.(caller.Response); ok { fmt.Print(r.Text) }
//	}
//
// # Discovery
//
// [Client.Discover] starts a heartbeat tracker and flushes it before it
// publishes $SRV.INFO.agents (SAP §8.5), then reads each reply as an agent. A
// caller addresses an endpoint by the subject in the discovery record and never
// builds one from identity (SAP §2, §4.3). [Client.Ping] and [Client.PingAll]
// use the micro service ping (SAP §8.4); [Client.LookupInstance] and
// [Client.Find] resolve one instance or one identity tuple.
//
// # Prompting
//
// [Agent.Prompt] runs every local MUST of SAP §5.4 before it publishes:
// non-empty prompt, attachments against attachments_ok, encoded size against
// max_payload (an Agent-Sender header counted), the connection's own limit, the
// protocol version, and a signed-only endpoint against the client's ability to
// sign. Local refusals match [agents.ErrLocal]; errors a peer reported are
// *[agents.ServiceError].
//
// # Streams
//
// A [Stream] yields events in wire order through an [iter.Seq2]. Acks reset the
// inactivity timeout (default 60 seconds, SAP §6.6); unknown chunk types and
// unknown status values are ignored; a malformed chunk of a known type is
// dropped, counted in [Client.Stats] and logged at debug level. An
// error-headered message ends the sequence with its [agents.ServiceError]. The
// zero-byte terminator ends it silently and sets [Stream.Terminated]. A query
// event takes exactly one reply from [Query.Reply].
//
// # Concurrency
//
// A Client is safe for concurrent use. It owns one reply subscription,
// _INBOX.agents.{client-id}.>, and routes each reply by its own token to the
// stream, discovery or request that issued it, each with a bounded buffer. A
// reply with no waiting stream (a late one, for instance) is dropped, counted
// in [Stats.DroppedReplies] and never delivered elsewhere. A stream that
// overflows its buffer ends with a [*SlowConsumerError] without affecting
// others.
//
// # Lifetime
//
// No goroutine of the SDK outlives its context or [Client.Close], and every
// subscription is drained on close. The client never closes the connection.
//
// # Sender identity
//
// With [WithIdentity] the client signs the Agent-Sender header of every prompt
// and status request once the connection's identity is known (SAP §13.5), and
// refuses locally to send to an endpoint that requires a signed sender when it
// cannot (SAP §13.9). It never signs a reply, and it sends no header from an
// unknown identity (SAP §13.4). [Client.ResolveSender] is the reverse lookup of
// SAP §13.10.
//
// See docs/secure-configuration.md for the trust policy and verified discovery.
package caller
