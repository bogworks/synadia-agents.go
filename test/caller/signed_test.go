// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
)

// signedFixture is the nkey server of the identity fixtures, a fake host and a
// client, all connected as alice.
func signedFixture(t *testing.T, mods []func(*host), opts ...caller.Option) *fixture {
	t.Helper()
	srv := startNkeyServer(t)
	hnc, cnc := connectAlice(t, srv), connectAlice(t, srv)
	h := newHost(t, hnc, mods...).start()
	c := newClient(t, cnc, opts...)
	a, err := c.LookupInstance(ctxT(t), h.id)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{srv: srv, h: h, c: c, a: a, cnc: cnc}
}

func withSigner(t *testing.T, name string) caller.Option {
	t.Helper()
	return caller.WithIdentity(caller.IdentityOptions{Signer: aliceSigner(t), Name: name})
}

func aliceID(t *testing.T) identity.AgentID {
	t.Helper()
	id, err := identity.NewAgentID("$G", alice.pub)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func liveOpts() identity.LiveOptions {
	return identity.LiveOptions{Nonces: identity.NewNonceSet(identity.NonceSetOptions{})}
}

// verifyLive classifies a message the way a receiver does (SAP §13.7).
func verifyLive(t *testing.T, m *nats.Msg) *identity.Sender {
	t.Helper()
	s, err := identity.VerifyLive(identity.MessageFromNATS(m), liveOpts())
	if err != nil {
		t.Fatalf("live verification of the request failed: %v", err)
	}
	return s
}

// TestSigned_PromptCarriesAgentSender asserts the signed path of a prompt: one
// Agent-Sender header whose v is the number 1, whose account and user are the
// caller's agent ID, whose sub is the subject published to, with an RFC 3339
// ts and a nonce and a signature made with the caller's seed, which a receiver
// verifies live. A host that knows nothing of the extension interoperates.
//
// catalog: SAP-13.5-01
// catalog: SAP-13.5-03
// catalog: SAP-13.5-04
// catalog: SAP-13.5-06
// catalog: SAP-13.5-09
// catalog: SAP-13.5-10
// catalog: SAP-13.5-12
// catalog: SAP-13.6.1-01
func TestSigned_PromptCarriesAgentSender(t *testing.T) {
	f := signedFixture(t, nil, withSigner(t, ""))
	st, err := f.a.PromptText(ctxT(t), "signed hello")
	if err != nil {
		t.Fatal(err)
	}
	evs, err := collect(t, st)
	if err != nil || texts(evs) != "echo: signed hello" {
		t.Fatalf("a peer that ignores the header must still work: %q %v", texts(evs), err)
	}
	m := f.h.nextPrompt(t)
	if vs := m.Header.Values(agents.HeaderAgentSender); len(vs) != 1 {
		t.Fatalf("%d Agent-Sender headers, want exactly 1", len(vs))
	}
	raw := m.Header.Get(agents.HeaderAgentSender)
	if !strings.Contains(raw, `"v":1`) || strings.Contains(raw, `"v":"1"`) {
		t.Fatalf("v is not the JSON number 1: %s", raw)
	}
	sender := verifyLive(t, m)
	if identity.TrustOf(sender) != identity.TrustVerified || sender.ID != aliceID(t) {
		t.Fatalf("sender %+v", sender)
	}
	h := sender.Header
	if h.Account != "$G" || h.User != alice.pub {
		t.Fatalf("account/user %q %q", h.Account, h.User)
	}
	if id, err := h.ID(); err != nil || id != aliceID(t) {
		t.Fatalf("%v %v", id, err)
	}
	if h.Sub != f.h.promptSubject || h.Sub != m.Subject {
		t.Fatalf("sub %q, published to %q", h.Sub, m.Subject)
	}
	if _, err := identity.ParseTimestamp(h.TS); err != nil || !identity.ValidNonce(h.Nonce) || h.Sig == "" {
		t.Fatalf("ts %q nonce %q sig %q", h.TS, h.Nonce, h.Sig)
	}
	// A different seed's signature does not verify for this user: the signature
	// is the caller's own.
	if len(h.Sig) != 86 {
		t.Fatalf("signature length %d", len(h.Sig))
	}
}

// TestSigned_StatusRequestCarriesAgentSender asserts that a status request is
// signed too, over its empty body, and that the signature names the status
// subject.
//
// catalog: SAP-13.5-02
func TestSigned_StatusRequestCarriesAgentSender(t *testing.T) {
	f := signedFixture(t, nil, withSigner(t, ""))
	if _, err := f.a.Status(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	m := <-f.h.statuses
	if len(m.Data) != 0 {
		t.Fatalf("status body %q", m.Data)
	}
	if vs := m.Header.Values(agents.HeaderAgentSender); len(vs) != 1 {
		t.Fatalf("%d Agent-Sender headers, want exactly 1", len(vs))
	}
	s := verifyLive(t, m)
	if identity.TrustOf(s) != identity.TrustVerified || s.Header.Sub != f.h.statusSubject {
		t.Fatalf("%+v", s)
	}
}

// TestSigned_HeaderWireForm asserts the wire form of the header: its exact
// name, compact one-line JSON, a second-precision UTC ts, a NUID-shaped nonce,
// and a display name that is outside the signature.
//
// catalog: SAP-13.5-05
// catalog: SAP-13.5-07
// catalog: SAP-13.5-11
// catalog: SAP-13.5.1-01
// catalog: SAP-13.5.1-02
func TestSigned_HeaderWireForm(t *testing.T) {
	f := signedFixture(t, nil, withSigner(t, "Ålice the caller"))
	st, err := f.a.PromptText(ctxT(t), "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collect(t, st); err != nil {
		t.Fatal(err)
	}
	m := f.h.nextPrompt(t)
	if _, ok := m.Header["Agent-Sender"]; !ok {
		t.Fatalf("headers %v: the name is exactly Agent-Sender", m.Header)
	}
	raw := m.Header.Get("Agent-Sender")
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(raw)); err != nil || compact.String() != raw || strings.ContainsAny(raw, "\r\n") {
		t.Fatalf("the header value is not compact one-line JSON: %q", raw)
	}
	ph, err := identity.ParseHeader(raw)
	if err != nil || ph == nil {
		t.Fatal(ph, err)
	}
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`).MatchString(ph.TS) {
		t.Fatalf("ts %q is not second precision with the Z suffix", ph.TS)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9]{22}$`).MatchString(ph.Nonce) {
		t.Fatalf("nonce %q is not NUID-shaped", ph.Nonce)
	}
	if ph.Name != "Ålice the caller" {
		t.Fatalf("name %q", ph.Name)
	}
	// The name is display only: changing it leaves the signature valid.
	changed := *ph
	changed.Name = "somebody else"
	m2 := &nats.Msg{Subject: m.Subject, Data: m.Data, Header: nats.Header{}}
	changed.Set(m2.Header)
	if s := verifyLive(t, m2); identity.TrustOf(s) != identity.TrustVerified || s.Name != "somebody else" {
		t.Fatalf("the name is part of the signature: %+v", s)
	}
}

