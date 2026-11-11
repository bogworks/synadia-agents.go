// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package sapconform_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/agentstest"
	"github.com/bogworks/synadia-agents.go/conformance/runner"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/bogworks/synadia-agents.go/identity"
)

// AT-006 evidence: the conformance runner run against this SDK's own host, in
// four configurations. Each asserts zero MUST and MUST NOT fails, logs the
// result counts and writes the JSON report to the test's temp directory, and to
// $SAP_CONFORM_EVIDENCE/<name>.json when that variable names a directory.

// goHostKnownFindings lists entries the Go host is known to fail, with the
// reason; none today. A listed failure stays visible in the log.
var goHostKnownFindings = map[string]string{}

const goHostQueryPrompt = "ask me"
const goHostSlowPrompt = "work slowly"

// goHostHandler echoes, asks a query for goHostQueryPrompt and works slowly
// for goHostSlowPrompt.
func goHostHandler(ctx context.Context, req *host.Request, w *host.Responder) error {
	switch req.Envelope.Prompt {
	case goHostQueryPrompt:
		ans, err := w.Ask(ctx, "proceed?", 1500*time.Millisecond)
		if err != nil {
			return host.Reject(409, "query unanswered")
		}
		return w.Send(ctx, ans.Prompt)
	case goHostSlowPrompt:
		select {
		case <-time.After(800 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return w.Send(ctx, req.Envelope.Prompt)
}

func goHostEvidence(t *testing.T, name string, rep *runner.Report, res results) {
	t.Helper()
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(t.TempDir(), name+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if dir := os.Getenv("SAP_CONFORM_EVIDENCE"); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	summarize(t, "go host "+name, res)
	for _, id := range res.ids(runner.Fail) {
		e := res[id]
		if why, ok := goHostKnownFindings[id]; ok {
			t.Logf("known finding: %s: %s", id, why)
			continue
		}
		if e.Level == "MUST" || e.Level == "MUST NOT" {
			t.Errorf("%s (%s, line %d): %s: %s", id, e.Level, e.SpecLine, e.Summary, e.Detail)
		}
	}
	for id, e := range res {
		if (e.Role == "host" || e.Role == "both") && e.Capability == "black-box" && e.Result == runner.NotRun {
			t.Errorf("%s was not run: %s", id, e.Detail)
		}
	}
}

func startGoReference(t *testing.T, nc *nats.Conn, opts ...host.Option) {
	t.Helper()
	subj, err := agents.NewAgentSubject("gohost", "tests", "main")
	if err != nil {
		t.Fatal(err)
	}
	ra, err := agentstest.NewReferenceAgent(nc, subj, agentstest.WithHandler(goHostHandler), agentstest.WithHost(opts...))
	if err != nil {
		t.Fatal(err)
	}
	if err := ra.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ra.Stop(context.Background()) })
}

func goHostConfig(nc *nats.Conn) runner.Config {
	cfg := officialConfig(nc)
	cfg.HeartbeatWindow = 3 * time.Second
	cfg.StreamTimeout = 5 * time.Second
	cfg.Timing = fastTiming()
	f := false
	cfg.SessionAware = &f
	return cfg
}

// TestGoHost_Echo: (1) the plain echo host, no section 13.
func TestGoHost_Echo(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	startGoReference(t, connect(t, s))
	cfg := goHostConfig(connect(t, s))
	cfg.ExpectMinSenderTrust = "any"
	rep, res := runOnce(t, cfg)
	goHostEvidence(t, "echo", rep, res)
}

// TestGoHost_Query: (2) a host that sends a query chunk.
func TestGoHost_Query(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	startGoReference(t, connect(t, s))
	cfg := goHostConfig(connect(t, s))
	cfg.ExpectMinSenderTrust = "any"
	cfg.QueryPrompt = goHostQueryPrompt
	cfg.AnswerQuerySubjectPrefixes = []string{"_INBOX."}
	cfg.QueryUnansweredWait = 5 * time.Second
	rep, res := runOnce(t, cfg)
	goHostEvidence(t, "query", rep, res)
	// The host ends an unanswered query with 409, one of the two permitted
	// outcomes, so the entry for the other (default-and-continue) is
	// not-applicable.
	for _, id := range queryPass {
		if id != idUnansweredDefault {
			res.want(t, runner.Pass, id)
		}
	}
	if r := res[idUnansweredDefault].Result; r != runner.Pass && r != runner.NotApplicable {
		t.Errorf("%s: %s", idUnansweredDefault, r)
	}
}

// TestGoHost_Signed: (3) min_sender_trust signed, a host identity seed, and the
// runner given sender and refused-sender seeds. The server authenticates by
// NKEY, so the host and the runner each connect as their own user.
func TestGoHost_Signed(t *testing.T) {
	t.Parallel()
	mk := func() (nkeys.KeyPair, string, identity.Signer) {
		kp, err := nkeys.CreateUser()
		if err != nil {
			t.Fatal(err)
		}
		pub, _ := kp.PublicKey()
		seed, _ := kp.Seed()
		sg, err := identity.SignerFromSeed(seed)
		if err != nil {
			t.Fatal(err)
		}
		return kp, pub, sg
	}
	hostKP, hostPub, hostSigner := mk()
	senderKP, senderPub, senderSigner := mk()
	_, refusedPub, refusedSigner := mk()

	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
		Nkeys: []*server.NkeyUser{{Nkey: hostPub}, {Nkey: senderPub}}})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats-server not ready")
	}
	t.Cleanup(s.Shutdown)
	conn := func(pub string, kp nkeys.KeyPair) *nats.Conn {
		nc, err := nats.Connect(s.ClientURL(), nats.Nkey(pub, func(n []byte) ([]byte, error) { return kp.Sign(n) }))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(nc.Close)
		return nc
	}
	startGoReference(t, conn(hostPub, hostKP),
		host.WithMinSenderTrust(identity.TrustSigned),
		host.WithIdentity(host.IdentityOptions{Signer: hostSigner}),
		host.WithAdmission(func(_ context.Context, sd *identity.Sender, _ *host.RequestInfo) (bool, error) {
			return sd == nil || sd.ID.User() != refusedPub, nil
		}))

	cfg := goHostConfig(conn(senderPub, senderKP))
	cfg.ExpectMinSenderTrust = "signed"
	cfg.SenderSigner = senderSigner
	cfg.SenderAccount = "$G"
	cfg.RefusedSigner = refusedSigner
	rep, res := runOnce(t, cfg)
	goHostEvidence(t, "signed", rep, res)
	res.want(t, runner.Pass, signedPass...)
}

// TestGoHost_KeepAlive: (4) keep-alives on and a short heartbeat interval.
func TestGoHost_KeepAlive(t *testing.T) {
	t.Parallel()
	s := startServer(t)
	startGoReference(t, connect(t, s),
		host.WithSubSecondHeartbeats(), host.WithHeartbeatInterval(300*time.Millisecond),
		host.WithKeepalive(200*time.Millisecond))
	cfg := goHostConfig(connect(t, s))
	cfg.ExpectMinSenderTrust = "any"
	cfg.HeartbeatWindow = 2 * time.Second
	cfg.SlowPrompt = goHostSlowPrompt
	rep, res := runOnce(t, cfg)
	goHostEvidence(t, "keepalive", rep, res)
}
