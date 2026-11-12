// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package examples_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/nats-io/nats.go"
)

// TestQuickstart runs the quick start of caller.Example_quickStart (the root
// module cannot run it: it needs a server) against an embedded nats-server.
// The code is the Example's, with the server URL the only difference.
func TestQuickstart(t *testing.T) {
	srv := startServer(t)
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	subject, err := agents.NewAgentSubject("echo", "me", "default")
	if err != nil {
		t.Fatal(err)
	}
	h, err := host.New(nc, subject, func(ctx context.Context, req *host.Request, w *host.Responder) error {
		return w.Send(ctx, "echo: "+req.Envelope.Prompt)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	c, err := caller.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	agent, err := c.Find(ctx, caller.Filter{Agent: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := agent.PromptText(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var got strings.Builder
	for ev, err := range stream.Events() {
		if err != nil {
			t.Fatal(err)
		}
		if r, ok := ev.(caller.Response); ok {
			got.WriteString(r.Text)
		}
	}
	if got.String() != "echo: hello" {
		t.Fatalf("reply = %q, want %q", got.String(), "echo: hello")
	}
}

// TestQuickstart_ReadmeMatchesExample: the README quick start is the Example,
// byte for byte, so the two cannot drift.
func TestQuickstart_ReadmeMatchesExample(t *testing.T) {
	src, err := os.ReadFile("../../caller/example_quickstart_test.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "func Example_quickStart() {")
	if i < 0 {
		t.Fatal("Example_quickStart not found")
	}
	end := strings.Index(s[i:], "\n}\n")
	if end < 0 {
		t.Fatal("end of Example_quickStart not found")
	}
	fn := s[i : i+end+3]
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), fn) {
		t.Fatal("README.md does not contain caller.Example_quickStart verbatim")
	}
}
