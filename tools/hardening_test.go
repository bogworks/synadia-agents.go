// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/nats-io/nats.go"
)

// --- AT-012/013 audit finding 1: the attachment open race ------------------------

func TestAttachments_LinkSwappedAfterTheCheckIsRefused(t *testing.T) {
	allowed, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("TOP-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(allowed, "in.txt"), []byte("fine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(allowed, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(allowed, "sub", "f.txt"), []byte("fine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "f.txt"), []byte("TOP-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(allowed, "probe")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_ = os.Remove(filepath.Join(allowed, "probe"))

	cases := map[string]struct {
		path string
		swap func(real string) error
	}{
		"the file becomes a link": {
			path: filepath.Join(allowed, "in.txt"),
			swap: func(real string) error {
				if err := os.Remove(real); err != nil {
					return err
				}
				return os.Symlink(filepath.Join(outside, "secret.txt"), real)
			},
		},
		"a directory on the way becomes a link": {
			path: filepath.Join(allowed, "sub", "f.txt"),
			swap: func(real string) error {
				dir := filepath.Dir(real)
				if err := os.Rename(dir, dir+".bak"); err != nil {
					return err
				}
				return os.Symlink(outside, dir)
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tg := echoTarget()
			h := newHarness(t, []*fakeTarget{tg}, WithAttachmentRoots(allowed))
			t.Cleanup(func() { // put the tree back for the next case
				if fi, err := os.Lstat(filepath.Join(allowed, "sub")); err == nil && fi.Mode()&os.ModeSymlink != 0 {
					_ = os.Remove(filepath.Join(allowed, "sub"))
					_ = os.Rename(filepath.Join(allowed, "sub.bak"), filepath.Join(allowed, "sub"))
				}
				_ = os.Remove(filepath.Join(allowed, "in.txt"))
				_ = os.WriteFile(filepath.Join(allowed, "in.txt"), []byte("fine"), 0o600)
			})
			swapped := false
			h.tl.afterAttachmentCheck = func(real string) {
				if err := tc.swap(real); err != nil {
					t.Errorf("swap: %v", err)
				}
				swapped = true
			}
			r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "attachments": []string{tc.path}})
			if !swapped {
				t.Fatal("the hook never ran")
			}
			if !strings.HasPrefix(str(r, "error"), "the prompt was not sent: the attachment ") || len(r) != 1 {
				t.Fatalf("a swapped link must refuse the prompt: %v", r)
			}
			tg.mu.Lock()
			defer tg.mu.Unlock()
			if len(tg.envs) != 0 {
				t.Fatalf("a prompt was sent: %+v", tg.envs)
			}
			noLocalPaths(t, r, allowed, outside)
		})
	}
}

func TestAttachments_ReadThroughTheRootKeepsTheContents(t *testing.T) {
	allowed := t.TempDir()
	body := bytes.Repeat([]byte("0123456789"), 1000)
	if err := os.Mkdir(filepath.Join(allowed, "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(allowed, "d", "big.bin"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg}, WithAttachmentRoots(allowed))
	go answerWith(t, tg, 0, "ok")
	wantState(t, h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "attachments": []string{filepath.Join(allowed, "d", "big.bin")}}), "completed")
	tg.mu.Lock()
	defer tg.mu.Unlock()
	a := tg.envs[0].Attachments
	if len(a) != 1 || a[0].Filename != "big.bin" {
		t.Fatalf("%+v", a)
	}
	got, err := a[0].Bytes()
	if err != nil || !bytes.Equal(got, body) {
		t.Errorf("contents differ: %v", err)
	}
}

// noLocalPaths fails when a result shows the model any of the directories.
func noLocalPaths(t *testing.T, r any, dirs ...string) {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	dirs = append(dirs, os.TempDir()+string(filepath.Separator)+"Test")
	for _, d := range dirs {
		if strings.Contains(string(b), d) {
			t.Errorf("the result shows the local path %q: %s", d, b)
		}
	}
}

// --- finding 2: where an answer may be published -----------------------------------

func askAndStop(t *testing.T, h *harness, tg *fakeTarget, q *fakeQuestion) string {
	t.Helper()
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "wait": false})
	tg.stream(t, 0).ask(q)
	eventually(t, "question taken in", func() bool {
		return str(h.do(ToolListAgentCalls, nil)["calls"].([]any)[0].(obj), "state") == "input_required"
	})
	return str(r, "call_id")
}

