// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// Envelope is the payload of a prompt request: a prompt and optional
// attachments. The same shape is the reply to a mid-stream query (SAP §7.2).
// On the wire it is plain UTF-8 text (shorthand for a prompt alone) or a JSON
// object. See SAP §5.1.
//
// Unknown top-level fields are kept in Extra when decoding and written back
// when encoding, so a relay preserves them; a field the protocol defines
// ("prompt", "attachments") in Extra is never written. See SAP §5.6.
type Envelope struct {
	// Prompt is the UTF-8 prompt text; it MUST be non-empty. See SAP §5.1.
	Prompt string
	// Attachments are zero or more files. See SAP §5.2.
	Attachments []Attachment
	// Extra holds unknown top-level fields, verbatim. See SAP §5.6.
	Extra map[string]json.RawMessage
}

// Validate applies the local MUSTs of a sender: a non-empty, valid UTF-8
// prompt (SAP §5.1, [ErrPromptEmpty], [ErrInvalidUTF8]) and well-formed attachments (SAP §5.2,
// [*AttachmentError]). Size and capability checks need the endpoint: see
// [PromptLimits].
func (e Envelope) Validate() error {
	if e.Prompt == "" {
		return ErrPromptEmpty
	}
	if !utf8.ValidString(e.Prompt) {
		return ErrInvalidUTF8
	}
	for i, a := range e.Attachments {
		if err := a.validateAt(i); err != nil {
			return err
		}
	}
	return nil
}

// Encode validates the envelope and serializes it as the JSON form of SAP
// §5.1. Programmatic callers always send JSON; the plain-text shorthand is
// for command-line use (see [EncodePlainPrompt]).
func (e Envelope) Encode() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	fields := []field{{"prompt", e.Prompt, true}}
	if len(e.Attachments) > 0 {
		fields = append(fields, field{"attachments", e.Attachments, true})
	}
	return marshalObject(fields, e.Extra, "attachments")
}

// EncodedSize returns the byte size of the encoded envelope, the number the
// max_payload check of SAP §5.4 compares. It fails as [Envelope.Encode] does.
func (e Envelope) EncodedSize() (int, error) {
	b, err := e.Encode()
	return len(b), err
}

// EncodePlainPrompt returns the plain-text shorthand of SAP §5.1 for a prompt
// without attachments: the prompt's bytes. A text that begins (after ASCII
// whitespace) with "{" would be read as JSON by the receiver (SAP §5.3), so
// it is refused; use [Envelope.Encode] for it. A prompt that is not valid
// UTF-8 is refused with [ErrInvalidUTF8].
func EncodePlainPrompt(prompt string) ([]byte, error) {
	if prompt == "" {
		return nil, ErrPromptEmpty
	}
	if !utf8.ValidString(prompt) {
		return nil, ErrInvalidUTF8
	}
	if startsWithBrace(prompt) {
		return nil, &AttachmentError{Index: -1, Reason: "plain text starting with '{' would be read as JSON (SAP §5.3); use the JSON envelope"}
	}
	return []byte(prompt), nil
}

// startsWithBrace implements the §5.3 discrimination: skip the ASCII
// whitespace 0x09, 0x0A, 0x0D, 0x20 and test for '{'.
func startsWithBrace[T string | []byte](s T) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case 0x09, 0x0a, 0x0d, 0x20:
			continue
		}
		return s[i] == '{'
	}
	return false
}

// DecodeEnvelope parses a received prompt payload by the SAP §5.3 rule: a
// zero-byte payload is invalid; after skipping ASCII whitespace (0x09, 0x0A,
// 0x0D, 0x20), a payload that starts with "{" is a JSON envelope and must
// parse as one, not fall back to text; any other payload is plain text and
// becomes the prompt unchanged. A plain-text payload that is not valid UTF-8,
// and a JSON prompt string that is not, are rejected rather than repaired.
//
// A JSON envelope needs a non-empty string prompt (SAP §5.1), an attachments
// field that is an array of {filename, content} objects with strict base64
// content (SAP §5.2), and keeps every unknown field in Extra (SAP §5.6). The
// attachments are not otherwise judged: an unusual filename is passed on, and
// a host decides what to do with it (see [SafeFileName]). Every failure is a
// [*ServiceError] with code 400, ready for a host to send; its description
// never repeats the payload. See SAP §5.3, §9.2.
func DecodeEnvelope(data []byte) (Envelope, error) {
	if len(data) == 0 {
		return Envelope{}, badRequest("zero-byte request payload")
	}
	if !startsWithBrace(data) {
		if !utf8.Valid(data) {
			return Envelope{}, badRequest("plain-text prompt is not valid UTF-8")
		}
		return Envelope{Prompt: string(data)}, nil
	}
	obj, err := splitObject(data)
	if err != nil {
		return Envelope{}, badRequest("malformed JSON envelope")
	}
	if raw, present := obj["prompt"]; present && !utf8.Valid(raw) {
		return Envelope{}, badRequest("envelope prompt is not valid UTF-8")
	}
	prompt, ok := stringField(obj, "prompt")
	if !ok {
		return Envelope{}, badRequest("envelope missing required string prompt")
	}
	if prompt == "" {
		return Envelope{}, badRequest("envelope prompt must be non-empty")
	}
	env := Envelope{Prompt: prompt, Extra: withoutKeys(obj, "prompt", "attachments")}
	if raw, present := obj["attachments"]; present {
		atts, err := decodeAttachmentList(raw, "envelope")
		if err != nil {
			return Envelope{}, badRequest(err.Error())
		}
		for _, a := range atts {
			if a.Filename == "" {
				return Envelope{}, badRequest("attachment has empty filename")
			}
			if _, ok := Base64DecodedSize(a.Content); !ok {
				return Envelope{}, badRequest("attachment has invalid base64 content (standard alphabet, padded, no whitespace)")
			}
		}
		if len(atts) > 0 {
			env.Attachments = atts
		}
	}
	return env, nil
}

