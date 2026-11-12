// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// signedBy builds a message signed by the given user, arriving on subj.
func signedBy(t *testing.T, seed, pub, nonce string) Message {
	t.Helper()
	h, err := SignHeader(SignOptions{
		Signer: newSigner(t, seed), ID: newID(t, testAcct, pub), Subject: subj,
		Payload: []byte(payload), TS: FormatTimestamp(testNow), Nonce: nonce,
	})
	if err != nil {
		t.Fatal(err)
	}
	return Message{Subject: subj, Data: []byte(payload), Header: nats.Header{HeaderName: {h.Encode()}}}
}

// Finding 1: live verification cannot succeed without replay protection.
func TestVerifyLive_RequiresNonceSet(t *testing.T) {
	msg := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
	s, err := VerifyLive(msg, LiveOptions{Now: fixedNow})
	if s != nil || !errors.Is(err, ErrNoNonceSet) {
		t.Fatalf("live verification without a NonceSet: %+v, %v", s, err)
	}
	var ie *Error
	if errors.As(err, &ie) {
		t.Fatal("a configuration error must not be a wire *Error")
	}
	// Absent and claimed senders also need the configuration: the check is
	// up front, so a misconfigured receiver fails on every request.
	if _, err := VerifyLive(Message{Subject: subj}, LiveOptions{Now: fixedNow}); !errors.Is(err, ErrNoNonceSet) {
		t.Fatalf("unsigned request without a NonceSet: %v", err)
	}
}

// Finding 1: VerifyLive itself records the nonce, so the same request
// verifies once. Mutation: accept a replay.
func TestVerifyLive_RecordsNonceAtomically(t *testing.T) {
	o := liveOpts()
	msg := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
	if s, err := VerifyLive(msg, o); err != nil || s == nil {
		t.Fatalf("first: %v", err)
	}
	if !o.Nonces.Seen(alicePub, "n1") {
		t.Fatal("VerifyLive did not record the nonce")
	}
	s, err := VerifyLive(msg, o)
	if s != nil || asError(t, err).Code != 401 {
		t.Fatalf("replay verified: %+v %v", s, err)
	}

	// Concurrent callers of the same request: exactly one wins.
	o = liveOpts()
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s, err := VerifyLive(msg, o); err == nil && s != nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d concurrent verifications succeeded, want exactly 1", wins)
	}
}

// A forged or stale header must not poison the set.
func TestVerifyLive_FailedChecksDoNotRecordNonce(t *testing.T) {
	o := liveOpts()
	bad := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
	bad.Data = []byte("tampered")
	if _, err := VerifyLive(bad, o); err == nil {
		t.Fatal("tampered payload verified")
	}
	stale := signedMsg(t, subj, subj, []byte(payload), testNow.Add(-time.Hour), "n2")
	if _, err := VerifyLive(stale, o); err == nil {
		t.Fatal("stale request verified")
	}
	if o.Nonces.Len() != 0 {
		t.Fatalf("a refused header left %d entries", o.Nonces.Len())
	}
}

// Stored verification stays nonce-free (SAP §13.8).
func TestVerifyStored_NeedsNoNonceSetAndRecordsNothing(t *testing.T) {
	msg := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
	for i := 0; i < 3; i++ {
		if s, err := VerifyStored(msg, StoredOptions{}); err != nil || s == nil {
			t.Fatalf("stored verification %d: %v", i, err)
		}
	}
}

// Finding 2: a set with a shorter window than the verifier is refused.
func TestVerifyLive_ShortNonceWindowIsRefused(t *testing.T) {
	msg := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
	o := LiveOptions{Now: fixedNow, ReplayWindow: 30 * time.Second,
		Nonces: NewNonceSet(NonceSetOptions{Window: time.Second, Now: fixedNow})}
	if _, err := VerifyLive(msg, o); !errors.Is(err, ErrNonceWindow) {
		t.Fatalf("got %v, want ErrNonceWindow", err)
	}
	// A longer window is fine (replays are excluded for longer, never less).
	o.Nonces = NewNonceSet(NonceSetOptions{Window: time.Minute, Now: fixedNow})
	if _, err := VerifyLive(msg, o); err != nil {
		t.Fatal(err)
	}
}

