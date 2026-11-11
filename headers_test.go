// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import "testing"

// SAP:L574-L575 (§9.1), SAP:L883 (§13.5.1): the header names.
func TestHeader_Names(t *testing.T) {
	if HeaderServiceErrorCode != "Nats-Service-Error-Code" || HeaderServiceError != "Nats-Service-Error" ||
		HeaderAgentSender != "Agent-Sender" || HeaderRequestInfo != "Nats-Request-Info" {
		t.Fatal("header constants")
	}
}

// SAP:L883 (§13.5.1): Agent-Sender is matched case-sensitively.
func TestHeader_ValueIsCaseSensitive(t *testing.T) {
	h := Header{"Agent-Sender": {"x", "y"}, "empty": {}}
	if v, ok := HeaderValue(h, HeaderAgentSender); !ok || v != "x" {
		t.Fatal(v, ok)
	}
	if _, ok := HeaderValue(h, "agent-sender"); ok {
		t.Fatal("case-insensitive match")
	}
	if v, ok := HeaderValue(h, "empty"); !ok || v != "" {
		t.Fatal(v, ok)
	}
	if _, ok := HeaderValue(nil, "x"); ok {
		t.Fatal("nil header")
	}
}

// SAP:L388 (§6.5), SAP:L394 (§6.5): the terminator is zero bytes with no headers;
// an error-headered empty message is not it.
func TestHeader_IsTerminator(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		h    Header
		want bool
	}{
		{"empty no headers", nil, nil, true},
		{"empty zero-length map", []byte{}, Header{}, true},
		{"empty with error headers", nil, Header{HeaderServiceErrorCode: {"500"}}, false},
		{"empty with any header", nil, Header{"X": {"y"}}, false},
		{"non-empty", []byte("x"), nil, false},
	}
	for _, c := range cases {
		if got := IsTerminator(c.data, c.h); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

// SAP:L700 (§12): every message is inspected for the error code header first.
func TestHeader_IsErrorSignal(t *testing.T) {
	if IsErrorSignal(nil) || IsErrorSignal(Header{"X": {"1"}}) {
		t.Fatal("false positive")
	}
	if !IsErrorSignal(Header{HeaderServiceErrorCode: {"400"}}) {
		t.Fatal("missed")
	}
	// Leniency on receive: a differently-cased key is still an error signal.
	if !IsErrorSignal(Header{"nats-service-error-code": {"400"}}) {
		t.Fatal("case fold")
	}
}
