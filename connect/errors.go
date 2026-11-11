// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package connect

import (
	"errors"
	"fmt"
)

// ErrContext is matched by every error this package returns for a context,
// URL, source or credential file it cannot use: a missing or malformed context,
// an invalid name, an unusable URL, an unreadable key file or an ambiguous
// [Source]. Use errors.Is. See parity row 1.13; outside SAP.
var ErrContext = errors.New("connect: invalid NATS context")

// ErrNoUserSeed is matched (together with [ErrContext]) when a signer is
// requested but the credential source holds no user seed: the context or
// source has no creds, nkey or user_seed, or only a user JWT. See SAP §13.6.
var ErrNoUserSeed = errors.New("connect: no user seed to sign with")

// ContextError reports why a context, URL or source could not be used. Its
// message never contains a seed, credential, token, password or URL userinfo.
// It matches [ErrContext] under errors.Is and unwraps to the underlying
// cause, if any (for example a file system error). See parity row 1.13.
type ContextError struct {
	// Context is the context name, or "" when no context is involved.
	Context string
	// Msg describes the problem and is free of secrets.
	Msg string
	// Err is the underlying cause, or nil.
	Err error
}

// Error implements error.
func (e *ContextError) Error() string {
	s := "connect: "
	if e.Context != "" {
		s += fmt.Sprintf("NATS context %q: ", e.Context)
	}
	s += e.Msg
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap returns the underlying cause.
func (e *ContextError) Unwrap() error { return e.Err }

// Is reports whether target is [ErrContext].
func (e *ContextError) Is(target error) bool { return target == ErrContext }

func ctxErr(name, format string, args ...any) *ContextError {
	return &ContextError{Context: name, Msg: fmt.Sprintf(format, args...)}
}

func ctxErrWrap(name string, cause error, format string, args ...any) *ContextError {
	return &ContextError{Context: name, Msg: fmt.Sprintf(format, args...), Err: cause}
}
