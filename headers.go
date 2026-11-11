// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import "strings"

// Header names SAP defines.
const (
	// HeaderServiceErrorCode carries the numeric status code of an error
	// response, as a string. See SAP §9.1.
	HeaderServiceErrorCode = "Nats-Service-Error-Code"

	// HeaderServiceError carries the short human-readable description of an
	// error response. See SAP §9.1.
	HeaderServiceError = "Nats-Service-Error"

	// HeaderAgentSender carries the sender identity of a request or heartbeat
	// (optional extension). The name is matched case-sensitively. See SAP
	// §13.5.
	HeaderAgentSender = "Agent-Sender"

	// HeaderRequestInfo is the header the NATS server stamps on a request
	// that crosses an account boundary. It is always a claim, never proof of
	// identity. See SAP §13.1.
	HeaderRequestInfo = "Nats-Request-Info"
)

// Header is the shape of NATS message headers (nats.Header and http.Header
// both have this underlying type, so either is assignable).
type Header = map[string][]string

// HeaderValue returns the first value of the header with exactly this name
// and whether it is present. The match is case-sensitive, as SAP requires of
// Agent-Sender (SAP §13.5.1) and as nats.go does.
func HeaderValue(h Header, name string) (string, bool) {
	v, ok := h[name]
	if !ok {
		return "", false
	}
	if len(v) == 0 {
		return "", true
	}
	return v[0], true
}

// lookupHeader is HeaderValue with a case-insensitive fallback. It is used for
// the two error headers only: a receiver that finds the code under a
// differently-cased key still reads it as an error signal (leniency on
// receive, SDK policy).
func lookupHeader(h Header, name string) (string, bool) {
	if v, ok := HeaderValue(h, name); ok {
		return v, true
	}
	for k, vs := range h {
		if strings.EqualFold(k, name) {
			if len(vs) == 0 {
				return "", true
			}
			return vs[0], true
		}
	}
	return "", false
}

// IsErrorSignal reports whether a received message carries the error code
// header, that is, whether it is an error response and not content. Callers
// MUST inspect every message for it before interpreting the body. See SAP
// §9.1, §12.
func IsErrorSignal(h Header) bool {
	_, ok := lookupHeader(h, HeaderServiceErrorCode)
	return ok
}

// IsTerminator reports whether a received message is the end-of-stream
// terminator: a zero-byte body carrying no NATS headers. An error-headered
// message with an empty body is an error signal, not the terminator. See SAP
// §6.5, §9.3.
func IsTerminator(data []byte, h Header) bool {
	return len(data) == 0 && len(h) == 0
}
