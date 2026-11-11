// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/nats-io/nats.go"
)

const fixtureDir = "testdata/agent-tools"

// --- fixtures: definitions ------------------------------------------------------

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFixtures_DefinitionsMatch(t *testing.T) {
	defs := ToolDefinitions()
	if got := len(defs); got != 6 {
		t.Fatalf("%d definitions, want 6", got)
	}
	for i, name := range ToolNames() {
		t.Run(name, func(t *testing.T) {
			fixture := readFixture(t, name+".json")
			// The embedded copy is the fixture, byte for byte.
			emb, err := definitionFiles.ReadFile("definitions/" + name + ".json")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(emb, fixture) {
				t.Errorf("definitions/%s.json differs from the vendored fixture", name)
			}
			var want, got any
			if err := json.Unmarshal(fixture, &want); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(defs[i])
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(want, got) {
				t.Errorf("ToolDefinitions()[%d] differs from fixture %s.json:\n got %s\nwant %s", i, name, raw, fixture)
			}
			if defs[i].Name != name {
				t.Errorf("definition %d is %q, want %q", i, defs[i].Name, name)
			}
		})
	}
}

func TestFixtures_BlockingDescriptionsMatch(t *testing.T) {
	fixture := readFixture(t, "blocking.json")
	emb, err := definitionFiles.ReadFile("definitions/blocking.json")
	if err != nil || !bytes.Equal(emb, fixture) {
		t.Fatalf("embedded blocking.json differs from the fixture (err %v)", err)
	}
	var want map[string]string
	if err := json.Unmarshal(fixture, &want); err != nil {
		t.Fatal(err)
	}
	l, err := loadDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, l.blocking) {
		t.Errorf("loaded blocking descriptions differ: %v", l.blocking)
	}
	// Only a description that mentions wait_agent has one.
	for _, d := range ToolDefinitions() {
		_, has := want[d.Name]
		if mentions := strings.Contains(d.Description, "wait_agent"); mentions != has {
			t.Errorf("%s: mentions wait_agent = %v but blocking entry = %v", d.Name, mentions, has)
		}
	}
}

func TestDefinitions_BlockingOnlyDerivation(t *testing.T) {
	tl := mustNew(t, nil, WithTools(BlockingTools()...))
	got := tl.Definitions()
	var names []string
	for _, d := range got {
		names = append(names, d.Name)
	}
	if !reflect.DeepEqual(names, BlockingTools()) {
		t.Fatalf("offered %v, want %v", names, BlockingTools())
	}
	var blocking map[string]string
	if err := json.Unmarshal(readFixture(t, "blocking.json"), &blocking); err != nil {
		t.Fatal(err)
	}
	full := map[string]Definition{}
	for _, d := range ToolDefinitions() {
		full[d.Name] = d
	}
	for _, d := range got {
		orig := full[d.Name]
		want := orig.Description
		if b, ok := blocking[d.Name]; ok {
			want = b
		}
		if d.Description != want {
			t.Errorf("%s: description is not the blocking one", d.Name)
		}
		var gp, op map[string]any
		_ = json.Unmarshal(d.Parameters, &gp)
		_ = json.Unmarshal(orig.Parameters, &op)
		delete(op["properties"].(map[string]any), "wait")
		if !reflect.DeepEqual(gp, op) {
			t.Errorf("%s: parameters are not the original minus wait:\n%s", d.Name, d.Parameters)
		}
	}
	// The remaining properties keep the fixture's order.
	if i, j := bytes.Index(got[1].Parameters, []byte(`"address"`)), bytes.Index(got[1].Parameters, []byte(`"label"`)); i < 0 || j < i {
		t.Errorf("property order changed: %s", got[1].Parameters)
	}
	if bytes.Contains(got[1].Parameters, []byte(`"wait"`)) {
		t.Error("prompt_agent still has wait")
	}
	// A copy: changing one helper's definitions touches no other.
	got[0].Name = "changed"
	if tl.Definitions()[0].Name != ToolDiscoverAgents || ToolDefinitions()[0].Name != ToolDiscoverAgents {
		t.Error("Definitions returned shared state")
	}
}

func TestDefinitions_AllSixKeepWait(t *testing.T) {
	tl := mustNew(t, nil)
	defs := tl.Definitions()
	if len(defs) != 6 {
		t.Fatalf("%d definitions", len(defs))
	}
	if !bytes.Contains(defs[1].Parameters, []byte(`"wait"`)) || !bytes.Contains(defs[3].Parameters, []byte(`"wait"`)) {
		t.Error("prompt_agent and answer_agent must keep wait when wait_agent is offered")
	}
}

func TestToolNames_AndBlockingSet(t *testing.T) {
	want := []string{"discover_agents", "prompt_agent", "wait_agent", "answer_agent", "cancel_agent", "list_agent_calls"}
	if !reflect.DeepEqual(ToolNames(), want) {
		t.Errorf("ToolNames = %v", ToolNames())
	}
	if !reflect.DeepEqual(BlockingTools(), []string{"discover_agents", "prompt_agent", "answer_agent"}) {
		t.Errorf("BlockingTools = %v", BlockingTools())
	}
	n := ToolNames()
	n[0] = "x"
	if ToolNames()[0] != "discover_agents" {
		t.Error("ToolNames shares state")
	}
}

func TestNew_RefusesNonsensicalToolSets(t *testing.T) {
	fb := &fakeBackend{}
	cases := []struct {
		name  string
		tools []string
		want  string // "" = accepted
	}{
		{"none", []string{}, "names no tool"},
		{"unknown", []string{"prompt_agent", "answer_agent", "teleport"}, `"teleport"`},
		{"wait without prompt", []string{"discover_agents", "wait_agent"}, "without prompt_agent"},
		{"answer without prompt", []string{"answer_agent"}, "without prompt_agent"},
		{"cancel and list without prompt", []string{"cancel_agent", "list_agent_calls"}, "without prompt_agent"},
		{"prompt without answer", []string{"discover_agents", "prompt_agent"}, "without answer_agent"},
		{"discover alone", []string{"discover_agents"}, ""},
		{"blocking", BlockingTools(), ""},
		{"all", ToolNames(), ""},
		{"prompt answer wait", []string{"wait_agent", "prompt_agent", "answer_agent"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tl, err := newTools(fb, WithTools(tc.tools...))
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				_ = tl.Close(context.Background())
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one containing %q", err, tc.want)
			}
		})
	}
	if _, err := New(nil); err == nil {
		t.Error("New(nil) must refuse")
	}
}

