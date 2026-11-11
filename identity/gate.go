// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// AcceptFunc is the admission hook: it decides whether the receiver accepts a
// sender. SAP §13.7.3 leaves acceptance, which is authorization, with the
// deployment; the SDK decides no policy itself.
//
// The hook runs for every classified prompt request (verified, claimed or
// absent; s is nil for absent) and never for status. Returning false refuses a
// verified sender with 403 and a claimed or absent sender with 401. A returned
// error answers 500 and the request is not served. The hook runs before the ack
// of SAP §6.4, so per-request network I/O in it delays the ack.
//
// The nonce of a request is recorded only after the hook admits it, so a request
// the hook refuses consumes no replay-set capacity. Residual exposure: a
// signature proves possession of some key, not authorization. With no hook (and
// no [GateOptions.PreAccept]) any holder of many throwaway keys can still fill
// the set with requests that are admitted, and the global cap then fails closed
// for every sender until entries expire. A deployment that faces untrusted
// publishers sets a hook, [GateOptions.PreAccept] or NATS-level publish limits.
type AcceptFunc func(ctx context.Context, s *Sender) (bool, error)

// GateOptions configure a [Gate].
type GateOptions struct {
	// MinSenderTrust is what the prompt endpoint requires. Empty means
	// [TrustAny]. Any other value than [TrustAny] and [TrustSigned] is an error.
	MinSenderTrust MinSenderTrust

	// Subject holds the sub acceptance rules (SAP §13.7.1).
	Subject SubjectRules

	// ReplayWindow is the ts skew and the nonce window. Zero means
	// [DefaultReplayWindow].
	ReplayWindow time.Duration

	// Nonces is the replay set. Nil means a new set sized by MaxNonces and
	// MaxNoncesPerSender with the gate's window. Pass one set to several gates
	// of one instance to share it. A supplied set whose window is shorter than
	// ReplayWindow is refused by [NewGate] with [ErrNonceWindow]: the gate would
	// otherwise accept a replay once the entry expired but the timestamp was
	// still inside the window.
	Nonces *NonceSet

	// MaxNonces bounds the replay set when Nonces is nil. Zero means
	// [DefaultMaxNonces].
	MaxNonces int

	// MaxNoncesPerSender is the per-sender quota of the new replay set when
	// Nonces is nil. Zero means [DefaultMaxNoncesPerSender]. See [NonceSet].
	MaxNoncesPerSender int

	// OperatorAttested is the deployment promise of [LiveOptions.OperatorAttested].
	OperatorAttested bool

	// Accept is the admission hook. Nil accepts every sender that passed the
	// other checks.
	Accept AcceptFunc

	// PreAccept is an optional cheap allow-list on the signing key, run for a
	// signed header after the signature verified and before the nonce is
	// recorded (and before min_sender_trust and Accept). It receives the user
	// public key and the account the header names. Returning false refuses the
	// request with 403 and records nothing, so an unknown key cannot consume
	// replay-set capacity. Nil admits every key that verifies. It does not run
	// for unsigned (claimed) headers, which record no nonce. SDK policy, not SAP.
	//
	// Residual exposure when both PreAccept and Accept are nil: any holder of
	// many throwaway keys can fill the replay set with admitted requests, and the
	// global cap fails closed (401 "nonce set is full") for every sender until
	// entries expire. See [NonceSet].
	PreAccept func(user, account string) bool

	// Logger receives every refusal with its detail. Nil is silent. Seeds,
	// signatures, nonces and payloads are never logged.
	Logger *slog.Logger

	// Now returns the current time. Nil means [time.Now]. Test hook.
	Now func() time.Time
}

// Gate classifies the requests of one receiver (SAP §13.7): it parses and
// verifies the Agent-Sender header (live mode), enforces min_sender_trust, runs
// the admission hook and records the nonce in the replay set last, so a request
// refused by policy consumes no replay-set capacity. A signature proves a key,
// not authorization: without [GateOptions.PreAccept] or [GateOptions.Accept] any
// key holder can consume replay-set capacity with admitted requests and the
// global cap fails closed. See [NonceSet]. It is safe for concurrent use.
type Gate struct {
	min    MinSenderTrust
	rules  SubjectRules
	window time.Duration
	nonces *NonceSet
	opAtt  bool
	accept AcceptFunc
	pre    func(user, account string) bool
	log    *slog.Logger
	now    func() time.Time
}

// NewGate returns a Gate.
func NewGate(opts GateOptions) (*Gate, error) {
	g := &Gate{min: opts.MinSenderTrust, rules: opts.Subject, window: opts.ReplayWindow,
		nonces: opts.Nonces, opAtt: opts.OperatorAttested, accept: opts.Accept, pre: opts.PreAccept, log: opts.Logger, now: opts.Now}
	switch g.min {
	case "":
		g.min = TrustAny
	case TrustAny, TrustSigned:
	default:
		return nil, fmt.Errorf("identity: unknown MinSenderTrust %q", g.min)
	}
	if g.rules.AccountTokenPosition < 0 {
		return nil, errors.New("identity: AccountTokenPosition must be 0 (none) or a 1-based position")
	}
	if g.window == 0 {
		g.window = DefaultReplayWindow
	}
	if g.now == nil {
		g.now = time.Now
	}
	if g.nonces == nil {
		g.nonces = NewNonceSet(NonceSetOptions{Window: g.window, MaxEntries: opts.MaxNonces,
			MaxPerSender: opts.MaxNoncesPerSender, Logger: g.log, Now: g.now})
	} else if g.nonces.Window() < g.window {
		return nil, fmt.Errorf("%w: set window %s, gate window %s", ErrNonceWindow, g.nonces.Window(), g.window)
	}
	return g, nil
}

