// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Sentinel errors for errors.Is. Several concrete error types below match them.
var (
	// ErrLocal matches every refusal the SDK makes locally, before anything is
	// published: an invalid token, an empty prompt, attachments the endpoint
	// does not accept, an oversize payload, a version mismatch. It never
	// matches an error reported by a peer, so a caller can tell the two apart.
	// See SAP §5.4.
	ErrLocal = errors.New("agents: local refusal")

	// ErrInvalidToken matches a [*TokenError] for a token that breaks a SAP
	// MUST. See SAP §2.2.
	ErrInvalidToken = errors.New("agents: invalid subject token")

	// ErrTokenNotRecommended matches a [*TokenError] from strict validation, for
	// a token that breaks a SAP SHOULD. See SAP §2.2.
	ErrTokenNotRecommended = errors.New("agents: subject token outside the recommended form")

	// ErrInvalidSubject matches a [*SubjectError]. See SAP §2.
	ErrInvalidSubject = errors.New("agents: invalid subject")

	// ErrPromptEmpty is returned when a prompt is empty. See SAP §5.1.
	ErrPromptEmpty error = &localError{msg: "prompt must be non-empty (SAP §5.1)"}

	// ErrInvalidUTF8 is returned when text that the protocol carries as UTF-8
	// (a prompt, a JSON string) is not valid UTF-8. The SDK refuses it rather
	// than let an encoder replace the bytes. See SAP §5.1.
	ErrInvalidUTF8 error = &localError{msg: "text is not valid UTF-8 (SAP §5.1)"}

	// ErrInvalidExtra matches an encoder failure caused by an Extra value that
	// is not exactly one valid JSON value.
	ErrInvalidExtra = errors.New("agents: extra field is not valid JSON")

	// ErrInvalidCapability matches a [*CapabilityError]: a prompt endpoint
	// whose max_payload or attachments_ok is missing or malformed. See SAP
	// §2.1.
	ErrInvalidCapability = errors.New("agents: invalid prompt endpoint capability metadata")

	// ErrAttachmentsNotSupported is returned when attachments are present and
	// the endpoint declared attachments_ok false. See SAP §5.4.
	ErrAttachmentsNotSupported error = &localError{msg: "this agent's prompt endpoint does not accept attachments (attachments_ok=false, SAP §5.4)"}

	// ErrPayloadTooLarge matches a [*PayloadTooLargeError]. See SAP §5.4.
	ErrPayloadTooLarge = errors.New("agents: payload exceeds max_payload")

	// ErrInvalidAttachment matches a [*AttachmentError]. See SAP §5.2.
	ErrInvalidAttachment = errors.New("agents: invalid attachment")

	// ErrVersionMismatch matches a [*VersionError]. See SAP §11.2.
	ErrVersionMismatch = errors.New("agents: protocol version not supported")

	// ErrProtocol matches a [*ProtocolError]: a received payload that could not
	// be interpreted as the protocol defines it.
	ErrProtocol = errors.New("agents: protocol violation")

	// ErrNotAgent matches a [*NotAgentError]: a service record that is not an
	// SAP agent. See SAP §3.1, §4.3.
	ErrNotAgent = errors.New("agents: not an SAP agent")

	// ErrStreamStalled matches a [*StreamStalledError]. See SAP §6.6.
	ErrStreamStalled = errors.New("agents: stream stalled")
)

// localError is a plain local refusal.
type localError struct{ msg string }

func (e *localError) Error() string        { return e.msg }
func (e *localError) Is(target error) bool { return target == ErrLocal }

// TokenError reports a subject token that a SAP rule refuses. See SAP §2.2.
type TokenError struct {
	// Role names the token's place: "agent", "owner", "name", "subject token"
	// or "instance id".
	Role string
	// Token is the offending value.
	Token string
	// Reason says which rule it breaks.
	Reason string
	// Recommended is true when the rule is a SAP SHOULD (strict mode), false
	// when it is a MUST.
	Recommended bool
}

func (e *TokenError) Error() string {
	return fmt.Sprintf("invalid %s token %q: %s", e.Role, e.Token, e.Reason)
}

// Is matches [ErrLocal] and either [ErrInvalidToken] or [ErrTokenNotRecommended].
func (e *TokenError) Is(target error) bool {
	if target == ErrLocal {
		return true
	}
	if e.Recommended {
		return target == ErrTokenNotRecommended
	}
	return target == ErrInvalidToken
}