func TestNew_OptionValidation(t *testing.T) {
	fb := &fakeBackend{}
	for name, o := range map[string]Option{
		"discover timeout": WithDiscoverTimeout(0),
		"max wait":         WithMaxWait(-1),
		"max wait agent":   WithMaxWaitAgent(-1),
		"max calls":        WithMaxCalls(0),
		"empty root":       WithAttachmentRoots(""),
		"empty staging":    WithStagingDir(""),
		"saved bytes":      WithMaxSavedBytesPerCall(-1),
		"nil extension":    WithExtensions(nil),
	} {
		if _, err := newTools(fb, o); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// --- harness --------------------------------------------------------------------

var resultSchema = map[string]string{
	ToolDiscoverAgents: "discover_agents.result.json",
	ToolPromptAgent:    "prompt_agent.result.json",
	ToolWaitAgent:      "prompt_agent.result.json",
	ToolAnswerAgent:    "prompt_agent.result.json",
	ToolCancelAgent:    "cancel_agent.result.json",
	ToolListAgentCalls: "list_agent_calls.result.json",
}

type harness struct {
	t       *testing.T
	tl      *Tools
	fb      *fakeBackend
	schemas *schemaSet
	staging string
}

func mustNew(t *testing.T, fb *fakeBackend, opts ...Option) *Tools {
	t.Helper()
	if fb == nil {
		fb = &fakeBackend{}
	}
	tl, err := newTools(fb, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tl.Close(context.Background()) })
	return tl
}

func newHarness(t *testing.T, targets []*fakeTarget, opts ...Option) *harness {
	t.Helper()
	checkLeaks(t)
	staging := filepath.Join(t.TempDir(), "staging")
	fb := &fakeBackend{targets: targets}
	tl := mustNew(t, fb, append([]Option{WithStagingDir(staging)}, opts...)...)
	return &harness{t: t, tl: tl, fb: fb, schemas: newSchemaSet(fixtureDir), staging: staging}
}

type obj = map[string]any

// run executes a tool, checks the result against its fixture schema and
// returns it as the generic JSON a model would read.
func (h *harness) run(ctx context.Context, name string, args any, opts ...ExecuteOption) obj {
	h.t.Helper()
	var raw json.RawMessage
	switch a := args.(type) {
	case nil:
	case string:
		raw = json.RawMessage(a)
	default:
		b, err := json.Marshal(a)
		if err != nil {
			h.t.Fatal(err)
		}
		raw = b
	}
	res, err := h.tl.Execute(ctx, name, raw, opts...)
	if err != nil {
		h.t.Fatalf("%s: %v", name, err)
	}
	n, err := normalize(res)
	if err != nil {
		h.t.Fatalf("%s: result is not JSON: %v", name, err)
	}
	obj, _ := n.(map[string]any)
	if schema, ok := resultSchema[name]; ok {
		// A refusal is {"error"} alone for every tool (docs/agent-tools.md section 2),
		// but only prompt_agent.result.json describes it: the other three schemas
		// require their own key. Check a refusal against that one.
		if _, has := obj["error"]; has && len(obj) <= 3 && obj["calls"] == nil && obj["agents"] == nil {
			schema = resultSchema[ToolPromptAgent]
		}
		violations, err := h.schemas.validate(schema, n)
		if err != nil {
			h.t.Fatal(err)
		}
		if len(violations) > 0 {
			h.t.Fatalf("%s result breaks %s: %s\nresult: %v", name, schema, joinErrs(violations), obj)
		}
	}
	return obj
}

func (h *harness) do(name string, args any, opts ...ExecuteOption) obj {
	h.t.Helper()
	return h.run(context.Background(), name, args, opts...)
}

func str(m obj, k string) string { s, _ := m[k].(string); return s }

func wantState(t *testing.T, r obj, state string) {
	t.Helper()
	if str(r, "state") != state {
		t.Fatalf("state %q, want %q: %v", str(r, "state"), state, r)
	}
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func echoTarget() *fakeTarget { return newFakeTarget("echo", "tests", "one", nil) }

// --- discover_agents ----------------------------------------------------------

func TestDiscover_EntriesAndInstances(t *testing.T) {
	md, id := identityMetadata(t)
	a := newFakeTarget("researcher", "ann", "main", md)
	a.inf.Description = "finds things"
	a.trust = "signed"
	a2 := newFakeTarget("researcher", "ann", "main", md) // a second instance at the same address
	b := newFakeTarget("coder", "bob", "main", nil)
	b.inf.Prompt.Metadata["attachments_ok"] = "false"
	self := newFakeTarget("me", "me", "main", nil)
	h := newHarness(t, []*fakeTarget{a, a2, b, self}, WithSelfAddress(self.subject), WithDiscoverTimeout(time.Second))

	r := h.do(ToolDiscoverAgents, nil)
	list := r["agents"].([]any)
	if len(list) != 2 {
		t.Fatalf("%d entries, want 2 (one per address, self left out): %v", len(list), list)
	}
	e0, e1 := list[0].(obj), list[1].(obj)
	if str(e0, "address") != a.subject || e0["instances"].(float64) != 2 || str(e0, "identity") != id ||
		e0["identity_verified"] != false || e0["requires_signed_prompts"] != true || e0["accepts_attachments"] != true ||
		str(e0, "agent") != "researcher" || str(e0, "owner") != "ann" || str(e0, "name") != "main" || str(e0, "description") != "finds things" {
		t.Errorf("entry 0: %v", e0)
	}
	if str(e1, "address") != b.subject || e1["identity"] != nil || e1["accepts_attachments"] != false || e1["requires_signed_prompts"] != false {
		t.Errorf("entry 1: %v", e1)
	}
	if got := h.fb.waits[0]; got != time.Second {
		t.Errorf("discover timeout %v not passed on", got)
	}

	// Filters are exact and combined, and reach the caller.
	r = h.do(ToolDiscoverAgents, obj{"agent": "coder", "owner": "bob"})
	if len(r["agents"].([]any)) != 1 {
		t.Errorf("filtered: %v", r)
	}
	if f := h.fb.filters[len(h.fb.filters)-1]; f != (caller.Filter{Agent: "coder", Owner: "bob"}) {
		t.Errorf("filter %+v", f)
	}
	r = h.do(ToolDiscoverAgents, obj{"name": "nobody"})
	if len(r["agents"].([]any)) != 0 {
		t.Errorf("no match: %v", r)
	}
}

func TestDiscover_FailureIsAResult(t *testing.T) {
	h := newHarness(t, nil)
	h.fb.err = errBoom
	r := h.do(ToolDiscoverAgents, nil)
	if !strings.HasPrefix(str(r, "error"), "discovery failed; ") || len(r) != 1 {
		t.Errorf("%v", r)
	}
}

// --- arguments -----------------------------------------------------------------

func TestArgs_Refusals(t *testing.T) {
	h := newHarness(t, []*fakeTarget{echoTarget()})
	cases := []struct {
		tool string
		args any
		want string
	}{
		{ToolDiscoverAgents, `{"agent": 3}`, `discover_agents: "agent" must be a non-empty string when given`},
		{ToolDiscoverAgents, `{"owner": ""}`, `discover_agents: "owner" must be a non-empty string when given`},
		{ToolDiscoverAgents, `{"name": null}`, `"name" must be a non-empty string`},
		{ToolDiscoverAgents, `[1]`, "the arguments must be a JSON object"},
		{ToolDiscoverAgents, `{oops`, "the arguments are not valid JSON"},
		{ToolPromptAgent, `{"prompt":"x"}`, `prompt_agent: "address" is required, as discover_agents returned it`},
		{ToolPromptAgent, `{"address":"a"}`, `prompt_agent: "prompt" is required and must not be empty`},
		{ToolPromptAgent, `{"address":"a","prompt":""}`, `"prompt" is required`},
		{ToolPromptAgent, `{"address":"a","prompt":"x","attachments":"f"}`, `"attachments" must be a list of file paths`},
		{ToolPromptAgent, `{"address":"a","prompt":"x","attachments":[""]}`, `"attachments" must be a list of file paths`},
		{ToolPromptAgent, `{"address":"a","prompt":"x","label":""}`, `"label" must be a non-empty string when given`},
		{ToolPromptAgent, `{"address":"a","prompt":"x","wait":"no"}`, `"wait" must be true or false`},
		{ToolWaitAgent, `{}`, `wait_agent: "call_ids" must be a non-empty list of call_id strings`},
		{ToolWaitAgent, `{"call_ids":[]}`, `"call_ids" must be a non-empty list`},
		{ToolWaitAgent, `{"call_ids":["a",1]}`, `"call_ids" must be a non-empty list`},
		{ToolWaitAgent, `{"call_ids":["a"],"timeout_ms":-1}`, `"timeout_ms" must be a whole number of milliseconds, 0 or more`},
		{ToolWaitAgent, `{"call_ids":["a"],"timeout_ms":1.5}`, `"timeout_ms" must be a whole number`},
		{ToolWaitAgent, `{"call_ids":["a"],"timeout_ms":"5"}`, `"timeout_ms" must be a whole number`},
		{ToolAnswerAgent, `{"answer":"y"}`, `answer_agent: "call_id" is required, as it came with the question`},
		{ToolAnswerAgent, `{"call_id":"c"}`, `answer_agent: "answer" is required and must not be empty`},
		{ToolAnswerAgent, `{"call_id":"c","answer":"y","wait":1}`, `"wait" must be true or false`},
		{ToolCancelAgent, `{"call_ids":"c"}`, `cancel_agent: "call_ids" must be a non-empty list of call_id strings`},
	}
	for _, tc := range cases {
		t.Run(tc.tool+" "+tc.want[:min(len(tc.want), 30)], func(t *testing.T) {
			r := h.do(tc.tool, tc.args)
			if str(r, "error") != tc.want && !strings.Contains(str(r, "error"), tc.want) {
				t.Errorf("error %q, want %q", str(r, "error"), tc.want)
			}
			if _, has := r["call_id"]; has || len(r) != 1 {
				t.Errorf("a refusal is {error} alone: %v", r)
			}
		})
	}
	if len(h.tl.known) != 0 {
		t.Error("a refused call must not have discovered anything")
	}
}

func TestArgs_Forms(t *testing.T) {
	h := newHarness(t, []*fakeTarget{echoTarget()})
	// None, null, empty text and "{}" all count as {}; a double-encoded object is unwrapped.
	for _, a := range []any{nil, "", "null", `{}`, `""`} {
		r := h.do(ToolListAgentCalls, a)
		if len(r["calls"].([]any)) != 0 {
			t.Errorf("%v: %v", a, r)
		}
	}
	r := h.do(ToolDiscoverAgents, `"{\"agent\":\"echo\"}"`)
	if len(r["agents"].([]any)) != 1 {
		t.Errorf("double-encoded args: %v", r)
	}
	// Unknown properties are ignored (a host may add parameters of its own).
	r = h.do(ToolListAgentCalls, obj{"host_param": 1})
	if _, bad := r["error"]; bad {
		t.Errorf("%v", r)
	}
	// Duplicate ids are removed, order kept.
	ids, err := parseCancelArgs(json.RawMessage(`{"call_ids":["b","a","b"]}`))
	if err != nil || !reflect.DeepEqual(ids, []string{"b", "a"}) {
		t.Errorf("%v %v", ids, err)
	}
	w, err := parseWaitArgs(json.RawMessage(`{"call_ids":["a"],"timeout_ms":1e3}`))
	if err != nil || *w.timeoutMS != 1000 {
		t.Errorf("1e3 should count as 1000: %+v %v", w, err)
	}
}

func TestExecute_UnknownAndNotOffered(t *testing.T) {
	h := newHarness(t, []*fakeTarget{echoTarget()}, WithTools(BlockingTools()...))
	r := h.do("teleport", nil)
	if str(r, "error") != `unknown tool "teleport"` {
		t.Errorf("%v", r)
	}
	r = h.do(ToolWaitAgent, obj{"call_ids": []string{"x"}})
	if !strings.Contains(str(r, "error"), `the tool "wait_agent" is not offered; your tools are discover_agents, prompt_agent, answer_agent`) {
		t.Errorf("%v", r)
	}
	r = h.do(ToolPromptAgent, obj{"address": echoTarget().subject, "prompt": "x", "wait": false})
	if !strings.Contains(str(r, "error"), `"wait" cannot be false here`) {
		t.Errorf("%v", r)
	}
	r = h.do(ToolAnswerAgent, obj{"call_id": "c", "answer": "y", "wait": false})
	if !strings.Contains(str(r, "error"), `"wait" cannot be false here`) {
		t.Errorf("%v", r)
	}
}

// --- prompt_agent: blocking -------------------------------------------------------

func answerWith(t *testing.T, tg *fakeTarget, n int, chunks ...string) {
	t.Helper()
	s := tg.stream(t, n)
	for _, c := range chunks {
		s.text(c)
	}
	s.end()
}

func TestPrompt_BlockingCompleted(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	go answerWith(t, tg, 0, "hello ", "world")
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "hi", "label": "greeting"}, WithToolCallID("tc_1"))
	wantState(t, r, "completed")
	if str(r, "reply") != "hello world" || str(r, "label") != "greeting" || !strings.HasPrefix(str(r, "call_id"), "call_") {
		t.Errorf("%v", r)
	}
	if _, open := r["open_calls"]; open {
		t.Errorf("no call is open: %v", r)
	}
	// The label is local and the prompt is exactly what the model wrote.
	if tg.envs[0].Prompt != "hi" || len(tg.envs[0].Attachments) != 0 {
		t.Errorf("envelope %+v", tg.envs[0])
	}
	if id, ok := ToolCallIDFromContext(tg.ctxs[0]); !ok || id != "tc_1" {
		t.Errorf("tool call id %q %v not in the prompt's context", id, ok)
	}
	if tg.maxWaits[0] != caller.DefaultMaxWait {
		t.Errorf("runtime limit %v", tg.maxWaits[0])
	}
}