func TestAnswerReplyPrefixes_UntrustedSubjectIsRefusedAndNothingIsPublished(t *testing.T) {
	for _, subject := range []string{"app.commands.deploy", "orders.create", "_INBOXX.a.b", "_INBOX.", "agents.prompt.x.y.z"} {
		t.Run(subject, func(t *testing.T) {
			tg := echoTarget()
			h := newHarness(t, []*fakeTarget{tg})
			q := newFakeQuestion("deploy?")
			q.subject = subject
			id := askAndStop(t, h, tg, q)
			r := h.do(ToolAnswerAgent, obj{"call_id": id, "answer": "yes"})
			if !strings.Contains(str(r, "error"), "do not trust") || len(r) > 3 {
				t.Fatalf("%v", r)
			}
			q.noReply(t)
			// The question stays open, and stopping the call does not publish there either.
			if got := h.do(ToolListAgentCalls, nil)["calls"].([]any)[0].(obj); str(got, "state") != "input_required" {
				t.Errorf("%v", got)
			}
			h.do(ToolCancelAgent, obj{"call_ids": []string{id}})
			q.noReply(t)
		})
	}
}

func TestAnswerReplyPrefixes_DefaultsAndOption(t *testing.T) {
	cases := []struct {
		name     string
		opts     []Option
		inbox    []string
		subject  string
		accepted bool
	}{
		{"default inbox", nil, nil, "_INBOX.abc.def", true},
		{"default refuses an app subject", nil, nil, "app.replies.1", false},
		{"connection inbox prefix", nil, []string{"MYBOX"}, "MYBOX.a.b", true},
		{"connection inbox prefix with a dot", nil, []string{"MYBOX."}, "MYBOX.a.b", true},
		{"connection prefix keeps the default", nil, []string{"MYBOX"}, "_INBOX.a.b", true},
		{"connection prefix is whole tokens", nil, []string{"MYBOX"}, "MYBOXES.a", false},
		{"explicit list", []Option{WithAnswerReplyPrefixes("app.replies")}, nil, "app.replies.7", true},
		{"explicit list replaces the default", []Option{WithAnswerReplyPrefixes("app.replies")}, nil, "_INBOX.a.b", false},
		{"explicit list replaces the connection prefix", []Option{WithAnswerReplyPrefixes("app.replies")}, []string{"MYBOX"}, "MYBOX.a.b", false},
		{"explicit list is whole tokens", []Option{WithAnswerReplyPrefixes("app.replies")}, nil, "app.repliesX.1", false},
		{"explicit list, several", []Option{WithAnswerReplyPrefixes("a.b", "c.d.")}, nil, "c.d.x", true},
		{"the protocol namespace stays refused", []Option{WithAnswerReplyPrefixes("agents")}, nil, "agents.prompt.x.y.z", false},
		{"system subjects stay refused", []Option{WithAnswerReplyPrefixes("$SYS")}, nil, "$SYS.REQ.x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tl := mustNew(t, &fakeBackend{inbox: tc.inbox}, tc.opts...)
			err := tl.replyAllowed(tc.subject)
			if (err == nil) != tc.accepted {
				t.Errorf("replyAllowed(%q) = %v, want accepted=%v", tc.subject, err, tc.accepted)
			}
		})
	}
}

func TestAnswerReplyPrefixes_TheClientIsHandedTheSamePolicy(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg}, WithAnswerReplyPrefixes("app.replies"))
	go answerWith(t, tg, 0, "ok")
	h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
	tg.mu.Lock()
	defer tg.mu.Unlock()
	pol := tg.params[0].replyPolicy
	if pol == nil {
		t.Fatal("no per-prompt reply policy was passed to the caller")
	}
	if pol("app.replies.1") != nil || pol("_INBOX.a.b") == nil || pol("agents.prompt.x") == nil {
		t.Error("the per-prompt policy is not the tools' policy")
	}
}

func TestAnswerReplyPrefixes_TrustedSubjectIsAnswered(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg}, WithAnswerReplyPrefixes("app.replies"))
	q := newFakeQuestion("go?")
	q.subject = "app.replies.42"
	id := askAndStop(t, h, tg, q)
	go func() {
		if got := q.got(t); got != "yes" {
			t.Errorf("answer %q", got)
		}
		s := tg.stream(t, 0)
		s.text("done")
		s.end()
	}()
	wantState(t, h.do(ToolAnswerAgent, obj{"call_id": id, "answer": "yes", "wait": true}), "completed")
}