// SubjectError reports a string that is not a valid SAP subject. See SAP §2.
type SubjectError struct {
	Subject string
	Reason  string
}

func (e *SubjectError) Error() string {
	return fmt.Sprintf("invalid subject %q: %s", e.Subject, e.Reason)
}

// Is matches [ErrInvalidSubject] and [ErrLocal].
func (e *SubjectError) Is(target error) bool {
	return target == ErrInvalidSubject || target == ErrLocal
}

// CapabilityError reports a prompt endpoint whose required capability
// metadata (max_payload, attachments_ok) is missing or malformed, which makes
// the endpoint unusable for a caller: SAP makes both mandatory and makes the
// caller enforce them. See SAP §2.1, §5.4.
type CapabilityError struct {
	// Key is the metadata key, MetaMaxPayload or MetaAttachmentsOK.
	Key    string
	Reason string
}

func (e *CapabilityError) Error() string {
	return "prompt endpoint metadata " + e.Key + ": " + e.Reason
}

// Is matches [ErrInvalidCapability] and [ErrLocal].
func (e *CapabilityError) Is(target error) bool {
	return target == ErrInvalidCapability || target == ErrLocal
}

// PayloadTooLargeError reports a request that does not fit a size limit.
// Connection is false when the limit is the endpoint's max_payload, which
// counts the payload, plus the Agent-Sender header where SAP §13.5.1 says it
// does, and true when it is the caller's own connection limit, which counts
// headers and payload. HeaderBytes is the size of that header; zero when none
// is sent. See SAP §5.4.
type PayloadTooLargeError struct {
	Limit       int64
	Actual      int64
	HeaderBytes int64
	Connection  bool
}

func (e *PayloadTooLargeError) Error() string {
	if e.Connection {
		return fmt.Sprintf("payload size %d bytes plus headers %d bytes exceeds the connection max_payload of %d bytes",
			e.Actual, e.HeaderBytes, e.Limit)
	}
	if e.HeaderBytes > 0 {
		return fmt.Sprintf("payload size %d bytes plus headers %d bytes exceeds max_payload of %d bytes (SAP §5.4)",
			e.Actual, e.HeaderBytes, e.Limit)
	}
	return fmt.Sprintf("payload size %d bytes exceeds max_payload of %d bytes (SAP §5.4)", e.Actual, e.Limit)
}

// Is matches [ErrPayloadTooLarge] and [ErrLocal].
func (e *PayloadTooLargeError) Is(target error) bool {
	return target == ErrPayloadTooLarge || target == ErrLocal
}

// AttachmentError reports an attachment that breaks SAP §5.2. Index is the
// attachment's 0-based place in its array, or -1 when not known.
type AttachmentError struct {
	Index  int
	Reason string
}

func (e *AttachmentError) Error() string {
	if e.Index < 0 {
		return "invalid attachment: " + e.Reason
	}
	return fmt.Sprintf("invalid attachment[%d]: %s", e.Index, e.Reason)
}

// Is matches [ErrInvalidAttachment] and [ErrLocal].
func (e *AttachmentError) Is(target error) bool {
	return target == ErrInvalidAttachment || target == ErrLocal
}

// VersionError reports a protocol version the SDK refuses. See SAP §11.
type VersionError struct {
	// Agent is the version string as the agent declared it.
	Agent  string
	Reason string
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("protocol version %q not supported: %s (SAP §11.2)", e.Agent, e.Reason)
}

// Is matches [ErrVersionMismatch] and [ErrLocal].
func (e *VersionError) Is(target error) bool {
	return target == ErrVersionMismatch || target == ErrLocal
}

// ProtocolError reports a received payload that could not be interpreted as
// the protocol defines it (malformed JSON, a chunk without its required
// fields). It is a fault of the peer, not a SAP error response (see
// [ServiceError]).
type ProtocolError struct {
	Reason string
	Err    error
}

func (e *ProtocolError) Error() string {
	if e.Err != nil {
		return "protocol error: " + e.Reason + ": " + e.Err.Error()
	}
	return "protocol error: " + e.Reason
}

// Unwrap returns the underlying cause, if any.
func (e *ProtocolError) Unwrap() error { return e.Err }

// Is matches [ErrProtocol].
func (e *ProtocolError) Is(target error) bool { return target == ErrProtocol }