func TestPrompt_ContextCarriesNoToolCallIDWhenNoneGiven(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	go answerWith(t, tg, 0, "x")
	h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "hi"})
	if _, ok := ToolCallIDFromContext(tg.ctxs[0]); ok {
		t.Error("no tool call id was given")
	}
}

func TestPrompt_UnknownAddressAndLookupFailure(t *testing.T) {
	h := newHarness(t, []*fakeTarget{echoTarget()})
	r := h.do(ToolPromptAgent, obj{"address": "agents.prompt.no.one.here", "prompt": "x"})
	want := `no agent answers at "agents.prompt.no.one.here"; call discover_agents for the current list`
	if str(r, "error") != want || len(r) != 1 {
		t.Errorf("%v", r)
	}
	h.fb.err = errBoom
	r = h.do(ToolPromptAgent, obj{"address": "agents.prompt.no.one.here", "prompt": "x"})
	if !strings.HasPrefix(str(r, "error"), `could not look up "agents.prompt.no.one.here"; `) {
		t.Errorf("%v", r)
	}
}

func TestPrompt_RefusedBeforeSending(t *testing.T) {
	tg := echoTarget()
	tg.promptErr = agents.ErrAttachmentsNotSupported
	h := newHarness(t, []*fakeTarget{tg})
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
	if !strings.HasPrefix(str(r, "error"), "the prompt was not sent: ") || len(r) != 1 {
		t.Errorf("%v", r)
	}
	if got := h.do(ToolListAgentCalls, nil)["calls"].([]any); len(got) != 0 {
		t.Errorf("a refused prompt must not be tracked: %v", got)
	}
}

func TestPrompt_FailureStates(t *testing.T) {
	cases := []struct {
		name    string
		feed    func(s *fakeStream)
		state   string
		errPart string
		partial string
	}{
		{"service error", func(s *fakeStream) { s.fail(&agents.ServiceError{Code: 403, Description: "Forbidden"}) },
			"failed", `answered with an error: 403 Forbidden`, ""},
		{"service error after text", func(s *fakeStream) {
			s.text("half ")
			s.fail(&agents.ServiceError{Code: 500})
		}, "failed", `answered with an error: 500`, "half "},
		{"stalled", func(s *fakeStream) { s.fail(&agents.StreamStalledError{Timeout: 60 * time.Second}) },
			"failed", "sent nothing for 1 minute", ""},
		{"no responders", func(s *fakeStream) { s.fail(nats.ErrNoResponders) },
			"failed", `no agent answers at`, ""},
		{"client closed", func(s *fakeStream) { s.fail(caller.ErrClosed) }, "failed", "the client the call ran on was closed", ""},
		{"slow consumer", func(s *fakeStream) { s.fail(&caller.SlowConsumerError{}) }, "failed", "", ""},
		{"expired", func(s *fakeStream) {
			s.text("so far")
			s.fail(&caller.MaxWaitError{MaxWait: 10 * time.Minute})
		}, "expired", "ran past its runtime limit of 10 minutes", "so far"},
		{"expired seconds", func(s *fakeStream) { s.fail(&caller.MaxWaitError{MaxWait: 90 * time.Second}) },
			"expired", "90 seconds", ""},
		{"ends without terminator", func(s *fakeStream) { s.close() }, "failed", "stream ended before the agent finished", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tg := echoTarget()
			h := newHarness(t, []*fakeTarget{tg})
			go func() { tc.feed(tg.stream(t, 0)) }()
			r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
			wantState(t, r, tc.state)
			if !strings.Contains(str(r, "error"), tc.errPart) || str(r, "error") == "" {
				t.Errorf("error %q, want it to contain %q", str(r, "error"), tc.errPart)
			}
			if str(r, "partial_reply") != tc.partial {
				t.Errorf("partial_reply %q, want %q", str(r, "partial_reply"), tc.partial)
			}
			if _, has := r["reply"]; has {
				t.Errorf("a failed call has no reply: %v", r)
			}
		})
	}
}

func TestPrompt_FailedCallStaysFailed(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	go func() { tg.stream(t, 0).fail(&agents.ServiceError{Code: 401}) }()
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
	id := str(r, "call_id")
	// A finished state is final: the same result every time (wait_agent, cancel_agent).
	w := h.do(ToolWaitAgent, obj{"call_ids": []string{id}})
	delete(w, "remaining")
	if !reflect.DeepEqual(w, r) {
		t.Errorf("wait_agent %v, want %v", w, r)
	}
	c := h.do(ToolCancelAgent, obj{"call_ids": []string{id}})["calls"].([]any)[0]
	if !reflect.DeepEqual(c, any(r)) {
		t.Errorf("cancel_agent %v, want %v", c, r)
	}
	wantState(t, h.do(ToolWaitAgent, obj{"call_ids": []string{id}}), "failed")
}

// --- questions --------------------------------------------------------------------

func TestQuestion_BlockingAnswerFlow(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	q1, q2 := newFakeQuestion("may I write /etc/hosts?"), newFakeQuestion("which file?")
	go func() {
		s := tg.stream(t, 0)
		s.text("working... ")
		s.ask(q1)
		s.ask(q2) // a stream may carry several questions at once (SAP §7.3)
	}()
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
	wantState(t, r, "input_required")
	if str(r, "question") != "may I write /etc/hosts?" {
		t.Fatalf("the oldest question first: %v", r)
	}
	if r["open_calls"].(float64) != 1 || !strings.Contains(str(r, "open_calls_note"), "collect it with wait_agent, or stop it with cancel_agent") {
		t.Errorf("open calls note: %v", r)
	}
	id := str(r, "call_id")

	// Answering one question shows the next.
	r = h.do(ToolAnswerAgent, obj{"call_id": id, "answer": "no"})
	wantState(t, r, "input_required")
	if str(r, "question") != "which file?" {
		t.Fatalf("%v", r)
	}
	if got := q1.got(t); got != "no" {
		t.Errorf("answer sent %q", got)
	}
	go func() {
		// The agent finishes once it has its answer.
		if got := q2.got(t); got != "none" {
			t.Errorf("answer sent %q", got)
		}
		s := tg.stream(t, 0)
		s.text("done")
		s.end()
	}()
	r = h.do(ToolAnswerAgent, obj{"call_id": id, "answer": "none"})
	wantState(t, r, "completed")
	if str(r, "reply") != "working... done" {
		t.Errorf("%v", r)
	}
	// No open question now.
	r = h.do(ToolAnswerAgent, obj{"call_id": id, "answer": "late"})
	if !strings.Contains(str(r, "error"), `has no open question: it is completed; wait_agent returns its result`) {
		t.Errorf("%v", r)
	}
	r = h.do(ToolAnswerAgent, obj{"call_id": "call_nope", "answer": "x"})
	if str(r, "error") != `"call_nope": no such call in your calls; list_agent_calls shows them` {
		t.Errorf("%v", r)
	}
}

