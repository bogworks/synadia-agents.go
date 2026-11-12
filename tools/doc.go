// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package tools is the AgentTools layer of the official SDKs for Go: six LLM
// tool definitions that let a model discover, prompt, wait on, answer, list and
// cancel calls to other agents on a NATS system, and an executor that runs a
// tool call by name over a [caller.Client]. It is outside the protocol: it adds
// nothing to the wire, and the agent being prompted sees an ordinary prompt
// from an ordinary caller (SAP §5). It rests on discovery (SAP §4), the prompt
// (SAP §5), the response stream (SAP §6), mid-stream questions (SAP §7) and the
// protocol's absence of a cancel message (SAP §6.7).
//
// The package is provider-neutral: it imports no LLM SDK. Definitions are
// plain structs that marshal to {"name", "description", "parameters"}, the
// shape of the language-neutral fixtures the official SDKs share, so a program
// maps them to any model API's tool format. The definitions and the
// blocking-only descriptions are the vendored fixtures, embedded.
//
//	tl, _ := tools.New(client)                    // client is a *caller.Client
//	defer tl.Close()
//	for _, d := range tl.Definitions() { ... }    // hand to the model API
//	res, _ := tl.Execute(ctx, call.Name, call.Arguments, tools.WithToolCallID(call.ID))
//	text, _ := json.Marshal(res)                  // hand back to the model
//
// # Results
//
// Every result is a JSON object ([Result]). Anything the model can act on comes
// back in words: a refusal is {"error": "..."}, a failed call has "error".
// [Tools.Execute] returns a Go error only after [Tools.Close].
//
// # Calls and scopes
//
// A call is one prompt, tracked from the moment it is sent. A call started
// while the program serves a prompt of its own belongs to that prompt; add
// [Tools.Interceptor] to the host (or wrap the handler in
// [Tools.RunInPromptScope]) and the calls still open when the handler returns
// are canceled and their questions refused. Outside a served prompt, calls
// share one scope that lives as long as the [Tools].
//
// # Limits and trust
//
// A peer controls what an agent streams back, so every call is bounded: reply
// text, question count, returned-file records and total result size
// ([WithMaxReplyBytes], [WithMaxQuestions], [WithMaxReturnedFiles],
// [WithMaxResultBytes]); a call over a cap fails with a reason and its stream is
// closed. Files are sent only from directories named with
// [WithAttachmentRoots] (and the staging directory), opened through an
// [os.Root], so a link swapped after the check cannot lead outside.
//
// A question names its own reply subject (SAP §7.1). An answer, or the refusal
// of a question, is published only under a trusted prefix ([WithAnswerReplyPrefixes];
// by default "_INBOX." and the connection's own inbox prefix); give the
// connection publish permission on those prefixes only.
//
// A delegation cycle is bounded two ways. Prompts the tools make while serving a
// prompt carry the depth in the [DepthHeader] header (add [DepthInterceptor]
// to the client; [Tools.Interceptor] reads it) and are refused past
// [WithMaxDepth]; the header is advisory between cooperating instances, since
// a hostile peer can reset it. The hard bound is [WithMaxActiveCalls], the calls
// open at once across every scope.
//
// Results show the model stable messages only: no absolute path and no
// transport detail. The detail goes to the [WithLogger] logger.
package tools
