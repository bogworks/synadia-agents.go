// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"errors"
	"fmt"

	agents "github.com/bogworks/synadia-agents.go"
)

var (
	// ErrRejected matches every [*RejectError]. See SAP §9.2.
	ErrRejected = errors.New("host: request rejected")

	// ErrInvalidCode is returned by [Reject] for a code outside the SAP §9.2
	// table (400, 401, 403, 404, 409, 429, 500). Upstream accepts any code
	// from 400 to 599; this SDK sends only the codes the taxonomy defines
	// (SAP §9.2: agents MUST use codes from the table).
	ErrInvalidCode = errors.New("host: status code is not in the SAP error taxonomy")

	// ErrStreamClosed is returned by a [Responder] call made after the
	// terminator was sent, or when the request has no reply subject. The host
	// publishes nothing after the terminator (SAP §6.5).
	ErrStreamClosed = errors.New("host: response stream already terminated")

	// ErrStatusChunk is returned by [Responder.SendChunk] for a status chunk:
	// the host owns the ack and the keep-alives (SAP §6.4).
	ErrStatusChunk = errors.New("host: status chunks are sent by the host, not the handler")

	// ErrQueryTimeout is returned by [Responder.Ask] when the caller does not
	// answer in time. The handler decides what follows (SAP §7.3).
	ErrQueryTimeout = errors.New("host: no reply to the query in time")

	// ErrNextCalledTwice is returned by the next function of an [Interceptor]
	// called a second time.
	ErrNextCalledTwice = errors.New("host: interceptor called next more than once")

	// ErrNextNotCalled is the failure recorded when an [Interceptor] returns
	// nil without calling next; the request is answered 500.
	ErrNextNotCalled = errors.New("host: interceptor returned without calling next")

	// ErrStarted is returned by [Host.Start] on a host that already started,
	// stopped or is stopping.
	ErrStarted = errors.New("host: already started or stopped")

	// ErrNotStarted is returned when a call needs a started host.
	ErrNotStarted = errors.New("host: not started")

	// ErrEndpointOverlap is wrapped with [ErrConfig] by [New] when an extra
	// endpoint's subject or pattern could match the prompt, status or heartbeat
	// subject of the instance, which would hand unclassified requests to
	// application code (SAP §13.7).
	ErrEndpointOverlap = errors.New("host: endpoint subject overlaps a protocol subject")

	// ErrConfig matches the error [New] returns for an invalid configuration.
	ErrConfig = errors.New("host: invalid configuration")
)

// RejectError refuses a request with a chosen SAP §9.2 code. A handler or an
// interceptor returns it to answer with that code instead of the generic 500.
// See SAP §9.1, §9.2.
type RejectError struct {
	// Code is one of 400, 401, 403, 404, 409, 429, 500 (SAP §9.2). A value
	// outside the table is answered as 500.
	Code int
	// Description is the short single-line text of the Nats-Service-Error
	// header; it is sanitized and capped at 200 bytes on the wire.
	Description string
	// Body is the optional JSON body of the error response (SAP §9.1).
	Body *agents.ErrorBody
}

// Error implements error.
func (e *RejectError) Error() string {
	if e == nil {
		return "host: request rejected: nil"
	}
	return fmt.Sprintf("host: request rejected: %d %s", e.Code, e.Description)
}

// Is matches ErrRejected. The sentinel of the code (agents.ErrBadRequest and
// its siblings) matches through Unwrap.
func (e *RejectError) Is(target error) bool { return target == ErrRejected }

// Unwrap returns the *agents.ServiceError the rejection answers with, so
// agents.IsCode and errors.Is(err, agents.ErrForbidden) work on it.
func (e *RejectError) Unwrap() error {
	if e == nil {
		return nil
	}
	se := agents.NewServiceError(e.Code, nonEmpty(e.Description, "request rejected"))
	se.Body = e.Body
	return se
}

// Reject returns a [*RejectError] with the code and description. It fails
// with [ErrInvalidCode] when the code is not in the SAP §9.2 table. See SAP
// §9.2.
func Reject(code int, description string) error {
	if !agents.KnownCode(code) {
		return fmt.Errorf("%w: %d", ErrInvalidCode, code)
	}
	return &RejectError{Code: code, Description: description}
}

// RejectWithBody is Reject with the optional JSON error body of SAP §9.1.
func RejectWithBody(code int, description string, body agents.ErrorBody) error {
	if !agents.KnownCode(code) {
		return fmt.Errorf("%w: %d", ErrInvalidCode, code)
	}
	return &RejectError{Code: code, Description: description, Body: &body}
}
