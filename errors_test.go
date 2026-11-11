// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// SAP:L597-L607 (§9.2): the status code taxonomy.
func TestError_Taxonomy(t *testing.T) {
	for _, c := range []int{400, 401, 403, 404, 409, 429, 500} {
		if !KnownCode(c) {
			t.Errorf("%d unknown", c)
		}
	}
	for _, c := range []int{0, 200, 402, 418, 502} {
		if KnownCode(c) {
			t.Errorf("%d known", c)
		}
	}
}

// REQ-007, SAP:L574-L585 (§9.1): code, header text and the optional JSON body.
func TestError_ParseServiceErrorWithJSONBody(t *testing.T) {
	h := Header{HeaderServiceErrorCode: {"429"}, HeaderServiceError: {"rate limited"}}
	body := []byte(`{"error":"rate_limited","message":"Too many concurrent requests","retry_after_s":30}`)
	se, ok := ParseServiceError(h, body)
	if !ok {
		t.Fatal("not an error signal")
	}
	if se.Code != 429 || se.Description != "rate limited" || se.Body == nil ||
		se.Body.Error != "rate_limited" || se.Body.Message != "Too many concurrent requests" ||
		string(se.Body.Extra["retry_after_s"]) != "30" {
		t.Fatalf("%+v %+v", se, se.Body)
	}
	if se.Message() != "Too many concurrent requests" {
		t.Fatal(se.Message())
	}
	err := fmt.Errorf("wrapped: %w", se)
	var got *ServiceError
	if !errors.As(err, &got) || !errors.Is(err, ErrRateLimited) || errors.Is(err, ErrInternal) {
		t.Fatal("errors.Is/As")
	}
	if c, ok := CodeOf(err); !ok || c != 429 {
		t.Fatal(c, ok)
	}
	if errors.Is(err, ErrLocal) {
		t.Fatal("a remote error must not look local")
	}
}

// REQ-007, SAP:L593 (§9.1): an empty or non-JSON body falls back to the header text.
func TestError_HeaderFallback(t *testing.T) {
	h := Header{HeaderServiceErrorCode: {"500"}, HeaderServiceError: {"boom"}}
	for name, body := range map[string][]byte{
		"empty":        nil,
		"not json":     []byte("plain text oops"),
		"json array":   []byte(`["a"]`),
		"json scalar":  []byte(`42`),
		"truncated":    []byte(`{"error":`),
		"leading text": []byte(`x{"message":"hi"}`),
	} {
		se, ok := ParseServiceError(h, body)
		if !ok || se.Body != nil || se.Message() != "boom" || se.Error() != "service error 500: boom" {
			t.Errorf("%s: %+v", name, se)
		}
	}
	// A JSON body without a message also falls back to the header (SAP:L589).
	se, _ := ParseServiceError(h, []byte(`{"error":"x"}`))
	if se.Message() != "boom" || se.Body == nil || se.Body.Error != "x" {
		t.Fatalf("%+v", se)
	}
}

// SAP:L574 (§9.1): a code that is not a number reads as 500, as the official SDK does.
func TestError_NonNumericCode(t *testing.T) {
	se, ok := ParseServiceError(Header{HeaderServiceErrorCode: {"abc"}}, nil)
	if !ok || se.Code != 500 {
		t.Fatal(se, ok)
	}
	if _, ok := ParseServiceError(Header{"X": {"1"}}, []byte(`{"error":"x"}`)); ok {
		t.Fatal("no code header must not be an error")
	}
}

// SAP:L584, L591 (§9.1): unknown body fields are tolerated and preserved.
func TestError_BodyRoundTripKeepsExtras(t *testing.T) {
	se := NewServiceError(409, "conflict\nwith state")
	se.Body = &ErrorBody{Error: "conflict", Message: "m", Extra: map[string]json.RawMessage{
		"detail": json.RawMessage(`{"a":1}`), "error": json.RawMessage(`"shadow"`)}}
	payload, err := se.Payload()
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != `{"error":"conflict","message":"m","detail":{"a":1}}` {
		t.Fatal(string(payload))
	}
	back, ok := ParseServiceError(se.Headers(), payload)
	if !ok || back.Code != 409 || back.Description != "conflict | with state" || string(back.Body.Extra["detail"]) != `{"a":1}` {
		t.Fatalf("%+v", back)
	}
	if p, err := NewServiceError(400, "x").Payload(); p != nil || err != nil {
		t.Fatal("no body, no payload")
	}
}

// SDK policy: a description is flat and capped for use in a header.
func TestError_SanitizeErrorDescription(t *testing.T) {
	cases := map[string]string{
		"plain":         "plain",
		"a\nb":          "a | b",
		"a\r\nb\n\n c ": "a | b | c",
		"\n\n":          "",
		"bad\xffbytes":  "bad�bytes",
	}
	for in, want := range cases {
		if got := SanitizeErrorDescription(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
	long := strings.Repeat("x", 500)
	if got := SanitizeErrorDescription(long); len(got) != 200 || !strings.HasSuffix(got, "...") {
		t.Fatalf("len %d", len(got))
	}
	// Truncation never splits a character.
	multi := strings.Repeat("é", 300)
	got := SanitizeErrorDescription(multi)
	if len(got) > 200 || strings.ToValidUTF8(got, "?") != got {
		t.Fatalf("split rune: len %d", len(got))
	}
}

// REQ-007: a caller can tell a local refusal from a remote error.
func TestError_LocalVersusRemote(t *testing.T) {
	local := []error{
		ErrPromptEmpty, ErrAttachmentsNotSupported,
		&PayloadTooLargeError{Limit: 1, Actual: 2},
		&AttachmentError{Index: 0, Reason: "x"},
		&VersionError{Agent: "9.9"},
		&TokenError{Role: RoleName, Token: "$x"},
		&SubjectError{Subject: "x"},
	}
	for _, e := range local {
		if !errors.Is(e, ErrLocal) {
			t.Errorf("%T not local", e)
		}
		if _, ok := CodeOf(e); ok {
			t.Errorf("%T has a code", e)
		}
	}
	for _, e := range []error{&ServiceError{Code: 500}, &ProtocolError{Reason: "x"}, &StreamStalledError{Timeout: time.Second}, &NotAgentError{Reason: "x"}} {
		if errors.Is(e, ErrLocal) {
			t.Errorf("%T local", e)
		}
	}
	if !errors.Is(&PayloadTooLargeError{}, ErrPayloadTooLarge) || !errors.Is(&AttachmentError{}, ErrInvalidAttachment) ||
		!errors.Is(&ProtocolError{}, ErrProtocol) || !errors.Is(&NotAgentError{}, ErrNotAgent) ||
		!errors.Is(&StreamStalledError{}, ErrStreamStalled) {
		t.Fatal("sentinel matching")
	}
	pe := &ProtocolError{Reason: "r", Err: errors.New("cause")}
	if errors.Unwrap(pe) == nil || !strings.Contains(pe.Error(), "cause") {
		t.Fatal("unwrap")
	}
}

func TestError_Messages(t *testing.T) {
	for _, e := range []error{
		&PayloadTooLargeError{Limit: 10, Actual: 20},
		&PayloadTooLargeError{Limit: 10, Actual: 20, HeaderBytes: 5},
		&AttachmentError{Index: -1, Reason: "r"}, &AttachmentError{Index: 2, Reason: "r"},
		&StreamStalledError{Timeout: time.Minute},
	} {
		if e.Error() == "" {
			t.Errorf("%T empty message", e)
		}
	}
}
