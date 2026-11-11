// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"slices"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
)

// Request is a prompt request as the handler sees it. See SAP §5.
type Request struct {
	// Envelope is the decoded prompt payload, unknown fields in Extra
	// (SAP §5.3, §5.6).
	Envelope agents.Envelope
	// Sender is the classified sender: a verified or claimed *identity.Sender,
	// nil for the absent class (no Agent-Sender header, or an unknown v).
	// Never grant on a claimed sender. See SAP §13.1.
	Sender *identity.Sender
	// Subject is the subject the request arrived on.
	Subject string
	// Header is a clone of the received headers (REQ-016); changing it does
	// not affect the host.
	Header nats.Header

	host *Host
}

// ResolveSender maps a verified sender back to the agent instance that
// registered its agent ID, with the id_sig checked (SAP §13.10, "Reverse
// lookup"). It identifies and never authorizes. ok is false when the sender is
// not verified or no verified instance claims the key (a human, a plain
// service, an offline agent). The index is cached for [WithResolveTTL].
func (r *Request) ResolveSender(ctx context.Context) (reg identity.Registration, ok bool, err error) {
	if r.Sender == nil || r.Sender.Trust != identity.TrustVerified || r.host == nil {
		return identity.Registration{}, false, nil
	}
	return r.host.resolver.Resolve(ctx, r.Sender.ID)
}

// RequestInfo is what an [Interceptor] and the admission hook see of a
// request, after the envelope decode and the sender classification.
type RequestInfo struct {
	// Envelope is the decoded envelope; unknown fields are in Extra.
	Envelope agents.Envelope
	// Sender is the classified sender, nil for the absent class. In the
	// admission hook it is the sender being judged.
	Sender *identity.Sender
	// Subject is the subject the request arrived on.
	Subject string
	// Header is a clone of the received headers.
	Header nats.Header
}

func cloneHeader(h nats.Header) nats.Header {
	if h == nil {
		return nats.Header{}
	}
	out := make(nats.Header, len(h))
	for k, v := range h {
		out[k] = slices.Clone(v)
	}
	return out
}