func TestNewGate_ShortNonceWindowIsRefused(t *testing.T) {
	short := NewNonceSet(NonceSetOptions{Window: time.Second})
	for name, opts := range map[string]GateOptions{
		"explicit 30 s window": {ReplayWindow: 30 * time.Second, Nonces: short},
		"default window":       {Nonces: short},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewGate(opts); !errors.Is(err, ErrNonceWindow) {
				t.Fatalf("got %v, want ErrNonceWindow", err)
			}
		})
	}
	ok := NewNonceSet(NonceSetOptions{Window: 30 * time.Second})
	if _, err := NewGate(GateOptions{ReplayWindow: 30 * time.Second, Nonces: ok}); err != nil {
		t.Fatalf("an equal window is refused: %v", err)
	}
	if got := NewNonceSet(NonceSetOptions{}).Window(); got != DefaultReplayWindow {
		t.Fatalf("default set window %v", got)
	}
}

// Finding 2, end to end: a request stays a replay for as long as its
// timestamp is inside the window, on both sides of skew.
func TestGate_ReplayRefusedThroughoutAcceptedWindow(t *testing.T) {
	for name, offset := range map[string]time.Duration{
		"timestamp now": 0, "timestamp 29 s ahead": 29 * time.Second, "timestamp 30 s old": -30 * time.Second,
	} {
		t.Run(name, func(t *testing.T) {
			clock := newFakeClock(testNow)
			g, err := NewGate(GateOptions{Now: clock.Now})
			if err != nil {
				t.Fatal(err)
			}
			msg := signedMsg(t, subj, subj, []byte(payload), testNow.Add(offset), "n1")
			if a := g.AdmitPrompt(context.Background(), msg); !a.OK() {
				t.Fatal(a.Err)
			}
			// Step to the last instant the timestamp check still accepts it.
			for step := time.Duration(0); ; step += time.Second {
				clock.now.Store(testNow.Add(step).UnixNano())
				skew := clock.Now().Sub(testNow.Add(offset)).Abs()
				if skew > DefaultReplayWindow {
					break
				}
				if a := g.AdmitPrompt(context.Background(), msg); a.OK() {
					t.Fatalf("replay admitted at +%v (skew %v, inside the window)", step, skew)
				}
			}
		})
	}
}

// Finding 3: one sender cannot exhaust capacity that other senders need.
func TestNonceSet_PerSenderQuota(t *testing.T) {
	clock := newFakeClock(testNow)
	s := NewNonceSet(NonceSetOptions{MaxEntries: 10, MaxPerSender: 3, Now: clock.Now})
	for i := 0; i < 3; i++ {
		if got := s.Record("A", fmt.Sprint("n", i), testNow); got != Recorded {
			t.Fatalf("A fill %d: %v", i, got)
		}
	}
	if got := s.Record("A", "n3", testNow); got != SenderQuotaExceeded {
		t.Fatalf("A over quota: %v", got)
	}
	// A replay of a held entry is still reported as a replay.
	if got := s.Record("A", "n0", testNow); got != Replayed {
		t.Fatalf("A replay: %v", got)
	}
	// Another sender is unaffected.
	if got := s.Record("B", "n0", testNow); got != Recorded {
		t.Fatalf("B after A's flood: %v", got)
	}
	// The quota frees as entries expire.
	clock.Advance(time.Minute)
	if got := s.Record("A", "n9", clock.Now()); got != Recorded {
		t.Fatalf("A after expiry: %v", got)
	}
	if s.Len() != 1 {
		t.Fatalf("Len %d", s.Len())
	}
}

