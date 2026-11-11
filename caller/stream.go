// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/nats-io/nats.go"
)

// errorGrace is how long a stream that has yielded an error waits for the
// terminator that follows it (SAP §9.3), so the trailing message is consumed
// and not counted as a late reply. A lost terminator costs this much.
const errorGrace = 250 * time.Millisecond

// Stream is the reply stream of one prompt: chunks in publication order, ended
// by the zero-byte terminator (SAP §6.5). Range over [Stream.Events] on one
// goroutine; [Stream.Close] and [Stream.Terminated] are safe from any other.
//
// A stream lives as long as the context given to [Agent.Prompt]: canceling it
// drops the reply registration, and the sequence yields the context's error.
// There is no cancel signal on the wire, and the agent is not told (SAP §6.7).
type Stream struct {
	c       *Client
	r       *route
	ctx     context.Context
	subject string

	inactivity time.Duration
	maxWait    time.Duration
	queryPol   func(string) error // nil: the client's policy

	consumed   atomic.Bool
	terminated atomic.Bool

	closeOnce sync.Once
	closed    chan struct{}
	stopCtx   func() bool

	// The deadlines run from the publication. A deadline that passes releases
	// the route and records its error; the consumer, however late, drains what
	// is queued and then gets that error, never a fresh timeout.
	tmu       sync.Mutex
	inactT    *time.Timer
	maxT      *time.Timer
	expiredBy error
	expired   chan struct{} // closed when a deadline passed

	errMu sync.Mutex
	err   error
}

// Subject returns the subject the prompt was published to.
func (s *Stream) Subject() string { return s.subject }

// Terminated reports whether the zero-byte terminator was observed: the agent
// ended the stream. It is the done signal; the SDK yields no synthetic item for
// it (SAP §6.4 makes that a MAY). A stream that ended by timeout, error,
// cancellation or Close is not terminated.
func (s *Stream) Terminated() bool { return s.terminated.Load() }

// Err returns the error that ended the sequence, or nil.
func (s *Stream) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *Stream) setErr(err error) {
	s.errMu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.errMu.Unlock()
}

// startTimers arms the inactivity and max-wait deadlines. Prompt calls it just
// before it publishes.
func (s *Stream) startTimers() {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	s.inactT = time.AfterFunc(s.inactivity, func() { s.expire(&agents.StreamStalledError{Timeout: s.inactivity}) })
	s.maxT = time.AfterFunc(s.maxWait, func() { s.expire(&MaxWaitError{MaxWait: s.maxWait}) })
}

// stopTimers disarms both deadlines for good.
func (s *Stream) stopTimers() {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	if s.expiredBy == nil {
		s.expiredBy = errTimersStopped
	}
	if s.inactT != nil {
		s.inactT.Stop()
	}
	if s.maxT != nil {
		s.maxT.Stop()
	}
}

// errTimersStopped marks a stream whose deadlines were disarmed before any
// passed; it is never returned.
var errTimersStopped = errors.New("caller: stream timers stopped")

// arrived restarts the inactivity deadline: any message that reaches the route,
// the ack included, shows the stream alive (SAP §6.4, §6.6), whether or not the
// program has read it yet.
func (s *Stream) arrived() {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	if s.expiredBy == nil && s.inactT != nil {
		s.inactT.Reset(s.inactivity)
	}
}

// expire records the deadline that passed and releases the route, so a stream
// that is never ranged does not hold it for ever.
func (s *Stream) expire(err error) {
	s.tmu.Lock()
	if s.expiredBy != nil {
		s.tmu.Unlock()
		return
	}
	s.expiredBy = err
	s.inactT.Stop()
	s.maxT.Stop()
	close(s.expired)
	s.tmu.Unlock()
	s.r.close()
}

// expiry returns the error of the deadline that passed, or nil.
func (s *Stream) expiry() error {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	if s.expiredBy == errTimersStopped {
		return nil
	}
	return s.expiredBy
}

// Close drops the reply registration and ends the sequence without error.
// Buffered items are discarded. It sends nothing on the wire (SAP §6.7),
// is idempotent, and is safe to call while another goroutine ranges over
// Events.
func (s *Stream) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.release()
	})
	return nil
}

// release unregisters the route and stops the context watcher.
func (s *Stream) release() {
	s.stopTimers()
	s.r.close()
	if s.stopCtx != nil {
		s.stopCtx()
	}
}

// Events returns the stream's items in wire order: acks, responses and
// queries. It yields at most one error, last, and then ends. Errors:
//
//   - *[agents.ServiceError] for an error-headered message (SAP §9.1); the
//     terminator that follows is consumed;
//   - *[agents.StreamStalledError] when nothing arrived for the inactivity
//     timeout (SAP §6.6);
//   - *[MaxWaitError] at the stream's ceiling;
//   - *[SlowConsumerError] when the program fell too far behind;
//   - nats.ErrNoResponders when no agent was listening on the subject;
//   - the context's error when it ended; [ErrClosed] when the client closed.
//
// The sequence ends without error at the terminator ([Stream.Terminated]) or
// after [Stream.Close]. Leaving the loop early closes the stream. A stream has
// one consumer: a second call yields [ErrStreamConsumed] once.
func (s *Stream) Events() iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		if !s.consumed.CompareAndSwap(false, true) {
			yield(nil, ErrStreamConsumed)
			return
		}
		defer s.Close()
		s.run(yield)
	}
}

