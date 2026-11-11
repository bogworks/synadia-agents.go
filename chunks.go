// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"encoding/json"
	"fmt"
	"time"
)

// Chunk types. See SAP §6.2.
const (
	ChunkResponse = "response" // content from the agent (§6.3)
	ChunkStatus   = "status"   // lifecycle signal (§6.4)
	ChunkQuery    = "query"    // mid-stream query (§7.1)
)

// StatusAck is the one defined status value. A host MUST emit exactly one ack
// status chunk as the first message on the reply subject, before any response
// or query chunk and before any latency-inducing work; it resets the caller's
// inactivity timeout. Callers MUST silently ignore other status values. See SAP
// §6.4.
const StatusAck = "ack"

// DefaultInactivityTimeout is the per-stream inactivity timeout SAP
// recommends: 60 seconds since the last observed chunk. See SAP §6.6.
const DefaultInactivityTimeout = 60 * time.Second

// Chunk is a decoded stream message: a [ResponseChunk], a [StatusChunk], a
// [QueryChunk] or an [UnknownChunk]. See SAP §6.2.
type Chunk interface {
	// Type returns the wire discriminator.
	Type() string
	isChunk()
}

// ResponseChunk is content from the agent. On the wire the data is a bare
// string or an object {text, attachments}; a caller accepts both, and
// concatenates Text in publication order. See SAP §6.3.
type ResponseChunk struct {
	Text        string
	Attachments []Attachment
	// Extra holds unknown fields of the data object, verbatim (SAP §6.6).
	Extra map[string]json.RawMessage
}

// StatusChunk is a lifecycle signal; Status is "ack" or a value to ignore. See
// SAP §6.4.
type StatusChunk struct{ Status string }

// QueryChunk is a mid-stream question. The caller publishes exactly one reply
// to ReplySubject, in the form of an [Envelope] (SAP §7.2). Each concurrent
// query has its own ReplySubject. See SAP §7.1.
type QueryChunk struct {
	// ID is an opaque correlation identifier, SHOULD be a UUID.
	ID string
	// ReplySubject is a fresh subject (typically _INBOX.xxx) for one reply.
	ReplySubject string
	// Prompt is the question text.
	Prompt      string
	Attachments []Attachment
	// Extra holds unknown fields of the data object, verbatim (SAP §6.6).
	Extra map[string]json.RawMessage
}

// UnknownChunk is a well-formed chunk of a type this SDK does not know. A
// caller MUST silently ignore it and keep consuming the stream; a relay keeps
// Data verbatim. See SAP §6.2, §6.6.
type UnknownChunk struct {
	ChunkType string
	Data      json.RawMessage
}

// Type returns ChunkResponse.
func (ResponseChunk) Type() string { return ChunkResponse }

// Type returns ChunkStatus.
func (StatusChunk) Type() string { return ChunkStatus }

// Type returns ChunkQuery.
func (QueryChunk) Type() string { return ChunkQuery }

// Type returns the unrecognized wire type.
func (u UnknownChunk) Type() string { return u.ChunkType }

func (ResponseChunk) isChunk() {}
func (StatusChunk) isChunk()   {}
func (QueryChunk) isChunk()    {}
func (UnknownChunk) isChunk()  {}

// DecodeChunk parses a non-terminating stream message body, a JSON object
// {"type", "data"}. Plain text is not accepted on the response side. A type it
// does not know, with a data field, yields an [UnknownChunk] and no error. A
// body that is not a JSON object with a string type and a data field, or a
// known type whose data is malformed,
// is a [*ProtocolError]. `data` is required on every non-terminating chunk, so
// a body without it is a protocol error whatever its type, and null is not a
// string. The caller has already handled the terminator and
// error headers (see [IsTerminator], [ParseServiceError]). See SAP §6.2-§6.4,
// §7.1.
func DecodeChunk(data []byte) (Chunk, error) {
	obj, err := splitObject(data)
	if err != nil {
		return nil, &ProtocolError{Reason: "chunk body must be a JSON object", Err: err}
	}
	typ, ok := stringField(obj, "type")
	if !ok {
		return nil, &ProtocolError{Reason: "chunk body missing string `type` field"}
	}
	raw, present := obj["data"]
	if !present {
		return nil, &ProtocolError{Reason: "chunk body missing required `data` field (SAP §6.2)"}
	}
	switch typ {
	case ChunkResponse:
		return decodeResponse(raw)
	case ChunkStatus:
		s, ok := rawString(raw)
		if !ok {
			return nil, &ProtocolError{Reason: "`status` chunk `data` must be a string"}
		}
		return StatusChunk{Status: s}, nil
	case ChunkQuery:
		return decodeQuery(raw)
	}
	return UnknownChunk{ChunkType: typ, Data: raw}, nil
}

