// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
)

// Bounds the tracker applies to what a peer can make it hold (SDK policy).
const (
	// DefaultTrackerMaxInstances is the most instances a tracker remembers.
	DefaultTrackerMaxInstances = 4096

	// maxHeartbeatBytes is the largest heartbeat payload the tracker reads; a
	// heartbeat is a small JSON object.
	maxHeartbeatBytes = 16 << 10

	// maxHeartbeatField is the longest instance_id, agent, owner or session the
	// tracker keeps.
	maxHeartbeatField = 256
)

// intervalOK reports whether a heartbeat's interval_s is within
// [agents.MaxHeartbeatInterval]. [agents.Heartbeat.Validate] has already ensured
// it is
// finite and positive.
func intervalOK(hb agents.Heartbeat) bool {
	return hb.IntervalS <= agents.MaxHeartbeatInterval.Seconds()
}

type trackerConfig struct {
	slack        float64
	logger       *slog.Logger
	maxInstances int
	verify       *identity.LiveOptions
	requireSign  bool
}

// TrackerOption configures a [HeartbeatTracker].
type TrackerOption func(*trackerConfig) error

// WithTrackerSlack sets the offline threshold as a multiple of an instance's
// own interval_s. The default is [agents.OfflineThresholdFactor], 3, the value
// SAP §8.2 recommends.
func WithTrackerSlack(n float64) TrackerOption {
	return func(c *trackerConfig) error {
		if !(n > 0) {
			return errors.New("caller: tracker slack must be positive")
		}
		c.slack = n
		return nil
	}
}

// WithTrackerLogger sets the tracker's logger; nil is silent.
func WithTrackerLogger(l *slog.Logger) TrackerOption {
	return func(c *trackerConfig) error {
		c.logger = l
		return nil
	}
}

// WithTrackerMaxInstances bounds how many instances the tracker remembers. When
// the table is full, an instance that is offline is evicted for a new one; if
// none is offline the new instance is ignored (SDK policy).
func WithTrackerMaxInstances(n int) TrackerOption {
	return func(c *trackerConfig) error {
		if n < 1 {
			return errors.New("caller: tracker needs room for at least one instance")
		}
		c.maxInstances = n
		return nil
	}
}

// WithHeartbeatVerification makes the tracker verify the Agent-Sender header of
// every heartbeat as a live message (SAP §13.7, §13.11) and discard one that
// fails; it never downgrades a failed heartbeat to a claim. With requireSigned
// a heartbeat that is not verified (absent or claimed) is discarded too; without
// it such a heartbeat is accepted, since SAP makes signing optional. opts must
// carry a replay set (see [identity.LiveOptions]).
func WithHeartbeatVerification(opts identity.LiveOptions, requireSigned bool) TrackerOption {
	return func(c *trackerConfig) error {
		if opts.Nonces == nil {
			return identity.ErrNoNonceSet
		}
		cp := opts
		c.verify = &cp
		c.requireSign = requireSigned
		return nil
	}
}

// Beat is one heartbeat as the tracker received it.
type Beat struct {
	// Heartbeat is the decoded payload (SAP §8.3).
	Heartbeat agents.Heartbeat
	// Subject is the heartbeat subject it arrived on.
	Subject string
	// Name is the instance name, read from the fifth token of Subject (SAP
	// §8.3); empty when the subject does not follow the default pattern.
	Name string
	// ReceivedAt is the local time of arrival.
	ReceivedAt time.Time
	// Sender is the verified sender of the heartbeat, set only when the tracker
	// verifies heartbeats and this one carried a verifying signature.
	Sender *identity.Sender
	// FromStatus is true when the beat was made from a status reply (SAP §8.7)
	// and not received on a heartbeat subject. Such a beat is never signed.
	FromStatus bool

	hdr nats.Header
}

// Header returns a copy of the headers the heartbeat arrived with, so a program
// can read what SAP carries there (§13.5). Changing it changes nothing in the
// SDK.
func (b Beat) Header() nats.Header { return cloneHeader(b.hdr) }

// Liveness is the tracker's view of one instance. See SAP §8.2.
type Liveness struct {
	InstanceID string
	Agent      string
	Owner      string
	Name       string
	Session    string
	// LastSeen is the local time of the last heartbeat.
	LastSeen time.Time
	// Interval is the instance's own declared cadence.
	Interval time.Duration
	// Online is true when the last heartbeat is at most slack times Interval old.
	Online bool
}

type trackerEntry struct {
	agent, owner, name, session string
	lastSeen                    time.Time
	interval                    time.Duration
}

type listener struct {
	id       uint64
	instance string // empty: every instance
	fn       func(Beat)
}

// HeartbeatTracker subscribes to the heartbeat wildcard and keeps each
// instance's last heartbeat, keyed on the instance_id in the payload and not on
// the subject, so several instances of one logical agent stay distinct (SAP
// §3.4, §8.1). It is safe for concurrent use.
//
// Listeners run synchronously on the subscription's goroutine, each under
// recover, in registration order, and must not block. A listener may remove
// itself from inside its own call.
type HeartbeatTracker struct {
	nc  *nats.Conn
	cfg trackerConfig
	log *slog.Logger

	startMu sync.Mutex
	sub     *nats.Subscription

	mu        sync.RWMutex
	entries   map[string]*trackerEntry
	listeners []listener
	nextID    uint64

	dropped atomic.Uint64
	started atomic.Bool
}

