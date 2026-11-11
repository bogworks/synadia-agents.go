// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
)

// ErrClosed is returned by [Tools.Execute] and [Tools.RunInPromptScope] after
// [Tools.Close]. It is the only Go error a tool call returns: everything the
// model can act on comes back as a result in words.
var ErrClosed = errors.New("tools: the helper is closed")

// CallState is a call's state (docs/agent-tools.md section 1.1). A finished
// state is final.
type CallState string

// The six call states.
const (
	// StateRunning: the agent works on it. Open.
	StateRunning CallState = "running"
	// StateInputRequired: the agent asked a question (SAP §7) that waits for
	// answer_agent. Open.
	StateInputRequired CallState = "input_required"
	// StateCompleted: the stream ended with its terminator (SAP §6.5).
	StateCompleted CallState = "completed"
	// StateFailed: no reply came: the prompt was refused (SAP §9), the stream
	// stalled (SAP §6.6), or the transport failed.
	StateFailed CallState = "failed"
	// StateCanceled: cancel_agent stopped it, or the prompt it belonged to ended.
	StateCancelled CallState = "cancelled" //nolint:misspell // the wire value of the state
	// StateExpired: it ran past its runtime limit.
	StateExpired CallState = "expired"
)

func (s CallState) open() bool { return s == StateRunning || s == StateInputRequired }

// Result is a tool's result: a JSON-serializable object the program hands its
// model as JSON text. A refusal is {"error": "..."} alone. The shapes are the
// result schemas of the language-neutral fixtures. Values are plain Go values
// (string, int, bool, []ReturnedFile, []map[string]any, ...) that
// encoding/json marshals as the schemas require.
type Result = map[string]any

// CallResult is one call's result as prompt_agent, answer_agent, wait_agent
// and the OnSettled function give it: call_id and state, label when the call
// has one, and the fields its state carries.
type CallResult map[string]any

// CallID returns the call's id, "" if the result has none.
func (r CallResult) CallID() string { s, _ := r["call_id"].(string); return s }

// State returns the call's state, "" if the result has none.
func (r CallResult) State() CallState { s, _ := r["state"].(string); return CallState(s) }

// ReturnedFile is a file the prompted agent sent, as a result lists it. The
// bytes never enter the result. See SAP §6.3 and §7.1 for where files arrive.
type ReturnedFile struct {
	// Filename is the name as the sender gave it.
	Filename string `json:"filename"`
	// SizeBytes is the decoded size; 0 when the content was not valid base64.
	SizeBytes int64 `json:"size_bytes"`
	// Path is the absolute path of the saved file; nil when it was not saved.
	Path *string `json:"path"`
	// Skipped says why the file was not saved ("over_limit" or
	// "invalid_content"); it is set iff Path is nil.
	Skipped string `json:"skipped,omitempty"`
}

// SettledInfo is what [WithOnSettled] is told besides the result.
type SettledInfo struct {
	// Awaited is true when a tool call was waiting for this call as it
	// finished, so its result went to the model already.
	Awaited bool
}

// ExecuteOption configures one [Tools.Execute].
type ExecuteOption func(*executeConfig)

type executeConfig struct{ toolCallID string }

// WithToolCallID passes the model's id for this tool call. prompt_agent makes
// it available to the client's prompt interceptors through
// [ToolCallIDFromContext]; it is never sent on the wire.
func WithToolCallID(id string) ExecuteOption {
	return func(c *executeConfig) { c.toolCallID = id }
}

type ctxKey int

const (
	scopeKey ctxKey = iota
	toolCallIDKey
	promptContextKey
)

// ToolCallIDFromContext returns the model's tool-call id that prompt_agent put
// in the context it passes to the client's prompt interceptors (SAP §5.6 lets
// an interceptor add envelope fields; the id itself is never sent). It is the
// Go form of the official SDKs' toolCallId in the prompt context.
func ToolCallIDFromContext(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(toolCallIDKey).(string)
	return s, ok
}

// PromptContextFromContext returns the values a [PromptRewrite] added with its
// Context field, as seen by a prompt interceptor. They are never sent.
func PromptContextFromContext(ctx context.Context) map[string]any {
	m, _ := ctx.Value(promptContextKey).(map[string]any)
	return m
}

// Tools is the AgentTools helper: the definitions a model is shown and the
// execution of a tool call by name, over a [caller.Client]. It adds nothing
// to the wire: the agent being prompted sees an ordinary prompt from an
// ordinary caller (SAP §5). Safe for concurrent use.
//
// A call is one prompt. Every call, blocking or not, is read by a goroutine of
// its own from the moment it is sent: it collects the text, saves returned
// files and queues questions, so its acks keep the stream alive while nobody
// waits and the reply is complete when the model asks for it. A blocking tool
// call only waits for that goroutine to reach a state other than running.
type Tools struct {
	b       backend
	cfg     config
	offered map[string]bool
	defs    []Definition
	cwd     string
	roots   []string
	staging string
	logger  *slog.Logger

	base       context.Context
	baseCancel context.CancelFunc
	wg         sync.WaitGroup

	mu      sync.Mutex
	changed chan struct{} // closed and replaced on every change of any call
	root    *scope
	served  map[*scope]struct{}
	known   map[string]target
	seq     uint64
	closed  bool
	active  int // calls open or being started, across every scope

	replyPrefixes []string
	// afterAttachmentCheck is a test hook: it runs after an attachment passed
	// its path check and before the file is opened.
	afterAttachmentCheck func(path string)
	// beforePublish is a test hook: it runs just before a prompt's closed-check
	// and publication.
	beforePublish func(sc *scope)

	rootsMu     sync.Mutex
	rootsClosed bool
	rootHandles map[string]*os.Root // allowed roots, opened once and held until Close

	stagingMu   sync.Mutex
	stagingPath string
	ownsStaging bool
}

// New returns the helper over client. Nothing is sent until a tool is
// executed. It refuses a nonsensical tool set (see [WithTools]).
func New(client *caller.Client, opts ...Option) (*Tools, error) {
	if client == nil {
		return nil, errors.New("tools: the caller client is required")
	}
	return newTools(clientBackend{client}, opts...)
}

func newTools(b backend, opts ...Option) (*Tools, error) {
	var cfg config
	for _, o := range opts {
		if err := o(&cfg); err != nil {
			return nil, err
		}
	}
	offered, err := offeredTools(&cfg)
	if err != nil {
		return nil, err
	}
	defs, err := offeredDefinitions(offered)
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("tools: working directory: %w", err)
	}
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
		return filepath.Join(cwd, p)
	}
	if cfg.maxWait == 0 {
		cfg.maxWait = caller.DefaultMaxWait
	}
	if !cfg.waitAgentSet {
		cfg.maxWaitAgent = cfg.maxWait
	}
	if cfg.maxCalls == 0 {
		cfg.maxCalls = DefaultMaxCalls
	}
	for _, d := range []struct {
		v   *int
		def int
	}{
		{&cfg.maxReplyBytes, DefaultMaxReplyBytes}, {&cfg.maxQuestions, DefaultMaxQuestions},
		{&cfg.maxFiles, DefaultMaxReturnedFiles}, {&cfg.maxResultBytes, DefaultMaxResultBytes},
		{&cfg.maxDepth, DefaultMaxDepth}, {&cfg.maxAttachments, DefaultMaxAttachments}, {&cfg.maxActive, DefaultMaxActiveCalls},
	} {
		if *d.v == 0 {
			*d.v = d.def
		}
	}
	if !cfg.maxSavedSet {
		cfg.maxSaved = DefaultMaxSavedBytesPerCall
	}
	t := &Tools{
		b: b, cfg: cfg, offered: offered, defs: defs, cwd: cwd,
		logger:  cfg.logger,
		changed: make(chan struct{}),
		root:    newScope(false, ""),
		served:  map[*scope]struct{}{},
		known:   map[string]target{},
	}
	if t.logger == nil {
		t.logger = slog.New(slog.DiscardHandler)
	}
	for _, r := range cfg.roots {
		t.roots = append(t.roots, abs(r))
	}
	if cfg.stagingDir != "" {
		t.staging = abs(cfg.stagingDir)
	}
	// Pin every allowed root now, before any model path is checked; a root that
	// does not exist yet is pinned on first use, still before the check.
	for _, r := range t.roots {
		if _, err := t.openRoot(r); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.closeRoots()
			return nil, fmt.Errorf("tools: attachment root: %w", err)
		}
	}
	if cfg.replyPrefixesSet {
		t.replyPrefixes = cfg.replyPrefixes
	} else {
		t.replyPrefixes = []string{"_INBOX."}
		for _, p := range b.inboxPrefixes() {
			if n, err := normalizePrefix(p); err == nil {
				t.replyPrefixes = append(t.replyPrefixes, n)
			}
		}
	}
	t.base, t.baseCancel = context.WithCancel(context.Background())
	return t, nil
}