// TestSigned_EndpointRequirement asserts the declared-requirement rules: an
// endpoint that requires a signed sender, or declares a value the caller does
// not know, is refused locally and without publishing when the caller cannot
// sign; with a signer it is served; an unknown identity sends no header to a
// permissive endpoint and fails locally against a signed one.
//
// catalog: SAP-13.9-04
// catalog: SAP-13.4-10
// catalog: SAP-13.4-11
func TestSigned_EndpointRequirement(t *testing.T) {
	t.Run("signed endpoint, caller without a signer", func(t *testing.T) {
		f := signedFixture(t, mod(func(h *host) { h.minTrust = "signed" }))
		_, err := f.a.PromptText(ctxT(t), "x")
		if !errors.Is(err, identity.ErrCannotSign) || !errors.Is(err, agents.ErrLocal) {
			t.Fatalf("error %v", err)
		}
		f.h.barrier(t, f.cnc)
		if f.h.promptsSeen() != 0 {
			t.Fatal("a prompt was published to an endpoint that requires a signature")
		}
	})
	t.Run("unknown value is read as signed", func(t *testing.T) {
		f := signedFixture(t, mod(func(h *host) { h.minTrust = "banana" }))
		if got := f.a.MinSenderTrust(); got != identity.TrustSigned {
			t.Fatalf("an unknown min_sender_trust reads as %q", got)
		}
		if _, err := f.a.PromptText(ctxT(t), "x"); !errors.Is(err, identity.ErrCannotSign) {
			t.Fatalf("error %v", err)
		}
		f.h.barrier(t, f.cnc)
		if f.h.promptsSeen() != 0 {
			t.Fatal("published")
		}
	})
	t.Run("signed endpoint, caller with a signer", func(t *testing.T) {
		f := signedFixture(t, mod(func(h *host) { h.minTrust = "signed" }), withSigner(t, ""))
		st, err := f.a.PromptText(ctxT(t), "x")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := collect(t, st); err != nil {
			t.Fatal(err)
		}
		if s := verifyLive(t, f.h.nextPrompt(t)); identity.TrustOf(s) != identity.TrustVerified {
			t.Fatal("not verified")
		}
	})
	t.Run("endpoint that requires nothing gets no header from a caller without identity", func(t *testing.T) {
		f := signedFixture(t, mod(func(h *host) { h.minTrust = "any" }))
		st, err := f.a.PromptText(ctxT(t), "x")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := collect(t, st); err != nil {
			t.Fatal(err)
		}
		if m := f.h.nextPrompt(t); len(m.Header) != 0 {
			t.Fatalf("headers %v", m.Header)
		}
	})
	t.Run("unknown identity", func(t *testing.T) {
		// A server with no accounts and no system account does not answer
		// $SYS.REQ.USER.INFO: the identity is unknown and must not be guessed.
		srv := startServer(t)
		hnc, cnc := connect(t, srv), connect(t, srv)
		perm := newHost(t, hnc, func(h *host) { h.minTrust = "any" }).start()
		req := newHost(t, hnc, func(h *host) { h.minTrust = "signed" }).start()
		c := newClient(t, cnc, caller.WithIdentity(caller.IdentityOptions{Signer: aliceSigner(t), LookupTimeout: 200 * time.Millisecond}))
		pa, err := c.LookupInstance(ctxT(t), perm.id)
		if err != nil {
			t.Fatal(err)
		}
		st, err := pa.PromptText(ctxT(t), "x")
		if err != nil {
			t.Fatalf("a permissive endpoint must still be served: %v", err)
		}
		if _, err := collect(t, st); err != nil {
			t.Fatal(err)
		}
		if m := perm.nextPrompt(t); len(m.Header) != 0 {
			t.Fatalf("a caller with an unknown identity sent headers %v", m.Header)
		}
		ra, err := c.LookupInstance(ctxT(t), req.id)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ra.PromptText(ctxT(t), "x")
		if !errors.Is(err, identity.ErrCannotSign) || !errors.Is(err, agents.ErrLocal) {
			t.Fatalf("error %v", err)
		}
		req.barrier(t, cnc)
		if req.promptsSeen() != 0 {
			t.Fatal("a prompt was published by a caller with no identity to a signed endpoint")
		}
	})
}

