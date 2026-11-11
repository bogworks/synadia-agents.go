// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Command signed shows the sender identity of SAP §13 on both sides. The host
// requires a signed sender (min_sender_trust "signed") and reports who sent
// each prompt; the caller signs every prompt and status request with the
// connection's user NKEY.
//
// The connection must authenticate with an NKEY (or a credentials file that
// holds a seed), because the signature is made with the same key the server
// knows the user by (SAP §13.4). One process can run both sides:
//
//	go run ./examples/signed -url nats://127.0.0.1:4222 -nkey ./user.nk
//
// or run -mode host in one terminal and -mode caller in another.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/bogworks/synadia-agents.go/connect"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
)

func main() {
	url := flag.String("url", nats.DefaultURL, "NATS server URL")
	nkey := flag.String("nkey", "", "path of a user seed file")
	creds := flag.String("creds", "", "path of a .creds file (instead of -nkey)")
	natsCtx := flag.String("context", "", "name of a NATS CLI context (instead of -url, -nkey and -creds)")
	mode := flag.String("mode", "both", "host, caller or both")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	src := connect.Source{Context: *natsCtx}
	if *natsCtx == "" {
		src = connect.Source{URL: *url, NKey: *nkey, Creds: *creds}
	}
	if err := run(ctx, src, *mode, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run reads the credentials once, so the connection and the signer cannot
// disagree (SAP §13.4), and then runs the requested side or sides.
func run(ctx context.Context, src connect.Source, mode string, out io.Writer) error {
	bundle, err := connect.ResolveBundle(ctx, src, connect.WithSignedIdentity())
	if err != nil {
		return err
	}
	defer bundle.Wipe()
	nc, err := nats.Connect(bundle.URL, bundle.Options...)
	if err != nil {
		return err
	}
	defer nc.Close()

	switch mode {
	case "host":
		return serve(ctx, nc, bundle.Signer, out, nil)
	case "caller":
		return ask(ctx, nc, bundle.Signer, out)
	case "both":
		errc := make(chan error, 1)
		hostCtx, stopHost := context.WithCancel(ctx)
		defer stopHost()
		ready := make(chan struct{})
		go func() { errc <- serve(hostCtx, nc, bundle.Signer, out, ready) }()
		select {
		case <-ready:
		case err := <-errc:
			return err
		}
		if err := ask(ctx, nc, bundle.Signer, out); err != nil {
			return err
		}
		stopHost()
		return <-errc
	}
	return fmt.Errorf("unknown -mode %q", mode)
}

func serve(ctx context.Context, nc *nats.Conn, signer identity.Signer, out io.Writer, ready chan<- struct{}) error {
	subject, err := agents.NewAgentSubject("signed", "example", "default")
	if err != nil {
		return err
	}
	h, err := host.New(nc, subject,
		func(ctx context.Context, req *host.Request, w *host.Responder) error {
			// req.Sender is verified here: the host serves nothing else.
			return w.Send(ctx, "hello "+req.Sender.Describe())
		},
		// Register user_nkey, account and id_sig and sign heartbeats (SAP §13.10, §13.11).
		host.WithIdentity(host.IdentityOptions{Signer: signer}),
		// Advertise and enforce min_sender_trust (SAP §13.9).
		host.WithMinSenderTrust(identity.TrustSigned),
	)
	if err != nil {
		return err
	}
	if err := h.Start(ctx); err != nil {
		return err
	}
	fmt.Fprintf(out, "ready %s min_sender_trust=%s\n", h.Subject().Prompt(), h.MinSenderTrust())
	if ready != nil {
		close(ready)
	}
	<-ctx.Done()
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return h.Stop(stopCtx)
}

func ask(ctx context.Context, nc *nats.Conn, signer identity.Signer, out io.Writer) error {
	c, err := caller.New(nc, caller.WithIdentity(caller.IdentityOptions{Signer: signer, Name: "signed-example"}))
	if err != nil {
		return err
	}
	defer c.Close()

	target, err := c.Find(ctx, caller.Filter{Agent: "signed"})
	if err != nil {
		return err
	}
	id, err := c.SelfID(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "calling as %s\n", id)

	stream, err := target.PromptText(ctx, "who am I?")
	if err != nil {
		return err
	}
	defer stream.Close()
	for ev, err := range stream.Events() {
		if err != nil {
			if agents.IsCode(err, agents.CodeUnauthorized) || errors.Is(err, agents.ErrForbidden) {
				return fmt.Errorf("the host refused this sender: %w", err)
			}
			return err
		}
		if r, ok := ev.(caller.Response); ok {
			fmt.Fprintln(out, r.Text)
		}
	}
	return nil
}