// NewHeartbeatTracker returns a tracker on nc. It subscribes nothing until
// [HeartbeatTracker.Start].
func NewHeartbeatTracker(nc *nats.Conn, opts ...TrackerOption) (*HeartbeatTracker, error) {
	if nc == nil {
		return nil, errors.New("caller: nil connection")
	}
	cfg := trackerConfig{slack: agents.OfflineThresholdFactor, maxInstances: DefaultTrackerMaxInstances}
	for _, o := range opts {
		if o == nil {
			continue
		}
		if err := o(&cfg); err != nil {
			return nil, err
		}
	}
	lg := cfg.logger
	if lg == nil {
		lg = slog.New(slog.DiscardHandler)
	}
	return &HeartbeatTracker{nc: nc, cfg: cfg, log: lg, entries: map[string]*trackerEntry{}}, nil
}

// Subject returns the wildcard the tracker subscribes to, agents.hb.*.*.*. See
// SAP §8.1, §8.5.
func (t *HeartbeatTracker) Subject() string { return agents.SubjectHeartbeatAll }

// Started reports whether the tracker is subscribed.
func (t *HeartbeatTracker) Started() bool { return t.started.Load() }

// Dropped counts heartbeats the tracker discarded: malformed, too large,
// failing verification, or refused for want of room.
func (t *HeartbeatTracker) Dropped() uint64 { return t.dropped.Load() }

// Start subscribes to the heartbeat wildcard and flushes, so the server has the
// interest when Start returns: callers SHOULD subscribe before their first
// ping (SAP §8.5). It is idempotent.
func (t *HeartbeatTracker) Start(ctx context.Context) error {
	t.startMu.Lock()
	defer t.startMu.Unlock()
	if t.sub != nil {
		return nil
	}
	sub, err := t.nc.Subscribe(t.Subject(), t.handle)
	if err != nil {
		return errf("heartbeat subscribe", err)
	}
	if err := flush(ctx, t.nc); err != nil {
		_ = sub.Unsubscribe()
		return errf("heartbeat subscribe", err)
	}
	t.sub = sub
	t.started.Store(true)
	return nil
}

// Stop drains the subscription, so pending heartbeats are delivered, and waits
// for it to close or for ctx. It is idempotent. The remembered instances are
// kept.
func (t *HeartbeatTracker) Stop(ctx context.Context) error {
	t.startMu.Lock()
	defer t.startMu.Unlock()
	sub := t.sub
	if sub == nil {
		return nil
	}
	t.sub = nil
	t.started.Store(false)
	if !waitClosed(ctx, sub, DefaultCloseTimeout) && ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// handle processes one heartbeat message on the subscription's goroutine.
func (t *HeartbeatTracker) handle(m *nats.Msg) {
	if len(m.Data) > maxHeartbeatBytes {
		t.drop("heartbeat too large")
		return
	}
	hb, err := agents.DecodeHeartbeat(m.Data)
	if err != nil {
		t.drop("malformed heartbeat")
		return
	}
	if len(hb.InstanceID) > maxHeartbeatField || len(hb.Agent) > maxHeartbeatField ||
		len(hb.Owner) > maxHeartbeatField || len(hb.Session) > maxHeartbeatField {
		t.drop("heartbeat field too long")
		return
	}
	if !intervalOK(hb) {
		t.drop("heartbeat interval_s above the ceiling")
		return
	}
	var sender *identity.Sender
	if t.cfg.verify != nil {
		opts := *t.cfg.verify
		s, err := identity.VerifyLive(identity.MessageFromNATS(m), opts)
		if err != nil {
			t.drop("heartbeat failed sender verification")
			return
		}
		if t.cfg.requireSign && identity.TrustOf(s) != identity.TrustVerified {
			t.drop("heartbeat is not signed")
			return
		}
		if identity.TrustOf(s) == identity.TrustVerified {
			sender = s
		}
	}
	now := time.Now()
	beat := Beat{
		Heartbeat:  hb,
		Subject:    m.Subject,
		Name:       agents.InstanceNameFromSubject(m.Subject),
		ReceivedAt: now,
		Sender:     sender,
		hdr:        m.Header,
	}
	if !t.record(beat) {
		return
	}
	t.dispatch(beat)
}

func (t *HeartbeatTracker) drop(why string) {
	t.dropped.Add(1)
	t.log.Debug("dropped a heartbeat", slog.String("reason", why))
}

// record stores the beat, evicting an offline instance when the table is full.
func (t *HeartbeatTracker) record(b Beat) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := b.Heartbeat.InstanceID
	e, ok := t.entries[id]
	if !ok {
		if len(t.entries) >= t.cfg.maxInstances && !t.evictOffline(b.ReceivedAt) {
			t.dropped.Add(1)
			t.log.Debug("heartbeat table full; ignoring a new instance")
			return false
		}
		e = &trackerEntry{}
		t.entries[id] = e
	}
	e.agent, e.owner, e.name, e.session = b.Heartbeat.Agent, b.Heartbeat.Owner, b.Name, b.Heartbeat.Session
	e.lastSeen = b.ReceivedAt
	e.interval = b.Heartbeat.Interval()
	return true
}