func TestNonceSet_GlobalCapStillFailsClosed(t *testing.T) {
	s := NewNonceSet(NonceSetOptions{MaxEntries: 4, MaxPerSender: 2, Now: fixedNow})
	for _, u := range []string{"A", "A", "B", "B"} {
		if got := s.Record(u, fmt.Sprint(s.Len()), testNow); got != Recorded {
			t.Fatalf("fill: %v", got)
		}
	}
	if got := s.Record("C", "x", testNow); got != SetFull {
		t.Fatalf("global cap: %v, want SetFull", got)
	}
	if got := s.Record("A", "y", testNow); got != SetFull {
		t.Fatalf("A at quota and set full: %v", got)
	}
	if NewNonceSet(NonceSetOptions{MaxEntries: 5, MaxPerSender: 99}).perMax != 5 {
		t.Fatal("quota not clamped to the global cap")
	}
	if NewNonceSet(NonceSetOptions{}).perMax != DefaultMaxNoncesPerSender {
		t.Fatal("default quota")
	}
}

func TestGate_SenderFloodIsRefusedOthersStillAdmitted(t *testing.T) {
	g := newGate(t, GateOptions{MaxNonces: 100, MaxNoncesPerSender: 5})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if a := g.AdmitPrompt(ctx, signedBy(t, aliceSeed, alicePub, fmt.Sprint("a", i))); !a.OK() {
			t.Fatalf("alice %d: %v", i, a.Err)
		}
	}
	a := g.AdmitPrompt(ctx, signedBy(t, aliceSeed, alicePub, "a5"))
	if a.OK() || a.Err.Code != 401 || !errors.Is(a.Err, ErrVerification) {
		t.Fatalf("alice over quota was admitted: %+v", a)
	}
	if g.Nonces().Seen(alicePub, "a5") {
		t.Fatal("a refused nonce was recorded")
	}
	if a := g.AdmitPrompt(ctx, signedBy(t, bobSeed, bobPub, "b0")); !a.OK() {
		t.Fatalf("bob refused after alice's flood: %v", a.Err)
	}
	// The global cap still fails closed through the gate.
	g2 := newGate(t, GateOptions{MaxNonces: 2, MaxNoncesPerSender: 1})
	g2.AdmitPrompt(ctx, signedBy(t, aliceSeed, alicePub, "a0"))
	g2.AdmitPrompt(ctx, signedBy(t, bobSeed, bobPub, "b0"))
	third := g2.AdmitPrompt(ctx, signedMsg(t, subj, subj, []byte(payload), testNow, "a1"))
	if third.OK() {
		t.Fatal("a full set admitted a request")
	}
}

