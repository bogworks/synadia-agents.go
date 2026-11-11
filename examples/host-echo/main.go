// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Command host-echo serves an agent that answers every prompt with its own
// text, prefixed "echo: ". It prints "ready" on standard output once it is
// registered.
//
//	go run ./examples/host-echo -url nats://127.0.0.1:4222
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/nats-io/nats.go"
)

func main() {
	url := flag.String("url", nats.DefaultURL, "NATS server URL")
	agent := flag.String("agent", "echo", "metadata.agent")
	owner := flag.String("owner", "example", "metadata.owner")
	name := flag.String("name", "default", "instance name")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	nc, err := nats.Connect(*url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer nc.Close()

	if err := run(ctx, nc, os.Stdout, *agent, *owner, *name); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run serves until ctx ends.
func run(ctx context.Context, nc *nats.Conn, out io.Writer, agent, owner, name string) error {
	subject, err := agents.NewAgentSubject(agent, owner, name)
	if err != nil {
		return err
	}
	h, err := host.New(nc, subject, func(ctx context.Context, req *host.Request, w *host.Responder) error {
		// The host has already sent the ack and sends the terminator when
		// this returns (SAP §6).
		return w.Send(ctx, "echo: "+req.Envelope.Prompt)
	})
	if err != nil {
		return err
	}
	if err := h.Start(ctx); err != nil {
		return err
	}
	fmt.Fprintf(out, "ready %s\n", h.Subject().Prompt())

	<-ctx.Done()
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return h.Stop(stopCtx)
}
