// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"errors"
	"testing"
)

// NFR-001: parsers of untrusted input never panic, and a refusal of an
// envelope is always a 400 ServiceError.
func FuzzDecodeEnvelope(f *testing.F) {
	for _, s := range []string{"", "hi", `{"prompt":"x"}`, `{"prompt":"x","attachments":[{"filename":"a","content":"aGk="}]}`, `{`, `{"prompt":1}`, " \n{}", `{"prompt":"x","attachments":[1]}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		env, err := DecodeEnvelope(data)
		if err != nil {
			if c, ok := CodeOf(err); !ok || c != 400 {
				t.Fatalf("refusal is not a 400: %v", err)
			}
			return
		}
		if env.Prompt == "" {
			t.Fatal("accepted an empty prompt")
		}
		if _, err := env.Encode(); err != nil {
			t.Fatalf("a decoded envelope does not re-encode: %v", err)
		}
	})
}

func FuzzDecodeChunk(f *testing.F) {
	for _, s := range []string{`{"type":"response","data":"x"}`, `{"type":"status","data":"ack"}`, `{"type":"query","data":{"id":"i","reply_subject":"r","prompt":"p"}}`, `{"type":"zz","data":1}`, `x`, ``} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := DecodeChunk(data)
		if err != nil {
			if !errors.Is(err, ErrProtocol) || c != nil {
				t.Fatalf("%v %v", c, err)
			}
			return
		}
		if c == nil {
			t.Fatal("nil chunk")
		}
	})
}

func FuzzDecodeHeartbeat(f *testing.F) {
	f.Add([]byte(sapHeartbeatExample))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := DecodeHeartbeat(data)
		if err != nil {
			if !errors.Is(err, ErrProtocol) {
				t.Fatal(err)
			}
			return
		}
		if h.IntervalS <= 0 {
			t.Fatal("non-positive interval accepted")
		}
	})
}

func FuzzParseSubject(f *testing.F) {
	f.Add("agents.prompt.cc.o.n")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		v, a, err := ParseSubject(s)
		if err != nil {
			return
		}
		if got, serr := a.Subject(v); serr != nil || got != s {
			t.Fatalf("parse/build disagree: %q -> %q (%v)", s, got, serr)
		}
	})
}

func FuzzParseServiceError(f *testing.F) {
	f.Add("429", "x", []byte(`{"error":"e"}`))
	f.Fuzz(func(t *testing.T, code, desc string, body []byte) {
		se, ok := ParseServiceError(Header{HeaderServiceErrorCode: {code}, HeaderServiceError: {desc}}, body)
		if !ok || se == nil {
			t.Fatal("code header not seen")
		}
		_ = se.Error()
	})
}

func FuzzSafeFileName(f *testing.F) {
	f.Add("../a b.txt")
	f.Fuzz(func(t *testing.T, s string) {
		got := SafeFileName(s, 1)
		if got == "" || len(got) > maxSaveNameBytes || got == "." || got == ".." {
			t.Fatalf("%q -> %q", s, got)
		}
	})
}