// Definitions returns the definitions of the tools offered, in the contract's
// order, as a fresh copy. Without wait_agent each loses its wait parameter and
// a description that mentions wait_agent is replaced by its blocking-only
// words (the vendored blocking.json); nothing else changes.
func (t *Tools) Definitions() []Definition {
	out := make([]Definition, len(t.defs))
	for i, d := range t.defs {
		d.Parameters = append([]byte(nil), d.Parameters...)
		out[i] = d
	}
	return out
}

// scope is where calls live: one per served prompt, and one for everything else.
type scope struct {
	served   bool
	depth    int // delegation depth of the served prompt; 0 outside one
	caller   string
	closed   bool
	reserved int
	// pubMu serializes closure with publication: a prompt is published only
	// while holding it for reading, after seeing the scope open; cancelScope
	// marks the scope closed and then takes it for writing, so it waits for the
	// publications in progress and none starts after.
	pubMu sync.RWMutex
	calls map[string]*call
	order []*call // in the order started
}

func newScope(served bool, callerID string) *scope {
	return &scope{served: served, caller: callerID, calls: map[string]*call{}}
}

type scopeRef struct {
	owner *Tools
	s     *scope
}

func (t *Tools) scopeFrom(ctx context.Context) *scope {
	if r, ok := ctx.Value(scopeKey).(*scopeRef); ok && r.owner == t {
		return r.s
	}
	return t.root
}

type openQuestion struct {
	q      *question
	files  []ReturnedFile
	fields map[string]any
}

// call is one prompt and what came back so far. Its fields are guarded by Tools.mu.
type call struct {
	id       string
	scope    *scope
	address  string
	target   target
	label    string
	hasLabel bool
	detached bool
	fields   map[string]any
	stream   callStream
	cancel   context.CancelFunc

	state       CallState
	seq         uint64
	startedAt   time.Time
	endedAt     time.Time
	text        strings.Builder
	replyFiles  []ReturnedFile
	questions   []*openQuestion
	arriving    *question
	errText     string
	replyFields map[string]any
	savedBytes  int64
	waiters     int

	// what the call holds, against the limits of the options
	retained    int // bytes: reply text, question texts, file records
	fileRecords int
	asked       int
}

func (c *call) open() bool { return c.state.open() }

// wake tells every waiting tool call that something changed. Callers hold t.mu.
func (t *Tools) wake() {
	close(t.changed)
	t.changed = make(chan struct{})
}

// Execute runs one tool call from the model and returns its result, which the
// program hands back as JSON text (json.Marshal(result)). args is the call's
// arguments as the model produced them: a JSON object, or JSON text holding
// one; none counts as {}. A tool the helper does not offer is refused like any
// other mistake. The only error is [ErrClosed]: a refusal is a result.
//
// The call runs in the scope ctx carries ([Tools.Interceptor],
// [Tools.RunInPromptScope]), else in the helper's shared scope. Canceling ctx
// cancels the call a blocking prompt_agent or answer_agent waits for, and ends
// a wait_agent wait without touching the calls (the contract's section 3.10).
func (t *Tools) Execute(ctx context.Context, name string, args json.RawMessage, opts ...ExecuteOption) (Result, error) {
	var ec executeConfig
	for _, o := range opts {
		o(&ec)
	}
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	sc := t.scopeFrom(ctx)
	if isToolName(name) && !t.offered[name] {
		return t.withOpenCalls(sc, Result{"error": fmt.Sprintf("the tool %q is not offered; your tools are %s", name, strings.Join(t.offeredNames(), ", "))}), nil
	}
	var res Result
	switch name {
	case ToolDiscoverAgents:
		res = t.discoverAgents(ctx, args)
	case ToolPromptAgent:
		res = t.promptAgent(ctx, sc, args, ec)
	case ToolWaitAgent:
		res = t.waitAgent(ctx, sc, args)
	case ToolAnswerAgent:
		res = t.answerAgent(ctx, sc, args)
	case ToolCancelAgent:
		res = t.cancelAgent(sc, args)
	case ToolListAgentCalls:
		res = t.listAgentCalls(sc, args)
	default:
		res = Result{"error": fmt.Sprintf("unknown tool %q", name)}
	}
	return t.withOpenCalls(sc, res), nil
}

func isToolName(n string) bool {
	for _, k := range ToolNames() {
		if k == n {
			return true
		}
	}
	return false
}

func (t *Tools) offeredNames() []string {
	var out []string
	for _, n := range ToolNames() {
		if t.offered[n] {
			out = append(out, n)
		}
	}
	return out
}

// Interceptor returns the host interceptor that opens a scope around each
// served prompt: tool calls made from the handler (with the ctx the handler
// got) belong to the prompt, and when the handler returns the open calls are
// canceled and their questions refused (the contract's section 3.3). The
// caller guard uses the sender's id when its signature verified (SAP §13.7).
// Add it with host.WithInterceptors.
func (t *Tools) Interceptor() host.Interceptor { return scopeInterceptor{t} }

type scopeInterceptor struct{ t *Tools }

func (i scopeInterceptor) AroundRequest(ctx context.Context, info *host.RequestInfo, next host.CallNext) error {
	t := i.t
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return next(ctx) // a closed helper must not keep an agent from serving
	}
	callerID := ""
	if info != nil && info.Sender != nil && info.Sender.Trust == identity.TrustVerified {
		callerID = string(info.Sender.ID)
	}
	if info != nil {
		ctx = ContextWithDepth(ctx, DepthFromHeader(info.Header))
	}
	return t.RunInPromptScope(ctx, callerID, next)
}

// RunInPromptScope runs fn as the handler of a served prompt, for hosts that
// do not use [Tools.Interceptor]: tool calls made with the ctx fn receives
// share a scope of their own, and when fn returns the calls still open are
// canceled and their questions refused. callerID is the served prompt's
// verified sender id, "<account>.<user>" (SAP §13.3), for the loop guard;
// leave it empty for an unsigned or merely claimed sender. The prompt's
// delegation depth is the one [ContextWithDepth] put in ctx, 0 without one;
// the tool prompts of this scope carry it plus one, and are refused once it
// reaches [WithMaxDepth].
func (t *Tools) RunInPromptScope(ctx context.Context, callerID string, fn func(ctx context.Context) error) error {
	s := newScope(true, callerID)
	s.depth, _ = depthFromContext(ctx)
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return ErrClosed
	}
	t.served[s] = struct{}{}
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.served, s)
		t.mu.Unlock()
		t.closeScope(s)
	}()
	return fn(context.WithValue(ctx, scopeKey, &scopeRef{t, s}))
}

