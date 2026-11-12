// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/bogworks/synadia-agents.go/internal/hosttest"
	"github.com/bogworks/synadia-agents.go/internal/stance"
	"github.com/nats-io/nats.go/micro"
)

// Defaults of the host. The values marked SDK policy are this SDK's choice,
// not a SAP requirement.
const (
	// DefaultHeartbeatInterval is the recommended cadence. See SAP §8.2.
	DefaultHeartbeatInterval = agents.DefaultHeartbeatInterval

	// DefaultKeepalive is the cadence of the keep-alive ack, 30 seconds, below
	// the 60 seconds of SAP §6.6. It applies when no [WithKeepalive] or
	// [WithoutKeepalive] option is given, and it is the cadence of the official
	// TypeScript and Python hosts. SDK policy; see [WithKeepalive] for the
	// deviation from SAP:L380 this records.
	DefaultKeepalive = 30 * time.Second

	// DefaultHeartbeatSignTimeout bounds the signing of one heartbeat (SDK
	// policy). See [WithHeartbeatSignTimeout].
	DefaultHeartbeatSignTimeout = 5 * time.Second

	// DefaultAttachmentsOK is the attachments_ok the prompt endpoint
	// advertises. See SAP §2.1.
	DefaultAttachmentsOK = true

	// DefaultMaxConcurrentPrompts is the number of prompts served at once
	// (SDK policy, REQ-019).
	DefaultMaxConcurrentPrompts = 64

	// DefaultMinSenderTrust is the min_sender_trust the prompt endpoint
	// advertises. See SAP §13.9.
	DefaultMinSenderTrust = identity.TrustAny

	// DefaultVersion is the service version registered when [WithVersion] is
	// not used. See SAP §3.1.
	DefaultVersion = "0.0.1"

	// DefaultStartTimeout bounds the flush of [Host.Start] when its context
	// has no deadline.
	DefaultStartTimeout = 10 * time.Second

	// DefaultCloseTimeout bounds how long [Host.Close] waits for the host to be
	// complete. [Host.Shutdown] takes a context of the caller's own instead.
	DefaultCloseTimeout = 10 * time.Second

	// DefaultMaxPayload is advertised when the connection reports no limit.
	DefaultMaxPayload int64 = 1 << 20
)

// BusyPolicy says what the host does with a prompt when every concurrency
// slot is taken. It is SDK policy (REQ-019).
type BusyPolicy int

const (
	// BusyQueue holds the prompt endpoint's subscription callback until a
	// slot frees, which back-pressures that subscription. It is the default.
	// Requests waiting in the subscription's buffer are subject to the NATS
	// slow-consumer limits.
	BusyQueue BusyPolicy = iota
	// BusyReject429 answers a prompt that finds no free slot with 429 and the
	// terminator at once (SAP §9.2).
	BusyReject429
)

// IdentityOptions configure the host's own identity registration (SAP §13.10).
type IdentityOptions struct {
	// Signer is the connection's user signer. With it the host registers
	// user_nkey, account and id_sig and signs every heartbeat (SAP §13.10,
	// §13.11); without one it registers the unsigned user_nkey and account
	// when the connection's identity is known, best effort, and signs
	// nothing.
	Signer identity.Signer
}

// AcceptSenderFunc is the admission hook (SAP §13.7.3; REQ-016). It runs for
// every classified prompt request, never for status, after classification and
// before the ack. A false for a verified sender is answered 403; for a claimed
// or absent one 401 "signature required"; an error is 500 and the request is
// never served. The hook decides policy; the SDK decides none. sender is nil
// for the absent class. req carries the headers and subject, with Sender set.
type AcceptSenderFunc func(ctx context.Context, sender *identity.Sender, req *RequestInfo) (bool, error)

type config struct {
	description      string
	version          string
	session          string
	maxPayload       int64
	attachmentsOK    bool
	inboundChecks    bool
	hbInterval       time.Duration
	subSecond        bool
	keepalive        time.Duration
	signTimeout      time.Duration
	extraMeta        map[string]string
	endpoints        []Endpoint
	logger           *slog.Logger
	identity         *IdentityOptions
	minTrust         identity.MinSenderTrust
	minTrustSet      bool
	stance           identity.Stance
	replayWindow     time.Duration
	maxNonces        int
	maxPerSender     int
	accept           AcceptSenderFunc
	resolveTTL       time.Duration
	operatorAttested bool
	accountTokenPos  int
	interceptors     []Interceptor
	hbExtras         func() map[string]any
	maxConc          int
	busy             BusyPolicy
	hooks            hosttest.Hooks
	replyPolicy      ReplySubjectPolicy
}

// Option configures a [Host]. An invalid value is reported by [New].
type Option func(*config) error

