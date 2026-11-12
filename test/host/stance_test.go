// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host_test

import (
	"testing"
	"time"

	"github.com/bogworks/synadia-agents.go/host"
	"github.com/bogworks/synadia-agents.go/identity"
)

// TestStance_PromptEndpoint asserts F1: with SignedSenders on the prompt
// endpoint advertises min_sender_trust signed and refuses an unsigned prompt
// with 401, while with it off or unset it advertises any and serves it; a signed
// prompt is served in every case; signing on alone still serves unsigned
// senders; and an explicit WithMinSenderTrust wins over the stance.
func TestStance_PromptEndpoint(t *testing.T) {
	env := startNkeyServer(t)
	nc := env.conn(t, carol)
	cl := env.conn(t, alice)
	withSigner := host.WithIdentity(host.IdentityOptions{Signer: signerOf(t, carol)})
	tests := []struct {
		name      string
		opts      []host.Option
		advertise string
		unsigned  int // expected code for an unsigned prompt, 0 = served
	}{
		{"no stance", nil, "any", 0},
		{"senders unset", []host.Option{host.WithStance(identity.Stance{Signing: identity.Off})}, "any", 0},
		{"senders on", []host.Option{host.WithStance(identity.Stance{SignedSenders: identity.On})}, "signed", 401},
		{"senders on, signing on", []host.Option{host.WithStance(identity.Stance{SignedSenders: identity.On, Signing: identity.On}), withSigner}, "signed", 401},
		{"senders off", []host.Option{host.WithStance(identity.Stance{SignedSenders: identity.Off})}, "any", 0},
		{"signing on only", []host.Option{host.WithStance(identity.Stance{Signing: identity.On}), withSigner}, "any", 0},
		{"signing on without a signer, RequireSigner off", []host.Option{host.WithStance(identity.Stance{SignedSenders: identity.On, Signing: identity.On, RequireSigner: identity.Off})}, "signed", 401},
		{"explicit any beats senders on", []host.Option{host.WithStance(identity.Stance{SignedSenders: identity.On}), host.WithMinSenderTrust(identity.TrustAny)}, "any", 0},
		{"explicit signed beats senders off", []host.Option{host.WithMinSenderTrust(identity.TrustSigned), host.WithStance(identity.Stance{SignedSenders: identity.Off})}, "signed", 401},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := "s" + "abcdefghijklmnopqrstuvwxyz"[i:i+1]
			h := newHostNamed(t, nc, name, sawSender, tc.opts...)
			ep := endpointOf(t, info(t, nc, h.InstanceID()), "prompt")
			if got := ep.Metadata["min_sender_trust"]; got != tc.advertise {
				t.Fatalf("advertised %q, want %q", got, tc.advertise)
			}
			subj := h.Subject().Prompt()

			s := rawPrompt(t, cl, subj, []byte("x"), nil, 5*time.Second)
			if tc.unsigned == 0 {
				if s.errorFrame() != nil {
					t.Fatalf("an unsigned prompt was refused: %+v", s.errorFrame())
				}
				if got := s.texts(t); len(got) != 1 || got[0] != string(identity.TrustAbsent) {
					t.Fatalf("handler saw %v", got)
				}
			} else if se := s.errorFrame(); se == nil || se.Code != tc.unsigned {
				t.Fatalf("an unsigned prompt: want %d, got %+v", tc.unsigned, se)
			}

			s = rawPrompt(t, cl, subj, []byte("x"), signed(t, alice, subj, []byte("x"), nil), 5*time.Second)
			if s.errorFrame() != nil {
				t.Fatalf("a signed prompt was refused: %+v", s.errorFrame())
			}
			if got := s.texts(t); len(got) != 1 || got[0] != string(identity.TrustVerified) {
				t.Fatalf("handler saw %v", got)
			}
		})
	}
}