func TestQuestion_FilesComeWithTheQuestion(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	q := newFakeQuestion("is this the file?")
	q.atts = []agents.Attachment{{Filename: "../evil/name.txt", Content: base64.StdEncoding.EncodeToString([]byte("data"))}}
	go func() { tg.stream(t, 0).ask(q) }()
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
	wantState(t, r, "input_required")
	files := r["attachments"].([]any)
	f := files[0].(obj)
	p := str(f, "path")
	if len(files) != 1 || f["size_bytes"].(float64) != 4 || !filepath.IsAbs(p) || !strings.HasPrefix(p, h.staging+string(filepath.Separator)+str(r, "call_id")) {
		t.Fatalf("%v", r)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "data" {
		t.Errorf("saved file: %q %v", b, err)
	}
	if strings.Contains(p, "evil") {
		t.Errorf("an unsafe name reached the path: %s", p)
	}
}

func TestQuestion_AnswerFailureKeepsTheQuestion(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	q := newFakeQuestion("q?")
	q.once.Store(true) // every reply fails: already replied
	go func() { tg.stream(t, 0).ask(q) }()
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
	id := str(r, "call_id")
	r = h.do(ToolAnswerAgent, obj{"call_id": id, "answer": "yes"})
	if !strings.HasPrefix(str(r, "error"), "the answer could not be sent; ") {
		t.Fatalf("%v", r)
	}
	// Still input_required, so the model can try again.
	wantState(t, h.do(ToolWaitAgent, obj{"call_ids": []string{id}}), "input_required")
}

func TestQuestion_MootAtTerminator(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	q := newFakeQuestion("q?")
	go func() { s := tg.stream(t, 0); s.text("final"); s.ask(q); s.end() }()
	// The question may be seen first or the stream may end first; either way a
	// stream that ends with its terminator is completed, and the question is
	// never refused (it is moot).
	id := str(h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "wait": false}), "call_id")
	eventually(t, "completed", func() bool {
		return str(h.do(ToolWaitAgent, obj{"call_ids": []string{id}, "timeout_ms": 0}), "state") == "completed"
	})
	q.noReply(t)
}

func TestQuestion_FailureRefusesOpenQuestions(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	q := newFakeQuestion("permission?")
	go func() { s := tg.stream(t, 0); s.ask(q) }()
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
	wantState(t, r, "input_required")
	tg.stream(t, 0).fail(&agents.StreamStalledError{Timeout: time.Minute})
	if got := q.got(t); got != QuestionRefusal || !strings.HasPrefix(got, "no") {
		t.Errorf("refusal %q", got)
	}
	wantState(t, h.do(ToolWaitAgent, obj{"call_ids": []string{str(r, "call_id")}}), "failed")
}

// --- detached calls, wait_agent ------------------------------------------------------

func TestWait_DetachedFirstReadyWithRemaining(t *testing.T) {
	a, b, c := newFakeTarget("a", "o", "n", nil), newFakeTarget("b", "o", "n", nil), newFakeTarget("c", "o", "n", nil)
	h := newHarness(t, []*fakeTarget{a, b, c})
	var ids []string
	for i, tg := range []*fakeTarget{a, b, c} {
		r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "go", "wait": false, "label": string(rune('a' + i))})
		wantState(t, r, "running")
		if i == 2 && r["open_calls"].(float64) != 3 {
			t.Errorf("%v", r)
		}
		if _, has := r["reply"]; has {
			t.Errorf("%v", r)
		}
		ids = append(ids, str(r, "call_id"))
	}
	// A timeout polls: none ready yet.
	r := h.do(ToolWaitAgent, obj{"call_ids": ids, "timeout_ms": 0})
	wantState(t, r, "running")
	if !reflect.DeepEqual(r["call_ids"], toAny(ids)) {
		t.Errorf("%v", r)
	}
	if _, has := r["call_id"]; has {
		t.Errorf("a timeout is not a call's result: %v", r)
	}
	r = h.do(ToolWaitAgent, obj{"call_ids": ids, "timeout_ms": 30})
	wantState(t, r, "running")

	// b finishes first, then c: the earliest is returned first.
	answerWith(t, b, 0, "from b")
	eventually(t, "b done", func() bool {
		return str(h.do(ToolListAgentCalls, nil)["calls"].([]any)[1].(obj), "state") == "completed"
	})
	answerWith(t, c, 0, "from c")
	eventually(t, "c done", func() bool {
		return str(h.do(ToolListAgentCalls, nil)["calls"].([]any)[2].(obj), "state") == "completed"
	})
	r = h.do(ToolWaitAgent, obj{"call_ids": ids})
	wantState(t, r, "completed")
	if str(r, "reply") != "from b" || str(r, "call_id") != ids[1] || str(r, "label") != "b" ||
		!reflect.DeepEqual(r["remaining"], toAny([]string{ids[0]})) {
		t.Errorf("%v", r)
	}
	// Waiting on the finished ones only: remaining is empty.
	r = h.do(ToolWaitAgent, obj{"call_ids": []string{ids[2], ids[1]}})
	if str(r, "call_id") != ids[1] || len(r["remaining"].([]any)) != 0 {
		t.Errorf("%v", r)
	}
	answerWith(t, a, 0, "from a")
	r = h.do(ToolWaitAgent, obj{"call_ids": []string{ids[0]}})
	if str(r, "reply") != "from a" {
		t.Errorf("%v", r)
	}
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, x := range s {
		out[i] = x
	}
	return out
}

func TestWait_UnknownCalls(t *testing.T) {
	h := newHarness(t, []*fakeTarget{echoTarget()})
	r := h.do(ToolWaitAgent, obj{"call_ids": []string{"x", "y"}})
	if str(r, "error") != `"x", "y": no such call in your calls; list_agent_calls shows them` || len(r) != 1 {
		t.Errorf("%v", r)
	}
}

func TestWait_QuestionOrderAndCap(t *testing.T) {
	a, b := newFakeTarget("a", "o", "n", nil), newFakeTarget("b", "o", "n", nil)
	h := newHarness(t, []*fakeTarget{a, b}, WithMaxWaitAgent(40*time.Millisecond))
	ia := str(h.do(ToolPromptAgent, obj{"address": a.subject, "prompt": "x", "wait": false}), "call_id")
	ib := str(h.do(ToolPromptAgent, obj{"address": b.subject, "prompt": "x", "wait": false}), "call_id")
	// The cap shortens a longer wait, and is the default.
	start := time.Now()
	r := h.do(ToolWaitAgent, obj{"call_ids": []string{ia, ib}, "timeout_ms": 60000})
	wantState(t, r, "running")
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the cap was not applied: waited %v", d)
	}
	r = h.do(ToolWaitAgent, obj{"call_ids": []string{ia, ib}})
	wantState(t, r, "running")
	// b asks first, then a: input_required counts as ready, the earliest first.
	qb, qa := newFakeQuestion("from b?"), newFakeQuestion("from a?")
	b.stream(t, 0).ask(qb)
	eventually(t, "b asked", func() bool {
		return str(h.do(ToolWaitAgent, obj{"call_ids": []string{ib}, "timeout_ms": 0}), "state") == "input_required"
	})
	a.stream(t, 0).ask(qa)
	eventually(t, "a asked", func() bool {
		return str(h.do(ToolWaitAgent, obj{"call_ids": []string{ia}, "timeout_ms": 0}), "state") == "input_required"
	})
	r = h.do(ToolWaitAgent, obj{"call_ids": []string{ia, ib}})
	wantState(t, r, "input_required")
	if str(r, "call_id") != ib || str(r, "question") != "from b?" || !reflect.DeepEqual(r["remaining"], toAny([]string{ia})) {
		t.Errorf("%v", r)
	}
}

func TestWait_ContextEndStopsTheWaitNotTheCall(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	id := str(h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "wait": false}), "call_id")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	r := h.run(ctx, ToolWaitAgent, obj{"call_ids": []string{id}})
	wantState(t, r, "running")
	if _, has := r["call_ids"]; !has {
		t.Errorf("%v", r)
	}
	// The call keeps running and can still be collected.
	answerWith(t, tg, 0, "late but whole")
	r = h.do(ToolWaitAgent, obj{"call_ids": []string{id}})
	if str(r, "reply") != "late but whole" {
		t.Errorf("%v", r)
	}
}