// Close cancels every open call in every scope, waits for the calls' readers
// to end, and removes the staging directory when the helper created it. It
// does not close the caller client. It is idempotent. Calls are canceled at
// once; the refusals of their open questions go out concurrently, and ctx
// bounds the whole operation, refusals included.
func (t *Tools) Close(ctx context.Context) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	scopes := []*scope{t.root}
	for s := range t.served {
		scopes = append(scopes, s)
	}
	t.mu.Unlock()
	var qs []*question
	for _, s := range scopes {
		qs = append(qs, t.cancelScope(s)...)
	}
	t.baseCancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		t.refuseAll(ctx, qs)
		t.wg.Wait()
	}()
	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	t.closeRoots()
	t.stagingMu.Lock()
	if t.ownsStaging && t.stagingPath != "" {
		if rerr := os.RemoveAll(t.stagingPath); rerr != nil && err == nil {
			err = rerr
		}
	}
	t.stagingMu.Unlock()
	return err
}

// cancelScope closes a scope and cancels its calls; it returns their open
// questions, which the caller refuses.
func (t *Tools) cancelScope(s *scope) []*question {
	t.mu.Lock()
	s.closed = true
	calls := append([]*call(nil), s.order...)
	t.mu.Unlock()
	s.pubMu.Lock()   // waits for a publication that began before the closure
	s.pubMu.Unlock() //nolint:staticcheck // SA2001: the empty section is the barrier
	var qs []*question
	for _, c := range calls {
		qs = append(qs, t.stopCall(c)...)
	}
	return qs
}

func (t *Tools) closeScope(s *scope) { t.refuseAll(context.Background(), t.cancelScope(s)) }

// --- discover_agents ---------------------------------------------------------

func (t *Tools) discoverAgents(ctx context.Context, raw json.RawMessage) Result {
	a, err := parseDiscoverArgs(raw)
	if err != nil {
		return Result{"error": err.Error()}
	}
	found, err := t.discover(ctx, caller.Filter{Agent: a.agent, Owner: a.owner, Name: a.name})
	if err != nil {
		t.logger.Warn("tools: discovery failed", "error", err)
		return Result{"error": "discovery failed; no list of agents is available right now"}
	}
	// Instances of one agent share its address and SHOULD register alike
	// (SAP §3.4). When they do not, the entry shows one whose identity verifies.
	type group struct {
		h         target
		verified  bool
		instances int
	}
	var order []string
	byAddr := map[string]*group{}
	for _, h := range found {
		addr := h.promptSubject()
		if addr == t.cfg.selfAddress {
			continue
		}
		v := verifiedIdentity(h)
		if g, ok := byAddr[addr]; !ok {
			byAddr[addr] = &group{h, v, 1}
			order = append(order, addr)
		} else {
			g.instances++
			if !g.verified && v {
				g.h, g.verified = h, true
			}
		}
	}
	entries := make([]map[string]any, 0, len(order))
	for _, addr := range order {
		g := byAddr[addr]
		info := g.h.info()
		extra := map[string]any{}
		for _, e := range t.cfg.extensions {
			df, ok := e.(DiscoveryFields)
			if !ok {
				continue
			}
			add, err := df.DiscoveryFields(ctx, g.h.handle())
			if err != nil {
				t.logger.Warn("tools: an extension failed on a discovery entry", "extension", e.Name(), "error", err)
				return Result{"error": fmt.Sprintf("extension %s failed on a discovery entry", e.Name())}
			}
			if err := addFields(extra, add, discoveryEntryFields, e.Name(), "a discovery entry"); err != nil {
				return Result{"error": err.Error()}
			}
		}
		var ident any
		if id, ok := registeredIdentity(info); ok {
			ident = id
		}
		accepts := true
		if v, ok := info.Prompt.AttachmentsOK(); ok && !v {
			accepts = false // as the SDK reads it: only an agent that says false refuses files
		}
		entry := map[string]any{
			"address":                 addr,
			"agent":                   info.Agent,
			"owner":                   info.Owner,
			"name":                    info.Name,
			"description":             info.Description,
			"identity":                ident,
			"identity_verified":       g.verified,
			"requires_signed_prompts": g.h.minSenderTrust() == identity.TrustSigned,
			"accepts_attachments":     accepts,
			"instances":               g.instances,
		}
		for k, v := range extra {
			entry[k] = v
		}
		entries = append(entries, entry)
	}
	return Result{"agents": entries}
}

func registeredIdentity(info agents.AgentInfo) (string, bool) {
	acct, user := info.Metadata[identity.MetadataKeyAccount], info.Metadata[identity.MetadataKeyUserNKey]
	if acct == "" || user == "" {
		return "", false
	}
	id, err := identity.NewAgentID(acct, user)
	if err != nil {
		return "", false
	}
	return string(id), true
}

func verifiedIdentity(h target) bool {
	return identity.VerifyAgentID(h.info().Metadata, h.promptSubject())
}

// discover runs a discovery and remembers the handle behind each address.
func (t *Tools) discover(ctx context.Context, f caller.Filter) ([]target, error) {
	found, err := t.b.discover(ctx, f, t.cfg.discoverWait)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, h := range found {
		addr := h.promptSubject()
		k, ok := t.known[addr]
		if !ok || verifiedIdentity(h) || !verifiedIdentity(k) {
			t.known[addr] = h
		}
	}
	return found, nil
}

// lookup returns the handle for an address: from an earlier discovery, else
// from a fresh one.
func (t *Tools) lookup(ctx context.Context, address string) (target, error) {
	t.mu.Lock()
	h, ok := t.known[address]
	t.mu.Unlock()
	if ok {
		return h, nil
	}
	if _, err := t.discover(ctx, caller.Filter{}); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.known[address], nil
}

// --- prompt_agent ------------------------------------------------------------

const scopeEnded = "the prompt you were answering has ended; no call can start in it"

