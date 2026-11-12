// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
)

// UserInfoSubject is the subject that returns a connection's own user and
// account to any connected user. See SAP §13.4.
const UserInfoSubject = "$SYS.REQ.USER.INFO"

const (
	// DefaultUserInfoTimeout is the default timeout of the $SYS.REQ.USER.INFO
	// request, 2 seconds. See SAP §13.4.
	DefaultUserInfoTimeout = 2 * time.Second

	// DefaultSelfIDRetryInterval is the default interval after which a failed
	// lookup is retried (SDK policy; SAP §13.4 says "a short interval").
	DefaultSelfIDRetryInterval = 5 * time.Second
)

var (
	// ErrNoIdentity means the server answered and the connection has no NKEY
	// user (no authentication, a password or token user, or an account name
	// outside [A-Za-z0-9_-]+): the connection runs the protocol without
	// identity. The remedy is an NKEY user or a credentials file. See SAP
	// §13.4.
	ErrNoIdentity = errors.New("identity: the connection has no NKEY identity")

	// ErrIdentityUnknown means the implementation does not know the
	// connection's identity: the server did not answer, a permission blocks the
	// request, the answer was unusable, or two sources disagreed. It must not
	// guess, and the failure is not kept for the connection's lifetime: a later
	// call retries after the retry interval. See SAP §13.4.
	ErrIdentityUnknown = errors.New("identity: the connection's identity is unknown")
)

// Requester is the part of a NATS connection the identity lookup needs. A
// *nats.Conn satisfies it.
type Requester interface {
	RequestWithContext(ctx context.Context, subj string, data []byte) (*nats.Msg, error)
}

// SelfIDOptions configure a [SelfIDResolver].
type SelfIDOptions struct {
	// Signer is the connection's signer. When it is a [JWTSigner] and
	// ConsultServer is false, the identity comes from its JWT alone, as SAP
	// §13.4 prefers: the server is not asked, because $SYS.REQ.USER.INFO can be
	// raced by a same-account subscriber. A signer's user must equal the user
	// the server reports.
	Signer Signer

	// ConsultServer asks $SYS.REQ.USER.INFO even when a JWT is in hand. Both
	// sources must then find the same pair, otherwise the identity is unknown
	// ([ErrIdentityUnknown] wrapping [ErrIdentityMismatch]). Without a JWT the
	// server is always asked.
	ConsultServer bool

	// Timeout is the $SYS.REQ.USER.INFO timeout. Zero means
	// [DefaultUserInfoTimeout].
	Timeout time.Duration

	// RetryInterval is how long a failed lookup is remembered before a later
	// call asks again. Zero means [DefaultSelfIDRetryInterval]. The program
	// sets it.
	RetryInterval time.Duration

	// Logger receives lookup failures. Nil is silent.
	Logger *slog.Logger

	// Now returns the current time. Nil means [time.Now]. Test hook.
	Now func() time.Time
}

// SelfIDResolver learns a connection's own agent ID once per connection (SAP
// §13.4) and remembers it. A failure is remembered only for the retry interval,
// so an unknown identity recovers by itself. It is safe for concurrent use;
// concurrent callers share one lookup.
//
// After a reconnect to a server that may answer differently, call [Reset].
type SelfIDResolver struct {
	conn   Requester
	opts   SelfIDOptions
	now    func() time.Time
	mu     sync.Mutex
	id     AgentID
	failed error
	failAt time.Time
	flight chan struct{}
	cancel context.CancelCauseFunc
	gen    uint64 // bumped by Reset; a lookup commits only if it started in the current generation

	// waiters counts callers blocked on the lookup in flight. Tests read it to
	// prove that concurrent callers share one lookup instead of racing it.
	waiters atomic.Int32
}

