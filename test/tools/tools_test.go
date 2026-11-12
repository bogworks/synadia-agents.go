// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package tools_test drives package tools against Go hosts (packages host and
// agentstest) on an embedded nats-server: discover, prompt, wait, answer a
// question, list and cancel, the way a model would through its tool calls.
package tools_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/agentstest"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/bogworks/synadia-agents.go/tools"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

const fixtureDir = "../../tools/testdata/agent-tools"

type obj = map[string]any

var resultSchema = map[string]string{
	tools.ToolDiscoverAgents: "discover_agents.result.json",
	tools.ToolPromptAgent:    "prompt_agent.result.json",
	tools.ToolWaitAgent:      "prompt_agent.result.json",
	tools.ToolAnswerAgent:    "prompt_agent.result.json",
	tools.ToolCancelAgent:    "cancel_agent.result.json",
	tools.ToolListAgentCalls: "list_agent_calls.result.json",
}

// env is one embedded server with a caller client, torn down in order by
// finish, which then checks that no goroutine was left behind.
type env struct {
	t        *testing.T
	baseline int
	srv      *server.Server
	nc       *nats.Conn
	client   *caller.Client
	schemas  *schemaSet
	closers  []func()
	pre      []func() // run before the closers: release handlers that would block Stop
	once     sync.Once
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, baseline: runtime.NumGoroutine(), schemas: newSchemaSet(fixtureDir)}
	srv, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats-server did not become ready")
	}
	e.srv = srv
	e.nc = e.connect()
	e.client, err = caller.New(e.nc)
	if err != nil {
		t.Fatal(err)
	}
	e.closers = append(e.closers, func() { _ = e.client.Close() })
	t.Cleanup(e.finish)
	return e
}

func (e *env) connect() *nats.Conn {
	e.t.Helper()
	nc, err := nats.Connect(e.srv.ClientURL(), nats.MaxReconnects(0))
	if err != nil {
		e.t.Fatal(err)
	}
	e.closers = append(e.closers, nc.Close)
	return nc
}

