// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package connect

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/bogworks/synadia-agents.go/internal/stance"
)

// Source selects where a [Bundle] gets its connection credentials: either a
// NATS CLI context by name, or a URL with at most one of a creds or nkey file.
// Exactly one of Context and URL is set. See parity row 1.7; outside SAP.
type Source struct {
	// Context is the name of a NATS CLI context, or "current".
	Context string
	// URL is a NATS URL (see [ParseURL]).
	URL string
	// Creds is the path of a .creds file; with URL only. "~/" is expanded.
	Creds string
	// NKey is the path of a user seed file; with URL only. "~/" is expanded.
	NKey string
}

// String shows the source with URL userinfo removed (a malformed URL prints as
// a placeholder). Key file paths are shown; their contents never are.
func (s Source) String() string {
	return fmt.Sprintf("connect.Source{Context:%q URL:%q Creds:%q NKey:%q}", s.Context, displayURL(s.URL), s.Creds, s.NKey)
}

// GoString is String, so %#v cannot print URL userinfo.
func (s Source) GoString() string { return s.String() }

// MarshalJSON redacts URL userinfo; the field names are the Go field names.
func (s Source) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct{ Context, URL, Creds, NKey string }{s.Context, displayURL(s.URL), s.Creds, s.NKey})
}

// Validate reports why the source is ambiguous or empty. The error matches
// [ErrContext].
func (s Source) Validate() error {
	hasCtx, hasURL := s.Context != "", s.URL != ""
	switch {
	case hasCtx == hasURL:
		return ctxErr("", "connection source must select exactly one of Context or URL")
	case hasCtx && (s.Creds != "" || s.NKey != ""):
		return ctxErr("", "a context connection cannot also select direct Creds or NKey credentials")
	case s.Creds != "" && s.NKey != "":
		return ctxErr("", "a URL connection must select at most one of Creds or NKey")
	}
	return nil
}

// BundleOption configures [ResolveBundle].
type BundleOption func(*bundleConfig)

type bundleConfig struct {
	signed bool
	stance identity.Stance
	ctx    []ContextOption
}

// WithSignedIdentity makes [ResolveBundle] return a Signer derived from the
// same credential read as the connection options (SAP §13.4, §13.6). It fails,
// matching [ErrNoUserSeed], if the source holds no user seed. Without it and
// without a [WithStance] that turns signing on, the default is no signer
// ("off"). It wins over a stance that turns signing off. See parity row 1.8.
func WithSignedIdentity() BundleOption { return func(c *bundleConfig) { c.signed = true } }

// WithStance sets the security stance (see [identity.Stance]). [ResolveBundle]
// reads one check of it: Signing makes the bundle carry the Signer when the
// source holds a user seed, as [WithSignedIdentity] does. Unlike
// WithSignedIdentity, a signing that came only from the profile and finds no
// seed yields no Signer and no error; an explicit Signing On fails with
// [ErrNoUserSeed] like WithSignedIdentity. An invalid stance fails
// ResolveBundle with an error that matches [ErrContext]. The default is
// [identity.DefaultProfile]. SDK policy; outside SAP.
func WithStance(s identity.Stance) BundleOption { return func(c *bundleConfig) { c.stance = s } }

// WithBundleContextOptions passes context-location options (such as
// [WithConfigDir]) to the context read of [ResolveBundle].
func WithBundleContextOptions(opts ...ContextOption) BundleOption {
	return func(c *bundleConfig) { c.ctx = append(c.ctx, opts...) }
}

// Bundle is a connection and, when requested, a signer, both made from one
// read of the credential files, so a rotation between two reads cannot split
// the connection's authentication from the sender signer (SAP §13.4). See
// parity rows 1.6 and 1.9.
//
// Use it as nats.Connect(b.URL, b.Options...). Printing or marshaling a
// Bundle shows only whether it is signed.
type Bundle struct {
	Connection
	// Signer is non-nil only with [WithSignedIdentity] or a [WithStance] that
	// turns signing on, and only when the source holds a user seed. It is the same key
	// the connection authenticates with.
	Signer identity.Signer

	auth   identity.Signer // the connection's own signer; wiped by Wipe
	signed bool
	once   sync.Once
}