func protoErr(format string, args ...any) error {
	return &ProtocolError{Reason: fmt.Sprintf(format, args...)}
}

// NotAgentError reports a service record that is not an SAP agent. See SAP
// §3.1, §4.3.
type NotAgentError struct{ Reason string }

func (e *NotAgentError) Error() string { return "not an SAP agent: " + e.Reason }

// Is matches [ErrNotAgent].
func (e *NotAgentError) Is(target error) bool { return target == ErrNotAgent }

// StreamStalledError reports that a stream was silent for longer than the
// inactivity timeout. See SAP §6.6.
type StreamStalledError struct{ Timeout time.Duration }

func (e *StreamStalledError) Error() string {
	return fmt.Sprintf("stream stalled: no chunk received within %s (SAP §6.6)", e.Timeout)
}

// Is matches [ErrStreamStalled].
func (e *StreamStalledError) Is(target error) bool { return target == ErrStreamStalled }

// Status codes of the SAP error taxonomy. Agents MUST use codes from this
// table. See SAP §9.2.
const (
	CodeBadRequest   = 400 // malformed request
	CodeUnauthorized = 401 // authentication required
	CodeForbidden    = 403 // authenticated but not authorized
	CodeNotFound     = 404 // not found
	CodeConflict     = 409 // conflicts with current agent state
	CodeRateLimited  = 429 // rate limited
	CodeInternal     = 500 // internal error
)

// KnownCode reports whether code is in the SAP §9.2 taxonomy.
func KnownCode(code int) bool {
	switch code {
	case CodeBadRequest, CodeUnauthorized, CodeForbidden, CodeNotFound,
		CodeConflict, CodeRateLimited, CodeInternal:
		return true
	}
	return false
}

// ErrorBody is the optional JSON body of an error response. Error is the
// stable machine-readable code (lowercase snake_case recommended), Message the
// human detail, and Extra any other fields, kept verbatim. See SAP §9.1.
type ErrorBody struct {
	Error   string
	Message string
	Extra   map[string]json.RawMessage
}

// ServiceError is an error response as SAP §9 defines it: a status code, the
// short Nats-Service-Error description and an optional JSON body. It is the
// error a caller sees for a remote failure and the form a host builds to
// send one. Test the code with errors.Is and the Err... sentinels, [CodeOf] or
// [IsCode]; use errors.As to read the fields.
type ServiceError struct {
	// Code is the Nats-Service-Error-Code header value. See SAP §9.1.
	Code int
	// Description is the Nats-Service-Error header value. See SAP §9.1.
	Description string
	// Body is the parsed JSON body, nil when the body was empty, not valid
	// JSON or not an object. See SAP §9.1.
	Body *ErrorBody
}

// codeError is an immutable sentinel for one status code. Being a plain
// comparable value, it cannot be changed through a shared pointer.
type codeError int

func (c codeError) Error() string {
	switch int(c) {
	case CodeBadRequest:
		return "service error 400: bad request"
	case CodeUnauthorized:
		return "service error 401: authentication required"
	case CodeForbidden:
		return "service error 403: forbidden"
	case CodeNotFound:
		return "service error 404: not found"
	case CodeConflict:
		return "service error 409: conflict"
	case CodeRateLimited:
		return "service error 429: rate limited"
	case CodeInternal:
		return "service error 500: internal error"
	}
	return "service error " + strconv.Itoa(int(c))
}

// Sentinels for the §9.2 codes. errors.Is(err, agents.ErrRateLimited) is true
// for any [*ServiceError] in err's chain with code 429. They are immutable.
// See SAP §9.2.
var (
	ErrBadRequest   error = codeError(CodeBadRequest)
	ErrUnauthorized error = codeError(CodeUnauthorized)
	ErrForbidden    error = codeError(CodeForbidden)
	ErrNotFound     error = codeError(CodeNotFound)
	ErrConflict     error = codeError(CodeConflict)
	ErrRateLimited  error = codeError(CodeRateLimited)
	ErrInternal     error = codeError(CodeInternal)
)

// NewServiceError builds an error response with the given code and
// description. The description is sanitized for use in a header (see
// [SanitizeErrorDescription]). It does not require a code from the §9.2
// table; use [KnownCode] where that is a rule.
func NewServiceError(code int, description string) *ServiceError {
	return &ServiceError{Code: code, Description: SanitizeErrorDescription(description)}
}

