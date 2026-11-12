// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package connect_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bogworks/synadia-agents.go/connect"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

// ---- helpers ---------------------------------------------------------------

type testUser struct{ seed, pub string }

func newUser(t *testing.T) testUser {
	t.Helper()
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := kp.Seed()
	pub, _ := kp.PublicKey()
	return testUser{string(seed), pub}
}

func newAccountPub(t *testing.T) string {
	t.Helper()
	kp, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := kp.PublicKey()
	return pub
}

// fakeJWT is an unsigned user JWT: identity.IdentityFromJWT reads the claims
// and does not check the signature (SAP §13.4).
func fakeJWT(t *testing.T, user, account string) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"typ": "JWT", "alg": "ed25519-nkey"}) + "." +
		enc(map[string]any{"sub": user, "iss": account, "nats": map[string]string{"type": "user"}}) + ".c2ln"
}

func credsText(jwt, seed string) string {
	return "-----BEGIN NATS USER JWT-----\n" + jwt + "\n------END NATS USER JWT------\n\n" +
		"************************* IMPORTANT *************************\n" +
		"    NKEY Seed printed below can be used to sign and prove identity.\n\n" +
		"-----BEGIN USER NKEY SEED-----\n" + seed + "\n------END USER NKEY SEED------\n\n" +
		"*************************************************************\n"
}

// env is a temporary NATS CLI config home plus a fake HOME.
type env struct {
	t    *testing.T
	home string
	dir  string // the nats config dir
}

func newEnv(t *testing.T) *env {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("NATS_CONFIG_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("NATS_CONTEXT", "")
	dir := filepath.Join(home, ".config", "nats")
	if err := os.MkdirAll(filepath.Join(dir, "context"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, home: home, dir: dir}
}

func (e *env) write(path, content string) string {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return path
}

func (e *env) context(name string, fields map[string]any) {
	e.t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		e.t.Fatal(err)
	}
	e.write(filepath.Join(e.dir, "context", name+".json"), string(b))
}

func applied(t *testing.T, opts []nats.Option) nats.Options {
	t.Helper()
	var o nats.Options
	for _, opt := range opts {
		if err := opt(&o); err != nil {
			t.Fatal(err)
		}
	}
	return o
}

func wantContextErr(t *testing.T, err error) *connect.ContextError {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, connect.ErrContext) {
		t.Fatalf("error %v does not match ErrContext", err)
	}
	var ce *connect.ContextError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a *ContextError", err)
	}
	return ce
}

const serverURL = "nats://127.0.0.1:4222"

// ---- ParseURL (parity 1.5) --------------------------------------------------

func TestParseURL(t *testing.T) {
	tests := []struct {
		name, in, wantURL, user, pass, token string
	}{
		{name: "bare", in: "nats://host:4222", wantURL: "nats://host:4222"},
		{name: "schemeless", in: "host:4222", wantURL: "nats://host:4222"},
		{name: "token", in: "nats://TOK@host:4222", wantURL: "nats://host:4222", token: "TOK"},
		{name: "user and password", in: "nats://alice:pw@host:4222", wantURL: "nats://host:4222", user: "alice", pass: "pw"}, //nolint:gosec // a fake credential in a test fixture
		{name: "user and empty password", in: "nats://alice:@host:4222", wantURL: "nats://host:4222", user: "alice"},
		{name: "percent decoded", in: "nats://al%40ice:p%3Aw@host", wantURL: "nats://host", user: "al@ice", pass: "p:w"},
		{name: "tls scheme", in: "tls://TOK@host:4443", wantURL: "tls://host:4443", token: "TOK"},
		{name: "websocket path kept", in: "wss://TOK@host/nats?x=1", wantURL: "wss://host/nats?x=1", token: "TOK"},
		{name: "several servers", in: "nats://TOK@a:4222, nats://TOK@b:4222", wantURL: "nats://a:4222,nats://b:4222", token: "TOK"},
		{name: "several bare servers", in: "a:4222,b:4222", wantURL: "nats://a:4222,nats://b:4222"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := connect.ParseURL(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if c.URL != tc.wantURL {
				t.Errorf("URL = %q, want %q", c.URL, tc.wantURL)
			}
			o := applied(t, c.Options)
			if o.User != tc.user || o.Password != tc.pass || o.Token != tc.token {
				t.Errorf("auth = (%q,%q,%q), want (%q,%q,%q)", o.User, o.Password, o.Token, tc.user, tc.pass, tc.token)
			}
		})
	}
}

func TestParseURL_Errors(t *testing.T) {
	tests := []struct{ name, in string }{
		{"empty", ""},
		{"only commas", " , ,"},
		{"unsupported scheme", "http://SECRETTOKEN@host:80"},
		{"nsc scheme", "nsc://op/acct/user"},
		{"missing host", "nats://SECRETTOKEN@:4222"},
		{"mixed credentials", "nats://SECRETTOKEN@a:4222,nats://other@b:4222"},
		{"bad url", "nats://SECRETTOKEN@ho st:4222"},
		{"bad percent", "nats://SECRETTOKEN%zz@host:4222"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := connect.ParseURL(tc.in)
			_ = wantContextErr(t, err)
			if strings.Contains(err.Error(), "SECRETTOKEN") || strings.Contains(err.Error(), "other@") {
				t.Errorf("error leaks userinfo: %v", err)
			}
		})
	}
}

