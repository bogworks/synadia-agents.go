// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package connect_test connects to an embedded nats-server through NATS CLI
// contexts: token, user and password, and NKEY authentication, a signed
// bundle, and the reconnect defaults (parity rows 1.4, 1.6, 1.8, 1.11, 10.10).
package connect_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bogworks/synadia-agents.go/connect"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

func start(t *testing.T, mod func(*server.Options)) *server.Server {
	t.Helper()
	opts := &server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true}
	mod(opts)
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

// configHome makes a NATS CLI config dir, selected the way the CLI selects it.
func configHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("NATS_CONFIG_HOME", "")
	t.Setenv("NATS_CONTEXT", "")
	return filepath.Join(dir, "nats")
}

func writeContext(t *testing.T, home, name string, fields map[string]any) {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "context"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "context", name+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func dial(t *testing.T, c connect.Connection, extra ...nats.Option) *nats.Conn {
	t.Helper()
	opts := append(append([]nats.Option{}, c.Options...), extra...)
	nc, err := nats.Connect(c.URL, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func TestContext_TokenAuth(t *testing.T) {
	srv := start(t, func(o *server.Options) { o.Authorization = "s3cret" })
	home := configHome(t)
	writeContext(t, home, "good", map[string]any{"url": srv.ClientURL(), "token": "s3cret"})
	writeContext(t, home, "userinfo", map[string]any{"url": "nats://s3cret@" + srv.Addr().String()})
	writeContext(t, home, "bad", map[string]any{"url": srv.ClientURL(), "token": "wrong"})
	writeContext(t, home, "none", map[string]any{"url": srv.ClientURL()})

	for _, name := range []string{"good", "userinfo"} {
		c, err := connect.LoadContext(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		if nc := dial(t, c); !nc.IsConnected() {
			t.Errorf("%s: not connected", name)
		}
	}
	for _, name := range []string{"bad", "none"} {
		c, err := connect.LoadContext(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := nats.Connect(c.URL, append(c.Options, nats.MaxReconnects(0))...); err == nil {
			t.Errorf("%s: connected without a valid token", name)
		}
	}
}

func TestContext_UserPasswordAuth(t *testing.T) {
	srv := start(t, func(o *server.Options) { o.Username, o.Password = "bob", "pw" })
	home := configHome(t)
	writeContext(t, home, "c", map[string]any{"url": srv.ClientURL(), "user": "bob", "password": "pw"})
	c, err := connect.LoadContext(t.Context(), "c")
	if err != nil {
		t.Fatal(err)
	}
	if nc := dial(t, c); !nc.IsConnected() {
		t.Error("not connected")
	}
}

func TestContext_NKeyAuthAndSignedBundle(t *testing.T) {
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := kp.PublicKey()
	seed, _ := kp.Seed()
	srv := start(t, func(o *server.Options) { o.Nkeys = []*server.NkeyUser{{Nkey: pub}} })

	home := configHome(t)
	seedPath := filepath.Join(t.TempDir(), "user.nk")
	if err := os.WriteFile(seedPath, append(seed, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	writeContext(t, home, "nk", map[string]any{"url": srv.ClientURL(), "nkey": seedPath})
	writeContext(t, home, "inline", map[string]any{"url": srv.ClientURL(), "user_seed": string(seed)})
	// Beside a token the token wins the connection, so the
	// signer would name a different identity: SignerFromContext refuses it.
	writeContext(t, home, "inline-token", map[string]any{"url": srv.ClientURL(), "user_seed": string(seed), "token": "ignored"})
	if err := os.WriteFile(filepath.Join(home, "context.txt"), []byte("nk\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("LoadContext", func(t *testing.T) {
		c, err := connect.LoadContext(t.Context(), "nk")
		if err != nil {
			t.Fatal(err)
		}
		if nc := dial(t, c); !nc.IsConnected() {
			t.Error("not connected")
		}
	})
	t.Run("current", func(t *testing.T) {
		c, err := connect.LoadContext(t.Context(), "current")
		if err != nil {
			t.Fatal(err)
		}
		if nc := dial(t, c); !nc.IsConnected() {
			t.Error("not connected")
		}
	})
	t.Run("ResolveBundle signed", func(t *testing.T) {
		b, err := connect.ResolveBundle(context.Background(), connect.Source{Context: "nk"}, connect.WithSignedIdentity())
		if err != nil {
			t.Fatal(err)
		}
		nc := dial(t, b.Connection, connect.ReconnectDefaults()...)
		if !nc.IsConnected() {
			t.Fatal("not connected")
		}
		// The signer is the key the server authenticated.
		sig, err := b.Signer.Sign([]byte("x"))
		if err != nil {
			t.Fatal(err)
		}
		if err := kp.Verify([]byte("x"), sig); err != nil || b.Signer.PublicKey() != pub {
			t.Errorf("signer does not match the connection's key: %v", err)
		}
		nc.Close()
		b.Wipe()
	})
	t.Run("ResolveBundle direct nkey file", func(t *testing.T) {
		b, err := connect.ResolveBundle(context.Background(), connect.Source{URL: srv.ClientURL(), NKey: seedPath}, connect.WithSignedIdentity())
		if err != nil {
			t.Fatal(err)
		}
		if nc := dial(t, b.Connection); !nc.IsConnected() {
			t.Error("not connected")
		}
	})
	t.Run("inline seed: the server authenticates the signer's key", func(t *testing.T) {
		sg, err := connect.SignerFromContext(t.Context(), "inline")
		if err != nil {
			t.Fatal(err)
		}
		c, err := connect.LoadContext(t.Context(), "inline")
		if err != nil {
			t.Fatal(err)
		}
		nc := dial(t, c)
		if !nc.IsConnected() {
			t.Fatal("not connected")
		}
		cz, err := srv.Connz(&server.ConnzOptions{Username: true})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, ci := range cz.Conns {
			if ci.AuthorizedUser == sg.PublicKey() {
				found = true
			}
		}
		if !found || sg.PublicKey() != pub {
			t.Fatalf("server users = %+v, signer = %q, want %q", cz.Conns, sg.PublicKey(), pub)
		}
		b, err := connect.ResolveBundle(context.Background(), connect.Source{Context: "inline"}, connect.WithSignedIdentity())
		if err != nil {
			t.Fatal(err)
		}
		defer b.Wipe()
		if nc2 := dial(t, b.Connection); !nc2.IsConnected() || b.Signer.PublicKey() != pub {
			t.Error("bundle connection or signer does not match the seed's user")
		}
	})
	t.Run("no seed: anonymous context yields no signer", func(t *testing.T) {
		writeContext(t, home, "anon", map[string]any{"url": srv.ClientURL()})
		if s, err := connect.SignerFromContext(t.Context(), "anon"); err == nil || s != nil {
			t.Fatalf("signer = %v, err = %v", s, err)
		}
	})
	t.Run("SignerFromContext inline seed", func(t *testing.T) {
		s, err := connect.SignerFromContext(t.Context(), "inline")
		if err != nil || s.PublicKey() != pub {
			t.Fatalf("signer = %v, err = %v", s, err)
		}
	})
	t.Run("SignerFromContext refuses a seed beside another credential", func(t *testing.T) {
		if s, err := connect.SignerFromContext(t.Context(), "inline-token"); err == nil || s != nil {
			t.Fatalf("signer = %v, err = %v", s, err)
		}
	})
	t.Run("wrong key is refused", func(t *testing.T) {
		other, _ := nkeys.CreateUser()
		os2, _ := other.Seed()
		path2 := filepath.Join(t.TempDir(), "o.nk")
		if err := os.WriteFile(path2, os2, 0o600); err != nil {
			t.Fatal(err)
		}
		writeContext(t, home, "other", map[string]any{"url": srv.ClientURL(), "nkey": path2})
		c, err := connect.LoadContext(t.Context(), "other")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := nats.Connect(c.URL, append(c.Options, nats.MaxReconnects(0))...); err == nil {
			t.Error("connected with an unknown NKEY")
		}
	})
}

func TestReconnectDefaults_ConnectsAndSurvivesRestart(t *testing.T) {
	srv := start(t, func(_ *server.Options) {})
	addr := srv.Addr().(*net.TCPAddr)
	reconnected := make(chan struct{}, 1)
	opts := connect.WithAgentReconnectDefaults(
		nats.ReconnectWait(50*time.Millisecond),
		nats.ReconnectJitter(0, 0),
		nats.ReconnectHandler(func(*nats.Conn) { reconnected <- struct{}{} }),
	)
	nc, err := nats.Connect(srv.ClientURL(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	srv.Shutdown()
	srv.WaitForShutdown()
	start(t, func(o *server.Options) { o.Host, o.Port = addr.IP.String(), addr.Port })
	select {
	case <-reconnected:
	case <-time.After(10 * time.Second):
		t.Fatal("did not reconnect")
	}
}
