// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// A request refused by min_sender_trust, PreAccept or the admission hook must
// not consume replay-set capacity.
func TestGate_RefusedRequestsConsumeNoNonceCapacity(t *testing.T) {
	ctx := context.Background()
	refuse := func(context.Context, *Sender) (bool, error) { return false, nil }
	fail := func(context.Context, *Sender) (bool, error) { return false, errors.New("db") }
	cases := []struct {
		name string
		opts GateOptions
		code int
	}{
		{"admission hook refuses", GateOptions{Accept: refuse}, 403},
		{"admission hook fails", GateOptions{Accept: fail}, 500},
		{"pre-accept refuses", GateOptions{PreAccept: func(string, string) bool { return false }}, 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newGate(t, c.opts)
			a := g.AdmitPrompt(ctx, signedMsg(t, subj, subj, []byte(payload), testNow, "n1"))
			if a.OK() || a.Err.Code != c.code {
				t.Fatalf("got %+v, want refusal %d", a, c.code)
			}
			if g.Nonces().Seen(alicePub, "n1") || g.Nonces().Len() != 0 {
				t.Error("a refused request consumed nonce-set capacity")
			}
		})
	}
	t.Run("min_sender_trust refuses an unsigned sender and records nothing", func(t *testing.T) {
		g := newGate(t, GateOptions{MinSenderTrust: TrustSigned})
		if a := g.AdmitPrompt(ctx, Message{Subject: subj}); a.OK() {
			t.Fatal("absent sender admitted")
		}
		if g.Nonces().Len() != 0 {
			t.Error("set not empty")
		}
	})
	t.Run("admitted request records and a replay is refused", func(t *testing.T) {
		var seen []string
		g := newGate(t, GateOptions{PreAccept: func(user, account string) bool {
			seen = append(seen, user+"|"+account)
			return true
		}})
		m := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
		if a := g.AdmitPrompt(ctx, m); !a.OK() {
			t.Fatal(a.Err)
		}
		if !g.Nonces().Seen(alicePub, "n1") {
			t.Error("admitted nonce not recorded")
		}
		if a := g.AdmitPrompt(ctx, m); a.OK() {
			t.Fatal("replay admitted")
		}
		if len(seen) != 1 || seen[0] != alicePub+"|"+testAcct {
			t.Errorf("PreAccept calls %v (a replay is refused before it)", seen)
		}
	})
	t.Run("a refused flood leaves the set empty", func(t *testing.T) {
		g := newGate(t, GateOptions{MaxNonces: 2, PreAccept: func(u, _ string) bool { return u == bobPub }})
		for _, n := range []string{"a1", "a2", "a3"} {
			if a := g.AdmitPrompt(ctx, signedMsg(t, subj, subj, nil, testNow, n)); a.OK() {
				t.Fatal("alice admitted")
			}
		}
		if g.Nonces().Len() != 0 {
			t.Fatal("refused requests filled the set")
		}
	})
}

func TestParseHeader_SubjectRejectsBOM(t *testing.T) {
	if validSubject("a.\ufeffb") {
		t.Error("U+FEFF accepted in a subject")
	}
	if !validSubject("a.b") {
		t.Error("plain subject refused")
	}
}

func TestParseHeader_LoneSurrogateIs400(t *testing.T) {
	base := func(extra string) string {
		return `{"v":1,"account":"ACME","user":"` + alicePub + `"` + extra + `}`
	}
	bad := []string{
		`,"name":"a\ud800b"`, `,"name":"a\udc00b"`, `,"name":"\ud800A"`, `,"name":"a\uD800"`,
		`,"name":"\\\ud800"`,
	}
	for _, e := range bad {
		if _, err := ParseHeader(base(e)); !errors.Is(err, ErrMalformedHeader) {
			t.Errorf("%s: %v, want malformed", e, err)
		}
	}
	good := []string{`,"name":"a😀b"`, `,"name":"\\ud800"`, `,"name":"aA"`}
	for _, e := range good {
		if _, err := ParseHeader(base(e)); err != nil {
			t.Errorf("%s: %v, want accepted", e, err)
		}
	}
}

// The enumerating caller giving up must not hand its ctx error to a waiter
// whose own context is live.
func TestResolver_EnumeratorCancelDoesNotPoisonWaiters(t *testing.T) {
	bob, bobID := reverseReg(t, bobSeed, bobPub, "bob")
	var mu sync.Mutex
	call := 0
	started := make(chan int, 4)
	lister := func(ctx context.Context) ([]Registration, error) {
		mu.Lock()
		n := call
		call++
		mu.Unlock()
		started <- n
		if n == 0 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return []Registration{bob}, nil
	}
	r := NewSenderResolver(lister, ResolverOptions{TTL: time.Hour})
	ctx1, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, _, err := r.Resolve(ctx1, bobID); first <- err }()
	waitStarted(t, started, 0)

	type res struct {
		ok  bool
		err error
	}
	second := make(chan res, 1)
	go func() {
		_, ok, err := r.Resolve(context.Background(), bobID)
		second <- res{ok, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for r.waiters.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("second caller never waited")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("enumerating caller: %v", err)
	}
	got := <-second
	if got.err != nil || !got.ok {
		t.Fatalf("waiter with a live ctx got ok=%v err=%v", got.ok, got.err)
	}
}

// A lone surrogate outside `name` is not rejected by the surrogate check: the
// official SDKs scan only `name` (TS sender-header.ts:152-171, Python
// sender_header.py:189-203). The decoder turns the escape into U+FFFD and the
// field's own rule decides; the outcome is pinned here.
func TestParseHeader_LoneSurrogateOutsideNameNotRejectedByThatCheck(t *testing.T) {
	v := `{"v":1,"account":"ACME","user":"` + alicePub + `","sub":"a.\ud800"}`
	h, err := ParseHeader(v)
	if err != nil {
		t.Fatalf("lone surrogate in sub: %v, want accepted (no per-field rule refuses U+FFFD)", err)
	}
	if h.Sub != "a.\ufffd" {
		t.Errorf("Sub = %q, want the decoder's U+FFFD substitution", h.Sub)
	}
}