// finish runs the teardown in reverse order and fails the test if goroutines
// started during it are still running (stdlib NumGoroutine, with retries).
func (e *env) finish() {
	e.once.Do(func() {
		for _, f := range e.pre {
			f()
		}
		for i := len(e.closers) - 1; i >= 0; i-- {
			e.closers[i]()
		}
		e.srv.Shutdown()
		e.srv.WaitForShutdown()
		var n int
		for i := 0; i < 150; i++ {
			if n = runtime.NumGoroutine(); n <= e.baseline {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		buf := make([]byte, 1<<17)
		e.t.Errorf("goroutine leak: %d before, %d after\n%s", e.baseline, n, buf[:runtime.Stack(buf, true)])
	})
}

func (e *env) newTools(opts ...tools.Option) *tools.Tools {
	e.t.Helper()
	opts = append([]tools.Option{tools.WithStagingDir(filepath.Join(e.t.TempDir(), "staging"))}, opts...)
	tl, err := tools.New(e.client, opts...)
	if err != nil {
		e.t.Fatal(err)
	}
	// Closed before the client and the connection.
	e.closers = append(e.closers, func() { _ = tl.Close() })
	return tl
}

// agent starts a host serving handler at agents.prompt.<agent>.tests.main.
func (e *env) agent(name string, h host.PromptHandler, opts ...host.Option) *agentstest.ReferenceAgent {
	e.t.Helper()
	subj, err := agents.NewAgentSubject(name, "tests", "main")
	if err != nil {
		e.t.Fatal(err)
	}
	ra, err := agentstest.NewReferenceAgent(e.connectNC(), subj, agentstest.WithHandler(h), agentstest.WithHost(opts...))
	if err != nil {
		e.t.Fatal(err)
	}
	if err := ra.Start(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	e.closers = append(e.closers, func() { _ = ra.Stop(context.Background()) })
	return ra
}

func (e *env) connectNC() *nats.Conn { return e.connect() }

// run executes a tool and checks the result against its fixture schema.
func (e *env) run(ctx context.Context, tl *tools.Tools, name string, args any, opts ...tools.ExecuteOption) obj {
	e.t.Helper()
	var raw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			e.t.Fatal(err)
		}
		raw = b
	}
	res, err := tl.Execute(ctx, name, raw, opts...)
	if err != nil {
		e.t.Fatalf("%s: %v", name, err)
	}
	n, err := normalize(res)
	if err != nil {
		e.t.Fatal(err)
	}
	o := n.(map[string]any)
	schema := resultSchema[name]
	if _, has := o["error"]; has && len(o) <= 3 && o["calls"] == nil && o["agents"] == nil {
		schema = resultSchema[tools.ToolPromptAgent] // a refusal is {"error"} alone, whichever tool
	}
	v, err := e.schemas.validate(schema, n)
	if err != nil {
		e.t.Fatal(err)
	}
	if len(v) > 0 {
		e.t.Fatalf("%s result breaks %s: %s\n%v", name, schema, joinErrs(v), o)
	}
	return o
}

func (e *env) do(tl *tools.Tools, name string, args any, opts ...tools.ExecuteOption) obj {
	e.t.Helper()
	return e.run(context.Background(), tl, name, args, opts...)
}

func str(m obj, k string) string { s, _ := m[k].(string); return s }

func state(t *testing.T, r obj, want string) {
	t.Helper()
	if str(r, "state") != want {
		t.Fatalf("state %q, want %q: %v", str(r, "state"), want, r)
	}
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func addr(name string) string { return "agents.prompt." + name + ".tests.main" }

// worker is a host handler scripted by its prompt:
//
//	ask         asks "proceed?" and replies "answer: <answer>"
//	slow        waits for release (or the stream's context), then replies "released"
//	reject      refuses with 403
//	files       replies with one attachment
//	echo files  replies with the names of the attachments it was sent
//	anything    echoes
type worker struct {
	release chan struct{}
	answers chan string // what each Ask got back
	started chan string // the prompt of every request, as it starts
	stop    func()      // releases the slow handlers; safe to call twice
}

func (e *env) newWorker() *worker {
	w := &worker{release: make(chan struct{}), answers: make(chan string, 16), started: make(chan string, 64)}
	var once sync.Once
	w.stop = func() { once.Do(func() { close(w.release) }) }
	e.pre = append(e.pre, w.stop)
	return w
}

func (w *worker) handle(ctx context.Context, req *host.Request, out *host.Responder) error {
	p := req.Envelope.Prompt
	w.started <- p
	switch p {
	case "ask":
		a, err := out.Ask(ctx, "proceed?", 10*time.Second)
		if err != nil {
			w.answers <- "ERR " + err.Error()
			return err
		}
		w.answers <- a.Prompt
		return out.Send(ctx, "answer: "+a.Prompt)
	case "slow":
		select {
		case <-w.release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return out.Send(ctx, "released")
	case "reject":
		return host.Reject(403, "Forbidden")
	case "files":
		return out.SendAttachments(ctx, "here is a file", agents.Attachment{
			Filename: "report.txt", Content: base64.StdEncoding.EncodeToString([]byte("report body")),
		})
	case "echo files":
		var names []string
		for _, a := range req.Envelope.Attachments {
			names = append(names, a.Filename)
		}
		return out.Send(ctx, "files: "+strings.Join(names, ","))
	}
	return out.Send(ctx, p)
}

func (w *worker) wait(t *testing.T, prompt string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case p := <-w.started:
			if p == prompt {
				return
			}
		case <-deadline:
			t.Fatalf("the host never started %q", prompt)
		}
	}
}

func (w *worker) answer(t *testing.T) string {
	t.Helper()
	select {
	case a := <-w.answers:
		return a
	case <-time.After(10 * time.Second):
		t.Fatal("the host's question was never settled")
		return ""
	}
}

// --- the flow -----------------------------------------------------------------------

// TestTools_DiscoverPromptWaitAnswerListCancel is the whole tool surface, in
// the order a model uses it, against a real host.
func TestTools_DiscoverPromptWaitAnswerListCancel(t *testing.T) {
	e := newEnv(t)
	w := e.newWorker()
	e.agent("worker", w.handle)
	tl := e.newTools()

	// discover_agents
	d := e.do(tl, tools.ToolDiscoverAgents, nil)
	list := d["agents"].([]any)
	if len(list) != 1 {
		t.Fatalf("%v", d)
	}
	entry := list[0].(obj)
	if str(entry, "address") != addr("worker") || str(entry, "agent") != "worker" || str(entry, "owner") != "tests" ||
		str(entry, "name") != "main" || entry["instances"].(float64) != 1 || entry["accepts_attachments"] != true ||
		entry["requires_signed_prompts"] != false || entry["identity_verified"] != false || entry["identity"] != nil {
		t.Errorf("entry %v", entry)
	}
	if r := e.do(tl, tools.ToolDiscoverAgents, obj{"agent": "nobody"}); len(r["agents"].([]any)) != 0 {
		t.Errorf("filter: %v", r)
	}

	// prompt_agent, blocking
	r := e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "hello", "label": "greeting"})
	state(t, r, "completed")
	if str(r, "reply") != "hello" || str(r, "label") != "greeting" {
		t.Errorf("%v", r)
	}

	// A question goes to the model, which answers it.
	r = e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "ask"})
	state(t, r, "input_required")
	if str(r, "question") != "proceed?" || r["open_calls"].(float64) != 1 {
		t.Fatalf("%v", r)
	}
	askID := str(r, "call_id")
	r = e.do(tl, tools.ToolAnswerAgent, obj{"call_id": askID, "answer": "yes, go on"})
	state(t, r, "completed")
	if str(r, "reply") != "answer: yes, go on" {
		t.Errorf("%v", r)
	}
	if got := w.answer(t); got != "yes, go on" {
		t.Errorf("the host got %q", got)
	}

	// Several prompts at once, collected with wait_agent.
	a := str(e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "slow", "wait": false, "label": "slow one"}), "call_id")
	b := str(e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "second", "wait": false}), "call_id")
	c := str(e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "slow", "wait": false}), "call_id")
	r = e.do(tl, tools.ToolWaitAgent, obj{"call_ids": []string{a, b, c}})
	state(t, r, "completed")
	if str(r, "call_id") != b || str(r, "reply") != "second" || len(r["remaining"].([]any)) != 2 {
		t.Errorf("the quick one is ready first: %v", r)
	}
	if r["open_calls"].(float64) != 2 {
		t.Errorf("%v", r)
	}
	r = e.do(tl, tools.ToolWaitAgent, obj{"call_ids": []string{a, c}, "timeout_ms": 50})
	state(t, r, "running")

	// list_agent_calls: oldest first, each with its state.
	l := e.do(tl, tools.ToolListAgentCalls, nil)["calls"].([]any)
	var states []string
	for _, x := range l {
		states = append(states, str(x.(obj), "state"))
	}
	if strings.Join(states, ",") != "completed,completed,running,completed,running" {
		t.Errorf("%v", states)
	}
	if str(l[2].(obj), "label") != "slow one" || str(l[2].(obj), "address") != addr("worker") {
		t.Errorf("%v", l[2])
	}

	// cancel_agent stops a call; the host is not told, and works on.
	r = e.do(tl, tools.ToolCancelAgent, obj{"call_ids": []string{a, "call_unknown"}})
	cs := r["calls"].([]any)
	if str(cs[0].(obj), "state") != "cancelled" || !strings.Contains(str(cs[1].(obj), "error"), "no such call") { //nolint:misspell // the wire value of the state
		t.Errorf("%v", r)
	}
	if r["open_calls"].(float64) != 1 {
		t.Errorf("one call is still open: %v", r)
	}
	// Releasing the host now: the canceled call's reply is discarded, the other one arrives.
	w.stop()
	r = e.do(tl, tools.ToolWaitAgent, obj{"call_ids": []string{c}})
	state(t, r, "completed")
	if str(r, "reply") != "released" {
		t.Errorf("%v", r)
	}
	if got := e.do(tl, tools.ToolWaitAgent, obj{"call_ids": []string{a}}); str(got, "state") != "cancelled" { //nolint:misspell // the wire value of the state
		t.Errorf("a canceled call stays canceled: %v", got)
	}
	if _, has := e.do(tl, tools.ToolListAgentCalls, nil)["open_calls"]; has {
		t.Error("no call is open now")
	}
	if err := tl.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTools_CancelRefusesTheOpenQuestion(t *testing.T) {
	e := newEnv(t)
	w := e.newWorker()
	e.agent("worker", w.handle)
	tl := e.newTools()
	id := str(e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "ask", "wait": false}), "call_id")
	r := e.do(tl, tools.ToolWaitAgent, obj{"call_ids": []string{id}})
	state(t, r, "input_required")
	r = e.do(tl, tools.ToolCancelAgent, obj{"call_ids": []string{id}})
	if str(r["calls"].([]any)[0].(obj), "state") != "cancelled" { //nolint:misspell // the wire value of the state
		t.Fatalf("%v", r)
	}
	// The refusal reached the asking host without the caller's subscription.
	if got := w.answer(t); got != tools.QuestionRefusal || !strings.HasPrefix(got, "no") {
		t.Errorf("the host got %q", got)
	}
}

