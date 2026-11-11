// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"encoding/json"
	"math"
)

// Parsing a tool call's arguments as the model produced them: a JSON object,
// or JSON text holding one. A model gets its arguments wrong now and then, so
// every problem comes back as words it can act on, never as a Go error.
// Properties the definitions do not name are ignored: a program may add
// parameters of its own and read them before it hands the call on.

type discoverArgs struct{ agent, owner, name string }

type promptArgs struct {
	address, prompt string
	attachments     []string
	label           *string
	wait            bool
}

type waitArgs struct {
	callIDs   []string
	timeoutMS *int64
}

type answerArgs struct {
	callID, answer string
	wait           *bool
}

// argsError is an argument problem, in words for the model.
type argsError string

func (e argsError) Error() string { return string(e) }

type fields map[string]json.RawMessage

// argsObject returns the arguments as an object: none, null and empty text
// count as {}. A JSON string holding an object (a model's double-encoded
// arguments) is unwrapped once.
func argsObject(tool string, raw json.RawMessage) (fields, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return fields{}, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, argsError(tool + ": the arguments are not valid JSON")
		}
		raw = bytes.TrimSpace([]byte(s))
		if len(raw) == 0 {
			return fields{}, nil
		}
		if !json.Valid(raw) {
			return nil, argsError(tool + ": the arguments are not valid JSON")
		}
	}
	if !json.Valid(raw) {
		return nil, argsError(tool + ": the arguments are not valid JSON")
	}
	if raw[0] != '{' {
		return nil, argsError(tool + ": the arguments must be a JSON object")
	}
	var f fields
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, argsError(tool + ": the arguments must be a JSON object")
	}
	return f, nil
}

func (f fields) str(key string) (s string, present, ok bool) {
	raw, present := f[key]
	if !present {
		return "", false, true
	}
	if err := json.Unmarshal(raw, &s); err != nil || raw[0] != '"' {
		return "", true, false
	}
	return s, true, true
}

func (f fields) boolean(key string) (b bool, present, ok bool) {
	raw, present := f[key]
	if !present {
		return false, false, true
	}
	if err := json.Unmarshal(raw, &b); err != nil || raw[0] == 'n' {
		return false, true, false
	}
	return b, true, true
}

func (f fields) strList(key string) (l []string, present, ok bool) {
	raw, present := f[key]
	if !present {
		return nil, false, true
	}
	if raw[0] != '[' || json.Unmarshal(raw, &l) != nil {
		return nil, true, false
	}
	for _, s := range l {
		if s == "" {
			return nil, true, false
		}
	}
	return l, true, true
}

func parseDiscoverArgs(raw json.RawMessage) (discoverArgs, error) {
	f, err := argsObject(ToolDiscoverAgents, raw)
	if err != nil {
		return discoverArgs{}, err
	}
	var out discoverArgs
	for _, k := range []struct {
		key string
		dst *string
	}{{"agent", &out.agent}, {"owner", &out.owner}, {"name", &out.name}} {
		s, present, ok := f.str(k.key)
		if present && (!ok || s == "") {
			return discoverArgs{}, argsError(`discover_agents: "` + k.key + `" must be a non-empty string when given`)
		}
		*k.dst = s
	}
	return out, nil
}

func parsePromptArgs(raw json.RawMessage) (promptArgs, error) {
	f, err := argsObject(ToolPromptAgent, raw)
	if err != nil {
		return promptArgs{}, err
	}
	var out promptArgs
	s, _, ok := f.str("address")
	if !ok || s == "" {
		return out, argsError(`prompt_agent: "address" is required, as discover_agents returned it`)
	}
	out.address = s
	s, _, ok = f.str("prompt")
	if !ok || s == "" {
		return out, argsError(`prompt_agent: "prompt" is required and must not be empty`)
	}
	out.prompt = s
	l, _, ok := f.strList("attachments")
	if !ok {
		return out, argsError(`prompt_agent: "attachments" must be a list of file paths`)
	}
	out.attachments = l
	if s, present, ok := f.str("label"); present && (!ok || s == "") {
		return out, argsError(`prompt_agent: "label" must be a non-empty string when given`)
	} else if present {
		out.label = &s
	}
	w, present, ok := f.boolean("wait")
	if !ok {
		return out, argsError(`prompt_agent: "wait" must be true or false`)
	}
	out.wait = !present || w
	return out, nil
}

func parseWaitArgs(raw json.RawMessage) (waitArgs, error) {
	f, err := argsObject(ToolWaitAgent, raw)
	if err != nil {
		return waitArgs{}, err
	}
	ids, err := parseCallIDs(ToolWaitAgent, f)
	if err != nil {
		return waitArgs{}, err
	}
	out := waitArgs{callIDs: ids}
	if tm, present := f["timeout_ms"]; present {
		n, ok := wholeNumber(tm)
		if !ok || n < 0 {
			return waitArgs{}, argsError(`wait_agent: "timeout_ms" must be a whole number of milliseconds, 0 or more`)
		}
		out.timeoutMS = &n
	}
	return out, nil
}

// wholeNumber reads a JSON number that is an integer, as JavaScript's
// Number.isInteger does: 1000.0 and 1e3 count.
func wholeNumber(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || raw[0] == '"' || raw[0] == 'n' || raw[0] == 't' || raw[0] == 'f' {
		return 0, false
	}
	var v float64
	if json.Unmarshal(raw, &v) != nil || math.IsInf(v, 0) || math.IsNaN(v) || v != math.Trunc(v) {
		return 0, false
	}
	if v > math.MaxInt64/2 {
		v = math.MaxInt64 / 2
	}
	if v < 0 {
		return -1, true
	}
	return int64(v), true
}

func parseAnswerArgs(raw json.RawMessage) (answerArgs, error) {
	f, err := argsObject(ToolAnswerAgent, raw)
	if err != nil {
		return answerArgs{}, err
	}
	var out answerArgs
	s, _, ok := f.str("call_id")
	if !ok || s == "" {
		return out, argsError(`answer_agent: "call_id" is required, as it came with the question`)
	}
	out.callID = s
	s, _, ok = f.str("answer")
	if !ok || s == "" {
		return out, argsError(`answer_agent: "answer" is required and must not be empty`)
	}
	out.answer = s
	w, present, ok := f.boolean("wait")
	if !ok {
		return out, argsError(`answer_agent: "wait" must be true or false`)
	}
	if present {
		out.wait = &w
	}
	return out, nil
}

func parseCancelArgs(raw json.RawMessage) ([]string, error) {
	f, err := argsObject(ToolCancelAgent, raw)
	if err != nil {
		return nil, err
	}
	return parseCallIDs(ToolCancelAgent, f)
}

// parseCallIDs reads call_ids: a non-empty list of non-empty strings,
// duplicates removed, order kept.
func parseCallIDs(tool string, f fields) ([]string, error) {
	l, present, ok := f.strList("call_ids")
	if !present || !ok || len(l) == 0 {
		return nil, argsError(tool + `: "call_ids" must be a non-empty list of call_id strings`)
	}
	seen := make(map[string]bool, len(l))
	out := l[:0:0]
	for _, id := range l {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}
