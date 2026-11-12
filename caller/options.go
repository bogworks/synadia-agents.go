// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/bogworks/synadia-agents.go/internal/stance"
)

// Defaults of the client. Each is SDK policy unless noted, and each can be
// overridden by an option.
const (
	// DefaultInboxPrefix is the reply-inbox prefix every SDK caller uses, so one
	// permission covers all of them. The client's single reply subscription is
	// DefaultInboxPrefix.{client-id}.>.
	DefaultInboxPrefix = "_INBOX.agents"

	// DefaultMaxWait is the ceiling on one stream, 10 minutes, the value both
	// official callers use.
	DefaultMaxWait = 10 * time.Minute

	// DefaultStatusTimeout is how long a status request waits for its reply.
	DefaultStatusTimeout = 2 * time.Second

	// DefaultLookupTimeout is how long an instance lookup or ping waits.
	DefaultLookupTimeout = 2 * time.Second

	// DefaultDiscoverStall ends a discovery that has had no reply for this
	// long once one has arrived, the value the official callers use.
	DefaultDiscoverStall = 750 * time.Millisecond

	// DefaultDiscoverMaxWait is the absolute ceiling of a discovery.
	DefaultDiscoverMaxWait = 2 * time.Second

	// DefaultStreamBuffer is the most unread messages one stream holds before it
	// ends with a [*SlowConsumerError].
	DefaultStreamBuffer = 1024

	// DefaultStreamBufferBytes is the most unread bytes one stream holds.
	DefaultStreamBufferBytes int64 = 32 << 20

	// DefaultDiscoverBuffer is the most replies one discovery buffers.
	DefaultDiscoverBuffer = 4096

	// DefaultDiscoverMaxRecords is the most records one discovery keeps. A
	// responder that floods distinct instances cannot grow the result past it
	// (SDK policy). See [WithDiscoverMaxRecords].
	DefaultDiscoverMaxRecords = 10000

	// DefaultCloseTimeout bounds how long [Client.Close] waits for the client
	// to shut down, and how long [Client.Shutdown] waits for its subscriptions
	// to drain when the context passed to it has no deadline.
	DefaultCloseTimeout = 5 * time.Second

	// DefaultFlushTimeout bounds the flush that follows a subscription when the
	// caller's context has no deadline.
	DefaultFlushTimeout = 5 * time.Second
)

type config struct {
	logger        *slog.Logger
	inboxPrefix   string
	policy        agents.VersionPolicy
	ident         *IdentityOptions
	interceptors  []Interceptor
	headerHook    HeaderHook
	inactivity    time.Duration
	maxWait       time.Duration
	buffer        int
	bufferBytes   int64
	resolveTTL    time.Duration
	trackerOpts   []TrackerOption
	discoverLimit int
	queryPolicy   func(string) error
	trust         *TrustPolicy
	stance        identity.Stance
}

func defaultConfig() config {
	return config{
		inboxPrefix:   DefaultInboxPrefix,
		inactivity:    agents.DefaultInactivityTimeout,
		maxWait:       DefaultMaxWait,
		buffer:        DefaultStreamBuffer,
		bufferBytes:   DefaultStreamBufferBytes,
		resolveTTL:    identity.DefaultResolveTTL,
		discoverLimit: DefaultDiscoverBuffer,
		queryPolicy:   DefaultQueryReplyPolicy,
	}
}

// Option configures a [Client].
type Option func(*config) error

// WithLogger sets the logger. A nil logger, the default, is silent. The client
// never logs payloads, seeds, nonces or signatures.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) error { c.logger = l; return nil }
}

// WithInboxPrefix sets the prefix of the client's reply subjects, replacing
// [DefaultInboxPrefix]. The client subscribes once to prefix.{client-id}.> and
// routes each reply by the token after it (REQ-019). The prefix must be a valid
// publish subject.
func WithInboxPrefix(prefix string) Option {
	return func(c *config) error {
		if err := agents.ValidatePublishSubject(prefix); err != nil {
			return fmt.Errorf("caller: inbox prefix: %w", err)
		}
		c.inboxPrefix = prefix
		return nil
	}
}