func TestAnswer_DetachedReturnsAtOnceUnlessWaitTrue(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	q := newFakeQuestion("q?")
	id := str(h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "wait": false}), "call_id")
	tg.stream(t, 0).ask(q)
	wantState(t, h.do(ToolWaitAgent, obj{"call_ids": []string{id}}), "input_required")
	r := h.do(ToolAnswerAgent, obj{"call_id": id, "answer": "yes"})
	wantState(t, r, "running") // a detached call returns at once
	if q.got(t) != "yes" {
		t.Error("answer lost")
	}
	q2 := newFakeQuestion("again?")
	tg.stream(t, 0).ask(q2)
	wantState(t, h.do(ToolWaitAgent, obj{"call_ids": []string{id}}), "input_required")
	go func() { time.Sleep(20 * time.Millisecond); tg.stream(t, 0).text("ok"); tg.stream(t, 0).end() }()
	r = h.do(ToolAnswerAgent, obj{"call_id": id, "answer": "yes", "wait": true})
	wantState(t, r, "completed")
}

func TestAnswer_BlockingWithWaitFalseReturnsAtOnce(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	q := newFakeQuestion("q?")
	go func() { tg.stream(t, 0).ask(q) }()
	id := str(h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"}), "call_id")
	r := h.do(ToolAnswerAgent, obj{"call_id": id, "answer": "yes", "wait": false})
	wantState(t, r, "running")
}

// --- cancel_agent -----------------------------------------------------------------

func TestCancel_OpenCallKeepsPartialAndRefusesQuestion(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	q := newFakeQuestion("permission?")
	id := str(h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "wait": false, "label": "L"}), "call_id")
	s := tg.stream(t, 0)
	s.text("partial ")
	s.ask(q)
	wantState(t, h.do(ToolWaitAgent, obj{"call_ids": []string{id}}), "input_required")

	r := h.do(ToolCancelAgent, obj{"call_ids": []string{id, "call_nope", id}})
	calls := r["calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("duplicates are removed: %v", r)
	}
	c0, c1 := calls[0].(obj), calls[1].(obj)
	if str(c0, "state") != "cancelled" || str(c0, "partial_reply") != "partial " || str(c0, "label") != "L" { //nolint:misspell // the wire value of the state
		t.Errorf("%v", c0)
	}
	if str(c1, "call_id") != "call_nope" || !strings.Contains(str(c1, "error"), "no such call") {
		t.Errorf("%v", c1)
	}
	if got := q.got(t); got != QuestionRefusal {
		t.Errorf("refusal %q", got)
	}
	// The stream was dropped, and nothing the agent sends later changes the result.
	eventually(t, "stream closed", func() bool {
		select {
		case <-s.closedCh:
			return true
		default:
			return false
		}
	})
	// A canceled call stays canceled and keeps its list entry with ended_at.
	again := h.do(ToolCancelAgent, obj{"call_ids": []string{id}})["calls"].([]any)[0]
	if !reflect.DeepEqual(again, any(c0)) {
		t.Errorf("second cancel %v, want %v", again, c0)
	}
	l := h.do(ToolListAgentCalls, nil)["calls"].([]any)[0].(obj)
	if str(l, "state") != "cancelled" || str(l, "ended_at") == "" || str(l, "label") != "L" { //nolint:misspell // the wire value of the state
		t.Errorf("%v", l)
	}
}

func TestCancel_BlockingPromptAbortedByContext(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		s := tg.stream(t, 0)
		s.text("some")
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	r := h.run(ctx, ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
	wantState(t, r, "cancelled") //nolint:misspell // the wire value of the state
	if str(r, "partial_reply") != "some" {
		t.Errorf("%v", r)
	}
}

func TestCancel_FinishedKeepsItsResult(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	go answerWith(t, tg, 0, "done")
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
	c := h.do(ToolCancelAgent, obj{"call_ids": []string{str(r, "call_id")}})["calls"].([]any)[0]
	if !reflect.DeepEqual(c, any(r)) {
		t.Errorf("%v want %v", c, r)
	}
}

// --- list_agent_calls -----------------------------------------------------------------

func TestList_OrderTimesAndOpenNote(t *testing.T) {
	a, b := newFakeTarget("a", "o", "n", nil), newFakeTarget("b", "o", "n", nil)
	h := newHarness(t, []*fakeTarget{a, b})
	i1 := str(h.do(ToolPromptAgent, obj{"address": a.subject, "prompt": "x", "wait": false}), "call_id")
	i2 := str(h.do(ToolPromptAgent, obj{"address": b.subject, "prompt": "x", "wait": false, "label": "second"}), "call_id")
	r := h.do(ToolListAgentCalls, nil)
	calls := r["calls"].([]any)
	if len(calls) != 2 || str(calls[0].(obj), "call_id") != i1 || str(calls[1].(obj), "call_id") != i2 {
		t.Fatalf("oldest first: %v", r)
	}
	if _, has := calls[0].(obj)["label"]; has {
		t.Errorf("no label was given: %v", calls[0])
	}
	if _, has := calls[0].(obj)["ended_at"]; has {
		t.Errorf("open calls have no ended_at: %v", calls[0])
	}
	if r["open_calls"].(float64) != 2 || !strings.Contains(str(r, "open_calls_note"), "2 calls you started are still open") {
		t.Errorf("%v", r)
	}
	answerWith(t, a, 0, "x")
	eventually(t, "first done", func() bool {
		return str(h.do(ToolListAgentCalls, nil)["calls"].([]any)[0].(obj), "state") == "completed"
	})
	l := h.do(ToolListAgentCalls, nil)
	first := l["calls"].([]any)[0].(obj)
	if str(first, "ended_at") == "" || l["open_calls"].(float64) != 1 || !strings.Contains(str(l, "open_calls_note"), "1 call you started is still open") {
		t.Errorf("%v", l)
	}
	st, _ := time.Parse(time.RFC3339, str(first, "started_at"))
	en, _ := time.Parse(time.RFC3339, str(first, "ended_at"))
	if en.Before(st) || !strings.HasSuffix(str(first, "started_at"), "Z") {
		t.Errorf("times %v %v", st, en)
	}
}

// --- limits -----------------------------------------------------------------------

func TestMaxCalls_DropsFinishedLongestAgoAndRefusesWhenAllOpen(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg}, WithMaxCalls(2))
	start := func() obj {
		return h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "wait": false})
	}
	i1, i2 := str(start(), "call_id"), str(start(), "call_id")
	r := start()
	if want := `all 2 tracked calls are still open; collect some with wait_agent or stop some with cancel_agent before you start another`; str(r, "error") != want {
		t.Fatalf("%v", r)
	}
	// Finish the second first, then the first: the second finished longest ago.
	tg.stream(t, 1).end()
	eventually(t, "second done", func() bool {
		return str(h.do(ToolWaitAgent, obj{"call_ids": []string{i2}, "timeout_ms": 0}), "state") == "completed"
	})
	tg.stream(t, 0).end()
	eventually(t, "first done", func() bool {
		return str(h.do(ToolWaitAgent, obj{"call_ids": []string{i1}, "timeout_ms": 0}), "state") == "completed"
	})
	i3 := str(start(), "call_id")
	ids := map[string]bool{}
	for _, c := range h.do(ToolListAgentCalls, nil)["calls"].([]any) {
		ids[str(c.(obj), "call_id")] = true
	}
	if ids[i2] || !ids[i1] || !ids[i3] {
		t.Errorf("the call that finished longest ago is dropped: %v", ids)
	}
	if r := h.do(ToolWaitAgent, obj{"call_ids": []string{i2}}); !strings.Contains(str(r, "error"), "no such call") {
		t.Errorf("%v", r)
	}
}

func TestMaxCalls_AdviceNamesOnlyOfferedTools(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg}, WithMaxCalls(1), WithTools(BlockingTools()...))
	q := newFakeQuestion("q?")
	go func() { tg.stream(t, 0).ask(q) }()
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
	wantState(t, r, "input_required")
	if want := "1 call you started is still open: answer its questions with answer_agent."; str(r, "open_calls_note") != want {
		t.Errorf("note %q", str(r, "open_calls_note"))
	}
	r = h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "y"})
	if want := `all 1 tracked calls are still open; answer their questions with answer_agent before you start another`; str(r, "error") != want {
		t.Errorf("%q", str(r, "error"))
	}
	r = h.do(ToolAnswerAgent, obj{"call_id": "nope", "answer": "x"})
	if str(r, "error") != `"nope": no such call in your calls` {
		t.Errorf("no list_agent_calls to point to: %q", str(r, "error"))
	}
}

// --- guards -----------------------------------------------------------------------