// ---- config directory and ReadContext (parity 1.3, 1.13) --------------------

// catalog: SAP-10.2-01
func TestReadContext_ConfigDirLookup(t *testing.T) {
	// The rules mirror client-sdk/typescript/src/context.ts resolveBaseDir and
	// client-sdk/python/.../context.py _nats_config_dir, which follow the NATS
	// CLI: $NATS_CONFIG_HOME, then $XDG_CONFIG_HOME/nats, then ~/.config/nats.
	e := newEnv(t)
	put := func(dir, who string) {
		e.write(filepath.Join(dir, "context", "c.json"), fmt.Sprintf(`{"url":%q}`, who))
	}
	put(e.dir, "nats://home:1")
	xdg := filepath.Join(e.home, "xdg")
	put(filepath.Join(xdg, "nats"), "nats://xdg:1")
	explicit := filepath.Join(e.home, "explicit")
	put(explicit, "nats://explicit:1")
	put(filepath.Join(e.home, "tilde"), "nats://tilde:1")
	override := filepath.Join(e.home, "override")
	put(override, "nats://override:1")

	tests := []struct {
		name   string
		env    map[string]string
		opts   []connect.ContextOption
		wantAt string
	}{
		{"home default", nil, nil, "home"},
		{"xdg", map[string]string{"XDG_CONFIG_HOME": xdg}, nil, "xdg"},
		{"nats config home beats xdg", map[string]string{"XDG_CONFIG_HOME": xdg, "NATS_CONFIG_HOME": explicit}, nil, "explicit"},
		{"nats config home tilde", map[string]string{"NATS_CONFIG_HOME": "~/tilde"}, nil, "tilde"},
		{"option beats env", map[string]string{"NATS_CONFIG_HOME": explicit}, []connect.ContextOption{connect.WithConfigDir(override)}, "override"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c, err := connect.LoadContext("c", tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			if want := "nats://" + tc.wantAt + ":1"; c.URL != want {
				t.Errorf("URL = %q, want %q", c.URL, want)
			}
		})
	}
}

func TestReadContext_Current(t *testing.T) {
	e := newEnv(t)
	e.context("a", map[string]any{"url": "nats://a:1"})
	e.context("b", map[string]any{"url": "nats://b:1"})

	t.Run("context.txt", func(t *testing.T) {
		e.write(filepath.Join(e.dir, "context.txt"), "a\n")
		cf, err := connect.ReadContext("current")
		if err != nil || cf.Name != "a" {
			t.Fatalf("got %v, %v", cf, err)
		}
	})
	t.Run("env wins over context.txt", func(t *testing.T) {
		t.Setenv("NATS_CONTEXT", "b")
		cf, err := connect.ReadContext("current")
		if err != nil || cf.Name != "b" {
			t.Fatalf("got %v, %v", cf, err)
		}
	})
	t.Run("empty selection file", func(t *testing.T) {
		e.write(filepath.Join(e.dir, "context.txt"), " \n")
		_, err := connect.ReadContext("current")
		_ = wantContextErr(t, err)
	})
	t.Run("no selection", func(t *testing.T) {
		if err := os.Remove(filepath.Join(e.dir, "context.txt")); err != nil {
			t.Fatal(err)
		}
		_, err := connect.ReadContext("current")
		_ = wantContextErr(t, err)
	})
}

func TestReadContext_NamesAndFiles(t *testing.T) {
	e := newEnv(t)
	e.write(filepath.Join(e.dir, "context", "bad.json"), `{"url": `)
	e.write(filepath.Join(e.dir, "context", "array.json"), `[1]`)
	e.write(filepath.Join(e.dir, "context", "null.json"), `null`)
	e.write(filepath.Join(e.dir, "context", "secretish.json"), `{"url": "nats://x:1", "token": "SECRETVALUE",`)
	e.context("ok", map[string]any{"url": "nats://ok:1"})

	for _, name := range []string{"", ".", "..", "../x", "a/b", `a\b`, "a\x00b"} {
		t.Run(fmt.Sprintf("invalid name %q", name), func(t *testing.T) {
			_, err := connect.ReadContext(name)
			_ = wantContextErr(t, err)
		})
	}
	for _, name := range []string{"missing", "bad", "array", "null", "secretish"} {
		t.Run(name, func(t *testing.T) {
			_, err := connect.ReadContext(name)
			ce := wantContextErr(t, err)
			if ce.Context != name {
				t.Errorf("Context = %q, want %q", ce.Context, name)
			}
			if strings.Contains(err.Error(), "SECRETVALUE") {
				t.Errorf("error leaks file content: %v", err)
			}
		})
	}
	t.Run("missing wraps fs.ErrNotExist", func(t *testing.T) {
		_, err := connect.ReadContext("missing")
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("error %v does not wrap os.ErrNotExist", err)
		}
	})
	t.Run("ok", func(t *testing.T) {
		cf, err := connect.ReadContext("ok")
		if err != nil {
			t.Fatal(err)
		}
		if cf.Name != "ok" || cf.Fields["url"] != "nats://ok:1" || !strings.HasSuffix(cf.Path, filepath.Join("context", "ok.json")) {
			t.Errorf("unexpected %+v", cf)
		}
		if s := fmt.Sprintf("%v %#v", cf, cf); strings.Contains(s, "nats://ok") {
			t.Errorf("ContextFile prints its fields: %s", s)
		}
	})
}

