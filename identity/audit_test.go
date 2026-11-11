// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// SAP §13.3.1 gives the account token no length cap: any nonempty name over
// [A-Za-z0-9_-] parses. MaxAccountLen is a sizing allowance only.
func TestAgentID_LongAccountNamesAreValid(t *testing.T) {
	for _, n := range []int{1, 63, 64, 65, 200, 4096} {
		account := strings.Repeat("a", n)
		text := account + "." + alicePub
		id, err := ParseAgentID(text)
		if err != nil {
			t.Fatalf("ParseAgentID with a %d-byte account name: %v", n, err)
		}
		if id.Account() != account || id.User() != alicePub || string(id) != text {
			t.Errorf("%d: round trip got %q/%q", n, id.Account(), id.User())
		}
		if _, err := NewAgentID(account, alicePub); err != nil {
			t.Errorf("NewAgentID with a %d-byte account name: %v", n, err)
		}
	}
	if _, err := ParseAgentID(strings.Repeat("a", 100) + ".bad"); !errors.Is(err, ErrInvalidAgentID) {
		t.Errorf("a bad user must still be refused: %v", err)
	}
}

// The wire-size helpers must match the header actually encoded, including JSON
// escaping of quotes, backslashes, control characters and invalid UTF-8.
func TestHeaderSizes_CountJSONEscaping(t *testing.T) {
	id := newID(t, testAcct, alicePub)
	names := []string{
		`say "hi"`, `back\slash`, "tab\there", "nul\x00byte", "\x1f", "line\nbreak",
		"  ", "<&>", "bad\xffutf8", strings.Repeat(`"\`, 40), "ünï名前",
	}
	subjects := []string{subj, `a"b\c`, "x\ty"}
	for _, name := range names {
		for _, subject := range subjects {
			for _, signed := range []bool{false, true} {
				h := SenderHeader{V: HeaderVersion, Account: id.Account(), User: id.User(), Name: name}
				if signed {
					h.Sub, h.TS = subject, strings.Repeat("T", tsLength)
					h.Nonce, h.Sig = strings.Repeat("N", nuidLength), strings.Repeat("S", sigBase64Length)
				}
				real := HeaderFramingBytes + len(h.Encode())
				if got := ExpectedHeaderWireBytes(id, name, subject, signed); got != real {
					t.Errorf("name %q subject %q signed=%v: ExpectedHeaderWireBytes %d, encoded header %d",
						name, subject, signed, got, real)
				}
			}
			// The widest header any signer sends: a 64-byte nonce and a name
			// account at the sizing allowance.
			h := SenderHeader{V: HeaderVersion, Account: strings.Repeat("a", MaxAccountLen), User: alicePub, Name: name,
				Sub: subject, TS: strings.Repeat("T", tsLength), Nonce: strings.Repeat("n", MaxNonceLength),
				Sig: strings.Repeat("S", sigBase64Length)}
			real := HeaderFramingBytes + len(h.Encode())
			if got := MaxHeaderWireBytes(subject, name); got < real {
				t.Errorf("name %q subject %q: MaxHeaderWireBytes %d is below the encoded header %d", name, subject, got, real)
			}
		}
	}
	// The real signer's header, escaping characters in the name.
	const name = `a"b\c`
	h, err := SignHeader(SignOptions{Signer: newSigner(t, aliceSeed), ID: id, Name: name, Subject: subj, Payload: []byte(payload)})
	if err != nil {
		t.Fatal(err)
	}
	if got := ExpectedHeaderWireBytes(id, name, subj, true); got != h.WireBytes() {
		t.Errorf("signed header with escapes: expected %d, real %d", got, h.WireBytes())
	}
	if got := MaxHeaderWireBytes(subj, name); got < h.WireBytes() {
		t.Errorf("bound %d below real %d", got, h.WireBytes())
	}
}

func reverseReg(t *testing.T, seed, user, owner string) (Registration, AgentID) {
	t.Helper()
	prompt := "agents.prompt.demo." + owner + ".example"
	id := newID(t, testAcct, user)
	meta, err := RegistrationMetadata(newSigner(t, seed), id, "demo", owner, prompt)
	if err != nil {
		t.Fatal(err)
	}
	meta["agent"], meta["owner"] = "demo", owner
	return Registration{Name: "demo", ID: owner, PromptSubject: prompt, Metadata: meta}, id
}

// controlledLister returns a lister whose nth enumeration blocks until
// release[n] is closed, signals started with n when it begins, and answers
// views[n] (the last view repeats).
func controlledLister(views [][]Registration, started chan<- int, release []chan struct{}) RegistrationLister {
	var mu sync.Mutex
	call := 0
	return func(context.Context) ([]Registration, error) {
		mu.Lock()
		n := call
		call++
		mu.Unlock()
		started <- n
		<-release[n]
		return views[min(n, len(views)-1)], nil
	}
}

func releaseChans(n int) []chan struct{} {
	c := make([]chan struct{}, n)
	for i := range c {
		c[i] = make(chan struct{})
	}
	return c
}

func waitStarted(t *testing.T, started <-chan int, want int) {
	t.Helper()
	select {
	case n := <-started:
		if n != want {
			t.Fatalf("enumeration %d started, want %d", n, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("enumeration %d did not start", want)
	}
}

// An enumeration that began before Invalidate must not commit: its result
// describes the registry as it was before the caller asked to forget it.
func TestResolver_InvalidateDuringEnumerationDiscardsStaleIndex(t *testing.T) {
	alice, _ := reverseReg(t, aliceSeed, alicePub, "alice")
	bob, bobID := reverseReg(t, bobSeed, bobPub, "bob")
	started := make(chan int, 8)
	release := releaseChans(3)
	r := NewSenderResolver(controlledLister([][]Registration{{alice}, {alice, bob}}, started, release), ResolverOptions{TTL: time.Hour})

	type result struct {
		ok  bool
		err error
	}
	first := make(chan result, 1)
	go func() {
		_, ok, err := r.Resolve(context.Background(), bobID)
		first <- result{ok, err}
	}()
	waitStarted(t, started, 0)
	r.Invalidate()
	close(release[0]) // the stale enumeration (alice only) now finishes

	// The caller is not handed the stale view and the stale index is not
	// cached: the resolver enumerates again.
	waitStarted(t, started, 1)
	select {
	case res := <-first:
		t.Fatalf("Resolve returned before the fresh enumeration finished: %+v", res)
	default:
	}
	close(release[1])
	if res := <-first; res.err != nil || !res.ok {
		t.Fatalf("after Invalidate: ok=%v err=%v, want the fresh view with bob", res.ok, res.err)
	}
	// The fresh index is cached: no third enumeration.
	if _, ok, err := r.Resolve(context.Background(), bobID); err != nil || !ok {
		t.Fatalf("cached lookup: ok=%v err=%v", ok, err)
	}
	select {
	case n := <-started:
		t.Fatalf("unexpected enumeration %d", n)
	default:
	}
}

// A caller waiting on the enumeration that Invalidate overtook must not be
// handed that enumeration's result either.
func TestResolver_InvalidateDuringEnumerationWaitersEnumerateAgain(t *testing.T) {
	alice, _ := reverseReg(t, aliceSeed, alicePub, "alice")
	bob, bobID := reverseReg(t, bobSeed, bobPub, "bob")
	started := make(chan int, 8)
	release := releaseChans(3)
	r := NewSenderResolver(controlledLister([][]Registration{{alice}, {alice, bob}}, started, release), ResolverOptions{TTL: time.Hour})

	oks := make(chan bool, 2)
	resolve := func() {
		_, ok, err := r.Resolve(context.Background(), bobID)
		if err != nil {
			t.Error(err)
		}
		oks <- ok
	}
	go resolve()
	waitStarted(t, started, 0)
	go resolve()
	deadline := time.Now().Add(5 * time.Second)
	for r.waiters.Load() != 1 { // the second caller is blocked on enumeration 0
		if time.Now().After(deadline) {
			t.Fatal("the second caller never waited on the in-flight enumeration")
		}
		runtime.Gosched()
	}
	r.Invalidate()
	close(release[0])
	waitStarted(t, started, 1)
	close(release[1])
	for i := 0; i < 2; i++ {
		if ok := <-oks; !ok {
			t.Fatal("a caller was answered from the enumeration that Invalidate overtook")
		}
	}
	select {
	case n := <-started:
		t.Fatalf("unexpected enumeration %d: callers should share the second one", n)
	case <-time.After(50 * time.Millisecond):
	}
}

// A pair whose window has already passed is not recorded and is not "Recorded".
func TestNonceSet_ExpiredPairIsNotRecorded(t *testing.T) {
	clock := newFakeClock(testNow)
	s := NewNonceSet(NonceSetOptions{Window: time.Minute, Now: clock.Now})
	old := testNow.Add(-2 * time.Minute)
	if got := s.Record("u", "n", old); got != Expired {
		t.Fatalf("an expired pair: %v, want Expired", got)
	}
	if s.Len() != 0 || s.Seen("u", "n") {
		t.Fatal("an expired pair was recorded")
	}
	if got := s.Record("u", "n", testNow); got != Recorded {
		t.Fatalf("a live pair: %v, want Recorded", got)
	}
}

// Expired maps to the stale-timestamp outcome of the window check: 401.
func TestVerifyLive_ExpiredNonceRecordIsStaleTimestamp401(t *testing.T) {
	// The replay set's clock is ahead of the verifier's: the live window check
	// passes, and the authoritative record then finds the pair already expired.
	later := func() time.Time { return testNow.Add(2 * time.Minute) }
	opts := LiveOptions{Now: fixedNow, ReplayWindow: time.Minute,
		Nonces: NewNonceSet(NonceSetOptions{Now: later, Window: time.Minute})}
	m := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
	s, err := VerifyLive(m, opts)
	if s != nil || err == nil {
		t.Fatalf("an expired pair verified: %+v", s)
	}
	if e := asError(t, err); e.Code != 401 || !errors.Is(err, ErrVerification) || !strings.Contains(e.Detail, "replay window") {
		t.Fatalf("want a 401 stale-timestamp refusal, got %d: %v", e.Code, err)
	}
	if opts.Nonces.Len() != 0 {
		t.Fatal("an expired pair was recorded")
	}
}