func TestGuards_OwnAddressAndServedCaller(t *testing.T) {
	md, id := identityMetadata(t)
	boss := newFakeTarget("boss", "o", "main", md)
	other := newFakeTarget("other", "o", "main", nil)
	self := newFakeTarget("me", "o", "main", nil)
	h := newHarness(t, []*fakeTarget{boss, other, self}, WithSelfAddress(self.subject))

	r := h.do(ToolPromptAgent, obj{"address": self.subject, "prompt": "x"})
	if str(r, "error") != `"`+self.subject+`" is your own address; answer the prompt yourself` || len(r) != 1 {
		t.Errorf("%v", r)
	}
	err := h.tl.RunInPromptScope(context.Background(), id, func(ctx context.Context) error {
		r := h.run(ctx, ToolPromptAgent, obj{"address": boss.subject, "prompt": "x"})
		if !strings.Contains(str(r, "error"), "sent the prompt you are answering; answer it rather than prompting it back") || len(r) != 1 {
			t.Errorf("%v", r)
		}
		// Another agent is fine.
		go answerWith(t, other, 0, "ok")
		wantState(t, h.run(ctx, ToolPromptAgent, obj{"address": other.subject, "prompt": "x"}), "completed")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Outside a served prompt the guard does not apply.
	go answerWith(t, boss, 0, "hi")
	wantState(t, h.do(ToolPromptAgent, obj{"address": boss.subject, "prompt": "x"}), "completed")
}

// --- scopes ---------------------------------------------------------------------------

func TestScope_ServedPromptEndCancelsOpenCallsAndRefusesQuestions(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	q := newFakeQuestion("permission?")
	var callID string
	err := h.tl.RunInPromptScope(context.Background(), "", func(ctx context.Context) error {
		r := h.run(ctx, ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "wait": false})
		callID = str(r, "call_id")
		if !strings.Contains(str(r, "open_calls_note"), "Calls end with the prompt you are answering: collect it with wait_agent before you answer.") {
			t.Errorf("served note: %v", r)
		}
		tg.stream(t, 0).text("half")
		tg.stream(t, 0).ask(q)
		wantState(t, h.run(ctx, ToolWaitAgent, obj{"call_ids": []string{callID}}), "input_required")
		// The shared scope does not see the call.
		if got := h.do(ToolListAgentCalls, nil)["calls"].([]any); len(got) != 0 {
			t.Errorf("the root scope sees a served call: %v", got)
		}
		if r := h.do(ToolWaitAgent, obj{"call_ids": []string{callID}}); !strings.Contains(str(r, "error"), "no such call") {
			t.Errorf("%v", r)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := q.got(t); got != QuestionRefusal {
		t.Errorf("refusal %q", got)
	}
	eventually(t, "stream dropped", func() bool {
		select {
		case <-tg.stream(t, 0).closedCh:
			return true
		default:
			return false
		}
	})
}

func TestScope_FnErrorIsReturnedAndScopeStillCloses(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	var ctx2 context.Context
	err := h.tl.RunInPromptScope(context.Background(), "", func(ctx context.Context) error {
		ctx2 = ctx
		h.run(ctx, ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "wait": false})
		return errBoom
	})
	if err != errBoom {
		t.Fatalf("%v", err)
	}
	// After the scope closed, no call can start in it.
	r := h.run(ctx2, ToolPromptAgent, obj{"address": tg.subject, "prompt": "y"})
	if str(r, "error") != scopeEnded {
		t.Errorf("%v", r)
	}
	if len(tg.envs) != 1 {
		t.Errorf("%d prompts sent", len(tg.envs))
	}
}

func TestScope_TwoServedPromptsAreIsolated(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	err := h.tl.RunInPromptScope(context.Background(), "", func(ctx1 context.Context) error {
		id := str(h.run(ctx1, ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "wait": false}), "call_id")
		return h.tl.RunInPromptScope(context.Background(), "", func(ctx2 context.Context) error {
			if got := h.run(ctx2, ToolListAgentCalls, nil)["calls"].([]any); len(got) != 0 {
				t.Errorf("scope 2 sees scope 1's call: %v", got)
			}
			if r := h.run(ctx2, ToolCancelAgent, obj{"call_ids": []string{id}}); !strings.Contains(str(r["calls"].([]any)[0].(obj), "error"), "no such call") {
				t.Errorf("%v", r)
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// --- onSettled -------------------------------------------------------------------------

func TestOnSettled_RootOnlyWithAwaitedFlag(t *testing.T) {
	type settled struct {
		res  CallResult
		info SettledInfo
	}
	got := make(chan settled, 8)
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg}, WithOnSettled(func(r CallResult, i SettledInfo) { got <- settled{r, i} }))
	// Detached, nobody waits: awaited false.
	id := str(h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "wait": false}), "call_id")
	answerWith(t, tg, 0, "one")
	s := <-got
	if s.res.CallID() != id || s.res.State() != StateCompleted || s.info.Awaited || s.res["reply"] != "one" {
		t.Errorf("%+v", s)
	}
	// Blocking: a tool call is waiting as it finishes.
	go answerWith(t, tg, 1, "two")
	h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "y"})
	s = <-got
	if !s.info.Awaited || s.res["reply"] != "two" {
		t.Errorf("%+v", s)
	}
	// Canceling reports too; a served prompt's calls do not.
	id3 := str(h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "z", "wait": false}), "call_id")
	h.do(ToolCancelAgent, obj{"call_ids": []string{id3}})
	if s = <-got; s.res.State() != StateCancelled {
		t.Errorf("%+v", s)
	}
	_ = h.tl.RunInPromptScope(context.Background(), "", func(ctx context.Context) error {
		h.run(ctx, ToolPromptAgent, obj{"address": tg.subject, "prompt": "served", "wait": false})
		return nil
	})
	select {
	case s := <-got:
		t.Errorf("a served call was reported: %+v", s)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestOnSettled_PanicIsRecovered(t *testing.T) {
	tg := echoTarget()
	var n atomic.Int32
	h := newHarness(t, []*fakeTarget{tg}, WithOnSettled(func(CallResult, SettledInfo) { n.Add(1); panic("x") }))
	go answerWith(t, tg, 0, "x")
	wantState(t, h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"}), "completed")
	eventually(t, "callback", func() bool { return n.Load() == 1 })
}

// --- files ------------------------------------------------------------------------------

func TestAttachments_RootsLinksAndSending(t *testing.T) {
	tg := echoTarget()
	allowed := t.TempDir()
	outside := t.TempDir()
	for _, f := range []struct{ dir, name, body string }{{allowed, "in.txt", "inside"}, {outside, "out.txt", "outside"}} {
		if err := os.WriteFile(filepath.Join(f.dir, f.name), []byte(f.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(outside, "out.txt"), filepath.Join(allowed, "escape.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Mkdir(filepath.Join(allowed, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, []*fakeTarget{tg}, WithAttachmentRoots(allowed))
	args := func(p ...string) obj {
		return obj{"address": tg.subject, "prompt": "x", "attachments": p}
	}
	for name, tc := range map[string]struct{ path, want string }{
		"outside":     {filepath.Join(outside, "out.txt"), "is outside the directories you may send files from"},
		"link escape": {filepath.Join(allowed, "escape.txt"), "is outside the directories"},
		"missing":     {filepath.Join(allowed, "nope.txt"), "does not exist"},
		"directory":   {filepath.Join(allowed, "sub"), "is not a file"},
		"dotdot":      {filepath.Join(allowed, "..", filepath.Base(outside), "out.txt"), "is outside"},
	} {
		t.Run(name, func(t *testing.T) {
			r := h.do(ToolPromptAgent, args(tc.path))
			if !strings.Contains(str(r, "error"), tc.want) || len(r) != 1 {
				t.Errorf("%v", r)
			}
		})
	}
	if len(tg.envs) != 0 {
		t.Fatal("a refused attachment sent a prompt")
	}
	go answerWith(t, tg, 0, "got it")
	wantState(t, h.do(ToolPromptAgent, args(filepath.Join(allowed, "in.txt"))), "completed")
	if a := tg.envs[0].Attachments; len(a) != 1 || a[0].Filename != "in.txt" {
		t.Fatalf("attachments %+v", a)
	}
	// A file returned by an agent can be sent on: the staging directory is always a root.
	tg2 := newFakeTarget("sender", "o", "n", nil)
	h.fb.targets = append(h.fb.targets, tg2)
	go func() {
		tg2.stream(t, 0).text("here", agents.Attachment{Filename: "r.bin", Content: base64.StdEncoding.EncodeToString([]byte("bytes"))})
		tg2.stream(t, 0).end()
	}()
	r := h.do(ToolPromptAgent, obj{"address": tg2.subject, "prompt": "send me a file"})
	path := str(r["attachments"].([]any)[0].(obj), "path")
	go answerWith(t, tg, 1, "thanks")
	wantState(t, h.do(ToolPromptAgent, args(path)), "completed")
	if a := tg.envs[1].Attachments; len(a) != 1 || a[0].Filename != "r.bin" {
		t.Errorf("attachments %+v", a)
	}
}

func TestAttachments_NoRootsMeansOnlyReturnedFiles(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	f := filepath.Join(t.TempDir(), "secret.env")
	_ = os.WriteFile(f, []byte("TOKEN=1"), 0o600)
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "attachments": []string{f}})
	if !strings.Contains(str(r, "error"), "is outside the directories you may send files from") {
		t.Errorf("%v", r)
	}
	// The working directory is not a root unless named.
	r = h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "attachments": []string{"tools_test.go"}})
	if !strings.Contains(str(r, "error"), "is outside") {
		t.Errorf("%v", r)
	}
}

func TestAttachments_RelativeRootIsTakenFromTheWorkingDirectory(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg}, WithAttachmentRoots("testdata"))
	go answerWith(t, tg, 0, "ok")
	wantState(t, h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "attachments": []string{"testdata/SOURCE.md"}}), "completed")
}

