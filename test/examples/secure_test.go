// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package examples_test

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	"github.com/nats-io/nkeys"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/bogworks/synadia-agents.go/identity"
)

// The secure configuration Examples (caller.ExampleWithTrustPolicy,
// host.ExampleWithAdmission, host.ExampleWithEndpoint) need a NATS server, so
// the root module compiles them without running them. This file runs the same
// code against an embedded nats-server. Each run* function below is the body of
// its Example with the lines that a reader supplies (the connection and the
// allowed ids) replaced and its exits turned into returns, and
// TestSecureExamples_MatchRunners fails if the two drift apart.

// runWithTrustPolicy is the body of caller.ExampleWithTrustPolicy.
func runWithTrustPolicy(nc *nats.Conn, trusted []identity.AgentID) (*caller.Client, error) {
	c, err := caller.New(nc, caller.WithTrustPolicy(caller.TrustPolicy{
		Agents: trusted,
		// Accept runs as well, when set, and must not block.
		Accept: func(_ identity.AgentID, info agents.AgentInfo) bool {
			return info.Owner == "example"
		},
	}))
	if err != nil {
		return nil, err
	}
	return c, nil
}

// runWithAdmission is the body of host.ExampleWithAdmission.
func runWithAdmission(nc *nats.Conn, allowed []identity.AgentID) (*host.Host, error) {
	subject, _ := agents.NewAgentSubject("secure", "example", "default")

	h, err := host.New(nc, subject,
		func(ctx context.Context, req *host.Request, w *host.Responder) error {
			return w.Send(ctx, "hello "+req.Sender.Describe())
		},
		// Unsigned and claimed senders are refused with 401.
		host.WithMinSenderTrust(identity.TrustSigned),
		// Verified senders not on the list are refused with 403.
		host.WithAdmission(func(_ context.Context, s *identity.Sender, _ *host.RequestInfo) (bool, error) {
			return s != nil && s.Trust == identity.TrustVerified && slices.Contains(allowed, s.ID), nil
		}),
	)
	if err != nil {
		return nil, err
	}
	return h, nil
}

// runWithEndpoint is the body of host.ExampleWithEndpoint.
func runWithEndpoint(allowed []identity.AgentID) host.Option {
	nonces := identity.NewNonceSet(identity.NonceSetOptions{})
	opts := identity.LiveOptions{Nonces: nonces}

	handler := micro.HandlerFunc(func(req micro.Request) {
		sender, err := identity.VerifyLive(identity.Message{
			Subject: req.Subject(), Data: req.Data(), Header: nats.Header(req.Headers()),
		}, opts)
		if err != nil || identity.TrustOf(sender) != identity.TrustVerified {
			_ = req.Error("401", "signature required", nil)
			return
		}
		if !slices.Contains(allowed, sender.ID) {
			_ = req.Error("403", "sender refused", nil)
			return
		}
		_ = req.Respond([]byte("ok"))
	})

	return host.WithEndpoint(host.Endpoint{
		Name:    "admin",
		Subject: "agents.admin.secure.example.default",
		Handler: handler,
	})
}

// secureUser is a throwaway test key, one of identity/testdata/keys.json.
type secureUser struct{ pub, seed string }

var (
	secAlice = secureUser{"UCDUW5V44EBDBIK2FL4CTNDBQFNGBEZVJHSZGQVKRHASN4AV4IWPB5NT", "SUAEJ6GDK6FSSB54LD45Q7W25AW7NUT7MVLBABIR5MIPFUTBW7ZNPK2KYE"}
	secBob   = secureUser{"UDLPRVYPNSPIPYH4RVGKEHP2AAG4J2CN4GQ3IBTKC6ZDV6SR7ONG27D4", "SUAA37BA6WVMWPX27LE3AREHH5ELK2K5OYDECALTSZBJ7CDMW4AOC3GNAI"}
	secCarol = secureUser{"UBTHWKVPPUINKFEZ6UZ6ZAWNT4RSRVVLELSMHWPMIRWRBFDCER5TSKLT", "SUACJSIK64RJ6WLD3JSDQUXM7DERPDXON6UKYY3MAFHX24M2L5TRAKTWAM"}
)

func startNkeyServer(t *testing.T) *server.Server {
	t.Helper()
	srv, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
		Nkeys: []*server.NkeyUser{{Nkey: secAlice.pub}, {Nkey: secBob.pub}, {Nkey: secCarol.pub}},
	})
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

