// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
)

// senderPlan is the Agent-Sender header one request will carry, known before
// signing so its size can be counted against max_payload.
type senderPlan struct {
	id     identity.AgentID
	signed bool
	sub    string
	wire   int
}

func (c *Client) signer() identity.Signer {
	if c.cfg.ident == nil {
		return nil
	}
	return c.cfg.ident.Signer
}

func (c *Client) senderName() string {
	if c.cfg.ident == nil {
		return ""
	}
	return c.cfg.ident.Name
}

// planSender decides whether a request to an endpoint carries an Agent-Sender
// header and which. SAP §13.4: a connection with no or unknown identity sends no
// header and fails locally against an endpoint that requires a signed sender;
// it never guesses an identity. An identity that disagrees with the signer is an
// error whatever the endpoint requires.
func (c *Client) planSender(ctx context.Context, sub string, required bool) (*senderPlan, error) {
	signer := c.signer()
	if required {
		if err := identity.CheckCanSend(identity.TrustSigned, signer != nil); err != nil {
			return nil, local(err)
		}
	}
	if c.cfg.ident == nil || (signer == nil && !c.cfg.ident.SendClaim) {
		return nil, nil
	}
	id, err := c.self.ID(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, identity.ErrIdentityMismatch) {
			return nil, err
		}
		if required {
			return nil, local(fmt.Errorf("%w: %w", identity.ErrCannotSign, err))
		}
		c.log.Debug("sender identity unknown; sending no Agent-Sender header")
		return nil, nil
	}
	signed := signer != nil
	return &senderPlan{
		id: id, signed: signed, sub: sub,
		wire: identity.ExpectedHeaderWireBytes(id, c.senderName(), sub, signed),
	}, nil
}

// header builds and signs the header over the exact payload bytes, with a fresh
// ts and nonce (SAP §13.5, §13.6).
func (c *Client) senderHeader(p *senderPlan, payload []byte) (identity.SenderHeader, error) {
	if !p.signed {
		return identity.ClaimHeader(p.id, c.senderName())
	}
	return identity.SignHeader(identity.SignOptions{
		Signer: c.signer(), ID: p.id, Name: c.senderName(), Subject: p.sub, Payload: payload,
	})
}

// SelfID returns the connection's own agent ID (SAP §13.4), looking it up once
// and remembering it. It fails with [identity.ErrNoIdentity] or
// [identity.ErrIdentityUnknown] when there is none, and a failure is retried
// after the retry interval, not kept for the connection's lifetime. A reconnect
// forgets the remembered ID.
func (c *Client) SelfID(ctx context.Context) (identity.AgentID, error) {
	if err := c.checkOpen(); err != nil {
		return "", err
	}
	return c.self.ID(ctx)
}

// RefreshSelfID forgets the remembered identity and looks it up again.
func (c *Client) RefreshSelfID(ctx context.Context) (identity.AgentID, error) {
	if err := c.checkOpen(); err != nil {
		return "", err
	}
	c.self.Reset()
	return c.self.ID(ctx)
}

type signConfig struct {
	sub     string
	nonce   string
	headers nats.Header
}

// SignOption configures [Client.SignSender], [Client.PublishSigned] and
// [Client.RequestSigned].
type SignOption func(*signConfig)

// WithSub sets the subject to sign when it differs from the one published to,
// which is only right behind a service import the caller's own account renamed
// (SAP §13.6.2).
func WithSub(sub string) SignOption { return func(c *signConfig) { c.sub = sub } }

// WithNonce sets the nonce instead of a fresh one, for a message whose body
// carries its own id. It must fit the header grammar (SAP §13.5).
func WithNonce(nonce string) SignOption { return func(c *signConfig) { c.nonce = nonce } }

// WithHeaders adds application headers to a signed publish or request. A header
// SAP defines or relies on is refused with [ErrReservedHeader].
func WithHeaders(h nats.Header) SignOption { return func(c *signConfig) { c.headers = h } }