func TestTools_FailedAndRefusedCalls(t *testing.T) {
	e := newEnv(t)
	w := e.newWorker()
	e.agent("worker", w.handle)
	tl := e.newTools()
	r := e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "reject"})
	state(t, r, "failed")
	if !strings.Contains(str(r, "error"), "answered with an error: 403") {
		t.Errorf("%v", r)
	}
	r = e.do(tl, tools.ToolPromptAgent, obj{"address": addr("ghost"), "prompt": "x"})
	if !strings.Contains(str(r, "error"), `no agent answers at "`+addr("ghost")+`"`) || len(r) > 3 {
		t.Errorf("%v", r)
	}
	r = e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker")})
	if !strings.Contains(str(r, "error"), `"prompt" is required`) {
		t.Errorf("%v", r)
	}
}

func TestTools_AgentGoneBecomesFailed(t *testing.T) {
	e := newEnv(t)
	w := e.newWorker()
	ra := e.agent("worker", w.handle)
	tl := e.newTools()
	state(t, e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "x"}), "completed")
	if err := ra.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "x"})
	state(t, r, "failed")
	if !strings.Contains(str(r, "error"), "no agent answers at") {
		t.Errorf("%v", r)
	}
}

func TestTools_ExpiredAtTheRuntimeLimit(t *testing.T) {
	e := newEnv(t)
	w := e.newWorker()
	e.agent("worker", w.handle)
	tl := e.newTools(tools.WithMaxWait(400 * time.Millisecond))
	r := e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "slow"})
	state(t, r, "expired")
	if !strings.Contains(str(r, "error"), "ran past its runtime limit of 400 ms") {
		t.Errorf("%v", r)
	}
}

