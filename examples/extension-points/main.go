// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Command extension-points uses every extension point the SDK offers a program
// built on it, with an invented "tenant" convention that has nothing to do with
// the protocol or with any one consumer:
//
//   - per-instance tokens: two instances of one agent, each with its own owner
//     and instance name, and a custom third subject token (SAP §2, §3.2);
//   - an extra micro endpoint the SDK registers and never interprets;
//   - the admission hook, which decides who may prompt (SAP §13.7.3);
//   - read access to the headers of a received prompt;
//   - the outgoing-header hook, which adds a header to every prompt.
//
// It runs both sides in one process against the server at -url:
//
//	go run ./examples/extension-points -url nats://127.0.0.1:4222
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
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
)

const tenantHeader = "X-Example-Tenant"

func main() {
	url := flag.String("url", nats.DefaultURL, "NATS server URL")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	nc, err := nats.Connect(*url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer nc.Close()

	if err := run(ctx, nc, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// newInstance builds one instance. Each call picks its own owner, name and
// third token: the per-instance tokens of REQ-016.
func newInstance(nc *nats.Conn, owner, name string) (*host.Host, error) {
	subject, err := agents.NewAgentSubject("ext", owner, name, agents.WithSubjectToken("ex"))
	if err != nil {
		return nil, err
	}
	handler := func(ctx context.Context, req *host.Request, w *host.Responder) error {
		// Received-header access: a clone of every header of the request.
		return w.Send(ctx, fmt.Sprintf("%s/%s saw tenant %q", owner, name, req.Header.Get(tenantHeader)))
	}
	return host.New(nc, subject, handler,
		// An extra micro endpoint, advertised in $SRV.INFO and never
		// interpreted by the SDK. Its subject is advertised verbatim.
		host.WithEndpoint(host.Endpoint{
			Name:    "tenants",
			Subject: "example.ext." + owner + "." + name + ".tenants",
			Handler: micro.HandlerFunc(func(r micro.Request) { _ = r.Respond([]byte("acme")) }),
		}),
		// The admission hook decides who may prompt; the SDK decides nothing.
		// It sees the classified sender (nil when the message is unsigned) and
		// the received headers.
		host.WithAdmission(func(_ context.Context, _ *identity.Sender, req *host.RequestInfo) (bool, error) {
			return req.Header.Get(tenantHeader) == "acme", nil
		}),
	)
}

// run starts two instances and prompts each, once with and once without the
// header the admission hook wants.
func run(ctx context.Context, nc *nats.Conn, out io.Writer) error {
	for _, in := range [][2]string{{"alice", "one"}, {"bob", "two"}} {
		h, err := newInstance(nc, in[0], in[1])
		if err != nil {
			return err
		}
		if err := h.Start(ctx); err != nil {
			return err
		}
		defer func() { _ = h.Close() }()
	}

	// The outgoing-header hook adds the tenant header to every prompt. It may
	// not set a header SAP defines (caller.ReservedHeader).
	withTenant, err := caller.New(nc, caller.WithRequestHeaders(
		func(context.Context, *caller.PromptInfo) (map[string]string, error) {
			return map[string]string{tenantHeader: "acme"}, nil
		}))
	if err != nil {
		return err
	}
	defer func() { _ = withTenant.Close() }()
	plain, err := caller.New(nc)
	if err != nil {
		return err
	}
	defer func() { _ = plain.Close() }()

	found, err := withTenant.Discover(ctx, caller.WithFilter(caller.Filter{Agent: "ext"}))
	if err != nil {
		return err
	}
	for _, a := range found {
		info := a.Info()
		if ep, ok := info.Endpoint("tenants"); ok {
			fmt.Fprintf(out, "%s advertises endpoint %q on %s\n", info.Owner, ep.Name, ep.Subject)
		}
		if err := prompt(ctx, out, "with the hook", a); err != nil {
			return err
		}
		other, err := plain.LookupInstance(ctx, a.InstanceID())
		if err != nil {
			return err
		}
		if err := prompt(ctx, out, "without it", other); err != nil {
			return err
		}
	}
	return nil
}

func prompt(ctx context.Context, out io.Writer, label string, a *caller.Agent) error {
	stream, err := a.PromptText(ctx, "hello")
	if err != nil {
		return err
	}
	defer stream.Close()
	for ev, err := range stream.Events() {
		if err != nil {
			var se *agents.ServiceError
			if errors.As(err, &se) {
				fmt.Fprintf(out, "%s: refused %d %s\n", label, se.Code, se.Description)
				return nil
			}
			return err
		}
		if r, ok := ev.(caller.Response); ok {
			fmt.Fprintf(out, "%s: %s\n", label, r.Text)
		}
	}
	return nil
}
