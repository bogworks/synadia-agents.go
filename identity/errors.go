// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"errors"
	"fmt"
)

// Sentinel errors for the wire outcomes of SAP §13.7. A returned *Error
// matches exactly one of them with [errors.Is].
var (
	// ErrMalformedHeader marks a malformed Agent-Sender header, answered with
	// 400. See SAP §13.5.1 and §13.7.
	ErrMalformedHeader = errors.New("identity: malformed Agent-Sender header")

	// ErrVerification marks a signed header that failed verification (bad
	// signature, subject, timestamp or replayed nonce), answered with 401. See
	// SAP §13.7.
	ErrVerification = errors.New("identity: sender verification failed")

	// ErrSignatureRequired marks a request that is not verified at an endpoint
	// that requires a verified sender, answered with 401. See SAP §13.9.
	ErrSignatureRequired = errors.New("identity: signature required")

	// ErrSenderRefused marks a verified sender that the receiver's admission
	// hook refused, answered with 403. See SAP §13.7.3.
	ErrSenderRefused = errors.New("identity: sender refused")

	// ErrAdmission marks a failure of the admission hook itself, answered with
	// 500 (SDK policy: a hook that fails does not serve the request).
	ErrAdmission = errors.New("identity: admission hook failed")
)

// Configuration errors of live verification. They are returned by
// [VerifyLive] and [NewGate] and never answered on the wire.
var (
	// ErrNoNonceSet is returned when live verification is configured without a
	// replay set. SAP §13.7 requires one for live requests.
	ErrNoNonceSet = errors.New("identity: live verification requires a NonceSet (replay protection)")

	// ErrNonceWindow is returned when a [NonceSet]'s window is shorter than the
	// replay window of the verifier or gate that uses it: a replay inside the
	// accepted window would then pass once its entry expired. See SAP §13.7.
	ErrNonceWindow = errors.New("identity: NonceSet window is shorter than the replay window")
)

// Wire descriptions. The description that goes on the wire is generic, so the
// refusal does not tell a forger which check failed. The detail, which may
// name the check, is for logs only. See SAP §9.2.
const (
	// DescriptionMalformed is the wire description of a 400.
	DescriptionMalformed = "malformed Agent-Sender header"
	// DescriptionRejected is the wire description of a 401 for a failed check
	// and of a 403.
	DescriptionRejected = "sender rejected"
	// DescriptionSignatureRequired is the wire description of a 401 when the
	// endpoint requires a signature the request does not carry.
	DescriptionSignatureRequired = "signature required"
	// DescriptionServerError is the wire description of a 500.
	DescriptionServerError = "server error"
)

// Error is a sender-identity refusal. Code is the SAP §9.2 status (400, 401,
// 403 or 500), Description the generic text for the error header on the wire,
// and Detail the specific reason, for logs only. It never contains a seed,
// signature or payload. See SAP §13.7.
type Error struct {
	Code        int
	Description string
	Detail      string

	kind error
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("identity: %d %s", e.Code, e.Description)
	}
	return fmt.Sprintf("identity: %d %s: %s", e.Code, e.Description, e.Detail)
}

// Unwrap returns the sentinel for the outcome, so that errors.Is(err,
// ErrMalformedHeader) and the like work.
func (e *Error) Unwrap() error { return e.kind }

func newError(code int, kind error, description, detail string) *Error {
	return &Error{Code: code, Description: description, Detail: detail, kind: kind}
}

func malformed(detail string) *Error {
	return newError(400, ErrMalformedHeader, DescriptionMalformed, detail)
}

func rejected(format string, args ...any) *Error {
	return newError(401, ErrVerification, DescriptionRejected, fmt.Sprintf(format, args...))
}