func TestReturnedFiles_LimitAndInvalidContent(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg}, WithMaxSavedBytesPerCall(10))
	enc := func(n int) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("a"), n)) }
	go func() {
		s := tg.stream(t, 0)
		s.text("files", agents.Attachment{Filename: "a.txt", Content: enc(8)},
			agents.Attachment{Filename: "big.txt", Content: enc(8)},
			agents.Attachment{Filename: "bad.txt", Content: "!!not base64!!"})
		s.text("more", agents.Attachment{Filename: "late.txt", Content: enc(5)})
		s.end()
	}()
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
	wantState(t, r, "completed")
	got := map[string]obj{}
	for _, f := range r["attachments"].([]any) {
		got[str(f.(obj), "filename")] = f.(obj)
	}
	if got["a.txt"]["path"] == nil || got["big.txt"]["path"] != nil || str(got["big.txt"], "skipped") != "over_limit" ||
		str(got["bad.txt"], "skipped") != "invalid_content" || got["bad.txt"]["size_bytes"].(float64) != 0 ||
		str(got["late.txt"], "skipped") != "over_limit" || got["big.txt"]["size_bytes"].(float64) != 8 {
		t.Errorf("%v", got)
	}
}

func TestReturnedFiles_ZeroLimitSavesNothing(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg}, WithMaxSavedBytesPerCall(0))
	go func() {
		s := tg.stream(t, 0)
		s.text("f", agents.Attachment{Filename: "a.txt", Content: base64.StdEncoding.EncodeToString([]byte("abc"))})
		s.end()
	}()
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
	f := r["attachments"].([]any)[0].(obj)
	if f["path"] != nil || str(f, "skipped") != "over_limit" || f["size_bytes"].(float64) != 3 {
		t.Errorf("%v", f)
	}
}

