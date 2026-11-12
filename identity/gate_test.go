// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func newGate(t *testing.T, opts GateOptions) *Gate {
	t.Helper()
	if opts.Now == nil {
		opts.Now = fixedNow
	}
	g, err := NewGate(opts)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGate_AdmitPrompt(t *testing.T) {
	ctx := context.Background()
	unsigned := Message{Subject: subj, Data: []byte(payload)}
	claimID := newID(t, testAcct, alicePub)
	claim, _ := ClaimHeader(claimID, "x")
	claimed := withHeader(unsigned, claim.Encode())
	badSig := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
	badSig.Data = []byte("tampered")
	hookRefuses := func(context.Context, *Sender) (bool, error) { return false, nil }
	hookBroken := func(context.Context, *Sender) (bool, error) { return false, errors.New("db down") }

	cases := []struct {
		name string
		opts GateOptions
		msg  Message
		code int // 0 = admitted
		kind error
		want Trust
	}{
		{"any: absent served", GateOptions{}, unsigned, 0, nil, TrustAbsent},
		{"any: claimed served", GateOptions{}, claimed, 0, nil, TrustClaimed},
		{"any: verified served", GateOptions{}, signedMsg(t, subj, subj, []byte(payload), testNow, "n1"), 0, nil, TrustVerified},
		{"any: malformed is 400", GateOptions{}, withHeader(unsigned, "{"), 400, ErrMalformedHeader, ""},
		{"any: bad signature is 401, whatever the endpoint", GateOptions{}, badSig, 401, ErrVerification, ""},
		{"signed: absent is 401", GateOptions{MinSenderTrust: TrustSigned}, unsigned, 401, ErrSignatureRequired, ""},
		{"signed: claimed is 401", GateOptions{MinSenderTrust: TrustSigned}, claimed, 401, ErrSignatureRequired, ""},
		{"signed: verified served", GateOptions{MinSenderTrust: TrustSigned}, signedMsg(t, subj, subj, []byte(payload), testNow, "n1"), 0, nil, TrustVerified},
		{"signed: bad signature stays 401 verification", GateOptions{MinSenderTrust: TrustSigned}, badSig, 401, ErrVerification, ""},
		{"hook refuses a verified sender: 403", GateOptions{Accept: hookRefuses}, signedMsg(t, subj, subj, []byte(payload), testNow, "n1"), 403, ErrSenderRefused, ""},
		{"hook refuses a claimed sender: 401", GateOptions{Accept: hookRefuses}, claimed, 401, ErrSignatureRequired, ""},
		{"hook refuses an absent sender: 401", GateOptions{Accept: hookRefuses}, unsigned, 401, ErrSignatureRequired, ""},
		{"hook fails: 500", GateOptions{Accept: hookBroken}, signedMsg(t, subj, subj, []byte(payload), testNow, "n1"), 500, ErrAdmission, ""},
		{"hook accepts", GateOptions{Accept: func(context.Context, *Sender) (bool, error) { return true, nil }}, signedMsg(t, subj, subj, []byte(payload), testNow, "n1"), 0, nil, TrustVerified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGate(t, tc.opts)
			a := g.AdmitPrompt(ctx, tc.msg)
			if tc.code == 0 {
				if !a.OK() || a.Err != nil || TrustOf(a.Sender) != tc.want {
					t.Fatalf("want admitted as %s, got %+v", tc.want, a)
				}
				return
			}
			if a.OK() || a.Err.Code != tc.code || !errors.Is(a.Err, tc.kind) {
				t.Fatalf("want %d (%v), got %+v", tc.code, tc.kind, a.Err)
			}
		})
	}
}

func TestGate_HookSeesTheSender(t *testing.T) {
	var got *Sender
	g := newGate(t, GateOptions{Accept: func(_ context.Context, s *Sender) (bool, error) { got = s; return true, nil }})
	if a := g.AdmitPrompt(context.Background(), signedMsg(t, subj, subj, nil, testNow, "n1")); !a.OK() {
		t.Fatal(a.Err)
	}
	if got == nil || got.ID != newID(t, testAcct, alicePub) {
		t.Fatalf("hook saw %+v", got)
	}
}

