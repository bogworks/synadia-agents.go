// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents_test

import (
	"errors"
	"fmt"

	agents "github.com/bogworks/synadia-agents.go"
)

func ExampleNewAgentSubject() {
	s, err := agents.NewAgentSubject("echo", "me", "default")
	if err != nil {
		panic(err)
	}
	fmt.Println(s.Prompt())
	fmt.Println(s.Heartbeat())
	fmt.Println(s.Status())
	// Output:
	// agents.prompt.echo.me.default
	// agents.hb.echo.me.default
	// agents.status.echo.me.default
}

func ExampleParseSubject() {
	verb, s, err := agents.ParseSubject("agents.prompt.echo.me.default")
	if err != nil {
		panic(err)
	}
	fmt.Println(verb, s.Agent(), s.Owner(), s.Name())
	// Output:
	// prompt echo me default
}

func ExampleDecodeEnvelope() {
	// Plain text is the shorthand of SAP section 5.1.
	env, err := agents.DecodeEnvelope([]byte("hello"))
	if err != nil {
		panic(err)
	}
	fmt.Println(env.Prompt)

	// A payload that is not a valid envelope is a 400 service error.
	_, err = agents.DecodeEnvelope([]byte("{not json"))
	code, _ := agents.CodeOf(err)
	fmt.Println(code)
	// Output:
	// hello
	// 400
}

func ExampleEncodeChunk() {
	b, err := agents.EncodeChunk(agents.ResponseChunk{Text: "hi"})
	if err != nil {
		panic(err)
	}
	fmt.Println(string(b))
	fmt.Println(string(agents.AckChunk()))
	// Output:
	// {"type":"response","data":"hi"}
	// {"type":"status","data":"ack"}
}

func ExampleDecodeChunk() {
	c, err := agents.DecodeChunk([]byte(`{"type":"response","data":"hi"}`))
	if err != nil {
		panic(err)
	}
	if r, ok := c.(agents.ResponseChunk); ok {
		fmt.Println(c.Type(), r.Text)
	}
	// Output:
	// response hi
}

func ExampleParseSize() {
	n, err := agents.ParseSize("8MB")
	if err != nil {
		panic(err)
	}
	fmt.Println(n, agents.FormatSize(n))

	_, err = agents.ParseSize("lots")
	fmt.Println(err != nil)
	// Output:
	// 8388608 8MB
	// true
}

func ExampleServiceError() {
	err := fmt.Errorf("prompt failed: %w", agents.NewServiceError(agents.CodeRateLimited, "slow down"))

	code, ok := agents.CodeOf(err)
	fmt.Println(code, ok)
	fmt.Println(agents.IsCode(err, agents.CodeRateLimited))
	fmt.Println(errors.Is(err, agents.ErrRateLimited))
	// Output:
	// 429 true
	// true
	// true
}
