// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

type user struct{ pub, seed string }

// Throwaway test keys, the ones of identity/testdata/keys.json.
var (
	alice = user{"UCDUW5V44EBDBIK2FL4CTNDBQFNGBEZVJHSZGQVKRHASN4AV4IWPB5NT", "SUAEJ6GDK6FSSB54LD45Q7W25AW7NUT7MVLBABIR5MIPFUTBW7ZNPK2KYE"}
	bob   = user{"UDLPRVYPNSPIPYH4RVGKEHP2AAG4J2CN4GQ3IBTKC6ZDV6SR7ONG27D4", "SUAA37BA6WVMWPX27LE3AREHH5ELK2K5OYDECALTSZBJ7CDMW4AOC3GNAI"}
	carol = user{"UBTHWKVPPUINKFEZ6UZ6ZAWNT4RSRVVLELSMHWPMIRWRBFDCER5TSKLT", "SUACJSIK64RJ6WLD3JSDQUXM7DERPDXON6UKYY3MAFHX24M2L5TRAKTWAM"}
)

// idEnv is a server with three nkey users in the global account.
type idEnv struct {
	srv *server.Server
}

func startNkeyServer(t *testing.T) *idEnv {
	t.Helper()
	srv, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
		Nkeys: []*server.NkeyUser{{Nkey: alice.pub}, {Nkey: bob.pub}, {Nkey: carol.pub}},
	})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats-server did not become ready")
	}
	t.Cleanup(srv.Shutdown)
	return &idEnv{srv}
}

func (e *idEnv) conn(t *testing.T, u user) *nats.Conn {
	t.Helper()
	kp, err := nkeys.FromSeed([]byte(u.seed))
	if err != nil {
		t.Fatal(err)
	}
	return connect(t, e.srv, nats.Nkey(u.pub, kp.Sign))
}

