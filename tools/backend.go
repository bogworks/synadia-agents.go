// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"iter"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/bogworks/synadia-agents.go/identity"
)

// backend is the part of the caller the tools use. *caller.Client satisfies it
// through clientBackend; the unit tests satisfy it with scripted streams, so
// every state of a call is reachable without a server.
type backend interface {
	discover(ctx context.Context, f caller.Filter, timeout time.Duration) ([]target, error)
	// inboxPrefixes are the reply-subject prefixes the connection itself
	// vouches for, besides "_INBOX."; nil when it names none.
	inboxPrefixes() []string
}

// promptParams are what the tools ask of one prompt besides its envelope.
type promptParams struct {
	maxWait time.Duration
	// replyPolicy vets the reply subject of every question of the prompt; nil
	// leaves the client's own policy.
	replyPolicy func(subject string) error
}

// target is one discovered agent instance.
type target interface {
	promptSubject() string
	info() agents.AgentInfo
	minSenderTrust() identity.MinSenderTrust
	// handle is the real caller.Agent handed to extensions; nil in tests.
	handle() *caller.Agent
	prompt(ctx context.Context, env agents.Envelope, pp promptParams) (callStream, error)
}

// callStream is the reply stream of one prompt.
type callStream interface {
	events() iter.Seq2[streamEvent, error]
	// close drops the stream; it is idempotent and sends nothing on the wire.
	close()
	// terminated reports that the agent ended the stream with its terminator.
	terminated() bool
}

// streamEvent is a response chunk or a question; acks and status are not
// surfaced.
type streamEvent struct {
	text        string
	attachments []agents.Attachment
	query       *question
}

// question is a mid-stream query (SAP §7), answered once.
type question struct {
	prompt       string
	replySubject string
	attachments  []agents.Attachment
	reply        func(ctx context.Context, answer string) error
}

type clientBackend struct{ c *caller.Client }

func (b clientBackend) discover(ctx context.Context, f caller.Filter, timeout time.Duration) ([]target, error) {
	opts := []caller.DiscoverOption{caller.WithFilter(f)}
	if timeout > 0 {
		opts = append(opts, caller.WithDiscoverTimeout(timeout))
	}
	found, err := b.c.Discover(ctx, opts...)
	if err != nil {
		return nil, err
	}
	out := make([]target, len(found))
	for i, a := range found {
		out[i] = clientTarget{a}
	}
	return out, nil
}

// inboxPrefixes names the connection's custom inbox prefix, if it set one.
func (b clientBackend) inboxPrefixes() []string {
	if nc := b.c.Connection(); nc != nil && nc.Opts.InboxPrefix != "" {
		return []string{nc.Opts.InboxPrefix}
	}
	return nil
}

type clientTarget struct{ a *caller.Agent }

func (t clientTarget) promptSubject() string                   { return t.a.PromptSubject() }
func (t clientTarget) info() agents.AgentInfo                  { return t.a.Info() }
func (t clientTarget) minSenderTrust() identity.MinSenderTrust { return t.a.MinSenderTrust() }
func (t clientTarget) handle() *caller.Agent                   { return t.a }

func (t clientTarget) prompt(ctx context.Context, env agents.Envelope, pp promptParams) (callStream, error) {
	opts := []caller.PromptOption{caller.WithPromptMaxWait(pp.maxWait)}
	if pp.replyPolicy != nil {
		opts = append(opts, caller.WithPromptQueryReplyPolicy(pp.replyPolicy))
	}
	s, err := t.a.Prompt(ctx, env, opts...)
	if err != nil {
		return nil, err
	}
	return clientStream{s}, nil
}

type clientStream struct{ s *caller.Stream }

func (c clientStream) close()           { _ = c.s.Close() }
func (c clientStream) terminated() bool { return c.s.Terminated() }

func (c clientStream) events() iter.Seq2[streamEvent, error] {
	return func(yield func(streamEvent, error) bool) {
		for ev, err := range c.s.Events() {
			if err != nil {
				yield(streamEvent{}, err)
				return
			}
			var out streamEvent
			switch e := ev.(type) {
			case caller.Response:
				out = streamEvent{text: e.Text, attachments: e.Attachments}
			case *caller.Query:
				out = streamEvent{query: &question{prompt: e.Prompt, replySubject: e.ReplySubject, attachments: e.Attachments, reply: e.Reply}}
			default:
				continue // acks and status keep the stream alive and say nothing more
			}
			if !yield(out, nil) {
				return
			}
		}
	}
}