// TestHooks lets internal/hosttest set the fixture-only raw prompt handler
// through [hosttest.RawOption] without any exported host symbol. The method
// is on the unexported config, so it is not part of the public API.
func (c *config) TestHooks() *hosttest.Hooks { return &c.hooks }

func defaultConfig() config {
	return config{
		version:       DefaultVersion,
		attachmentsOK: DefaultAttachmentsOK,
		inboundChecks: true,
		hbInterval:    DefaultHeartbeatInterval,
		keepalive:     DefaultKeepalive,
		signTimeout:   DefaultHeartbeatSignTimeout,
		minTrust:      DefaultMinSenderTrust,
		maxConc:       DefaultMaxConcurrentPrompts,
	}
}

// WithDescription sets the service description; the default is
// "<agent> agent <name>". See SAP §3.1.
func WithDescription(s string) Option {
	return func(c *config) error { c.description = s; return nil }
}

// semver is the version grammar nats.go/micro enforces on a service.
var semver = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)

// WithVersion sets the service version, a semver of the harness
// implementation; the default is [DefaultVersion]. See SAP §3.1.
func WithVersion(v string) Option {
	return func(c *config) error {
		if !semver.MatchString(v) {
			return fmt.Errorf("%w: version %q is not a semver", ErrConfig, v)
		}
		c.version = v
		return nil
	}
}

// WithSession sets metadata.session and the heartbeat's session field. It is
// advertised only when set; a session-aware harness MUST set it. See SAP §3.2.
func WithSession(s string) Option {
	return func(c *config) error { c.session = s; return nil }
}

// WithMaxPayload sets the max_payload the prompt endpoint advertises. The
// default is the connection's negotiated limit; a larger value is clamped down
// to it with a warning, because the server would refuse anything larger before
// it reached the handler. See SAP §2.1.
func WithMaxPayload(n int64) Option {
	return func(c *config) error {
		if n <= 0 {
			return fmt.Errorf("%w: max payload must be positive", ErrConfig)
		}
		c.maxPayload = n
		return nil
	}
}

// WithAttachmentsOK sets the attachments_ok the prompt endpoint advertises;
// the default is [DefaultAttachmentsOK]. See SAP §2.1.
func WithAttachmentsOK(ok bool) Option {
	return func(c *config) error { c.attachmentsOK = ok; return nil }
}

// WithInboundChecks turns the host's own enforcement of the advertised limits
// on or off (SDK policy; SAP §5.4 makes it a MAY). On by default: a payload
// over max_payload, and attachments sent to an endpoint that advertises
// attachments_ok false, are answered 400 (SAP §9.2).
func WithInboundChecks(on bool) Option {
	return func(c *config) error { c.inboundChecks = on; return nil }
}

// WithHeartbeatInterval sets the heartbeat cadence; the default is
// [DefaultHeartbeatInterval]. An interval under one second also needs
// [WithSubSecondHeartbeats] (SDK policy; SAP §8.2 says SHOULD NOT).
func WithHeartbeatInterval(d time.Duration) Option {
	return func(c *config) error { c.hbInterval = d; return nil }
}

// WithSubSecondHeartbeats is the explicit opt-in that allows a heartbeat
// interval under one second. See SAP §8.2.
func WithSubSecondHeartbeats() Option {
	return func(c *config) error { c.subSecond = true; return nil }
}

// WithKeepalive sets the cadence of the keep-alive ack: while a handler runs,
// the host sends an extra ack status chunk every d, which resets the caller's
// inactivity timeout (SAP §6.4, §6.6). The default is [DefaultKeepalive], 30
// seconds; WithKeepalive(0) turns keep-alives off, like [WithoutKeepalive]. A
// negative d is an [ErrConfig] error.
//
// The default matches the official TypeScript and Python hosts because their
// callers' default 60 second inactivity timeout otherwise stalls a long-running
// prompt: the experiment TestGoHost_KeepAlive_DefaultTimeouts (test/interop,
// run with SAP_INTEROP_LONG=1) shows both callers giving up on a silent Go
// host without keep-alives and succeeding with them.
//
// SAP:L380 says an agent MUST emit exactly one ack as the first message of the
// reply stream, so the default is a recorded deviation from the spec text (see
// docs/upstream-issues.md, issue 18). Turn keep-alives off, with
// WithKeepalive(0) or [WithoutKeepalive], for strict SAP:L380 behavior.
func WithKeepalive(d time.Duration) Option {
	return func(c *config) error {
		if d < 0 {
			return fmt.Errorf("%w: keepalive must not be negative (0 disables it)", ErrConfig)
		}
		c.keepalive = d
		return nil
	}
}

// WithoutKeepalive disables the keep-alive ack, so a stream carries exactly
// one ack, the strict reading of SAP:L380 (SAP §6.4). It is the same as
// WithKeepalive(0), and it undoes an earlier [WithKeepalive]. Without
// keep-alives a caller whose inactivity timeout is shorter than a silent
// handler gives up on it; see [WithKeepalive].
func WithoutKeepalive() Option {
	return func(c *config) error { c.keepalive = 0; return nil }
}

