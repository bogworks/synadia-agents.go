// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"container/heap"
	"log/slog"
	"sync"
	"time"
)

// DefaultMaxNonces is the default maximum size of a [NonceSet].
const DefaultMaxNonces = 100_000

// DefaultMaxNoncesPerSender is the default per-sender quota of a [NonceSet]: the
// most unexpired entries one user public key may hold. It is SDK policy. With
// the default 30 s window it admits about 333 signed requests per second from
// one key.
const DefaultMaxNoncesPerSender = 10_000

// RecordResult is the outcome of [NonceSet.Record].
type RecordResult int

const (
	// Recorded means the pair was new and is now in the set.
	Recorded RecordResult = iota
	// Replayed means the pair is already in the set and unexpired: a replay.
	Replayed
	// SetFull means the set is at its maximum size. The receiver refuses the
	// message as a replay (fail closed). See [NonceSet].
	SetFull
	// SenderQuotaExceeded means the sender (user public key) already holds its
	// quota of unexpired entries. The receiver refuses the message (fail
	// closed); other senders are not affected. See [NonceSet].
	SenderQuotaExceeded
	// Expired means the pair's window had already passed when it was offered
	// (ts plus the set's window is before now). Nothing is recorded: there is
	// nothing left to remember, since live verification refuses such a ts as
	// stale. The receiver refuses the message as a stale timestamp.
	Expired
)

// NonceSetOptions configure a [NonceSet].
type NonceSetOptions struct {
	// Window is the replay window. An entry for a header with timestamp ts
	// lives until ts + Window, which is as long as live verification can still
	// accept that header. Zero means [DefaultReplayWindow].
	Window time.Duration
	// MaxEntries bounds the set. Zero means [DefaultMaxNonces].
	MaxEntries int
	// MaxPerSender bounds the unexpired entries of one sender, identified by
	// its user public key. Zero means [DefaultMaxNoncesPerSender]; a value
	// above MaxEntries is lowered to MaxEntries.
	MaxPerSender int
	// Logger receives one warning each time the set fills. Nil is silent. A
	// nonce is never logged.
	Logger *slog.Logger
	// Now returns the current time. Nil means [time.Now]. Test hook.
	Now func() time.Time
}

type nonceKey struct{ user, nonce string }

type nonceExpiry struct {
	at  time.Time
	key nonceKey
}

type nonceHeap []nonceExpiry