// NewSelfIDResolver returns a resolver over conn.
func NewSelfIDResolver(conn Requester, opts SelfIDOptions) *SelfIDResolver {
	if opts.Timeout == 0 {
		opts.Timeout = DefaultUserInfoTimeout
	}
	if opts.RetryInterval == 0 {
		opts.RetryInterval = DefaultSelfIDRetryInterval
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &SelfIDResolver{conn: conn, opts: opts, now: now}
}

// Peek returns the remembered ID without asking the server, and false when
// there is none: never looked up, in flight, or failed.
func (r *SelfIDResolver) Peek() (AgentID, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.id, r.id != ""
}

// Reset forgets the remembered ID and any remembered failure, so the next call
// asks again. A lookup in flight belongs to the connection that was reset: its
// context is canceled, and whatever it returns, even a late reply, is never
// committed. Callers waiting on it ask again against the new connection.
func (r *SelfIDResolver) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gen++
	r.id, r.failed = "", nil
	if r.cancel != nil {
		r.cancel(errSelfIDReset)
	}
	r.flight, r.cancel = nil, nil
}

var errSelfIDReset = errors.New("identity: lookup abandoned: the resolver was reset")

// ID returns the connection's agent ID. It returns [ErrNoIdentity] or
// [ErrIdentityUnknown] (wrapped, with the reason) when there is none. A
// failure inside the retry interval is repeated without a new request. Until
// ID returns an ID, the connection sends no Agent-Sender header. Canceling
// ctx abandons the wait and is not remembered as a failure.
func (r *SelfIDResolver) ID(ctx context.Context) (AgentID, error) {
	for {
		r.mu.Lock()
		if r.id != "" {
			id := r.id
			r.mu.Unlock()
			return id, nil
		}
		if r.failed != nil && r.now().Sub(r.failAt) < r.opts.RetryInterval {
			err := r.failed
			r.mu.Unlock()
			return "", err
		}
		if r.flight != nil {
			ch := r.flight
			r.mu.Unlock()
			r.waiters.Add(1)
			select {
			case <-ch:
				r.waiters.Add(-1)
				continue
			case <-ctx.Done():
				r.waiters.Add(-1)
				return "", ctx.Err()
			}
		}
		ch := make(chan struct{})
		lookupCtx, cancelTimeout := context.WithTimeoutCause(ctx, r.opts.Timeout,
			fmt.Errorf("no reply from %s within %s", UserInfoSubject, r.opts.Timeout))
		lookupCtx, cancelCause := context.WithCancelCause(lookupCtx)
		r.flight, r.cancel = ch, cancelCause
		gen := r.gen
		r.mu.Unlock()

		id, err := r.lookup(lookupCtx)
		callerGone := ctx.Err() != nil
		cancelTimeout()
		cancelCause(nil)

		r.mu.Lock()
		if r.gen != gen {
			// Reset ran during the lookup: the result describes the old
			// connection. Discard it and, unless the caller is gone, ask again.
			close(ch)
			r.mu.Unlock()
			if callerGone {
				return "", ctx.Err()
			}
			continue
		}
		r.flight, r.cancel = nil, nil
		switch {
		case err == nil:
			r.id, r.failed = id, nil
		case callerGone:
			// Not a verdict about the connection.
		default:
			r.failed, r.failAt = err, r.now()
			if r.opts.Logger != nil {
				r.opts.Logger.Warn("own sender identity unavailable; no Agent-Sender header is sent until a retry succeeds",
					slog.String("reason", err.Error()), slog.Duration("retry_after", r.opts.RetryInterval))
			}
		}
		close(ch)
		r.mu.Unlock()
		if callerGone && err != nil {
			return "", ctx.Err()
		}
		return id, err
	}
}

// ObserveError feeds an asynchronous NATS error to the resolver. A permission
// that blocks $SYS.REQ.USER.INFO yields no reply but an asynchronous -ERR
// 'Permissions Violation for Publish ...'; the program calls ObserveError from
// its nats.ErrorHandler, and an in-flight lookup then fails at once as unknown
// instead of waiting for the timeout (SAP §13.4, SHOULD). It reports whether
// the error concerned the lookup.
func (r *SelfIDResolver) ObserveError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	if !strings.Contains(text, "permissions violation for publish") || !strings.Contains(text, strings.ToLower(UserInfoSubject)) {
		return false
	}
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel(fmt.Errorf("publish to %s is a permissions violation for this user", UserInfoSubject))
	return true
}

