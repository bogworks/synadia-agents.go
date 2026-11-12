// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package connect_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bogworks/synadia-agents.go/connect"
	"github.com/bogworks/synadia-agents.go/identity"
)

// ---- Source redaction (AT-013, finding 1) -----------------------------------

func TestSource_FormattingRedactsURLUserinfo(t *testing.T) {
	srcs := []connect.Source{
		{URL: "nats://TOPSECRETTOKEN@host:4222"},
		{URL: "nats://alice:TOPSECRETPW@host:4222", Creds: "/keys/a.creds"},                          //nolint:gosec // a fake credential in a test fixture
		{URL: "nats://alice:TOPSECRETPW@a:4222,nats://alice:TOPSECRETPW@b:4222", NKey: "/keys/a.nk"}, //nolint:gosec // a fake credential in a test fixture
		{URL: "nats://alice:TOPSECRETPW,more@host"},                                                  // malformed userinfo
		{Context: "prod"},
	}
	for _, s := range srcs {
		var js []byte
		var err error
		if js, err = json.Marshal(s); err != nil {
			t.Fatal(err)
		}
		pjs, err := json.Marshal(&s)
		if err != nil {
			t.Fatal(err)
		}
		outs := map[string]string{
			"%v": fmt.Sprintf("%v", s), "%+v": fmt.Sprintf("%+v", s), "%#v": fmt.Sprintf("%#v", s),
			"%s":     fmt.Sprintf("%s", s), //nolint:staticcheck // S1025: the test exercises the fmt %s verb on purpose
			"ptr %v": fmt.Sprintf("%v", &s), "ptr %#v": fmt.Sprintf("%#v", &s),
			"json": string(js), "json ptr": string(pjs),
		}
		for verb, out := range outs {
			if strings.Contains(out, "TOPSECRET") {
				t.Errorf("Source %s leaks userinfo: %s", verb, out)
			}
		}
	}
	// What is safe stays visible: host and key file paths.
	out := fmt.Sprintf("%+v", srcs[1])
	if !strings.Contains(out, "host:4222") || !strings.Contains(out, "/keys/a.creds") {
		t.Errorf("redaction removed too much: %s", out)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(fmt.Sprint(mustJSON(t, srcs[0]))), &m); err != nil || m["URL"] == "" {
		t.Errorf("json shape: %v %v", m, err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // the test reads a fixture or temp path it built itself
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---- URL errors (finding 2) ---------------------------------------------------

func TestParseURL_ErrorsNeverQuoteUserinfo(t *testing.T) {
	inputs := []string{
		"nats://alice:secret,more@host",
		"alice:secret,more@host",
		"nats://alice:secret,more@host,nats://alice:secret,more@host2",
		"nats://alice:secret@ho st",
		"nats://alice:secret://x@host",
		"nats://alice:se%zzcret@host",
		"nats://alice:secret@",
		"nats://alice:secret@host:99999999999",
		"http://alice:secret@host",
		"nats://tok@a,nats://alice:secret@b",
		"nats://alice:secret,more@",
		"nats://alice:secret/x,more@host",
	}
	for _, in := range inputs {
		_, err := connect.ParseURL(in)
		if err == nil {
			continue // accepted forms cannot leak through an error
		}
		if !errors.Is(err, connect.ErrContext) {
			t.Errorf("%q: error %v does not match ErrContext", in, err)
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "alice") {
			t.Errorf("%q: error leaks userinfo: %v", in, err)
		}
	}
	if _, err := connect.ParseURL("nats://alice:secret,more@host"); err == nil {
		t.Error("malformed userinfo was accepted")
	}
}

// ---- ContextFile JSON (finding 3) ---------------------------------------------

func TestContextFile_MarshalJSONRedactsSecrets(t *testing.T) {
	e := newEnv(t)
	e.context("c", map[string]any{ //nolint:gosec // a fake credential in a test fixture
		"url":          "nats://TOKENINURL@host:4222",
		"user_seed":    "SUASECRETSEEDVALUE",
		"user_jwt":     "JWTSECRETVALUE",
		"token":        "SECRETTOKENVALUE",
		"password":     "SECRETPASSWORDVALUE",
		"user":         "bob",
		"creds":        "/keys/bob.creds",
		"extra":        map[string]any{"api_secret": "NESTEDSECRETVALUE", "ok": "visible"},
		"inbox_prefix": "_INBOX.x",
	})
	cf, err := connect.ReadContext("c")
	if err != nil {
		t.Fatal(err)
	}
	js, err := json.Marshal(cf)
	if err != nil {
		t.Fatal(err)
	}
	pjs, _ := json.Marshal(&cf)
	for _, out := range []string{string(js), string(pjs)} {
		for _, secret := range []string{"SUASECRET", "JWTSECRET", "SECRETTOKEN", "SECRETPASSWORD", "NESTEDSECRET", "TOKENINURL"} {
			if strings.Contains(out, secret) {
				t.Errorf("ContextFile JSON leaks %s: %s", secret, out)
			}
		}
		for _, keep := range []string{"/keys/bob.creds", "bob", "visible", "_INBOX.x", "host:4222", `"c"`} {
			if !strings.Contains(out, keep) {
				t.Errorf("ContextFile JSON dropped %q: %s", keep, out)
			}
		}
	}
	// Raw access stays explicit.
	if cf.Fields["user_seed"] != "SUASECRETSEEDVALUE" {
		t.Error("Fields no longer hold the raw value")
	}
	if cf.Fields["url"] != "nats://TOKENINURL@host:4222" {
		t.Error("marshaling mutated Fields")
	}
}

// ---- signer vs connection identity (finding 4) --------------------------------

func TestSignerFromContext_RefusesIdentityDifferentFromConnection(t *testing.T) {
	e := newEnv(t)
	u := newUser(t)
	acct := newAccountPub(t)
	jwt := fakeJWT(t, u.pub, acct)
	tests := []struct {
		name   string
		fields map[string]any
		wantOK bool
	}{
		{"seed beside token", map[string]any{"url": serverURL, "user_seed": u.seed, "token": "T"}, false},
		{"seed beside user and password", map[string]any{"url": serverURL, "user_seed": u.seed, "user": "bob", "password": "pw"}, false},
		{"seed beside url token", map[string]any{"url": "nats://T@host:4222", "user_seed": u.seed}, false},
		{"seed beside url user and password", map[string]any{"url": "nats://bob:pw@host:4222", "user_seed": u.seed}, false}, //nolint:gosec // a fake credential in a test fixture
		{"lone seed, no other credential", map[string]any{"url": serverURL, "user_seed": u.seed}, true},
		{"seed with its jwt and a token", map[string]any{"url": serverURL, "user_seed": u.seed, "user_jwt": jwt, "token": "T"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e.context("c", tc.fields)
			s, err := connect.SignerFromContext("c")
			if tc.wantOK {
				if err != nil || s.PublicKey() != u.pub {
					t.Fatalf("err = %v", err)
				}
				return
			}
			_ = wantContextErr(t, err)
			if !errors.Is(err, identity.ErrIdentityMismatch) {
				t.Errorf("err = %v, want ErrIdentityMismatch", err)
			}
			for _, secret := range []string{u.seed, "T@", "pw"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaks %q: %v", secret, err)
				}
			}
			if s != nil {
				t.Error("a signer was returned with the error")
			}
		})
	}
}

// ---- key file permissions (finding 6) -----------------------------------------

func writeMode(t *testing.T, path, content string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // beat the umask
		t.Fatal(err)
	}
	return path
}

func TestKeyFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes are not checked on Windows")
	}
	e := newEnv(t)
	u := newUser(t)
	jwt := fakeJWT(t, u.pub, newAccountPub(t))
	cp0, kp0 := writeCertPair(t, e)
	certPEM, keyPEM := mustRead(t, cp0), mustRead(t, kp0)

	type kind struct {
		name   string
		fields func(path string) map[string]any
		body   string
	}
	kinds := []kind{
		{"nkey", func(p string) map[string]any { return map[string]any{"url": serverURL, "nkey": p} }, u.seed + "\n"},
		{"creds", func(p string) map[string]any { return map[string]any{"url": serverURL, "creds": p} }, credsText(jwt, u.seed)},
	}
	modes := []struct {
		mode   os.FileMode
		reject bool
	}{
		{0o600, false}, {0o400, false}, {0o640, true}, {0o604, true}, {0o644, true}, {0o660, true}, {0o666, true}, {0o700, false}, {0o707, true},
	}
	for _, k := range kinds {
		for _, m := range modes {
			t.Run(fmt.Sprintf("%s %04o", k.name, m.mode), func(t *testing.T) {
				p := writeMode(t, filepath.Join(e.home, "keys", k.name+fmt.Sprintf("%04o", m.mode)), k.body, m.mode)
				e.context("c", k.fields(p))
				_, err := connect.LoadContext("c")
				if !m.reject {
					if err != nil {
						t.Fatalf("mode %04o refused: %v", m.mode, err)
					}
					return
				}
				_ = wantContextErr(t, err)
				if !strings.Contains(err.Error(), "0600") || !strings.Contains(err.Error(), p) {
					t.Errorf("error does not name the file and 0600: %v", err)
				}
				if strings.Contains(err.Error(), u.seed) {
					t.Errorf("error leaks the seed: %v", err)
				}
				// The relax option accepts the file.
				if _, err := connect.LoadContext("c", connect.WithInsecureKeyFilePermissions()); err != nil {
					t.Errorf("relaxed load failed: %v", err)
				}
				// So does a bundle through its context options.
				if _, err := connect.ResolveBundle(t.Context(), connect.Source{Context: "c"}, connect.WithSignedIdentity()); err == nil {
					t.Error("bundle accepted an insecure key file")
				}
				if _, err := connect.ResolveBundle(t.Context(), connect.Source{Context: "c"}, connect.WithSignedIdentity(),
					connect.WithBundleContextOptions(connect.WithInsecureKeyFilePermissions())); err != nil {
					t.Errorf("relaxed bundle failed: %v", err)
				}
				if _, err := connect.SignerFromContext("c"); err == nil {
					t.Error("SignerFromContext accepted an insecure key file")
				}
			})
		}
	}

	t.Run("URL source files", func(t *testing.T) {
		p := writeMode(t, filepath.Join(e.home, "keys", "src.nk"), u.seed+"\n", 0o644)
		src := connect.Source{URL: serverURL, NKey: p}
		if _, err := connect.ResolveBundle(t.Context(), src); err == nil || !strings.Contains(err.Error(), "0600") {
			t.Errorf("err = %v", err)
		}
		if _, err := connect.ResolveBundle(t.Context(), src, connect.WithBundleContextOptions(connect.WithInsecureKeyFilePermissions())); err != nil {
			t.Errorf("relaxed: %v", err)
		}
		cp := writeMode(t, filepath.Join(e.home, "keys", "src.creds"), credsText(jwt, u.seed), 0o644)
		if _, err := connect.ResolveBundle(t.Context(), connect.Source{URL: serverURL, Creds: cp}); err == nil {
			t.Error("creds with 0644 accepted")
		}
	})

	t.Run("TLS private key", func(t *testing.T) {
		cert := writeMode(t, filepath.Join(e.home, "tls", "c.crt"), certPEM, 0o644) // public: any mode
		key := writeMode(t, filepath.Join(e.home, "tls", "c.key"), keyPEM, 0o644)
		e.context("t", map[string]any{"url": serverURL, "cert": cert, "key": key})
		_, err := connect.LoadContext("t")
		_ = wantContextErr(t, err)
		if !strings.Contains(err.Error(), "0600") || !strings.Contains(err.Error(), key) {
			t.Errorf("err = %v", err)
		}
		if strings.Contains(err.Error(), "PRIVATE KEY") {
			t.Errorf("error quotes key material: %v", err)
		}
		if _, err := connect.LoadContext("t", connect.WithInsecureKeyFilePermissions()); err != nil {
			t.Errorf("relaxed: %v", err)
		}
		if err := os.Chmod(key, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := connect.LoadContext("t"); err != nil {
			t.Errorf("0600 key refused: %v", err)
		}
	})

	t.Run("CA file may be public", func(t *testing.T) {
		ca := writeMode(t, filepath.Join(e.home, "tls", "ca.pem"), certPEM, 0o644)
		e.context("ca", map[string]any{"url": serverURL, "ca": ca})
		if _, err := connect.LoadContext("ca"); err != nil {
			t.Errorf("public CA refused: %v", err)
		}
	})
}