// WithVersionPolicy sets which protocol versions the client accepts from an
// agent. The default, [agents.VersionDefault], accepts exactly the SDK's
// MAJOR.MINOR while the protocol is 0.x. An agent the policy refuses fails
// [Agent.Prompt] with a [*agents.VersionError]; [Agent.Status], a probe, is
// not refused. See SAP §11.2.
func WithVersionPolicy(p agents.VersionPolicy) Option {
	return func(c *config) error { c.policy = p; return nil }
}

// IdentityOptions configure the optional sender identity extension (SAP §13).
// The zero value configures nothing: no lookup is made and no header is sent.
type IdentityOptions struct {
	// Signer signs the Agent-Sender header of every prompt and status request
	// (SAP §13.5, §13.6). The program supplies the seed; the SDK reads key
	// material from nowhere else. Nil means the client cannot sign.
	Signer identity.Signer

	// Name is an optional display name carried in the header. It is never
	// identity and is outside the signature.
	Name string

	// SendClaim sends an unsigned claim (SAP §13.5, MAY) when the connection's
	// identity is known and Signer is nil. A claim discloses the user NKEY to
	// every receiver and grants nothing.
	SendClaim bool

	// ConsultServer asks $SYS.REQ.USER.INFO even when the signer carries a JWT
	// (see [identity.SelfIDOptions]).
	ConsultServer bool

	// LookupTimeout is the $SYS.REQ.USER.INFO timeout; zero means
	// [identity.DefaultUserInfoTimeout].
	LookupTimeout time.Duration

	// RetryInterval is how long an unknown identity is remembered before a
	// request asks again; zero means [identity.DefaultSelfIDRetryInterval].
	RetryInterval time.Duration
}

// WithIdentity enables the sender identity extension. With a signer the client
// signs every prompt and status request once the connection's identity is
// known, counts the header against max_payload, and refuses locally to send to
// an endpoint that requires a signed sender when it cannot (SAP §13.4, §13.9).
// A connection whose identity is none or unknown sends no header to an endpoint
// that does not require one. To rotate a key, restart the client with the new
// signer; there is no live rotation (SDK policy).
func WithIdentity(o IdentityOptions) Option {
	return func(c *config) error {
		if o.Name != "" && !identity.ValidName(o.Name) {
			return fmt.Errorf("caller: invalid sender display name")
		}
		cp := o
		c.ident = &cp
		return nil
	}
}

// WithInterceptors installs prompt interceptors, run in order for every prompt
// (REQ-016). See [Interceptor].
func WithInterceptors(i ...Interceptor) Option {
	return func(c *config) error {
		for _, x := range i {
			if x == nil {
				return fmt.Errorf("caller: nil interceptor")
			}
		}
		c.interceptors = append(c.interceptors, i...)
		return nil
	}
}

// HeaderHook returns application headers to add to one outgoing prompt. It may
// not set a header SAP defines or relies on; see [ReservedHeader].
type HeaderHook func(ctx context.Context, p *PromptInfo) (map[string]string, error)

// WithRequestHeaders installs the outgoing-header hook of REQ-016: it adds
// application headers to every prompt. A header SAP defines or relies on cannot
// be set (decision: Agent-Sender, Nats-Service-Error, Nats-Service-Error-Code,
// Nats-Request-Info, Nats-Msg-Id, matched case-insensitively); setting one fails
// the prompt with [ErrReservedHeader] before anything is published. SAP neither
// requires nor forbids the hook, so it is SDK policy.
func WithRequestHeaders(fn HeaderHook) Option {
	return func(c *config) error { c.headerHook = fn; return nil }
}