// A sender cannot mint quota partitions by varying the (unattested) account.
//
// catalog: SAP-13.7.2-01
func TestNonceSet_QuotaIsKeyedOnUserNotAccount(t *testing.T) {
	g := newGate(t, GateOptions{MaxNoncesPerSender: 2})
	ctx := context.Background()
	for i, acct := range []string{"ACME", "OTHER", "THIRD"} {
		h, err := SignHeader(SignOptions{
			Signer: newSigner(t, aliceSeed), ID: newID(t, acct, alicePub), Subject: subj,
			Payload: []byte(payload), TS: FormatTimestamp(testNow), Nonce: fmt.Sprint("n", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		msg := Message{Subject: subj, Data: []byte(payload), Header: nats.Header{HeaderName: {h.Encode()}}}
		a := g.AdmitPrompt(ctx, msg)
		if (i < 2) != a.OK() {
			t.Fatalf("account %s: admitted=%v", acct, a.OK())
		}
	}
}

// Finding 4: Reset during an in-flight lookup. Run with -race.
func TestSelfID_ResetDuringLookupDiscardsStaleResult(t *testing.T) {
	const oldReply = `{"data":{"user":"` + alicePub + `","account":"OLDACCT"}}`
	const newReply = `{"data":{"user":"` + alicePub + `","account":"NEWACCT"}}`
	release := make(chan struct{})
	canceled := make(chan struct{})
	started := make(chan struct{})
	conn := &fakeConn{reply: func(ctx context.Context, n int) (*nats.Msg, error) {
		if n == 1 {
			close(started)
			<-ctx.Done() // Reset must cancel the lookup's context
			close(canceled)
			<-release // a late reply from the old connection, ignoring the cancellation
			return &nats.Msg{Data: []byte(oldReply)}, nil
		}
		return &nats.Msg{Data: []byte(newReply)}, nil
	}}
	r := NewSelfIDResolver(conn, SelfIDOptions{Timeout: 10 * time.Second})

	type result struct {
		id  AgentID
		err error
	}
	first := make(chan result, 1)
	go func() { id, err := r.ID(context.Background()); first <- result{id, err} }()
	<-started

	r.Reset()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("Reset did not cancel the in-flight lookup")
	}
	// A caller after Reset starts its own lookup against the new connection.
	id, err := r.ID(context.Background())
	if err != nil || id.Account() != "NEWACCT" {
		t.Fatalf("after Reset: %q %v", id, err)
	}
	close(release) // the stale reply arrives now
	res := <-first
	if res.err != nil || res.id.Account() != "NEWACCT" {
		t.Fatalf("the pre-Reset caller got %q, %v; the stale result must not be returned", res.id, res.err)
	}
	if got, ok := r.Peek(); !ok || got.Account() != "NEWACCT" {
		t.Fatalf("stale reply committed: %q %v", got, ok)
	}
}

func TestSelfID_StaleLookupDoesNotCommitWhenNothingFollows(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	conn := &fakeConn{reply: func(_ context.Context, n int) (*nats.Msg, error) {
		if n == 1 {
			close(started)
			<-release
			return &nats.Msg{Data: []byte(`{"data":{"user":"` + alicePub + `","account":"OLDACCT"}}`)}, nil
		}
		return nil, errors.New("connection reset")
	}}
	r := NewSelfIDResolver(conn, SelfIDOptions{Timeout: 10 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := r.ID(ctx); done <- err }()
	<-started
	r.Reset()
	cancel() // the caller gives up as well
	close(release)
	if err := <-done; err == nil {
		t.Fatal("expected an error from the abandoned caller")
	}
	if _, ok := r.Peek(); ok {
		t.Fatal("a pre-Reset result was committed")
	}
}

func TestSelfID_ResetRaceWithManyCallers(_ *testing.T) {
	conn := &fakeConn{reply: func(ctx context.Context, _ int) (*nats.Msg, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
		}
		return &nats.Msg{Data: []byte(userInfoOK)}, nil
	}}
	r := NewSelfIDResolver(conn, SelfIDOptions{Timeout: time.Second})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if j%7 == 0 {
					r.Reset()
				}
				_, _ = r.ID(context.Background())
				r.Peek()
			}
		}()
	}
	wg.Wait()
}

// Finding 5: repeated JSON member names.
func TestParseHeader_DuplicateKeysAreMalformed(t *testing.T) {
	good := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
	value := good.Header.Get(HeaderName)
	if !strings.Contains(value, `"account":"ACME"`) || !strings.Contains(value, `"user":"`) {
		t.Fatalf("unexpected header shape %s", value)
	}
	cases := map[string]string{
		"account repeated":          strings.Replace(value, `"account":"ACME"`, `"account":"EVIL","account":"ACME"`, 1),
		"account repeated, escaped": strings.Replace(value, `"account":"ACME"`, `"account":"ACME","\u0061ccount":"EVIL"`, 1),
		"account repeated, mixed":   strings.Replace(value, `"account":"ACME"`, `"\u0061\u0063count":"EVIL","account":"ACME"`, 1),
		"sig repeated":              strings.Replace(value, `{"v":1,`, `{"v":1,"sig":"x","\u0073ig":"y",`, 1),
		"v repeated":                strings.Replace(value, `"v":1`, `"v":1,"v":2`, 1),
		"unknown member repeated":   strings.Replace(value, `"v":1`, `"v":1,"x":1,"x":2`, 1),
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			if v == value {
				t.Fatal("case did not alter the header")
			}
			h, err := ParseHeader(v)
			if h != nil || asError(t, err).Code != 400 || !errors.Is(err, ErrMalformedHeader) {
				t.Fatalf("got %+v, %v; want 400 malformed", h, err)
			}
			s, err := VerifyLive(withHeader(good, v), liveOpts())
			if s != nil || asError(t, err).Code != 400 {
				t.Fatalf("VerifyLive: %+v %v", s, err)
			}
			s, err = VerifyStored(withHeader(good, v), StoredOptions{})
			if s != nil || asError(t, err).Code != 400 {
				t.Fatalf("VerifyStored: %+v %v", s, err)
			}
			g := newGate(t, GateOptions{})
			if a := g.AdmitPrompt(context.Background(), withHeader(good, v)); a.OK() || a.Err.Code != 400 {
				t.Fatalf("Gate: %+v", a)
			}
		})
	}
	if _, err := ParseHeader(value); err != nil {
		t.Fatalf("the unaltered header: %v", err)
	}
}