func signerOf(t *testing.T, u user) identity.Signer {
	t.Helper()
	s, err := identity.SignerFromSeed([]byte(u.seed))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func agentID(t *testing.T, u user) identity.AgentID {
	t.Helper()
	id, err := identity.NewAgentID("$G", u.pub)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// signed returns the headers of a request signed by u for subject and payload;
// fix may adjust the sign options.
func signed(t *testing.T, u user, subject string, payload []byte, fix func(*identity.SignOptions)) nats.Header {
	t.Helper()
	o := identity.SignOptions{Signer: signerOf(t, u), ID: agentID(t, u), Subject: subject, Payload: payload}
	if fix != nil {
		fix(&o)
	}
	h, err := identity.SignHeader(o)
	if err != nil {
		t.Fatal(err)
	}
	hdr := nats.Header{}
	h.Set(hdr)
	return hdr
}

func rawHeader(v string) nats.Header {
	h := nats.Header{}
	h.Set("Agent-Sender", v)
	return h
}

// TestIdentity_Registration: a host with a seed registers user_nkey, account
// and an id_sig over its advertised prompt subject; one without a seed
// registers the unsigned pair; a host without identity registers nothing.
//
// catalog: SAP-13.10-01 SAP-13.10-02 SAP-13.10-03
func TestIdentity_Registration(t *testing.T) {
	env := startNkeyServer(t)
	nc := env.conn(t, carol)
	h := newHost(t, nc, echo, host.WithIdentity(host.IdentityOptions{Signer: signerOf(t, carol)}))
	id, ok := h.Identity()
	if !ok || id != agentID(t, carol) {
		t.Fatalf("identity %q %v", id, ok)
	}
	md := info(t, nc, h.InstanceID()).Metadata
	if md["user_nkey"] != carol.pub || md["account"] != "$G" || md["id_sig"] == "" {
		t.Fatalf("metadata %v", md)
	}
	promptSubj := h.Subject().Prompt()
	if !identity.VerifyAgentID(md, promptSubj) {
		t.Error("id_sig does not verify against the advertised prompt subject")
	}
	if identity.VerifyAgentID(md, h.Subject().Status()) {
		t.Error("id_sig verifies against another subject")
	}
	// The signed input is exactly AGENT-ID-V1 over the advertised subject.
	want, err := identity.SignAgentID(signerOf(t, carol), id, testAgent, testOwner, promptSubj)
	if err != nil || want != md["id_sig"] {
		t.Errorf("id_sig %q, want %q (%v)", md["id_sig"], want, err)
	}

	unsigned := newHostNamed(t, nc, "u", echo, host.WithIdentity(host.IdentityOptions{}))
	umd := info(t, nc, unsigned.InstanceID()).Metadata
	if umd["user_nkey"] != carol.pub || umd["account"] != "$G" {
		t.Errorf("unsigned registration %v", umd)
	}
	if _, has := umd["id_sig"]; has {
		t.Error("id_sig without a seed")
	}
	plain := newHostNamed(t, nc, "p", echo)
	if _, ok := plain.Identity(); ok {
		t.Error("identity without WithIdentity")
	}
	for _, k := range []string{"user_nkey", "account", "id_sig"} {
		if _, has := info(t, nc, plain.InstanceID()).Metadata[k]; has {
			t.Errorf("%s registered without identity", k)
		}
	}
}

// TestIdentity_SignerMustMatchConnection: a signer whose user is not the
// connection's user cannot register (SAP §13.4, §13.10).
func TestIdentity_SignerMustMatchConnection(t *testing.T) {
	env := startNkeyServer(t)
	nc := env.conn(t, carol)
	h, err := host.New(nc, subjectOf(t, testName), echo, host.WithIdentity(host.IdentityOptions{Signer: signerOf(t, alice)}))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Start(context.Background()); err == nil {
		_ = h.Close(context.Background())
		t.Fatal("Start accepted a signer that is not the connection's user")
	}
	if h.InstanceID() != "" && h.Service() != nil && !h.Service().Stopped() {
		t.Error("a failed Start left a service registered")
	}
	// A failed host cannot be started again, and Stop is still fine.
	if err := h.Start(context.Background()); !errors.Is(err, host.ErrStarted) {
		t.Errorf("second Start: %v", err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Error(err)
	}
}

// TestIdentity_SignedHeartbeats: a host with a seed attaches Agent-Sender to
// every heartbeat: sub is the heartbeat subject, ts the beat's own, a fresh
// nonce, the signature over the exact payload bytes; without a seed the beat
// carries no header and the same payload.
//
// catalog: SAP-13.11-01 SAP-13.11-02 SAP-13.11-03 SAP-13.11-04 SAP-13.11-05 SAP-13.11-06 SAP-13.6.2-04
func TestIdentity_SignedHeartbeats(t *testing.T) {
	env := startNkeyServer(t)
	nc := env.conn(t, carol)
	obs := env.conn(t, alice)
	sub := subscribeHeartbeats(t, obs)
	h := newHost(t, nc, echo, host.WithIdentity(host.IdentityOptions{Signer: signerOf(t, carol)}),
		host.WithHeartbeatInterval(60*time.Millisecond), host.WithSubSecondHeartbeats())
	nonces := identity.NewNonceSet(identity.NonceSetOptions{})
	seen := map[string]bool{}
	for range 4 {
		m, err := sub.NextMsg(2 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		hb, err := agents.DecodeHeartbeat(m.Data)
		if err != nil {
			t.Fatal(err)
		}
		hdr, ok, err := identity.ReadHeaderValue(m.Header)
		if err != nil || !ok {
			t.Fatalf("no Agent-Sender on a heartbeat: %v", err)
		}
		sh, err := identity.ParseHeader(hdr)
		if err != nil || sh == nil || !sh.Signed() {
			t.Fatalf("header: %v", err)
		}
		if sh.Sub != h.Subject().Heartbeat() || sh.Sub != m.Subject {
			t.Errorf("sub %q", sh.Sub)
		}
		if sh.TS != hb.TS {
			t.Errorf("ts %q is not the beat's own %q", sh.TS, hb.TS)
		}
		if seen[sh.Nonce] {
			t.Error("nonce reused")
		}
		seen[sh.Nonce] = true
		// Verified over the exact payload bytes, by the seed that signs id_sig.
		s, err := identity.VerifyLive(identity.MessageFromNATS(m), identity.LiveOptions{Nonces: nonces})
		if err != nil || s == nil || s.Trust != identity.TrustVerified || s.ID != agentID(t, carol) {
			t.Fatalf("verify: %v %+v", err, s)
		}
		tampered := identity.Message{Subject: m.Subject, Data: append([]byte(" "), m.Data...), Header: m.Header}
		if _, err := identity.VerifyStored(tampered, identity.StoredOptions{}); err == nil {
			t.Error("a heartbeat with other bytes verifies")
		}
	}

	// No seed: no header, same payload shape.
	plainSub := subscribeHeartbeats(t, obs)
	_ = newHostNamed(t, nc, "plain", echo, host.WithHeartbeatInterval(60*time.Millisecond), host.WithSubSecondHeartbeats())
	for {
		m, err := plainSub.NextMsg(2 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(m.Subject, ".plain") {
			continue
		}
		if _, has := m.Header["Agent-Sender"]; has {
			t.Error("a heartbeat without a seed carries Agent-Sender")
		}
		if _, err := agents.DecodeHeartbeat(m.Data); err != nil {
			t.Error(err)
		}
		break
	}
}

// TestIdentity_StatusReplyNeverSigned: no reply carries Agent-Sender, the
// status reply included (SAP §13.11).
//
// catalog: SAP-13.11-08
func TestIdentity_StatusReplyNeverSigned(t *testing.T) {
	env := startNkeyServer(t)
	nc := env.conn(t, carol)
	cl := env.conn(t, alice)
	h := newHost(t, nc, echo, host.WithIdentity(host.IdentityOptions{Signer: signerOf(t, carol)}))
	m, _ := status(t, cl, h, nil, signed(t, alice, h.Subject().Status(), nil, nil))
	if len(m.Header["Agent-Sender"]) != 0 {
		t.Error("the status reply is signed")
	}
	s := rawPrompt(t, cl, h.Subject().Prompt(), []byte("x"), nil, 5*time.Second)
	for i, f := range s.Frames {
		if _, has := f.Header["Agent-Sender"]; has {
			t.Errorf("frame %d of a prompt reply is signed", i)
		}
	}
}

// gateCase is one request against a prompt endpoint.
type gateCase struct {
	name    string
	hdr     func(t *testing.T, subj string, payload []byte) nats.Header
	any     int // expected code at min_sender_trust any, 0 = served
	signed  int // expected code at min_sender_trust signed
	trust   identity.Trust
	payload string
}

func sign(u user, fix func(*identity.SignOptions)) func(t *testing.T, subj string, payload []byte) nats.Header {
	return func(t *testing.T, subj string, payload []byte) nats.Header { return signed(t, u, subj, payload, fix) }
}

// TestGate_Dispatch: how each class of sender fares at an endpoint that
// requires nothing and at one that requires a signature.
//
// catalog: SAP-13.7-01 SAP-13.5.1-06 SAP-13.7-07 SAP-13.7-08 SAP-13.7-10 SAP-13.7-11 SAP-13.9-03 SAP-13.1-01
func TestGate_Dispatch(t *testing.T) {
	env := startNkeyServer(t)
	nc := env.conn(t, carol)
	cl := env.conn(t, alice)
	claim, err := identity.ClaimHeader(agentID(t, alice), "alice")
	if err != nil {
		t.Fatal(err)
	}
	cases := []gateCase{
		{"absent", func(*testing.T, string, []byte) nats.Header { return nil }, 0, 401, identity.TrustAbsent, "x"},
		{"claimed", func(*testing.T, string, []byte) nats.Header { return rawHeader(claim.Encode()) }, 0, 401, identity.TrustClaimed, "x"},
		{"verified", sign(alice, nil), 0, 0, identity.TrustVerified, "x"},
		{"bad signature stays 401 at any", sign(alice, func(o *identity.SignOptions) { o.Payload = []byte("other") }), 401, 401, "", "x"},
		{"malformed is 400", func(*testing.T, string, []byte) nats.Header { return rawHeader("{") }, 400, 400, "", "x"},
		{"unknown version is absent", func(*testing.T, string, []byte) nats.Header {
			return rawHeader(`{"v":2,"account":"$G","user":"` + alice.pub + `","future":true}`)
		}, 0, 401, identity.TrustAbsent, "x"},
	}
	anyHost := newHost(t, nc, sawSender, host.WithMinSenderTrust(identity.TrustAny))
	signedHost := newHostNamed(t, nc, "strict", sawSender, host.WithMinSenderTrust(identity.TrustSigned))
	if got := info(t, nc, signedHost.InstanceID()); endpointOf(t, got, "prompt").Metadata["min_sender_trust"] != "signed" {
		t.Fatal("signed not advertised")
	}
	for _, tc := range cases {
		for _, ep := range []struct {
			label string
			h     *host.Host
			want  int
		}{{"any", anyHost, tc.any}, {"signed", signedHost, tc.signed}} {
			t.Run(tc.name+"/"+ep.label, func(t *testing.T) {
				payload := []byte(tc.payload)
				s := rawPrompt(t, cl, ep.h.Subject().Prompt(), payload, tc.hdr(t, ep.h.Subject().Prompt(), payload), 5*time.Second)
				if ep.want == 0 {
					if s.errorFrame() != nil {
						t.Fatalf("refused: %+v", s.errorFrame())
					}
					if got := s.texts(t); len(got) != 1 || got[0] != string(tc.trust) {
						t.Fatalf("handler saw %v, want trust %q", got, tc.trust)
					}
					return
				}
				se := s.errorFrame()
				if se == nil || se.Code != ep.want {
					t.Fatalf("want %d, got %+v", ep.want, se)
				}
				// Refusal: error frame then terminator, no ack, no handler.
				if len(s.Frames) != 2 || !s.last().terminator() || s.acks() != 0 {
					t.Errorf("refusal shape: %d frames, %d acks", len(s.Frames), s.acks())
				}
				wantDesc := map[int]string{400: identity.DescriptionMalformed, 401: identity.DescriptionRejected, 403: identity.DescriptionRejected}[ep.want]
				if ep.want == 401 && tc.trust != "" {
					wantDesc = identity.DescriptionSignatureRequired
				}
				if se.Description != wantDesc {
					t.Errorf("description %q, want %q", se.Description, wantDesc)
				}
			})
		}
	}
}

// sawSender echoes the trust class of the sender the handler sees.
func sawSender(ctx context.Context, req *host.Request, w *host.Responder) error {
	return w.Send(ctx, string(identity.TrustOf(req.Sender)))
}

// TestGate_VerificationChecks: the checks a signed header passes: signature
// over the payload, sub equal to the arrival subject, ts inside the window,
// a nonce seen once.
//
// catalog: SAP-13.7-03 SAP-13.7-04 SAP-13.7-05 SAP-13.7-06 SAP-13.5-08 SAP-13.5.1-04 SAP-13.5.1-05 SAP-13.5.1-06
func TestGate_VerificationChecks(t *testing.T) {
	env := startNkeyServer(t)
	nc := env.conn(t, carol)
	cl := env.conn(t, alice)
	h := newHost(t, nc, sawSender, host.WithReplayWindow(30*time.Second))
	subj := h.Subject().Prompt()
	now := time.Now().UTC()
	replayed := signed(t, alice, subj, []byte("x"), nil)
	cases := []struct {
		name string
		hdr  nats.Header
		want int
	}{
		{"valid", signed(t, alice, subj, []byte("x"), nil), 0},
		{"fractional seconds ts", signed(t, alice, subj, []byte("x"), func(o *identity.SignOptions) {
			o.TS = now.Format("2006-01-02T15:04:05.000Z")
		}), 0},
		{"payload differs from the signed one", signed(t, alice, subj, []byte("not x"), nil), 401},
		{"sub is another subject", signed(t, alice, h.Subject().Status(), []byte("x"), nil), 401},
		{"ts too old", signed(t, alice, subj, []byte("x"), func(o *identity.SignOptions) {
			o.TS = identity.FormatTimestamp(now.Add(-2 * time.Minute))
		}), 401},
		{"ts too far ahead", signed(t, alice, subj, []byte("x"), func(o *identity.SignOptions) {
			o.TS = identity.FormatTimestamp(now.Add(2 * time.Minute))
		}), 401},
		{"signed by another user's seed", forgedUser(t, subj), 401},
		{"replay, first use", replayed, 0},
		{"replay, second use", replayed, 401},
		{"version as a string", rawHeader(`{"v":"1","account":"$G","user":"` + alice.pub + `"}`), 400},
		{"no user", rawHeader(`{"v":1,"account":"$G"}`), 400},
		{"sig without nonce", rawHeader(`{"v":1,"account":"$G","user":"` + alice.pub + `","sub":"a","ts":"2026-01-01T00:00:00Z","sig":"x"}`), 400},
		{"duplicate member", rawHeader(`{"v":1,"v":1,"account":"$G","user":"` + alice.pub + `"}`), 400},
		{"not an object", rawHeader(`[1]`), 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := rawPrompt(t, cl, subj, []byte("x"), tc.hdr, 5*time.Second)
			if tc.want == 0 {
				if got := s.texts(t); s.errorFrame() != nil || len(got) != 1 || got[0] != "verified" {
					t.Fatalf("not served as verified: %v %+v", got, s.errorFrame())
				}
				return
			}
			if se := s.errorFrame(); se == nil || se.Code != tc.want {
				t.Fatalf("want %d, got %+v", tc.want, se)
			}
		})
	}
}

// forgedUser is a header signed by bob's seed that names alice as the user.
func forgedUser(t *testing.T, subj string) nats.Header {
	t.Helper()
	sh, err := identity.ParseHeader(signed(t, bob, subj, []byte("x"), nil).Get("Agent-Sender"))
	if err != nil || sh == nil {
		t.Fatal(err)
	}
	sh.User = alice.pub
	return rawHeader(sh.Encode())
}

// TestGate_NonceSetFailsClosed: a full replay set refuses further signed
// requests rather than dropping an entry (SDK policy, SAP §13.7).
func TestGate_NonceSetFailsClosed(t *testing.T) {
	env := startNkeyServer(t)
	nc := env.conn(t, carol)
	cl := env.conn(t, alice)
	h := newHost(t, nc, sawSender, host.WithMaxNonces(2), host.WithMaxNoncesPerSender(2))
	subj := h.Subject().Prompt()
	first := signed(t, alice, subj, []byte("x"), nil)
	for i, hdr := range []nats.Header{first, signed(t, alice, subj, []byte("x"), nil)} {
		if s := rawPrompt(t, cl, subj, []byte("x"), hdr, 5*time.Second); s.errorFrame() != nil {
			t.Fatalf("request %d refused: %+v", i, s.errorFrame())
		}
	}
	if se := rawPrompt(t, cl, subj, []byte("x"), signed(t, alice, subj, []byte("x"), nil), 5*time.Second).errorFrame(); se == nil || se.Code != 401 {
		t.Fatalf("a full set did not refuse: %+v", se)
	}
	if se := rawPrompt(t, cl, subj, []byte("x"), first, 5*time.Second).errorFrame(); se == nil || se.Code != 401 {
		t.Fatalf("an entry was dropped to make room: %+v", se)
	}
	if h.Gate().Nonces().Len() != 2 {
		t.Errorf("set size %d", h.Gate().Nonces().Len())
	}
}

// TestGate_AdmissionHook: the hook sees the verified sender with headers and
// subject; false is 403 for a verified sender and 401 otherwise; an error is
// 500 and the request is never served.
//
// catalog: SAP-13.7-12 SAP-13.7-13 SAP-13.7.3-01 SAP-13.7.3-02
func TestGate_AdmissionHook(t *testing.T) {
	env := startNkeyServer(t)
	nc := env.conn(t, carol)
	cl := env.conn(t, alice)
	claim, _ := identity.ClaimHeader(agentID(t, alice), "")
	type seen struct {
		trust   identity.Trust
		id      identity.AgentID
		subject string
		header  string
		isNil   bool
	}
	var last atomic.Value
	mode := atomic.Value{}
	mode.Store("refuse")
	var calls atomic.Int32
	var served atomic.Int32
	hook := func(ctx context.Context, s *identity.Sender, req *host.RequestInfo) (bool, error) {
		calls.Add(1)
		sn := seen{trust: identity.TrustOf(s), subject: req.Subject, header: req.Header.Get("X-App"), isNil: req.Sender == nil}
		if s != nil {
			sn.id = s.ID
		}
		last.Store(sn)
		switch mode.Load().(string) {
		case "refuse":
			return false, nil
		case "error":
			return false, errors.New("directory down: secret")
		case "panic":
			panic("hook bug")
		}
		return true, nil
	}
	h := newHost(t, nc, func(ctx context.Context, req *host.Request, w *host.Responder) error {
		served.Add(1)
		return w.Send(ctx, "in")
	}, host.WithAdmission(hook))
	subj := h.Subject().Prompt()
	withApp := func(h nats.Header) nats.Header {
		if h == nil {
			h = nats.Header{}
		}
		h.Set("X-App", "tag")
		return h
	}
	cases := []struct {
		name string
		mode string
		hdr  nats.Header
		code int
		desc string
	}{
		{"refuses a verified sender: 403", "refuse", signed(t, alice, subj, []byte("x"), nil), 403, identity.DescriptionRejected},
		{"refuses a claimed sender: 401", "refuse", rawHeader(claim.Encode()), 401, identity.DescriptionSignatureRequired},
		{"refuses an absent sender: 401", "refuse", nil, 401, identity.DescriptionSignatureRequired},
		{"hook error: 500", "error", signed(t, alice, subj, []byte("x"), nil), 500, identity.DescriptionServerError},
		{"hook panic: 500", "panic", nil, 500, identity.DescriptionServerError},
		{"accepts", "accept", signed(t, alice, subj, []byte("x"), nil), 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode.Store(tc.mode)
			before := served.Load()
			s := rawPrompt(t, cl, subj, []byte("x"), withApp(tc.hdr), 5*time.Second)
			if tc.code == 0 {
				if s.errorFrame() != nil || served.Load() != before+1 {
					t.Fatalf("not served: %+v", s.errorFrame())
				}
				sn := last.Load().(seen)
				if sn.trust != identity.TrustVerified || sn.id != agentID(t, alice) || sn.subject != subj || sn.header != "tag" || sn.isNil {
					t.Errorf("hook saw %+v", sn)
				}
				return
			}
			se := s.errorFrame()
			if se == nil || se.Code != tc.code || se.Description != tc.desc {
				t.Fatalf("got %+v", se)
			}
			if strings.Contains(se.Description, "secret") || served.Load() != before || s.acks() != 0 {
				t.Error("served, acked, or leaked")
			}
		})
	}
	// The hook does not run for a malformed header, nor for status.
	mode.Store("refuse")
	n := calls.Load()
	rawPrompt(t, cl, subj, []byte("x"), rawHeader("{"), 5*time.Second)
	status(t, cl, h, nil, nil)
	if calls.Load() != n {
		t.Error("the hook ran for a malformed header or for status")
	}
}

// TestStatus_NeverRefusedOnIdentityGrounds: at an endpoint that requires a
// signature, a status request is answered whoever asks: unsigned, claimed, a
// bad signature, a replay, a malformed header, even when the admission hook
// refuses everyone.
//
// catalog: SAP-13.9-06 SAP-13.9-07 SAP-13.9-05
func TestStatus_NeverRefusedOnIdentityGrounds(t *testing.T) {
	env := startNkeyServer(t)
	nc := env.conn(t, carol)
	cl := env.conn(t, alice)
	var hookCalls atomic.Int32
	h := newHost(t, nc, echo, host.WithMinSenderTrust(identity.TrustSigned),
		host.WithAdmission(func(context.Context, *identity.Sender, *host.RequestInfo) (bool, error) {
			hookCalls.Add(1)
			return false, nil
		}))
	subj := h.Subject().Status()
	claim, _ := identity.ClaimHeader(agentID(t, alice), "a")
	replay := signed(t, alice, subj, nil, nil)
	cases := []struct {
		name string
		hdr  nats.Header
	}{
		{"unsigned", nil},
		{"claimed", rawHeader(claim.Encode())},
		{"verified", signed(t, alice, subj, nil, nil)},
		{"bad signature", signed(t, alice, subj, []byte("body"), nil)},
		{"stale ts", signed(t, alice, subj, nil, func(o *identity.SignOptions) { o.TS = identity.FormatTimestamp(time.Now().Add(-time.Hour)) })},
		{"replay 1", replay},
		{"replay 2", replay},
		{"malformed", rawHeader("{{")},
		{"unknown version", rawHeader(`{"v":9,"account":"x","user":"y"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, hb := status(t, cl, h, nil, tc.hdr)
			if hb.InstanceID != h.InstanceID() {
				t.Errorf("reply %+v", hb)
			}
		})
	}
	if hookCalls.Load() != 0 {
		t.Error("the admission hook ran for status")
	}
	st := endpointOf(t, info(t, nc, h.InstanceID()), "status")
	if _, has := st.Metadata["min_sender_trust"]; has {
		t.Error("status declares min_sender_trust")
	}
}

// TestIdentity_ClaimedHasNoID: the handler sees a claimed sender without an ID
// to key on, and a verified sender resolves back to the agent that registered
// its key (SAP §13.1, §13.10).
func TestIdentity_ClaimedHasNoID(t *testing.T) {
	env := startNkeyServer(t)
	hostConn := env.conn(t, carol)
	aliceConn := env.conn(t, alice)
	got := make(chan *host.Request, 4)
	capture := func(ctx context.Context, req *host.Request, w *host.Responder) error {
		got <- req
		return nil
	}
	h := newHost(t, hostConn, capture)
	// Alice also runs an agent, registered with her seed.
	aliceAgent := newHostNamed(t, aliceConn, "alice-agent", echo, host.WithIdentity(host.IdentityOptions{Signer: signerOf(t, alice)}))

	claim, _ := identity.ClaimHeader(agentID(t, alice), "Mallory")
	rawPrompt(t, aliceConn, h.Subject().Prompt(), []byte("x"), rawHeader(claim.Encode()), 5*time.Second)
	req := <-got
	if req.Sender == nil || req.Sender.Trust != identity.TrustClaimed || req.Sender.ID != "" || req.Sender.Name != "Mallory" {
		t.Fatalf("claimed sender: %+v", req.Sender)
	}
	if _, ok, err := req.ResolveSender(context.Background()); ok || err != nil {
		t.Errorf("a claimed sender resolved: %v %v", ok, err)
	}

	rawPrompt(t, aliceConn, h.Subject().Prompt(), []byte("x"), signed(t, alice, h.Subject().Prompt(), []byte("x"), nil), 5*time.Second)
	req = <-got
	if req.Sender == nil || req.Sender.Trust != identity.TrustVerified || req.Sender.ID != agentID(t, alice) {
		t.Fatalf("verified sender: %+v", req.Sender)
	}
	reg, ok, err := req.ResolveSender(context.Background())
	if err != nil || !ok || reg.ID != aliceAgent.InstanceID() {
		t.Fatalf("resolve: %+v %v %v", reg, ok, err)
	}

	// Bob has no agent: a verified sender that is not a reachable agent.
	bobConn := env.conn(t, bob)
	rawPrompt(t, bobConn, h.Subject().Prompt(), []byte("x"), signed(t, bob, h.Subject().Prompt(), []byte("x"), nil), 5*time.Second)
	req = <-got
	if _, ok, err := req.ResolveSender(context.Background()); ok || err != nil {
		t.Errorf("bob resolved: %v %v", ok, err)
	}
}

// TestInterop_PeerWithoutIdentity: a host with identity support serves a
// request with no identity at all as an absent sender when it requires none,
// and one that sends an unknown Agent-Sender version is served unchanged.
//
// catalog: SAP-13.13-01 SAP-13.13-02 SAP-13.5.1-03
func TestInterop_PeerWithoutIdentity(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, sawSender)
	for name, hdr := range map[string]nats.Header{
		"no header": nil,
		"unknown v": rawHeader(`{"v":7,"account":"A","user":"U","sig":"x"}`),
	} {
		s := rawPrompt(t, nc, h.Subject().Prompt(), []byte("x"), hdr, 5*time.Second)
		if got := s.texts(t); s.errorFrame() != nil || len(got) != 1 || got[0] != "absent" {
			t.Errorf("%s: %v %+v", name, got, s.errorFrame())
		}
	}
	// Unknown fields of a well-formed header are ignored: it is still a claim.
	hdr := rawHeader(`{"v":1,"account":"$G","user":"` + alice.pub + `","future":{"x":1}}`)
	s := rawPrompt(t, nc, h.Subject().Prompt(), []byte("x"), hdr, 5*time.Second)
	if got := s.texts(t); s.errorFrame() != nil || len(got) != 1 || got[0] != "claimed" {
		t.Errorf("unknown header field: %v %+v", got, s.errorFrame())
	}
}
