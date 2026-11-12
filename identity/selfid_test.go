// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

const userInfoOK = `{"data":{"user":"` + alicePub + `","account":"ACME","permissions":{}},"server":{"name":"s"}}`

// catalog: SAP-13.3.1-02 SAP-13.4-05
func TestIdentityFromUserInfoReply(t *testing.T) {
	const opAcct = "AABYLMBR6Q2CDXTLGRQCFA2GP76BGCDF7NZF2OVHH4RQ7L3Y3TZWJDRL"
	mk := func(user, account string) string {
		b, _ := json.Marshal(map[string]any{"data": map[string]any{"user": user, "account": account}})
		return string(b)
	}
	cases := []struct {
		name  string
		reply string
		want  AgentID
		kind  error
	}{
		{"operator mode", mk(alicePub, opAcct), AgentID(opAcct + "." + alicePub), nil},
		{"named account", mk(alicePub, "ACME"), AgentID("ACME." + alicePub), nil},
		{"global account", mk(alicePub, "$G"), AgentID("$G." + alicePub), nil},
		{"unrepresentable account name", mk(alicePub, "acme corp"), "", ErrNoIdentity},
		{"dotted account name", mk(alicePub, "a.b"), "", ErrNoIdentity},
		{"no authentication", mk("", "$G"), "", ErrNoIdentity},
		{"token user", mk("[REDACTED]", "$G"), "", ErrNoIdentity},
		{"password user", mk("alice", "$G"), "", ErrNoIdentity},
		{"error reply", `{"error":{"code":500,"description":"x"}}`, "", ErrIdentityUnknown},
		{"not JSON", `hello`, "", ErrIdentityUnknown},
		{"no data", `{}`, "", ErrIdentityUnknown},
		{"user is a number", `{"data":{"user":5,"account":"ACME"}}`, "", ErrIdentityUnknown},
		{"unknown fields are ignored", `{"data":{"user":"` + alicePub + `","account":"ACME","x":1},"y":2}`, AgentID("ACME." + alicePub), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := IdentityFromUserInfoReply([]byte(tc.reply))
			if tc.kind == nil {
				if err != nil || got != tc.want {
					t.Fatalf("got %q, %v", got, err)
				}
				return
			}
			if !errors.Is(err, tc.kind) || got != "" {
				t.Fatalf("got %q, %v; want %v", got, err, tc.kind)
			}
		})
	}
}

// fakeConn answers $SYS.REQ.USER.INFO according to a script.
type fakeConn struct {
	mu    sync.Mutex
	calls int
	reply func(ctx context.Context, call int) (*nats.Msg, error)
}

func (f *fakeConn) RequestWithContext(ctx context.Context, subj string, _ []byte) (*nats.Msg, error) {
	if subj != UserInfoSubject {
		return nil, errors.New("unexpected subject " + subj)
	}
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	return f.reply(ctx, n)
}