func TestAnswerReplyPrefixes_OptionValidation(t *testing.T) {
	for name, o := range map[string]Option{
		"none":     WithAnswerReplyPrefixes(),
		"empty":    WithAnswerReplyPrefixes(""),
		"wildcard": WithAnswerReplyPrefixes("app.*"),
		"space":    WithAnswerReplyPrefixes("app replies"),
	} {
		if _, err := newTools(&fakeBackend{}, o); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// --- finding 3: per-call caps against a hostile stream ------------------------------

func TestDefaults_Limits(t *testing.T) {
	tl := mustNew(t, nil)
	for name, tc := range map[string]struct{ got, want int }{
		"reply bytes":  {tl.cfg.maxReplyBytes, 4 << 20},
		"questions":    {tl.cfg.maxQuestions, 64},
		"files":        {tl.cfg.maxFiles, 64},
		"result bytes": {tl.cfg.maxResultBytes, 8 << 20},
		"depth":        {tl.cfg.maxDepth, 4},
		"active calls": {tl.cfg.maxActive, 1024},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: default %d, want %d", name, tc.got, tc.want)
		}
	}
	for name, o := range map[string]Option{
		"reply bytes": WithMaxReplyBytes(0), "questions": WithMaxQuestions(-1), "files": WithMaxReturnedFiles(0),
		"result bytes": WithMaxResultBytes(0), "depth": WithMaxDepth(0), "active": WithMaxActiveCalls(0),
	} {
		if _, err := newTools(&fakeBackend{}, o); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func b64(n int) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("a"), n)) }

func waitFailed(t *testing.T, h *harness, id string) obj {
	t.Helper()
	var r obj
	eventually(t, "the call fails", func() bool {
		r = h.do(ToolWaitAgent, obj{"call_ids": []string{id}, "timeout_ms": 20})
		return str(r, "state") == "failed"
	})
	return r
}

func TestLimits_HostileStreamEndsTheCallAndClosesTheStream(t *testing.T) {
	cases := []struct {
		name string
		opts []Option
		feed func(s *fakeStream, qs []*fakeQuestion)
		want string
	}{
		{
			name: "reply text", opts: []Option{WithMaxReplyBytes(100)}, want: "limit of 100B of text",
			feed: func(s *fakeStream, _ []*fakeQuestion) {
				for i := 0; i < 10; i++ {
					s.text(strings.Repeat("x", 30))
				}
			},
		},
		{
			name: "question count", opts: []Option{WithMaxQuestions(2)}, want: "more than 2 questions",
			feed: func(s *fakeStream, qs []*fakeQuestion) {
				for _, q := range qs {
					s.ask(q)
				}
			},
		},
		{
			name: "file records", opts: []Option{WithMaxReturnedFiles(2)}, want: "more than 2 files",
			feed: func(s *fakeStream, _ []*fakeQuestion) {
				a := agents.Attachment{Filename: "f.txt", Content: b64(3)}
				s.text("files", a, a, a)
			},
		},
		{
			name: "total result size", opts: []Option{WithMaxResultBytes(500)}, want: "total size limit",
			feed: func(s *fakeStream, _ []*fakeQuestion) {
				for i := 0; i < 5; i++ {
					s.text(strings.Repeat("x", 200))
				}
			},
		},
		{
			name: "question text counts toward the total", opts: []Option{WithMaxResultBytes(300)}, want: "total size limit",
			feed: func(s *fakeStream, qs []*fakeQuestion) {
				s.text(strings.Repeat("x", 200))
				for _, q := range qs {
					s.ask(q)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tg := echoTarget()
			h := newHarness(t, []*fakeTarget{tg}, tc.opts...)
			qs := []*fakeQuestion{newFakeQuestion(strings.Repeat("q", 120)), newFakeQuestion(strings.Repeat("r", 120)), newFakeQuestion(strings.Repeat("s", 120))}
			id := str(h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "wait": false}), "call_id")
			s := tg.stream(t, 0)
			tc.feed(s, qs)
			r := waitFailed(t, h, id)
			if !strings.Contains(str(r, "error"), tc.want) {
				t.Errorf("reason %q, want it to contain %q", str(r, "error"), tc.want)
			}
			select {
			case <-s.closedCh:
			case <-time.After(time.Second):
				t.Error("the stream was not closed")
			}
			// Whatever was open is refused; nothing is left waiting.
			for _, q := range qs {
				if q.once.Load() {
					if got := q.got(t); got != QuestionRefusal {
						t.Errorf("a question was answered %q", got)
					}
				}
			}
			if n, _ := r["partial_reply"].(string); len(n) > 500 {
				t.Errorf("the failed call kept %d bytes of text", len(n))
			}
			tl := h.tl
			tl.mu.Lock()
			defer tl.mu.Unlock()
			if tl.active != 0 {
				t.Errorf("a failed call still holds a budget slot: %d", tl.active)
			}
		})
	}
}

func TestLimits_FilesOverTheCapAreNotSaved(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg}, WithMaxReturnedFiles(1))
	id := str(h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "wait": false}), "call_id")
	a := agents.Attachment{Filename: "f.txt", Content: b64(3)}
	tg.stream(t, 0).text("files", a, a)
	waitFailed(t, h, id)
	if _, err := os.Stat(filepath.Join(h.staging, id)); !os.IsNotExist(err) {
		t.Errorf("files of a message over the cap were saved: %v", err)
	}
}

