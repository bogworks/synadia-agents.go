// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func mustBadRequest(t *testing.T, err error, ctx string) {
	t.Helper()
	var se *ServiceError
	if err == nil || !errors.As(err, &se) || se.Code != 400 || !errors.Is(err, ErrBadRequest) {
		t.Errorf("%s: want a 400 ServiceError, got %v", ctx, err)
	}
}

// SAP:L278-L284 (§5.3): the discrimination rule, step by step.
func TestEnvelope_DecodeDiscrimination(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		prompt string
		bad    bool
	}{
		{"plain text", "summarize this", "summarize this", false},
		{"plain text keeps whitespace", "  hello\n", "  hello\n", false},
		{"whitespace-only is text", "   ", "   ", false},
		{"json", `{"prompt":"hi"}`, "hi", false},
		{"leading whitespace then json", " \t\r\n{\"prompt\":\"hi\"}", "hi", false},
		{"leading digit is text", "123", "123", false},
		{"array is text", `["a"]`, `["a"]`, false},
		{"quoted is text", `"x"`, `"x"`, false},
		{"zero bytes", "", "", true},
		{"brace but invalid json", `{"prompt":`, "", true},
		{"brace but trailing garbage", `{"prompt":"x"} junk`, "", true},
		{"json without prompt", `{"text":"x"}`, "", true},
		{"prompt not a string", `{"prompt":5}`, "", true},
		{"prompt null", `{"prompt":null}`, "", true},
		{"empty prompt", `{"prompt":""}`, "", true},
		{"nbsp is not skipped whitespace", " {\"prompt\":\"x\"}", " {\"prompt\":\"x\"}", false},
	}
	for _, c := range cases {
		env, err := DecodeEnvelope([]byte(c.in))
		if c.bad {
			mustBadRequest(t, err, c.name)
			continue
		}
		if err != nil || env.Prompt != c.prompt || len(env.Attachments) != 0 {
			t.Errorf("%s: %+v %v", c.name, env, err)
		}
	}
}