func TestTools_FilesTravelBothWays(t *testing.T) {
	e := newEnv(t)
	w := e.newWorker()
	e.agent("worker", w.handle)
	root := t.TempDir()
	src := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(src, []byte("notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	tl := e.newTools(tools.WithAttachmentRoots(root))

	r := e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "echo files", "attachments": []string{src}})
	state(t, r, "completed")
	if str(r, "reply") != "files: notes.txt" {
		t.Errorf("%v", r)
	}
	r = e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "files"})
	state(t, r, "completed")
	f := r["attachments"].([]any)[0].(obj)
	b, err := os.ReadFile(str(f, "path"))
	if err != nil || string(b) != "report body" || f["size_bytes"].(float64) != 11 || str(f, "filename") != "report.txt" {
		t.Errorf("%v %q %v", f, b, err)
	}
	// Outside the roots is refused, and nothing is sent.
	other := filepath.Join(t.TempDir(), "x.txt")
	_ = os.WriteFile(other, []byte("x"), 0o600)
	r = e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "echo files", "attachments": []string{other}})
	if !strings.Contains(str(r, "error"), "is outside the directories you may send files from") {
		t.Errorf("%v", r)
	}
}

func TestTools_ManyConcurrentCallsStayIsolated(t *testing.T) {
	e := newEnv(t)
	w := e.newWorker()
	e.agent("worker", w.handle)
	tl := e.newTools(tools.WithMaxCalls(64))
	const n = 24
	ids := make([]string, n)
	for i := range ids {
		ids[i] = str(e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "job " + string(rune('a'+i)), "wait": false}), "call_id")
	}
	for i := range ids {
		r := e.do(tl, tools.ToolWaitAgent, obj{"call_ids": []string{ids[i]}})
		if str(r, "reply") != "job "+string(rune('a'+i)) {
			t.Errorf("call %d got %q", i, str(r, "reply"))
		}
	}
	if st := e.client.Stats(); st.DroppedReplies != 0 || st.ActiveRoutes != 0 {
		t.Errorf("stats %+v", st)
	}
}

func TestTools_SelfAddressLeftOutAndRefused(t *testing.T) {
	e := newEnv(t)
	w := e.newWorker()
	e.agent("worker", w.handle)
	e.agent("other", e.newWorker().handle)
	tl := e.newTools(tools.WithSelfAddress(addr("worker")))
	d := e.do(tl, tools.ToolDiscoverAgents, nil)["agents"].([]any)
	if len(d) != 1 || str(d[0].(obj), "address") != addr("other") {
		t.Errorf("%v", d)
	}
	r := e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "x"})
	if !strings.Contains(str(r, "error"), "is your own address") {
		t.Errorf("%v", r)
	}
}

