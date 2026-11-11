// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package identity_test runs the sender identity (SAP §13) against embedded
// nats-server instances, built from the fixtures of the identity package's
// testdata: a request-info lookup, an account token position export, a closed
// import with a server stamp, a denied $SYS subject and operator mode.
package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

const testdata = "../../identity/testdata"

type user struct{ pub, seed string }

var (
	alice = user{"UCDUW5V44EBDBIK2FL4CTNDBQFNGBEZVJHSZGQVKRHASN4AV4IWPB5NT", "SUAEJ6GDK6FSSB54LD45Q7W25AW7NUT7MVLBABIR5MIPFUTBW7ZNPK2KYE"}
	bob   = user{"UDLPRVYPNSPIPYH4RVGKEHP2AAG4J2CN4GQ3IBTKC6ZDV6SR7ONG27D4", "SUAA37BA6WVMWPX27LE3AREHH5ELK2K5OYDECALTSZBJ7CDMW4AOC3GNAI"}
	carol = user{"UBTHWKVPPUINKFEZ6UZ6ZAWNT4RSRVVLELSMHWPMIRWRBFDCER5TSKLT", "SUACJSIK64RJ6WLD3JSDQUXM7DERPDXON6UKYY3MAFHX24M2L5TRAKTWAM"}
)