// Wipe clears the retained key material and drops the options. Call it only
// after the connection has closed: the connection signs reconnects through the
// same key, so wiping early makes the next reconnect fail to authenticate. It
// is idempotent but not safe to call concurrently with use of the Bundle.
//
// Wipe clears the key held by the signer. Go cannot guarantee that copies made
// by nats.go, the TLS stack (a client certificate's private key) or the
// runtime are zeroed. See parity row 1.10.
func (b *Bundle) Wipe() {
	b.once.Do(func() {
		wipeSigner(b.auth)
		wipeSigner(b.Signer)
		b.Options = nil
	})
}

// String reports only the identity mode.
func (b *Bundle) String() string {
	if b.signed {
		return "connect.Bundle(signed)"
	}
	return "connect.Bundle(off)"
}

// GoString is String, so %#v cannot print option values.
func (b *Bundle) GoString() string { return b.String() }

// MarshalJSON reports only the identity mode.
func (b *Bundle) MarshalJSON() ([]byte, error) {
	m := "off"
	if b.signed {
		m = "signed"
	}
	return json.Marshal(struct {
		Identity string `json:"identity"`
	}{m})
}

// ResolveBundle resolves src into a [Bundle]: the URL and options, and with
// [WithSignedIdentity] the signer behind them. A context source reads the
// context and the key files it names once; a URL source parses the URL (its
// userinfo applies unless Creds or NKey is set) and reads the one file. A
// canceled ctx fails before any read. The error matches [ErrContext]. See
// parity rows 1.6 to 1.9; outside SAP (deviation D-1).
func ResolveBundle(ctx context.Context, src Source, opts ...BundleOption) (*Bundle, error) {
	var cfg bundleConfig
	for _, o := range opts {
		o(&cfg)
	}
	eff, serr := stance.Resolve(cfg.stance)
	if serr != nil {
		return nil, ctxErrWrap("", serr, "invalid security stance")
	}
	// Explicit signing (WithSignedIdentity, or Signing On) fails without a seed;
	// signing from the profile alone yields no signer instead.
	wantSigner := cfg.signed || eff.Signing
	mustSign := cfg.signed || (eff.Signing && eff.Explicit.Signing)
	if err := ctx.Err(); err != nil {
		// Match both ErrContext and context.Canceled / DeadlineExceeded.
		return nil, ctxErrWrap("", err, "resolving the connection source was canceled")
	}
	if err := src.Validate(); err != nil {
		return nil, err
	}
	var (
		conn   Connection
		signer identity.Signer
	)
	if src.Context != "" {
		s, err := loadSnapshot(ctx, src.Context, cfg.ctx)
		if err != nil {
			return nil, err
		}
		conn, signer = s.conn, s.signer
		if mustSign && signer == nil {
			return nil, noSeedError(s.name)
		}
	} else {
		ua, err := parseURL(src.URL)
		if err != nil {
			return nil, err
		}
		relax := newContextConfig(cfg.ctx).insecureKeyFiles
		conn = Connection{URL: strings.Join(ua.servers, ",")}
		switch {
		case src.Creds != "":
			a, err := credsAuth(ctx, "", src.Creds, relax)
			if err != nil {
				return nil, err
			}
			conn.Options, signer = append(conn.Options, a.opt), a.signer
		case src.NKey != "":
			a, err := nkeyFileAuth(ctx, "", src.NKey, relax)
			if err != nil {
				return nil, err
			}
			conn.Options, signer = append(conn.Options, a.opt), a.signer
		default:
			conn.Options = append(conn.Options, ua.options()...)
		}
		if mustSign && signer == nil {
			return nil, noSeedError("")
		}
	}
	b := &Bundle{Connection: conn, auth: signer}
	if wantSigner && signer != nil {
		b.Signer, b.signed = signer, true
	}
	return b, nil
}
