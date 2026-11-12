// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
)

// HeartbeatSigner is the identity a host signs its heartbeats with. See SAP
// §13.11.
type HeartbeatSigner struct {
	ID     identity.AgentID
	Signer identity.Signer
}

// SignHeartbeatOptions are the inputs of [SignHeartbeatHeader].
type SignHeartbeatOptions struct {
	Sender HeartbeatSigner
	// Subject is the heartbeat subject as published.
	Subject string
	// TS is the heartbeat's own ts field.
	TS string
	// Data is the exact payload bytes published.
	Data []byte
	// Nonce is for tests and vectors; empty means a fresh nonce.
	Nonce string
}

// SignHeartbeatHeader builds the Agent-Sender header of a heartbeat: sub is the
// heartbeat subject as published, ts the heartbeat's own ts, the nonce fresh,
// the signature over the exact payload bytes (SAP §13.11). For hand-rolled
// publishers; [Host] uses it for its own beats.
//
// [identity.Signer] takes no context, so the signer runs in its own goroutine:
// when ctx ends first the call returns ctx.Err() at once and the goroutine
// lives until the signer returns.
func SignHeartbeatHeader(ctx context.Context, o SignHeartbeatOptions) (identity.SenderHeader, error) {
	if o.Sender.Signer == nil {
		return identity.SenderHeader{}, errors.New("host: heartbeat signer is nil")
	}
	if err := ctx.Err(); err != nil {
		return identity.SenderHeader{}, err
	}
	type result struct {
		h   identity.SenderHeader
		err error
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if recover() != nil {
				ch <- result{err: errors.New("host: heartbeat signer panicked")}
			}
		}()
		hdr, err := identity.SignHeader(identity.SignOptions{
			Signer:  o.Sender.Signer,
			ID:      o.Sender.ID,
			Subject: o.Subject,
			Payload: o.Data,
			TS:      o.TS,
			Nonce:   o.Nonce,
		})
		ch <- result{hdr, err}
	}()
	select {
	case r := <-ch:
		return r.h, r.err
	case <-ctx.Done():
		return identity.SenderHeader{}, ctx.Err()
	}
}

// SignHeartbeat is [SignHeartbeatHeader] returning NATS headers ready to
// publish with. A reply, a status reply included, is never signed (SAP §13.11).
func SignHeartbeat(ctx context.Context, o SignHeartbeatOptions) (nats.Header, error) {
	h, err := SignHeartbeatHeader(ctx, o)
	if err != nil {
		return nil, err
	}
	hdr := nats.Header{}
	h.Set(hdr)
	return hdr, nil
}

// heartbeatFields are the field names of SAP §8.3 that an extra must not use.
var heartbeatFields = []string{"agent", "owner", "session", "instance_id", "ts", "interval_s", "protocol_version", "endpoints"}

// extras reads the provider once and returns its fields as raw JSON, or nil
// when there is no provider or its answer cannot go on the wire.
func (h *Host) extras() map[string]json.RawMessage {
	if h.cfg.hbExtras == nil {
		return nil
	}
	var m map[string]any
	ok := func() (ok bool) {
		defer func() {
			if recover() != nil {
				h.log.Error("heartbeat extras provider panicked; publishing without extras")
				ok = false
			}
		}()
		m = h.cfg.hbExtras()
		return true
	}()
	if !ok || len(m) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		if slices.Contains(heartbeatFields, k) {
			h.log.Error("heartbeat extras reuse a SAP §8.3 field name; publishing without extras", "field", k)
			return nil
		}
		b, err := json.Marshal(v)
		if err != nil {
			h.log.Error("heartbeat extras do not serialize; publishing without extras", "field", k)
			return nil
		}
		out[k] = b
	}
	return out
}

