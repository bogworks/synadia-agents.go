// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Audit finding 1: SaveAttachments creates files through one opened directory.
func TestSaveAttachments_SymlinkedAncestor(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	res, err := SaveAttachments([]Attachment{NewAttachment("a.txt", []byte("hi"))}, filepath.Join(link, "sub"), SaveOptions{})
	if err != nil || len(res) != 1 || res[0].Path == "" {
		t.Fatal(res, err)
	}
	// dir is resolved once, at call time: the file is in the directory the path named.
	if b, err := os.ReadFile(filepath.Join(realDir, "sub", "a.txt")); err != nil || string(b) != "hi" { //nolint:gosec // the test reads a fixture or temp path it built itself
		t.Fatal(b, err)
	}
}

func TestSaveAttachments_DirectorySwappedMidCall(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "dest")
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	swap := func() {
		if err := os.Rename(dir, filepath.Join(base, "moved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, dir); err != nil {
			t.Skip("symlinks unavailable:", err)
		}
	}
	atts := []Attachment{NewAttachment("a.txt", []byte("1")), NewAttachment("b.txt", []byte("2"))}
	if _, err := saveAttachments(atts, dir, SaveOptions{}, swap); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("files escaped through the swapped path: %v", ents)
	}
	if ents, _ := os.ReadDir(filepath.Join(base, "moved")); len(ents) != 2 {
		t.Fatalf("files not in the opened directory: %v", ents)
	}
}

func TestSaveAttachments_SymlinkAtFilenameNotFollowed(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "victim")
	if err := os.Symlink(target, filepath.Join(dir, "a.txt")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	res, err := SaveAttachments([]Attachment{NewAttachment("a.txt", []byte("x"))}, dir, SaveOptions{})
	if err != nil || filepath.Base(res[0].Path) != "a (2).txt" {
		t.Fatal(res, err)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("symlink was followed")
	}
}

// Audit finding 3: null is not a string, in every decoder.
func TestDecoders_NullIsNotAString(t *testing.T) {
	envs := []string{
		`{"prompt":null}`,
		`{"prompt":"x","attachments":[{"filename":null,"content":""}]}`,
		`{"prompt":"x","attachments":[{"filename":"a","content":null}]}`,
	}
	for _, in := range envs {
		if _, err := DecodeEnvelope([]byte(in)); !IsCode(err, 400) {
			t.Errorf("envelope %s: %v", in, err)
		}
	}
	chunks := []string{
		`{"type":null,"data":"x"}`,
		`{"type":"response","data":null}`,
		`{"type":"response","data":{"text":null}}`,
		`{"type":"response","data":{"text":"x","attachments":[{"filename":null,"content":""}]}}`,
		`{"type":"status","data":null}`,
		`{"type":"query","data":{"id":null,"reply_subject":"_INBOX.x","prompt":"p"}}`,
		`{"type":"query","data":{"id":"i","reply_subject":null,"prompt":"p"}}`,
		`{"type":"query","data":{"id":"i","reply_subject":"_INBOX.x","prompt":null}}`,
	}
	for _, in := range chunks {
		if _, err := DecodeChunk([]byte(in)); !errors.Is(err, ErrProtocol) {
			t.Errorf("chunk %s: %v", in, err)
		}
	}
	hb := []string{
		`{"agent":null,"owner":"o","instance_id":"i","ts":"2026-04-21T14:23:01Z","interval_s":1}`,
		`{"agent":"a","owner":null,"instance_id":"i","ts":"2026-04-21T14:23:01Z","interval_s":1}`,
		`{"agent":"a","owner":"o","instance_id":null,"ts":"2026-04-21T14:23:01Z","interval_s":1}`,
		`{"agent":"a","owner":"o","instance_id":"i","ts":null,"interval_s":1}`,
	}
	for _, in := range hb {
		if _, err := DecodeHeartbeat([]byte(in)); !errors.Is(err, ErrProtocol) {
			t.Errorf("heartbeat %s: %v", in, err)
		}
	}
	var a Attachment
	if err := json.Unmarshal([]byte(`{"filename":null,"content":""}`), &a); err == nil {
		t.Error("attachment null filename accepted")
	}
	// A null endpoint-metadata value is a malformed declaration: ignored, kept in Extra.
	h, err := DecodeHeartbeat([]byte(`{"agent":"a","owner":"o","instance_id":"i","ts":"2026-04-21T14:23:01Z","interval_s":1,"endpoints":{"prompt":{"subject":"s","metadata":{"max_payload":null}}}}`))
	if err != nil || h.Endpoints != nil || h.Extra["endpoints"] == nil {
		t.Errorf("null metadata value: %+v %v", h, err)
	}
	// error body: a null message or code is not a string, so it is kept as an extra.
	se, _ := ParseServiceError(map[string][]string{HeaderServiceErrorCode: {"400"}}, []byte(`{"error":null,"message":null}`))
	if se.Body == nil || se.Body.Error != "" || se.Body.Message != "" || len(se.Body.Extra) != 2 {
		t.Errorf("null body fields: %+v", se.Body)
	}
}