func TestLimits_WithinTheCapsACallCompletes(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg}, WithMaxReplyBytes(100), WithMaxReturnedFiles(2), WithMaxResultBytes(2000))
	go func() {
		s := tg.stream(t, 0)
		s.text(strings.Repeat("x", 100), agents.Attachment{Filename: "f", Content: b64(3)}, agents.Attachment{Filename: "g", Content: b64(3)})
		s.end()
	}()
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
	wantState(t, r, "completed")
}

// --- finding 4: the loop guard ---------------------------------------------------------

// agent is one agent of a delegation ring: it serves a prompt by prompting the
// next agent with its tools, as a model told to "hand it on" would.
func buildRing(t *testing.T, n int, opts ...Option) (first *Tools, firstAddr string, prompts *atomic.Int32, last *atomic.Value) {
	t.Helper()
	fb := &fakeBackend{}
	targets := make([]*fakeTarget, n)
	tls := make([]*Tools, n)
	for i := range targets {
		targets[i] = newFakeTarget(fmt.Sprintf("ring%d", i), "t", "n", nil)
		fb.targets = append(fb.targets, targets[i])
	}
	for i := range tls {
		tls[i] = mustNew(t, fb, append([]Option{WithSelfAddress(targets[i].subject), WithStagingDir(filepath.Join(t.TempDir(), "s"))}, opts...)...)
	}
	prompts, last = new(atomic.Int32), new(atomic.Value)
	for i := range targets {
		me, next := tls[i], targets[(i+1)%n]
		targets[i].auto = func(ctx context.Context, _ agents.Envelope, s *fakeStream) {
			prompts.Add(1)
			// What the wire carries: the header the interceptor would have stamped.
			h := nats.Header{}
			if d, ok := ctx.Value(outgoingDepthKey{}).(int); ok {
				h.Set(DepthHeader, strconv.Itoa(d))
			}
			_ = me.RunInPromptScope(ContextWithDepth(context.Background(), DepthFromHeader(h)), "", func(sc context.Context) error {
				res, _ := me.Execute(sc, ToolPromptAgent, json.RawMessage(fmt.Sprintf(`{"address":%q,"prompt":"hand it on"}`, next.subject)))
				if e, ok := res["error"].(string); ok {
					last.Store(e)
				}
				s.text("passed")
				s.end()
				return nil
			})
		}
	}
	return tls[0], targets[1].subject, prompts, last
}

func TestDepth_ThreeAgentCycleStopsAtTheMaxDepth(t *testing.T) {
	checkLeaks(t)
	first, addr, prompts, last := buildRing(t, 3)
	res, err := first.Execute(context.Background(), ToolPromptAgent, json.RawMessage(fmt.Sprintf(`{"address":%q,"prompt":"go"}`, addr)))
	if err != nil {
		t.Fatal(err)
	}
	if CallResult(res).State() != StateCompleted {
		t.Fatalf("%v", res)
	}
	// The first prompt is made outside a served prompt and carries no depth; each
	// hop after it carries one more, so the ring stops after DefaultMaxDepth more.
	if got, want := int(prompts.Load()), DefaultMaxDepth+1; got != want {
		t.Errorf("%d prompts went round the ring, want %d", got, want)
	}
	if e, _ := last.Load().(string); !strings.Contains(e, "handed on 4 times") {
		t.Errorf("the last hop was told %q", e)
	}
}

func TestDepth_OptionChangesTheLimit(t *testing.T) {
	checkLeaks(t)
	first, addr, prompts, _ := buildRing(t, 3, WithMaxDepth(1))
	if _, err := first.Execute(context.Background(), ToolPromptAgent, json.RawMessage(fmt.Sprintf(`{"address":%q,"prompt":"go"}`, addr))); err != nil {
		t.Fatal(err)
	}
	if got := int(prompts.Load()); got != 2 {
		t.Errorf("%d prompts, want 2", got)
	}
}

