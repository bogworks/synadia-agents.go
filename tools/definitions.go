// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// The six tool names, in the order the contract lists them. See
// docs/agent-tools.md section 1 of the official SDK repository.
const (
	// ToolDiscoverAgents lists the agents that can be prompted, one entry per address.
	ToolDiscoverAgents = "discover_agents"
	// ToolPromptAgent sends one prompt and waits for the reply or a question, or returns at once.
	ToolPromptAgent = "prompt_agent"
	// ToolWaitAgent waits for the first of some calls that finished or asked a question.
	ToolWaitAgent = "wait_agent"
	// ToolAnswerAgent answers the open question of a call.
	ToolAnswerAgent = "answer_agent"
	// ToolCancelAgent stops calls: it drops their streams and refuses their questions.
	ToolCancelAgent = "cancel_agent"
	// ToolListAgentCalls lists the calls tracked in the current scope.
	ToolListAgentCalls = "list_agent_calls"
)

// ToolNames returns the six tool names in the contract's order, as a fresh
// slice (AGENT_TOOL_NAMES in the official SDKs).
func ToolNames() []string {
	return []string{
		ToolDiscoverAgents, ToolPromptAgent, ToolWaitAgent,
		ToolAnswerAgent, ToolCancelAgent, ToolListAgentCalls,
	}
}

// BlockingTools returns the three tools an agent offers when it runs no calls
// at once, in the contract's order: discover_agents, prompt_agent and
// answer_agent (BLOCKING_AGENT_TOOLS in the official SDKs). Every definition
// costs input tokens on every model call, and without wait_agent nothing can
// be detached. Pass the result to [WithTools].
func BlockingTools() []string {
	return []string{ToolDiscoverAgents, ToolPromptAgent, ToolAnswerAgent}
}

// Definition is one tool as a model is shown it: a name, the description it
// reads and its parameters as a JSON Schema. A program maps it to its model
// API's tool format (an MCP inputSchema, a chat-completions function, ...)
// without changing a word. It marshals as {"name", "description",
// "parameters"}, the shape of the language-neutral fixtures.
type Definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

//go:embed definitions/*.json
var definitionFiles embed.FS

type loadedDefinitions struct {
	defs     []Definition
	blocking map[string]string
}

var loadDefinitions = sync.OnceValues(func() (*loadedDefinitions, error) {
	out := &loadedDefinitions{blocking: map[string]string{}}
	for _, name := range ToolNames() {
		raw, err := definitionFiles.ReadFile("definitions/" + name + ".json")
		if err != nil {
			return nil, err
		}
		var d Definition
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, fmt.Errorf("tools: definition %s: %w", name, err)
		}
		if d.Name != name {
			return nil, fmt.Errorf("tools: definition file %s names %q", name, d.Name)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, d.Parameters); err != nil {
			return nil, fmt.Errorf("tools: definition %s parameters: %w", name, err)
		}
		d.Parameters = compact.Bytes()
		out.defs = append(out.defs, d)
	}
	raw, err := definitionFiles.ReadFile("definitions/blocking.json")
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &out.blocking); err != nil {
		return nil, fmt.Errorf("tools: blocking descriptions: %w", err)
	}
	return out, nil
})

// ToolDefinitions returns the six definitions in the contract's order, as a
// fresh copy on every call: a program may change its copy (add a parameter of
// its own, say) without touching anyone else's. They are the vendored
// language-neutral fixtures, embedded.
func ToolDefinitions() []Definition {
	l, err := loadDefinitions()
	if err != nil {
		panic(err) //nolint:forbidigo // the embedded files are checked by this package's tests, so a failure is a build defect
	}
	out := make([]Definition, len(l.defs))
	for i, d := range l.defs {
		d.Parameters = bytes.Clone(d.Parameters)
		out[i] = d
	}
	return out
}

// offeredDefinitions returns the definitions of the offered tools in the
// contract's order. Without wait_agent nothing can be detached: each
// definition loses its wait parameter, and a description that mentions
// wait_agent is replaced by its blocking-only words. Nothing else changes.
func offeredDefinitions(offered map[string]bool) ([]Definition, error) {
	l, err := loadDefinitions()
	if err != nil {
		return nil, err
	}
	var out []Definition
	for _, d := range ToolDefinitions() {
		if !offered[d.Name] {
			continue
		}
		if !offered[ToolWaitAgent] {
			p, err := removeProperty(d.Parameters, "wait")
			if err != nil {
				return nil, err
			}
			d.Parameters = p
			if strings.Contains(d.Description, ToolWaitAgent) {
				b, ok := l.blocking[d.Name]
				if !ok {
					return nil, fmt.Errorf("tools: no blocking-only description for %s", d.Name)
				}
				d.Description = b
			}
		}
		out = append(out, d)
	}
	return out, nil
}

type member struct {
	key string
	val json.RawMessage
}

// members reads a JSON object into its members in document order.
func members(raw json.RawMessage) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("tools: not a JSON object")
	}
	var out []member
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		key, _ := k.(string)
		out = append(out, member{key, v})
	}
	return out, nil
}

func joinMembers(ms []member) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range ms {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(m.key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(m.val)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// removeProperty deletes properties.<name> from a JSON Schema, keeping the
// order of everything else.
func removeProperty(schema json.RawMessage, name string) (json.RawMessage, error) {
	top, err := members(schema)
	if err != nil {
		return nil, err
	}
	for i, m := range top {
		if m.key != "properties" {
			continue
		}
		props, err := members(m.val)
		if err != nil {
			return nil, err
		}
		kept := props[:0:0]
		for _, p := range props {
			if p.key != name {
				kept = append(kept, p)
			}
		}
		top[i].val = joinMembers(kept)
	}
	return joinMembers(top), nil
}
