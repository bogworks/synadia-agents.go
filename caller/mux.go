// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
)

var tokenEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

// randToken returns n lowercase base32 characters from crypto/rand.
func randToken(n int) string {
	b := make([]byte, (n*5+7)/8)
	_, _ = rand.Read(b) // never fails since Go 1.24
	return strings.ToLower(tokenEnc.EncodeToString(b))[:n]
}

// route is one registered reply token: the bounded queue of one prompt, one
// discovery or one request. The mux fills it; its owner drains it.
type route struct {
	m        *mux
	token    string
	subject  string
	ch       chan *nats.Msg
	bytes    atomic.Int64
	maxBytes int64
	maxMsgs  int

	arrive func() // called after a message was queued; may be nil
	stop   func() // called when the client closes; may be nil

	over     chan struct{} // closed when the route overflowed
	overOnce sync.Once
	gone     sync.Once
}

// mux is the client's one reply subscription. Every prompt, discovery and
// request has its own reply token below the client's prefix; the callback
// routes a message to its route by that token. A reply with no route is
// dropped and counted (REQ-019).
type mux struct {
	c      *Client
	prefix string // {inbox prefix}.{client id}

	initMu sync.Mutex
	sub    *nats.Subscription

	mu       sync.Mutex
	routes   map[string]*route
	closed   bool
	draining bool
	drained  chan struct{}

	dropped  atomic.Uint64
	overflow atomic.Uint64
}

func newMux(c *Client, inboxPrefix string) *mux {
	return &mux{
		c:      c,
		prefix: inboxPrefix + "." + randToken(16),
		routes: map[string]*route{},
	}
}

// ensure creates the subscription once and flushes it, so the server has the
// interest before the first publish names a reply subject below it.
func (m *mux) ensure(ctx context.Context) error {
	m.initMu.Lock()
	defer m.initMu.Unlock()
	if m.sub != nil {
		return nil
	}
	if m.c.closed.Load() {
		return ErrClosed
	}
	sub, err := m.c.nc.Subscribe(m.prefix+".>", m.deliver)
	if err != nil {
		return err
	}
	if err := flush(ctx, m.c.nc); err != nil {
		_ = sub.Unsubscribe()
		return err
	}
	m.sub = sub
	return nil
}

func flush(ctx context.Context, nc *nats.Conn) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultFlushTimeout)
		defer cancel()
	}
	return nc.FlushWithContext(ctx)
}

// register adds a route with the given bounds. arrive, when not nil, runs after
// each message is queued; stop, when not nil, runs when the client closes.
func (m *mux) register(msgs int, maxBytes int64, arrive, stop func()) (*route, error) {
	token := randToken(16)
	r := &route{
		m:        m,
		token:    token,
		subject:  m.prefix + "." + token,
		ch:       make(chan *nats.Msg, msgs),
		maxBytes: maxBytes,
		maxMsgs:  msgs,
		over:     make(chan struct{}),
		arrive:   arrive,
		stop:     stop,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	m.routes[token] = r
	return r, nil
}

func (m *mux) active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.routes)
}

// deliver is the subscription callback.
func (m *mux) deliver(msg *nats.Msg) {
	token, ok := strings.CutPrefix(msg.Subject, m.prefix+".")
	if !ok || strings.Contains(token, ".") {
		m.countDropped(msg)
		return
	}
	m.mu.Lock()
	r := m.routes[token]
	m.mu.Unlock()
	if r == nil {
		m.countDropped(msg)
		return
	}
	size := msgSize(msg)
	if r.bytes.Add(size) > r.maxBytes {
		r.bytes.Add(-size)
		r.overflowed()
		return
	}
	select {
	case r.ch <- msg:
		if r.arrive != nil {
			r.arrive()
		}
	default:
		r.bytes.Add(-size)
		r.overflowed()
	}
}

// msgSize is what a queued message holds against its route's byte budget: its
// body and its headers (every key and value), since a peer controls both.
func msgSize(msg *nats.Msg) int64 {
	n := int64(len(msg.Data))
	for k, vs := range msg.Header {
		n += int64(len(k))
		for _, v := range vs {
			n += int64(len(v))
		}
	}
	return n
}

func (m *mux) countDropped(*nats.Msg) {
	m.dropped.Add(1)
	m.c.log.Debug("dropped a reply with no matching request or stream")
}

// overflowed ends delivery to the route: no later message may follow the
// ones that were lost.
func (r *route) overflowed() {
	r.overOnce.Do(func() {
		r.m.overflow.Add(1)
		close(r.over)
		r.m.remove(r.token)
	})
}

func (m *mux) remove(token string) {
	m.mu.Lock()
	delete(m.routes, token)
	if m.draining && len(m.routes) == 0 && m.drained != nil {
		close(m.drained)
		m.drained = nil
	}
	m.mu.Unlock()
}

// take removes the size of a dequeued message from the byte budget.
func (r *route) take(msg *nats.Msg) { r.bytes.Add(-msgSize(msg)) }

// close unregisters the route; later replies are dropped and counted.
func (r *route) close() {
	r.gone.Do(func() { r.m.remove(r.token) })
}

// closeAll marks the mux closed and forgets every route.
func (m *mux) closeAll() {
	m.mu.Lock()
	m.closed = true
	old := m.routes
	m.routes = map[string]*route{}
	if m.drained != nil {
		close(m.drained)
		m.drained = nil
	}
	m.mu.Unlock()
	for _, r := range old {
		if r.stop != nil {
			r.stop()
		}
	}
}

// beginDrain returns a channel closed when no route is left.
func (m *mux) beginDrain() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.draining = true
	ch := make(chan struct{})
	if len(m.routes) == 0 {
		close(ch)
		return ch
	}
	m.drained = ch
	return ch
}

// shutdown drains the subscription and waits for it to close, bounded by
// [DefaultCloseTimeout].
func (m *mux) shutdown() {
	m.initMu.Lock()
	sub := m.sub
	m.sub = nil
	m.initMu.Unlock()
	if sub == nil {
		return
	}
	waitClosed(context.Background(), sub, DefaultCloseTimeout)
}

// waitClosed drains sub and waits until it is no longer valid, until timeout,
// or until ctx is done. It returns false when it gave up.
func waitClosed(ctx context.Context, sub *nats.Subscription, timeout time.Duration) bool {
	if err := sub.Drain(); err != nil {
		_ = sub.Unsubscribe()
		return true
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for sub.IsValid() {
		select {
		case <-tick.C:
		case <-deadline.C:
			_ = sub.Unsubscribe()
			return false
		case <-ctx.Done():
			_ = sub.Unsubscribe()
			return false
		}
	}
	return true
}
