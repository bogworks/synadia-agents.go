// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

// SAP:L333-L343 (§6.2): typed chunks, and unknown types are surfaced for the caller to ignore.
func TestChunk_DecodeTypes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Chunk
	}{
		{"response string form", `{"type":"response","data":"Hello, world."}`, ResponseChunk{Text: "Hello, world."}},
		{"response object form", `{"type":"response","data":{"text":"Hi","attachments":[{"filename":"a","content":"aGk="}]}}`,
			ResponseChunk{Text: "Hi", Attachments: []Attachment{{Filename: "a", Content: "aGk="}}}},
		{"response object no attachments", `{"type":"response","data":{"text":""}}`, ResponseChunk{Text: ""}},
		{"status ack", `{"type":"status","data":"ack"}`, StatusChunk{Status: "ack"}},
		{"status unknown value", `{"type":"status","data":"thinking"}`, StatusChunk{Status: "thinking"}},
		{"query", `{"type":"query","data":{"id":"a8f1","reply_subject":"_INBOX.X","prompt":"Confirm?"}}`,
			QueryChunk{ID: "a8f1", ReplySubject: "_INBOX.X", Prompt: "Confirm?"}},
		{"unknown type", `{"type":"progress","data":{"pct":5}}`, UnknownChunk{ChunkType: "progress", Data: json.RawMessage(`{"pct":5}`)}},
		{"unknown type with null data", `{"type":"x","data":null}`, UnknownChunk{ChunkType: "x", Data: json.RawMessage(`null`)}},
	}
	for _, c := range cases {
		got, err := DecodeChunk([]byte(c.in))
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %#v, %v want %#v", c.name, got, err, c.want)
		}
		if err == nil && got.Type() == "" {
			t.Errorf("%s: empty Type", c.name)
		}
	}
}

// SAP:L341 (§6.2): plain text is not accepted on the response side; malformed known shapes are protocol errors.
func TestChunk_DecodeMalformed(t *testing.T) {
	for name, in := range map[string]string{
		"plain text":          `hello`,
		"array":               `[1]`,
		"no type":             `{"data":"x"}`,
		"type number":         `{"type":1,"data":"x"}`,
		"response no data":    `{"type":"response"}`,
		"response data num":   `{"type":"response","data":5}`,
		"response no text":    `{"type":"response","data":{"attachments":[]}}`,
		"response text num":   `{"type":"response","data":{"text":1}}`,
		"response att scalar": `{"type":"response","data":{"text":"x","attachments":"no"}}`,
		"response att item":   `{"type":"response","data":{"text":"x","attachments":[{"filename":"a"}]}}`,
		"status not string":   `{"type":"status","data":1}`,
		"query not object":    `{"type":"query","data":"x"}`,
		"query no id":         `{"type":"query","data":{"reply_subject":"r","prompt":"p"}}`,
		"query no reply":      `{"type":"query","data":{"id":"i","prompt":"p"}}`,
		"query no prompt":     `{"type":"query","data":{"id":"i","reply_subject":"r"}}`,
		"empty":               ``,
	} {
		got, err := DecodeChunk([]byte(in))
		var pe *ProtocolError
		if err == nil || !errors.As(err, &pe) || !errors.Is(err, ErrProtocol) || got != nil {
			t.Errorf("%s: got %#v, %v", name, got, err)
		}
	}
}

// SAP:L364 (§6.3): multiple response chunks concatenate in publication order.
func TestChunk_ResponseTextConcatenates(t *testing.T) {
	var text string
	for _, in := range []string{`{"type":"response","data":"Hel"}`, `{"type":"status","data":"ack"}`, `{"type":"response","data":{"text":"lo"}}`} {
		c, err := DecodeChunk([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		if r, ok := c.(ResponseChunk); ok {
			text += r.Text
		}
	}
	if text != "Hello" {
		t.Fatal(text)
	}
}

// SAP:L360, L405 (§6.3, §6.6): unknown fields in data objects are tolerated and preserved.
//
// catalog: SAP-6.6-08
func TestChunk_UnknownDataFieldsPreserved(t *testing.T) {
	in := `{"type":"response","data":{"text":"x","cost":{"usd":1},"attachments":[{"filename":"f","content":"","tag":1}]}}`
	c, err := DecodeChunk([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	rc := c.(ResponseChunk)
	if string(rc.Extra["cost"]) != `{"usd":1}` || string(rc.Attachments[0].Extra["tag"]) != "1" {
		t.Fatalf("%+v", rc)
	}
	out, err := EncodeChunk(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"type":"response","data":{"text":"x","attachments":[{"filename":"f","content":"","tag":1}],"cost":{"usd":1}}}` {
		t.Fatal(string(out))
	}
	q, err := DecodeChunk([]byte(`{"type":"query","data":{"id":"i","reply_subject":"_INBOX.a","prompt":"p","hint":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(q.(QueryChunk).Extra["hint"]) != "true" {
		t.Fatal("query extra lost")
	}
}

// SAP:L349-L355 (§6.3): a response without attachments uses the bare-string form.
func TestChunk_EncodeForms(t *testing.T) {
	cases := []struct {
		in   Chunk
		want string
	}{
		{ResponseChunk{Text: "Hello, world."}, `{"type":"response","data":"Hello, world."}`},
		{ResponseChunk{Text: "<a>"}, `{"type":"response","data":"<a>"}`},
		{ResponseChunk{Text: "t", Attachments: []Attachment{NewAttachment("a", []byte("hi"))}}, `{"type":"response","data":{"text":"t","attachments":[{"filename":"a","content":"aGk="}]}}`},
		{StatusChunk{Status: StatusAck}, `{"type":"status","data":"ack"}`},
		{QueryChunk{ID: "i", ReplySubject: "_INBOX.x", Prompt: "p"}, `{"type":"query","data":{"id":"i","reply_subject":"_INBOX.x","prompt":"p"}}`},
		{UnknownChunk{ChunkType: "future", Data: json.RawMessage(`[1]`)}, `{"type":"future","data":[1]}`},
	}
	for _, c := range cases {
		got, err := EncodeChunk(c.in)
		if err != nil || string(got) != c.want {
			t.Errorf("got %s, %v want %s", got, err, c.want)
		}
		back, err := DecodeChunk(got)
		if err != nil || back.Type() != c.in.Type() {
			t.Errorf("round trip %s: %v %v", got, back, err)
		}
	}
}

// SAP:L380 (§6.4): the ack is {type:status,data:ack}.
func TestChunk_AckChunk(t *testing.T) {
	if string(AckChunk()) != `{"type":"status","data":"ack"}` {
		t.Fatal(string(AckChunk()))
	}
	c, _ := DecodeChunk(AckChunk())
	if s, ok := c.(StatusChunk); !ok || s.Status != StatusAck {
		t.Fatal(c)
	}
}

// SAP:L441 (§7.1): a query needs an id and a publishable reply subject.
func TestChunk_EncodeQueryRequiresFields(t *testing.T) {
	if _, err := EncodeChunk(QueryChunk{ReplySubject: "_INBOX.x"}); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
	if _, err := EncodeChunk(QueryChunk{ID: "i", ReplySubject: "a.*"}); !errors.Is(err, ErrInvalidSubject) {
		t.Fatal(err)
	}
	if _, err := EncodeChunk(nil); err == nil {
		t.Fatal("nil chunk accepted")
	}
}

// SAP:L402 (§6.6): the recommended inactivity timeout.
func TestChunk_DefaultInactivityTimeout(t *testing.T) {
	if DefaultInactivityTimeout != 60*time.Second {
		t.Fatal(DefaultInactivityTimeout)
	}
}