// evictOffline removes one offline instance; the caller holds t.mu.
func (t *HeartbeatTracker) evictOffline(now time.Time) bool {
	for id, e := range t.entries {
		if !t.online(e, now) {
			delete(t.entries, id)
			return true
		}
	}
	return false
}

func (t *HeartbeatTracker) online(e *trackerEntry, now time.Time) bool {
	return now.Sub(e.lastSeen) <= time.Duration(t.cfg.slack*float64(e.interval))
}

// dispatch calls the listeners on a copy of the list, so one may unsubscribe
// from inside its own call.
func (t *HeartbeatTracker) dispatch(b Beat) {
	t.mu.RLock()
	ls := make([]listener, 0, len(t.listeners))
	for _, l := range t.listeners {
		if l.instance == "" || l.instance == b.Heartbeat.InstanceID {
			ls = append(ls, l)
		}
	}
	t.mu.RUnlock()
	for _, l := range ls {
		t.call(l.fn, b)
	}
}

func (t *HeartbeatTracker) call(fn func(Beat), b Beat) {
	defer func() {
		if r := recover(); r != nil {
			t.log.Warn("heartbeat listener panicked")
		}
	}()
	fn(b)
}

// Liveness returns the tracker's view of one instance. now defaults to the
// current time. An instance is online when its last heartbeat is at most
// slack times its own interval_s old, the boundary included (SAP §8.2). The
// second result is false for an instance never heard.
func (t *HeartbeatTracker) Liveness(instanceID string, now ...time.Time) (Liveness, bool) {
	at := time.Now()
	if len(now) > 0 {
		at = now[0]
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.entries[instanceID]
	if !ok {
		return Liveness{}, false
	}
	return t.view(instanceID, e, at), true
}

func (t *HeartbeatTracker) view(id string, e *trackerEntry, at time.Time) Liveness {
	return Liveness{
		InstanceID: id, Agent: e.agent, Owner: e.owner, Name: e.name, Session: e.session,
		LastSeen: e.lastSeen, Interval: e.interval, Online: t.online(e, at),
	}
}

// Snapshot returns the view of every remembered instance, in no order.
func (t *HeartbeatTracker) Snapshot(now ...time.Time) []Liveness {
	at := time.Now()
	if len(now) > 0 {
		at = now[0]
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Liveness, 0, len(t.entries))
	for id, e := range t.entries {
		out = append(out, t.view(id, e, at))
	}
	return out
}

// OnHeartbeat registers fn for the heartbeats of one instance and returns the
// function that removes it.
func (t *HeartbeatTracker) OnHeartbeat(instanceID string, fn func(Beat)) (cancel func()) {
	return t.addListener(instanceID, fn)
}

// OnAnyHeartbeat registers fn for every heartbeat and returns the function that
// removes it.
func (t *HeartbeatTracker) OnAnyHeartbeat(fn func(Beat)) (cancel func()) {
	return t.addListener("", fn)
}

func (t *HeartbeatTracker) addListener(instance string, fn func(Beat)) func() {
	t.mu.Lock()
	t.nextID++
	id := t.nextID
	t.listeners = append(t.listeners, listener{id: id, instance: instance, fn: fn})
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		for i, l := range t.listeners {
			if l.id == id {
				t.listeners = append(t.listeners[:i:i], t.listeners[i+1:]...)
				return
			}
		}
	}
}

// requiresSigned reports whether the tracker drops unsigned heartbeats.
func (t *HeartbeatTracker) requiresSigned() bool { return t.cfg.verify != nil && t.cfg.requireSign }

// observe feeds a status reply into the tracker as a just-arrived heartbeat
// (SAP §8.7, MAY).
func (t *HeartbeatTracker) observe(hb agents.Heartbeat, subject string) {
	if len(hb.InstanceID) > maxHeartbeatField || len(hb.Agent) > maxHeartbeatField ||
		len(hb.Owner) > maxHeartbeatField || len(hb.Session) > maxHeartbeatField {
		return
	}
	if !intervalOK(hb) {
		t.drop("status interval_s above the ceiling")
		return
	}
	b := Beat{Heartbeat: hb, Subject: subject, Name: agents.InstanceNameFromSubject(subject), ReceivedAt: time.Now(), FromStatus: true}
	if t.record(b) {
		t.dispatch(b)
	}
}

func (b Beat) String() string {
	return fmt.Sprintf("beat(%s %s.%s.%s)", b.Heartbeat.InstanceID, b.Heartbeat.Agent, b.Heartbeat.Owner, b.Name)
}

func cloneHeader(h nats.Header) nats.Header {
	if h == nil {
		return nil
	}
	out := make(nats.Header, len(h))
	for k, v := range h {
		out[k] = append([]string(nil), v...)
	}
	return out
}
