// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
)

func TestNewUUID_IsVersion4(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for range 100 {
		u := newUUID()
		if !re.MatchString(u) || seen[u] {
			t.Fatalf("bad or repeated uuid %q", u)
		}
		seen[u] = true
	}
}

func TestNewInbox_UnderSDKPrefix(t *testing.T) {
	a, b := newInbox(), newInbox()
	if a == b || !strings.HasPrefix(a, "_INBOX.agents.") {
		t.Fatalf("%q %q", a, b)
	}
	if err := agents.ValidatePublishSubject(a); err != nil {
		t.Fatal(err)
	}
}

func TestRunChain_OrderAndMisuse(t *testing.T) {
	var log []string
	mk := func(tag string) Interceptor {
		return InterceptorFunc(func(ctx context.Context, _ *RequestInfo, next CallNext) error {
			log = append(log, tag)
			return next(ctx)
		})
	}
	err := runChain(context.Background(), []Interceptor{mk("a"), mk("b")}, &RequestInfo{}, func(context.Context) error {
		log = append(log, "serve")
		return nil
	})
	if err != nil || strings.Join(log, ",") != "a,b,serve" {
		t.Fatalf("%v %v", err, log)
	}
	twice := InterceptorFunc(func(ctx context.Context, _ *RequestInfo, next CallNext) error {
		_ = next(ctx)
		return next(ctx)
	})
	calls := 0
	err = runChain(context.Background(), []Interceptor{twice}, &RequestInfo{}, func(context.Context) error { calls++; return nil })
	if !errors.Is(err, ErrNextCalledTwice) || calls != 1 {
		t.Fatalf("%v, serve ran %d times", err, calls)
	}
	// A nil context passed to next falls back to the caller's.
	nilCtx := InterceptorFunc(func(_ context.Context, _ *RequestInfo, next CallNext) error { return next(nil) })
	if err := runChain(context.Background(), []Interceptor{nilCtx}, &RequestInfo{}, func(ctx context.Context) error {
		if ctx == nil {
			t.Error("nil context reached serve")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRejectError_UnwrapsToServiceError(t *testing.T) {
	err := RejectWithBody(403, "no", agents.ErrorBody{Error: "denied"})
	var se *agents.ServiceError
	if !errors.As(err, &se) || se.Code != 403 || se.Body == nil || se.Body.Error != "denied" {
		t.Fatalf("%+v", se)
	}
	if !errors.Is(err, agents.ErrForbidden) || errors.Is(err, agents.ErrBadRequest) || !errors.Is(err, ErrRejected) {
		t.Fatal("sentinels")
	}
	if _, ok := agents.CodeOf(err); !ok {
		t.Fatal("CodeOf")
	}
	if !errors.Is(RejectWithBody(7, "x", agents.ErrorBody{}), ErrInvalidCode) {
		t.Fatal("invalid code with body")
	}
}

func TestSignHeartbeat_VerifiesAsLive(t *testing.T) {
	const seed = "SUACJSIK64RJ6WLD3JSDQUXM7DERPDXON6UKYY3MAFHX24M2L5TRAKTWAM"
	const pub = "UBTHWKVPPUINKFEZ6UZ6ZAWNT4RSRVVLELSMHWPMIRWRBFDCER5TSKLT"
	signer, err := identity.SignerFromSeed([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	id, err := identity.NewAgentID("$G", pub)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"agent":"a"}`)
	subj := "agents.hb.a.o.n"
	ts := identity.FormatTimestamp(time.Now())
	hdr, err := SignHeartbeat(context.Background(), SignHeartbeatOptions{
		Sender: HeartbeatSigner{ID: id, Signer: signer}, Subject: subj, TS: ts, Data: data,
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := identity.VerifyLive(identity.Message{Subject: subj, Data: data, Header: hdr},
		identity.LiveOptions{Nonces: identity.NewNonceSet(identity.NonceSetOptions{})})
	if err != nil || s == nil || s.ID != id || s.Header.TS != ts || s.Header.Sub != subj {
		t.Fatalf("%v %+v", err, s)
	}
	// A fixed nonce is honored (vectors).
	h2, err := SignHeartbeatHeader(context.Background(), SignHeartbeatOptions{
		Sender: HeartbeatSigner{ID: id, Signer: signer}, Subject: subj, TS: ts, Data: data, Nonce: "fixed-nonce",
	})
	if err != nil || h2.Nonce != "fixed-nonce" {
		t.Fatalf("%v %+v", err, h2)
	}
}

func TestCheckEndpoints(t *testing.T) {
	subj, err := agents.NewAgentSubject("a", "o", "n")
	if err != nil {
		t.Fatal(err)
	}
	ok := Endpoint{Name: "x", Subject: "x.y", Handler: nil}
	if err := checkEndpoints([]Endpoint{ok}, subj); err == nil {
		t.Fatal("a nil handler passed")
	}
}

// stubRequest is a micro.Request that records how it was answered.
type stubRequest struct {
	data      []byte
	errCode   string
	errDesc   string
	responded bool
}

func (r *stubRequest) Respond([]byte, ...micro.RespondOpt) error { r.responded = true; return nil }
func (r *stubRequest) RespondJSON(any, ...micro.RespondOpt) error {
	r.responded = true
	return nil
}
func (r *stubRequest) Error(code, desc string, _ []byte, _ ...micro.RespondOpt) error {
	r.errCode, r.errDesc = code, desc
	return nil
}
func (r *stubRequest) Data() []byte           { return r.data }
func (r *stubRequest) Headers() micro.Headers { return nil }
func (r *stubRequest) Subject() string        { return "agents.status.a.o.n" }
func (r *stubRequest) Reply() string          { return "_INBOX.x" }

// TestStatus_BuildFailureIs500 asserts SAP-8.7.1-03: a failure to build the
// status payload is returned as a 500 error response, not left to the
// framework. The failure is forced by an interval the heartbeat refuses.
func TestStatus_BuildFailureIs500(t *testing.T) {
	subj, err := agents.NewAgentSubject("a", "o", "n")
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(&nats.Conn{}, subj, func(context.Context, *Request, *Responder) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	h.id = "svc"
	h.state = stateRunning
	ok := &stubRequest{}
	h.onStatus(ok)
	if !ok.responded || ok.errCode != "" {
		t.Fatalf("a healthy host did not answer: %+v", ok)
	}
	h.cfg.hbInterval = 0
	bad := &stubRequest{}
	h.onStatus(bad)
	if bad.errCode != "500" || bad.errDesc != "status handler error" || bad.responded {
		t.Fatalf("build failure: %+v", bad)
	}
}

// TestDefaultConfig_KeepaliveIs30s: with no option the host keeps acking every
// DefaultKeepalive (30 s); WithKeepalive(0) and WithoutKeepalive turn it off and
// a negative cadence is refused.
func TestDefaultConfig_KeepaliveIs30s(t *testing.T) {
	apply := func(opts ...Option) (config, error) {
		c := defaultConfig()
		for _, o := range opts {
			if err := o(&c); err != nil {
				return c, err
			}
		}
		return c, nil
	}
	if c, _ := apply(); c.keepalive != DefaultKeepalive || DefaultKeepalive != 30*time.Second {
		t.Errorf("default keepalive = %s, want 30s", c.keepalive)
	}
	if c, err := apply(WithKeepalive(0)); err != nil || c.keepalive != 0 {
		t.Errorf("WithKeepalive(0) = %s, %v", c.keepalive, err)
	}
	if c, err := apply(WithoutKeepalive()); err != nil || c.keepalive != 0 {
		t.Errorf("WithoutKeepalive = %s, %v", c.keepalive, err)
	}
	if _, err := apply(WithKeepalive(-1)); !errors.Is(err, ErrConfig) {
		t.Errorf("negative keepalive: %v", err)
	}
}
