// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"sync/atomic"
)

// CallNext continues a request: it publishes the ack, runs the interceptors
// after the caller's and then the handler. The context passed reaches the
// handler (the host still cancels it when the host stops).
type CallNext func(ctx context.Context) error

// Interceptor wraps the handling of a prompt request (REQ-016). The first
// interceptor listed is the outermost.
//
// AroundRequest runs after the envelope decode, the limit checks and the
// sender admission, before the ack. Call next exactly once, or return an error:
// returning nil without calling next answers 500; a second call returns
// [ErrNextCalledTwice]. An interceptor that refuses before next leaves the
// caller an error frame and the terminator, and no ack. An error returned
// after next succeeded leaves the reply standing and is logged with a fixed
// line.
type Interceptor interface {
	AroundRequest(ctx context.Context, info *RequestInfo, next CallNext) error
}

// InterceptorFunc adapts a function to [Interceptor].
type InterceptorFunc func(ctx context.Context, info *RequestInfo, next CallNext) error

// AroundRequest calls f.
func (f InterceptorFunc) AroundRequest(ctx context.Context, info *RequestInfo, next CallNext) error {
	return f(ctx, info, next)
}

// runChain runs serve inside the interceptors.
func runChain(ctx context.Context, chain []Interceptor, info *RequestInfo, serve func(context.Context) error) error {
	var at func(i int, ctx context.Context) error
	at = func(i int, ctx context.Context) error {
		if i == len(chain) {
			return serve(ctx)
		}
		var called atomic.Bool
		return chain[i].AroundRequest(ctx, info, func(c context.Context) error {
			if !called.CompareAndSwap(false, true) {
				return ErrNextCalledTwice
			}
			if c == nil {
				c = ctx
			}
			return at(i+1, c)
		})
	}
	return at(0, ctx)
}
