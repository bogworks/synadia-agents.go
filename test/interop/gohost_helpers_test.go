// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package interop_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/connect"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/bogworks/synadia-agents.go/test/interop"
)

// The Go host under an official caller (AT-003). The handler below mirrors the
// peers' host modes (peers/ts/host.mjs) so the same scenarios run against the
// Go host as against the official hosts in the self-test.

// hostMode is the behavior of the Go host's prompt handler.
type hostMode struct {
	kind      string // echo, query, error, plainerror, attachments, slow
	chunks    int
	errCode   int
	errDesc   string
	slowDelay time.Duration
}

// seen is what the Go host observed for one prompt.
type seen struct {
	Prompt      string
	Attachments []seenAttachment
	Sender      *identity.Sender
	QueryReply  string
}

type seenAttachment struct {
	Name   string
	Size   int
	SHA256 string
}

// goHost is a running Go host and its record of prompts.
type goHost struct {
	*host.Host
	Subject string
	mu      sync.Mutex
	prompts []seen
	notify  chan struct{}
}

func (g *goHost) record(s seen) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prompts = append(g.prompts, s)
	select {
	case g.notify <- struct{}{}:
	default:
	}
}

func (g *goHost) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.prompts)
}

// waitPrompt returns the n-th (0-based) prompt the host handled.
func (g *goHost) waitPrompt(t *testing.T, n int) seen {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		g.mu.Lock()
		if len(g.prompts) > n {
			s := g.prompts[n]
			g.mu.Unlock()
			return s
		}
		g.mu.Unlock()
		select {
		case <-g.notify:
		case <-deadline:
			t.Fatalf("the Go host handled no prompt #%d in 10s", n)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// startGoHost connects to url (as the nkey user in seedFile when non-empty,
// signing when sign) and starts a Go host in the given mode.
func startGoHost(t *testing.T, url, seedFile string, sign bool, m hostMode, opts ...host.Option) *goHost {
	t.Helper()
	src := connect.Source{URL: url, NKey: seedFile}
	var bopts []connect.BundleOption
	if sign {
		bopts = append(bopts, connect.WithSignedIdentity())
	}
	b, err := connect.ResolveBundle(context.Background(), src, bopts...)
	if err != nil {
		t.Fatalf("resolve bundle: %v", err)
	}
	nc, err := nats.Connect(b.URL, b.Options...)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { nc.Close(); b.Wipe() })

	subj, err := agents.NewAgentSubject("interop", "interop", "main")
	if err != nil {
		t.Fatal(err)
	}
	g := &goHost{Subject: subj.Prompt(), notify: make(chan struct{}, 1)}
	all := append([]host.Option{host.WithDescription("interop " + m.kind + " host (go)")}, opts...)
	if b.Signer != nil {
		all = append(all, host.WithIdentity(host.IdentityOptions{Signer: b.Signer}))
	}
	h, err := host.New(nc, subj, g.handler(m), all...)
	if err != nil {
		t.Fatalf("host.New: %v", err)
	}
	g.Host = h
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("host start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = h.Stop(ctx)
	})
	return g
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func splitRunes(text string, n int) []string {
	r := []rune(text)
	size := max(1, (len(r)+n-1)/n)
	var parts []string
	for i := 0; i < len(r); i += size {
		parts = append(parts, string(r[i:min(i+size, len(r))]))
	}
	return parts
}

func (g *goHost) handler(m hostMode) host.PromptHandler {
	return func(ctx context.Context, req *host.Request, w *host.Responder) error {
		s := seen{Prompt: req.Envelope.Prompt, Sender: req.Sender}
		for _, a := range req.Envelope.Attachments {
			data, err := a.Bytes()
			if err != nil {
				return err
			}
			s.Attachments = append(s.Attachments, seenAttachment{a.Filename, len(data), sum(data)})
		}
		defer func() { g.record(s) }()
		switch m.kind {
		case "plainerror":
			return errors.New("secret internal detail")
		case "error":
			return host.Reject(m.errCode, m.errDesc)
		case "query":
			if err := w.Send(ctx, "thinking"); err != nil {
				return err
			}
			ans, err := w.Ask(ctx, "proceed?", 10*time.Second)
			if err != nil {
				return err
			}
			s.QueryReply = ans.Prompt
			return w.Send(ctx, "reply: "+ans.Prompt)
		case "attachments":
			return w.SendAttachments(ctx, "received "+strconv.Itoa(len(req.Envelope.Attachments)), req.Envelope.Attachments...)
		case "slow":
			select {
			case <-time.After(m.slowDelay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		n := m.chunks
		if n < 1 {
			n = 3
		}
		for _, part := range splitRunes(req.Envelope.Prompt, n) {
			if err := w.Send(ctx, part); err != nil {
				return err
			}
		}
		return nil
	}
}

// goHostCallers are the official callers the Go host is tested with.
var goHostCallers = []interop.Impl{interop.TypeScript, interop.Python}

func eachCaller(t *testing.T, fn func(t *testing.T, e *interop.Env, impl interop.Impl)) {
	t.Helper()
	e := interop.Require(t)
	for _, impl := range goHostCallers {
		t.Run(string(impl)+"Caller", func(t *testing.T) {
			t.Parallel()
			fn(t, e, impl)
		})
	}
}

func mustExit0(t *testing.T, res *interop.CallerResult) {
	t.Helper()
	if res.Exit != 0 {
		t.Fatalf("caller exit %d; lines %v; stderr:\n%s", res.Exit, res.Lines, res.Stderr())
	}
}
