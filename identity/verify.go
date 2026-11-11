// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// Trust is the trust class of a sender. See SAP §13.1.
type Trust string

const (
	// TrustVerified means the Agent-Sender header carries a valid signature:
	// the sender holds the seed for the user.
	TrustVerified Trust = "verified"
	// TrustClaimed means the header has no signature. Display only: a receiver
	// grants nothing on it. A Nats-Request-Info stamp is never more than a
	// claim and never raises a message above claimed or absent; a request with
	// only the stamp is absent.
	TrustClaimed Trust = "claimed"
	// TrustAbsent means no Agent-Sender header, or one with an unknown v. The
	// message has no sender. [VerifyLive] and [VerifyStored] return a nil *Sender for it.
	TrustAbsent Trust = "absent"
)

// Message is the part of a message verification needs: the subject it arrived
// on (the stored subject, for a stored record), the raw payload, and the
// headers. A *nats.Msg, a service request and a JetStream message all convert
// with [MessageFromNATS].
type Message struct {
	Subject string
	Data    []byte
	Header  nats.Header
}

// MessageFromNATS converts a NATS message.
func MessageFromNATS(m *nats.Msg) Message {
	return Message{Subject: m.Subject, Data: m.Data, Header: m.Header}
}

// Sender is a classified sender. A nil *Sender is the absent class. See SAP
// §13.1 and §13.7.
type Sender struct {
	// Trust is [TrustVerified] or [TrustClaimed].
	Trust Trust
	// ID is the sender's agent ID. It is set only for a verified sender: the
	// claimed class deliberately has no ID, so a claim cannot be used as a key.
	ID AgentID
	// Claim holds the account and user a claimed sender wrote in its header. It
	// is display data only.
	Claim ClaimedIdentity
	// Name is the display name from the header. It is never identity.
	Name string
	// Header is the parsed header.
	Header SenderHeader
	// AccountAttested is true only under [LiveOptions.OperatorAttested], when a
	// server stamp (an inserted account token, or Nats-Request-Info) agreed with
	// the signed account. Otherwise the account is the sender's own word beside
	// a verified user. See SAP §13.7.2.
	AccountAttested bool
}

// ClaimedIdentity is the account and user in a header without a valid
// signature.
type ClaimedIdentity struct {
	Account string
	User    string
}

// TrustOf returns the trust class of s: [TrustAbsent] for nil.
func TrustOf(s *Sender) Trust {
	if s == nil {
		return TrustAbsent
	}
	return s.Trust
}

// Describe returns a short text for logs that always carries the trust class,
// as SAP §13.12 asks of whatever shows an identity.
func (s *Sender) Describe() string {
	switch TrustOf(s) {
	case TrustVerified:
		return "verified " + string(s.ID)
	case TrustClaimed:
		return "claimed " + s.Claim.Account + "." + s.Claim.User
	default:
		return "absent"
	}
}

// SubjectRules configure which sub values are acceptable for an arrival
// subject (SAP §13.7.1). They are the same in live and stored verification.
type SubjectRules struct {
	// AccountTokenPosition is the 1-based position of the caller's account token
	// that an export with account_token_position inserts into the arrival
	// subject. Zero means none. When set, the token at that position of the
	// arrival subject MUST equal the header's account in every form, and sub is
	// acceptable either as the arrival subject or as the arrival subject with
	// that token removed (SAP §13.7.1 (a), (b)). A position beyond the subject's
	// token count fails the check.
	AccountTokenPosition int

	// RenamedSubject implements SAP §13.7.1 (c), for a subject the signer also
	// publishes for its own account, such as a heartbeat. It rebuilds from the
	// arrival subject the subject the signer published in its own account; sub
	// MUST equal it. It is an equality, never a match on a shape. Return false
	// when the arrival subject is not one the receiver knows a mapping for. Nil
	// disables form (c).
	RenamedSubject func(arrival string) (string, bool)
}

// LiveOptions configure [VerifyLive].
type LiveOptions struct {
	SubjectRules

	// ReplayWindow is the ts skew tolerated. Zero means [DefaultReplayWindow].
	ReplayWindow time.Duration

	// Now returns the current time. Nil means [time.Now]. Test hook.
	Now func() time.Time

	// Nonces is the replay set (SAP §13.7). It is required: [VerifyLive]
	// returns a configuration error ([ErrNoNonceSet]) when it is nil, so live
	// verification cannot succeed without replay protection. The set's window
	// must be at least ReplayWindow, otherwise [VerifyLive] returns
	// [ErrNonceWindow]: a nonce must be remembered for as long as its header
	// can still pass the timestamp check. For a verified header [VerifyLive]
	// records (user, nonce) atomically, after the signature verified, so a
	// forged or stale header cannot poison the set; a pair already present, a
	// full set and a sender over its quota are refused with 401.
	Nonces *NonceSet

	// OperatorAttested is a deployment promise the SDK cannot verify: every
	// request reaching this receiver crossed a service import, so
	// Nats-Request-Info and an inserted account token are the server's stamp,
	// never a peer's forgery. With it set, a signed header is cross-checked
	// against the stamp: a present acc must equal the signed account and a
	// present user the signed user, otherwise 401. Agreement sets
	// [Sender.AccountAttested]. Off by default. See SAP §13.7.2.
	OperatorAttested bool

	// PreAccept is an optional allow-list hook for the signing key, run for a
	// signed header after its signature verified and before the nonce is
	// recorded. Returning false refuses the request with 403 and records
	// nothing, so a key the deployment does not know cannot consume nonce-set
	// capacity. Nil admits every key that verifies. See [GateOptions.PreAccept]
	// for the exposure when it is nil.
	PreAccept func(user, account string) bool
}