// WithHeartbeatSignTimeout bounds the signing of one heartbeat; the default is
// [DefaultHeartbeatSignTimeout] (SDK policy). The signer runs in its own
// goroutine, because [identity.Signer] takes no context: when it does not
// return within d the beat is skipped as for a signing failure and counted in
// [Stats.HeartbeatSignTimeouts], and the host goes on. The abandoned goroutine
// lives until the signer returns, so a signer that never returns leaks one
// goroutine (the host starts no second signing while one is outstanding), and
// [Host.Close] does not wait for it. d must be positive.
func WithHeartbeatSignTimeout(d time.Duration) Option {
	return func(c *config) error {
		if d <= 0 {
			return fmt.Errorf("%w: heartbeat sign timeout must be positive", ErrConfig)
		}
		c.signTimeout = d
		return nil
	}
}

// WithExtraMetadata adds keys to the service metadata (SAP §3.2: additional
// keys MAY be included). The required keys and the identity keys the host
// registers win; user_nkey, account and id_sig are dropped from the extras
// when the host does not register them.
func WithExtraMetadata(m map[string]string) Option {
	return func(c *config) error { c.extraMeta = maps.Clone(m); return nil }
}

// WithEndpoint registers an extra micro endpoint beside prompt and status
// (REQ-004, REQ-016). It can be repeated; endpoints register in order after
// the protocol endpoints.
func WithEndpoint(e Endpoint) Option {
	return func(c *config) error {
		e.Metadata = maps.Clone(e.Metadata)
		c.endpoints = append(c.endpoints, e)
		return nil
	}
}

// WithLogger sets the logger; nil is silent. See package documentation.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) error { c.logger = l; return nil }
}

// WithIdentity enables the sender identity extension's registration side:
// the host learns its own agent ID, registers user_nkey, account and, with a
// signer, id_sig (SAP §13.10), and signs heartbeats (SAP §13.11). Without it
// no self lookup is made and no identity metadata is registered.
func WithIdentity(o IdentityOptions) Option {
	return func(c *config) error { c.identity = &o; return nil }
}

// WithMinSenderTrust sets the min_sender_trust the prompt endpoint advertises
// and enforces (SAP §13.9). The default is [DefaultMinSenderTrust], or
// identity.TrustSigned when [WithStance] turns SignedSenders on; it is always
// advertised, so a host that implements the extension declares it. An explicit
// call wins over the stance, whatever the order of the options.
func WithMinSenderTrust(t identity.MinSenderTrust) Option {
	return func(c *config) error {
		if t != identity.TrustAny && t != identity.TrustSigned {
			return fmt.Errorf("%w: min_sender_trust must be %q or %q", ErrConfig, identity.TrustAny, identity.TrustSigned)
		}
		c.minTrust = t
		c.minTrustSet = true
		return nil
	}
}

// WithStance sets the security stance (see [identity.Stance]). The host reads
// two checks of it: SignedSenders makes the prompt endpoint require a signed
// sender, as [WithMinSenderTrust] with identity.TrustSigned does, and Signing
// makes [New] fail unless [WithIdentity] gave a Signer (the error matches
// [ErrConfig]). An explicit [WithMinSenderTrust] wins over SignedSenders. The
// default is [identity.DefaultProfile]. When no stance is chosen and no
// min_sender_trust is set, a logger set with [WithLogger] gets one warning per
// process that says so. SDK policy.
func WithStance(s identity.Stance) Option {
	return func(c *config) error {
		if _, err := stance.Resolve(s); err != nil {
			return fmt.Errorf("%w: %w", ErrConfig, err)
		}
		c.stance = s
		return nil
	}
}

// WithReplayWindow sets the timestamp skew and the nonce window of live
// verification; the default is identity.DefaultReplayWindow. See SAP §13.7.
func WithReplayWindow(d time.Duration) Option {
	return func(c *config) error {
		if d <= 0 {
			return fmt.Errorf("%w: replay window must be positive", ErrConfig)
		}
		c.replayWindow = d
		return nil
	}
}

// WithMaxNonces bounds the replay set; the default is identity.DefaultMaxNonces.
// A full set refuses new signed requests (fail closed).
func WithMaxNonces(n int) Option {
	return func(c *config) error {
		if n <= 0 {
			return fmt.Errorf("%w: max nonces must be positive", ErrConfig)
		}
		c.maxNonces = n
		return nil
	}
}

// WithMaxNoncesPerSender sets the per-sender quota of the replay set; the
// default is identity.DefaultMaxNoncesPerSender.
func WithMaxNoncesPerSender(n int) Option {
	return func(c *config) error {
		if n <= 0 {
			return fmt.Errorf("%w: max nonces per sender must be positive", ErrConfig)
		}
		c.maxPerSender = n
		return nil
	}
}