func (h nonceHeap) Len() int           { return len(h) }
func (h nonceHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h nonceHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *nonceHeap) Push(x any)        { *h = append(*h, x.(nonceExpiry)) } // invariant: only nonceExpiry is ever pushed
func (h *nonceHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// NonceSet is the live-verification replay set of SAP §13.7, keyed by (user,
// nonce). It is safe for concurrent use.
//
// SDK policy: the set is bounded in time, in size, and per sender. Each pair is kept only
// until its timestamp's window has passed. The set has a maximum size and when
// it is full it refuses new pairs ([SetFull]) rather than dropping old entries,
// so an attacker cannot evict a nonce to replay it. It logs when that happens.
// The cost is that a flood of validly signed requests can make a host refuse
// further signed requests until entries expire. The set lives in one instance:
// instances behind a queue group do not share it, and a restart empties it; the
// ts window bounds both exposures (SAP §13.7).
//
// Capacity is partitioned by sender: one user public key may hold at most
// [NonceSetOptions.MaxPerSender] unexpired entries, and a sender at its quota
// is refused ([SenderQuotaExceeded]) while other senders are still admitted. The
// partition is the user public key alone, not account plus user: the account in
// a header is the sender's own word (SAP §13.7.2), so keying the quota on it
// would let one key mint fresh partitions by varying the account.
//
// A [Gate] records a nonce only after its policy (PreAccept, min_sender_trust and
// the admission hook) admitted the request, so refused requests consume nothing.
//
// Residual risk: the global cap remains. A signature proves possession of some
// key, not authorization: with no [GateOptions.PreAccept] and no admission hook
// any key holder can consume quota, and the cap fails closed for everyone. Enough distinct valid senders, each
// within its quota, can still fill the set, and every further signed request is
// then refused until entries expire. A holder of many keys is not stopped by a
// per-key quota. Deployments that face untrusted publishers should also limit
// publish rates at the NATS level (per-user or per-account rate limits and
// max_payload and connection limits) so that no principal can send signed
// requests faster than the receiver intends to admit.
type NonceSet struct {
	mu      sync.Mutex
	window  time.Duration
	max     int
	perMax  int
	counts  map[string]int
	log     *slog.Logger
	now     func() time.Time
	entries map[nonceKey]time.Time
	expiry  nonceHeap
	warned  bool

	quotaWarned bool // one warning per episode, re-armed when the set drains to half
}

// NewNonceSet returns an empty set.
func NewNonceSet(opts NonceSetOptions) *NonceSet {
	s := &NonceSet{window: opts.Window, max: opts.MaxEntries, perMax: opts.MaxPerSender, log: opts.Logger, now: opts.Now,
		entries: make(map[nonceKey]time.Time), counts: make(map[string]int)}
	if s.window == 0 {
		s.window = DefaultReplayWindow
	}
	if s.max <= 0 {
		s.max = DefaultMaxNonces
	}
	if s.perMax <= 0 {
		s.perMax = DefaultMaxNoncesPerSender
	}
	if s.perMax > s.max {
		s.perMax = s.max
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// Window returns the replay window of the set: how long past its timestamp an
// entry is kept. A [Gate] or [VerifyLive] call refuses a set whose window is
// shorter than the window it accepts timestamps in.
func (s *NonceSet) Window() time.Duration { return s.window }

// Len returns the number of unexpired entries.
func (s *NonceSet) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(s.now())
	return len(s.entries)
}

// Seen reports whether (user, nonce) is in the set and unexpired. It records
// nothing. It is the cheap early exit of live verification; [NonceSet.Record]
// is the authoritative check-and-set under concurrency.
func (s *NonceSet) Seen(user, nonce string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweepLocked(now)
	_, ok := s.entries[nonceKey{user, nonce}]
	return ok
}

// Record is an atomic check-and-set: it records (user, nonce) until ts plus
// the window, unless the pair is already present ([Replayed]), the set is full
// ([SetFull]), the sender is at its quota ([SenderQuotaExceeded]) or the pair's
// window has already passed ([Expired]). Nothing is recorded in those cases. The user is the sender's partition.
func (s *NonceSet) Record(user, nonce string, ts time.Time) RecordResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.sweepLocked(now)
	key := nonceKey{user, nonce}
	if _, ok := s.entries[key]; ok {
		return Replayed
	}
	expires := ts.Add(s.window)
	if expires.Before(now) {
		return Expired // outside the window already: nothing to remember
	}
	if len(s.entries) >= s.max {
		if !s.warned {
			s.warned = true
			if s.log != nil {
				s.log.Warn("sender nonce set is full; refusing signed requests as replays until entries expire",
					"max_entries", s.max)
			}
		}
		return SetFull
	}
	if s.counts[user] >= s.perMax {
		if !s.quotaWarned {
			s.quotaWarned = true
			if s.log != nil {
				s.log.Warn("a sender reached its nonce quota; refusing its signed requests until entries expire",
					"user", user, "max_per_sender", s.perMax)
			}
		}
		return SenderQuotaExceeded
	}
	s.entries[key] = expires
	s.counts[user]++
	heap.Push(&s.expiry, nonceExpiry{at: expires, key: key})
	return Recorded
}

// sweepLocked drops every entry whose window has passed, and re-arms the
// overflow warning once the set has fallen to half its maximum.
func (s *NonceSet) sweepLocked(now time.Time) {
	for len(s.expiry) > 0 && s.expiry[0].at.Before(now) {
		e := heap.Pop(&s.expiry).(nonceExpiry) // invariant: only nonceExpiry is ever pushed
		delete(s.entries, e.key)
		if s.counts[e.key.user]--; s.counts[e.key.user] <= 0 {
			delete(s.counts, e.key.user)
		}
	}
	if s.warned && len(s.entries) <= s.max/2 {
		s.warned = false
		s.quotaWarned = false
	}
}