// MinSenderTrust returns the requirement the gate enforces on prompt requests.
func (g *Gate) MinSenderTrust() MinSenderTrust { return g.min }

// Nonces returns the replay set.
func (g *Gate) Nonces() *NonceSet { return g.nonces }

// Admission is the outcome of [Gate.AdmitPrompt].
type Admission struct {
	// Sender is the classified sender: nil for the absent class. It is set
	// whenever Err is nil.
	Sender *Sender
	// Err is the refusal. It is nil when the request is admitted. The caller
	// answers it with an error-headered message and the terminator (SAP §9).
	Err *Error
}

// OK reports whether the request is admitted.
func (a Admission) OK() bool { return a.Err == nil }

// Classify parses and verifies the sender of a live request and records its
// nonce (a verified request classifies once), without applying min_sender_trust
// or the hook. A malformed
// header returns an *[Error] with code 400, a failing check one with code 401,
// and an absent sender (nil, nil).
func (g *Gate) Classify(msg Message) (*Sender, error) {
	s, commit, err := g.classifyDeferred(msg)
	if err != nil {
		return nil, err
	}
	if commit != nil {
		if cerr := commit(); cerr != nil {
			return nil, cerr
		}
	}
	return s, nil
}

// classifyDeferred verifies without recording the nonce; commit (nil when there
// is nothing to record) records it atomically.
func (g *Gate) classifyDeferred(msg Message) (*Sender, func() *Error, error) {
	s, commit, err := verifyDeferred(msg, g.rules, &liveChecks{
		window: g.window, now: g.now, nonces: g.nonces,
		operatorAttested: g.opAtt, preAccept: g.pre,
	})
	if err != nil {
		var ie *Error
		if errors.As(err, &ie) {
			return nil, nil, ie
		}
		return nil, nil, newError(500, ErrAdmission, DescriptionServerError, err.Error())
	}
	return s, commit, nil
}

// AdmitPrompt runs the full admission of a prompt request, in this order:
// header parse (400), ts, sub, nonce lookup, signature and the atomic nonce
// record (401 for a replay, a full set or a sender over its quota), then
// min_sender_trust (401 when signed is required and the sender is not
// verified), the admission hook (403, 401 or 500), and last the atomic nonce
// record. The nonce is recorded only after the signature, [GateOptions.PreAccept],
// min_sender_trust and the hook all passed, so a stale or transplanted header,
// or a request refused by policy, cannot poison the set or consume its capacity.
// A known replay is still refused early (the Seen check), and the final record
// is atomic, so concurrent identical requests are admitted at most once. Every
// refusal is logged with its detail.
func (g *Gate) AdmitPrompt(ctx context.Context, msg Message) Admission {
	sender, commit, err := g.classifyDeferred(msg)
	if err != nil {
		return g.refuse(msg, err.(*Error)) //nolint:errcheck // invariant: sender verification returns only *Error
	}

	if g.min == TrustSigned && TrustOf(sender) != TrustVerified {
		return g.refuse(msg, newError(401, ErrSignatureRequired, DescriptionSignatureRequired,
			"endpoint requires a verified sender; got "+sender.Describe()))
	}

	if g.accept != nil {
		ok, aerr := g.accept(ctx, sender)
		if aerr != nil {
			g.logWarn("admission hook failed; request not served", msg, 500, "hook error")
			return Admission{Err: newError(500, ErrAdmission, DescriptionServerError, "admission hook failed")}
		}
		if !ok {
			if TrustOf(sender) == TrustVerified {
				return g.refuse(msg, newError(403, ErrSenderRefused, DescriptionRejected,
					"verified sender not accepted: "+sender.Describe()))
			}
			return g.refuse(msg, newError(401, ErrSignatureRequired, DescriptionSignatureRequired,
				"unauthenticated sender not accepted: "+sender.Describe()))
		}
	}
	if commit != nil {
		if cerr := commit(); cerr != nil {
			return g.refuse(msg, cerr)
		}
	}
	return Admission{Sender: sender}
}

// StatusResult is the outcome of [Gate.ClassifyStatus].
type StatusResult struct {
	// Sender is the classified sender, nil for absent, and nil too when the
	// classification failed.
	Sender *Sender
	// Rejection is the failed classification, kept so the host can log it. It
	// is NOT a reason to refuse: the status request is answered either way.
	Rejection *Error
}

// Answerable reports whether the host answers the status request. It is always
// true: a status endpoint declares nothing and is always answerable, because a
// liveness probe MUST NOT depend on the prober's credentials. See SAP §13.9
// and §8.7.
func (StatusResult) Answerable() bool { return true }

// ClassifyStatus classifies the sender of a status request so the agent knows
// who probed it, records a verified nonce, and never refuses. An unsigned
// sender, a failing classification and a replay are logged at most; the reply
// is sent anyway. min_sender_trust and the admission hook do not apply. See SAP
// §13.9.
func (g *Gate) ClassifyStatus(msg Message) StatusResult {
	sender, err := g.Classify(msg)
	if err != nil {
		rej := err.(*Error) //nolint:errcheck // invariant: sender verification returns only *Error
		g.logWarn("status request: Agent-Sender rejected (reply sent anyway)", msg, rej.Code, rej.Detail)
		return StatusResult{Rejection: rej}
	}
	return StatusResult{Sender: sender}
}

func (g *Gate) refuse(msg Message, e *Error) Admission {
	g.logWarn("prompt request refused on sender identity", msg, e.Code, e.Detail)
	return Admission{Err: e}
}

func (g *Gate) logWarn(text string, msg Message, code int, reason string) {
	if g.log == nil {
		return
	}
	g.log.Warn(text, slog.String("subject", msg.Subject), slog.Int("code", code), slog.String("reason", reason))
}