// TestSigned_HeaderBytesCountAgainstMaxPayload asserts that the caller counts
// the Agent-Sender header's bytes in its local max_payload check: a payload
// that fits alone is refused once the header is added, and the refusal says how
// many header bytes were counted.
//
// catalog: SAP-13.5.1-07
func TestSigned_HeaderBytesCountAgainstMaxPayload(t *testing.T) {
	mods := mod(func(h *host) { h.maxPayload = "1KB" })
	env := agents.Envelope{Prompt: strings.Repeat("x", 900)}
	size, err := env.EncodedSize()
	if err != nil || size >= 1024 || size < 900 {
		t.Fatal(size, err)
	}
	signed := signedFixture(t, mods, withSigner(t, ""))
	_, err = signed.a.Prompt(ctxT(t), env)
	var pl *agents.PayloadTooLargeError
	if !errors.As(err, &pl) || pl.Connection || pl.HeaderBytes <= 0 || pl.Limit != 1024 || !errors.Is(err, agents.ErrPayloadTooLarge) {
		t.Fatalf("error %v (%+v)", err, pl)
	}
	signed.h.barrier(t, signed.cnc)
	if signed.h.promptsSeen() != 0 {
		t.Fatal("published")
	}
	// The same prompt from a caller that sends no header fits.
	plain := signedFixture(t, mods)
	st, err := plain.a.Prompt(ctxT(t), env)
	if err != nil {
		t.Fatalf("the same prompt without a header: %v", err)
	}
	_ = st.Close()
}