func TestTools_BlockingToolsOnly(t *testing.T) {
	e := newEnv(t)
	w := e.newWorker()
	e.agent("worker", w.handle)
	tl := e.newTools(tools.WithTools(tools.BlockingTools()...))
	if got := len(tl.Definitions()); got != 3 {
		t.Fatalf("%d definitions", got)
	}
	r := e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "ask"})
	state(t, r, "input_required")
	if !strings.Contains(str(r, "open_calls_note"), "answer its questions with answer_agent") {
		t.Errorf("%v", r)
	}
	state(t, e.do(tl, tools.ToolAnswerAgent, obj{"call_id": str(r, "call_id"), "answer": "ok"}), "completed")
	if r := e.do(tl, tools.ToolListAgentCalls, nil); !strings.Contains(str(r, "error"), "is not offered") {
		t.Errorf("%v", r)
	}
}

func TestTools_BlockingPromptAbortedByContextCancelsTheCall(t *testing.T) {
	e := newEnv(t)
	w := e.newWorker()
	e.agent("worker", w.handle)
	tl := e.newTools()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan obj, 1)
	go func() {
		done <- e.run(ctx, tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "slow"})
	}()
	w.wait(t, "slow")
	cancel()
	select {
	case r := <-done:
		state(t, r, "cancelled") //nolint:misspell // the wire value of the state
	case <-time.After(5 * time.Second):
		t.Fatal("the aborted prompt did not return")
	}
	w.stop()
}

// --- scope: a call belongs to the prompt being served ----------------------------

// TestTools_ServedPromptScope runs the tools inside a host's handler, the
// agent-as-caller shape: when the handler returns, its open calls are
// canceled and the question one of them asked is refused.
func TestTools_ServedPromptScope(t *testing.T) {
	e := newEnv(t)
	w := e.newWorker()
	e.agent("worker", w.handle)

	// The boss uses its own caller client and its own tools helper.
	bossNC := e.connect()
	bossClient, err := caller.New(bossNC)
	if err != nil {
		t.Fatal(err)
	}
	e.closers = append(e.closers, func() { _ = bossClient.Close() })
	bossTools, err := tools.New(bossClient, tools.WithStagingDir(filepath.Join(t.TempDir(), "s")), tools.WithSelfAddress(addr("boss")))
	if err != nil {
		t.Fatal(err)
	}
	e.closers = append(e.closers, func() { _ = bossTools.Close() })

	type outcome struct{ note, open string }
	got := make(chan outcome, 1)
	e.agent("boss", func(ctx context.Context, _ *host.Request, out *host.Responder) error {
		// The model's turn: start a call, let it reach its question, then answer
		// the caller without collecting it.
		r := e.run(ctx, bossTools, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "ask", "wait": false})
		id := str(r, "call_id")
		r = e.run(ctx, bossTools, tools.ToolWaitAgent, obj{"call_ids": []string{id}})
		if str(r, "state") != "input_required" {
			t.Errorf("%v", r)
		}
		got <- outcome{note: str(r, "open_calls_note"), open: "1"}
		return out.Send(ctx, "answered without collecting")
	}, host.WithInterceptors(bossTools.Interceptor()))

	// An outside caller prompts the boss.
	outside := e.newTools()
	r := e.do(outside, tools.ToolPromptAgent, obj{"address": addr("boss"), "prompt": "do the thing"})
	state(t, r, "completed")
	if str(r, "reply") != "answered without collecting" {
		t.Errorf("%v", r)
	}
	o := <-got
	if !strings.Contains(o.note, "Calls end with the prompt you are answering") {
		t.Errorf("served note: %q", o.note)
	}
	// The worker's question was refused when the boss's prompt ended.
	if a := w.answer(t); a != tools.QuestionRefusal {
		t.Errorf("the worker got %q", a)
	}
	// The boss's shared scope never saw the call.
	if l := e.do(bossTools, tools.ToolListAgentCalls, nil)["calls"].([]any); len(l) != 0 {
		t.Errorf("%v", l)
	}
}