func badRequest(desc string) *ServiceError {
	return &ServiceError{Code: CodeBadRequest, Description: desc}
}

// decodeAttachmentList reads a JSON array of attachment objects.
func decodeAttachmentList(raw json.RawMessage, ctx string) ([]Attachment, error) {
	var items []json.RawMessage
	if isJSONNull(raw) || json.Unmarshal(raw, &items) != nil {
		return nil, fmt.Errorf("%s attachments must be an array", ctx)
	}
	atts := make([]Attachment, len(items))
	for i, it := range items {
		if err := json.Unmarshal(it, &atts[i]); err != nil {
			return nil, fmt.Errorf("%s attachment[%d]: %w", ctx, i, err)
		}
	}
	return atts, nil
}

// PromptLimits is what a caller enforces locally before it publishes, read
// from a prompt endpoint's metadata (SAP §2.1). Both values are mandatory in
// the metadata and callers MUST enforce them (SAP:L90-L92, SAP:L286-L293), so
// get one from [EndpointInfo.PromptLimits] or [PromptEndpointMetadata.Limits];
// [PromptLimits.Prepare] refuses to work with a value that lacks either.
type PromptLimits struct {
	// MaxPayload is the endpoint's max_payload in bytes; it must be positive.
	MaxPayload int64
	// AttachmentsOK is attachments_ok; it must be set.
	AttachmentsOK *bool
}

// Validate reports a [*CapabilityError] when MaxPayload is not positive or
// AttachmentsOK is unset: the endpoint's required metadata was missing or
// malformed, so no request may be published to it. See SAP §2.1, §5.4.
func (l PromptLimits) Validate() error {
	if l.MaxPayload <= 0 {
		return &CapabilityError{Key: MetaMaxPayload, Reason: "missing or not a positive size"}
	}
	if l.AttachmentsOK == nil {
		return &CapabilityError{Key: MetaAttachmentsOK, Reason: "missing or not \"true\" or \"false\""}
	}
	return nil
}

// CheckAttachments fails with [ErrAttachmentsNotSupported] when any attachment
// is present and the endpoint declared attachments_ok false. See SAP §5.4.
func (l PromptLimits) CheckAttachments(count int) error {
	if count > 0 && l.AttachmentsOK != nil && !*l.AttachmentsOK {
		return ErrAttachmentsNotSupported
	}
	return nil
}

// CheckSize makes the two size checks of SAP §5.4, each ignored when its limit
// is zero. The endpoint check compares the final encoded payload with the
// endpoint's MaxPayload: the payload alone (SAP:L84-L92, SAP:L286-L293), plus
// senderHeaderBytes, the framed size of the Agent-Sender header block, which
// SAP §13.5.1 (SAP:L887) says counts toward it; pass 0 when no such header is
// sent. The connection check is separate: connMaxPayload is the caller's own
// server limit, which NATS applies to headers and payload together. A failure
// is a [*PayloadTooLargeError] whose Connection field says which limit it was;
// the endpoint limit is checked first.
func (l PromptLimits) CheckSize(payloadSize, senderHeaderBytes int, connMaxPayload int64) error {
	total := int64(payloadSize) + int64(senderHeaderBytes)
	if l.MaxPayload > 0 && total > l.MaxPayload {
		return &PayloadTooLargeError{Limit: l.MaxPayload, Actual: int64(payloadSize), HeaderBytes: int64(senderHeaderBytes)}
	}
	if connMaxPayload > 0 && total > connMaxPayload {
		return &PayloadTooLargeError{Limit: connMaxPayload, Actual: int64(payloadSize), HeaderBytes: int64(senderHeaderBytes), Connection: true}
	}
	return nil
}

// Prepare runs every local check of SAP §5.4 for a request: it requires valid
// limits ([PromptLimits.Validate]), validates the envelope, checks the
// attachments against attachments_ok, encodes it, and checks the size as
// [PromptLimits.CheckSize] does. It returns the bytes to publish. The error
// matches [ErrLocal].
func (l PromptLimits) Prepare(env Envelope, senderHeaderBytes int, connMaxPayload int64) ([]byte, error) {
	if err := l.Validate(); err != nil {
		return nil, err
	}
	if err := env.Validate(); err != nil {
		return nil, err
	}
	if err := l.CheckAttachments(len(env.Attachments)); err != nil {
		return nil, err
	}
	b, err := env.Encode()
	if err != nil {
		return nil, err
	}
	if err := l.CheckSize(len(b), senderHeaderBytes, connMaxPayload); err != nil {
		return nil, err
	}
	return b, nil
}

// CheckInbound is the host's optional server-side enforcement of the same
// limits on a received request: attachments sent although attachments_ok is
// false, and a payload over max_payload, are each a 400. SAP makes server-side
// enforcement a MAY (§5.4) and lists both as 400 (§9.2). It returns nil or a
// [*ServiceError].
func (l PromptLimits) CheckInbound(payloadSize int, env Envelope) error {
	if l.AttachmentsOK != nil && !*l.AttachmentsOK && len(env.Attachments) > 0 {
		return badRequest("attachments are not accepted by this endpoint")
	}
	if l.MaxPayload > 0 && int64(payloadSize) > l.MaxPayload {
		return badRequest("request exceeds max_payload")
	}
	return nil
}