func (f *fakeConn) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func blackhole(ctx context.Context, _ int) (*nats.Msg, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func answer(body string) func(context.Context, int) (*nats.Msg, error) {
	return func(context.Context, int) (*nats.Msg, error) { return &nats.Msg{Data: []byte(body)}, nil }
}

// An unknown identity is retried after the interval and recovers; meanwhile
// there is no ID, so no Agent-Sender header is sent. (AT-005)
//
// catalog: SAP-13.4-02 SAP-13.4-06 SAP-13.4-07 SAP-13.4-08
func TestSelfID_UnknownIsRetriedAndRecovers(t *testing.T) {
	clock := newFakeClock(testNow)
	var healthy atomic.Bool
	conn := &fakeConn{reply: func(ctx context.Context, n int) (*nats.Msg, error) {
		if healthy.Load() {
			return answer(userInfoOK)(ctx, n)
		}
		return blackhole(ctx, n)
	}}
	r := NewSelfIDResolver(conn, SelfIDOptions{Timeout: 20 * time.Millisecond, RetryInterval: time.Minute, Now: clock.Now})
	ctx := context.Background()

	_, err := r.ID(ctx)
	if !errors.Is(err, ErrIdentityUnknown) {
		t.Fatalf("first call: %v", err)
	}
	if conn.Calls() != 1 {
		t.Fatalf("calls = %d", conn.Calls())
	}
	if _, ok := r.Peek(); ok {
		t.Fatal("an ID is remembered after a failure")
	}
	// Inside the interval the failure is repeated without asking again.
	if _, err := r.ID(ctx); !errors.Is(err, ErrIdentityUnknown) || conn.Calls() != 1 {
		t.Fatalf("inside the interval: %v, calls %d", err, conn.Calls())
	}
	// The server comes back. Before the interval ends nothing changes...
	healthy.Store(true)
	clock.Advance(30 * time.Second)
	if _, err := r.ID(ctx); err == nil {
		t.Fatal("retried before the interval")
	}
	// ...after it, the identity recovers: the failure was not kept for life.
	clock.Advance(31 * time.Second)
	id, err := r.ID(ctx)
	if err != nil || id != AgentID("ACME."+alicePub) {
		t.Fatalf("after the interval: %q, %v", id, err)
	}
	if got, ok := r.Peek(); !ok || got != id {
		t.Fatalf("Peek: %q %v", got, ok)
	}
	// Success is remembered for the connection's lifetime.
	calls := conn.Calls()
	for i := 0; i < 3; i++ {
		if again, err := r.ID(ctx); err != nil || again != id {
			t.Fatal(err)
		}
	}
	if conn.Calls() != calls {
		t.Fatal("a remembered ID asked the server again")
	}
	r.Reset()
	if _, err := r.ID(ctx); err != nil || conn.Calls() != calls+1 {
		t.Fatalf("after Reset: %v, calls %d", err, conn.Calls())
	}
}

// catalog: SAP-13.4-06
func TestSelfID_FailureKinds(t *testing.T) {
	cases := []struct {
		name  string
		reply func(context.Context, int) (*nats.Msg, error)
		kind  error
	}{
		{"timeout", blackhole, ErrIdentityUnknown},
		{"no responders", func(context.Context, int) (*nats.Msg, error) { return nil, nats.ErrNoResponders }, ErrIdentityUnknown},
		{"connection closed", func(context.Context, int) (*nats.Msg, error) { return nil, nats.ErrConnectionClosed }, ErrIdentityUnknown},
		{"password user", answer(`{"data":{"user":"alice","account":"$G"}}`), ErrNoIdentity},
		{"no authentication", answer(`{"data":{"user":"","account":"$G"}}`), ErrNoIdentity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewSelfIDResolver(&fakeConn{reply: tc.reply}, SelfIDOptions{Timeout: 20 * time.Millisecond})
			_, err := r.ID(context.Background())
			if !errors.Is(err, tc.kind) {
				t.Fatalf("got %v, want %v", err, tc.kind)
			}
		})
	}
}

// catalog: SAP-13.4-09
func TestSelfID_PermissionViolationFailsFast(t *testing.T) {
	conn := &fakeConn{reply: blackhole}
	r := NewSelfIDResolver(conn, SelfIDOptions{Timeout: 10 * time.Second})
	done := make(chan error, 1)
	start := time.Now()
	go func() { _, err := r.ID(context.Background()); done <- err }()
	deadline := time.Now().Add(2 * time.Second)
	for conn.Calls() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if r.ObserveError(errors.New("nats: unrelated")) {
		t.Fatal("an unrelated error was taken")
	}
	if !r.ObserveError(errors.New(`nats: Permissions Violation for Publish to "$SYS.REQ.USER.INFO"`)) {
		t.Fatal("the permissions violation was not taken")
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrIdentityUnknown) || !strings.Contains(err.Error(), "permissions violation") {
			t.Fatalf("got %v", err)
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal("did not fail fast")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting for the timeout")
	}
	if r.ObserveError(errors.New(`Permissions Violation for Publish to "$SYS.REQ.USER.INFO"`)) {
		t.Fatal("an error with no lookup in flight was taken")
	}
}

