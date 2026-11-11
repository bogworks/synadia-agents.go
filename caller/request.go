// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller

import (
	"context"
	"errors"
	"fmt"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/nats-io/nats.go"
)

// headerBlockBytes is the framing of any header block: "NATS/1.0\r\n" and the
// closing blank line.
const headerBlockBytes = len("NATS/1.0\r\n") + len("\r\n")

// framedHeaderBytes is what the header block of a message adds on the wire:
// the framing plus every "Key: Value\r\n" line. A message without headers adds
// nothing.
func framedHeaderBytes(h nats.Header) int {
	if len(h) == 0 {
		return 0
	}
	n := headerBlockBytes
	for k, vs := range h {
		for _, v := range vs {
			n += len(k) + len(": ") + len(v) + len("\r\n")
		}
	}
	return n
}

// publish sends m, mapping the connection's size refusal to the typed error.
func (c *Client) publish(m *nats.Msg) error {
	if err := c.nc.PublishMsg(m); err != nil {
		if errors.Is(err, nats.ErrMaxPayload) {
			return &agents.PayloadTooLargeError{Limit: c.nc.MaxPayload(), Actual: int64(len(m.Data)), HeaderBytes: int64(framedHeaderBytes(m.Header)), Connection: true}
		}
		return errf("publish", err)
	}
	return nil
}

// request publishes m with a reply token of the client's mux and waits for one
// reply. The wait ends with ctx, with timeout (nats.ErrTimeout), or when the
// client closes. A server "no responders" status is nats.ErrNoResponders.
func (c *Client) request(ctx context.Context, m *nats.Msg, timeout time.Duration) (*nats.Msg, error) {
	if err := c.checkOpen(); err != nil {
		return nil, err
	}
	if err := c.mux.ensure(ctx); err != nil {
		return nil, errf("reply subscription", err)
	}
	r, err := c.mux.register(1, c.cfg.bufferBytes, nil, nil)
	if err != nil {
		return nil, err
	}
	defer r.close()
	m.Reply = r.subject
	if err := c.publish(m); err != nil {
		return nil, err
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case reply := <-r.ch:
		r.take(reply)
		if isNoResponders(reply) {
			return nil, fmt.Errorf("caller: request %s: %w", m.Subject, nats.ErrNoResponders)
		}
		return reply, nil
	case <-r.over:
		return nil, &SlowConsumerError{Buffered: 1, Bytes: r.maxBytes}
	case <-t.C:
		return nil, fmt.Errorf("caller: request %s: %w", m.Subject, nats.ErrTimeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, ErrClosed
	}
}