// StoredOptions configure [VerifyStored]. Stored verification is authorship
// only (SAP §13.8): there is no timestamp window and no nonce set, so a record
// older than the window verifies and a redelivered record verifies again.
// Deduplication on (user, nonce) is the consumer's.
type StoredOptions struct {
	SubjectRules
}

// VerifyLive classifies a live request and verifies its Agent-Sender header
// (SAP §13.7). The outcomes:
//
//   - no header, or an unknown v: (nil, nil), the absent class;
//   - a malformed header: an *[Error] with code 400;
//   - a header without sig: a claimed [Sender];
//   - a header with sig: the signature is verified against user, and sub, ts
//     and the nonce are checked. All pass: a verified [Sender]. Any fails: an
//     *[Error] with code 401, whatever the endpoint's min_sender_trust.
//
// The ts must lie within the replay window, and a verified header's nonce is
// recorded in [LiveOptions.Nonces] (check-and-record in one operation), so the
// same signed request verifies once. A nil Nonces, or a set whose window is
// shorter than the replay window, is a configuration error and not an *[Error].
// See SAP §13.7.
func VerifyLive(msg Message, opts LiveOptions) (*Sender, error) {
	window := opts.ReplayWindow
	if window == 0 {
		window = DefaultReplayWindow
	}
	if opts.Nonces == nil {
		return nil, ErrNoNonceSet
	}
	if opts.Nonces.Window() < window {
		return nil, fmt.Errorf("%w: set window %s, replay window %s", ErrNonceWindow, opts.Nonces.Window(), window)
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	s, commit, err := verifyDeferred(msg, opts.SubjectRules, &liveChecks{
		window: window, now: now, nonces: opts.Nonces,
		operatorAttested: opts.OperatorAttested, preAccept: opts.PreAccept,
	})
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

// VerifyStored verifies the authorship of a stored message (SAP §13.8): the
// signature and sub, from the stored subject, payload and header. It does not
// apply the ts window and does not consult a nonce set. The outcomes are those
// of [VerifyLive] except that the freshness failures cannot occur.
//
// A valid signature proves who signed the content. It does not prove that the
// record is unique, or that the signer published it into this stream. A stream
// whose records carry Agent-Sender MUST NOT transform subjects on the way in.
func VerifyStored(msg Message, opts StoredOptions) (*Sender, error) {
	return verify(msg, opts.SubjectRules, nil)
}

// liveChecks carries the freshness checks that only live verification runs. A
// nil *liveChecks is stored mode.
type liveChecks struct {
	window           time.Duration
	now              func() time.Time
	nonces           *NonceSet
	operatorAttested bool
	preAccept        func(user, account string) bool
}

func verify(msg Message, rules SubjectRules, live *liveChecks) (*Sender, error) {
	s, commit, err := verifyDeferred(msg, rules, live)
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

// verifyDeferred runs every verification check but does not record the nonce.
// For a verified live header it returns a commit function that atomically
// records (user, nonce) and returns the refusal when the pair is a replay, the
// set is full or the sender is over its quota. A caller that applies further
// policy (min_sender_trust, an admission hook) calls commit only once that
// policy admitted the request, so a refused request consumes no capacity. The
// early Seen check still refuses a known replay before any policy runs, and
// commit is the authoritative atomic check-and-record, so concurrent identical
// requests are admitted at most once.
func verifyDeferred(msg Message, rules SubjectRules, live *liveChecks) (*Sender, func() *Error, error) {
	if rules.AccountTokenPosition < 0 {
		return nil, nil, errors.New("identity: AccountTokenPosition must be 0 (none) or a 1-based position")
	}
	value, ok, err := ReadHeaderValue(msg.Header)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, nil
	}
	h, err := ParseHeader(value)
	if err != nil {
		return nil, nil, err
	}
	if h == nil {
		return nil, nil, nil
	}
	if !h.Signed() {
		return &Sender{
			Trust: TrustClaimed, Claim: ClaimedIdentity{Account: h.Account, User: h.User},
			Name: h.Name, Header: *h,
		}, nil, nil
	}

	if live != nil {
		ts, perr := ParseTimestamp(h.TS)
		if perr != nil {
			return nil, nil, malformed("`ts` is not an RFC 3339 UTC timestamp")
		}
		skew := live.now().Sub(ts).Abs()
		if skew > live.window {
			return nil, nil, rejected("ts %s is %d s from now (window %d s)", h.TS, int64(skew/time.Second), int64(live.window/time.Second))
		}
	}

	if problem := checkSubject(h.Account, h.Sub, msg.Subject, rules); problem != "" {
		return nil, nil, rejected("%s", problem)
	}

	attested := false
	if live != nil && live.operatorAttested {
		stamp, serr := ReadRequestInfo(msg.Header)
		if serr != nil {
			return nil, nil, rejected("Nats-Request-Info is present but is not a server stamp (operator-attested mode)")
		}
		if stamp != nil {
			if stamp.Account != "" && stamp.Account != h.Account {
				return nil, nil, rejected("Nats-Request-Info acc %q disagrees with the signed account %q", stamp.Account, h.Account)
			}
			if stamp.User != "" && stamp.User != h.User {
				return nil, nil, rejected("Nats-Request-Info user %q disagrees with the signed user %s", stamp.User, h.User)
			}
		}
		attested = rules.AccountTokenPosition > 0 || (stamp != nil && stamp.Account != "")
	}

	if live != nil && live.nonces.Seen(h.User, h.Nonce) {
		return nil, nil, rejected("nonce already seen for %s", h.User)
	}

	sig, derr := decodeSignature(h.Sig)
	if derr != nil {
		return nil, nil, malformed("`sig` is not base64url without padding that decodes to 64 bytes")
	}
	input := SignedInput(h.Account, h.User, h.Sub, h.TS, h.Nonce, msg.Data)
	if !verifyWithPublicKey(h.User, input, sig) {
		return nil, nil, rejected("signature does not verify for %s", h.User)
	}

	id, err := NewAgentID(h.Account, h.User)
	if err != nil {
		return nil, nil, malformed(err.Error())
	}
	if live == nil {
		return &Sender{Trust: TrustVerified, ID: id, Name: h.Name, Header: *h, AccountAttested: attested}, nil, nil
	}
	if live.preAccept != nil && !live.preAccept(h.User, h.Account) {
		return nil, nil, newError(403, ErrSenderRefused, DescriptionRejected, "signing key "+h.User+" is not accepted")
	}
	ts, _ := ParseTimestamp(h.TS) // validated above
	commit := func() *Error {
		// The authoritative check-and-record.
		switch live.nonces.Record(h.User, h.Nonce, ts) {
		case Replayed:
			return rejected("nonce already seen for %s", h.User)
		case Expired:
			return rejected("ts %s is outside the replay window (window %d s)", h.TS, int64(live.window/time.Second))
		case SetFull:
			return rejected("nonce set is full; refusing as a replay (fail closed)")
		case SenderQuotaExceeded:
			return rejected("nonce quota of %s is exhausted; refusing (fail closed)", h.User)
		}
		return nil
	}
	return &Sender{Trust: TrustVerified, ID: id, Name: h.Name, Header: *h, AccountAttested: attested}, commit, nil
}

// checkSubject applies SAP §13.7.1 and returns a reason, or "" when sub is
// acceptable for the arrival subject. sub is always compared with the arrival
// subject of this message, never with a pattern.
func checkSubject(account, sub, arrival string, rules SubjectRules) string {
	if pos := rules.AccountTokenPosition; pos > 0 {
		tokens := strings.Split(arrival, ".")
		if pos > len(tokens) {
			return fmt.Sprintf("accountTokenPosition %d is beyond the %d-token arrival subject", pos, len(tokens))
		}
		if tokens[pos-1] != account {
			return fmt.Sprintf("arrival subject token %q at position %d is not the header account %q", tokens[pos-1], pos, account)
		}
		stripped := strings.Join(append(append([]string{}, tokens[:pos-1]...), tokens[pos:]...), ".")
		if sub == arrival || sub == stripped {
			return ""
		}
		if want, ok := renamed(rules, arrival); ok && sub == want {
			return ""
		}
		return fmt.Sprintf("sub %q is neither the arrival subject %q nor %q", sub, arrival, stripped)
	}
	if sub == arrival {
		return ""
	}
	if want, ok := renamed(rules, arrival); ok && sub == want {
		return ""
	}
	return fmt.Sprintf("sub %q is not the arrival subject %q", sub, arrival)
}

func renamed(rules SubjectRules, arrival string) (string, bool) {
	if rules.RenamedSubject == nil {
		return "", false
	}
	want, ok := rules.RenamedSubject(arrival)
	return want, ok && want != ""
}