// WithInactivityTimeout sets the default per-stream inactivity timeout, which
// counts from the latest chunk seen, an ack included. The default is
// [agents.DefaultInactivityTimeout], 60 seconds. See SAP §6.6.
func WithInactivityTimeout(d time.Duration) Option {
	return func(c *config) error {
		if d <= 0 {
			return fmt.Errorf("caller: inactivity timeout must be positive")
		}
		c.inactivity = d
		return nil
	}
}

// WithMaxWait sets the default ceiling on one stream, [DefaultMaxWait].
func WithMaxWait(d time.Duration) Option {
	return func(c *config) error {
		if d <= 0 {
			return fmt.Errorf("caller: max wait must be positive")
		}
		c.maxWait = d
		return nil
	}
}

// WithStreamBuffer bounds the unread messages and bytes one stream may hold.
// A stream that exceeds either ends with a [*SlowConsumerError] instead of
// growing without bound or blocking other streams (REQ-019). Zero keeps the
// default for that bound.
func WithStreamBuffer(messages int, bytes int64) Option {
	return func(c *config) error {
		if messages < 0 || bytes < 0 {
			return fmt.Errorf("caller: stream buffer bounds must not be negative")
		}
		if messages > 0 {
			c.buffer = messages
		}
		if bytes > 0 {
			c.bufferBytes = bytes
		}
		return nil
	}
}

// WithResolveTTL sets how long the sender resolver reuses one enumeration
// (SAP §13.10 suggests a short cache). Zero means [identity.DefaultResolveTTL];
// a negative value enumerates on every lookup.
func WithResolveTTL(d time.Duration) Option {
	return func(c *config) error {
		if d != 0 {
			c.resolveTTL = d
		}
		return nil
	}
}

// DefaultQueryReplyPolicy is the query reply policy a client uses unless
// [WithQueryReplyPolicy] replaces it. It refuses a subject that is not a valid
// publish subject (empty tokens, wildcards, whitespace), one whose first token
// starts with "$" (system subjects such as $SYS or $JS), and one under the
// "agents" protocol namespace, where other agents' prompt, status and heartbeat
// subjects live. Official hosts answer on inbox subjects (_INBOX.*), which
// pass. SDK policy; see SAP §7.1, which lets the agent choose the subject.
func DefaultQueryReplyPolicy(subject string) error {
	if err := agents.ValidatePublishSubject(subject); err != nil {
		return err
	}
	first, _, _ := strings.Cut(subject, ".")
	switch {
	case strings.HasPrefix(first, "$"):
		return fmt.Errorf("system namespace")
	case first == agents.SubjectRoot:
		return fmt.Errorf("protocol namespace")
	}
	return nil
}

// WithQueryReplyPolicy sets the policy that vets the agent-supplied
// reply_subject of every query before [Query.Reply] publishes there. SAP §7.1
// lets the agent pick the subject, so a hostile agent could aim the program's
// answer at any subject its NATS permissions allow; the policy is the
// program's defense, and narrow publish permissions on the connection are the
// other: grant publish only on the inbox prefixes the agents you trust use. A
// non-nil error from fn refuses the reply with a [*QueryReplyRefusedError] and
// publishes nothing. fn replaces [DefaultQueryReplyPolicy] entirely and must
// not block; call the default from fn to keep its rules. A nil fn restores
// the default. SDK policy.
func WithQueryReplyPolicy(fn func(subject string) error) Option {
	return func(c *config) error {
		if fn == nil {
			fn = DefaultQueryReplyPolicy
		}
		c.queryPolicy = fn
		return nil
	}
}