// Audit finding 4: data is required on every non-terminating chunk.
func TestChunk_DataRequired(t *testing.T) {
	for _, in := range []string{`{"type":"x"}`, `{"type":"response"}`, `{"type":"status"}`, `{"type":"query"}`} {
		if _, err := DecodeChunk([]byte(in)); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: %v", in, err)
		}
	}
	c, err := DecodeChunk([]byte(`{"type":"x","data":1}`))
	if u, ok := c.(UnknownChunk); err != nil || !ok || u.ChunkType != "x" {
		t.Errorf("unknown type with data: %#v %v", c, err)
	}
}

// Audit finding 5: SAP:L286-L293, SAP:L887 (§13.5.1). The endpoint limit counts
// the payload alone, plus the Agent-Sender header only when one is sent; the
// connection limit is a separate check.
func TestPromptLimits_EndpointLimitCountsPayloadAlone(t *testing.T) {
	l := PromptLimits{MaxPayload: 100}
	if err := l.CheckSize(100, 0, 0); err != nil {
		t.Fatalf("payload equal to max_payload refused: %v", err)
	}
	if err := l.CheckSize(100, 0, 1000); err != nil {
		t.Fatalf("a roomy connection must not matter: %v", err)
	}
	var pe *PayloadTooLargeError
	if err := l.CheckSize(100, 20, 0); !errors.As(err, &pe) || pe.Connection || pe.Limit != 100 || pe.HeaderBytes != 20 {
		t.Fatalf("sender header counted toward the endpoint limit: %v", err)
	}
	if err := l.CheckSize(60, 0, 50); !errors.As(err, &pe) || !pe.Connection || pe.Limit != 50 {
		t.Fatalf("connection limit: %v", err)
	}
	if err := l.CheckSize(120, 0, 50); !errors.As(err, &pe) || pe.Connection || pe.Limit != 100 {
		t.Fatalf("endpoint limit is checked first: %v", err)
	}
	if err := (PromptLimits{MaxPayload: 1000}).CheckSize(40, 11, 50); !errors.As(err, &pe) || !pe.Connection {
		t.Fatalf("headers count toward the connection limit: %v", err)
	}
}