func (t *Tools) promptAgent(ctx context.Context, sc *scope, raw json.RawMessage, ec executeConfig) Result {
	a, err := parsePromptArgs(raw)
	if err != nil {
		return Result{"error": err.Error()}
	}
	if !a.wait && !t.offered[ToolWaitAgent] {
		return Result{"error": `prompt_agent: "wait" cannot be false here; the call waits for the reply or a question`}
	}
	t.mu.Lock()
	scopeClosed := sc.closed
	t.mu.Unlock()
	if scopeClosed {
		return Result{"error": scopeEnded}
	}
	if a.address == t.cfg.selfAddress {
		return Result{"error": fmt.Sprintf("%q is your own address; answer the prompt yourself", a.address)}
	}
	if sc.depth >= t.cfg.maxDepth {
		return Result{"error": fmt.Sprintf("this prompt was already handed on %d times (the limit); answer it yourself rather than prompting another agent", sc.depth)}
	}
	tgt, err := t.lookup(ctx, a.address)
	if err != nil {
		t.logger.Warn("tools: agent lookup failed", "address", a.address, "error", err)
		return Result{"error": fmt.Sprintf("could not look up %q; the agents could not be listed right now", a.address)}
	}
	if tgt == nil {
		return Result{"error": t.noAgentAt(a.address, "")}
	}
	if sc.caller != "" {
		if id, ok := registeredIdentity(tgt.info()); ok && id == sc.caller {
			return Result{"error": fmt.Sprintf("the agent at %q sent the prompt you are answering; answer it rather than prompting it back", a.address)}
		}
	}
	budget, err := t.attachmentBudget(tgt, len(a.attachments))
	if err != nil {
		return Result{"error": err.Error()}
	}
	refs, err := t.resolveAttachments(a.attachments)
	if err != nil {
		return Result{"error": err.Error()}
	}
	paths := make([]string, len(refs))
	for i, r := range refs {
		paths[i] = r.real
	}
	switch t.reserve(sc) {
	case reserveBudget:
		return Result{"error": fmt.Sprintf("too many calls are open across all prompts (the limit is %d); try again when some have finished", t.cfg.maxActive)}
	case reserveScopeFull:
		collect, stop := t.openCallActions("some", "their")
		advice := collect
		if stop != "" {
			advice = collect + " or " + stop
		}
		return Result{"error": fmt.Sprintf("all %d tracked calls are still open; %s before you start another", t.cfg.maxCalls, advice)}
	}
	reserved := true
	defer func() {
		if reserved {
			t.mu.Lock()
			sc.reserved--
			t.active--
			t.mu.Unlock()
		}
	}()

	callID := newCallID()
	text := a.prompt
	pctx := map[string]any{}
	extra := map[string]any{}
	label := ""
	if a.label != nil {
		label = *a.label
	}
	for _, e := range t.cfg.extensions {
		rw, ok := e.(PromptRewriter)
		if !ok {
			continue
		}
		out, err := rw.RewritePrompt(ctx, &PromptContext{
			Address: a.address, Prompt: text, Attachments: paths, Target: tgt.handle(),
			CallID: callID, Label: label, ToolCallID: ec.toolCallID, Caller: sc.caller,
		})
		if err != nil {
			t.logger.Warn("tools: an extension failed on the prompt", "extension", e.Name(), "error", err)
			return Result{"error": fmt.Sprintf("extension %s failed on the prompt", e.Name())}
		}
		if out == nil {
			continue
		}
		if out.Error != "" {
			return Result{"error": out.Error}
		}
		if out.Prompt != nil {
			text = *out.Prompt
		}
		for k, v := range out.Context {
			pctx[k] = v
		}
		if err := addFields(extra, out.Fields, callResultFields, e.Name(), "a call's result"); err != nil {
			return Result{"error": err.Error()}
		}
	}
	env := agents.Envelope{Prompt: text}
	if env.Attachments, err = t.openAttachments(refs, budget); err != nil {
		return Result{"error": err.Error()}
	}

	callCtx, cancel := context.WithCancel(t.base)
	sctx := context.WithValue(callCtx, promptContextKey, pctx)
	if ec.toolCallID != "" {
		sctx = context.WithValue(sctx, toolCallIDKey, ec.toolCallID)
	}
	if sc.served {
		sctx = context.WithValue(sctx, outgoingDepthKey{}, sc.depth+1)
	}
	if t.beforePublish != nil {
		t.beforePublish(sc)
	}
	sc.pubMu.RLock()
	t.mu.Lock()
	ended := sc.closed || t.closed
	t.mu.Unlock()
	if ended {
		sc.pubMu.RUnlock()
		cancel()
		return Result{"error": scopeEnded}
	}
	stream, err := tgt.prompt(sctx, env, promptParams{maxWait: t.cfg.maxWait, replyPolicy: t.replyAllowed})
	sc.pubMu.RUnlock()
	if err != nil {
		cancel()
		return Result{"error": t.notSent(a.address, err)}
	}
	c := &call{
		id: callID, scope: sc, address: a.address, target: tgt, label: label,
		hasLabel: a.label != nil, detached: !a.wait, fields: extra, stream: stream,
		cancel: cancel, state: StateRunning, startedAt: time.Now().UTC(),
	}
	t.mu.Lock()
	if sc.closed || t.closed {
		t.mu.Unlock()
		stream.close()
		cancel()
		return Result{"error": scopeEnded}
	}
	sc.reserved--
	reserved = false
	sc.calls[callID] = c
	sc.order = append(sc.order, c)
	t.wg.Add(1)
	t.mu.Unlock()
	go t.read(c) //nolint:gosec // the reader outlives this tool call by design; it ends with its stream, or at Close

	if !a.wait {
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.callResultLocked(c)
	}
	return t.untilReady(ctx, c)
}

type reserveResult int

const (
	reserveOK reserveResult = iota
	reserveScopeFull
	reserveBudget
)

// reserve makes room for one more call in sc: it drops finished calls, the one
// that finished longest ago first. It refuses when every tracked call of the
// scope is open, and when the helper as a whole has [WithMaxActiveCalls] open.
func (t *Tools) reserve(sc *scope) reserveResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active >= t.cfg.maxActive {
		return reserveBudget
	}
	for len(sc.calls)+sc.reserved >= t.cfg.maxCalls {
		var oldest *call
		for _, c := range sc.order {
			if !c.open() && (oldest == nil || c.seq < oldest.seq) {
				oldest = c
			}
		}
		if oldest == nil {
			return reserveScopeFull
		}
		delete(sc.calls, oldest.id)
		for i, c := range sc.order {
			if c == oldest {
				sc.order = append(sc.order[:i], sc.order[i+1:]...)
				break
			}
		}
	}
	sc.reserved++
	t.active++
	return reserveOK
}

// notSent words a prompt the caller refused or could not send. A refusal the
// SDK made itself (agents.ErrLocal) is in words the model can act on; anything
// else is a transport matter and stays in the log.
func (t *Tools) notSent(address string, err error) string {
	if errors.Is(err, agents.ErrLocal) {
		return "the prompt was not sent: " + err.Error()
	}
	t.logger.Warn("tools: a prompt was not sent", "address", address, "error", err)
	if isNoResponders(err) {
		return "the prompt was not sent: " + t.noAgentAt(address, "")
	}
	return "the prompt was not sent; the agent could not be reached"
}

// attachmentRef is an attachment that passed its path check: real is the path
// with its links followed, root the (links followed) allowed root it is under,
// and rel the path of the file inside that root.
type attachmentRef struct{ real, root, rel string }

// maxAttachmentFileBytes is the largest file prompt_agent reads: it cannot fit
// a NATS payload beyond 64 MiB (SAP §5.2).
const maxAttachmentFileBytes = 64 << 20

// attachmentBudget is the most attachment bytes the prompt may carry: the
// target's max_payload (SAP §2.1, §5.2), or [maxAttachmentFileBytes] when the
// record does not state a usable one (the caller then judges the envelope
// itself). It refuses, before any path is looked at, a list longer than
// [WithMaxAttachments] and any attachment sent to a target that declares
// attachments_ok=false. The model-supplied list is the untrusted input: without
// these bounds one path listed many times is read into memory that many times.
func (t *Tools) attachmentBudget(tgt target, n int) (int64, error) {
	budget := int64(maxAttachmentFileBytes)
	if n == 0 {
		return budget, nil
	}
	if n > t.cfg.maxAttachments {
		return 0, argsError(fmt.Sprintf("prompt_agent: too many attachments (%d); send at most %d", n, t.cfg.maxAttachments))
	}
	inf := tgt.info()
	if v, ok := inf.Prompt.AttachmentsOK(); ok && !v {
		return 0, argsError("prompt_agent: the agent does not accept attachments")
	}
	if mp, ok := inf.Prompt.MaxPayload(); ok && mp < budget {
		budget = mp
	}
	return budget, nil
}