func TestDepth_InterceptorReadsTheHeaderAndTheCallerInterceptorStampsIt(t *testing.T) {
	checkLeaks(t)
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	h.fb.targets = []*fakeTarget{tg}
	hdr := nats.Header{}
	hdr.Set(DepthHeader, "4")
	var res Result
	err := h.tl.Interceptor().AroundRequest(context.Background(), &host.RequestInfo{Header: hdr}, func(ctx context.Context) error {
		res, _ = h.tl.Execute(ctx, ToolPromptAgent, json.RawMessage(fmt.Sprintf(`{"address":%q,"prompt":"x"}`, tg.subject)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(str(res, "error"), "handed on 4 times") {
		t.Errorf("a prompt at the limit may not delegate: %v", res)
	}
	// One hop short of the limit goes out, stamped with one more.
	hdr.Set(DepthHeader, "3")
	go answerWith(t, tg, 0, "ok")
	_ = h.tl.Interceptor().AroundRequest(context.Background(), &host.RequestInfo{Header: hdr}, func(ctx context.Context) error {
		res, _ = h.tl.Execute(ctx, ToolPromptAgent, json.RawMessage(fmt.Sprintf(`{"address":%q,"prompt":"x"}`, tg.subject)))
		return nil
	})
	wantState(t, obj(res), "completed")
	tg.mu.Lock()
	pctx := tg.ctxs[0]
	tg.mu.Unlock()
	ex, err := DepthInterceptor().BeforePrompt(pctx, &caller.PromptInfo{})
	if err != nil || ex == nil || ex.Headers[DepthHeader] != "4" {
		t.Fatalf("the outgoing prompt carries %+v, %v", ex, err)
	}
	if caller.ReservedHeader(DepthHeader) {
		t.Error("the caller refuses the depth header")
	}
	if ex, err := DepthInterceptor().BeforePrompt(context.Background(), &caller.PromptInfo{}); err != nil || ex != nil {
		t.Errorf("a prompt outside a served prompt carries %+v, %v", ex, err)
	}
}

func TestDepth_HeaderParsing(t *testing.T) {
	for in, want := range map[string]int{"": 0, "0": 0, "3": 3, "-2": 0, "x": 0, "1.5": 0, "99999999999999999999": 0, "2000000": 1 << 20} {
		h := nats.Header{}
		if in != "" {
			h.Set(DepthHeader, in)
		}
		if got := DepthFromHeader(h); got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}
	if DepthFromHeader(nil) != 0 {
		t.Error("nil header")
	}
}

func TestBudget_ActiveCallsAcrossScopesAreBounded(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg}, WithMaxActiveCalls(3))
	args := obj{"address": tg.subject, "prompt": "x", "wait": false}
	for i := 0; i < 2; i++ {
		wantState(t, h.do(ToolPromptAgent, args), "running")
	}
	var inside, outside obj
	err := h.tl.RunInPromptScope(context.Background(), "", func(ctx context.Context) error {
		wantState(t, h.run(ctx, ToolPromptAgent, args), "running") // the third, in a scope of its own
		inside = h.run(ctx, ToolPromptAgent, args)
		outside = h.do(ToolPromptAgent, args)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []obj{inside, outside} {
		if !strings.Contains(str(r, "error"), "too many calls are open across all prompts (the limit is 3)") {
			t.Errorf("%v", r)
		}
	}
	// The served prompt ended, so its call is gone from the count: one more fits.
	eventually(t, "the budget frees up", func() bool {
		return str(h.do(ToolPromptAgent, args), "state") == "running"
	})
	if r := h.do(ToolPromptAgent, args); str(r, "error") == "" {
		t.Errorf("the budget did not hold: %v", r)
	}
}

func TestBudget_FinishedCallsFreeTheirSlot(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg}, WithMaxActiveCalls(1))
	for i := 0; i < 3; i++ {
		go answerWith(t, tg, i, "ok")
		wantState(t, h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"}), "completed")
	}
	// A prompt refused before it is sent does not leak a slot either.
	tg.promptErr = errBoom
	for i := 0; i < 3; i++ {
		if r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"}); !strings.HasPrefix(str(r, "error"), "the prompt was not sent") {
			t.Fatalf("%v", r)
		}
	}
	h.tl.mu.Lock()
	defer h.tl.mu.Unlock()
	if h.tl.active != 0 {
		t.Errorf("%d slots held", h.tl.active)
	}
}

// --- finding 5: Close keeps to its deadline ----------------------------------------------

func TestClose_ManyPendingQuestionsHonourTheDeadline(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	const calls, perCall = 6, 20
	for i := 0; i < calls; i++ {
		h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "wait": false})
	}
	var replies atomic.Int32
	for i := 0; i < calls; i++ {
		s := tg.stream(t, i)
		for j := 0; j < perCall; j++ {
			// A reply that never completes by itself, as one to a stalled connection.
			s.ch <- fakeItem{ev: streamEvent{query: &question{
				prompt: fmt.Sprintf("q%d.%d", i, j), replySubject: "_INBOX.a.b",
				reply: func(ctx context.Context, _ string) error { replies.Add(1); <-ctx.Done(); return ctx.Err() },
			}}}
		}
	}
	eventually(t, "every question taken in", func() bool {
		h.tl.mu.Lock()
		defer h.tl.mu.Unlock()
		n := 0
		for _, c := range h.tl.root.order {
			n += len(c.questions)
		}
		return n == calls*perCall
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := h.tl.Close(ctx)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Close took %v against a 300ms deadline", took)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Close returned %v, want the context's deadline error", err)
	}
	if replies.Load() == 0 {
		t.Error("no refusal was attempted")
	}
	// Every call was canceled at once, whatever became of the refusals.
	for _, c := range h.tl.root.order {
		h.tl.mu.Lock()
		st := c.state
		h.tl.mu.Unlock()
		if st != StateCancelled {
			t.Errorf("call %s is %s", c.id, st)
		}
	}
}

