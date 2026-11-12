// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller

import (
	"errors"
	"fmt"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
)

// localError is a refusal this package makes without publishing anything. It
// matches [agents.ErrLocal], so a program can tell it from an error a peer
// reported (REQ-007). See SAP §5.4.
type localError struct{ msg string }

func (e *localError) Error() string        { return e.msg }
func (e *localError) Is(target error) bool { return target == agents.ErrLocal }

var (
	// ErrClosed is returned by a [Client] that has been closed or drained, and
	// ends every stream that was still running when the client closed.
	ErrClosed = errors.New("caller: client closed")

	// ErrStreamConsumed is yielded when [Stream.Events] is ranged over a second
	// time: a stream has one consumer and is single-use.
	ErrStreamConsumed = errors.New("caller: stream already consumed")

	// ErrAgentNotFound is returned by [Client.LookupInstance] and [Client.Find]
	// when no agent answers or the answer is not an SAP agent. See SAP §4.2.
	ErrAgentNotFound = errors.New("caller: agent not found")

	// ErrSlowConsumer matches a [*SlowConsumerError]: the program did not read a
	// stream fast enough and its bounded buffer overflowed (SDK policy).
	ErrSlowConsumer = errors.New("caller: stream consumer too slow")

	// ErrMaxWait matches a [*MaxWaitError]: the stream ran longer than its
	// ceiling without ending (SDK policy; SAP §6.6 requires only the inactivity
	// timeout).
	ErrMaxWait = errors.New("caller: stream exceeded its maximum wait")

	// ErrReservedHeader is returned, before anything is published, when an
	// outgoing-header hook or interceptor sets a header SAP defines or relies
	// on (SDK policy, REQ-016). It matches [agents.ErrLocal].
	ErrReservedHeader error = &localError{"caller: header is reserved by the protocol and cannot be set"}

	// ErrInvalidHeader is returned, before anything is published, for a header
	// name or value that cannot be framed safely. It matches [agents.ErrLocal].
	ErrInvalidHeader error = &localError{"caller: invalid header name or value"}

	// ErrReservedField is returned, before anything is published, when an
	// interceptor tries to set the envelope's "prompt" or "attachments" field as
	// an extra. It matches [agents.ErrLocal]. See SAP §5.6.
	ErrReservedField error = &localError{"caller: envelope field is not an extra"}

	// ErrQueryAlreadyReplied matches a [*QueryAlreadyRepliedError]. See SAP §7.2.
	ErrQueryAlreadyReplied = errors.New("caller: query already answered")

	// ErrQueryReplyRefused matches a [*QueryReplyRefusedError]: the reply
	// subject of a query was refused by the query reply policy and nothing was
	// published. It matches [agents.ErrLocal]. See SAP §7.1.
	ErrQueryReplyRefused = errors.New("caller: query reply subject refused")

	// ErrUntrustedAgent is returned by [Agent.Prompt] and [Agent.Status], before
	// anything is published, when the client has a trust policy
	// ([WithTrustPolicy]) and the agent was not admitted by a discovery that
	// verified its id_sig and applied the policy. It matches [agents.ErrLocal].
	ErrUntrustedAgent error = &localError{"caller: agent is not admitted by the client's trust policy"}

	// ErrNoSigner is returned by the signing helpers when the client has no
	// signer. It matches [agents.ErrLocal].
	ErrNoSigner error = &localError{"caller: no signer is configured"}
)

// SlowConsumerError reports a stream whose bounded buffer overflowed. The
// stream ends after the items that were buffered before the overflow.
type SlowConsumerError struct {
	// Buffered is the configured bound in messages; Bytes the bound in bytes.
	Buffered int
	Bytes    int64
}

func (e *SlowConsumerError) Error() string {
	return fmt.Sprintf("caller: stream consumer too slow (more than %d messages or %d bytes unread)", e.Buffered, e.Bytes)
}

// Is matches [ErrSlowConsumer].
func (e *SlowConsumerError) Is(target error) bool { return target == ErrSlowConsumer }

// MaxWaitError reports a stream that did not end within its ceiling.
type MaxWaitError struct{ MaxWait time.Duration }

func (e *MaxWaitError) Error() string {
	return fmt.Sprintf("caller: stream exceeded its maximum wait of %s", e.MaxWait)
}

// Is matches [ErrMaxWait].
func (e *MaxWaitError) Is(target error) bool { return target == ErrMaxWait }

// QueryAlreadyRepliedError is returned by the second call to [Query.Reply] or
// [Query.ReplyEnvelope]: a query takes exactly one reply. It is a local refusal
// that matches [ErrQueryAlreadyReplied] and [agents.ErrLocal]. See SAP §7.2.
type QueryAlreadyRepliedError struct{ ID string }

func (e *QueryAlreadyRepliedError) Error() string {
	return "caller: query already answered (SAP §7.2 allows exactly one reply)"
}

// Is matches [ErrQueryAlreadyReplied] and [agents.ErrLocal].
func (e *QueryAlreadyRepliedError) Is(target error) bool {
	return target == ErrQueryAlreadyReplied || target == agents.ErrLocal
}

// localWrapError marks err as a local refusal without changing its text or its own
// matches.
type localWrapError struct{ err error }

func (e *localWrapError) Error() string        { return e.err.Error() }
func (e *localWrapError) Unwrap() error        { return e.err }
func (e *localWrapError) Is(target error) bool { return target == agents.ErrLocal }

func local(err error) error { return &localWrapError{err} }

// QueryReplyRefusedError is returned by [Query.Reply] and [Query.ReplyEnvelope]
// when the query reply policy refuses the agent-supplied reply subject. The
// reply is not published and does not use up the query's one reply. It
// matches [ErrQueryReplyRefused] and [agents.ErrLocal], and unwraps to the
// policy's own error.
type QueryReplyRefusedError struct {
	// Subject is the refused reply subject.
	Subject string
	// Err is the policy's reason.
	Err error
}

func (e *QueryReplyRefusedError) Error() string {
	return fmt.Sprintf("caller: query reply subject %q refused: %v", e.Subject, e.Err)
}

// Unwrap returns the policy's error.
func (e *QueryReplyRefusedError) Unwrap() error { return e.Err }

// Is matches [ErrQueryReplyRefused] and [agents.ErrLocal].
func (e *QueryReplyRefusedError) Is(target error) bool {
	return target == ErrQueryReplyRefused || target == agents.ErrLocal
}
