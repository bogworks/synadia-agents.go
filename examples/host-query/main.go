// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Command host-query serves an agent that asks the caller a question in the
// middle of the stream (SAP §7) and answers according to the reply.
//
//	go run ./examples/host-query -url nats://127.0.0.1:4222
//	go run ./examples/caller-basic -agent quiz -answer yes
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
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/nats-io/nats.go"
)

func main() {
	url := flag.String("url", nats.DefaultURL, "NATS server URL")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

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

func handle(ctx context.Context, req *host.Request, w *host.Responder) error {
	if err := w.Send(ctx, "working on: "+req.Envelope.Prompt+"\n"); err != nil {
		return err
	}
	// The agent chooses how long it waits (SAP §7.3). The stream stays open.
	reply, err := w.Ask(ctx, "continue? (yes/no)", 20*time.Second)
	switch {
	case errors.Is(err, host.ErrQueryTimeout):
		// No answer: this agent goes on with a default instead of failing.
		return w.Send(ctx, "no answer, continuing\n")
	case err != nil:
		return err
	}
	if reply.Prompt == "yes" {
		return w.Send(ctx, "continuing\n")
	}
	return w.Send(ctx, "stopping\n")
}

// run serves until ctx ends.
func run(ctx context.Context, nc *nats.Conn, out io.Writer) error {
	subject, err := agents.NewAgentSubject("quiz", "example", "default")
	if err != nil {
		return err
	}
	h, err := host.New(nc, subject, handle)
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
	return h.Shutdown(stopCtx)
}