func TestClose_RefusalsGoOutConcurrently(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	const n = 8
	h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "wait": false})
	var inflight, peak atomic.Int32
	s := tg.stream(t, 0)
	for j := 0; j < n; j++ {
		s.ch <- fakeItem{ev: streamEvent{query: &question{
			prompt: fmt.Sprint(j), replySubject: "_INBOX.a.b",
			reply: func(ctx context.Context, _ string) error {
				cur := inflight.Add(1)
				for {
					p := peak.Load()
					if cur <= p || peak.CompareAndSwap(p, cur) {
						break
					}
				}
				time.Sleep(100 * time.Millisecond)
				inflight.Add(-1)
				return nil
			},
		}}}
	}
	eventually(t, "every question taken in", func() bool {
		h.tl.mu.Lock()
		defer h.tl.mu.Unlock()
		return len(h.tl.root.order[0].questions) == n
	})
	start := time.Now()
	if err := h.tl.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 600*time.Millisecond || peak.Load() < 2 {
		t.Errorf("refusals went one at a time: took %v, peak %d", took, peak.Load())
	}
}

// --- finding 6: stable model-facing errors ------------------------------------------------

func TestErrors_ResultsShowNoLocalPathsOrTransportDetail(t *testing.T) {
	secret := t.TempDir()
	leak := fmt.Errorf("open %s/x.sock: connection refused to 10.1.2.3:4222", secret)
	notAFile := filepath.Join(secret, "plain")
	if err := os.WriteFile(notAFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(secret, "outside.txt")
	_ = os.WriteFile(outsideFile, []byte("x"), 0o600)

	check := func(t *testing.T, r any) {
		t.Helper()
		noLocalPaths(t, r, secret)
		if b, _ := json.Marshal(r); strings.Contains(string(b), "10.1.2.3") || strings.Contains(string(b), "connection refused") {
			t.Errorf("transport detail reached the model: %s", b)
		}
	}
	t.Run("discovery", func(t *testing.T) {
		h := newHarness(t, nil)
		h.fb.err = leak
		r := h.do(ToolDiscoverAgents, nil)
		if !strings.HasPrefix(str(r, "error"), "discovery failed") {
			t.Fatalf("%v", r)
		}
		check(t, r)
	})
	t.Run("lookup", func(t *testing.T) {
		h := newHarness(t, nil)
		h.fb.err = leak
		r := h.do(ToolPromptAgent, obj{"address": "agents.prompt.a.b.c", "prompt": "x"})
		if !strings.Contains(str(r, "error"), "could not look up") {
			t.Fatalf("%v", r)
		}
		check(t, r)
	})
	t.Run("prompt not sent", func(t *testing.T) {
		tg := echoTarget()
		tg.promptErr = leak
		h := newHarness(t, []*fakeTarget{tg})
		r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
		if str(r, "error") != "the prompt was not sent; the agent could not be reached" {
			t.Fatalf("%v", r)
		}
		check(t, r)
	})
	t.Run("attachments", func(t *testing.T) {
		tg := echoTarget()
		h := newHarness(t, []*fakeTarget{tg}, WithAttachmentRoots(t.TempDir()))
		for _, p := range []string{filepath.Join(secret, "nope.txt"), secret, outsideFile} {
			r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "attachments": []string{p}})
			if str(r, "error") == "" {
				t.Fatalf("%q: %v", p, r)
			}
			check(t, r)
		}
	})
	t.Run("staging unavailable", func(t *testing.T) {
		tg := echoTarget()
		h := newHarness(t, []*fakeTarget{tg}, WithStagingDir(filepath.Join(notAFile, "staging")))
		r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "attachments": []string{outsideFile}})
		if !strings.Contains(str(r, "error"), "staging directory is unavailable") {
			t.Fatalf("%v", r)
		}
		check(t, r)
	})
	t.Run("returned file cannot be saved", func(t *testing.T) {
		tg := echoTarget()
		h := newHarness(t, []*fakeTarget{tg}, WithStagingDir(filepath.Join(notAFile, "staging")))
		go func() {
			s := tg.stream(t, 0)
			s.text("f", agents.Attachment{Filename: "a.txt", Content: b64(3)})
			s.end()
		}()
		r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
		wantState(t, r, "failed")
		if str(r, "error") != savedFailure {
			t.Errorf("%v", r)
		}
		check(t, r)
	})
	t.Run("stream failure", func(t *testing.T) {
		tg := echoTarget()
		h := newHarness(t, []*fakeTarget{tg})
		go func() { tg.stream(t, 0).fail(leak) }()
		r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
		wantState(t, r, "failed")
		check(t, r)
	})
	t.Run("answer cannot be sent", func(t *testing.T) {
		tg := echoTarget()
		h := newHarness(t, []*fakeTarget{tg})
		q := &question{prompt: "q", replySubject: "_INBOX.a.b", reply: func(context.Context, string) error { return leak }}
		id := str(h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "wait": false}), "call_id")
		tg.stream(t, 0).ch <- fakeItem{ev: streamEvent{query: q}}
		eventually(t, "question taken in", func() bool {
			return str(h.do(ToolListAgentCalls, nil)["calls"].([]any)[0].(obj), "state") == "input_required"
		})
		r := h.do(ToolAnswerAgent, obj{"call_id": id, "answer": "yes"})
		if str(r, "error") != "the answer could not be sent; the question is still open" {
			t.Fatalf("%v", r)
		}
		check(t, r)
	})
	t.Run("extensions", func(t *testing.T) {
		tg := echoTarget()
		h := newHarness(t, []*fakeTarget{tg}, WithExtensions(&failingExt{err: leak}))
		r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
		if str(r, "error") == "" {
			t.Fatalf("%v", r)
		}
		check(t, r)
	})
}

