// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package host is the agent side of the Synadia Agents Protocol (SAP) 0.3: it
// registers an agent as a NATS micro service, answers prompt requests with the
// reply stream SAP §6 defines, answers status requests, publishes heartbeats
// and, optionally, applies the sender identity extension (SAP §13).
//
// A host is built from the validated subject of one agent instance, a prompt
// handler and options:
//
//	subj, _ := agents.NewAgentSubject("my-agent", "me", "default")
//	h, _ := host.New(nc, subj, func(ctx context.Context, req *host.Request, w *host.Responder) error {
//	    return w.Send(ctx, "hello")
//	})
//	_ = h.Start(ctx)
//	defer h.Close()
//
// # What the host does for the handler
//
// For every prompt the host decodes the envelope (SAP §5.3), optionally
// enforces the endpoint's own limits (SDK policy, SAP §5.4), classifies and
// admits the sender (SAP §13.7), runs the interceptors, publishes the one
// leading ack (SAP §6.4) and calls the handler. The handler writes chunks
// through a [Responder]. When the handler returns, the host sends an error
// frame if it returned an error (SAP §9), then the zero-byte terminator
// (SAP §6.5), and nothing after it.
//
// A handler error becomes a SAP error frame: a [*RejectError] chooses its
// §9.2 code, an error wrapping agents.ErrProtocol is 400, and every other
// error is 500 "handler error"; its text never reaches the caller. An
// agents.ErrProtocol error is answered with the fixed text "malformed request";
// its reason goes to the logger only.
//
// # Reply subjects
//
// A prompt's reply subject is chosen by the caller, and the host publishes the
// whole reply stream there. Every prompt and status request is therefore vetted
// by a [ReplySubjectPolicy] first (SDK policy; [WithReplySubjectPolicy]). The
// default, [DefaultReplySubjectPolicy], refuses "$" system subjects, wildcards,
// this instance's own subjects and any "agents.prompt.", "agents.status." or
// "agents.hb." subject (a reply there would start a protocol loop or forge a
// heartbeat), and accepts ordinary inbox prefixes and other subjects. A
// refused request, status included, is dropped unanswered (there is no safe
// place to answer), counted in [Stats] and logged at warn. The policy is
// a second line of defense: run the host's NATS user with narrow publish
// permissions as well, limited to the inboxes it must answer.
//
// Extra endpoints ([WithEndpoint]) must not overlap the instance's prompt,
// status or heartbeat subject, wildcards included; [New] refuses them with
// [ErrEndpointOverlap].
//
// # Concurrency
//
// The nats.go micro callback of the prompt endpoint hands each request to its
// own goroutine, bounded by [WithMaxConcurrentPrompts] (default
// [DefaultMaxConcurrentPrompts], SDK policy for REQ-019). When every slot is
// busy, the [BusyPolicy] decides: [BusyQueue] holds the subscription's
// callback until a slot frees, [BusyReject429] answers 429 and the terminator
// at once. The status endpoint is served inline and outside the limit, so a
// liveness probe is always answered (SAP §8.7, §13.9).
//
// # Stopping
//
// [Host.Start] and [Host.Stop] are serialized through registration: a Stop that
// races a Start waits for it and then removes everything it registered.
//
// [Host.Stop] stops accepting prompts, waits for the handlers in flight and the
// micro callbacks in progress until its context ends and then cancels the
// handlers' contexts; a canceled handler's stream still gets an error frame and
// the terminator. The host is complete when the heartbeat and prompt workers
// have exited and those callbacks have finished. A Stop that returns the
// context's error leaves the shutdown running, and a later Stop or [Host.Close]
// waits for it. Nothing is answered once the host is complete: a request still
// queued on a drained subscription is dropped. [Host.Close] cancels first.
// Neither closes the connection, and the host publishes no going-away signal
// (SAP §8.6).
//
// # Acks and keep-alives
//
// The host sends an ack as the first message of the stream (SAP §6.4) and,
// by default, a further keep-alive ack every [DefaultKeepalive] (30 seconds)
// while the handler runs, as the official TypeScript and Python SDKs do: their
// callers' default 60 second inactivity timeout otherwise stalls long-running
// prompts. SAP:L380 says exactly one ack, so the default is a recorded
// deviation from the spec text (docs/upstream-issues.md, issue 18). Use
// [WithKeepalive] to change the cadence, and WithKeepalive(0) or
// [WithoutKeepalive] to turn it off for strict SAP:L380 behavior.
//
// # Statistics
//
// The micro statistics ($SRV.STATS) measure the dispatch of a prompt only; the
// accurate error counts, last error and processing time are in [Stats] and in
// the custom data of the prompt endpoint's micro stats.
//
// # Logging
//
// The host logs through the *slog.Logger of [WithLogger] and is silent
// without one. It never logs payloads, seeds, signatures or nonces.
//
// See docs/secure-configuration.md for the options that restrict who may prompt.
package host