// resolveAttachments checks each path: it must name a regular file under an
// allowed root. Every allowed root is pinned as an [os.Root] before any
// model-supplied path is looked at, the path is placed under a root lexically
// (after filepath.Clean), and the stat goes through that root's handle, whose
// own refusal of an escaping link is the enforcement. No model path is ever
// resolved or opened by pathname, so swapping a root directory (or a parent of
// it) for a link cannot redirect a read.
func (t *Tools) resolveAttachments(paths []string) ([]attachmentRef, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	staging, err := t.stagingDir()
	if err != nil {
		t.logger.Warn("tools: the staging directory is unavailable", "error", err)
		return nil, argsError("prompt_agent: the staging directory is unavailable")
	}
	type pinned struct {
		dir    string
		handle *os.Root
	}
	var roots []pinned
	for _, r := range append([]string{staging}, t.roots...) {
		h, err := t.openRoot(r) // before any model path is checked
		if err != nil {
			t.logger.Warn("tools: an attachment root is unavailable", "error", err)
			continue
		}
		roots = append(roots, pinned{r, h})
	}
	out := make([]attachmentRef, 0, len(paths))
	for _, p := range paths {
		abs := filepath.Clean(p)
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(t.cwd, abs)
		}
		name := filepath.Base(p) // the model's own name for it, without the directories
		var ref *attachmentRef
		var statErr error
		var st os.FileInfo
		for _, r := range roots {
			_, rel, ok := within([]string{r.dir}, abs)
			if !ok {
				continue
			}
			if st, statErr = r.handle.Stat(rel); statErr == nil {
				ref = &attachmentRef{real: abs, root: r.dir, rel: rel}
				break
			}
			if errors.Is(statErr, fs.ErrNotExist) {
				break
			}
		}
		switch {
		case ref != nil && st.Mode().IsRegular():
			out = append(out, *ref)
		case ref != nil:
			return nil, argsError(fmt.Sprintf("prompt_agent: the attachment %q is not a file", name))
		case statErr != nil && errors.Is(statErr, fs.ErrNotExist):
			return nil, argsError(fmt.Sprintf("prompt_agent: the attachment %q does not exist", name))
		default:
			return nil, argsError(fmt.Sprintf("prompt_agent: %q is outside the directories you may send files from", name))
		}
	}
	return out, nil
}

// openAttachments reads each checked file through its root: every allowed root
// is opened once as an [os.Root], the file is opened relative to it, and the
// bytes come from that same handle, so a link or directory swapped between
// the check and the open cannot lead outside the root.
//
// Before any file is read, each is sized through its root and the sum must fit
// budget, the target's max_payload; an over-size set is refused with an error
// the model can act on.
func (t *Tools) openAttachments(refs []attachmentRef, budget int64) ([]agents.Attachment, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	openRoot := func(ref attachmentRef) (*os.Root, error) { return t.openRoot(ref.root) }
	var total int64
	for _, ref := range refs {
		root, err := openRoot(ref)
		var st os.FileInfo
		if err == nil {
			st, err = root.Stat(ref.rel)
		}
		if err != nil {
			t.logger.Warn("tools: an attachment could not be sized", "error", err)
			return nil, fmt.Errorf("the prompt was not sent: the attachment %q could not be read", filepath.Base(ref.rel))
		}
		if st.Size() > budget-total || st.Size() > maxAttachmentFileBytes {
			return nil, fmt.Errorf("the prompt was not sent: the attachments are larger than the agent accepts in one prompt (limit %s); send fewer or smaller files", agents.FormatSize(budget))
		}
		total += st.Size()
	}
	out := make([]agents.Attachment, 0, len(refs))
	remaining := budget // what the files accepted so far have not used
	for _, ref := range refs {
		name := filepath.Base(ref.rel)
		fail := func(err error) ([]agents.Attachment, error) {
			t.logger.Warn("tools: an attachment could not be read", "error", err)
			return nil, fmt.Errorf("the prompt was not sent: the attachment %q could not be read", name)
		}
		root, err := openRoot(ref)
		if err != nil {
			return fail(err)
		}
		if t.afterAttachmentCheck != nil {
			t.afterAttachmentCheck(ref.real)
		}
		f, err := root.Open(ref.rel)
		if err != nil {
			return fail(err)
		}
		data, err := readRegular(f, min(remaining, maxAttachmentFileBytes))
		_ = f.Close()
		if err != nil {
			return fail(err)
		}
		remaining -= int64(len(data))
		out = append(out, agents.NewAttachment(name, data))
	}
	return out, nil
}

// closeRoots closes every pinned root and refuses further pinning.
func (t *Tools) closeRoots() {
	t.rootsMu.Lock()
	defer t.rootsMu.Unlock()
	for _, r := range t.rootHandles {
		_ = r.Close()
	}
	t.rootHandles = nil
	t.rootsClosed = true
}