// WithStance sets the security stance (see [identity.Stance]). The client
// reads two checks of it. VerifiedDiscovery makes discovery verified by
// default, as a [WithTrustPolicy] that accepts any signer whose id_sig verifies
// does, and it gates [Agent.Prompt] and [Agent.Status] the same way
// ([ErrUntrustedAgent]); an explicit [WithTrustPolicy] wins over it. Signing
// makes [New] fail unless [WithIdentity] gave a Signer. The stance also goes to
// the heartbeat tracker the client builds, unless [WithTracker] sets one with
// [WithTrackerStance]. The default is [identity.DefaultProfile]. When no stance
// is chosen and no trust policy is set, a logger set with [WithLogger] gets one
// warning per process that says so. SDK policy.
func WithStance(s identity.Stance) Option {
	return func(c *config) error {
		if _, err := stance.Resolve(s); err != nil {
			return fmt.Errorf("caller: %w", err)
		}
		c.stance = s
		return nil
	}
}

// WithTracker passes options to the heartbeat tracker the client owns.
func WithTracker(opts ...TrackerOption) Option {
	return func(c *config) error { c.trackerOpts = append(c.trackerOpts, opts...); return nil }
}

// reservedHeaders are the headers a hook or interceptor may not set, compared
// case-insensitively.
var reservedHeaders = []string{
	agents.HeaderAgentSender,
	agents.HeaderServiceError,
	agents.HeaderServiceErrorCode,
	agents.HeaderRequestInfo,
	"Nats-Msg-Id",
}

// ReservedHeader reports whether name is a header the outgoing-header hook and
// interceptors may not set. The comparison is case-insensitive.
func ReservedHeader(name string) bool {
	for _, r := range reservedHeaders {
		if strings.EqualFold(name, r) {
			return true
		}
	}
	return false
}

func validHeader(name, value string) error {
	if name == "" || strings.ContainsAny(name, " \t\r\n:\x00") {
		return ErrInvalidHeader
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return ErrInvalidHeader
	}
	if ReservedHeader(name) {
		return fmt.Errorf("%w: %s", ErrReservedHeader, name)
	}
	return nil
}

// TrustPolicy says which agent identities a program trusts. A record satisfies
// it when its id_sig verifies over the record's own prompt subject (SAP §13.10)
// and the verified signer passes both fields that are set. A policy with
// neither field set is refused by [WithTrustPolicy]; to trust any verified
// signer, set Accept to a function that returns true. SDK policy.
type TrustPolicy struct {
	// Agents is an allowlist of signer identities. When non-empty, the signer
	// must be one of them.
	Agents []identity.AgentID

	// Accept, when non-nil, must return true for the signer and the record.
	// It receives a copy of the record and must not block.
	Accept func(identity.AgentID, agents.AgentInfo) bool
}

func (p *TrustPolicy) trusts(id identity.AgentID, info *agents.AgentInfo) bool {
	if len(p.Agents) > 0 && !slices.Contains(p.Agents, id) {
		return false
	}
	return p.Accept == nil || p.Accept(id, cloneInfo(info))
}

// WithTrustPolicy makes verified discovery the default. Client.Discover,
// Client.Find, Client.ListAgents and Client.LookupInstance return only records
// whose id_sig verifies over the record's own prompt subject and whose verified
// signer satisfies the policy; others are dropped and counted in
// [Stats.RejectedRecords]. A per-call [WithRequireVerifiedIdentity] can narrow
// the result further but cannot disable verification, and there is deliberately
// no per-call opt-out. [Agent.Prompt] and [Agent.Status] refuse with
// [ErrUntrustedAgent] an agent discovery did not admit.
//
// Without this option, verification stays opt-in per call, unless [WithStance]
// turns verified discovery on; an explicit policy wins over the stance. A valid id_sig
// identifies the registrant; it does not prove the discovery reply came from
// them or that the signer deserves trust beyond the policy (see
// [WithRequireVerifiedIdentity]). The policy is copied.
func WithTrustPolicy(p TrustPolicy) Option {
	return func(c *config) error {
		if len(p.Agents) == 0 && p.Accept == nil {
			return fmt.Errorf("caller: trust policy names no identity and has no Accept function")
		}
		cp := TrustPolicy{Agents: slices.Clone(p.Agents), Accept: p.Accept}
		c.trust = &cp
		return nil
	}
}