func (s *Stream) run(yield func(Event, error) bool) {
	over := s.r.over
	overflow := false
	expired := s.expired
	timedOut := false

	fail := func(err error) {
		s.setErr(err)
		yield(nil, err)
	}
	for {
		// Close wins over buffered items, as a closed client must end its streams.
		select {
		case <-s.c.done:
			fail(ErrClosed)
			return
		case <-s.closed:
			return
		default:
		}
		if overflow && len(s.r.ch) == 0 {
			fail(&SlowConsumerError{Buffered: s.r.maxMsgs, Bytes: s.r.maxBytes})
			return
		}
		if timedOut && len(s.r.ch) == 0 {
			if err := s.expiry(); err != nil {
				fail(err)
				return
			}
		}
		select {
		case <-s.c.done:
			fail(ErrClosed)
			return
		case <-s.closed:
			return
		case <-s.ctx.Done():
			fail(s.ctx.Err())
			return
		case <-over:
			over = nil
			overflow = true
		case <-expired:
			// What was queued before the deadline is still read; the error
			// follows it.
			expired = nil
			timedOut = true
		case m := <-s.r.ch:
			s.r.take(m)
			select {
			case <-s.closed: // Close wins over a reply that was ready with it
				return
			default:
			}
			ev, end, err := s.handle(m)
			switch {
			case err != nil:
				s.setErr(err)
				if !yield(nil, err) {
					return
				}
				s.awaitTerminator()
				return
			case end:
				return
			case ev != nil:
				if !yield(ev, nil) {
					return
				}
			}
		}
	}
}

// awaitTerminator consumes the terminator that follows an error, for a short
// grace, so it is not counted as a late reply.
func (s *Stream) awaitTerminator() {
	t := time.NewTimer(errorGrace)
	defer t.Stop()
	for {
		select {
		case m := <-s.r.ch:
			s.r.take(m)
			if agents.IsTerminator(m.Data, m.Header) {
				s.terminated.Store(true)
				return
			}
		case <-t.C:
			return
		case <-s.closed:
			return
		case <-s.c.done:
			return
		case <-s.ctx.Done():
			return
		}
	}
}

// handle interprets one message. It returns the event to yield (nil for a
// message the SDK consumes), whether the stream ended at the terminator, or the
// error that ends it. SAP §9 requires the error headers to be read before the
// body of every message.
func (s *Stream) handle(m *nats.Msg) (ev Event, end bool, err error) {
	if isNoResponders(m) {
		return nil, false, fmt.Errorf("caller: prompt %s: %w", s.subject, nats.ErrNoResponders)
	}
	if se, ok := agents.ParseServiceError(m.Header, m.Data); ok {
		return nil, false, se
	}
	if agents.IsTerminator(m.Data, m.Header) {
		s.terminated.Store(true)
		return nil, true, nil
	}
	chunk, derr := agents.DecodeChunk(m.Data)
	if derr != nil {
		// A malformed chunk of a known type, or a body that is no chunk at all,
		// is dropped and the stream goes on (SAP §6.6 for unknown types; the
		// official callers do the same here). It is counted and logged at debug.
		s.c.malformed.Add(1)
		s.c.log.Debug("dropped a malformed chunk", slog.String("subject", s.subject))
		return nil, false, nil
	}
	switch ch := chunk.(type) {
	case agents.ResponseChunk:
		return Response{meta: meta{m.Header}, Text: ch.Text, Attachments: ch.Attachments, Extra: ch.Extra}, false, nil
	case agents.StatusChunk:
		if ch.Status == agents.StatusAck {
			return Status{meta: meta{m.Header}, Value: ch.Status}, false, nil
		}
		return nil, false, nil // unrecognized status values are ignored (SAP §6.4)
	case agents.QueryChunk:
		if verr := agents.ValidatePublishSubject(ch.ReplySubject); verr != nil {
			s.c.malformed.Add(1)
			s.c.log.Debug("dropped a query with an unusable reply subject", slog.String("subject", s.subject))
			return nil, false, nil
		}
		return &Query{
			meta: meta{m.Header}, ID: ch.ID, ReplySubject: ch.ReplySubject, Prompt: ch.Prompt,
			Attachments: ch.Attachments, Extra: ch.Extra, c: s.c, policy: s.queryPol,
		}, false, nil
	default: // agents.UnknownChunk: silently ignored (SAP §6.2, §6.6)
		return nil, false, nil
	}
}

// isNoResponders reports the server's 503 status for a publish nobody was
// listening to.
func isNoResponders(m *nats.Msg) bool {
	return len(m.Data) == 0 && m.Header.Get("Status") == "503"
}