// TestSigned_ClaimWithoutSeed asserts that a caller that holds no seed may send
// an unsigned claim, which a receiver classifies as claimed and which never
// satisfies a signed endpoint.
//
// catalog: SAP-13.5-13
func TestSigned_ClaimWithoutSeed(t *testing.T) {
	f := signedFixture(t, nil, caller.WithIdentity(caller.IdentityOptions{SendClaim: true}))
	st, err := f.a.PromptText(ctxT(t), "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collect(t, st); err != nil {
		t.Fatal(err)
	}
	m := f.h.nextPrompt(t)
	s := verifyLive(t, m)
	if identity.TrustOf(s) != identity.TrustClaimed || s.Header.Sig != "" {
		t.Fatalf("%+v", s)
	}
	g := signedFixture(t, mod(func(h *host) { h.minTrust = "signed" }), caller.WithIdentity(caller.IdentityOptions{SendClaim: true}))
	if _, err := g.a.PromptText(ctxT(t), "x"); !errors.Is(err, identity.ErrCannotSign) {
		t.Fatalf("a claim satisfied a signed endpoint: %v", err)
	}
}

// TestSigned_HelpersSignTheSubject asserts the signing helpers: the signed
// subject is the one published to by default (SAP §13.6.2), behind a renaming
// import it is the exporter's subject, and a publish carries the nonce as its
// message id.
//
// catalog: SAP-13.6.2-01 SAP-13.6.2-03
// catalog: SAP-13.6.2-02
func TestSigned_HelpersSignTheSubject(t *testing.T) {
	f := signedFixture(t, nil, withSigner(t, ""))
	sn := newSniffer(t, f.cnc)
	ctx := ctxT(t)
	if err := f.c.PublishSigned(ctx, "work.items", []byte("payload-1")); err != nil {
		t.Fatal(err)
	}
	if err := f.c.PublishSigned(ctx, "local.name", []byte("payload-2"), caller.WithSub("exporter.subject"), caller.WithNonce("fixed-nonce-1")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "both publishes", func() bool { return len(sn.on("work.items")) == 1 && len(sn.on("local.name")) == 1 })

	m1 := sn.on("work.items")[0]
	s1 := verifyLive(t, m1)
	if identity.TrustOf(s1) != identity.TrustVerified || s1.Header.Sub != "work.items" {
		t.Fatalf("%+v", s1)
	}
	if m1.Header.Get("Nats-Msg-Id") != s1.Header.Nonce {
		t.Fatalf("Nats-Msg-Id %q, nonce %q", m1.Header.Get("Nats-Msg-Id"), s1.Header.Nonce)
	}

	m2 := sn.on("local.name")[0]
	opts := liveOpts()
	opts.RenamedSubject = func(arrival string) (string, bool) { return "exporter.subject", arrival == "local.name" }
	s2, err := identity.VerifyLive(identity.MessageFromNATS(m2), opts)
	if err != nil || identity.TrustOf(s2) != identity.TrustVerified || s2.Header.Sub != "exporter.subject" || s2.Header.Nonce != "fixed-nonce-1" {
		t.Fatalf("%+v %v", s2, err)
	}
	// Without the rename rule the renamed signature does not verify.
	if _, err := identity.VerifyLive(identity.MessageFromNATS(m2), liveOpts()); err == nil {
		t.Fatal("a renamed subject verified without a rename rule")
	}

	// SignSender and RequestSigned.
	hv, err := f.c.SignSender(ctx, "work.items", []byte("p"))
	if err != nil || !strings.HasPrefix(hv, `{"v":1,`) {
		t.Fatal(hv, err)
	}
	responder, err := f.h.nc.Subscribe("rpc.echo", func(m *nats.Msg) {
		if identity.TrustOf(func() *identity.Sender {
			s, _ := identity.VerifyLive(identity.MessageFromNATS(m), liveOpts())
			return s
		}()) == identity.TrustVerified {
			_ = m.Respond([]byte("verified"))
			return
		}
		_ = m.Respond([]byte("unverified"))
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = responder.Unsubscribe() }()
	_ = f.h.nc.Flush()
	reply, err := f.c.RequestSigned(ctx, "rpc.echo", []byte("hi"), 2*time.Second, caller.WithHeaders(nats.Header{"X-App": {"1"}}))
	if err != nil || string(reply.Data) != "verified" {
		t.Fatalf("%v %v", reply, err)
	}
	if _, err := f.c.RequestSigned(ctx, "rpc.echo", []byte("hi"), 2*time.Second, caller.WithHeaders(nats.Header{"agent-sender": {"forged"}})); !errors.Is(err, caller.ErrReservedHeader) {
		t.Fatalf("a forged Agent-Sender through WithHeaders: %v", err)
	}

	// Without a signer the helpers refuse locally.
	g := signedFixture(t, nil)
	if err := g.c.PublishSigned(ctx, "work.items", nil); !errors.Is(err, caller.ErrNoSigner) || !errors.Is(err, agents.ErrLocal) {
		t.Fatalf("%v", err)
	}
}

// TestSigned_ResolveSender asserts the reverse lookup: an agent ID maps back to
// the instance whose id_sig verifies against its own prompt subject, an
// instance with only a claim in its metadata is not returned, and the
// enumeration is cached briefly instead of repeated per lookup.
//
// catalog: SAP-13.10-04
// catalog: SAP-13.10-05
// catalog: SAP-13.10-06
func TestSigned_ResolveSender(t *testing.T) {
	srv := startNkeyServer(t)
	hnc, cnc := connectAlice(t, srv), connectAlice(t, srv)
	signer := aliceSigner(t)
	id := aliceID(t)

	liveHost := newHost(t, hnc, func(h *host) {
		h.agent, h.owner, h.name = "real", "tester", "one"
	})
	reg, err := identity.RegistrationMetadata(signer, id, liveHost.agent, liveHost.owner, "agents.prompt.real.tester.one")
	if err != nil {
		t.Fatal(err)
	}
	liveHost.extraMeta = reg
	liveHost.start()

	// A second instance publishes the same user_nkey and account but a bad
	// id_sig: only a claim.
	liar := newHost(t, hnc, func(h *host) {
		h.agent, h.owner, h.name = "liar", "tester", "two"
		h.extraMeta = map[string]string{"user_nkey": alice.pub, "account": "$G", "id_sig": strings.Repeat("A", 86)}
	}).start()

	c := newClient(t, cnc, caller.WithResolveTTL(time.Minute))
	ctx := ctxT(t)
	got, ok, err := c.ResolveSender(ctx, id)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if got.ID != liveHost.id || got.PromptSubject != liveHost.promptSubject {
		t.Fatalf("resolved %+v, want the instance with the verifying id_sig (%s), not %s", got, liveHost.id, liar.id)
	}
	other, err := identity.NewAgentID("$G", "UDLPRVYPNSPIPYH4RVGKEHP2AAG4J2CN4GQ3IBTKC6ZDV6SR7ONG27D4")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.ResolveSender(ctx, other); err != nil || ok {
		t.Fatalf("an unregistered sender resolved: %v %v", ok, err)
	}
	if n := liveHost.infoReqs.Load(); n != 1 {
		t.Fatalf("%d enumerations for two lookups, want 1 (cached)", n)
	}
	c.InvalidateSenders()
	if _, ok, err := c.ResolveSender(ctx, id); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if n := liveHost.infoReqs.Load(); n != 2 {
		t.Fatalf("%d enumerations after Invalidate, want 2", n)
	}
}

// TestTracker_VerifiesSignedHeartbeats asserts that a tracker told to verify
// discards a heartbeat that fails verification and never downgrades it to a
// claim, and that requiring signatures also discards heartbeats that carry none.
//
// catalog: SAP-13.11-07
func TestTracker_VerifiesSignedHeartbeats(t *testing.T) {
	srv := startNkeyServer(t)
	hnc, cnc := connectAlice(t, srv), connectAlice(t, srv)
	signer := aliceSigner(t)
	good, forged, bare := newHost(t, hnc), newHost(t, hnc), newHost(t, hnc)

	publish := func(h *host, mutate func(payload []byte) []byte) {
		hb := h.heartbeat()
		payload, err := agents.EncodeHeartbeat(hb)
		if err != nil {
			t.Fatal(err)
		}
		subject := "agents.hb." + h.agent + "." + h.owner + "." + h.name
		msg := &nats.Msg{Subject: subject, Data: payload}
		if mutate != nil {
			sh, err := identity.SignHeader(identity.SignOptions{Signer: signer, ID: aliceID(t), Subject: subject, Payload: payload})
			if err != nil {
				t.Fatal(err)
			}
			msg.Header = nats.Header{}
			sh.Set(msg.Header)
			msg.Data = mutate(payload)
		}
		if err := hnc.PublishMsg(msg); err != nil {
			t.Fatal(err)
		}
	}
	run := func(require bool) *caller.HeartbeatTracker {
		tr, err := caller.NewHeartbeatTracker(cnc, caller.WithHeartbeatVerification(liveOpts(), require))
		if err != nil {
			t.Fatal(err)
		}
		if err := tr.Start(ctxT(t)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tr.Stop(ctxT(t)) })
		publish(good, func(p []byte) []byte { return p })
		publish(forged, func(p []byte) []byte { return append(append([]byte(nil), p...), ' ') })
		publish(bare, nil)
		// Heartbeats are handled in order: once the last one published is
		// accounted for, every earlier one has been judged.
		wantDropped := uint64(1)
		if require {
			wantDropped = 2 // the forged one and the unsigned one
		}
		eventually(t, "heartbeats to be handled", func() bool {
			_, ok := tr.Liveness(good.id)
			return ok && tr.Dropped() >= wantDropped
		})
		if !require {
			eventually(t, "the unsigned heartbeat to be handled", func() bool { _, ok := tr.Liveness(bare.id); return ok })
		}
		return tr
	}
	tr := run(false)
	if _, ok := tr.Liveness(good.id); !ok {
		t.Fatal("a verified heartbeat was discarded")
	}
	if _, ok := tr.Liveness(forged.id); ok {
		t.Fatal("a heartbeat that failed verification was kept")
	}
	if _, ok := tr.Liveness(bare.id); !ok {
		t.Fatal("an unsigned heartbeat is acceptable when signing is optional")
	}
	strict := run(true)
	if _, ok := strict.Liveness(good.id); !ok {
		t.Fatal("a verified heartbeat was discarded")
	}
	if _, ok := strict.Liveness(bare.id); ok {
		t.Fatal("an unsigned heartbeat was kept although signatures are required")
	}
	if _, ok := strict.Liveness(forged.id); ok {
		t.Fatal("a failed heartbeat was downgraded to a claim")
	}
}