func decodeResponse(raw json.RawMessage) (Chunk, error) {
	if s, ok := rawString(raw); ok {
		return ResponseChunk{Text: s}, nil
	}
	obj, err := splitObject(raw)
	if err != nil {
		return nil, &ProtocolError{Reason: "`response` chunk `data` must be a string or object"}
	}
	text, ok := stringField(obj, "text")
	if !ok {
		return nil, &ProtocolError{Reason: "`response` chunk `data.text` must be a string"}
	}
	rc := ResponseChunk{Text: text, Extra: withoutKeys(obj, "text", "attachments")}
	if a, present := obj["attachments"]; present {
		atts, err := decodeAttachmentList(a, "`response` chunk")
		if err != nil {
			return nil, &ProtocolError{Reason: err.Error()}
		}
		rc.Attachments = atts
	}
	return rc, nil
}

func decodeQuery(raw json.RawMessage) (Chunk, error) {
	obj, err := splitObject(raw)
	if err != nil {
		return nil, &ProtocolError{Reason: "`query` chunk `data` must be an object"}
	}
	q := QueryChunk{Extra: withoutKeys(obj, "id", "reply_subject", "prompt", "attachments")}
	for _, f := range []struct {
		key string
		dst *string
	}{{"id", &q.ID}, {"reply_subject", &q.ReplySubject}, {"prompt", &q.Prompt}} {
		v, ok := stringField(obj, f.key)
		if !ok {
			return nil, &ProtocolError{Reason: fmt.Sprintf("`query` chunk missing string `%s`", f.key)}
		}
		*f.dst = v
	}
	if a, present := obj["attachments"]; present {
		atts, err := decodeAttachmentList(a, "`query` chunk")
		if err != nil {
			return nil, &ProtocolError{Reason: err.Error()}
		}
		q.Attachments = atts
	}
	return q, nil
}

// EncodeChunk serializes a chunk for publication. A [ResponseChunk] without
// attachments or extra fields uses the bare-string shorthand of SAP §6.3, and
// otherwise the object form. A [QueryChunk] needs an ID and a ReplySubject
// that passes [ValidatePublishSubject]. An [UnknownChunk] is written back
// verbatim, for a relay.
func EncodeChunk(c Chunk) ([]byte, error) {
	switch c := c.(type) {
	case ResponseChunk:
		if len(c.Attachments) == 0 && len(c.Extra) == 0 {
			return marshalObject([]field{{"type", ChunkResponse, true}, {"data", c.Text, true}}, nil)
		}
		fields := []field{{"text", c.Text, true}}
		if len(c.Attachments) > 0 {
			fields = append(fields, field{"attachments", c.Attachments, true})
		}
		data, err := marshalObject(fields, c.Extra, "attachments")
		if err != nil {
			return nil, err
		}
		return marshalObject([]field{{"type", ChunkResponse, true}, {"data", json.RawMessage(data), true}}, nil)
	case StatusChunk:
		return marshalObject([]field{{"type", ChunkStatus, true}, {"data", c.Status, true}}, nil)
	case QueryChunk:
		if c.ID == "" {
			return nil, protoErr("query chunk needs an id")
		}
		if err := ValidatePublishSubject(c.ReplySubject); err != nil {
			return nil, err
		}
		fields := []field{{"id", c.ID, true}, {"reply_subject", c.ReplySubject, true}, {"prompt", c.Prompt, true}}
		if len(c.Attachments) > 0 {
			fields = append(fields, field{"attachments", c.Attachments, true})
		}
		data, err := marshalObject(fields, c.Extra, "attachments")
		if err != nil {
			return nil, err
		}
		return marshalObject([]field{{"type", ChunkQuery, true}, {"data", json.RawMessage(data), true}}, nil)
	case UnknownChunk:
		data := c.Data
		if len(data) == 0 {
			data = json.RawMessage("null")
		}
		return marshalObject([]field{{"type", c.ChunkType, true}, {"data", data, true}}, nil)
	}
	return nil, protoErr("unsupported chunk %T", c)
}

// AckChunk returns the encoded ack status chunk, {"type":"status","data":"ack"},
// the first message of every reply stream. See SAP §6.4.
func AckChunk() []byte {
	b, _ := EncodeChunk(StatusChunk{Status: StatusAck})
	return b
}