// ---- LoadContext (parity 1.4) -------------------------------------------------

// catalog: SAP-10.2-01
func TestLoadContext_Auth(t *testing.T) {
	e := newEnv(t)
	u := newUser(t)
	acct := newAccountPub(t)
	jwt := fakeJWT(t, u.pub, acct)

	credsPath := e.write(filepath.Join(e.home, "keys", "u.creds"), credsText(jwt, u.seed))
	nkeyPath := e.write(filepath.Join(e.home, "keys", "u.nk"), u.seed+"\n")

	tests := []struct {
		name   string
		fields map[string]any
		check  func(t *testing.T, o nats.Options)
	}{
		{"token", map[string]any{"url": serverURL, "token": "T1"}, func(t *testing.T, o nats.Options) {
			t.Helper()
			if o.Token != "T1" || o.User != "" {
				t.Errorf("token=%q user=%q", o.Token, o.User)
			}
		}},
		{"user and password", map[string]any{"url": serverURL, "user": "bob", "password": "pw"}, func(t *testing.T, o nats.Options) {
			t.Helper()
			if o.User != "bob" || o.Password != "pw" || o.Token != "" {
				t.Errorf("auth = %q %q %q", o.User, o.Password, o.Token)
			}
		}},
		{"user and password beat token", map[string]any{"url": serverURL, "user": "bob", "password": "pw", "token": "T"}, func(t *testing.T, o nats.Options) {
			t.Helper()
			if o.User != "bob" || o.Token != "" {
				t.Errorf("auth = %q %q", o.User, o.Token)
			}
		}},
		{"url userinfo token", map[string]any{"url": "nats://UT@127.0.0.1:4222"}, func(t *testing.T, o nats.Options) {
			t.Helper()
			if o.Token != "UT" {
				t.Errorf("token = %q", o.Token)
			}
		}},
		{"explicit field beats url userinfo", map[string]any{"url": "nats://UT@127.0.0.1:4222", "user": "bob", "password": "pw"}, func(t *testing.T, o nats.Options) {
			t.Helper()
			if o.Token != "" || o.User != "bob" {
				t.Errorf("token=%q user=%q", o.Token, o.User)
			}
		}},
		{"nkey file with trailing newline", map[string]any{"url": serverURL, "nkey": nkeyPath}, func(t *testing.T, o nats.Options) {
			t.Helper()
			if o.Nkey != u.pub || o.SignatureCB == nil {
				t.Errorf("Nkey = %q", o.Nkey)
			}
		}},
		{"creds file", map[string]any{"url": serverURL, "creds": credsPath}, func(t *testing.T, o nats.Options) {
			t.Helper()
			got, err := o.UserJWT()
			if err != nil || got != jwt || o.SignatureCB == nil {
				t.Errorf("jwt = %q, %v", got, err)
			}
		}},
		{"creds beat nkey, token and user", map[string]any{"url": serverURL, "creds": credsPath, "nkey": nkeyPath, "token": "T", "user": "x"}, func(t *testing.T, o nats.Options) {
			t.Helper()
			if o.Nkey != "" || o.Token != "" || o.User != "" || o.UserJWT == nil {
				t.Errorf("creds did not win: nkey=%q token=%q user=%q", o.Nkey, o.Token, o.User)
			}
		}},
		{"nkey beats user_jwt", map[string]any{"url": serverURL, "nkey": nkeyPath, "user_jwt": "x.y.z", "token": "T"}, func(t *testing.T, o nats.Options) {
			t.Helper()
			if o.Nkey != u.pub || o.UserJWT != nil {
				t.Errorf("nkey did not win")
			}
		}},
		{"user_jwt with seed", map[string]any{"url": serverURL, "user_jwt": jwt, "user_seed": u.seed}, func(t *testing.T, o nats.Options) {
			t.Helper()
			got, _ := o.UserJWT()
			sig, err := o.SignatureCB([]byte("nonce"))
			if got != jwt || err != nil || len(sig) != 64 {
				t.Errorf("jwt ok=%v sig len=%d err=%v", got == jwt, len(sig), err)
			}
		}},
		{"user_jwt without seed signs empty", map[string]any{"url": serverURL, "user_jwt": "a.b.c", "token": "T"}, func(t *testing.T, o nats.Options) {
			t.Helper()
			got, _ := o.UserJWT()
			sig, err := o.SignatureCB([]byte("nonce"))
			if got != "a.b.c" || err != nil || len(sig) != 0 || o.Token != "" {
				t.Errorf("jwt=%q sig=%d err=%v token=%q", got, len(sig), err, o.Token)
			}
		}},
		{"inbox prefix", map[string]any{"url": serverURL, "inbox_prefix": "_INBOX.custom"}, func(t *testing.T, o nats.Options) {
			t.Helper()
			if o.InboxPrefix != "_INBOX.custom" {
				t.Errorf("InboxPrefix = %q", o.InboxPrefix)
			}
		}},
		{"ignored upstream fields", map[string]any{"url": serverURL, "socks_proxy": "x", "jetstream_domain": "d", "color_scheme": "c", "windows_cert_store": "w"}, func(t *testing.T, o nats.Options) {
			t.Helper()
			if o.Token != "" || o.Nkey != "" || o.UserJWT != nil {
				t.Errorf("an ignored field changed the options")
			}
		}},
		{"tls_first alone", map[string]any{"url": serverURL, "tls_first": true}, func(t *testing.T, o nats.Options) {
			t.Helper()
			if !o.TLSHandshakeFirst || o.TLSConfig == nil || o.TLSConfig.MinVersion < 0x0303 {
				t.Errorf("tls_first not applied: %+v", o.TLSConfig)
			}
		}},
		{"tls_first false is ignored", map[string]any{"url": serverURL, "tls_first": false}, func(t *testing.T, o nats.Options) {
			t.Helper()
			if o.TLSConfig != nil || o.TLSHandshakeFirst {
				t.Errorf("TLS configured")
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e.context("c", tc.fields)
			c, err := connect.LoadContext("c")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(c.URL, "@") {
				t.Errorf("URL %q keeps userinfo", c.URL)
			}
			tc.check(t, applied(t, c.Options))
		})
	}
}

func TestLoadContext_PathExpansion(t *testing.T) {
	e := newEnv(t)
	u := newUser(t)
	e.write(filepath.Join(e.home, "keys", "u.nk"), u.seed)

	t.Run("tilde expands to HOME", func(t *testing.T) {
		e.context("c", map[string]any{"url": serverURL, "nkey": "~/keys/u.nk"})
		c, err := connect.LoadContext("c")
		if err != nil {
			t.Fatal(err)
		}
		if got := applied(t, c.Options).Nkey; got != u.pub {
			t.Errorf("Nkey = %q", got)
		}
	})
	t.Run("relative resolves against the working directory", func(t *testing.T) {
		t.Chdir(filepath.Join(e.home, "keys"))
		e.context("c", map[string]any{"url": serverURL, "nkey": "u.nk"})
		c, err := connect.LoadContext("c")
		if err != nil {
			t.Fatal(err)
		}
		if got := applied(t, c.Options).Nkey; got != u.pub {
			t.Errorf("Nkey = %q", got)
		}
	})
	t.Run("relative does not resolve against the context directory", func(t *testing.T) {
		t.Chdir(t.TempDir())
		e.write(filepath.Join(e.dir, "context", "u.nk"), u.seed)
		e.context("c", map[string]any{"url": serverURL, "nkey": "u.nk"})
		_, err := connect.LoadContext("c")
		_ = wantContextErr(t, err)
	})
	t.Run("a bare tilde-user path is not expanded", func(t *testing.T) {
		e.context("c", map[string]any{"url": serverURL, "nkey": "~keys/u.nk"})
		_, err := connect.LoadContext("c")
		_ = wantContextErr(t, err)
	})
}

func TestLoadContext_Errors(t *testing.T) {
	e := newEnv(t)
	u := newUser(t)
	acct := newAccountPub(t)
	stranger := newUser(t)
	badSeedText := "SUASECRETSEEDTEXTTHATISNOTVALID"
	e.write(filepath.Join(e.home, "bad.nk"), badSeedText)
	e.write(filepath.Join(e.home, "pub.nk"), u.pub)
	e.write(filepath.Join(e.home, "bad.creds"), "not a creds file "+badSeedText)
	e.write(filepath.Join(e.home, "mismatch.creds"), credsText(fakeJWT(t, stranger.pub, acct), u.seed))

	tests := []struct {
		name    string
		fields  map[string]any
		wantErr string // substring
		secret  string
	}{
		{"missing url", map[string]any{"token": "T"}, "missing `url`", ""},
		{"bad url", map[string]any{"url": "http://SECRETURLTOKEN@h:1"}, "unsupported scheme", "SECRETURLTOKEN"},
		{"url wrong type", map[string]any{"url": 5}, `"url" must be a string`, ""},
		{"token wrong type", map[string]any{"url": serverURL, "token": 5}, `"token" must be a string`, ""},
		{"creds wrong type", map[string]any{"url": serverURL, "creds": []any{"x"}}, `"creds" must be a string`, ""},
		{"tls_first wrong type", map[string]any{"url": serverURL, "tls_first": "yes"}, `"tls_first" must be a boolean`, ""},
		{"creds file missing", map[string]any{"url": serverURL, "creds": filepath.Join(e.home, "nope.creds")}, "cannot read creds file", ""},
		{"nkey file missing", map[string]any{"url": serverURL, "nkey": filepath.Join(e.home, "nope.nk")}, "cannot read nkey file", ""},
		{"nkey file invalid", map[string]any{"url": serverURL, "nkey": filepath.Join(e.home, "bad.nk")}, "not usable", badSeedText},
		{"nkey file holds a public key", map[string]any{"url": serverURL, "nkey": filepath.Join(e.home, "pub.nk")}, "not usable", ""},
		{"creds file invalid", map[string]any{"url": serverURL, "creds": filepath.Join(e.home, "bad.creds")}, "not usable", badSeedText},
		{"creds seed does not match jwt", map[string]any{"url": serverURL, "creds": filepath.Join(e.home, "mismatch.creds")}, "not usable", u.seed},
		{"user_seed invalid", map[string]any{"url": serverURL, "user_jwt": "a.b.c", "user_seed": badSeedText}, "not usable", badSeedText},
		{"user_seed does not match jwt", map[string]any{"url": serverURL, "user_jwt": fakeJWT(t, stranger.pub, acct), "user_seed": u.seed}, "not usable", u.seed},
		{"cert without key", map[string]any{"url": serverURL, "cert": filepath.Join(e.home, "c.pem")}, "together", ""},
		{"key without cert", map[string]any{"url": serverURL, "key": filepath.Join(e.home, "k.pem")}, "together", ""},
		{"ca missing", map[string]any{"url": serverURL, "ca": filepath.Join(e.home, "nope.pem")}, "cannot read ca file", ""},
		{"ca not pem", map[string]any{"url": serverURL, "ca": filepath.Join(e.home, "bad.nk")}, "no PEM certificate", badSeedText},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e.context("c", tc.fields)
			_, err := connect.LoadContext("c")
			ce := wantContextErr(t, err)
			if ce.Context != "c" {
				t.Errorf("Context = %q, want c", ce.Context)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q lacks %q", err, tc.wantErr)
			}
			for _, s := range []string{tc.secret, u.seed, stranger.seed} {
				if s != "" && strings.Contains(err.Error(), s) {
					t.Errorf("error leaks a secret: %v", err)
				}
			}
		})
	}
}