func TestGate_NewGateRejectsBadOptions(t *testing.T) {
	if _, err := NewGate(GateOptions{MinSenderTrust: "bogus"}); err == nil {
		t.Error("unknown MinSenderTrust accepted")
	}
	if _, err := NewGate(GateOptions{Subject: SubjectRules{AccountTokenPosition: -2}}); err == nil {
		t.Error("negative position accepted")
	}
	g, err := NewGate(GateOptions{})
	if err != nil || g.MinSenderTrust() != TrustAny {
		t.Errorf("default is %v, %v", g.MinSenderTrust(), err)
	}
}

// Mutation: accept a replayed nonce. The gate records after admission.
func TestGate_ReplayedNonceIsRefused(t *testing.T) {
	g := newGate(t, GateOptions{})
	msg := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
	if a := g.AdmitPrompt(context.Background(), msg); !a.OK() {
		t.Fatal(a.Err)
	}
	a := g.AdmitPrompt(context.Background(), msg)
	if a.OK() || a.Err.Code != 401 || !errors.Is(a.Err, ErrVerification) {
		t.Fatalf("a replay was admitted: %+v", a)
	}
	// A fresh nonce is fine.
	if a := g.AdmitPrompt(context.Background(), signedMsg(t, subj, subj, []byte(payload), testNow, "n2")); !a.OK() {
		t.Fatal(a.Err)
	}
}

func TestGate_NonceRecordedOnlyAfterEveryCheckPasses(t *testing.T) {
	g := newGate(t, GateOptions{Accept: func(context.Context, *Sender) (bool, error) { return false, nil }})
	// The hook refuses; the nonce is recorded only after the hook admits.
	msg := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
	g.AdmitPrompt(context.Background(), msg)
	if g.Nonces().Seen(alicePub, "n1") {
		t.Error("a request refused by the hook was recorded")
	}
	// A transplanted header (wrong subject) must not poison the set.
	g2 := newGate(t, GateOptions{})
	bad := signedMsg(t, subj, "agents.prompt.other.x.y", []byte(payload), testNow, "n7")
	if a := g2.AdmitPrompt(context.Background(), bad); a.OK() {
		t.Fatal("transplanted header admitted")
	}
	if g2.Nonces().Seen(alicePub, "n7") {
		t.Error("a refused header's nonce was recorded")
	}
}

func TestGate_ConcurrentSameNonceHasOneWinner(t *testing.T) {
	g := newGate(t, GateOptions{})
	msg := signedMsg(t, subj, subj, []byte(payload), testNow, "n1")
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if g.AdmitPrompt(context.Background(), msg).OK() {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d concurrent copies of one request were admitted", won)
	}
}

func TestGate_FullNonceSetFailsClosedAndLogs(t *testing.T) {
	var buf lockedBuffer
	g := newGate(t, GateOptions{MaxNonces: 2, Logger: slog.New(slog.NewTextHandler(&buf, nil))})
	ctx := context.Background()
	for _, n := range []string{"a1", "a2"} {
		if a := g.AdmitPrompt(ctx, signedMsg(t, subj, subj, nil, testNow, n)); !a.OK() {
			t.Fatal(a.Err)
		}
	}
	a := g.AdmitPrompt(ctx, signedMsg(t, subj, subj, nil, testNow, "a3"))
	if a.OK() || a.Err.Code != 401 {
		t.Fatalf("a full set admitted a new nonce: %+v", a)
	}
	// Old entries were not dropped: a1 is still refused as a replay.
	if a := g.AdmitPrompt(ctx, signedMsg(t, subj, subj, nil, testNow, "a1")); a.OK() {
		t.Fatal("an old entry was evicted")
	}
	if !strings.Contains(buf.String(), "nonce set is full") {
		t.Errorf("overflow not logged: %q", buf.String())
	}
	if strings.Contains(buf.String(), "a3") || strings.Contains(buf.String(), "a1") {
		t.Errorf("a nonce was logged: %q", buf.String())
	}
}