// openRoot returns the [os.Root] for an allowed root, opening it the first time
// and holding it until [Tools.Close]. Every attachment is sized and read
// through it, never through a second path-based open.
func (t *Tools) openRoot(dir string) (*os.Root, error) {
	t.rootsMu.Lock()
	defer t.rootsMu.Unlock()
	if t.rootsClosed {
		return nil, errors.New("the executor is closed")
	}
	if r, ok := t.rootHandles[dir]; ok {
		return r, nil
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	if t.rootHandles == nil {
		t.rootHandles = map[string]*os.Root{}
	}
	t.rootHandles[dir] = r
	return r, nil
}

// readRegular reads all of f, which must be a regular file of at most max bytes.
func readRegular(f *os.File, max int64) ([]byte, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if st.Size() > max {
		return nil, errors.New("file too large")
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("file too large")
	}
	return data, nil
}

// within returns the first root path is under and the path relative to it.
func within(roots []string, path string) (root, rel string, ok bool) {
	for _, r := range roots {
		rel, err := filepath.Rel(r, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "." {
			return r, rel, true
		}
	}
	return "", "", false
}

func (t *Tools) stagingDir() (string, error) {
	t.stagingMu.Lock()
	defer t.stagingMu.Unlock()
	if t.stagingPath != "" {
		return t.stagingPath, nil
	}
	if t.staging != "" {
		if err := os.MkdirAll(t.staging, 0o700); err != nil {
			return "", err
		}
		t.stagingPath = t.staging
		return t.stagingPath, nil
	}
	dir, err := os.MkdirTemp("", "agent-tools-")
	if err != nil {
		return "", err
	}
	t.stagingPath, t.ownsStaging = dir, true
	return dir, nil
}

func newCallID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return "call_" + hex.EncodeToString(b[:])
}

// --- the call's own goroutine -------------------------------------------------

// callFailure is a call's failure, in words, raised inside its reader.
type callFailure string

func (e callFailure) Error() string { return string(e) }

// read reads a call's stream to its end; see the type comment of Tools.
func (t *Tools) read(c *call) {
	defer t.wg.Done()
	defer c.cancel() // release the call's context once the stream is done
	var streamErr error
loop:
	for ev, err := range c.stream.events() {
		if err != nil {
			streamErr = err
			break loop
		}
		t.mu.Lock()
		open := c.open()
		t.mu.Unlock()
		if !open {
			// Canceled while this message was on its way.
			if ev.query != nil {
				t.refuseAll(context.Background(), []*question{ev.query})
			}
			continue
		}
		if ev.query == nil {
			t.mu.Lock()
			msg := t.admitLocked(c, len(ev.text), true, 0, ev.attachments)
			if msg == "" {
				c.text.WriteString(ev.text)
			}
			t.mu.Unlock()
			if msg != "" {
				streamErr = callFailure(msg)
				break loop
			}
			if len(ev.attachments) > 0 {
				files, err := t.save(c, ev.attachments)
				if err != nil {
					streamErr = err
					break loop
				}
				t.mu.Lock()
				c.replyFiles = append(c.replyFiles, files...)
				t.mu.Unlock()
			}
			continue
		}
		q := ev.query
		t.mu.Lock()
		msg := t.admitLocked(c, len(q.prompt), false, 1, q.attachments)
		if msg == "" {
			c.arriving = q
		}
		t.mu.Unlock()
		if msg != "" {
			t.refuseAll(context.Background(), []*question{q})
			streamErr = callFailure(msg)
			break loop
		}
		var files []ReturnedFile
		if len(q.attachments) > 0 {
			var err error
			if files, err = t.save(c, q.attachments); err != nil {
				streamErr = err
				break loop
			}
		}
		fields, err := t.afterReply(c, "question", q.prompt, files)
		if err != nil {
			streamErr = err
			break loop
		}
		t.mu.Lock()
		if !c.open() {
			// Canceled meanwhile: cancelCall took the question and refused it.
			t.mu.Unlock()
			continue
		}
		c.arriving = nil
		c.questions = append(c.questions, &openQuestion{q: q, files: files, fields: fields})
		if c.state == StateRunning {
			t.setStateLocked(c, StateInputRequired)
		}
		t.mu.Unlock()
	}
	if streamErr == nil {
		t.mu.Lock()
		open := c.open()
		text := c.text.String()
		files := append([]ReturnedFile(nil), c.replyFiles...)
		t.mu.Unlock()
		if !open {
			return
		}
		if !c.stream.terminated() {
			// A stream that ends without error and without the terminator was
			// dropped under us (SAP §6.5): not a reply.
			t.failCall(c, "the stream ended before the agent finished")
			return
		}
		fields, err := t.afterReply(c, "reply", text, files)
		if err != nil {
			t.failCall(c, err.Error())
			return
		}
		t.mu.Lock()
		if c.open() {
			c.replyFields = fields
			t.finishLocked(c, StateCompleted, "")
		}
		t.mu.Unlock()
		return
	}
	// A question left over is refused except on completion, where it is moot.
	t.mu.Lock()
	open := c.open()
	t.mu.Unlock()
	if !open {
		return // canceled already: cancelCall refused every question
	}
	c.stream.close() // a stream that failed us, or went over a limit, is done with
	var mw *caller.MaxWaitError
	if errors.As(streamErr, &mw) {
		t.mu.Lock()
		qs := t.finishLocked(c, StateExpired, fmt.Sprintf(
			"the call ran past its runtime limit of %s and was stopped; the agent may still be working on it",
			duration(mw.MaxWait)))
		t.mu.Unlock()
		t.refuseAll(context.Background(), qs)
		return
	}
	t.failCall(c, t.failure(c, streamErr))
}

// failCall fails the call and refuses its open questions: nobody can answer
// them now, so the asking agent hears so at once.
func (t *Tools) failCall(c *call, msg string) {
	t.mu.Lock()
	qs := t.finishLocked(c, StateFailed, msg)
	t.mu.Unlock()
	t.refuseAll(context.Background(), qs)
}

// admitLocked checks that a message fits the call's limits and, when it does,
// counts it; otherwise it returns why the call must stop. text is the bytes of
// reply or question text, isReply says they are reply text, questions how many
// questions the message is, and atts the files it carries (each a record of
// the result, saved or not). Callers hold t.mu.
func (t *Tools) admitLocked(c *call, text int, isReply bool, questions int, atts []agents.Attachment) string {
	records := 0
	for _, a := range atts {
		records += len(a.Filename) + fileRecordOverhead
	}
	switch {
	case isReply && c.text.Len()+text > t.cfg.maxReplyBytes:
		return fmt.Sprintf("the agent's reply went past the limit of %s of text, so the call was stopped", agents.FormatSize(int64(t.cfg.maxReplyBytes)))
	case c.asked+questions > t.cfg.maxQuestions:
		return fmt.Sprintf("the agent asked more than %d questions, so the call was stopped", t.cfg.maxQuestions)
	case c.fileRecords+len(atts) > t.cfg.maxFiles:
		return fmt.Sprintf("the agent sent more than %d files, so the call was stopped", t.cfg.maxFiles)
	case c.retained+text+records > t.cfg.maxResultBytes:
		return fmt.Sprintf("the agent's reply went past the total size limit of %s, so the call was stopped", agents.FormatSize(int64(t.cfg.maxResultBytes)))
	}
	c.asked += questions
	c.fileRecords += len(atts)
	c.retained += text + records
	return ""
}

// fileRecordOverhead is what a returned-file record is counted at beyond its
// file name, in bytes: the path and the numbers of its entry in a result.
const fileRecordOverhead = 256

func (t *Tools) failure(c *call, err error) string {
	var cf callFailure
	if errors.As(err, &cf) {
		return string(cf)
	}
	var se *agents.ServiceError
	if errors.As(err, &se) {
		detail := ""
		if se.Description != "" {
			detail = " " + se.Description
		}
		return fmt.Sprintf("the agent at %q answered with an error: %d%s", c.address, se.Code, detail)
	}
	var st *agents.StreamStalledError
	if errors.As(err, &st) {
		return fmt.Sprintf("the agent at %q sent nothing for %s, so the call was given up; the agent may have stopped",
			c.address, duration(st.Timeout))
	}
	if isNoResponders(err) {
		return t.noAgentAt(c.address, " any more")
	}
	if errors.Is(err, caller.ErrClosed) {
		return "the client the call ran on was closed"
	}
	var sc *caller.SlowConsumerError
	if errors.As(err, &sc) {
		return "the agent's replies came in faster than they could be read, so the call was given up"
	}
	t.logger.Warn("tools: a call failed", "call_id", c.id, "address", c.address, "error", err)
	return fmt.Sprintf("the call to the agent at %q failed; the connection to it broke or the agent misbehaved", c.address)
}

// save writes returned files under the call's own directory of the staging
// directory, within the per-call total.
func (t *Tools) save(c *call, atts []agents.Attachment) ([]ReturnedFile, error) {
	t.mu.Lock()
	remaining := t.cfg.maxSaved - c.savedBytes
	t.mu.Unlock()
	if remaining < 0 {
		remaining = 0
	}
	var saved []agents.SavedAttachment
	if remaining == 0 {
		// SaveOptions treats 0 as "the default"; here it means nothing fits.
		for _, a := range atts {
			n, ok := decodedSize(a.Content)
			s := agents.SavedAttachment{Filename: a.Filename, Size: n, Skipped: agents.SkipOverLimit}
			if !ok {
				s.Skipped = agents.SkipInvalidContent
			}
			saved = append(saved, s)
		}
	} else {
		staging, err := t.stagingDir()
		if err != nil {
			t.logger.Warn("tools: the staging directory is unavailable", "call_id", c.id, "error", err)
			return nil, callFailure(savedFailure)
		}
		saved, err = agents.SaveAttachments(atts, filepath.Join(staging, c.id), agents.SaveOptions{MaxTotalBytes: remaining})
		if err != nil {
			t.logger.Warn("tools: a returned file could not be saved", "call_id", c.id, "error", err)
			return nil, callFailure(savedFailure)
		}
	}
	out := make([]ReturnedFile, len(saved))
	for i, s := range saved {
		f := ReturnedFile{Filename: s.Filename, SizeBytes: s.Size}
		if s.Path != "" {
			p := s.Path
			f.Path = &p
			t.mu.Lock()
			c.savedBytes += s.Size
			t.mu.Unlock()
		} else {
			f.Skipped = string(s.Skipped)
		}
		out[i] = f
	}
	return out, nil
}

const savedFailure = "a file the agent sent could not be saved here, so the call was stopped"

func decodedSize(content string) (int64, bool) {
	b, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		return 0, false
	}
	return int64(len(b)), true
}

func isNoResponders(err error) bool { return errors.Is(err, nats.ErrNoResponders) }