type failingExt struct{ err error }

func (failingExt) Name() string { return "failing" }
func (f *failingExt) RewritePrompt(context.Context, *PromptContext) (*PromptRewrite, error) {
	return nil, f.err
}

func TestErrors_DetailGoesToTheLogger(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, nil))
	h := newHarness(t, nil, WithLogger(lg))
	h.fb.err = errors.New("dial tcp 10.9.9.9:4222: refused")
	h.do(ToolDiscoverAgents, nil)
	if !strings.Contains(buf.String(), "10.9.9.9:4222") {
		t.Errorf("the detail was not logged: %q", buf.String())
	}
}

// --- finding 7 is TestStaging_GivenDirectoryIsKept; finding 8 -----------------------------

func TestListAgentCalls_ValidatesItsArguments(t *testing.T) {
	h := newHarness(t, nil)
	for name, tc := range map[string]struct {
		args   string
		refuse bool
	}{
		"none":              {"", false},
		"null":              {"null", false},
		"empty object":      {"{}", false},
		"unknown property":  {`{"x":1}`, false},
		"double encoded":    {`"{}"`, false},
		"truncated object":  {`{`, true},
		"garbage":           {`nope`, true},
		"array":             {`[1]`, true},
		"number":            {`7`, true},
		"bad double encode": {`"{"`, true},
	} {
		t.Run(name, func(t *testing.T) {
			r, err := h.tl.Execute(context.Background(), ToolListAgentCalls, json.RawMessage(tc.args))
			if err != nil {
				t.Fatal(err)
			}
			_, refused := r["error"]
			if refused != tc.refuse {
				t.Errorf("%q: %v", tc.args, r)
			}
		})
	}
}

// TestAttachments_BoundedBeforeReading asserts that a model-supplied list is
// bounded before any file is read: many copies of one path are refused by
// count, a set whose sizes sum past the target's max_payload is refused, and
// a target that declares attachments_ok=false refuses any. Each refusal sends
// nothing and reads nothing.
func TestAttachments_BoundedBeforeReading(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), 600), 0o600); err != nil {
		t.Fatal(err)
	}
	small := filepath.Join(dir, "small.txt")
	if err := os.WriteFile(small, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	mk := func(md map[string]string) (*harness, *fakeTarget, *int) {
		tg := echoTarget()
		for k, v := range md {
			tg.inf.Prompt.Metadata[k] = v
		}
		h := newHarness(t, []*fakeTarget{tg}, WithAttachmentRoots(dir), WithMaxAttachments(8))
		reads := new(int)
		h.tl.afterAttachmentCheck = func(string) { *reads++ }
		return h, tg, reads
	}
	repeat := func(p string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = p
		}
		return out
	}
	for name, tc := range map[string]struct {
		md    map[string]string
		paths []string
		want  string
	}{
		"count":      {nil, repeat(big, 50), "too many attachments (50); send at most 8"},
		"sum":        {map[string]string{"max_payload": "1000B"}, repeat(big, 2), "larger than the agent accepts"},
		"not accept": {map[string]string{"attachments_ok": "false"}, []string{small}, "does not accept attachments"},
	} {
		t.Run(name, func(t *testing.T) {
			h, tg, reads := mk(tc.md)
			r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "attachments": tc.paths})
			if !strings.Contains(str(r, "error"), tc.want) || len(r) != 1 {
				t.Fatalf("%v", r)
			}
			if *reads != 0 || len(tg.envs) != 0 {
				t.Fatalf("read %d files, sent %d prompts", *reads, len(tg.envs))
			}
		})
	}
	t.Run("within limits", func(t *testing.T) {
		h, tg, _ := mk(map[string]string{"max_payload": "1000B"})
		go answerWith(t, tg, 0, "ok")
		wantState(t, h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "attachments": []string{big, small}}), "completed")
		if len(tg.envs[0].Attachments) != 2 {
			t.Fatalf("%+v", tg.envs[0].Attachments)
		}
	})
}