// ---- TLS ---------------------------------------------------------------------

func writeCertPair(t *testing.T, e *env) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "connect-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = e.write(filepath.Join(e.home, "tls", "cert.pem"), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	keyPath = e.write(filepath.Join(e.home, "tls", "key.pem"), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	return certPath, keyPath
}

func TestLoadContext_TLS(t *testing.T) {
	e := newEnv(t)
	certPath, keyPath := writeCertPair(t, e)

	t.Run("cert key ca", func(t *testing.T) {
		e.context("c", map[string]any{"url": "tls://127.0.0.1:4222", "cert": certPath, "key": keyPath, "ca": certPath, "tls_first": true})
		c, err := connect.LoadContext("c")
		if err != nil {
			t.Fatal(err)
		}
		o := applied(t, c.Options)
		if o.TLSConfig == nil || len(o.TLSConfig.Certificates) != 1 || o.TLSConfig.RootCAs == nil || !o.TLSHandshakeFirst {
			t.Fatalf("TLS not applied: %+v", o.TLSConfig)
		}
	})
	t.Run("tilde paths", func(t *testing.T) {
		e.context("c", map[string]any{"url": "tls://127.0.0.1:4222", "cert": "~/tls/cert.pem", "key": "~/tls/key.pem"})
		c, err := connect.LoadContext("c")
		if err != nil {
			t.Fatal(err)
		}
		if o := applied(t, c.Options); o.TLSConfig == nil || len(o.TLSConfig.Certificates) != 1 || o.TLSConfig.RootCAs != nil {
			t.Fatalf("TLS not applied: %+v", o.TLSConfig)
		}
	})
	t.Run("ca only", func(t *testing.T) {
		e.context("c", map[string]any{"url": "tls://127.0.0.1:4222", "ca": certPath})
		c, err := connect.LoadContext("c")
		if err != nil {
			t.Fatal(err)
		}
		if o := applied(t, c.Options); o.TLSConfig == nil || len(o.TLSConfig.Certificates) != 0 || o.TLSConfig.RootCAs == nil {
			t.Fatalf("TLS not applied: %+v", o.TLSConfig)
		}
	})
	t.Run("cert and key that do not pair", func(t *testing.T) {
		e.context("c", map[string]any{"url": "tls://127.0.0.1:4222", "cert": certPath, "key": certPath})
		_, err := connect.LoadContext("c")
		_ = wantContextErr(t, err)
	})
}