// afterReply runs the extensions' reply look. A failure fails the call: the
// reader has no tool call to return it to.
func (t *Tools) afterReply(c *call, kind, text string, files []ReturnedFile) (map[string]any, error) {
	fields := map[string]any{}
	for _, e := range t.cfg.extensions {
		rl, ok := e.(ReplyLooker)
		if !ok {
			continue
		}
		add, err := rl.LookReply(t.base, &ReplyContext{
			CallID: c.id, Address: c.address, Target: c.target.handle(), Kind: kind, Text: text, Attachments: files,
		})
		if err == nil {
			err = addFields(fields, add, callResultFields, e.Name(), "a call's result")
		}
		if err != nil {
			var rf *reservedFieldError
			if errors.As(err, &rf) {
				// The discovery and prompt hooks refuse; the reply look runs in the
				// call's reader with nobody to refuse to: it fails the call, loudly.
				t.logger.Error("tools: an extension bug: a reply look set a field the contract defines; the call fails",
					"call_id", c.id, "kind", kind, "field", rf.field, "extension", rf.ext)
				return nil, callFailure(fmt.Sprintf("an extension of these tools has a bug (it set %q on the %s, a field the tools define); the agent at %q is not at fault",
					rf.field, kind, c.address))
			}
			t.logger.Warn("tools: an extension failed on a reply look", "call_id", c.id, "kind", kind, "extension", e.Name(), "error", err)
			return nil, callFailure(fmt.Sprintf("an extension failed on the %s", kind))
		}
	}
	return fields, nil
}

// --- state --------------------------------------------------------------------

// setStateLocked moves an open call between running and input_required.
func (t *Tools) setStateLocked(c *call, s CallState) {
	c.state = s
	if s == StateInputRequired {
		t.seq++
		c.seq = t.seq
	}
	t.wake()
}

// finishLocked gives the call its final state; the first one wins. It returns
// the questions that were open and the one being taken in, taken off the call:
// the caller refuses them, except on completion, where they are moot.
func (t *Tools) finishLocked(c *call, state CallState, errText string) []*question {
	if !c.open() {
		return nil
	}
	awaited := c.waiters > 0
	t.active--
	c.state = state
	c.errText = errText
	c.endedAt = time.Now().UTC()
	t.seq++
	c.seq = t.seq
	var open []*question
	for _, q := range c.questions {
		open = append(open, q.q)
	}
	c.questions = nil
	if c.arriving != nil {
		open = append(open, c.arriving)
		c.arriving = nil
	}
	t.wake()
	if c.scope == t.root && t.cfg.onSettled != nil && !t.closed {
		report, res := t.cfg.onSettled, CallResult(t.callResultLocked(c))
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.logger.Error("tools: the OnSettled function panicked", "call_id", c.id)
				}
			}()
			report(res, SettledInfo{Awaited: awaited})
		}()
	}
	return open
}

// cancelCall ends the call as canceled, drops its stream, then refuses its
// open questions, including one still being taken in. Nothing is sent to the
// agent beyond the refusals (SAP §6.7: no cancel message).
func (t *Tools) cancelCall(c *call) { t.refuseAll(context.Background(), t.stopCall(c)) }

// stopCall ends the call as canceled and drops its stream; it returns the
// questions to refuse.
func (t *Tools) stopCall(c *call) []*question {
	t.mu.Lock()
	wasOpen := c.open()
	qs := t.finishLocked(c, StateCancelled, "")
	t.mu.Unlock()
	if !wasOpen {
		return nil
	}
	c.cancel()
	c.stream.close()
	return qs
}

// refuseTimeout is the most time one batch of refusals takes in all, however
// many questions it holds.
const refuseTimeout = 5 * time.Second

// refuseWorkers is how many refusals go out at once.
const refuseWorkers = 16

// refuseAll answers each question with [QuestionRefusal], concurrently, and
// gives up on the stragglers when ctx ends or refuseTimeout has passed. A
// question whose reply subject is not trusted is not answered at all.
func (t *Tools) refuseAll(ctx context.Context, qs []*question) {
	if len(qs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, refuseTimeout)
	defer cancel()
	jobs := make(chan *question)
	var wg sync.WaitGroup
	for i := 0; i < min(len(qs), refuseWorkers); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for q := range jobs {
				if t.replyAllowed(q.replySubject) != nil {
					continue
				}
				_ = q.reply(ctx, QuestionRefusal) // already answered, or the connection is gone: nothing more to say
			}
		}()
	}
	defer wg.Wait()
	defer close(jobs)
	for _, q := range qs {
		select {
		case jobs <- q:
		case <-ctx.Done():
			return
		}
	}
}

// replyAllowed vets the reply subject a question names: the client's own
// policy first, then the trusted prefixes (see [WithAnswerReplyPrefixes]).
func (t *Tools) replyAllowed(subject string) error {
	if err := caller.DefaultQueryReplyPolicy(subject); err != nil {
		return err
	}
	for _, p := range t.replyPrefixes {
		if strings.HasPrefix(subject, p) && len(subject) > len(p) {
			return nil
		}
	}
	return errors.New("reply subject outside the trusted prefixes")
}

// untilReady waits until the call is no longer running; ctx ending cancels it.
func (t *Tools) untilReady(ctx context.Context, c *call) Result {
	for {
		t.mu.Lock()
		if c.state != StateRunning {
			r := t.callResultLocked(c)
			t.mu.Unlock()
			return r
		}
		ch := t.changed
		c.waiters++
		t.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
		}
		t.mu.Lock()
		c.waiters--
		t.mu.Unlock()
		if ctx.Err() != nil {
			t.cancelCall(c)
			t.mu.Lock()
			r := t.callResultLocked(c)
			t.mu.Unlock()
			return r
		}
	}
}

// --- wait_agent, answer_agent, cancel_agent, list_agent_calls ------------------

func (t *Tools) waitAgent(ctx context.Context, sc *scope, raw json.RawMessage) Result {
	a, err := parseWaitArgs(raw)
	if err != nil {
		return Result{"error": err.Error()}
	}
	t.mu.Lock()
	calls, err := t.callsLocked(sc, a.callIDs)
	t.mu.Unlock()
	if err != nil {
		return Result{"error": err.Error()}
	}
	limit := t.cfg.maxWaitAgent
	wait := limit
	if a.timeoutMS != nil {
		if d := time.Duration(*a.timeoutMS) * time.Millisecond; *a.timeoutMS < int64(limit/time.Millisecond) {
			wait = d
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		t.mu.Lock()
		var first *call
		for _, c := range calls {
			if c.state != StateRunning && (first == nil || c.seq < first.seq) {
				first = c
			}
		}
		if first != nil {
			res := t.callResultLocked(first)
			remaining := []string{}
			for _, c := range calls {
				if c != first && c.open() {
					remaining = append(remaining, c.id)
				}
			}
			res["remaining"] = remaining
			t.mu.Unlock()
			return res
		}
		select {
		case <-timer.C:
			t.mu.Unlock()
			return timeoutResult(calls)
		default:
		}
		if ctx.Err() != nil {
			t.mu.Unlock()
			return timeoutResult(calls)
		}
		ch := t.changed
		for _, c := range calls {
			c.waiters++
		}
		t.mu.Unlock()
		select {
		case <-ch:
		case <-timer.C:
			// Re-arm as already-fired so the next pass returns the timeout.
			timer.Reset(0)
		case <-ctx.Done():
		}
		t.mu.Lock()
		for _, c := range calls {
			c.waiters--
		}
		t.mu.Unlock()
	}
}

func timeoutResult(calls []*call) Result {
	ids := make([]string, len(calls))
	for i, c := range calls {
		ids[i] = c.id
	}
	return Result{"state": string(StateRunning), "call_ids": ids}
}

func (t *Tools) answerAgent(ctx context.Context, sc *scope, raw json.RawMessage) Result {
	a, err := parseAnswerArgs(raw)
	if err != nil {
		return Result{"error": err.Error()}
	}
	if a.wait != nil && !*a.wait && !t.offered[ToolWaitAgent] {
		return Result{"error": `answer_agent: "wait" cannot be false here; the call waits for the reply or the next question`}
	}
	t.mu.Lock()
	c, ok := sc.calls[a.callID]
	if !ok {
		t.mu.Unlock()
		return Result{"error": t.unknownCalls([]string{a.callID})}
	}
	if c.state != StateInputRequired || len(c.questions) == 0 {
		msg := fmt.Sprintf("call %q has no open question: it is %s", c.id, c.state)
		if !c.open() {
			msg += t.ifOffered(ToolWaitAgent, "; wait_agent returns its result")
		}
		t.mu.Unlock()
		return Result{"error": msg}
	}
	if err := t.replyAllowed(c.questions[0].q.replySubject); err != nil {
		t.mu.Unlock()
		t.logger.Warn("tools: an answer was not sent: the reply subject is not trusted", "call_id", a.callID, "error", err)
		return Result{"error": "the answer was not sent: the agent asked for it on a subject these tools do not trust" +
			t.ifOffered(ToolCancelAgent, "; cancel_agent stops the call")}
	}
	// Taken off before the answer goes out, so a concurrent tool call never
	// sees a question that is being answered.
	q := c.questions[0]
	c.questions = c.questions[1:]
	if len(c.questions) > 0 {
		t.setStateLocked(c, StateInputRequired)
	} else {
		t.setStateLocked(c, StateRunning)
	}
	detached := c.detached
	t.mu.Unlock()
	if err := q.q.reply(ctx, a.answer); err != nil {
		t.mu.Lock()
		if c.open() {
			c.questions = append([]*openQuestion{q}, c.questions...)
			t.setStateLocked(c, StateInputRequired)
		}
		t.mu.Unlock()
		t.logger.Warn("tools: an answer could not be sent", "call_id", a.callID, "error", err)
		return Result{"error": "the answer could not be sent; the question is still open"}
	}
	wait := !detached
	if a.wait != nil {
		wait = *a.wait
	}
	if !wait {
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.callResultLocked(c)
	}
	return t.untilReady(ctx, c)
}

func (t *Tools) cancelAgent(sc *scope, raw json.RawMessage) Result {
	ids, err := parseCancelArgs(raw)
	if err != nil {
		return Result{"error": err.Error()}
	}
	results := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		t.mu.Lock()
		c, ok := sc.calls[id]
		t.mu.Unlock()
		if !ok {
			results = append(results, map[string]any{"call_id": id, "error": t.unknownCalls([]string{id})})
			continue
		}
		t.cancelCall(c)
		t.mu.Lock()
		results = append(results, t.callResultLocked(c))
		t.mu.Unlock()
	}
	return Result{"calls": results}
}