// buildHeartbeat builds the heartbeat-shaped payload shared by the periodic
// beat and the status reply (SAP §8.3, §8.7.1).
func (h *Host) buildHeartbeat() (agents.Heartbeat, []byte, error) {
	hb := agents.Heartbeat{
		Agent:           h.subject.Agent(),
		Owner:           h.subject.Owner(),
		Session:         h.cfg.session,
		InstanceID:      h.id,
		ProtocolVersion: agents.ProtocolVersion,
		Endpoints: map[string]agents.HeartbeatEndpoint{
			agents.EndpointPrompt: {Subject: h.subject.Prompt(), Metadata: cloneMap(h.promptMeta)},
		},
		Extra: h.extras(),
	}
	hb.SetInterval(h.cfg.hbInterval)
	hb.Stamp(time.Now())
	data, err := agents.EncodeHeartbeat(hb)
	if err != nil {
		return hb, nil, err
	}
	return hb, data, nil
}

// errHeartbeatSign marks a beat skipped because the signer failed.
var errHeartbeatSign = errors.New("host: heartbeat signing failed")

// errSignTimeout is the cause of a beat skipped because the signer was too slow.
var errSignTimeout = errors.New("host: heartbeat signer timed out")

// signBeat signs one beat within the signing timeout and ctx. At most one
// signer call is outstanding: while an abandoned one has not returned, a new
// beat is skipped instead of starting a second goroutine on a stuck signer.
func (h *Host) signBeat(ctx context.Context, o SignHeartbeatOptions) (nats.Header, error) {
	if !h.signing.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("%w: the previous signing has not returned", errSignTimeout)
	}
	sctx, cancel := context.WithTimeout(ctx, h.cfg.signTimeout)
	defer cancel()
	type result struct {
		hdr nats.Header
		err error
	}
	ch := make(chan result, 1)
	//nolint:gosec // identity.Signer takes no context; the call must outlive this one and is bounded by signing
	go func() {
		defer h.signing.Store(false)
		hdr, err := SignHeartbeat(context.Background(), o)
		ch <- result{hdr, err}
	}()
	select {
	case r := <-ch:
		return r.hdr, r.err
	case <-sctx.Done():
		if ctx.Err() == nil {
			return nil, errSignTimeout
		}
		return nil, ctx.Err()
	}
}

// publishHeartbeat publishes one beat, signed when the host holds a seed.
func (h *Host) publishHeartbeat(ctx context.Context) error {
	hb, data, err := h.buildHeartbeat()
	if err != nil {
		return fmt.Errorf("host: build heartbeat: %w", err)
	}
	subj := h.subject.Heartbeat()
	msg := &nats.Msg{Subject: subj, Data: data}
	if h.signer != nil && h.hasID {
		hdr, err := h.signBeat(ctx, SignHeartbeatOptions{
			Sender:  HeartbeatSigner{ID: h.agentID, Signer: h.signer},
			Subject: subj, TS: hb.TS, Data: data,
		})
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			// An identity-enabled host never publishes an unsigned beat: the
			// beat is skipped, counted and logged (SAP §13.11).
			h.hbSignFail.Add(1)
			if errors.Is(err, errSignTimeout) {
				h.hbSignTO.Add(1)
			}
			h.log.Error("heartbeat signing failed; beat skipped", "subject", subj, "timeout", errors.Is(err, errSignTimeout))
			return errHeartbeatSign
		}
		msg.Header = hdr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := h.checkStop(); err != nil {
		return err
	}
	return h.nc.PublishMsg(msg)
}

// heartbeatLoop publishes a beat every interval until ctx ends. One beat is in
// flight at a time: a tick that finds the previous beat still being signed is
// skipped, so a slow signer never piles beats up or reorders them, and never
// holds teardown (the loop returns at once; see [Host.Shutdown]).
func (h *Host) heartbeatLoop(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	t := time.NewTicker(h.cfg.hbInterval)
	defer t.Stop()
	var mu sync.Mutex
	beating := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		mu.Lock()
		if beating {
			mu.Unlock()
			h.log.Warn("heartbeat skipped: the previous beat is still being signed")
			continue
		}
		beating = true
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { mu.Lock(); beating = false; mu.Unlock() }()
			if err := h.publishHeartbeat(ctx); err != nil && ctx.Err() == nil && !errors.Is(err, errHeartbeatSign) && !errors.Is(err, ErrStopped) {
				h.log.Error("heartbeat publish failed")
			}
		}()
	}
}
