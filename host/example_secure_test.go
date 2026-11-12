// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host_test

import (
	"context"
	"fmt"
	"slices"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/bogworks/synadia-agents.go/identity"
)

// A host that serves verified senders only, and of those only the ones on an
// allow list. The example needs a NATS server, so it compiles but does not run.
func ExampleWithAdmission() {
	var nc *nats.Conn // a connection that authenticates with an NKEY or creds
	subject, _ := agents.NewAgentSubject("secure", "example", "default")

	allowed := []identity.AgentID{"ACCOUNT.USER"} // real IDs: identity.ParseAgentID

	h, err := host.New(nc, subject,
		func(ctx context.Context, req *host.Request, w *host.Responder) error {
			return w.Send(ctx, "hello "+req.Sender.Describe())
		},
		// Unsigned and claimed senders are refused with 401.
		host.WithMinSenderTrust(identity.TrustSigned),
		// Verified senders not on the list are refused with 403.
		host.WithAdmission(func(ctx context.Context, s *identity.Sender, _ *host.RequestInfo) (bool, error) {
			return s != nil && s.Trust == identity.TrustVerified && slices.Contains(allowed, s.ID), nil
		}),
	)
	if err != nil {
		fmt.Println(err)
		return
	}
	_ = h
}

// An extra endpoint is outside the admission gate. Its handler verifies the
// signature, records the nonce and checks the sender itself. The example needs
// a NATS server, so it compiles but does not run.
func ExampleWithEndpoint() {
	nonces := identity.NewNonceSet(identity.NonceSetOptions{})
	opts := identity.LiveOptions{Nonces: nonces}
	allowed := []identity.AgentID{"ACCOUNT.USER"}

	handler := micro.HandlerFunc(func(req micro.Request) {
		sender, err := identity.VerifyLive(identity.Message{
			Subject: req.Subject(), Data: req.Data(), Header: nats.Header(req.Headers()),
		}, opts)
		if err != nil || identity.TrustOf(sender) != identity.TrustVerified {
			_ = req.Error("401", "signature required", nil)
			return
		}
		if !slices.Contains(allowed, sender.ID) {
			_ = req.Error("403", "sender refused", nil)
			return
		}
		_ = req.Respond([]byte("ok"))
	})

	_ = host.WithEndpoint(host.Endpoint{
		Name:    "admin",
		Subject: "agents.admin.secure.example.default",
		Handler: handler,
	})
}