func startServer(t *testing.T, conf string) *server.Server {
	t.Helper()
	opts, err := server.ProcessConfigFile(filepath.Join(testdata, conf))
	if err != nil {
		t.Fatal(err)
	}
	opts.Host, opts.Port = "127.0.0.1", -1
	opts.NoLog, opts.NoSigs = true, true
	srv, err := server.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats-server did not become ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

func connectNkey(t *testing.T, srv *server.Server, u user, extra ...nats.Option) *nats.Conn {
	t.Helper()
	kp, err := nkeys.FromSeed([]byte(u.seed))
	if err != nil {
		t.Fatal(err)
	}
	opts := append([]nats.Option{nats.Nkey(u.pub, kp.Sign), nats.MaxReconnects(0)}, extra...)
	nc, err := nats.Connect(srv.ClientURL(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func connectCreds(t *testing.T, srv *server.Server, name string) (*nats.Conn, string) {
	t.Helper()
	path, err := filepath.Abs(filepath.Join(testdata, "operator", name))
	if err != nil {
		t.Fatal(err)
	}
	nc, err := nats.Connect(srv.ClientURL(), nats.UserCredentials(path), nats.MaxReconnects(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc, path
}

func signer(t *testing.T, u user) identity.Signer {
	t.Helper()
	s, err := identity.SignerFromSeed([]byte(u.seed))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func selfID(t *testing.T, nc *nats.Conn, opts identity.SelfIDOptions) identity.AgentID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := identity.NewSelfIDResolver(nc, opts).ID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestRequestInfo_ConfigFileServers learns a connection's own identity from
// $SYS.REQ.USER.INFO on servers without operator mode (SAP §13.4, §13.3.1).
func TestRequestInfo_ConfigFileServers(t *testing.T) {
	cases := []struct {
		name string
		conf string
		user user
		want identity.AgentID
	}{
		{"no accounts is $G", "nkey-noaccounts.conf", alice, identity.AgentID("$G." + alice.pub)},
		{"ACME user", "accounts.conf", alice, identity.AgentID("ACME." + alice.pub)},
		{"APP user", "accounts.conf", bob, identity.AgentID("APP." + bob.pub)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := startServer(t, tc.conf)
			nc := connectNkey(t, srv, tc.user)
			got := selfID(t, nc, identity.SelfIDOptions{Signer: signer(t, tc.user)})
			if got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if _, err := identity.ParseAgentID(string(got)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestRequestInfo_OperatorMode checks both identity sources of SAP §13.4 on an
// operator-mode server: the credentials JWT, and $SYS.REQ.USER.INFO, which must
// find the same 113-character pair.
func TestRequestInfo_OperatorMode(t *testing.T) {
	srv := startServer(t, "operator/operator.conf")
	nc, path := connectCreds(t, srv, "alice.creds")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := identity.SignerFromCreds(raw)
	if err != nil {
		t.Fatal(err)
	}
	fromJWT := selfID(t, nc, identity.SelfIDOptions{Signer: s})
	both := selfID(t, nc, identity.SelfIDOptions{Signer: s, ConsultServer: true})
	fromServer := selfID(t, nc, identity.SelfIDOptions{})
	if fromJWT != both || both != fromServer || len(fromJWT) != 113 {
		t.Fatalf("jwt %s, both %s, server %s", fromJWT, both, fromServer)
	}
	if !strings.HasPrefix(fromJWT.Account(), "A") {
		t.Fatalf("account %s is not an account NKEY", fromJWT.Account())
	}
}

// TestRequestInfo_DeniedSysIsUnknownAndFailsFast: a permission that blocks
// $SYS.REQ.USER.INFO yields no reply but an asynchronous permissions error. The
// identity is unknown at once, not after the timeout, and no Agent-Sender is
// sent (SAP §13.4).
func TestRequestInfo_DeniedSysIsUnknownAndFailsFast(t *testing.T) {
	srv := startServer(t, "nkey-deny-sys.conf")
	var (
		mu       sync.Mutex
		resolver *identity.SelfIDResolver
	)
	nc := connectNkey(t, srv, alice, nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
		mu.Lock()
		r := resolver
		mu.Unlock()
		if r != nil {
			r.ObserveError(err)
		}
	}))
	mu.Lock()
	resolver = identity.NewSelfIDResolver(nc, identity.SelfIDOptions{
		Signer: signer(t, alice), Timeout: 20 * time.Second, RetryInterval: time.Hour})
	r := resolver
	mu.Unlock()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	id, err := r.ID(ctx)
	if !errors.Is(err, identity.ErrIdentityUnknown) || id != "" {
		t.Fatalf("got %q, %v; want ErrIdentityUnknown", id, err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("took %s: the permissions error did not fail the lookup fast", elapsed)
	}
	if !strings.Contains(err.Error(), "permissions violation") {
		t.Errorf("reason: %v", err)
	}
	// Unknown is not none: this is not ErrNoIdentity.
	if errors.Is(err, identity.ErrNoIdentity) {
		t.Error("an unknown identity was reported as none")
	}
	// With the identity unknown the caller cannot sign, so it fails locally
	// against a signed endpoint and sends no header (SAP §13.9).
	if err := identity.CheckCanSend(identity.TrustSigned, false); !errors.Is(err, identity.ErrCannotSign) {
		t.Fatal(err)
	}
}

// host subscribes to subject, verifies each live request through a gate (with
// the account token position configured) and answers with the outcome.
func host(t *testing.T, nc *nats.Conn, subject string, gate *identity.Gate) {
	t.Helper()
	_, err := nc.Subscribe(subject, func(m *nats.Msg) {
		a := gate.AdmitPrompt(context.Background(), identity.MessageFromNATS(m))
		out := map[string]any{"ok": a.OK(), "trust": string(identity.TrustOf(a.Sender)), "arrival": m.Subject}
		if a.Err != nil {
			out["code"] = a.Err.Code
		}
		if a.Sender != nil {
			out["id"] = string(a.Sender.ID)
			out["attested"] = a.Sender.AccountAttested
		}
		b, _ := json.Marshal(out)
		_ = m.Respond(b)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
}

type outcome struct {
	OK       bool   `json:"ok"`
	Trust    string `json:"trust"`
	Arrival  string `json:"arrival"`
	Code     int    `json:"code"`
	ID       string `json:"id"`
	Attested bool   `json:"attested"`
}

// signedRequest signs payload for signSubject, publishes to publishSubject and
// returns the host's verdict.
func signedRequest(t *testing.T, nc *nats.Conn, id identity.AgentID, s identity.Signer, signSubject, publishSubject string, payload []byte) outcome {
	t.Helper()
	h, err := identity.SignHeader(identity.SignOptions{Signer: s, ID: id, Subject: signSubject, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	msg := nats.NewMsg(publishSubject)
	msg.Data = payload
	h.Set(msg.Header)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := nc.RequestMsgWithContext(ctx, msg)
	if err != nil {
		t.Fatalf("request to %s: %v", publishSubject, err)
	}
	var o outcome
	if err := json.Unmarshal(resp.Data, &o); err != nil {
		t.Fatal(err)
	}
	return o
}

// TestAccountToken_InsertedByTheExport runs SAP §13.7.1 (b) against a real
// export with account_token_position: 2. The server inserts the caller's account
// at the position; the receiver removes it before comparing, and the token must
// equal the header's account.
//
// catalog: SAP-13.6.2-03 SAP-13.7.1-03 SAP-13.7.1-06
func TestAccountToken_InsertedByTheExport(t *testing.T) {
	srv := startServer(t, "account-token-position.conf")
	hostConn := connectNkey(t, srv, alice) // ACME
	gate, err := identity.NewGate(identity.GateOptions{Subject: identity.SubjectRules{AccountTokenPosition: 2}})
	if err != nil {
		t.Fatal(err)
	}
	host(t, hostConn, "svc.*.prompt", gate)

	bobConn := connectNkey(t, srv, bob) // APP
	bobID, err := identity.NewAgentID("APP", bob.pub)
	if err != nil {
		t.Fatal(err)
	}
	s := signer(t, bob)

	t.Run("sign the local name the caller publishes", func(t *testing.T) {
		o := signedRequest(t, bobConn, bobID, s, "svc.prompt", "svc.prompt", []byte("a"))
		if !o.OK || o.Trust != "verified" || o.Arrival != "svc.APP.prompt" || o.ID != string(bobID) {
			t.Fatalf("got %+v", o)
		}
	})
	t.Run("sign the token-bearing subject the import names", func(t *testing.T) {
		o := signedRequest(t, bobConn, bobID, s, "svc.APP.prompt", "svc.prompt", []byte("b"))
		if !o.OK || o.Trust != "verified" {
			t.Fatalf("got %+v", o)
		}
	})
	t.Run("publish the full subject through the import without a rename", func(t *testing.T) {
		o := signedRequest(t, bobConn, bobID, s, "svc.APP.prompt", "svc.APP.prompt", []byte("c"))
		if !o.OK || o.Trust != "verified" {
			t.Fatalf("got %+v", o)
		}
	})
	t.Run("a header naming another account fails the token check", func(t *testing.T) {
		evil, err := identity.NewAgentID("EVIL", bob.pub)
		if err != nil {
			t.Fatal(err)
		}
		o := signedRequest(t, bobConn, evil, s, "svc.prompt", "svc.prompt", []byte("d"))
		if o.OK || o.Code != 401 {
			t.Fatalf("a foreign account was admitted: %+v", o)
		}
	})
	t.Run("a header signed for another subject fails", func(t *testing.T) {
		o := signedRequest(t, bobConn, bobID, s, "svc.other", "svc.prompt", []byte("e"))
		if o.OK || o.Code != 401 {
			t.Fatalf("got %+v", o)
		}
	})
	t.Run("a replay of the same signed request is refused", func(t *testing.T) {
		h, err := identity.SignHeader(identity.SignOptions{Signer: s, ID: bobID, Subject: "svc.prompt", Payload: []byte("f")})
		if err != nil {
			t.Fatal(err)
		}
		send := func() outcome {
			msg := nats.NewMsg("svc.prompt")
			msg.Data = []byte("f")
			h.Set(msg.Header)
			resp, err := bobConn.RequestMsg(msg, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			var o outcome
			_ = json.Unmarshal(resp.Data, &o)
			return o
		}
		if o := send(); !o.OK {
			t.Fatalf("first: %+v", o)
		}
		if o := send(); o.OK || o.Code != 401 {
			t.Fatalf("replay: %+v", o)
		}
	})
}

// TestClosedImport_ServerStampAttestsTheAccount: ACME denies every local user
// direct publishes to the prompt subjects, so each request crossed a share:true
// import and Nats-Request-Info is the server's stamp. With OperatorAttested the
// signed account is cross-checked against it (SAP §13.7.2).
func TestClosedImport_ServerStampAttestsTheAccount(t *testing.T) {
	srv := startServer(t, "closed-import.conf")
	hostConn := connectNkey(t, srv, carol) // ACME, cannot publish to agents.prompt.>
	gate, err := identity.NewGate(identity.GateOptions{OperatorAttested: true})
	if err != nil {
		t.Fatal(err)
	}
	host(t, hostConn, "agents.prompt.demo.alice.example", gate)

	bobConn := connectNkey(t, srv, bob) // APP
	bobID, _ := identity.NewAgentID("APP", bob.pub)
	const subject = "agents.prompt.demo.alice.example"

	o := signedRequest(t, bobConn, bobID, signer(t, bob), subject, subject, []byte("hi"))
	if !o.OK || o.Trust != "verified" || !o.Attested || o.Arrival != subject {
		t.Fatalf("got %+v", o)
	}

	// Bob claims to be in another account: the server stamp says APP.
	forged, _ := identity.NewAgentID("ACME", bob.pub)
	o = signedRequest(t, bobConn, forged, signer(t, bob), subject, subject, []byte("hi2"))
	if o.OK || o.Code != 401 {
		t.Fatalf("a forged account passed the stamp cross-check: %+v", o)
	}

	// The stamp itself parses with the package's reader.
	got := make(chan *identity.RequestInfo, 1)
	if _, err := hostConn.Subscribe("agents.status.>", func(m *nats.Msg) {
		info, err := identity.ReadRequestInfo(m.Header)
		if err != nil {
			t.Errorf("ReadRequestInfo: %v", err)
		}
		got <- info
		_ = m.Respond(nil)
	}); err != nil {
		t.Fatal(err)
	}
	_ = hostConn.Flush()
	if _, err := bobConn.Request("agents.status.demo.alice.example", nil, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case info := <-got:
		if info == nil || info.Account != "APP" || info.User != bob.pub {
			t.Fatalf("stamp: %+v", info)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no status request seen")
	}
}

// TestOperatorMode_StampAndSignedIdentity runs a signed request across an
// import with share: true on an operator-mode server. The 113-character agent
// ID is verified, and the account NKEY agrees with the server stamp.
func TestOperatorMode_StampAndSignedIdentity(t *testing.T) {
	srv := startServer(t, "operator/operator.conf")
	hostConn, _ := connectCreds(t, srv, "alice.creds") // ACME
	gate, err := identity.NewGate(identity.GateOptions{OperatorAttested: true})
	if err != nil {
		t.Fatal(err)
	}
	const subject = "agents.prompt.demo.alice.example"
	host(t, hostConn, subject, gate)

	bobConn, path := connectCreds(t, srv, "bob.creds") // APP
	raw, _ := os.ReadFile(path)
	s, err := identity.SignerFromCreds(raw)
	if err != nil {
		t.Fatal(err)
	}
	id, err := identity.IdentityFromJWT(s.JWT())
	if err != nil {
		t.Fatal(err)
	}
	o := signedRequest(t, bobConn, id, s, subject, subject, []byte("hello"))
	if !o.OK || o.Trust != "verified" || !o.Attested || o.ID != string(id) || len(o.ID) != 113 {
		t.Fatalf("got %+v", o)
	}
}
