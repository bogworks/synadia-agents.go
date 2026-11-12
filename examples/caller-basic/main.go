// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Command caller-basic discovers an agent, reads its status, sends it a prompt
// and prints the streamed reply. It answers a mid-stream question (SAP §7) with
// the -answer flag.
//
//	go run ./examples/caller-basic -url nats://127.0.0.1:4222 -prompt "hello"
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
	"github.com/nats-io/nats.go"
)

func main() {
	url := flag.String("url", nats.DefaultURL, "NATS server URL")
	agent := flag.String("agent", "", "only agents with this metadata.agent (default: any)")
	prompt := flag.String("prompt", "hello", "the prompt to send")
	answer := flag.String("answer", "yes", "the reply to any question the agent asks")
	wait := flag.Duration("timeout", 30*time.Second, "overall time limit")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *wait)
	defer cancel()

	nc, err := nats.Connect(*url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer nc.Close()

	if err := run(ctx, nc, os.Stdout, caller.Filter{Agent: *agent}, *prompt, *answer); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run is the whole example.
func run(ctx context.Context, nc *nats.Conn, out io.Writer, f caller.Filter, prompt, answer string) error {
	c, err := caller.New(nc)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.Background()) }()

	// Discover: $SRV.INFO.agents (SAP §4.1).
	found, err := c.Discover(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "discovered %d agent instance(s)\n", len(found))
	for _, a := range found {
		info := a.Info()
		fmt.Fprintf(out, "  %s/%s instance=%s subject=%s\n", info.Agent, info.Owner, info.InstanceID, a.PromptSubject())
	}

	target, err := c.Find(ctx, f)
	if err != nil {
		return err
	}

	// Status: the same payload as a heartbeat (SAP §8.7).
	hb, err := target.Status(ctx)
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	fmt.Fprintf(out, "status: instance=%s interval=%s\n", hb.InstanceID, hb.Interval())

	// Prompt and stream (SAP §5, §6).
	stream, err := target.PromptText(ctx, prompt)
	if err != nil {
		return err
	}
	defer stream.Close()
	for ev, err := range stream.Events() {
		if err != nil {
			var se *agents.ServiceError
			if errors.As(err, &se) {
				return fmt.Errorf("the agent answered %d: %s", se.Code, se.Description)
			}
			return err
		}
		switch e := ev.(type) {
		case caller.Status:
			fmt.Fprintf(out, "[%s]\n", e.Value)
		case caller.Response:
			fmt.Fprint(out, e.Text)
		case *caller.Query:
			fmt.Fprintf(out, "\n[question] %s -> %s\n", e.Prompt, answer)
			if err := e.Reply(ctx, answer); err != nil {
				return err
			}
		}
	}
	fmt.Fprintln(out)
	return nil
}