// TestSigned_RenamedImportSignsTheExportersSubject asserts the rename case of
// SAP §13.6.2 on a prompt: the caller publishes to its own local name and signs
// the exporter's subject, which a receiver that knows the mapping verifies.
//
// catalog: SAP-13.6.2-02
func TestSigned_RenamedImportSignsTheExportersSubject(t *testing.T) {
	f := signedFixture(t, nil, withSigner(t, ""))
	// A stand-in for a service import that renames local.alias to the export.
	if _, err := f.h.nc.Subscribe("local.alias", func(m *nats.Msg) {
		_ = f.h.nc.PublishMsg(&nats.Msg{Subject: f.h.promptSubject, Reply: m.Reply, Data: m.Data, Header: m.Header})
	}); err != nil {
		t.Fatal(err)
	}
	_ = f.h.nc.Flush()
	st, err := f.a.PromptText(ctxT(t), "via the alias",
		caller.WithPromptSubject("local.alias"), caller.WithSignedSubject(f.h.promptSubject))
	if err != nil {
		t.Fatal(err)
	}
	if st.Subject() != "local.alias" {
		t.Fatalf("published to %q", st.Subject())
	}
	evs, err := collect(t, st)
	if err != nil || texts(evs) != "echo: via the alias" {
		t.Fatalf("%q %v", texts(evs), err)
	}
	m := f.h.nextPrompt(t)
	s, err := identity.VerifyLive(identity.MessageFromNATS(m), liveOpts())
	if err != nil || identity.TrustOf(s) != identity.TrustVerified || s.Header.Sub != f.h.promptSubject {
		t.Fatalf("%+v %v", s, err)
	}
}

// TestSigned_SelfID asserts the connection's own agent ID, looked up from the
// server, and that a refresh asks again.
func TestSigned_SelfID(t *testing.T) {
	f := signedFixture(t, nil, withSigner(t, ""))
	id, err := f.c.SelfID(ctxT(t))
	if err != nil || id != aliceID(t) {
		t.Fatalf("%v %v", id, err)
	}
	id, err = f.c.RefreshSelfID(ctxT(t))
	if err != nil || id != aliceID(t) {
		t.Fatalf("%v %v", id, err)
	}
	// A server with no accounts does not answer: the identity is unknown, not guessed.
	srv := startServer(t)
	c := newClient(t, connect(t, srv), caller.WithIdentity(caller.IdentityOptions{LookupTimeout: 150 * time.Millisecond}))
	if _, err := c.SelfID(ctxT(t)); !errors.Is(err, identity.ErrIdentityUnknown) && !errors.Is(err, identity.ErrNoIdentity) {
		t.Fatalf("error %v", err)
	}
}
