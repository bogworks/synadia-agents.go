// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller_test

import (
	"context"
	"fmt"
	"log"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/nats-io/nats.go"
)

// Example_quickStart serves an echo agent, discovers it, prompts it and reads
// the reply as it streams. It needs a NATS server at nats.DefaultURL, so it
// compiles but does not run under go test; the same code runs against an
// embedded server in the test module (test/examples).
func Example_quickStart() {
	nc, err := nats.Connect(nats.DefaultURL)
	if err != nil {
		log.Fatal(err)
	}
	defer nc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Host: register an agent. The host sends the ack first and the
	// terminator last.
	subject, err := agents.NewAgentSubject("echo", "me", "default")
	if err != nil {
		log.Fatal(err)
	}
	h, err := host.New(nc, subject, func(ctx context.Context, req *host.Request, w *host.Responder) error {
		return w.Send(ctx, "echo: "+req.Envelope.Prompt)
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := h.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer h.Close()

	// Caller: discover the agent (SAP section 4), prompt it, read the stream.
	c, err := caller.New(nc)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	agent, err := c.Find(ctx, caller.Filter{Agent: "echo"})
	if err != nil {
		log.Fatal(err)
	}
	stream, err := agent.PromptText(ctx, "hello")
	if err != nil {
		log.Fatal(err)
	}
	defer stream.Close()
	for ev, err := range stream.Events() { // ack, response and query events
		if err != nil {
			log.Fatal(err) // *agents.ServiceError, a timeout, ...
		}
		if r, ok := ev.(caller.Response); ok {
			fmt.Print(r.Text)
		}
	}
}