// WithAdmission sets the admission hook (SAP §13.7.3; REQ-016). See
// [AcceptSenderFunc].
func WithAdmission(f AcceptSenderFunc) Option {
	return func(c *config) error { c.accept = f; return nil }
}

// WithResolveTTL sets how long the reverse-lookup index of
// [Request.ResolveSender] is reused; the default is identity.DefaultResolveTTL.
// See SAP §13.10.
func WithResolveTTL(d time.Duration) Option {
	return func(c *config) error { c.resolveTTL = d; return nil }
}

// WithOperatorAttested is the deployment promise that every request reaching
// this host crossed a service import, so the server's stamp can be
// cross-checked against a signed header. Off by default. See SAP §13.7.2.
func WithOperatorAttested(b bool) Option {
	return func(c *config) error { c.operatorAttested = b; return nil }
}

// WithAccountTokenPosition sets the 1-based position of the account token an
// export with account_token_position inserts into the arrival subject. See SAP
// §13.7.1.
func WithAccountTokenPosition(pos int) Option {
	return func(c *config) error {
		if pos < 0 {
			return fmt.Errorf("%w: account token position must not be negative", ErrConfig)
		}
		c.accountTokenPos = pos
		return nil
	}
}

// WithInterceptors adds host interceptors; the first listed is the outermost.
func WithInterceptors(i ...Interceptor) Option {
	return func(c *config) error {
		for _, x := range i {
			if x == nil {
				return fmt.Errorf("%w: nil interceptor", ErrConfig)
			}
		}
		c.interceptors = append(c.interceptors, i...)
		return nil
	}
}

// WithHeartbeatExtras adds fields to every heartbeat and status reply, read
// when each is built. A provider that panics, returns a field name of SAP §8.3
// or a value that does not serialize costs that beat its extras, never the
// beat; the failure is logged at error level without its text.
func WithHeartbeatExtras(f func() map[string]any) Option {
	return func(c *config) error { c.hbExtras = f; return nil }
}

// WithMaxConcurrentPrompts sets how many prompts are served at once; the
// default is [DefaultMaxConcurrentPrompts]. n must be at least 1.
func WithMaxConcurrentPrompts(n int) Option {
	return func(c *config) error {
		if n < 1 {
			return fmt.Errorf("%w: concurrency must be at least 1", ErrConfig)
		}
		c.maxConc = n
		return nil
	}
}

// WithBusyPolicy sets what happens when every slot is taken; the default is
// [BusyQueue].
func WithBusyPolicy(p BusyPolicy) Option {
	return func(c *config) error {
		if p != BusyQueue && p != BusyReject429 {
			return fmt.Errorf("%w: unknown busy policy %d", ErrConfig, p)
		}
		c.busy = p
		return nil
	}
}

// Endpoint is an extra micro endpoint the host registers without
// interpreting it (REQ-004, REQ-016; the TypeScript extraEndpoints).
type Endpoint struct {
	// Name is the endpoint name. It must not be "prompt" or "status", a
	// reserved verb (SAP §2), or repeat.
	Name string
	// Subject is advertised verbatim; the host does not prefix it. Required.
	Subject string
	// QueueGroup is the queue group; empty selects micro's default "q", as
	// both official hosts do.
	QueueGroup string
	// Handler serves the endpoint. The host calls it on the subscription's
	// goroutine; it is not subject to the prompt concurrency limit.
	Handler micro.Handler
	// Metadata is the endpoint metadata advertised in $SRV.INFO.
	Metadata map[string]string
}

// WithReplySubjectPolicy sets the policy that vets the reply subject of every
// prompt and status request before the host serves it or publishes anything
// there. A request whose subject the policy refuses is dropped unanswered (the
// host has no safe place to answer), counted in [Stats.RefusedReplies] and
// logged at warn without its payload. The default is
// [DefaultReplySubjectPolicy] (extended with this instance's own subjects and the subjects of extra endpoints),
// which refuses "$" system subjects, wildcards and the subjects that would
// start a protocol loop but accepts ordinary inbox prefixes and other
// "agents." subjects, so callers may use custom inboxes. A nil policy is a
// configuration error. A policy that panics refuses the request.
//
// The policy is a second line of defense: a caller that may publish to the
// prompt subject chooses where the host replies, so run the host's NATS user
// with publish permissions limited to the inboxes it must answer. This is SDK
// policy; SAP delegates subject authorization to NATS (SAP §2, §6).
func WithReplySubjectPolicy(f func(subject string) error) Option {
	return func(c *config) error {
		if f == nil {
			return fmt.Errorf("%w: nil reply subject policy", ErrConfig)
		}
		c.replyPolicy = f
		return nil
	}
}