// Finding 5, other identity JSON the package parses.
func TestDuplicateKeysRejectedInOtherIdentityJSON(t *testing.T) {
	t.Run("request info", func(t *testing.T) {
		for name, v := range map[string]string{
			"acc":         `{"acc":"A","acc":"B"}`,
			"acc escaped": `{"acc":"A","\u0061cc":"B"}`,
			"user":        `{"user":"` + alicePub + `","user":"` + bobPub + `"}`,
			"ignored":     `{"rtt":1,"rtt":2}`,
		} {
			if _, err := ParseRequestInfo(v); !errors.Is(err, ErrBadRequestInfo) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := ParseRequestInfo(`{"acc":"A","rtt":1}`); err != nil {
			t.Errorf("valid stamp: %v", err)
		}
	})
	t.Run("user info reply", func(t *testing.T) {
		for name, v := range map[string]string{
			"data.user": `{"data":{"user":"` + alicePub + `","user":"` + bobPub + `","account":"A"}}`,
			"data":      `{"data":{"user":"` + alicePub + `","account":"A"},"data":{"user":"` + bobPub + `","account":"B"}}`,
		} {
			if _, err := IdentityFromUserInfoReply([]byte(v)); !errors.Is(err, ErrIdentityUnknown) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
	t.Run("agent info record", func(t *testing.T) {
		if _, ok := parseInfo([]byte(`{"name":"a","name":"b"}`)); ok {
			t.Error("a record repeating name was parsed")
		}
		if _, ok := parseInfo([]byte(`{"name":"a","metadata":{"k":"1","k":"2"}}`)); ok {
			t.Error("a record repeating a metadata key was parsed")
		}
		if _, ok := parseInfo([]byte(`{"name":"a"}`)); !ok {
			t.Error("a valid record was refused")
		}
	})
	t.Run("user JWT claims", func(t *testing.T) {
		enc := func(claims string) string {
			return "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".sig"
		}
		dup := `{"sub":"` + alicePub + `","sub":"` + bobPub + `","iss":"ACME"}`
		if _, err := IdentityFromJWT(enc(dup)); err == nil {
			t.Error("a JWT repeating sub was accepted")
		}
		ok := `{"sub":"` + alicePub + `","iss":"ACME"}`
		if _, err := IdentityFromJWT(enc(ok)); err != nil {
			t.Errorf("valid claims: %v", err)
		}
	})
	t.Run("checker", func(t *testing.T) {
		for v, want := range map[string]bool{
			`{}`: false, `[]`: false, `{"a":1,"b":2}`: false, `{"a":{"a":1}}`: false, `[{"a":1},{"a":2}]`: false,
			`{"a":1,"a":2}`: true, `{"a":{"b":1,"b":2}}`: true, `[{"a":1,"a":2}]`: true, `{"a":1,"\u0061":2}`: true,
		} {
			if got := checkNoDuplicateKeys([]byte(v)) != nil; got != want {
				t.Errorf("%s: duplicate=%v, want %v", v, got, want)
			}
		}
		if checkNoDuplicateKeys([]byte(`{"a":1} x`)) == nil || checkNoDuplicateKeys([]byte(`{"a":`)) == nil {
			t.Error("invalid JSON passed the checker")
		}
	})
}