func (t *Tools) listAgentCalls(sc *scope, raw json.RawMessage) Result {
	if _, err := argsObject(ToolListAgentCalls, raw); err != nil {
		return Result{"error": err.Error()}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	calls := make([]map[string]any, 0, len(sc.order))
	for _, c := range sc.order {
		e := map[string]any{
			"call_id":    c.id,
			"address":    c.address,
			"state":      string(c.state),
			"started_at": isoTime(c.startedAt),
		}
		if c.hasLabel {
			e["label"] = c.label
		}
		if !c.endedAt.IsZero() {
			e["ended_at"] = isoTime(c.endedAt)
		}
		calls = append(calls, e)
	}
	return Result{"calls": calls}
}

// isoTime formats a time as JavaScript's toISOString does: UTC, milliseconds.
func isoTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// callsLocked returns the calls ids name in sc, or an error naming the ones it
// does not track.
func (t *Tools) callsLocked(sc *scope, ids []string) ([]*call, error) {
	var unknown []string
	for _, id := range ids {
		if _, ok := sc.calls[id]; !ok {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		return nil, argsError(t.unknownCalls(unknown))
	}
	out := make([]*call, len(ids))
	for i, id := range ids {
		out[i] = sc.calls[id]
	}
	return out, nil
}

// --- words ----------------------------------------------------------------------
//
// What a result tells the model to do next points only to tools the helper
// offers: the model can call no other.

func (t *Tools) ifOffered(tool, words string) string {
	if t.offered[tool] {
		return words
	}
	return ""
}

func (t *Tools) unknownCalls(ids []string) string {
	q := make([]string, len(ids))
	for i, id := range ids {
		q[i] = fmt.Sprintf("%q", id)
	}
	return strings.Join(q, ", ") + ": no such call in your calls" +
		t.ifOffered(ToolListAgentCalls, "; list_agent_calls shows them")
}

func (t *Tools) noAgentAt(address, still string) string {
	return fmt.Sprintf("no agent answers at %q%s", address, still) +
		t.ifOffered(ToolDiscoverAgents, "; call discover_agents for the current list")
}

// openCallActions says what the model can do about open calls with the tools it
// has: collect them, or answer their questions when nothing is detached, and
// stop them. Calls are open only where prompt_agent is offered, and it always
// comes with answer_agent, so there is always a way to collect them.
func (t *Tools) openCallActions(them, their string) (collect, stop string) {
	if t.offered[ToolWaitAgent] {
		collect = "collect " + them + " with wait_agent"
	} else {
		collect = "answer " + their + " questions with answer_agent"
	}
	if t.offered[ToolCancelAgent] {
		stop = "stop " + them + " with cancel_agent"
	}
	return collect, stop
}

// --- results ----------------------------------------------------------------------

func (t *Tools) callResultLocked(c *call) Result {
	r := Result{}
	for k, v := range c.fields {
		r[k] = v
	}
	r["call_id"] = c.id
	r["state"] = string(c.state)
	if c.hasLabel {
		r["label"] = c.label
	}
	partial := func() {
		if c.text.Len() > 0 {
			r["partial_reply"] = c.text.String()
		}
	}
	files := func(fs []ReturnedFile) {
		if len(fs) > 0 {
			r["attachments"] = append([]ReturnedFile(nil), fs...)
		}
	}
	switch c.state {
	case StateRunning:
	case StateInputRequired:
		q := c.questions[0]
		for k, v := range q.fields {
			r[k] = v
		}
		r["question"] = q.q.prompt
		files(q.files)
	case StateCompleted:
		for k, v := range c.replyFields {
			r[k] = v
		}
		r["reply"] = c.text.String()
		files(c.replyFiles)
	case StateCancelled:
		partial()
		files(c.replyFiles)
	case StateFailed, StateExpired:
		msg := c.errText
		if msg == "" {
			msg = "the call failed"
		}
		r["error"] = msg
		partial()
		files(c.replyFiles)
	}
	return r
}

// withOpenCalls says how many calls are open, when any is.
func (t *Tools) withOpenCalls(sc *scope, res Result) Result {
	t.mu.Lock()
	open := 0
	for _, c := range sc.order {
		if c.open() {
			open++
		}
	}
	t.mu.Unlock()
	if open == 0 {
		return res
	}
	counted := "1 call you started is"
	them, their := "it", "its"
	if open != 1 {
		counted = fmt.Sprintf("%d calls you started are", open)
		them, their = "them", "their"
	}
	collect, stop := t.openCallActions(them, their)
	var note string
	if sc.served {
		note = fmt.Sprintf("%s still open. Calls end with the prompt you are answering: %s before you answer.", counted, collect)
	} else if stop != "" {
		note = fmt.Sprintf("%s still open: %s, or %s.", counted, collect, stop)
	} else {
		note = fmt.Sprintf("%s still open: %s.", counted, collect)
	}
	out := make(Result, len(res)+2)
	for k, v := range res {
		out[k] = v
	}
	out["open_calls"] = open
	out["open_calls_note"] = note
	return out
}

func duration(d time.Duration) string {
	ms := d.Milliseconds()
	switch {
	case ms >= 60_000 && ms%60_000 == 0:
		if m := ms / 60_000; m != 1 {
			return fmt.Sprintf("%d minutes", m)
		}
		return "1 minute"
	case ms >= 1_000 && ms%1_000 == 0:
		if s := ms / 1_000; s != 1 {
			return fmt.Sprintf("%d seconds", s)
		}
		return "1 second"
	}
	return fmt.Sprintf("%d ms", ms)
}