// ---- Bundle (parity 1.6 to 1.10) ----------------------------------------------

func TestSource_Validate(t *testing.T) {
	tests := []struct {
		name string
		src  connect.Source
		ok   bool
	}{
		{"context", connect.Source{Context: "c"}, true},
		{"url", connect.Source{URL: serverURL}, true},
		{"url with creds", connect.Source{URL: serverURL, Creds: "x"}, true},
		{"url with nkey", connect.Source{URL: serverURL, NKey: "x"}, true},
		{"empty", connect.Source{}, false},
		{"context and url", connect.Source{Context: "c", URL: serverURL}, false},
		{"context with creds", connect.Source{Context: "c", Creds: "x"}, false},
		{"context with nkey", connect.Source{Context: "c", NKey: "x"}, false},
		{"creds and nkey", connect.Source{URL: serverURL, Creds: "x", NKey: "y"}, false},
		{"creds without url", connect.Source{Creds: "x"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.src.Validate()
			if tc.ok != (err == nil) {
				t.Fatalf("Validate() = %v, want ok=%v", err, tc.ok)
			}
			if err != nil {
				_ = wantContextErr(t, err)
			}
		})
	}
}

func TestResolveBundle_Context(t *testing.T) {
	e := newEnv(t)
	u := newUser(t)
	acct := newAccountPub(t)
	jwt := fakeJWT(t, u.pub, acct)
	nk := e.write(filepath.Join(e.home, "u.nk"), u.seed)
	cr := e.write(filepath.Join(e.home, "u.creds"), credsText(jwt, u.seed))
	e.context("nkey", map[string]any{"url": serverURL, "nkey": nk})
	e.context("creds", map[string]any{"url": serverURL, "creds": cr})
	e.context("jwtseed", map[string]any{"url": serverURL, "user_jwt": jwt, "user_seed": u.seed})
	e.context("jwtonly", map[string]any{"url": serverURL, "user_jwt": jwt})
	e.context("token", map[string]any{"url": serverURL, "token": "T"})

	t.Run("signed", func(t *testing.T) {
		for _, name := range []string{"nkey", "creds", "jwtseed"} {
			t.Run(name, func(t *testing.T) {
				b, err := connect.ResolveBundle(t.Context(), connect.Source{Context: name}, connect.WithSignedIdentity())
				if err != nil {
					t.Fatal(err)
				}
				if b.Signer == nil || b.Signer.PublicKey() != u.pub {
					t.Fatalf("signer = %v", b.Signer)
				}
				if b.URL != serverURL || len(b.Options) == 0 {
					t.Errorf("connection = %q, %d options", b.URL, len(b.Options))
				}
			})
		}
	})
	t.Run("off has no signer", func(t *testing.T) {
		b, err := connect.ResolveBundle(t.Context(), connect.Source{Context: "nkey"})
		if err != nil || b.Signer != nil {
			t.Fatalf("signer = %v, err = %v", b.Signer, err)
		}
	})
	t.Run("signed without a seed", func(t *testing.T) {
		for _, name := range []string{"jwtonly", "token"} {
			_, err := connect.ResolveBundle(t.Context(), connect.Source{Context: name}, connect.WithSignedIdentity())
			_ = wantContextErr(t, err)
			if !errors.Is(err, connect.ErrNoUserSeed) {
				t.Errorf("%s: error %v does not match ErrNoUserSeed", name, err)
			}
		}
	})
	t.Run("missing context", func(t *testing.T) {
		_, err := connect.ResolveBundle(t.Context(), connect.Source{Context: "nope"})
		_ = wantContextErr(t, err)
	})
	t.Run("config dir option", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		b, err := connect.ResolveBundle(t.Context(), connect.Source{Context: "nkey"},
			connect.WithBundleContextOptions(connect.WithConfigDir(e.dir)))
		if err != nil || b.URL != serverURL {
			t.Fatalf("b=%v err=%v", b, err)
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := connect.ResolveBundle(ctx, connect.Source{Context: "nkey"})
		if !errors.Is(err, context.Canceled) || !errors.Is(err, connect.ErrContext) {
			t.Fatalf("err = %v, want both context.Canceled and ErrContext", err)
		}
	})
	t.Run("expired deadline", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(t.Context(), time.Unix(0, 0))
		defer cancel()
		_, err := connect.ResolveBundle(ctx, connect.Source{Context: "nkey"})
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, connect.ErrContext) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("ambiguous source", func(t *testing.T) {
		_, err := connect.ResolveBundle(t.Context(), connect.Source{Context: "nkey", URL: serverURL})
		_ = wantContextErr(t, err)
	})
}