// Audit finding 7: missing capability metadata makes the endpoint unusable.
func TestPromptLimits_PrepareRefusesWithoutLimits(t *testing.T) {
	tr := true
	env := Envelope{Prompt: "hi"}
	for name, l := range map[string]PromptLimits{
		"zero": {}, "no max_payload": {AttachmentsOK: &tr}, "no attachments_ok": {MaxPayload: 10 << 10},
		"negative": {MaxPayload: -1, AttachmentsOK: &tr},
	} {
		b, err := l.Prepare(env, 0, 0)
		var ce *CapabilityError
		if b != nil || !errors.As(err, &ce) || !errors.Is(err, ErrInvalidCapability) || !errors.Is(err, ErrLocal) {
			t.Errorf("%s: %q %v", name, b, err)
		}
	}
	if _, err := (PromptEndpointMetadata{AttachmentsOK: true}).Limits().Prepare(env, 0, 0); !errors.Is(err, ErrInvalidCapability) {
		t.Errorf("zero max_payload from a host: %v", err)
	}
	a, err := NewAgentInfo(ServiceInfo{Name: "agents", Metadata: map[string]string{"agent": "a", "owner": "o", "protocol_version": "0.3"},
		Endpoints: []EndpointInfo{{Name: "prompt", Subject: "agents.prompt.a.o.n"}, {Name: "status", Subject: "agents.status.a.o.n"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.PromptLimits(); !errors.Is(err, ErrInvalidCapability) {
		t.Errorf("AgentInfo.PromptLimits: %v", err)
	}
}

// Audit finding 8: encoders never emit bad JSON.
func TestEncoders_ExtraMustBeOneValidJSONValue(t *testing.T) {
	bad := map[string]json.RawMessage{
		"oops": []byte("oops"), "two": []byte(`1 2`), "empty": []byte(``), "trailing": []byte(`{"a":1}x`),
	}
	for name, raw := range bad {
		x := map[string]json.RawMessage{name: raw}
		_, err1 := Envelope{Prompt: "x", Extra: x}.Encode()
		_, err2 := EncodeChunk(ResponseChunk{Text: "x", Extra: x})
		_, err3 := EncodeChunk(QueryChunk{ID: "i", ReplySubject: "_INBOX.x", Prompt: "p", Extra: x})
		_, err4 := EncodeHeartbeat(Heartbeat{Agent: "a", Owner: "o", InstanceID: "i", TS: "2026-04-21T14:23:01Z", IntervalS: 1, Extra: x})
		_, err5 := Attachment{Filename: "f", Extra: x}.MarshalJSON()
		_, err6 := (&ServiceError{Code: 400, Body: &ErrorBody{Extra: x}}).Payload()
		_, err7 := Envelope{Prompt: "x", Attachments: []Attachment{{Filename: "f", Extra: x}}}.Encode()
		for i, err := range []error{err1, err2, err3, err4, err5, err6, err7} {
			if !errors.Is(err, ErrInvalidExtra) {
				t.Errorf("%s encoder %d: %v", name, i+1, err)
			}
		}
	}
}

func TestAttachment_MarshalReservesOwnedKeys(t *testing.T) {
	a := Attachment{Filename: "f", Content: "", Extra: map[string]json.RawMessage{
		"filename": json.RawMessage(`"evil"`), "content": json.RawMessage(`"evil"`), "k": json.RawMessage(`1`)}}
	b, err := json.Marshal(a)
	if err != nil || string(b) != `{"filename":"f","content":"","k":1}` {
		t.Fatal(string(b), err)
	}
}

// Audit finding 9: the code sentinels are immutable and matched by code.
func TestServiceError_CodeSentinels(t *testing.T) {
	err := error(&ServiceError{Code: 429, Description: "slow down"})
	wrapped := errors.Join(errors.New("ctx"), err)
	if !errors.Is(wrapped, ErrRateLimited) || errors.Is(wrapped, ErrInternal) {
		t.Fatal("sentinel match")
	}
	if code, ok := CodeOf(wrapped); !ok || code != 429 {
		t.Fatal(code, ok)
	}
	if !IsCode(wrapped, 429) || IsCode(wrapped, 500) || IsCode(errors.New("x"), 429) || IsCode(nil, 429) {
		t.Fatal("IsCode")
	}
	if !IsCode(&ServiceError{Code: 418}, 418) {
		t.Fatal("IsCode outside the §9.2 table")
	}
	// A pointer target is not a code pattern.
	if errors.Is(err, &ServiceError{Code: 429}) {
		t.Fatal("errors.Is matched a *ServiceError pattern")
	}
	// The sentinels are comparable values, not pointers to shared structs.
	for _, s := range []error{ErrBadRequest, ErrUnauthorized, ErrForbidden, ErrNotFound, ErrConflict, ErrRateLimited, ErrInternal} {
		if reflect.TypeOf(s).Kind() == reflect.Pointer {
			t.Errorf("%v is a pointer", s)
		}
		if !strings.HasPrefix(s.Error(), "service error ") {
			t.Errorf("%v", s)
		}
	}
}

// Audit finding 10: no public path builds an invalid subject.
func TestSubject_NoInvalidSubjectFromPublicAPI(t *testing.T) {
	s, err := NewAgentSubject("a", "o", "n")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Subject(Verb("bogus")); err == nil || got != "" || !errors.Is(err, ErrInvalidSubject) {
		t.Errorf("bogus verb: %q %v", got, err)
	}
	if got, err := s.Subject(VerbStatus); err != nil || got != "agents.status.a.o.n" {
		t.Errorf("status: %q %v", got, err)
	}
	var zero AgentSubject
	if _, err := zero.Subject(VerbPrompt); err == nil {
		t.Error("zero subject built a subject")
	}
	if zero.Prompt() != "" || zero.Heartbeat() != "" || !zero.IsZero() {
		t.Error("zero value builds a subject")
	}
	// A subject cannot be assembled from bad tokens by any constructor.
	if _, err := NewAgentSubject("a", "bad.name", "n"); err == nil {
		t.Error("bad owner accepted")
	}
	if _, _, err := ParseSubject("agents.prompt.a.bad name.n"); err == nil {
		t.Error("parse accepted a bad token")
	}
	// Every exported AgentSubject field is gone: the struct is only readable through methods.
	typ := reflect.TypeOf(AgentSubject{})
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).IsExported() {
			t.Errorf("exported field %s", typ.Field(i).Name)
		}
	}
}

// Audit finding 11: SAP:L504-L511. Required values are validated; ts is RFC 3339 UTC.
func TestHeartbeat_RequiredValuesValidated(t *testing.T) {
	const ts = "2026-04-21T14:23:01Z"
	mk := func(agent, owner, id, stamp string) string {
		b, _ := json.Marshal(map[string]any{"agent": agent, "owner": owner, "instance_id": id, "ts": stamp, "interval_s": 1})
		return string(b)
	}
	for name, in := range map[string]string{
		"empty agent": mk("", "o", "i", ts), "empty owner": mk("a", "", "i", ts),
		"empty instance": mk("a", "o", "", ts), "empty ts": mk("a", "o", "i", ""),
		"ts word": mk("a", "o", "i", "t"), "ts no zone": mk("a", "o", "i", "2026-04-21T14:23:01"),
		"ts date only": mk("a", "o", "i", "2026-04-21"), "ts offset": mk("a", "o", "i", "2026-04-21T14:23:01+02:00"),
	} {
		if _, err := DecodeHeartbeat([]byte(in)); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s decoded: %v", name, err)
		}
	}
	for _, stamp := range []string{ts, "2026-04-21T14:23:01.123456Z", "2026-04-21T14:23:01+00:00"} {
		h, err := DecodeHeartbeat([]byte(mk("a", "o", "i", stamp)))
		if err != nil || h.TS != stamp {
			t.Errorf("%s: %q %v", stamp, h.TS, err)
			continue
		}
		if _, err := h.Time(); err != nil {
			t.Errorf("%s: %v", stamp, err)
		}
		// The original text round-trips.
		b, err := EncodeHeartbeat(h)
		if back, _ := DecodeHeartbeat(b); err != nil || back.TS != stamp {
			t.Errorf("round trip %s: %v", stamp, err)
		}
	}
	h := Heartbeat{Agent: "a", Owner: "o", InstanceID: "i", TS: "t", IntervalS: 1}
	if _, err := EncodeHeartbeat(h); err == nil {
		t.Error("EncodeHeartbeat accepted ts \"t\"")
	}
}
