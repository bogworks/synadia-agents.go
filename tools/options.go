// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
)

// DefaultMaxCalls is the number of calls tracked per scope (256, the value
// both official SDKs use). When a new call would pass it, the call that
// finished longest ago is dropped; when every tracked call is still open,
// prompt_agent refuses.
const DefaultMaxCalls = 256

// DefaultMaxSavedBytesPerCall is the total of returned files saved per call,
// 64 MiB, the SDK's default for [agents.SaveAttachments].
const DefaultMaxSavedBytesPerCall = agents.DefaultSaveMaxTotalBytes

// QuestionRefusal is what an open question is answered with when its call is
// canceled (by cancel_agent, a host's abort, or the end of a served prompt),
// fails or expires. It starts with "no", so an agent that asked for a
// permission reads a denial. It is the constant both official SDKs use
// (AGENT_TOOLS_QUESTION_REFUSAL). See SAP §7.2 for the reply.
const QuestionRefusal = "no: the caller stopped waiting for this prompt and cannot answer"

// Defaults of the limits that bound what one call, one served prompt and the
// whole process can cost. They are SDK policy: SAP lets an agent stream for as
// long as it likes and ask as many questions as it likes (SAP §6, §7), so a
// program that offers these tools to a model protects itself with them.
const (
	// DefaultMaxReplyBytes is the reply text kept per call, 4 MiB.
	DefaultMaxReplyBytes = 4 << 20
	// DefaultMaxQuestions is the questions one call may ask over its life, 64.
	DefaultMaxQuestions = 64
	// DefaultMaxReturnedFiles is the returned-file records one call may
	// collect, 64. Skipped files count: each is a record in the result.
	DefaultMaxReturnedFiles = 64
	// DefaultMaxResultBytes is the total a call may hold for its result, 8 MiB:
	// reply text, question texts and returned-file records together.
	DefaultMaxResultBytes = 8 << 20
	// DefaultMaxDepth is how many delegation hops deep a prompt may go, 4.
	DefaultMaxDepth = 4
	// DefaultMaxActiveCalls is the calls open at once across every scope of one
	// [Tools], 1024.
	DefaultMaxActiveCalls = 1024
	// DefaultMaxAttachments is the attachments one prompt_agent call may carry,
	// 32 (SDK policy).
	DefaultMaxAttachments = 32
)

type config struct {
	toolsGiven   bool
	toolNames    []string
	selfAddress  string
	discoverWait time.Duration
	maxWait      time.Duration
	maxWaitAgent time.Duration
	waitAgentSet bool
	maxCalls     int
	roots        []string
	stagingDir   string
	maxSaved     int64
	maxSavedSet  bool
	onSettled    func(CallResult, SettledInfo)
	extensions   []Extension
	logger       *slog.Logger

	maxReplyBytes    int
	maxQuestions     int
	maxFiles         int
	maxResultBytes   int
	maxDepth         int
	maxAttachments   int
	maxActive        int
	replyPrefixes    []string
	replyPrefixesSet bool
}

// Option configures [New].
type Option func(*config) error

// WithTools offers only the named tools, of the six; the default is all six.
// Definitions holds only these, in the contract's order, and Execute refuses
// any other. Without wait_agent nothing can be detached: prompt_agent and
// answer_agent lose their wait parameter and refuse wait false. A set that
// makes no sense is refused by [New]: no tool at all; any tool but
// discover_agents without prompt_agent, which starts the calls they work on;
// prompt_agent without answer_agent, since a question the prompted agent asks
// would reach a model with no way to answer it. [BlockingTools] is the usual
// short set.
func WithTools(names ...string) Option {
	return func(c *config) error {
		c.toolsGiven = true
		c.toolNames = append([]string(nil), names...)
		return nil
	}
}

// WithSelfAddress sets the agent's own prompt subject: it is left out of
// discovery and prompt_agent refuses it (the loop guard of the contract's
// section 3.8).
func WithSelfAddress(addr string) Option {
	return func(c *config) error { c.selfAddress = addr; return nil }
}

// WithDiscoverTimeout sets how long one discovery waits (the caller's
// WithDiscoverTimeout strategy). Unset, the caller's discovery default
// applies (SAP §4.1).
func WithDiscoverTimeout(d time.Duration) Option {
	return func(c *config) error {
		if d <= 0 {
			return fmt.Errorf("tools: discover timeout must be greater than 0 (got %v)", d)
		}
		c.discoverWait = d
		return nil
	}
}

// WithMaxWait sets the runtime limit per call; past it the call is expired.
// The default is [caller.DefaultMaxWait], 10 minutes. This is the contract's
// maxWaitMs: a limit on how long a call runs, not on one tool call.
func WithMaxWait(d time.Duration) Option {
	return func(c *config) error {
		if d <= 0 {
			return fmt.Errorf("tools: max wait must be greater than 0 (got %v)", d)
		}
		c.maxWait = d
		return nil
	}
}