func TestResolveBundle_URL(t *testing.T) {
	e := newEnv(t)
	u := newUser(t)
	nk := e.write(filepath.Join(e.home, "u.nk"), u.seed+"\n")
	cr := e.write(filepath.Join(e.home, "u.creds"), credsText(fakeJWT(t, u.pub, newAccountPub(t)), u.seed))

	t.Run("userinfo token", func(t *testing.T) {
		b, err := connect.ResolveBundle(t.Context(), connect.Source{URL: "nats://TOK@127.0.0.1:4222"})
		if err != nil {
			t.Fatal(err)
		}
		if o := applied(t, b.Options); o.Token != "TOK" || strings.Contains(b.URL, "TOK") {
			t.Errorf("token=%q url=%q", o.Token, b.URL)
		}
	})
	t.Run("nkey replaces userinfo", func(t *testing.T) {
		b, err := connect.ResolveBundle(t.Context(), connect.Source{URL: "nats://TOK@127.0.0.1:4222", NKey: nk}, connect.WithSignedIdentity())
		if err != nil {
			t.Fatal(err)
		}
		if o := applied(t, b.Options); o.Token != "" || o.Nkey != u.pub || b.Signer.PublicKey() != u.pub {
			t.Errorf("token=%q nkey=%q", o.Token, o.Nkey)
		}
	})
	t.Run("creds signed", func(t *testing.T) {
		b, err := connect.ResolveBundle(t.Context(), connect.Source{URL: serverURL, Creds: cr}, connect.WithSignedIdentity())
		if err != nil || b.Signer.PublicKey() != u.pub {
			t.Fatalf("b=%v err=%v", b, err)
		}
	})
	t.Run("signed needs a seed", func(t *testing.T) {
		_, err := connect.ResolveBundle(t.Context(), connect.Source{URL: serverURL}, connect.WithSignedIdentity())
		_ = wantContextErr(t, err)
		if !errors.Is(err, connect.ErrNoUserSeed) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("missing key file", func(t *testing.T) {
		_, err := connect.ResolveBundle(t.Context(), connect.Source{URL: serverURL, NKey: filepath.Join(e.home, "nope")})
		_ = wantContextErr(t, err)
	})
	t.Run("bad url", func(t *testing.T) {
		_, err := connect.ResolveBundle(t.Context(), connect.Source{URL: "ftp://x"})
		_ = wantContextErr(t, err)
	})
}

func TestBundle_WipeAndRedaction(t *testing.T) {
	e := newEnv(t)
	u := newUser(t)
	nk := e.write(filepath.Join(e.home, "u.nk"), u.seed)
	b, err := connect.ResolveBundle(t.Context(), connect.Source{URL: serverURL, NKey: nk}, connect.WithSignedIdentity())
	if err != nil {
		t.Fatal(err)
	}
	o := applied(t, b.Options)
	if _, err := o.SignatureCB([]byte("n")); err != nil {
		t.Fatalf("sign before wipe: %v", err)
	}
	for _, s := range []string{fmt.Sprint(b), fmt.Sprintf("%#v", b), fmt.Sprintf("%+v", b)} {
		if strings.Contains(s, u.seed) || s != "connect.Bundle(signed)" {
			t.Errorf("bundle prints %q", s)
		}
	}
	js, err := json.Marshal(b)
	if err != nil || string(js) != `{"identity":"signed"}` {
		t.Errorf("json = %s, %v", js, err)
	}

	b.Wipe()
	b.Wipe() // idempotent
	if b.Options != nil {
		t.Error("Wipe left the options")
	}
	if _, err := o.SignatureCB([]byte("n")); err == nil {
		t.Error("the connection can still sign after Wipe")
	}
	if _, err := b.Signer.Sign([]byte("n")); err == nil {
		t.Error("the signer can still sign after Wipe")
	}
	off, _ := connect.ResolveBundle(t.Context(), connect.Source{URL: serverURL})
	if fmt.Sprint(off) != "connect.Bundle(off)" {
		t.Errorf("off bundle prints %q", fmt.Sprint(off))
	}
	off.Wipe()
}

// ---- SignerFromContext (parity 10.10) -----------------------------------------

func TestSignerFromContext(t *testing.T) {
	e := newEnv(t)
	u := newUser(t)
	acct := newAccountPub(t)
	jwt := fakeJWT(t, u.pub, acct)
	nk := e.write(filepath.Join(e.home, "u.nk"), u.seed)
	cr := e.write(filepath.Join(e.home, "u.creds"), credsText(jwt, u.seed))

	tests := []struct {
		name    string
		fields  map[string]any
		wantJWT bool
		noSeed  bool
	}{
		{"creds", map[string]any{"url": serverURL, "creds": cr}, true, false},
		{"nkey", map[string]any{"url": serverURL, "nkey": nk}, false, false},
		{"user_seed", map[string]any{"url": serverURL, "user_seed": u.seed}, false, false},
		{"user_seed and user_jwt", map[string]any{"url": serverURL, "user_seed": u.seed, "user_jwt": jwt}, true, false},
		{"creds win over nkey", map[string]any{"url": serverURL, "creds": cr, "nkey": "/does/not/exist"}, true, false},
		{"nothing to sign with", map[string]any{"url": serverURL, "token": "T"}, false, true},
		{"jwt only", map[string]any{"url": serverURL, "user_jwt": jwt}, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e.context("c", tc.fields)
			s, err := connect.SignerFromContext("c")
			if tc.noSeed {
				_ = wantContextErr(t, err)
				if !errors.Is(err, connect.ErrNoUserSeed) {
					t.Errorf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.PublicKey() != u.pub {
				t.Errorf("PublicKey = %q", s.PublicKey())
			}
			// The concrete signer always has a JWT method; it is empty without a JWT.
			js, ok := s.(identity.JWTSigner)
			hasJWT := ok && js.JWT() == jwt
			if hasJWT != tc.wantJWT {
				t.Errorf("JWT signer = %v, want %v", hasJWT, tc.wantJWT)
			}
			sig, err := s.Sign([]byte("data"))
			if err != nil || len(sig) != 64 {
				t.Errorf("Sign: %d bytes, %v", len(sig), err)
			}
		})
	}
	t.Run("missing context", func(t *testing.T) {
		_, err := connect.SignerFromContext("nope")
		_ = wantContextErr(t, err)
	})
	t.Run("bad seed does not leak", func(t *testing.T) {
		e.context("c", map[string]any{"url": serverURL, "user_seed": "SUASECRETNOTASEED"})
		_, err := connect.SignerFromContext("c")
		_ = wantContextErr(t, err)
		if strings.Contains(err.Error(), "SECRETNOTASEED") {
			t.Errorf("error leaks the seed: %v", err)
		}
	})
}

// ---- reconnect defaults (parity 1.11, 1.12) -----------------------------------

func TestReconnectDefaults(t *testing.T) {
	o := applied(t, connect.ReconnectDefaults())
	if o.MaxReconnect != -1 || o.ReconnectWait != 2*time.Second || o.ReconnectJitter != 200*time.Millisecond ||
		o.ReconnectJitterTLS != time.Second || !o.RetryOnFailedConnect {
		t.Errorf("defaults = %+v", o)
	}
}

func TestWithAgentReconnectDefaults(t *testing.T) {
	user := []nats.Option{nats.MaxReconnects(0), nats.RetryOnFailedConnect(false), nats.Name("x")}
	out := applied(t, connect.WithAgentReconnectDefaults(user...))
	if out.MaxReconnect != 0 || out.RetryOnFailedConnect || out.Name != "x" {
		t.Errorf("caller values did not win: %+v", out)
	}
	if out.ReconnectWait != 2*time.Second || out.ReconnectJitter != 200*time.Millisecond {
		t.Errorf("unset defaults missing: %+v", out)
	}
	if len(user) != 3 {
		t.Error("input slice modified")
	}
	if n := len(connect.WithAgentReconnectDefaults()); n != len(connect.ReconnectDefaults()) {
		t.Errorf("no caller options gave %d options", n)
	}
}

// A lone inline user_seed is the connection's credential: LoadContext and the
// bundle path authenticate as the key SignerFromContext signs with.
func TestLoneUserSeed_ConnectionMatchesSigner(t *testing.T) {
	e := newEnv(t)
	u := newUser(t)
	e.context("lone", map[string]any{"url": serverURL, "user_seed": u.seed})
	e.context("none", map[string]any{"url": serverURL})
	e.context("tokened", map[string]any{"url": serverURL, "user_seed": u.seed, "token": "T"})

	s, err := connect.SignerFromContext("lone")
	if err != nil {
		t.Fatal(err)
	}
	if s.PublicKey() != u.pub {
		t.Fatalf("signer PublicKey = %q, want %q", s.PublicKey(), u.pub)
	}
	check := func(t *testing.T, o nats.Options) {
		t.Helper()
		if o.Nkey != u.pub || o.SignatureCB == nil || o.Token != "" || o.UserJWT != nil {
			t.Fatalf("options do not carry nkey auth for %q: nkey=%q", u.pub, o.Nkey)
		}
		if o.Nkey != s.PublicKey() {
			t.Errorf("connection user %q != signer %q", o.Nkey, s.PublicKey())
		}
	}
	t.Run("LoadContext", func(t *testing.T) {
		c, err := connect.LoadContext("lone")
		if err != nil {
			t.Fatal(err)
		}
		check(t, applied(t, c.Options))
	})
	t.Run("ResolveBundle signed", func(t *testing.T) {
		b, err := connect.ResolveBundle(t.Context(), connect.Source{Context: "lone"}, connect.WithSignedIdentity())
		if err != nil {
			t.Fatal(err)
		}
		defer b.Wipe()
		check(t, applied(t, b.Options))
		if b.Signer == nil || b.Signer.PublicKey() != u.pub {
			t.Fatalf("bundle signer = %v", b.Signer)
		}
	})
	t.Run("ResolveBundle unsigned still connects as the seed", func(t *testing.T) {
		b, err := connect.ResolveBundle(t.Context(), connect.Source{Context: "lone"})
		if err != nil {
			t.Fatal(err)
		}
		defer b.Wipe()
		check(t, applied(t, b.Options))
	})
	t.Run("no seed, no signer", func(t *testing.T) {
		if s, err := connect.SignerFromContext("none"); err == nil || s != nil || !errors.Is(err, connect.ErrNoUserSeed) {
			t.Fatalf("signer = %v, err = %v", s, err)
		}
		c, err := connect.LoadContext("none")
		if err != nil {
			t.Fatal(err)
		}
		if o := applied(t, c.Options); o.Nkey != "" {
			t.Errorf("anonymous context carries nkey %q", o.Nkey)
		}
	})
	t.Run("seed beside a token: connection uses the token, signer refuses", func(t *testing.T) {
		c, err := connect.LoadContext("tokened")
		if err != nil {
			t.Fatal(err)
		}
		if o := applied(t, c.Options); o.Token != "T" || o.Nkey != "" {
			t.Errorf("token=%q nkey=%q", o.Token, o.Nkey)
		}
		if s, err := connect.SignerFromContext("tokened"); err == nil || s != nil {
			t.Errorf("signer = %v, err = %v", s, err)
		}
	})
}