func connectAs(t *testing.T, srv *server.Server, u secureUser) *nats.Conn {
	t.Helper()
	kp, err := nkeys.FromSeed([]byte(u.seed))
	if err != nil {
		t.Fatal(err)
	}
	nc, err := nats.Connect(srv.ClientURL(), nats.MaxReconnects(0), nats.Nkey(u.pub, kp.Sign))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func signerOf(t *testing.T, u secureUser) identity.Signer {
	t.Helper()
	s, err := identity.SignerFromSeed([]byte(u.seed))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// idOf is the agent id of a user in the global account.
func idOf(t *testing.T, u secureUser) identity.AgentID {
	t.Helper()
	id, err := identity.NewAgentID("$G", u.pub)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// signedHeader returns the headers of a request signed by u for subject and
// payload.
func signedHeader(t *testing.T, u secureUser, subject string, payload []byte) nats.Header {
	t.Helper()
	h, err := identity.SignHeader(identity.SignOptions{Signer: signerOf(t, u), ID: idOf(t, u), Subject: subject, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	hdr := nats.Header{}
	h.Set(hdr)
	return hdr
}

func startSecureHost(t *testing.T, h *host.Host) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
}

// TestSecureExamples_WithTrustPolicy runs the code of
// caller.ExampleWithTrustPolicy: of four hosts only the one whose id_sig
// verifies, whose signer is on the list and that Accept approves is discovered.
func TestSecureExamples_WithTrustPolicy(t *testing.T) {
	srv := startNkeyServer(t)
	start := func(u secureUser, owner, name string, signed bool) *host.Host {
		t.Helper()
		subject, err := agents.NewAgentSubject("secure", owner, name)
		if err != nil {
			t.Fatal(err)
		}
		var opts []host.Option
		if signed {
			opts = append(opts, host.WithIdentity(host.IdentityOptions{Signer: signerOf(t, u)}))
		}
		h, err := host.New(connectAs(t, srv, u), subject, func(ctx context.Context, _ *host.Request, w *host.Responder) error {
			return w.Send(ctx, "x")
		}, opts...)
		if err != nil {
			t.Fatal(err)
		}
		startSecureHost(t, h)
		return h
	}
	good := start(secAlice, "example", "good", true)
	start(secAlice, "other", "refused-by-accept", true)
	start(secBob, "example", "untrusted-signer", true)
	start(secCarol, "example", "unsigned", false)

	c, err := runWithTrustPolicy(connectAs(t, srv, secAlice), []identity.AgentID{idOf(t, secAlice)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got, err := c.Discover(ctx, caller.WithDiscoverTimeout(500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, a := range got {
		ids = append(ids, a.InstanceID())
	}
	if len(got) != 1 || got[0].InstanceID() != good.InstanceID() {
		t.Fatalf("discovered %v, want only %s", ids, good.InstanceID())
	}
}

// promptErr sends one prompt with hdr and returns the error the host answered,
// or nil when it was served, with the text of the response.
func promptErr(t *testing.T, nc *nats.Conn, subject string, hdr nats.Header) (*agents.ServiceError, string) {
	t.Helper()
	inbox := nats.NewInbox()
	sub, err := nc.SubscribeSync(inbox)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := nc.PublishMsg(&nats.Msg{Subject: subject, Reply: inbox, Data: []byte("x"), Header: hdr}); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var serr *agents.ServiceError
	for {
		m, err := sub.NextMsg(5 * time.Second)
		if err != nil {
			t.Fatalf("no terminator: %v", err)
		}
		if se, ok := agents.ParseServiceError(m.Header, m.Data); ok {
			serr = se
		} else if c, err := agents.DecodeChunk(m.Data); err == nil {
			if r, ok := c.(agents.ResponseChunk); ok {
				text.WriteString(r.Text)
			}
		}
		if agents.IsTerminator(m.Data, m.Header) {
			return serr, text.String()
		}
	}
}

// TestSecureExamples_WithAdmission runs the code of host.ExampleWithAdmission:
// an allowed verified sender is served, a verified sender not on the list is
// refused with 403, and an unsigned one with 401.
func TestSecureExamples_WithAdmission(t *testing.T) {
	srv := startNkeyServer(t)
	h, err := runWithAdmission(connectAs(t, srv, secCarol), []identity.AgentID{idOf(t, secAlice)})
	if err != nil {
		t.Fatal(err)
	}
	startSecureHost(t, h)
	subj := h.Subject().Prompt()

	se, text := promptErr(t, connectAs(t, srv, secAlice), subj, signedHeader(t, secAlice, subj, []byte("x")))
	if se != nil || !strings.HasPrefix(text, "hello ") {
		t.Fatalf("allowed sender: error %+v, text %q", se, text)
	}
	se, _ = promptErr(t, connectAs(t, srv, secBob), subj, signedHeader(t, secBob, subj, []byte("x")))
	if se == nil || se.Code != 403 {
		t.Fatalf("verified sender not on the list: %+v, want 403", se)
	}
	se, _ = promptErr(t, connectAs(t, srv, secBob), subj, nil)
	if se == nil || se.Code != 401 {
		t.Fatalf("unsigned sender: %+v, want 401", se)
	}
}

// TestSecureExamples_WithEndpoint runs the code of host.ExampleWithEndpoint: the
// handler serves a signed request from an allowed sender, refuses an unsigned
// one and a replay with 401, and a verified sender not on the list with 403.
func TestSecureExamples_WithEndpoint(t *testing.T) {
	srv := startNkeyServer(t)
	subject, err := agents.NewAgentSubject("secure", "example", "default")
	if err != nil {
		t.Fatal(err)
	}
	h, err := host.New(connectAs(t, srv, secCarol), subject, func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		return w.Send(ctx, "x")
	}, runWithEndpoint([]identity.AgentID{idOf(t, secAlice)}))
	if err != nil {
		t.Fatal(err)
	}
	startSecureHost(t, h)

	const admin = "agents.admin.secure.example.default"
	nc := connectAs(t, srv, secAlice)
	ask := func(hdr nats.Header) (string, string) {
		t.Helper()
		m, err := nc.RequestMsg(&nats.Msg{Subject: admin, Data: []byte("x"), Header: hdr}, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if code := m.Header.Get(micro.ErrorCodeHeader); code != "" {
			return code, m.Header.Get(micro.ErrorHeader)
		}
		return "", string(m.Data)
	}
	signed := signedHeader(t, secAlice, admin, []byte("x"))
	if code, body := ask(signed); code != "" || body != "ok" {
		t.Fatalf("signed request from an allowed sender: %q %q", code, body)
	}
	if code, _ := ask(signed); code != "401" {
		t.Fatalf("replay: code %q, want 401", code)
	}
	if code, _ := ask(nil); code != "401" {
		t.Fatalf("unsigned: code %q, want 401", code)
	}
	if code, _ := ask(signedHeader(t, secBob, admin, []byte("x"))); code != "403" {
		t.Fatalf("verified sender not on the list: code %q, want 403", code)
	}
}

// bodyLines returns the statements of the body of fn in src, printed by
// go/printer without comments and with blank lines and indentation dropped, so
// only the code is compared.
func bodyLines(t *testing.T, path, fn string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0) // no comments
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn || fd.Recv != nil {
			continue
		}
		var buf bytes.Buffer
		if err := printer.Fprint(&buf, fset, fd.Body); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, l := range strings.Split(buf.String(), "\n") {
			l = strings.TrimSpace(l)
			if l == "" || l == "{" || l == "}" && len(out) == 0 {
				continue
			}
			out = append(out, l)
		}
		// Drop the closing brace of the body.
		if n := len(out); n > 0 && out[n-1] == "}" {
			out = out[:n-1]
		}
		return strings.Join(out, "\n")
	}
	t.Fatalf("func %s not found in %s", fn, path)
	return ""
}

// TestSecureExamples_MatchRunners fails when the code of an Example and the code
// the TestSecureExamples_* tests run differ. The Example's body, after the few
// listed substitutions (what a reader supplies, and the exits), must equal the
// runner's body line for line; comments and formatting are ignored.
func TestSecureExamples_MatchRunners(t *testing.T) {
	cases := []struct {
		example, exampleFn, runnerFn string
		subst                        [][2]string // old, new: each must match exactly once
	}{
		{
			"../../caller/example_secure_test.go", "ExampleWithTrustPolicy", "runWithTrustPolicy",
			[][2]string{
				{"var nc *nats.Conn\n", ""},
				{"trusted := []identity.AgentID{\"ACCOUNT.USER\"}\n", ""},
				{"fmt.Println(err)\nreturn\n", "return nil, err\n"},
				{"defer c.Close()", "return c, nil"},
			},
		},
		{
			"../../host/example_secure_test.go", "ExampleWithAdmission", "runWithAdmission",
			[][2]string{
				{"var nc *nats.Conn\n", ""},
				{"allowed := []identity.AgentID{\"ACCOUNT.USER\"}\n", ""},
				{"fmt.Println(err)\nreturn\n", "return nil, err\n"},
				{"_ = h", "return h, nil"},
			},
		},
		{
			"../../host/example_secure_test.go", "ExampleWithEndpoint", "runWithEndpoint",
			[][2]string{
				{"allowed := []identity.AgentID{\"ACCOUNT.USER\"}\n", ""},
				{"_ = host.WithEndpoint(", "return host.WithEndpoint("},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.exampleFn, func(t *testing.T) {
			want := bodyLines(t, tc.example, tc.exampleFn) + "\n"
			for _, s := range tc.subst {
				if n := strings.Count(want, s[0]); n != 1 {
					t.Fatalf("substitution %q matches %d times in %s, want 1; the Example changed, update the runner and the list", s[0], n, tc.exampleFn)
				}
				want = strings.Replace(want, s[0], s[1], 1)
			}
			got := bodyLines(t, "secure_test.go", tc.runnerFn) + "\n"
			if got != want {
				t.Fatalf("%s no longer matches %s.\n--- Example, substituted:\n%s--- runner:\n%s", tc.runnerFn, tc.exampleFn, want, got)
			}
		})
	}
}