// SAP:L242-L245, L276-L284 (§5.1): invalid UTF-8 is rejected, never repaired.
func TestEnvelope_InvalidUTF8(t *testing.T) {
	for name, in := range map[string]string{
		"plain":       "a\xffb",
		"json prompt": "{\"prompt\":\"a\xffb\"}",
		"truncated":   "{\"prompt\":\"a\xe2\x82\"}",
	} {
		_, err := DecodeEnvelope([]byte(in))
		var se *ServiceError
		if !errors.As(err, &se) || se.Code != 400 {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := EncodePlainPrompt("a\xffb"); !errors.Is(err, ErrInvalidUTF8) || !errors.Is(err, ErrLocal) {
		t.Errorf("EncodePlainPrompt: %v", err)
	}
	if _, err := (Envelope{Prompt: "a\xffb"}).Encode(); !errors.Is(err, ErrInvalidUTF8) {
		t.Errorf("Encode: %v", err)
	}
}

// SAP:L259, L272, L284 (§5.1, §5.2): bad attachments are a 400.
func TestEnvelope_DecodeAttachmentErrors(t *testing.T) {
	for name, in := range map[string]string{
		"not an array":     `{"prompt":"x","attachments":{}}`,
		"null":             `{"prompt":"x","attachments":null}`,
		"item not object":  `{"prompt":"x","attachments":["a"]}`,
		"missing filename": `{"prompt":"x","attachments":[{"content":""}]}`,
		"missing content":  `{"prompt":"x","attachments":[{"filename":"a"}]}`,
		"empty filename":   `{"prompt":"x","attachments":[{"filename":"","content":""}]}`,
		"filename number":  `{"prompt":"x","attachments":[{"filename":1,"content":""}]}`,
		"url-safe base64":  `{"prompt":"x","attachments":[{"filename":"a","content":"aGVsbG8-"}]}`,
		"unpadded base64":  `{"prompt":"x","attachments":[{"filename":"a","content":"aGVsbG8"}]}`,
		"whitespace":       `{"prompt":"x","attachments":[{"filename":"a","content":"aGVs bG8="}]}`,
		"trailing newline": `{"prompt":"x","attachments":[{"filename":"a","content":"aGVsbG8=\n"}]}`,
	} {
		_, err := DecodeEnvelope([]byte(in))
		mustBadRequest(t, err, name)
	}
}

// SAP:L307 (§5.6): unknown fields are tolerated and preserved, also inside attachment objects.
//
// catalog: SAP-5.6-02
func TestEnvelope_UnknownFieldsPreservedOnRelay(t *testing.T) {
	in := `{"prompt":"p","x-trace":{"id":7},"attachments":[{"filename":"a.txt","content":"aGk=","mime":"text/plain"}],"__proto__":1}`
	env, err := DecodeEnvelope([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if string(env.Extra["x-trace"]) != `{"id":7}` || string(env.Extra["__proto__"]) != `1` || len(env.Extra) != 2 {
		t.Fatalf("%v", env.Extra)
	}
	if string(env.Attachments[0].Extra["mime"]) != `"text/plain"` {
		t.Fatalf("%v", env.Attachments[0].Extra)
	}
	out, err := env.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]json.RawMessage
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"prompt", "x-trace", "attachments", "__proto__"} {
		if _, ok := back[k]; !ok {
			t.Errorf("relay lost %s: %s", k, out)
		}
	}
	if !strings.Contains(string(back["attachments"]), `"mime":"text/plain"`) {
		t.Errorf("attachment extra lost: %s", back["attachments"])
	}
	again, _ := DecodeEnvelope(out)
	if again.Prompt != "p" || string(again.Extra["x-trace"]) != `{"id":7}` {
		t.Fatal("second round trip")
	}
}

// SAP:L258, L270-L272 (§5.1, §5.2): the encoder writes JSON with standard padded base64.
func TestEnvelope_EncodeShape(t *testing.T) {
	env := Envelope{Prompt: "summarize <b>&</b>", Attachments: []Attachment{NewAttachment("report.pdf", []byte("hello"))}}
	got, err := env.Encode()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"prompt":"summarize <b>&</b>","attachments":[{"filename":"report.pdf","content":"aGVsbG8="}]}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if n, _ := env.EncodedSize(); n != len(want) {
		t.Fatal(n)
	}
	// The protocol's own fields win a clash with an extra of the same name.
	env.Extra = map[string]json.RawMessage{"prompt": json.RawMessage(`"evil"`), "attachments": json.RawMessage(`[]`), "k": json.RawMessage(`1`)}
	got, _ = env.Encode()
	if !strings.HasSuffix(string(got), `,"k":1}`) || strings.Contains(string(got), "evil") {
		t.Fatalf("%s", got)
	}
}

// SAP:L258, L259 (§5.1): an empty prompt is refused locally; so is a bad attachment.
func TestEnvelope_EncodeRefusesInvalid(t *testing.T) {
	if _, err := (Envelope{}).Encode(); !errors.Is(err, ErrPromptEmpty) || !errors.Is(err, ErrLocal) {
		t.Fatal(err)
	}
	bad := Envelope{Prompt: "x", Attachments: []Attachment{{Filename: "a", Content: "not base64!"}}}
	var ae *AttachmentError
	if _, err := bad.Encode(); !errors.As(err, &ae) || ae.Index != 0 {
		t.Fatal(err)
	}
	if _, err := (Envelope{Prompt: "x", Attachments: []Attachment{{Filename: "", Content: ""}}}).Encode(); !errors.Is(err, ErrInvalidAttachment) {
		t.Fatal(err)
	}
}

// SAP:L244, L280-L282 (§5.1, §5.3): the plain-text shorthand, and why '{' is refused.
func TestEnvelope_EncodePlainPrompt(t *testing.T) {
	b, err := EncodePlainPrompt("hello")
	if err != nil || string(b) != "hello" {
		t.Fatal(b, err)
	}
	if _, err := EncodePlainPrompt(""); !errors.Is(err, ErrPromptEmpty) {
		t.Fatal(err)
	}
	if _, err := EncodePlainPrompt(" \n{not json"); err == nil {
		t.Fatal("brace-led text accepted")
	}
	// A decoded plain prompt equals what was sent.
	env, _ := DecodeEnvelope(b)
	if env.Prompt != "hello" {
		t.Fatal(env)
	}
}

// SAP:L290-L291 (§5.4): attachments_ok false fails locally; no declaration makes no assertion.
func TestEnvelope_LimitsAttachments(t *testing.T) {
	f, tr := false, true
	if err := (PromptLimits{AttachmentsOK: &f}).CheckAttachments(1); !errors.Is(err, ErrAttachmentsNotSupported) || !errors.Is(err, ErrLocal) {
		t.Fatal(err)
	}
	for name, l := range map[string]PromptLimits{"false none sent": {AttachmentsOK: &f}, "true": {AttachmentsOK: &tr}, "undeclared": {}} {
		n := 1
		if name == "false none sent" {
			n = 0
		}
		if err := l.CheckAttachments(n); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// SAP:L291, L887 (§5.4, §13.5.1): the final encoded size plus header bytes against the smaller cap.
func TestEnvelope_LimitsSize(t *testing.T) {
	cases := []struct {
		name         string
		l            PromptLimits
		size, hdr    int
		conn         int64
		wantLimit    int64 // 0: no error
		wantHdrBytes int64
	}{
		{"within", PromptLimits{MaxPayload: 100}, 100, 0, 0, 0, 0},
		{"over by one", PromptLimits{MaxPayload: 100}, 101, 0, 0, 100, 0},
		{"header counted", PromptLimits{MaxPayload: 100}, 90, 11, 0, 100, 11},
		{"header fits", PromptLimits{MaxPayload: 100}, 90, 10, 0, 0, 0},
		{"connection cap smaller", PromptLimits{MaxPayload: 1000}, 60, 0, 50, 50, 0},
		{"endpoint cap smaller", PromptLimits{MaxPayload: 40}, 60, 0, 500, 40, 0},
		{"only connection cap", PromptLimits{}, 60, 0, 50, 50, 0},
		{"header on the connection cap", PromptLimits{MaxPayload: 1000}, 40, 11, 50, 50, 11},
		{"nothing declared", PromptLimits{}, 1 << 30, 0, 0, 0, 0},
	}
	for _, c := range cases {
		err := c.l.CheckSize(c.size, c.hdr, c.conn)
		var pe *PayloadTooLargeError
		switch {
		case c.wantLimit == 0 && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.wantLimit != 0 && (!errors.As(err, &pe) || pe.Limit != c.wantLimit || pe.HeaderBytes != c.wantHdrBytes || pe.Actual != int64(c.size) || !errors.Is(err, ErrLocal)):
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// SAP:L288-L292 (§5.4): Prepare runs every local check before anything is published.
func TestEnvelope_Prepare(t *testing.T) {
	f, tr := false, true
	env := Envelope{Prompt: "hi", Attachments: []Attachment{NewAttachment("a", []byte("x"))}}
	if _, err := (PromptLimits{MaxPayload: 1 << 20, AttachmentsOK: &f}).Prepare(env, 0, 0); !errors.Is(err, ErrAttachmentsNotSupported) {
		t.Fatal(err)
	}
	if _, err := (PromptLimits{MaxPayload: 10, AttachmentsOK: &tr}).Prepare(env, 0, 0); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatal(err)
	}
	if _, err := (PromptLimits{MaxPayload: 1 << 20, AttachmentsOK: &tr}).Prepare(Envelope{}, 0, 0); !errors.Is(err, ErrPromptEmpty) {
		t.Fatal(err)
	}
	b, err := (PromptLimits{MaxPayload: 1 << 20, AttachmentsOK: &tr}).Prepare(env, 0, 0)
	if err != nil || len(b) == 0 {
		t.Fatal(b, err)
	}
}

// SAP:L293 (§5.4), SAP:L599 (§9.2): optional server-side enforcement, both 400.
func TestEnvelope_CheckInbound(t *testing.T) {
	f, tr := false, true
	withAtt := Envelope{Prompt: "x", Attachments: []Attachment{NewAttachment("a", nil)}}
	mustBadRequest(t, PromptLimits{AttachmentsOK: &f}.CheckInbound(10, withAtt), "attachments not ok")
	mustBadRequest(t, PromptLimits{MaxPayload: 5}.CheckInbound(6, Envelope{Prompt: "x"}), "oversize")
	for _, l := range []PromptLimits{{}, {AttachmentsOK: &tr, MaxPayload: 10}, {AttachmentsOK: &f, MaxPayload: 10}} {
		e := withAtt
		if l.AttachmentsOK != nil && !*l.AttachmentsOK {
			e = Envelope{Prompt: "x"}
		}
		if err := l.CheckInbound(10, e); err != nil {
			t.Errorf("%+v: %v", l, err)
		}
	}
}

func TestEnvelope_AttachmentRoundTrip(t *testing.T) {
	data := []byte{0, 1, 2, 0xff, 0xfe}
	a := NewAttachment("f.bin", data)
	b, err := a.Bytes()
	if err != nil || string(b) != string(data) {
		t.Fatal(b, err)
	}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := (Attachment{Filename: "x", Content: "aGVsbG8"}).Bytes(); !errors.Is(err, ErrInvalidAttachment) {
		t.Fatal(err)
	}
	var back Attachment
	raw, _ := json.Marshal(a)
	if err := json.Unmarshal(raw, &back); err != nil || back.Filename != "f.bin" || back.Content != a.Content {
		t.Fatal(back, err)
	}
	if err := json.Unmarshal([]byte(`[]`), &back); err == nil {
		t.Fatal("array accepted")
	}
}
