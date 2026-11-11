// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"

	"github.com/bogworks/synadia-agents.go/caller"
)

// Extension adds to the tools without changing them (docs/agent-tools.md
// section 6.3). An Extension implements any of the optional hook interfaces
// [DiscoveryFields], [PromptRewriter] and [ReplyLooker]; the helper finds them
// by type assertion and runs several extensions in the order given to
// [WithExtensions]. A hook may not set a field the contract defines: discovery
// and prompt hooks that try get a refusal, a reply look that tries fails the
// call and is logged.
type Extension interface {
	// Name identifies the extension in logs and refusals.
	Name() string
}

// DiscoveryFields adds fields to a discover_agents entry, given the agent's
// live handle. See SAP §4 for the discovery record; the entry shape is the
// discover_agents result fixture.
type DiscoveryFields interface {
	DiscoveryFields(ctx context.Context, a *caller.Agent) (map[string]any, error)
}

// PromptRewriter rewrites a prompt before prompt_agent sends it, after the
// guards. See [PromptContext] and [PromptRewrite].
type PromptRewriter interface {
	RewritePrompt(ctx context.Context, p *PromptContext) (*PromptRewrite, error)
}

// ReplyLooker looks at a completed reply and at an arriving question, and may
// add fields to that result. It runs in the call's reader, outside any tool
// call: an error or a reserved field fails the call (SAP §6.3 and §7.1 carry
// the content it sees).
type ReplyLooker interface {
	LookReply(ctx context.Context, r *ReplyContext) (map[string]any, error)
}

// PromptContext is what a [PromptRewriter] sees.
type PromptContext struct {
	// Address is the target's prompt subject.
	Address string
	// Prompt is the prompt text as the extensions before this one left it.
	Prompt string
	// Attachments are the attachment paths, resolved and checked against the roots.
	Attachments []string
	// Target is the agent the prompt goes to.
	Target *caller.Agent
	// CallID is the call's id as the model will see it.
	CallID string
	// Label is the call's label, "" when it has none.
	Label string
	// ToolCallID is the model's id for this tool call, "" when not given.
	ToolCallID string
	// Caller is the served prompt's verified sender id, "" when there is none.
	Caller string
}

// PromptRewrite is what a [PromptRewriter] may return; every field is optional.
type PromptRewrite struct {
	// Prompt, when non-nil, is the prompt's new text.
	Prompt *string
	// Context holds values made available to the client's prompt interceptors
	// through [PromptContextFromContext]. They are never sent.
	Context map[string]any
	// Fields are added to every result of the call.
	Fields map[string]any
	// Error refuses the prompt: nothing is sent and the model gets this text.
	Error string
}

// ReplyContext is what a [ReplyLooker] sees.
type ReplyContext struct {
	CallID  string
	Address string
	Target  *caller.Agent
	// Kind is "reply" for a completed reply and "question" for an arriving question.
	Kind string
	// Text is the whole reply, or the question.
	Text string
	// Attachments are the files that came with it, saved.
	Attachments []ReturnedFile
}

// The fields the contract defines; an extension may not set them.
var callResultFields = map[string]bool{
	"call_id": true, "state": true, "label": true, "reply": true, "question": true,
	"error": true, "partial_reply": true, "attachments": true, "remaining": true,
	"call_ids": true, "open_calls": true, "open_calls_note": true,
}

var discoveryEntryFields = map[string]bool{
	"address": true, "agent": true, "owner": true, "name": true, "description": true,
	"identity": true, "identity_verified": true, "requires_signed_prompts": true,
	"accepts_attachments": true, "instances": true,
}

// reservedFieldError reports an extension that set a contract field.
type reservedFieldError struct{ ext, field, where string }

func (e *reservedFieldError) Error() string {
	return "tools: extension " + e.ext + " may not set \"" + e.field + "\" on " + e.where
}

// addFields merges an extension's fields into dst, refusing contract fields.
func addFields(dst, added map[string]any, reserved map[string]bool, ext, where string) error {
	for k, v := range added {
		if reserved[k] {
			return &reservedFieldError{ext: ext, field: k, where: where}
		}
		dst[k] = v
	}
	return nil
}