// WithMaxWaitAgent sets the cap on wait_agent's timeout_ms, which is also its
// default. Zero makes wait_agent only poll. The default is the runtime limit
// of [WithMaxWait].
func WithMaxWaitAgent(d time.Duration) Option {
	return func(c *config) error {
		if d < 0 {
			return fmt.Errorf("tools: max wait_agent must be 0 or more (got %v)", d)
		}
		c.maxWaitAgent, c.waitAgentSet = d, true
		return nil
	}
}

// WithMaxCalls sets how many calls are tracked per scope; the default is
// [DefaultMaxCalls].
func WithMaxCalls(n int) Option {
	return func(c *config) error {
		if n <= 0 {
			return fmt.Errorf("tools: max calls must be greater than 0 (got %d)", n)
		}
		c.maxCalls = n
		return nil
	}
}

// WithAttachmentRoots names directories files may be sent from, besides the
// staging directory, which always is one. A path is checked after its links
// are followed by the root handle pinned when the helper is made, and a relative root is taken from the working directory when
// the helper is made. By default there are none, so only returned files can
// be sent (the contract's section 3.9).
func WithAttachmentRoots(roots ...string) Option {
	return func(c *config) error {
		for _, r := range roots {
			if r == "" {
				return fmt.Errorf("tools: an attachment root must not be empty")
			}
		}
		c.roots = append(c.roots, roots...)
		return nil
	}
}

// WithStagingDir sets where returned files are saved, one directory per call.
// A relative path is taken from the working directory when the helper is
// made. The default is a new private directory under the system's temporary
// directory, removed by Close; a directory given here is kept.
func WithStagingDir(dir string) Option {
	return func(c *config) error {
		if dir == "" {
			return fmt.Errorf("tools: the staging directory must not be empty")
		}
		c.stagingDir = dir
		return nil
	}
}

// WithMaxSavedBytesPerCall sets the total of returned files saved per call;
// the default is [DefaultMaxSavedBytesPerCall]. Zero saves nothing.
func WithMaxSavedBytesPerCall(n int64) Option {
	return func(c *config) error {
		if n < 0 {
			return fmt.Errorf("tools: max saved bytes per call must be 0 or more (got %d)", n)
		}
		c.maxSaved, c.maxSavedSet = n, true
		return nil
	}
}

// WithOnSettled sets a function called for every call that finishes outside a
// served prompt, with its result and whether a tool call was waiting for it
// as it finished. It is a signal only: the tracked result is authoritative and
// wait_agent returns it. A panic in f is recovered and logged.
func WithOnSettled(f func(CallResult, SettledInfo)) Option {
	return func(c *config) error { c.onSettled = f; return nil }
}

// WithExtensions adds extensions, run in the order given.
func WithExtensions(e ...Extension) Option {
	return func(c *config) error {
		for _, x := range e {
			if x == nil {
				return fmt.Errorf("tools: a nil extension")
			}
		}
		c.extensions = append(c.extensions, e...)
		return nil
	}
}

// WithLogger sets the logger. The model is told only stable messages; the
// detail behind one (a transport error, a file system error that may name a
// local path) goes here, at the warn level, and so do a failing OnSettled
// function and an extension's bug in a reply look. A nil logger, the default,
// is silent. No prompt or reply text and no credential is logged.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) error { c.logger = l; return nil }
}

func positive(name string, n int) error {
	if n <= 0 {
		return fmt.Errorf("tools: %s must be greater than 0 (got %d)", name, n)
	}
	return nil
}

// WithMaxReplyBytes sets the reply text kept per call; the default is
// [DefaultMaxReplyBytes]. A call whose agent streams more is failed and its
// stream closed: a peer's stream is otherwise unbounded (SAP §6).
func WithMaxReplyBytes(n int) Option {
	return func(c *config) error { c.maxReplyBytes = n; return positive("max reply bytes", n) }
}

// WithMaxQuestions sets how many questions one call may ask in all; the
// default is [DefaultMaxQuestions]. A call that asks more is failed and its
// stream closed.
func WithMaxQuestions(n int) Option {
	return func(c *config) error { c.maxQuestions = n; return positive("max questions", n) }
}

// WithMaxReturnedFiles sets how many returned-file records one call may
// collect; the default is [DefaultMaxReturnedFiles]. A call that is sent more
// is failed and its stream closed, and none of the files of the offending
// message is saved.
func WithMaxReturnedFiles(n int) Option {
	return func(c *config) error { c.maxFiles = n; return positive("max returned files", n) }
}

// WithMaxResultBytes sets the total one call may hold for its result: reply
// text, question texts and returned-file records; the default is
// [DefaultMaxResultBytes]. A call that passes it is failed and its stream
// closed.
func WithMaxResultBytes(n int) Option {
	return func(c *config) error { c.maxResultBytes = n; return positive("max result bytes", n) }
}

