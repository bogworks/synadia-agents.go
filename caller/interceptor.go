// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// PromptInfo is what an interceptor or the header hook sees of one prompt
// before it goes out.
type PromptInfo struct {
	// Agent is the agent being prompted.
	Agent *Agent
	// Prompt is the prompt text.
	Prompt string
	// Subject is the subject the prompt will be published to.
	Subject string
	// Attachments is the number of attachments carried.
	Attachments int
	// Extra is a copy of the envelope's extra top-level fields (SAP §5.6).
	Extra map[string]json.RawMessage
}

// Extras are the additions one interceptor makes to a prompt.
type Extras struct {
	// Fields are extra top-level envelope fields (SAP §5.6). "prompt" and
	// "attachments" are refused with [ErrReservedField]. Each value is encoded
	// as JSON.
	Fields map[string]any
	// Headers are extra message headers. A header SAP defines or relies on is
	// refused with [ErrReservedHeader]; see [ReservedHeader].
	Headers map[string]string
	// State is handed back to [PublishInterceptor.BeforePublish]; it is never
	// sent.
	State any
}

// Interceptor extends every prompt of a [Client] (REQ-016). It is generic and
// protocol-level: it knows nothing of any application.
type Interceptor interface {
	// BeforePrompt is phase one: it returns what the prompt should carry in
	// addition. It must not publish. An error, or a panic, fails the prompt
	// before anything is sent. The returned Extras may be nil.
	BeforePrompt(ctx context.Context, p *PromptInfo) (*Extras, error)
}

// PublishInterceptor is the optional second phase, found by type assertion. It
// runs after the envelope is final, signed and size-checked, immediately before
// the publish, with the State its own BeforePrompt returned (nil when it
// returned no Extras). Nothing it does can stop the prompt: a panic is
// recovered and logged without its value.
type PublishInterceptor interface {
	Interceptor
	BeforePublish(ctx context.Context, p *PromptInfo, state any)
}

// InterceptorFunc adapts a function to an [Interceptor].
type InterceptorFunc func(ctx context.Context, p *PromptInfo) (*Extras, error)

// BeforePrompt calls f.
func (f InterceptorFunc) BeforePrompt(ctx context.Context, p *PromptInfo) (*Extras, error) {
	return f(ctx, p)
}

// collected is what the first phase gathered.
type collected struct {
	fields  map[string]json.RawMessage
	headers map[string]string
	states  []any
}

// runBeforePrompt runs phase one of every interceptor, then the header hook,
// and merges what they add; a later one wins a key an earlier one set.
func (c *Client) runBeforePrompt(ctx context.Context, p *PromptInfo) (*collected, error) {
	out := &collected{states: make([]any, len(c.cfg.interceptors))}
	for i, ic := range c.cfg.interceptors {
		ex, err := safeBefore(ctx, ic, p)
		if err != nil {
			return nil, fmt.Errorf("caller: interceptor %d: %w", i, err)
		}
		if ex == nil {
			continue
		}
		out.states[i] = ex.State
		for k, v := range ex.Fields {
			if k == "prompt" || k == "attachments" {
				return nil, fmt.Errorf("%w: %q", ErrReservedField, k)
			}
			raw, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("caller: interceptor %d: field %q: %w", i, k, err)
			}
			if out.fields == nil {
				out.fields = map[string]json.RawMessage{}
			}
			out.fields[k] = raw
		}
		if err := out.addHeaders(ex.Headers); err != nil {
			return nil, err
		}
	}
	if hook := c.cfg.headerHook; hook != nil {
		hs, err := safeHook(ctx, hook, p)
		if err != nil {
			return nil, fmt.Errorf("caller: header hook: %w", err)
		}
		if err := out.addHeaders(hs); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (o *collected) addHeaders(hs map[string]string) error {
	for k, v := range hs {
		if err := validHeader(k, v); err != nil {
			return err
		}
		if o.headers == nil {
			o.headers = map[string]string{}
		}
		o.headers[k] = v
	}
	return nil
}

func safeBefore(ctx context.Context, ic Interceptor, p *PromptInfo) (ex *Extras, err error) {
	defer func() {
		if r := recover(); r != nil {
			ex, err = nil, fmt.Errorf("interceptor panicked")
		}
	}()
	return ic.BeforePrompt(ctx, p)
}

func safeHook(ctx context.Context, h HeaderHook, p *PromptInfo) (hs map[string]string, err error) {
	defer func() {
		if r := recover(); r != nil {
			hs, err = nil, fmt.Errorf("hook panicked")
		}
	}()
	return h(ctx, p)
}

// runBeforePublish runs phase two; failures are logged and never stop the
// prompt.
func (c *Client) runBeforePublish(ctx context.Context, p *PromptInfo, col *collected) {
	for i, ic := range c.cfg.interceptors {
		pi, ok := ic.(PublishInterceptor)
		if !ok {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					c.log.Error("prompt interceptor BeforePublish panicked; publishing the prompt",
						slog.Int("interceptor", i))
				}
			}()
			pi.BeforePublish(ctx, p, col.states[i])
		}()
	}
}