// The status endpoint is always answerable (SAP §13.9). Mutation: refuse status
// for an unsigned sender.
func TestGate_StatusIsAlwaysAnswerable(t *testing.T) {
	var buf lockedBuffer
	g := newGate(t, GateOptions{
		MinSenderTrust: TrustSigned,
		Accept:         func(context.Context, *Sender) (bool, error) { return false, nil },
		Logger:         slog.New(slog.NewTextHandler(&buf, nil)),
	})
	claim, _ := ClaimHeader(newID(t, testAcct, alicePub), "")
	bad := signedMsg(t, subj, subj, nil, testNow, "n1")
	bad.Data = []byte("tampered")
	replay := signedMsg(t, "agents.status.demo.alice.example", "agents.status.demo.alice.example", nil, testNow, "r1")

	cases := []struct {
		name      string
		msg       Message
		sender    Trust
		rejection int
	}{
		{"unsigned sender on a signed-only gate", Message{Subject: "agents.status.demo.alice.example"}, TrustAbsent, 0},
		{"claimed sender", withHeader(Message{Subject: "agents.status.demo.alice.example"}, claim.Encode()), TrustClaimed, 0},
		{"verified sender the hook would refuse", replay, TrustVerified, 0},
		{"bad signature is logged, not refused", bad, TrustAbsent, 401},
		{"malformed header is logged, not refused", withHeader(replay, "{"), TrustAbsent, 400},
		{"a replayed nonce is logged, not refused", replay, TrustAbsent, 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := g.ClassifyStatus(tc.msg)
			if !res.Answerable() {
				t.Fatal("status was refused")
			}
			if tc.rejection == 0 {
				if res.Rejection != nil || TrustOf(res.Sender) != tc.sender {
					t.Fatalf("got %+v", res)
				}
				return
			}
			if res.Rejection == nil || res.Rejection.Code != tc.rejection || res.Sender != nil {
				t.Fatalf("got %+v", res)
			}
		})
	}
	if !strings.Contains(buf.String(), "reply sent anyway") {
		t.Errorf("failed classification not logged: %q", buf.String())
	}
	// An unsigned status request must not be a rejection at all.
	if res := g.ClassifyStatus(Message{Subject: "agents.status.demo.alice.example"}); res.Rejection != nil {
		t.Fatalf("unsigned status carries a rejection: %v", res.Rejection)
	}
}

func TestGate_StatusRecordsVerifiedNonce(t *testing.T) {
	g := newGate(t, GateOptions{})
	msg := signedMsg(t, "agents.status.x.y.z", "agents.status.x.y.z", nil, testNow, "s1")
	if res := g.ClassifyStatus(msg); res.Sender == nil || res.Rejection != nil {
		t.Fatalf("got %+v", res)
	}
	if !g.Nonces().Seen(alicePub, "s1") {
		t.Fatal("the status nonce was not recorded")
	}
}

func TestGate_AccountTokenPosition(t *testing.T) {
	g := newGate(t, GateOptions{Subject: SubjectRules{AccountTokenPosition: 2}})
	ok := signedMsg(t, "svc.prompt", "svc.ACME.prompt", nil, testNow, "n1")
	if a := g.AdmitPrompt(context.Background(), ok); !a.OK() {
		t.Fatalf("inserted token refused: %v", a.Err)
	}
	bad := signedMsg(t, "svc.prompt", "svc.EVIL.prompt", nil, testNow, "n2")
	if a := g.AdmitPrompt(context.Background(), bad); a.OK() || a.Err.Code != 401 {
		t.Fatalf("a foreign inserted token was admitted: %+v", a)
	}
}

func TestGate_WindowFollowsTheClock(t *testing.T) {
	now := testNow
	g, err := NewGate(GateOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	msg := signedMsg(t, subj, subj, nil, testNow, "n1")
	now = testNow.Add(29 * time.Second)
	if a := g.AdmitPrompt(context.Background(), msg); !a.OK() {
		t.Fatal(a.Err)
	}
	now = testNow.Add(40 * time.Second)
	if a := g.AdmitPrompt(context.Background(), msg); a.OK() {
		t.Fatal("an expired header was admitted")
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestToError(t *testing.T) {
	want := newError(401, ErrAdmission, "unauthorized", "x")
	if got := toError(fmt.Errorf("wrapped: %w", want)); got != want {
		t.Fatalf("a wrapped *Error was not unwrapped: %v", got)
	}
	got := toError(errors.New("not an *Error"))
	if got.Code != 500 || got.Detail != "not an *Error" {
		t.Fatalf("fallback = %d %q, want a generic 500", got.Code, got.Detail)
	}
}