func TestSelfID_CallerCancelIsNotRemembered(t *testing.T) {
	conn := &fakeConn{reply: blackhole}
	r := NewSelfIDResolver(conn, SelfIDOptions{Timeout: 10 * time.Second, RetryInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, err := r.ID(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	conn.reply = answer(userInfoOK)
	if id, err := r.ID(context.Background()); err != nil || id == "" {
		t.Fatalf("a canceled wait was remembered as a failure: %v", err)
	}
}

func TestSelfID_ConcurrentCallersShareOneLookup(t *testing.T) {
	const callers = 10
	release := make(chan struct{})
	started := make(chan struct{}, callers)
	conn := &fakeConn{reply: func(ctx context.Context, n int) (*nats.Msg, error) {
		started <- struct{}{}
		<-release
		return answer(userInfoOK)(ctx, n)
	}}
	r := NewSelfIDResolver(conn, SelfIDOptions{})
	// The first lookup is held open until every other caller is provably
	// blocked on it, so none of them can have read a cached answer.
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.ID(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no lookup started")
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.waiters.Load() != callers-1 {
		if time.Now().After(deadline) {
			t.Fatalf("%d callers waiting on the in-flight lookup, want %d", r.waiters.Load(), callers-1)
		}
		runtime.Gosched()
	}
	select {
	case <-started:
		t.Error("a second lookup started while the first was in flight")
	default:
	}
	close(release)
	wg.Wait()
	if conn.Calls() != 1 {
		t.Fatalf("%d lookups", conn.Calls())
	}
}

func fakeJWT(t *testing.T, sub, iss, issuerAccount string) string {
	t.Helper()
	claims := map[string]any{"sub": sub, "iss": iss}
	if issuerAccount != "" {
		claims["nats"] = map[string]any{"issuer_account": issuerAccount}
	}
	raw, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"typ":"JWT"}`)) + "." + enc(raw) + "." + enc([]byte("sig"))
}

// catalog: SAP-13.4-01
func TestIdentityFromJWT(t *testing.T) {
	const acc = "AABYLMBR6Q2CDXTLGRQCFA2GP76BGCDF7NZF2OVHH4RQ7L3Y3TZWJDRL"
	const signingIssued = "ASIGNINGKEYNOTCHECKED"
	if id, err := IdentityFromJWT(fakeJWT(t, alicePub, acc, "")); err != nil || id != AgentID(acc+"."+alicePub) {
		t.Fatalf("iss: %q %v", id, err)
	}
	// A signing key issued the JWT: the account is nats.issuer_account.
	if id, err := IdentityFromJWT(fakeJWT(t, alicePub, signingIssued, acc)); err != nil || id.Account() != acc {
		t.Fatalf("issuer_account: %q %v", id, err)
	}
	for name, jwt := range map[string]string{
		"two parts":   "a.b",
		"not base64":  "a.!!!.c",
		"not JSON":    "a." + base64.RawURLEncoding.EncodeToString([]byte("x")) + ".c",
		"sub not key": fakeJWT(t, "alice", acc, ""),
		"bad account": fakeJWT(t, alicePub, "a.b", ""),
	} {
		if _, err := IdentityFromJWT(jwt); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// catalog: SAP-13.4-03 SAP-13.4-04
func TestSelfID_JWTSourceDoesNotAskTheServer(t *testing.T) {
	const acc = "AABYLMBR6Q2CDXTLGRQCFA2GP76BGCDF7NZF2OVHH4RQ7L3Y3TZWJDRL"
	jwt := fakeJWT(t, alicePub, acc, "")
	signer, err := SignerFromSeedAndJWT([]byte(aliceSeed), jwt)
	if err != nil {
		t.Fatal(err)
	}
	conn := &fakeConn{reply: answer(userInfoOK)}
	r := NewSelfIDResolver(conn, SelfIDOptions{Signer: signer})
	id, err := r.ID(context.Background())
	if err != nil || id != AgentID(acc+"."+alicePub) || conn.Calls() != 0 {
		t.Fatalf("got %q, %v, %d server calls (SAP §13.4: SHOULD NOT ask the server with a JWT in hand)", id, err, conn.Calls())
	}

	// Consulting both: they must agree, otherwise the identity is unknown.
	r2 := NewSelfIDResolver(&fakeConn{reply: answer(userInfoOK)}, SelfIDOptions{Signer: signer, ConsultServer: true})
	if _, err := r2.ID(context.Background()); !errors.Is(err, ErrIdentityUnknown) || !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("JWT says %s, server says ACME: %v", acc, err)
	}
	agree := `{"data":{"user":"` + alicePub + `","account":"` + acc + `"}}`
	r3 := NewSelfIDResolver(&fakeConn{reply: answer(agree)}, SelfIDOptions{Signer: signer, ConsultServer: true})
	if id, err := r3.ID(context.Background()); err != nil || id != AgentID(acc+"."+alicePub) {
		t.Fatalf("agreeing sources: %q %v", id, err)
	}
}

func TestSelfID_SignerMustMatchTheServersUser(t *testing.T) {
	signer := newSigner(t, bobSeed)
	r := NewSelfIDResolver(&fakeConn{reply: answer(userInfoOK)}, SelfIDOptions{Signer: signer})
	if _, err := r.ID(context.Background()); !errors.Is(err, ErrIdentityMismatch) || !errors.Is(err, ErrIdentityUnknown) {
		t.Fatalf("got %v", err)
	}
}

func TestSigner_FromSeedAndCreds(t *testing.T) {
	t.Run("seed with whitespace", func(t *testing.T) {
		s, err := SignerFromSeed([]byte("  " + aliceSeed + "\n"))
		if err != nil || s.PublicKey() != alicePub {
			t.Fatalf("%v", err)
		}
	})
	t.Run("seed in a BEGIN block", func(t *testing.T) {
		block := "-----BEGIN USER NKEY SEED-----\n" + aliceSeed + "\n------END USER NKEY SEED------\n"
		s, err := SignerFromSeed([]byte(block))
		if err != nil || s.PublicKey() != alicePub {
			t.Fatalf("%v", err)
		}
	})
	for name, seed := range map[string]string{
		"empty":        "",
		"public key":   alicePub,
		"bad checksum": aliceSeed[:len(aliceSeed)-1] + "A",
		"account seed": "SAAEJ6GDK6FSSB54LD45Q7W25AW7NUT7MVLBABIR5MIPFUTBW7ZNPK2KYE",
		"lower case":   strings.ToLower(aliceSeed),
		"two seeds":    aliceSeed + "\n" + bobSeed,
		"garbage":      "not a seed",
		"truncated":    aliceSeed[:20],
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := SignerFromSeed([]byte(seed))
			if !errors.Is(err, ErrInvalidSeed) {
				t.Fatalf("got %v", err)
			}
			if seed != "" && strings.Contains(err.Error(), seed) {
				t.Fatal("the error contains the seed")
			}
		})
	}
	t.Run("seed does not belong to the JWT", func(t *testing.T) {
		jwt := fakeJWT(t, bobPub, "AABYLMBR6Q2CDXTLGRQCFA2GP76BGCDF7NZF2OVHH4RQ7L3Y3TZWJDRL", "")
		if _, err := SignerFromSeedAndJWT([]byte(aliceSeed), jwt); !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("operator creds file", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join("testdata", "operator", "alice.creds"))
		if err != nil {
			t.Fatal(err)
		}
		s, err := SignerFromCreds(raw)
		if err != nil {
			t.Fatal(err)
		}
		id, err := IdentityFromJWT(s.JWT())
		if err != nil || id.User() != s.PublicKey() || len(id) != 113 {
			t.Fatalf("%q %v", id, err)
		}
	})
	t.Run("creds without a JWT", func(t *testing.T) {
		if _, err := SignerFromCreds([]byte(aliceSeed)); !errors.Is(err, ErrInvalidSeed) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("formatting never prints the seed", func(t *testing.T) {
		s := newSigner(t, aliceSeed)
		for _, out := range []string{
			strings.TrimSpace(fmt.Sprintf("%v|%+v|%s|%#v", s, s, s, s)),
			func() string { b, _ := json.Marshal(s); return string(b) }(),
		} {
			if strings.Contains(out, aliceSeed) || strings.Contains(out, aliceSeed[2:]) {
				t.Fatalf("seed leaked: %s", out)
			}
		}
	})
	t.Run("wipe", func(t *testing.T) {
		s := newSigner(t, aliceSeed)
		s.(SignerWiper).Wipe()
		if _, err := s.Sign([]byte("x")); err == nil {
			t.Fatal("a wiped signer signs")
		}
	})
}

func TestSelfID_RequestFailureKeepsItsCause(t *testing.T) {
	r := NewSelfIDResolver(&fakeConn{reply: func(context.Context, int) (*nats.Msg, error) { return nil, nats.ErrConnectionClosed }}, SelfIDOptions{Timeout: 20 * time.Millisecond})
	_, err := r.ID(context.Background())
	if !errors.Is(err, ErrIdentityUnknown) || !errors.Is(err, nats.ErrConnectionClosed) {
		t.Fatalf("got %v, want both ErrIdentityUnknown and the transport cause", err)
	}
}