// TestTools_ToolCallIDReachesPromptInterceptors: the model's tool-call id is in
// the context the client's interceptors see, and is never sent.
func TestTools_ToolCallIDReachesPromptInterceptors(t *testing.T) {
	e := newEnv(t)
	w := e.newWorker()
	var sawHeader sync.Map
	e.agent("worker", func(ctx context.Context, req *host.Request, out *host.Responder) error {
		for k, v := range req.Header {
			sawHeader.Store(k, v)
		}
		if strings.Contains(string(mustJSON(req.Envelope.Extra)), "tc_77") {
			t.Error("the tool call id went on the wire")
		}
		return w.handle(ctx, req, out)
	})
	ids := make(chan string, 4)
	c2, err := caller.New(e.connect(), caller.WithInterceptors(caller.InterceptorFunc(
		func(ctx context.Context, _ *caller.PromptInfo) (*caller.Extras, error) {
			id, _ := tools.ToolCallIDFromContext(ctx)
			ids <- id
			return nil, nil
		})))
	if err != nil {
		t.Fatal(err)
	}
	e.closers = append(e.closers, func() { _ = c2.Close() })
	tl, err := tools.New(c2, tools.WithStagingDir(filepath.Join(t.TempDir(), "s")))
	if err != nil {
		t.Fatal(err)
	}
	e.closers = append(e.closers, func() { _ = tl.Close() })
	state(t, e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "hi"}, tools.WithToolCallID("tc_77")), "completed")
	if id := <-ids; id != "tc_77" {
		t.Errorf("interceptor saw tool call id %q", id)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// TestTools_CloseStopsEverything: Close cancels open calls, ends blocked tool
// calls, and leaves no goroutine behind (checked by the env's teardown).
func TestTools_CloseStopsEverything(t *testing.T) {
	e := newEnv(t)
	w := e.newWorker()
	e.agent("worker", w.handle)
	tl := e.newTools()
	for i := 0; i < 5; i++ {
		e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "slow", "wait": false})
	}
	blocked := make(chan obj, 1)
	go func() { blocked <- e.do(tl, tools.ToolPromptAgent, obj{"address": addr("worker"), "prompt": "ask"}) }()
	w.wait(t, "ask")
	eventually(t, "the question to arrive", func() bool {
		l := e.do(tl, tools.ToolListAgentCalls, nil)["calls"].([]any)
		return len(l) == 6 && str(l[5].(obj), "state") == "input_required"
	})
	// ... which the blocking prompt returns as it finishes waiting.
	r := <-blocked
	state(t, r, "input_required")
	if err := tl.Close(); err != nil {
		t.Fatal(err)
	}
	if got := w.answer(t); got != tools.QuestionRefusal {
		t.Errorf("the host got %q", got)
	}
	if _, err := tl.Execute(context.Background(), tools.ToolListAgentCalls, nil); !errors.Is(err, tools.ErrClosed) {
		t.Errorf("%v", err)
	}
	w.stop()
	e.finish()
}

// TestTools_ThreeAgentCycleStopsAtTheMaxDepth runs a ring of three agents, each
// told to hand its prompt on to the next, over real NATS: the depth header the
// caller interceptor stamps reaches the next host's interceptor, and the ring
// stops after DefaultMaxDepth hops.
func TestTools_ThreeAgentCycleStopsAtTheMaxDepth(t *testing.T) {
	e := newEnv(t)
	names := []string{"ring0", "ring1", "ring2"}
	var served atomic.Int32
	var refused atomic.Value
	for i, name := range names {
		next := names[(i+1)%len(names)]
		c, err := caller.New(e.connect(), caller.WithInterceptors(tools.DepthInterceptor()))
		if err != nil {
			t.Fatal(err)
		}
		e.closers = append(e.closers, func() { _ = c.Close() })
		tl, err := tools.New(c, tools.WithStagingDir(filepath.Join(t.TempDir(), "s")), tools.WithSelfAddress(addr(name)))
		if err != nil {
			t.Fatal(err)
		}
		e.closers = append(e.closers, func() { _ = tl.Close() })
		e.agent(name, func(ctx context.Context, _ *host.Request, out *host.Responder) error {
			served.Add(1)
			r := e.run(ctx, tl, tools.ToolPromptAgent, obj{"address": addr(next), "prompt": "hand it on"})
			if msg := str(r, "error"); msg != "" {
				refused.Store(msg)
			}
			return out.Send(ctx, "passed")
		}, host.WithInterceptors(tl.Interceptor()))
	}
	outside := e.newTools()
	state(t, e.do(outside, tools.ToolPromptAgent, obj{"address": addr("ring0"), "prompt": "go"}), "completed")
	if got, want := int(served.Load()), tools.DefaultMaxDepth+1; got != want {
		t.Errorf("%d prompts were served, want %d", got, want)
	}
	if msg, _ := refused.Load().(string); !strings.Contains(msg, "handed on 4 times") {
		t.Errorf("the last hop was told %q", msg)
	}
}