// ---- context file reading: regular file, size cap, permissions ---------------

func TestReadContext_FileChecks(t *testing.T) {
	e := newEnv(t)
	ctxPath := func(n string) string { return filepath.Join(e.dir, "context", n+".json") }

	t.Run("not a regular file", func(t *testing.T) {
		if err := os.MkdirAll(ctxPath("dir"), 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := connect.ReadContext("dir")
		_ = wantContextErr(t, err)
	})
	t.Run("over the size cap", func(t *testing.T) {
		big := `{"url": "nats://x:1", "pad": "` + strings.Repeat("a", 1<<20) + `"}`
		e.write(ctxPath("big"), big)
		_, err := connect.ReadContext("big")
		_ = wantContextErr(t, err)
		if !strings.Contains(err.Error(), "too large") {
			t.Errorf("error does not say why: %v", err)
		}
	})
	t.Run("current selection is a directory", func(t *testing.T) {
		e2 := newEnv(t)
		if err := os.MkdirAll(filepath.Join(e2.dir, "context.txt"), 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("NATS_CONTEXT", "")
		_, err := connect.ReadContext("current")
		_ = wantContextErr(t, err)
	})
	t.Run("current selection over the size cap", func(t *testing.T) {
		e2 := newEnv(t)
		e2.write(filepath.Join(e2.dir, "context.txt"), strings.Repeat("a", 1<<20+1))
		t.Setenv("NATS_CONTEXT", "")
		_, err := connect.ReadContext("current")
		_ = wantContextErr(t, err)
	})

	if runtime.GOOS == "windows" {
		return
	}
	secrets := map[string]string{"user_seed": "SUAXSECRET", "password": "PWSECRET", "token": "TOKSECRET", "user_jwt": "JWTSECRET"}
	for field, v := range secrets {
		body := fmt.Sprintf(`{"url": "nats://x:1", %q: %q}`, field, v)
		t.Run("group-readable context with inline "+field, func(t *testing.T) {
			p := writeMode(t, ctxPath("shared-"+field), body, 0o644)
			_, err := connect.ReadContext("shared-" + field)
			_ = wantContextErr(t, err)
			if !strings.Contains(err.Error(), "0600") || strings.Contains(err.Error(), v) {
				t.Errorf("error %v", err)
			}
			if _, err := connect.ReadContext("shared-"+field, connect.WithInsecureKeyFilePermissions()); err != nil {
				t.Errorf("WithInsecureKeyFilePermissions not honored: %v", err)
			}
			if err := os.Chmod(p, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := connect.ReadContext("shared-" + field); err != nil {
				t.Errorf("0600 refused: %v", err)
			}
		})
	}
	urlCases := []struct {
		name, url, secret string
	}{
		{"token", "nats://URLTOKSECRET@host:4222", "URLTOKSECRET"},
		{"user and password", "nats://alice:URLPWSECRET@host:4222", "URLPWSECRET"},
		{"second server", "nats://a:4222, nats://URLSECOND2@b:4222", "URLSECOND2"},
		{"unparsable", "nats://bad%zzURLBADSECRET@host:4222", "URLBADSECRET"},
	}
	for _, tc := range urlCases {
		t.Run("url userinfo "+tc.name, func(t *testing.T) {
			name := "urlui-" + strings.ReplaceAll(tc.name, " ", "-")
			body := fmt.Sprintf(`{"url": %q}`, tc.url)
			p := writeMode(t, ctxPath(name), body, 0o644)
			_, err := connect.ReadContext(name)
			_ = wantContextErr(t, err)
			if !strings.Contains(err.Error(), "0600") || strings.Contains(err.Error(), tc.secret) {
				t.Errorf("error %v", err)
			}
			if err := os.Chmod(p, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := connect.ReadContext(name); err != nil {
				t.Errorf("0600 refused: %v", err)
			}
		})
	}
	t.Run("group-readable context without a secret", func(t *testing.T) {
		writeMode(t, ctxPath("plain"), `{"url": "nats://x:1", "creds": "/keys/a.creds"}`, 0o644)
		if _, err := connect.ReadContext("plain"); err != nil {
			t.Errorf("a context with no inline secret was refused: %v", err)
		}
		writeMode(t, ctxPath("plainlist"), `{"url": "nats://a:1,nats://b:2"}`, 0o644)
		if _, err := connect.ReadContext("plainlist"); err != nil {
			t.Errorf("a server list without userinfo was refused: %v", err)
		}
	})
}