func (r *SelfIDResolver) lookup(ctx context.Context) (AgentID, error) {
	var jwtID AgentID
	if js, ok := r.opts.Signer.(JWTSigner); ok && js.JWT() != "" {
		id, err := IdentityFromJWT(js.JWT())
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrIdentityUnknown, err)
		}
		jwtID = id
		if id.User() != js.PublicKey() {
			return "", fmt.Errorf("%w: %w: the seed derives %s but the JWT names %s", ErrIdentityUnknown, ErrIdentityMismatch, js.PublicKey(), id.User())
		}
		if !r.opts.ConsultServer {
			return jwtID, nil
		}
	}

	live, err := r.userInfo(ctx)
	if err != nil {
		return "", err
	}
	if s := r.opts.Signer; s != nil && live.User() != s.PublicKey() {
		return "", fmt.Errorf("%w: %w: the server reports user %s, the signer is %s", ErrIdentityUnknown, ErrIdentityMismatch, live.User(), s.PublicKey())
	}
	if jwtID != "" && jwtID != live {
		return "", fmt.Errorf("%w: %w: the JWT and %s name different pairs", ErrIdentityUnknown, ErrIdentityMismatch, UserInfoSubject)
	}
	return live, nil
}

func (r *SelfIDResolver) userInfo(ctx context.Context) (AgentID, error) {
	msg, err := r.conn.RequestWithContext(ctx, UserInfoSubject, nil)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
			err = cause
		}
		if errors.Is(err, nats.ErrNoResponders) {
			return "", fmt.Errorf("%w: no responder for %s (server without the system responder?)", ErrIdentityUnknown, UserInfoSubject)
		}
		return "", fmt.Errorf("%w: %w", ErrIdentityUnknown, err)
	}
	return IdentityFromUserInfoReply(msg.Data)
}

// IdentityFromUserInfoReply derives the agent ID from a $SYS.REQ.USER.INFO
// reply ({"data":{"user":...,"account":...}}). Unknown fields are ignored. The
// result follows the table of SAP §13.4:
//
//   - an NKEY user and an account the form can carry: the ID;
//   - an empty user, a "[REDACTED]" user, a password user name, or an account
//     name outside [A-Za-z0-9_-]+ and "$G": [ErrNoIdentity];
//   - an unusable reply, or one carrying an error: [ErrIdentityUnknown].
func IdentityFromUserInfoReply(reply []byte) (AgentID, error) { //nolint:revive // documented public name (docs/protocol-mapping.md), kept for parity with upstream identityFromUserInfoReply
	if checkNoDuplicateKeys(reply) != nil {
		return "", fmt.Errorf("%w: the %s reply repeats a JSON member name", ErrIdentityUnknown, UserInfoSubject)
	}
	var r struct {
		Error *json.RawMessage `json:"error"`
		Data  *struct {
			User    *string `json:"user"`
			Account *string `json:"account"`
		} `json:"data"`
	}
	if err := json.Unmarshal(reply, &r); err != nil {
		return "", fmt.Errorf("%w: the %s reply is not JSON", ErrIdentityUnknown, UserInfoSubject)
	}
	if r.Error != nil {
		return "", fmt.Errorf("%w: %s answered with an error", ErrIdentityUnknown, UserInfoSubject)
	}
	if r.Data == nil || r.Data.User == nil || r.Data.Account == nil {
		return "", fmt.Errorf("%w: the %s reply lacks string data.user and data.account", ErrIdentityUnknown, UserInfoSubject)
	}
	user, account := *r.Data.User, *r.Data.Account
	switch {
	case user == "":
		return "", fmt.Errorf("%w: no authentication, the server reports an empty user", ErrNoIdentity)
	case user == "[REDACTED]":
		return "", fmt.Errorf("%w: token authentication, the server reports a redacted user", ErrNoIdentity)
	case !userKeyShape.MatchString(user):
		return "", fmt.Errorf("%w: password authentication, the server reports a user name, not an NKEY", ErrNoIdentity)
	}
	id, err := NewAgentID(account, user)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoIdentity, err)
	}
	return id, nil
}