// WithMaxDepth sets how many delegation hops a prompt may go: a prompt the
// program serves at depth d may start tool prompts only while d is less than n.
// The default is [DefaultMaxDepth]. The depth travels in the Agent-Tools-Depth
// header (see [DepthHeader], [DepthInterceptor]), which is advisory between
// cooperating SDK instances: a hostile peer can reset or omit it, so
// [WithMaxActiveCalls] is the hard bound.
func WithMaxDepth(n int) Option {
	return func(c *config) error { c.maxDepth = n; return positive("max depth", n) }
}

// WithMaxAttachments sets how many attachments one prompt_agent call may list;
// the default is [DefaultMaxAttachments]. A longer list is refused, with a
// message to the model, before any file is read. Whatever the count, the files'
// total size is also held to the target's max_payload (SAP §2.1, §5.2) before
// any is read.
func WithMaxAttachments(n int) Option {
	return func(c *config) error { c.maxAttachments = n; return positive("max attachments", n) }
}

// WithMaxActiveCalls sets the calls open at once across every scope of the
// helper; the default is [DefaultMaxActiveCalls]. It is the hard bound on a
// delegation cycle, which a peer can keep alive by resetting the depth header.
func WithMaxActiveCalls(n int) Option {
	return func(c *config) error { c.maxActive = n; return positive("max active calls", n) }
}

// WithAnswerReplyPrefixes sets the subject prefixes an answer (and the refusal
// of a question) may be published under. A question names its own reply
// subject (SAP §7.1), so a hostile agent could aim the model's answer at any
// subject the connection may publish to; answer_agent refuses, with a result
// and publishing nothing, a question whose reply subject is not under one of
// these prefixes. A prefix matches whole tokens: "_INBOX" and "_INBOX." both
// match "_INBOX.x.y". Given, the list replaces the default: "_INBOX." plus the
// connection's own inbox prefix when it set one. Grant the connection publish
// permission on those prefixes only; that is the other half of the defense.
// SDK policy.
func WithAnswerReplyPrefixes(prefixes ...string) Option {
	return func(c *config) error {
		if len(prefixes) == 0 {
			return fmt.Errorf("tools: WithAnswerReplyPrefixes needs at least one prefix")
		}
		norm := make([]string, 0, len(prefixes))
		for _, p := range prefixes {
			n, err := normalizePrefix(p)
			if err != nil {
				return err
			}
			norm = append(norm, n)
		}
		c.replyPrefixes, c.replyPrefixesSet = norm, true
		return nil
	}
}

// normalizePrefix returns prefix with exactly one trailing dot, after checking
// that it is a valid subject.
func normalizePrefix(p string) (string, error) {
	p = strings.TrimSuffix(p, ".")
	if err := agents.ValidatePublishSubject(p); err != nil {
		return "", fmt.Errorf("tools: reply prefix %q is not a valid subject prefix: %w", p, err)
	}
	return p + ".", nil
}

// offeredTools validates the tool set and returns it as a set. Messages follow
// the official SDKs.
func offeredTools(c *config) (map[string]bool, error) {
	all := ToolNames()
	if !c.toolsGiven {
		m := make(map[string]bool, len(all))
		for _, n := range all {
			m[n] = true
		}
		return m, nil
	}
	valid := map[string]bool{}
	for _, n := range all {
		valid[n] = true
	}
	for _, n := range c.toolNames {
		if !valid[n] {
			return nil, fmt.Errorf("tools: WithTools names %q, which is not one of %s", n, strings.Join(all, ", "))
		}
	}
	given := map[string]bool{}
	for _, n := range c.toolNames {
		given[n] = true
	}
	offered := map[string]bool{}
	var orphans []string
	for _, n := range all {
		if given[n] {
			offered[n] = true
			if n != ToolDiscoverAgents {
				orphans = append(orphans, n)
			}
		}
	}
	if len(offered) == 0 {
		return nil, fmt.Errorf("tools: WithTools names no tool; a role that must not delegate needs no tools")
	}
	if !offered[ToolPromptAgent] {
		withoutPrompt := orphans
		if len(withoutPrompt) > 0 {
			which := "it works on"
			if len(withoutPrompt) > 1 {
				which = "they work on"
			}
			return nil, fmt.Errorf("tools: WithTools offers %s without prompt_agent, which starts the calls %s",
				strings.Join(withoutPrompt, ", "), which)
		}
	}
	if offered[ToolPromptAgent] && !offered[ToolAnswerAgent] {
		return nil, fmt.Errorf("tools: WithTools offers prompt_agent without answer_agent, so a question the " +
			"prompted agent asks would reach a model with no way to answer it, and the asking agent would wait out its timeout")
	}
	return offered, nil
}
