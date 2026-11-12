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

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
)

// Client is the caller side of SAP over one NATS connection: it discovers
// agents, prompts them and tracks their heartbeats. A Client is safe for
// concurrent use, and many prompts may run at once on it (REQ-019).
//
// A Client owns one reply subscription for every prompt, discovery and request
// it makes, routing each reply by its own reply token. It never closes the
// connection it was given. No goroutine of the SDK outlives [Client.Close] or
// the context of the call that started it.
type Client struct {
	nc  *nats.Conn
	cfg config
	log *slog.Logger
	mux *mux

	tracker *HeartbeatTracker

	self *identity.SelfIDResolver // looks up only when asked or when identity is configured

	resolverMu sync.Mutex
	resolver   *identity.SenderResolver

	done      chan struct{}
	closeOnce sync.Once
	closed    atomic.Bool
	draining  atomic.Bool
	wg        sync.WaitGroup
	waitOnce  sync.Once     // starts the goroutine that closes waitDone
	waitDone  chan struct{} // closed when wg is zero after Close

	malformed atomic.Uint64
	rejected  atomic.Uint64
}

// New returns a Client on nc. It makes no network call: the reply subscription
// is created by the first prompt, discovery or request, and flushed before the
// first publish that names it. The caller keeps ownership of nc. See SAP §10.1.
func New(nc *nats.Conn, opts ...Option) (*Client, error) {
	if nc == nil {
		return nil, errors.New("caller: nil connection")
	}
	if nc.IsClosed() {
		return nil, nats.ErrConnectionClosed
	}
	cfg := defaultConfig()
	for _, o := range opts {
		if o == nil {
			continue
		}
		if err := o(&cfg); err != nil {
			return nil, err
		}
	}
	c := &Client{nc: nc, cfg: cfg, done: make(chan struct{})}
	c.log = cfg.logger
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	c.mux = newMux(c, cfg.inboxPrefix)
	topts := append([]TrackerOption{WithTrackerLogger(c.log)}, cfg.trackerOpts...)
	tr, err := NewHeartbeatTracker(nc, topts...)
	if err != nil {
		return nil, err
	}
	c.tracker = tr
	so := identity.SelfIDOptions{Logger: c.log}
	if cfg.ident != nil {
		so.Signer = cfg.ident.Signer
		so.ConsultServer = cfg.ident.ConsultServer
		so.Timeout = cfg.ident.LookupTimeout
		so.RetryInterval = cfg.ident.RetryInterval
	}
	c.self = identity.NewSelfIDResolver(nc, so)
	c.watchReconnects()
	return c, nil
}

// watchReconnects forgets the remembered identity after a reconnect, which may
// land on a server that answers differently (SAP §13.4). Its goroutine ends
// with the client.
func (c *Client) watchReconnects() {
	ch := c.nc.StatusChanged(nats.CONNECTED)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer c.nc.RemoveStatusListener(ch)
		for {
			select {
			case <-c.done:
				return
			case _, ok := <-ch:
				if !ok {
					return
				}
				c.self.Reset()
			}
		}
	}()
}

// Connection returns the NATS connection the client uses.
func (c *Client) Connection() *nats.Conn { return c.nc }

// Tracker returns the heartbeat tracker the client owns. [Client.Discover]
// starts it before its first discovery request (SAP §8.5).
func (c *Client) Tracker() *HeartbeatTracker { return c.tracker }

// IsClosed reports whether Close has been called.
func (c *Client) IsClosed() bool { return c.closed.Load() }

// Done returns a channel closed when the client is closed.
func (c *Client) Done() <-chan struct{} { return c.done }

// Stats is a snapshot of the client's counters.
type Stats struct {
	// DroppedReplies counts messages that arrived on the client's reply
	// subscription with no stream or request waiting for them: a reply that came
	// after its prompt ended, timed out or was canceled, or one for a token the
	// client never issued. They are never delivered anywhere else (REQ-019).
	DroppedReplies uint64
	// MalformedChunks counts chunks of a known type whose data was malformed.
	// They are dropped and the stream continues (SAP §6.6; SDK policy).
	MalformedChunks uint64
	// RejectedRecords counts discovery replies dropped as untrustworthy: a
	// record whose default-form subject disagrees with its metadata, a lookup
	// reply for an instance other than the one asked for, and a record that
	// failed a required id_sig verification. See [WithRequireVerifiedIdentity].
	RejectedRecords uint64
	// SlowConsumers counts streams and discoveries ended by an overflowed buffer.
	SlowConsumers uint64
	// ActiveRoutes is the number of streams and requests in flight.
	ActiveRoutes int
}

// Stats returns the client's counters.
func (c *Client) Stats() Stats {
	return Stats{
		DroppedReplies:  c.mux.dropped.Load(),
		MalformedChunks: c.malformed.Load(),
		RejectedRecords: c.rejected.Load(),
		SlowConsumers:   c.mux.overflow.Load(),
		ActiveRoutes:    c.mux.active(),
	}
}

func (c *Client) checkOpen() error {
	if c.closed.Load() {
		return ErrClosed
	}
	return nil
}

// Close ends every stream still running with [ErrClosed], stops the heartbeat
// tracker, drains the reply subscription and waits for the SDK's goroutines to
// exit. It does not close the connection.
//
// Close waits until ctx is done. When ctx has no deadline the drain is bounded
// by [DefaultCloseTimeout]; when it has one, the deadline alone bounds it. If
// ctx ends first, Close returns ctx.Err() wrapped (test it with [errors.Is]);
// the client is closed and the subscriptions are unsubscribed all the same, and
// a later Close waits again for the goroutines still running. Close is
// idempotent.
func (c *Client) Close(ctx context.Context) error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.done)
		c.mux.closeAll()
	})
	var first error
	note := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	c.waitOnce.Do(func() {
		c.waitDone = make(chan struct{})
		go func() {
			c.wg.Wait()
			close(c.waitDone)
		}()
	})
	select {
	case <-c.waitDone:
	case <-ctx.Done():
		note(ctx.Err())
	}
	note(c.tracker.Stop(ctx))
	note(c.mux.shutdown(ctx))
	if first != nil {
		return fmt.Errorf("caller: close: %w", first)
	}
	return nil
}

// Drain refuses new prompts with [ErrClosed], waits for the streams in flight
// to end on their own, and then closes the client. When ctx is done first it
// closes the client at once and returns ctx.Err().
func (c *Client) Drain(ctx context.Context) error {
	c.draining.Store(true)
	idle := c.mux.beginDrain()
	select {
	case <-idle:
		return c.Close(ctx)
	case <-ctx.Done():
		_ = c.Close(ctx)
		return ctx.Err()
	case <-c.done:
		return nil
	}
}

// ObserveError feeds an asynchronous NATS error to the identity lookup, so a
// permission that blocks $SYS.REQ.USER.INFO fails the lookup at once instead
// of after its timeout (SAP §13.4). Call it from the connection's error
// handler. It reports whether the error concerned the lookup.
func (c *Client) ObserveError(err error) bool {
	return c.self.ObserveError(err)
}

// errf wraps err with the operation for the caller.
func errf(op string, err error) error { return fmt.Errorf("caller: %s: %w", op, err) }

// versionPolicy is the policy in force.
func (c *Client) versionPolicy() agents.VersionPolicy { return c.cfg.policy }