// Each file fits max_payload alone; the second does not fit what the first left.
func TestAttachments_EachFileIsCheckedAgainstTheRemainingBudget(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.bin"), filepath.Join(dir, "b.bin")
	for p, n := range map[string]int{a: 600, b: 500} {
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tg := echoTarget()
	tg.inf.Prompt.Metadata["max_payload"] = "1000B"
	h := newHarness(t, []*fakeTarget{tg}, WithAttachmentRoots(dir))
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "attachments": []string{a, b}})
	if !strings.Contains(str(r, "error"), "larger than the agent accepts") || len(r) != 1 {
		t.Fatalf("%v", r)
	}
	tg.mu.Lock()
	defer tg.mu.Unlock()
	if len(tg.envs) != 0 {
		t.Fatalf("a prompt was sent: %+v", tg.envs)
	}
}

// --- Codex: the pinned root and publication against closure ---------------------------

// A root directory swapped for a link to an outside directory between the point
// the root is pinned and the read cannot redirect the read: the secret in the
// outside directory is never sent, whichever way the call ends.
func TestAttachments_RootDirectorySwappedForALinkNeverReadsOutside(t *testing.T) {
	parent := t.TempDir()
	allowed, outside := filepath.Join(parent, "allowed"), filepath.Join(parent, "outside")
	for _, d := range []string{allowed, outside} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(allowed, "in.txt"), []byte("fine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "in.txt"), []byte("TOP-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg}, WithAttachmentRoots(allowed)) // pinned here
	swap := func() error {
		if err := os.Rename(allowed, allowed+".bak"); err != nil {
			return err
		}
		return os.Symlink(outside, allowed)
	}
	swapped := false
	h.tl.afterAttachmentCheck = func(string) {
		if err := swap(); err != nil {
			t.Errorf("swap: %v", err)
			return
		}
		swapped = true
	}
	go answerWith(t, tg, 0, "ok")
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "attachments": []string{filepath.Join(allowed, "in.txt")}})
	if !swapped {
		t.Fatal("the hook never ran")
	}
	tg.mu.Lock()
	defer tg.mu.Unlock()
	for _, env := range tg.envs {
		for _, a := range env.Attachments {
			b, err := a.Bytes()
			if err != nil || strings.Contains(string(b), "TOP-SECRET") {
				t.Fatalf("the outside secret was sent: %q, %v", b, err)
			}
			if string(b) != "fine" {
				t.Errorf("read %q, want the pinned directory's file", b)
			}
		}
	}
	noLocalPaths(t, r, parent)
}

// A root swapped for a link after construction but before the prompt is made
// stays pinned to the original directory: the original file is what is read.
func TestAttachments_RootSwappedBeforeTheCheckStaysPinned(t *testing.T) {
	parent := t.TempDir()
	allowed, outside := filepath.Join(parent, "allowed"), filepath.Join(parent, "outside")
	for _, d := range []string{allowed, outside} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(allowed, "in.txt"), []byte("fine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "in.txt"), []byte("TOP-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg}, WithAttachmentRoots(allowed))
	if err := os.Rename(allowed, allowed+".bak"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, allowed); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	go answerWith(t, tg, 0, "ok")
	h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "attachments": []string{filepath.Join(allowed, "in.txt")}})
	tg.mu.Lock()
	defer tg.mu.Unlock()
	for _, env := range tg.envs {
		for _, a := range env.Attachments {
			if b, _ := a.Bytes(); strings.Contains(string(b), "TOP-SECRET") {
				t.Fatalf("the outside secret was sent: %q", b)
			}
		}
	}
}

// A prompt paused right before publication while its scope closes is not
// published, and the call says the scope ended.
func TestPrompt_NotPublishedAfterItsScopeClosed(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	paused, release := make(chan *scope, 1), make(chan struct{})
	h.tl.beforePublish = func(sc *scope) {
		paused <- sc
		<-release
	}
	var res obj
	err := h.tl.RunInPromptScope(context.Background(), "", func(ctx context.Context) error {
		done := make(chan struct{})
		go func() {
			defer close(done)
			res = h.run(ctx, ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "wait": false})
		}()
		sc := <-paused
		h.tl.closeScope(context.Background(), sc)
		close(release)
		<-done
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := str(res, "error"); got != scopeEnded {
		t.Fatalf("want the scope-ended error, got %v", res)
	}
	tg.mu.Lock()
	defer tg.mu.Unlock()
	if n := len(tg.envs); n != 0 {
		t.Fatalf("%d prompt(s) published after the scope closed", n)
	}
}
