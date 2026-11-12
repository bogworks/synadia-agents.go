// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct{ now atomic.Int64 }

func newFakeClock(t time.Time) *fakeClock {
	c := &fakeClock{}
	c.now.Store(t.UnixNano())
	return c
}
func (c *fakeClock) Now() time.Time          { return time.Unix(0, c.now.Load()).UTC() }
func (c *fakeClock) Advance(d time.Duration) { c.now.Add(int64(d)) }

func TestNonceSet_RecordAndReplay(t *testing.T) {
	clock := newFakeClock(testNow)
	s := NewNonceSet(NonceSetOptions{Now: clock.Now})
	if got := s.Record("u", "n", testNow); got != Recorded {
		t.Fatalf("first: %v", got)
	}
	if got := s.Record("u", "n", testNow); got != Replayed {
		t.Fatalf("second: %v", got)
	}
	if !s.Seen("u", "n") || s.Seen("u", "other") || s.Seen("v", "n") {
		t.Fatal("Seen is keyed by (user, nonce)")
	}
	if got := s.Record("v", "n", testNow); got != Recorded {
		t.Fatalf("same nonce, other user: %v", got)
	}
}

func TestNonceSet_EntriesExpireAtTimestampPlusWindow(t *testing.T) {
	clock := newFakeClock(testNow)
	s := NewNonceSet(NonceSetOptions{Now: clock.Now})
	// A header 29 s in the future is legal and must be refused on replay at
	// arrival + 31 s: the entry lives until ts + window, not arrival + window.
	ts := testNow.Add(29 * time.Second)
	s.Record("u", "n", ts)
	clock.Advance(31 * time.Second)
	if !s.Seen("u", "n") {
		t.Fatal("expired before ts + window")
	}
	clock.Advance(29 * time.Second) // now = ts + 31 s
	if s.Seen("u", "n") {
		t.Fatal("still present after ts + window")
	}
	if s.Len() != 0 {
		t.Fatalf("Len = %d after expiry", s.Len())
	}
}

func TestNonceSet_BoundedAndFailsClosed(t *testing.T) {
	var buf lockedBuffer
	clock := newFakeClock(testNow)
	s := NewNonceSet(NonceSetOptions{MaxEntries: 3, Now: clock.Now, Logger: slog.New(slog.NewTextHandler(&buf, nil))})
	for i := 0; i < 3; i++ {
		if got := s.Record("u", fmt.Sprint("n", i), testNow); got != Recorded {
			t.Fatalf("fill %d: %v", i, got)
		}
	}
	if got := s.Record("u", "n3", testNow); got != SetFull {
		t.Fatalf("overflow: %v", got)
	}
	if s.Len() != 3 {
		t.Fatalf("Len = %d, want 3 (nothing dropped)", s.Len())
	}
	for i := 0; i < 3; i++ {
		if got := s.Record("u", fmt.Sprint("n", i), testNow); got != Replayed {
			t.Fatalf("old entry %d: %v (an old entry was dropped)", i, got)
		}
	}
	// One log line per overflow episode, never the nonce.
	s.Record("u", "n4", testNow)
	if n := strings.Count(buf.String(), "nonce set is full"); n != 1 {
		t.Fatalf("logged %d times: %q", n, buf.String())
	}
	if strings.Contains(buf.String(), "n3") {
		t.Fatal("nonce logged")
	}
	// After expiry, room again, and the warning re-arms.
	clock.Advance(time.Minute)
	if got := s.Record("u", "n5", clock.Now()); got != Recorded {
		t.Fatalf("after expiry: %v", got)
	}
}

func TestNonceSet_SafeUnderRace(t *testing.T) {
	s := NewNonceSet(NonceSetOptions{MaxEntries: 500})
	now := time.Now()
	var wg sync.WaitGroup
	var recorded atomic.Int64
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(_ int) {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				// Half of the keys are shared across goroutines.
				key := fmt.Sprint("n", i%200)
				if s.Record("u", key, now) == Recorded {
					recorded.Add(1)
				}
				s.Seen("u", key)
				s.Len()
			}
		}(g)
	}
	wg.Wait()
	if recorded.Load() != 200 {
		t.Fatalf("%d distinct keys recorded, want exactly 200", recorded.Load())
	}
}
