// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"iter"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nkeys"
)

// checkLeaks fails the test if goroutines started during it are still running
// a moment after it ends (stdlib NumGoroutine, with retries).
func checkLeaks(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		var n int
		for i := 0; i < 100; i++ {
			if n = runtime.NumGoroutine(); n <= before {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		buf := make([]byte, 1<<16)
		t.Errorf("goroutine leak: %d before, %d after\n%s", before, n, buf[:runtime.Stack(buf, true)])
	})
}

type fakeBackend struct {
	mu      sync.Mutex
	targets []*fakeTarget
	err     error
	filters []caller.Filter
	waits   []time.Duration
	inbox   []string
}

func (f *fakeBackend) inboxPrefixes() []string { return f.inbox }

func (f *fakeBackend) discover(_ context.Context, flt caller.Filter, timeout time.Duration) ([]target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filters = append(f.filters, flt)
	f.waits = append(f.waits, timeout)
	if f.err != nil {
		return nil, f.err
	}
	var out []target
	for _, tg := range f.targets {
		i := tg.inf
		if (flt.Agent != "" && flt.Agent != i.Agent) || (flt.Owner != "" && flt.Owner != i.Owner) || (flt.Name != "" && flt.Name != i.Name) {
			continue
		}
		out = append(out, tg)
	}
	return out, nil
}

// fakeTarget scripts one agent instance. onPrompt gets each prompt and the
// stream it must feed.
type fakeTarget struct {
	subject string
	inf     agents.AgentInfo
	trust   identity.MinSenderTrust
	// promptErr, when set, is returned by prompt.
	promptErr error

	mu       sync.Mutex
	envs     []agents.Envelope
	ctxs     []context.Context
	maxWaits []time.Duration
	streams  []*fakeStream
	params   []promptParams
	// auto, when set, runs in its own goroutine for every prompt, to play the agent.
	auto func(ctx context.Context, env agents.Envelope, s *fakeStream)
}

func newFakeTarget(agent, owner, name string, md map[string]string) *fakeTarget {
	subject := "agents.prompt." + agent + "." + owner + "." + name
	if md == nil {
		md = map[string]string{}
	}
	return &fakeTarget{
		subject: subject,
		inf: agents.AgentInfo{
			InstanceID: "inst-" + name, Agent: agent, Owner: owner, Name: name,
			Description: "the " + agent, Metadata: md,
			Prompt: agents.EndpointInfo{Name: "prompt", Subject: subject, Metadata: map[string]string{"attachments_ok": "true", "max_payload": "1MiB"}},
		},
		trust: identity.TrustAny,
	}
}

func (f *fakeTarget) promptSubject() string                   { return f.subject }
func (f *fakeTarget) info() agents.AgentInfo                  { return f.inf }
func (f *fakeTarget) minSenderTrust() identity.MinSenderTrust { return f.trust }
func (f *fakeTarget) handle() *caller.Agent                   { return nil }

func (f *fakeTarget) prompt(ctx context.Context, env agents.Envelope, pp promptParams) (callStream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	maxWait := pp.maxWait
	f.params = append(f.params, pp)
	if f.promptErr != nil {
		return nil, f.promptErr
	}
	s := newFakeStream()
	// A canceled context drops the stream, as the real caller does.
	go func() {
		select {
		case <-ctx.Done():
			s.close()
		case <-s.closedCh:
		}
	}()
	f.envs = append(f.envs, env)
	f.ctxs = append(f.ctxs, ctx)
	f.maxWaits = append(f.maxWaits, maxWait)
	f.streams = append(f.streams, s)
	if f.auto != nil {
		go f.auto(ctx, env, s)
	}
	return s, nil
}

// stream waits for the nth prompt's stream.
func (f *fakeTarget) stream(t *testing.T, n int) *fakeStream {
	t.Helper()
	for i := 0; i < 500; i++ {
		f.mu.Lock()
		if len(f.streams) > n {
			s := f.streams[n]
			f.mu.Unlock()
			return s
		}
		f.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("prompt %d never arrived", n)
	return nil
}

type fakeItem struct {
	ev  streamEvent
	err error
	end bool
}

type fakeStream struct {
	ch       chan fakeItem
	closedCh chan struct{}
	once     sync.Once
	term     atomic.Bool
}

func newFakeStream() *fakeStream {
	return &fakeStream{ch: make(chan fakeItem, 64), closedCh: make(chan struct{})}
}

func (s *fakeStream) close()           { s.once.Do(func() { close(s.closedCh) }) }
func (s *fakeStream) terminated() bool { return s.term.Load() }

func (s *fakeStream) events() iter.Seq2[streamEvent, error] {
	return func(yield func(streamEvent, error) bool) {
		for {
			select {
			case it := <-s.ch:
				switch {
				case it.err != nil:
					yield(streamEvent{}, it.err)
					return
				case it.end:
					s.term.Store(true)
					return
				}
				if !yield(it.ev, nil) {
					return
				}
			case <-s.closedCh:
				return
			}
		}
	}
}

func (s *fakeStream) text(x string, atts ...agents.Attachment) {
	s.ch <- fakeItem{ev: streamEvent{text: x, attachments: atts}}
}
func (s *fakeStream) end()         { s.ch <- fakeItem{end: true} }
func (s *fakeStream) fail(e error) { s.ch <- fakeItem{err: e} }
func (s *fakeStream) ask(q *fakeQuestion) {
	s.ch <- fakeItem{ev: streamEvent{query: &question{prompt: q.prompt, replySubject: q.subject, attachments: q.atts, reply: q.reply}}}
}

// fakeQuestion records the reply it gets.
type fakeQuestion struct {
	subject string
	prompt  string
	atts    []agents.Attachment
	replies chan string
	once    atomic.Bool
}

func newFakeQuestion(prompt string) *fakeQuestion {
	return &fakeQuestion{prompt: prompt, subject: "_INBOX.agent1.q1", replies: make(chan string, 4)}
}

func (q *fakeQuestion) reply(_ context.Context, a string) error {
	if !q.once.CompareAndSwap(false, true) {
		return &caller.QueryAlreadyRepliedError{ID: q.prompt}
	}
	q.replies <- a
	return nil
}

func (q *fakeQuestion) got(t *testing.T) string {
	t.Helper()
	select {
	case a := <-q.replies:
		return a
	case <-time.After(5 * time.Second):
		t.Fatalf("question %q was never replied to", q.prompt)
		return ""
	}
}

func (q *fakeQuestion) noReply(t *testing.T) {
	t.Helper()
	select {
	case a := <-q.replies:
		t.Fatalf("question %q got an unexpected reply %q", q.prompt, a)
	case <-time.After(50 * time.Millisecond):
	}
}

// identityMetadata returns registration metadata that claims a fresh agent ID
// (not signed: identity_verified stays false).
func identityMetadata(t *testing.T) (map[string]string, string) {
	t.Helper()
	acct, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatal(err)
	}
	user, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	ap, _ := acct.PublicKey()
	up, _ := user.PublicKey()
	return map[string]string{identity.MetadataKeyAccount: ap, identity.MetadataKeyUserNKey: up}, ap + "." + up
}

var errBoom = errors.New("boom")