func (c *Client) signedHeader(ctx context.Context, subject string, payload []byte, opts []SignOption) (identity.SenderHeader, signConfig, error) {
	var sc signConfig
	for _, o := range opts {
		o(&sc)
	}
	if err := c.checkOpen(); err != nil {
		return identity.SenderHeader{}, sc, err
	}
	signer := c.signer()
	if signer == nil {
		return identity.SenderHeader{}, sc, ErrNoSigner
	}
	id, err := c.self.ID(ctx)
	if err != nil {
		return identity.SenderHeader{}, sc, local(fmt.Errorf("%w: %w", identity.ErrCannotSign, err))
	}
	sub := sc.sub
	if sub == "" {
		sub = subject
	}
	if sc.nonce == "" {
		if sc.nonce, err = identity.NewNonce(); err != nil {
			return identity.SenderHeader{}, sc, err
		}
	}
	h, err := identity.SignHeader(identity.SignOptions{
		Signer: signer, ID: id, Name: c.senderName(), Subject: sub, Payload: payload, Nonce: sc.nonce,
	})
	return h, sc, err
}

// SignSender returns the Agent-Sender header value that signs payload for
// subject, to attach to a publish the program makes itself (a JetStream publish,
// say). It needs a signer (SAP §13.5, §13.6).
func (c *Client) SignSender(ctx context.Context, subject string, payload []byte, opts ...SignOption) (string, error) {
	h, _, err := c.signedHeader(ctx, subject, payload, opts)
	if err != nil {
		return "", err
	}
	return h.Encode(), nil
}

func signedMsg(subject string, payload []byte, h identity.SenderHeader, sc signConfig) (*nats.Msg, error) {
	hdr := nats.Header{}
	for k, vs := range sc.headers {
		for _, v := range vs {
			if err := validHeader(k, v); err != nil {
				return nil, err
			}
		}
		hdr[k] = append([]string(nil), vs...)
	}
	h.Set(hdr)
	hdr.Set("Nats-Msg-Id", sc.nonce)
	return &nats.Msg{Subject: subject, Data: payload, Header: hdr}, nil
}

// PublishSigned publishes payload to subject with a signed Agent-Sender header
// and Nats-Msg-Id set to the nonce, so a JetStream stream deduplicates a retry
// (SAP §13.5, §13.8).
func (c *Client) PublishSigned(ctx context.Context, subject string, payload []byte, opts ...SignOption) error {
	h, sc, err := c.signedHeader(ctx, subject, payload, opts)
	if err != nil {
		return err
	}
	m, err := signedMsg(subject, payload, h, sc)
	if err != nil {
		return err
	}
	return c.publish(m)
}

// RequestSigned makes a request signed like [Client.PublishSigned] and returns
// the first reply, waiting at most timeout. The reply arrives on the client's
// own reply subscription.
func (c *Client) RequestSigned(ctx context.Context, subject string, payload []byte, timeout time.Duration, opts ...SignOption) (*nats.Msg, error) {
	h, sc, err := c.signedHeader(ctx, subject, payload, opts)
	if err != nil {
		return nil, err
	}
	m, err := signedMsg(subject, payload, h, sc)
	if err != nil {
		return nil, err
	}
	return c.request(ctx, m, timeout)
}

// ResolveSender maps a verified sender's agent ID back to the agent instance
// that registered it, by enumerating $SRV.INFO.agents and keeping only
// instances whose id_sig verifies against their own prompt subject (SAP
// §13.10). The enumeration is cached for the client's resolve TTL. The result
// identifies; it never authorizes. The second result is false when no verified
// instance claims the ID: the sender is then not a reachable agent.
func (c *Client) ResolveSender(ctx context.Context, id identity.AgentID) (identity.Registration, bool, error) {
	if err := c.checkOpen(); err != nil {
		return identity.Registration{}, false, err
	}
	c.resolverMu.Lock()
	if c.resolver == nil {
		c.resolver = identity.NewSenderResolver(
			identity.ConnLister(c.nc, identity.ListOptions{}),
			identity.ResolverOptions{TTL: c.cfg.resolveTTL})
	}
	r := c.resolver
	c.resolverMu.Unlock()
	return r.Resolve(ctx, id)
}

// InvalidateSenders drops the cached sender index; the next [Client.ResolveSender]
// enumerates again.
func (c *Client) InvalidateSenders() {
	c.resolverMu.Lock()
	r := c.resolver
	c.resolverMu.Unlock()
	if r != nil {
		r.Invalidate()
	}
}
