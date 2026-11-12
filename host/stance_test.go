// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

func stanceSigner(t *testing.T) identity.Signer {
	t.Helper()
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	seed, err := kp.Seed()
	if err != nil {
		t.Fatal(err)
	}
	s, err := identity.SignerFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newStanceHost(t *testing.T, opts ...Option) (*Host, error) {
	t.Helper()
	subj, err := agents.NewAgentSubject("a", "o", "n")
	if err != nil {
		t.Fatal(err)
	}
	return New(&nats.Conn{}, subj, func(context.Context, *Request, *Responder) error { return nil }, opts...)
}

// TestWithStance_SignedSendersAndSigning asserts F1 and the signer requirement:
// the prompt endpoint advertises and enforces the level the SignedSenders
// setting gives, an explicit WithMinSenderTrust wins whatever the order, and
// signing on makes New fail without a signer unless RequireSigner is off.
func TestWithStance_SignedSendersAndSigning(t *testing.T) {
	signer := stanceSigner(t)
	withSigner := WithIdentity(IdentityOptions{Signer: signer})
	noSigner := WithIdentity(IdentityOptions{})
	st := func(s identity.Stance) Option { return WithStance(s) }
	on, off := identity.On, identity.Off
	tests := []struct {
		name    string
		opts    []Option
		want    identity.MinSenderTrust
		wantErr bool
	}{
		{"no stance", nil, identity.TrustAny, false},
		{"senders unset is the default", []Option{st(identity.Stance{Signing: off})}, identity.TrustAny, false},
		{"senders on", []Option{st(identity.Stance{SignedSenders: on})}, identity.TrustSigned, false},
		{"senders off", []Option{st(identity.Stance{SignedSenders: off})}, identity.TrustAny, false},
		{"signing on with signer", []Option{st(identity.Stance{Signing: on}), withSigner}, identity.TrustAny, false},
		{"signing on without identity", []Option{st(identity.Stance{Signing: on})}, "", true},
		{"signing on with identity but no signer", []Option{st(identity.Stance{Signing: on}), noSigner}, "", true},
		{"signing on, RequireSigner on, no signer", []Option{st(identity.Stance{Signing: on, RequireSigner: on})}, "", true},
		{"signing on, RequireSigner off, no signer", []Option{st(identity.Stance{Signing: on, RequireSigner: off})}, identity.TrustAny, false},
		{"signing off, RequireSigner on, no signer", []Option{st(identity.Stance{Signing: off, RequireSigner: on})}, identity.TrustAny, false},
		{"signing unset, no signer", []Option{st(identity.Stance{SignedSenders: on})}, identity.TrustSigned, false},
		{"explicit any beats senders on", []Option{st(identity.Stance{SignedSenders: on}), WithMinSenderTrust(identity.TrustAny)}, identity.TrustAny, false},
		{"explicit any beats senders on, other order", []Option{WithMinSenderTrust(identity.TrustAny), st(identity.Stance{SignedSenders: on})}, identity.TrustAny, false},
		{"explicit signed beats senders off", []Option{st(identity.Stance{SignedSenders: off}), WithMinSenderTrust(identity.TrustSigned)}, identity.TrustSigned, false},
		{"explicit signed beats senders off, other order", []Option{WithMinSenderTrust(identity.TrustSigned), st(identity.Stance{SignedSenders: off})}, identity.TrustSigned, false},
		{"invalid senders", []Option{st(identity.Stance{SignedSenders: 3})}, "", true},
		{"invalid signing", []Option{st(identity.Stance{Signing: 3})}, "", true},
		{"invalid RequireSigner", []Option{st(identity.Stance{RequireSigner: 3})}, "", true},
		{"invalid setting", []Option{st(identity.Stance{HeartbeatVerification: 3})}, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, err := newStanceHost(t, tc.opts...)
			if tc.wantErr {
				if !errors.Is(err, ErrConfig) {
					t.Fatalf("err = %v, want ErrConfig", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if h.cfg.minTrust != tc.want {
				t.Errorf("min_sender_trust %q, want %q", h.cfg.minTrust, tc.want)
			}
			if got := h.promptMeta[identity.MetadataKeyMinSenderTrust]; got != string(tc.want) {
				t.Errorf("advertised %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWithStance_RequireSignerOffWarnsOnce asserts that signing on without a
// signer and RequireSigner off logs one warning to the configured logger, and
// does not panic without a logger.
func TestWithStance_RequireSignerOffWarnsOnce(t *testing.T) {
	s := WithStance(identity.Stance{Signing: identity.On, RequireSigner: identity.Off})
	var buf bytes.Buffer
	if _, err := newStanceHost(t, s, WithLogger(slog.New(slog.NewTextHandler(&buf, nil)))); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != 1 || !strings.Contains(buf.String(), "no Signer") {
		t.Fatalf("%d warnings, want 1: %s", n, buf.String())
	}
	if _, err := newStanceHost(t, s); err != nil {
		t.Fatal(err)
	}
	var quiet bytes.Buffer
	withSigner := WithIdentity(IdentityOptions{Signer: stanceSigner(t)})
	if _, err := newStanceHost(t, s, withSigner, WithLogger(slog.New(slog.NewTextHandler(&quiet, nil)))); err != nil {
		t.Fatal(err)
	}
	if quiet.Len() != 0 {
		t.Fatalf("a signer was given, but got %q", quiet.String())
	}
}

// TestWithStance_WarnsOnlyToTheConfiguredLogger asserts that an unchosen stance
// logs one warning per process to a logger the program set, never when no
// logger was set, and not when a stance or a min_sender_trust was chosen.
func TestWithStance_WarnsOnlyToTheConfiguredLogger(t *testing.T) {
	logTo := func(buf *bytes.Buffer) Option {
		return WithLogger(slog.New(slog.NewTextHandler(buf, nil)))
	}
	warnings := func(buf *bytes.Buffer) int { return strings.Count(buf.String(), "no security settings were chosen") }

	t.Run("once, to the configured logger", func(t *testing.T) {
		resetStanceWarning()
		var buf bytes.Buffer
		for range 3 {
			if _, err := newStanceHost(t, logTo(&buf)); err != nil {
				t.Fatal(err)
			}
		}
		if n := warnings(&buf); n != 1 {
			t.Fatalf("%d warnings, want 1: %s", n, buf.String())
		}
		for _, want := range []string{"level=WARN", "SDK defaults", "WithStance"} {
			if !strings.Contains(buf.String(), want) {
				t.Errorf("warning lacks %q: %s", want, buf.String())
			}
		}
	})
	t.Run("no logger, no warning and the once is kept", func(t *testing.T) {
		resetStanceWarning()
		if _, err := newStanceHost(t); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if _, err := newStanceHost(t, logTo(&buf)); err != nil {
			t.Fatal(err)
		}
		if n := warnings(&buf); n != 1 {
			t.Fatalf("%d warnings, want 1", n)
		}
	})
	t.Run("never the default logger", func(t *testing.T) {
		resetStanceWarning()
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		t.Cleanup(func() { slog.SetDefault(prev) })
		if _, err := newStanceHost(t); err != nil {
			t.Fatal(err)
		}
		if buf.Len() != 0 {
			t.Fatalf("the default logger got %q", buf.String())
		}
	})
	for name, opts := range map[string][]Option{
		"stance chosen":      {WithStance(identity.Stance{SignedSenders: identity.Off})},
		"min trust explicit": {WithMinSenderTrust(identity.TrustAny)},
	} {
		t.Run(name+", no warning", func(t *testing.T) {
			resetStanceWarning()
			var buf bytes.Buffer
			if _, err := newStanceHost(t, append(opts, logTo(&buf))...); err != nil {
				t.Fatal(err)
			}
			if n := warnings(&buf); n != 0 {
				t.Fatalf("%d warnings: %s", n, buf.String())
			}
		})
	}
}