func TestStaging_DefaultIsPrivateAndRemovedOnClose(t *testing.T) {
	checkLeaks(t)
	tg := echoTarget()
	fb := &fakeBackend{targets: []*fakeTarget{tg}}
	tl, err := newTools(fb)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		s := tg.stream(t, 0)
		s.text("f", agents.Attachment{Filename: "a.txt", Content: base64.StdEncoding.EncodeToString([]byte("abc"))})
		s.end()
	}()
	res, err := tl.Execute(context.Background(), ToolPromptAgent, json.RawMessage(`{"address":"`+tg.subject+`","prompt":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	path := *res["attachments"].([]ReturnedFile)[0].Path
	fi, err := os.Stat(filepath.Dir(path))
	if err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("per-call dir: %v %v", fi, err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("file: %v %v", fi, err)
	}
	if err := tl.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("staging not removed: %v", err)
	}
}

func TestStaging_GivenDirectoryIsKept(t *testing.T) {
	tg := echoTarget()
	dir := filepath.Join(t.TempDir(), "kept-staging")
	h := newHarness(t, []*fakeTarget{tg}, WithStagingDir(dir)) // the option given here is the one in force
	go func() {
		s := tg.stream(t, 0)
		s.text("f", agents.Attachment{Filename: "a.txt", Content: base64.StdEncoding.EncodeToString([]byte("abc"))})
		s.end()
	}()
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
	path := str(r["attachments"].([]any)[0].(obj), "path")
	if !strings.HasPrefix(path, dir+string(filepath.Separator)) {
		t.Errorf("the file was saved at %q, not under the configured %q", path, dir)
	}
	if err := h.tl.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a given staging directory is kept: %v", err)
	}
}

// --- Close ------------------------------------------------------------------------------

func TestClose_CancelsOpenCallsRefusesQuestionsAndEndsExecute(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	q := newFakeQuestion("q?")
	h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x", "wait": false})
	tg.stream(t, 0).ask(q)
	eventually(t, "question taken in", func() bool {
		return str(h.do(ToolListAgentCalls, nil)["calls"].([]any)[0].(obj), "state") == "input_required"
	})
	if err := h.tl.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := q.got(t); got != QuestionRefusal {
		t.Errorf("refusal %q", got)
	}
	if _, err := h.tl.Execute(context.Background(), ToolListAgentCalls, nil); err != ErrClosed {
		t.Errorf("Execute after Close: %v", err)
	}
	if err := h.tl.RunInPromptScope(context.Background(), "", func(context.Context) error { return nil }); err != ErrClosed {
		t.Errorf("RunInPromptScope after Close: %v", err)
	}
	if err := h.tl.Close(context.Background()); err != nil {
		t.Errorf("Close is idempotent: %v", err)
	}
}

func TestClose_BlockedPromptReturnsCancelled(t *testing.T) {
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg})
	res := make(chan obj, 1)
	go func() { res <- h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"}) }()
	tg.stream(t, 0)
	_ = h.tl.Close(context.Background())
	select {
	case r := <-res:
		wantState(t, r, "cancelled") //nolint:misspell // the wire value of the state
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked prompt did not return after Close")
	}
}

func TestInterceptor_ClosedHelperDoesNotBlockServing(t *testing.T) {
	h := newHarness(t, nil)
	_ = h.tl.Close(context.Background())
	called := false
	err := h.tl.Interceptor().AroundRequest(context.Background(), nil, func(context.Context) error { called = true; return nil })
	if err != nil || !called {
		t.Errorf("%v %v", err, called)
	}
}

// --- extensions -------------------------------------------------------------------------

type testExt struct {
	name      string
	discovery func(*caller.Agent) (map[string]any, error)
	rewrite   func(*PromptContext) (*PromptRewrite, error)
	look      func(*ReplyContext) (map[string]any, error)
}

func (e *testExt) Name() string { return e.name }

type discExt struct{ *testExt }

func (e discExt) DiscoveryFields(_ context.Context, a *caller.Agent) (map[string]any, error) {
	return e.discovery(a)
}

type rewriteExt struct{ *testExt }

func (e rewriteExt) RewritePrompt(_ context.Context, p *PromptContext) (*PromptRewrite, error) {
	return e.rewrite(p)
}

type lookExt struct{ *testExt }

func (e lookExt) LookReply(_ context.Context, r *ReplyContext) (map[string]any, error) {
	return e.look(r)
}

func TestExtensions_DiscoveryFields(t *testing.T) {
	tg := echoTarget()
	ok := discExt{&testExt{name: "tags", discovery: func(*caller.Agent) (map[string]any, error) { return map[string]any{"region": "eu"}, nil }}}
	h := newHarness(t, []*fakeTarget{tg}, WithExtensions(ok))
	e := h.do(ToolDiscoverAgents, nil)["agents"].([]any)[0].(obj)
	if str(e, "region") != "eu" || str(e, "agent") != "echo" {
		t.Errorf("%v", e)
	}
	bad := discExt{&testExt{name: "greedy", discovery: func(*caller.Agent) (map[string]any, error) { return map[string]any{"address": "x"}, nil }}}
	h = newHarness(t, []*fakeTarget{tg}, WithExtensions(bad))
	r := h.do(ToolDiscoverAgents, nil)
	if !strings.Contains(str(r, "error"), `extension greedy may not set "address"`) || len(r) != 1 {
		t.Errorf("%v", r)
	}
	failing := discExt{&testExt{name: "broken", discovery: func(*caller.Agent) (map[string]any, error) { return nil, errBoom }}}
	h = newHarness(t, []*fakeTarget{tg}, WithExtensions(failing))
	if r := h.do(ToolDiscoverAgents, nil); !strings.Contains(str(r, "error"), "extension broken failed") {
		t.Errorf("%v", r)
	}
}

func TestExtensions_PromptRewrite(t *testing.T) {
	tg := echoTarget()
	var seen PromptContext
	ctxVals := make(chan map[string]any, 1)
	rw := rewriteExt{&testExt{name: "rw", rewrite: func(p *PromptContext) (*PromptRewrite, error) {
		seen = *p
		np := p.Prompt + " [signed]"
		return &PromptRewrite{Prompt: &np, Context: map[string]any{"trace": "t-1"}, Fields: map[string]any{"cost": 3}}, nil
	}}}
	second := rewriteExt{&testExt{name: "rw2", rewrite: func(p *PromptContext) (*PromptRewrite, error) {
		if p.Prompt != "hi [signed]" {
			t.Errorf("extensions run in order, each seeing the last one's text: %q", p.Prompt)
		}
		return nil, nil
	}}}
	h := newHarness(t, []*fakeTarget{tg}, WithExtensions(rw, second))
	go func() { answerWith(t, tg, 0, "ok"); ctxVals <- PromptContextFromContext(tg.ctxs[0]) }()
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "hi", "label": "lbl"}, WithToolCallID("tc"))
	wantState(t, r, "completed")
	if tg.envs[0].Prompt != "hi [signed]" || r["cost"].(float64) != 3 {
		t.Errorf("%v %+v", r, tg.envs[0])
	}
	if seen.Address != tg.subject || seen.Prompt != "hi" || seen.Label != "lbl" || seen.ToolCallID != "tc" || seen.CallID != str(r, "call_id") {
		t.Errorf("%+v", seen)
	}
	if v := <-ctxVals; v["trace"] != "t-1" {
		t.Errorf("%v", v)
	}
	// A refusal and a reserved field.
	h = newHarness(t, []*fakeTarget{tg}, WithExtensions(rewriteExt{&testExt{name: "no", rewrite: func(*PromptContext) (*PromptRewrite, error) {
		return &PromptRewrite{Error: "policy says no"}, nil
	}}}))
	if r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"}); str(r, "error") != "policy says no" || len(r) != 1 {
		t.Errorf("%v", r)
	}
	h = newHarness(t, []*fakeTarget{tg}, WithExtensions(rewriteExt{&testExt{name: "greedy", rewrite: func(*PromptContext) (*PromptRewrite, error) {
		return &PromptRewrite{Fields: map[string]any{"reply": "forged"}}, nil
	}}}))
	if r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"}); !strings.Contains(str(r, "error"), `may not set "reply"`) {
		t.Errorf("%v", r)
	}
	if len(tg.envs) != 1 {
		t.Errorf("a refused rewrite sent a prompt: %d", len(tg.envs))
	}
}

func TestExtensions_ReplyLook(t *testing.T) {
	tg := echoTarget()
	var kinds []string
	look := lookExt{&testExt{name: "look", look: func(r *ReplyContext) (map[string]any, error) {
		kinds = append(kinds, r.Kind+":"+r.Text)
		return map[string]any{"checked_" + r.Kind: true}, nil
	}}}
	h := newHarness(t, []*fakeTarget{tg}, WithExtensions(look))
	q := newFakeQuestion("q?")
	go func() { tg.stream(t, 0).text("a"); tg.stream(t, 0).ask(q) }()
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
	if r["checked_question"] != true {
		t.Fatalf("%v", r)
	}
	go func() { q.got(t); tg.stream(t, 0).text("b"); tg.stream(t, 0).end() }()
	r = h.do(ToolAnswerAgent, obj{"call_id": str(r, "call_id"), "answer": "y"})
	if r["checked_reply"] != true || str(r, "reply") != "ab" {
		t.Errorf("%v", r)
	}
	if !reflect.DeepEqual(kinds, []string{"question:q?", "reply:ab"}) {
		t.Errorf("%v", kinds)
	}
}

func TestExtensions_ReplyLookReservedFieldFailsTheCall(t *testing.T) {
	tg := echoTarget()
	look := lookExt{&testExt{name: "greedy", look: func(*ReplyContext) (map[string]any, error) {
		return map[string]any{"state": "completed"}, nil
	}}}
	h := newHarness(t, []*fakeTarget{tg}, WithExtensions(look))
	go answerWith(t, tg, 0, "x")
	r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
	wantState(t, r, "failed")
	if !strings.Contains(str(r, "error"), "an extension of these tools has a bug") || !strings.Contains(str(r, "error"), "is not at fault") {
		t.Errorf("%v", r)
	}
	// An error from the hook fails the call too, and refuses a question being taken in.
	q := newFakeQuestion("q?")
	failing := lookExt{&testExt{name: "broken", look: func(*ReplyContext) (map[string]any, error) { return nil, errBoom }}}
	h = newHarness(t, []*fakeTarget{tg}, WithExtensions(failing))
	go func() { tg.stream(t, 1).ask(q) }()
	r = h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "x"})
	wantState(t, r, "failed")
	if got := q.got(t); got != QuestionRefusal {
		t.Errorf("refusal %q", got)
	}
}

// --- concurrency -------------------------------------------------------------------------

func TestConcurrent_ManyCallsStayIsolated(t *testing.T) {
	const n = 30
	tg := echoTarget()
	h := newHarness(t, []*fakeTarget{tg}, WithMaxCalls(100))
	results := make(chan string, n)
	for i := 0; i < n; i++ {
		go func() {
			r := h.do(ToolPromptAgent, obj{"address": tg.subject, "prompt": "p"})
			results <- str(r, "reply")
		}()
	}
	// Answer each stream with its own index.
	for i := 0; i < n; i++ {
		s := tg.stream(t, i)
		go func() { s.text(strings.Repeat("x", i+1)); s.end() }()
	}
	seen := map[int]bool{}
	for i := 0; i < n; i++ {
		select {
		case r := <-results:
			seen[len(r)] = true
		case <-time.After(10 * time.Second):
			t.Fatal("timeout")
		}
	}
	if len(seen) != n {
		t.Errorf("%d distinct replies, want %d", len(seen), n)
	}
}

// --- the schema checker itself ------------------------------------------------------------

// A checker that accepts everything would make every fixture test above pass
// vacuously, so it must reject what the fixtures forbid.
func TestSchemaChecker_RejectsWhatTheFixturesForbid(t *testing.T) {
	ss := newSchemaSet(fixtureDir)
	bad := []struct {
		schema string
		doc    string
	}{
		{"prompt_agent.result.json", `{"call_id":"c","state":"completed"}`},                                                                         // completed needs reply
		{"prompt_agent.result.json", `{"call_id":"c","state":"input_required"}`},                                                                    // needs question
		{"prompt_agent.result.json", `{"call_id":"c","state":"failed"}`},                                                                            // needs error
		{"prompt_agent.result.json", `{"call_id":"c","state":"running","reply":"x"}`},                                                               // reply only when completed
		{"prompt_agent.result.json", `{"error":"x","call_id":"c"}`},                                                                                 // a refusal has no call_id
		{"prompt_agent.result.json", `{"call_id":"c","state":"running","open_calls":1}`},                                                            // open_calls needs its note
		{"prompt_agent.result.json", `{"call_id":"c","state":"bogus"}`},                                                                             // not a state
		{"prompt_agent.result.json", `{"call_id":"c","state":"completed","reply":"x","attachments":[{"filename":"a","size_bytes":1,"path":null}]}`}, // null path needs skipped
		{"prompt_agent.result.json", `{"state":"running","call_ids":[]}`},                                                                           // timeout needs ids
		{"list_agent_calls.result.json", `{"calls":[{"call_id":"c","address":"a","state":"running","started_at":"yesterday"}]}`},
		{"list_agent_calls.result.json", `{"calls":[{"call_id":"c","address":"a","state":"running","started_at":"2026-01-01T00:00:00Z","extra":1}]}`},
		{"cancel_agent.result.json", `{"calls":[{"call_id":"c","error":"x","state":"cancelled"}]}`}, //nolint:misspell // the wire value of the state
		{"cancel_agent.result.json", `{"calls":[{"call_id":"c","state":"running","open_calls":1,"open_calls_note":"n"}]}`},
		{"discover_agents.result.json", `{"agents":[{"address":"a"}]}`},
		{"discover_agents.result.json", `{"agents":[{"address":"a","agent":"a","owner":"o","name":"n","description":"","identity":"nope","identity_verified":false,"requires_signed_prompts":false,"accepts_attachments":true,"instances":1}]}`},
		{"discover_agents.result.json", `{"agents":[{"address":"a","agent":"a","owner":"o","name":"n","description":"","identity":null,"identity_verified":false,"requires_signed_prompts":false,"accepts_attachments":true,"instances":0}]}`},
	}
	for i, b := range bad {
		var doc any
		if err := json.Unmarshal([]byte(b.doc), &doc); err != nil {
			t.Fatal(err)
		}
		v, err := ss.validate(b.schema, doc)
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		if len(v) == 0 {
			t.Errorf("%d: %s accepted %s", i, b.schema, b.doc)
		}
	}
	good := []struct{ schema, doc string }{
		{"prompt_agent.result.json", `{"call_id":"c","state":"completed","reply":""}`},
		{"prompt_agent.result.json", `{"error":"x"}`},
		{"prompt_agent.result.json", `{"state":"running","call_ids":["a"]}`},
		{"prompt_agent.result.json", `{"call_id":"c","state":"cancelled","partial_reply":"p","remaining":[]}`},      //nolint:misspell // the wire value of the state
		{"cancel_agent.result.json", `{"calls":[{"call_id":"c","state":"cancelled"},{"call_id":"d","error":"x"}]}`}, //nolint:misspell // the wire value of the state
		{"list_agent_calls.result.json", `{"calls":[]}`},
	}
	for i, g := range good {
		var doc any
		_ = json.Unmarshal([]byte(g.doc), &doc)
		v, err := ss.validate(g.schema, doc)
		if err != nil || len(v) > 0 {
			t.Errorf("good %d: %v %v", i, v, err)
		}
	}
}
