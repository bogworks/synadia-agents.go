// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"strconv"

	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/nats-io/nats.go"
)

// DepthHeader is the application header that carries a prompt's delegation
// depth: how many tool prompts led to it. It is outside the protocol (SAP §5.6
// leaves application headers to the program) and advisory: it lets cooperating
// instances of these tools stop a delegation cycle early, but a hostile peer
// can omit or reset it, so [WithMaxActiveCalls] is the hard bound.
const DepthHeader = "Agent-Tools-Depth"

// maxParsedDepth keeps an absurd header value from overflowing a hop count.
const maxParsedDepth = 1 << 20

type (
	depthKeyType     struct{}
	outgoingDepthKey struct{}
)

// ContextWithDepth returns ctx marked as serving a prompt that arrived at
// delegation depth n. A host that does not use [Tools.Interceptor] reads the
// header with [DepthFromHeader] and passes the result here before it calls
// [Tools.RunInPromptScope].
func ContextWithDepth(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, depthKeyType{}, n)
}

func depthFromContext(ctx context.Context) (int, bool) {
	n, ok := ctx.Value(depthKeyType{}).(int)
	return n, ok
}

// DepthFromHeader reads the delegation depth from the headers of a received
// prompt: 0 when the header is missing, not a whole number or negative.
func DepthFromHeader(h nats.Header) int {
	n, err := strconv.Atoi(h.Get(DepthHeader))
	if err != nil || n < 0 {
		return 0
	}
	return min(n, maxParsedDepth)
}

// DepthInterceptor returns the caller interceptor that stamps [DepthHeader]
// on the prompts these tools make while serving a prompt of their own: add it
// to the client with [caller.WithInterceptors]. It sends no header for a prompt
// made outside a served prompt, or by anything but these tools. Without it a
// prompt carries no depth, the next hop counts from 0, and only
// [WithMaxActiveCalls] bounds a cycle.
func DepthInterceptor() caller.Interceptor { return depthInterceptor{} }

type depthInterceptor struct{}

func (depthInterceptor) BeforePrompt(ctx context.Context, _ *caller.PromptInfo) (*caller.Extras, error) {
	n, ok := ctx.Value(outgoingDepthKey{}).(int)
	if !ok {
		return nil, nil
	}
	return &caller.Extras{Headers: map[string]string{DepthHeader: strconv.Itoa(n)}}, nil
}