// Message returns the human-readable detail: the body's message when the body
// carries one, else the header description. See SAP §9.1.
func (e *ServiceError) Message() string {
	if e.Body != nil && e.Body.Message != "" {
		return e.Body.Message
	}
	return e.Description
}

func (e *ServiceError) Error() string {
	return fmt.Sprintf("service error %d: %s", e.Code, e.Message())
}

// Is reports whether target is one of the Err... code sentinels with e's code,
// which lets errors.Is(err, agents.ErrRateLimited) test a code.
func (e *ServiceError) Is(target error) bool {
	t, ok := target.(codeError)
	return ok && int(t) == e.Code
}

// Headers returns the NATS headers of an error response: the code and the
// sanitized description. See SAP §9.1.
func (e *ServiceError) Headers() map[string][]string {
	return map[string][]string{
		HeaderServiceErrorCode: {strconv.Itoa(e.Code)},
		HeaderServiceError:     {SanitizeErrorDescription(e.Description)},
	}
}

// Payload returns the JSON body of the error response, or nil when there is no
// body. See SAP §9.1.
func (e *ServiceError) Payload() ([]byte, error) {
	if e.Body == nil {
		return nil, nil
	}
	return marshalObject([]field{
		{"error", e.Body.Error, e.Body.Error != ""},
		{"message", e.Body.Message, e.Body.Message != ""},
	}, e.Body.Extra, "error", "message")
}

// IsCode reports whether err's chain holds a [*ServiceError] with the given
// code. It also tests codes outside the §9.2 table.
func IsCode(err error, code int) bool {
	c, ok := CodeOf(err)
	return ok && c == code
}

// CodeOf returns the status code of the first [*ServiceError] in err's chain.
func CodeOf(err error) (int, bool) {
	var se *ServiceError
	if errors.As(err, &se) {
		return se.Code, true
	}
	return 0, false
}

// maxErrorDescLen is the longest header description the SDK sends (SDK policy,
// as the official host SDK does); richer detail belongs in the body.
const maxErrorDescLen = 200

// SanitizeErrorDescription makes a description safe for a NATS header: line
// breaks are flattened (a newline in a header value truncates the following
// headers) and the result is capped at 200 bytes on a character boundary. SDK
// policy. See SAP §9.1.
func SanitizeErrorDescription(desc string) string {
	parts := strings.FieldsFunc(desc, func(r rune) bool { return r == '\n' || r == '\r' })
	kept := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	flat := strings.ToValidUTF8(strings.Join(kept, " | "), "\ufffd")
	if len(flat) <= maxErrorDescLen {
		return flat
	}
	cut := maxErrorDescLen - 3
	for cut > 0 && !utf8.RuneStart(flat[cut]) {
		cut--
	}
	return flat[:cut] + "..."
}

// ParseServiceError reads an error response from a received message's headers
// and body. It reports false when the message carries no
// Nats-Service-Error-Code header, that is, when it is not an error signal. A
// code that is not a number reads as 500. When the body is empty, is not
// valid JSON, or is not a JSON object, the header description stands alone
// and Body is nil. Callers MUST do this check on every message before they
// interpret its body. See SAP §9.1, §6.5.
func ParseServiceError(headers map[string][]string, body []byte) (*ServiceError, bool) {
	codeStr, ok := lookupHeader(headers, HeaderServiceErrorCode)
	if !ok {
		return nil, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(codeStr))
	if err != nil {
		code = CodeInternal
	}
	desc, _ := lookupHeader(headers, HeaderServiceError)
	se := &ServiceError{Code: code, Description: desc}
	if len(body) > 0 {
		se.Body = parseErrorBody(body)
	}
	return se, true
}

func parseErrorBody(body []byte) *ErrorBody {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	obj, err := splitObject(body)
	if err != nil {
		return nil
	}
	eb := &ErrorBody{}
	for k, v := range obj {
		switch k {
		case "error", "message":
			if s, ok := rawString(v); ok {
				if k == "error" {
					eb.Error = s
				} else {
					eb.Message = s
				}
				continue
			}
		}
		if eb.Extra == nil {
			eb.Extra = map[string]json.RawMessage{}
		}
		eb.Extra[k] = v
	}
	return eb
}

// SizeError reports a max_payload value that is not in the SAP §2.1 grammar.
type SizeError struct {
	Input  string
	Reason string
}

func (e *SizeError) Error() string { return fmt.Sprintf("invalid size %q: %s", e.Input, e.Reason) }
