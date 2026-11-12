// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package connect

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
)

// CurrentContext is the selector that resolves through $NATS_CONTEXT and the
// context.txt selection file.
const CurrentContext = "current"

// ContextFile is a NATS CLI context file that has been read and parsed but not
// turned into options. It may name key files and may hold an inline seed in
// Fields; treat it as sensitive and do not print it. See parity row 1.3;
// outside SAP.
type ContextFile struct {
	// Name is the resolved context name (after "current" resolution).
	Name string
	// Path is the absolute path of the context JSON file.
	Path string
	// Fields is the parsed JSON object, verbatim.
	Fields map[string]any
}

// String prints the name and path only, never the fields.
func (c ContextFile) String() string { return "ContextFile(" + c.Name + ")" }

// GoString is String, so %#v cannot print the fields.
func (c ContextFile) GoString() string { return c.String() }

// MarshalJSON redacts the fields that hold secrets: any field whose name
// contains "seed", "token", "password", "passwd", "secret", "jwt" or "private"
// (user_seed, token, password, user_jwt, ...), at any depth, and the userinfo
// of "url". Fields itself stays raw for callers that need a value.
func (c ContextFile) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Name   string         `json:"name"`
		Path   string         `json:"path"`
		Fields map[string]any `json:"fields"`
	}{c.Name, c.Path, redactFields(c.Fields)})
}

const redacted = "[redacted]"

func secretFieldName(k string) bool {
	k = strings.ToLower(k)
	for _, w := range []string{"seed", "token", "password", "passwd", "secret", "jwt", "private"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

func redactFields(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch {
		case v == nil:
			out[k] = nil
		case secretFieldName(k):
			out[k] = redacted
		default:
			out[k] = redactValue(k, v)
		}
	}
	return out
}

func redactValue(k string, v any) any {
	switch x := v.(type) {
	case map[string]any:
		return redactFields(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = redactValue(k, e)
		}
		return out
	case string:
		if strings.EqualFold(k, "url") {
			return displayURL(x)
		}
	}
	return v
}

// ContextOption customizes how a context is located.
type ContextOption func(*contextConfig)

type contextConfig struct {
	dir              string
	insecureKeyFiles bool
}

// WithInsecureKeyFilePermissions turns off the key file permission check. By
// default, on Unix, a creds, nkey (seed) or TLS private key file that group or
// others can access (mode & 0o077 != 0) is refused with an error that names the
// required mode, 0600: a shared key is almost always a deployment mistake. Use
// this option only where the file system cannot hold modes (some mounted
// volumes) or the key is deliberately shared. The check does not run on
// Windows. It applies to [LoadContext], [SignerFromContext] and, through
// [WithBundleContextOptions], to [ResolveBundle]. SDK policy; outside SAP.
func WithInsecureKeyFilePermissions() ContextOption {
	return func(c *contextConfig) { c.insecureKeyFiles = true }
}

// WithConfigDir reads contexts from dir/context/NAME.json (and the selection
// from dir/context.txt) instead of the directory the NATS CLI would use, and
// ignores $NATS_CONFIG_HOME and $XDG_CONFIG_HOME. $NATS_CONTEXT still resolves
// "current". A leading "~/" in dir is expanded.
func WithConfigDir(dir string) ContextOption {
	return func(c *contextConfig) { c.dir = dir }
}

func newContextConfig(opts []ContextOption) contextConfig {
	var c contextConfig
	for _, o := range opts {
		o(&c)
	}
	return c
}

// configDir mirrors the official SDKs, which mirror the NATS CLI: first
// $NATS_CONFIG_HOME (with "~/" expansion), then $XDG_CONFIG_HOME/nats, then
// ~/.config/nats.
func (c contextConfig) configDir() (string, error) {
	if c.dir != "" {
		return expandHome(c.dir)
	}
	if d := os.Getenv("NATS_CONFIG_HOME"); d != "" {
		return expandHome(d)
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "nats"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ctxErrWrap("", err, "cannot locate the home directory")
	}
	return filepath.Join(home, ".config", "nats"), nil
}

// expandHome expands a leading "~/" to the home directory, as the NATS CLI
// does. Any other path, including a relative one, is returned unchanged.
func expandHome(p string) (string, error) {
	if !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ctxErrWrap("", err, "cannot expand ~ in a path: no home directory")
	}
	return filepath.Join(home, p[2:]), nil
}

// ReadContext reads the NATS CLI context named selector, or the current one
// when selector is "current" ([CurrentContext]). It is the reading half of
// [LoadContext]; it reads the context file and nothing it points to. The name
// must not be empty, contain "/", "\" or NUL, or be "." or "..". See the
// package documentation for the lookup rules, and parity row 1.3; outside SAP
// (deviation D-1).
func ReadContext(selector string, opts ...ContextOption) (ContextFile, error) {
	cfg := newContextConfig(opts)
	dir, err := cfg.configDir()
	if err != nil {
		return ContextFile{}, err
	}
	name := selector
	if selector == CurrentContext {
		if name, err = currentName(dir); err != nil {
			return ContextFile{}, err
		}
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return ContextFile{}, ctxErr("", "invalid context name %q", name)
	}
	path := filepath.Join(dir, "context", name+".json")
	raw, mode, err := readSmallFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ContextFile{}, ctxErrWrap(name, err, "not found at %s", path)
		}
		return ContextFile{}, ctxErrWrap(name, err, "cannot read %s", path)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		// Never include the decoder's text: it can quote file content.
		return ContextFile{}, ctxErr(name, "%s is not a valid JSON object", path)
	}
	cf := ContextFile{Name: name, Path: path, Fields: fields}
	// A context that holds a secret inline is a credential file: apply the
	// same 0600 rule as a creds or seed file (SDK policy; outside SAP).
	if !cfg.insecureKeyFiles && runtime.GOOS != "windows" && cf.hasInlineSecret() {
		if perm := mode.Perm(); perm&0o077 != 0 {
			return ContextFile{}, ctxErr(name, "context file %s holds a secret and is accessible by group or others (mode %04o): it must be mode 0600 (chmod 600), or pass WithInsecureKeyFilePermissions",
				path, perm)
		}
	}
	return cf, nil
}

// maxContextFileSize bounds a context file and context.txt: a real one is a
// few hundred bytes, so anything larger is not read (SDK policy).
const maxContextFileSize = 1 << 20

// readSmallFile opens path, requires a regular file of at most
// [maxContextFileSize] bytes (checked on the open file, not the path) and
// returns its content and mode. A directory, device or named pipe is refused
// before any read, so a hostile path cannot block or exhaust memory.
func readSmallFile(path string) ([]byte, fs.FileMode, error) {
	// A path-level check first: opening a named pipe for reading can block.
	if li, err := os.Stat(path); err == nil && !li.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path) //nolint:gosec // G304: the context directory and name are the caller's, validated upstream
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Size() > maxContextFileSize {
		return nil, 0, fmt.Errorf("%s is too large (over %d bytes)", path, maxContextFileSize)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxContextFileSize+1))
	if err != nil {
		return nil, 0, err
	}
	if len(raw) > maxContextFileSize {
		return nil, 0, fmt.Errorf("%s is too large (over %d bytes)", path, maxContextFileSize)
	}
	return raw, fi.Mode(), nil
}

// hasInlineSecret reports whether the context carries a credential in the
// file itself. The "nkey" and "creds" fields are paths, not secrets. Userinfo
// (a token, or user and password) in any listed server URL is an inline secret.
func (c ContextFile) hasInlineSecret() bool {
	for _, k := range []string{"user_seed", "password", "token", "user_jwt"} {
		if v, ok := c.Fields[k].(string); ok && v != "" {
			return true
		}
	}
	if u, ok := c.Fields["url"].(string); ok && urlListHasUserinfo(u) {
		return true
	}
	return false
}

// urlListHasUserinfo reports whether any entry of a comma-separated server
// list carries URL userinfo. An entry that does not parse but contains '@' is
// treated as a secret, conservatively. It never returns or logs the entry.
func urlListHasUserinfo(list string) bool {
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parse := entry
		if !strings.Contains(parse, "://") {
			parse = "nats://" + parse
		}
		u, err := url.Parse(parse)
		if err != nil {
			if strings.Contains(entry, "@") {
				return true
			}
			continue
		}
		if u.User != nil {
			return true
		}
	}
	return false
}

func currentName(dir string) (string, error) {
	if n := os.Getenv("NATS_CONTEXT"); n != "" {
		return n, nil
	}
	path := filepath.Join(dir, "context.txt")
	raw, _, err := readSmallFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ctxErrWrap("", err, "no context is selected ($NATS_CONTEXT is unset and %s does not exist)", path)
		}
		return "", ctxErrWrap("", err, "cannot read %s", path)
	}
	name := strings.TrimSpace(string(raw))
	if name == "" {
		return "", ctxErr("", "no context is selected (%s is empty)", path)
	}
	return name, nil
}

// field returns a string field of a context. A missing, null or empty value is
// "" with no error; any other type is an error.
func (c ContextFile) field(key string) (string, error) {
	v, ok := c.Fields[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", ctxErr(c.Name, "field %q must be a string", key)
	}
	return s, nil
}

func (c ContextFile) boolField(key string) (bool, error) {
	v, ok := c.Fields[key]
	if !ok || v == nil {
		return false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, ctxErr(c.Name, "field %q must be a boolean", key)
	}
	return b, nil
}

// auth is the authentication a context or source selected: the nats.go option
// and, when a user seed was involved, the signer behind it. The option signs
// through the signer, so wiping the signer disables later (re)connects.
type auth struct {
	opt    nats.Option
	signer identity.Signer
}

// maxKeyFileSize caps what readKeyFile reads for a creds, nkey seed, TLS
// certificate or TLS key file. Each is a few KiB; the cap keeps a huge file or
// /dev/zero from exhausting memory.
const maxKeyFileSize = 64 << 10

// maxCABundleSize caps what readKeyFile reads for the ca field, which can name
// a CA bundle: a system bundle such as /etc/ssl/certs/ca-certificates.crt is
// about 180 KiB.
const maxCABundleSize = 1 << 20

// readKeyFile reads a file the context names. The file must be a regular file
// of at most limit bytes (maxKeyFileSize, 64 KiB, for creds, nkey, cert and key
// files; maxCABundleSize, 1 MiB, for the ca field), checked on the opened
// descriptor: a FIFO, device or directory is refused, and the open itself does
// not block on a FIFO. When
// secret is true (creds, seed and TLS private key files) and relax is false, it
// also refuses a file that group or others can access, on platforms that have
// such modes. The mode is read from the open file, so it cannot change between
// check and read. ctx is checked before the open and before the read; a regular
// capped file reads in bounded time, so the read is not interrupted. The caller
// zeroes the returned bytes.
func readKeyFile(ctx context.Context, name, field, path string, limit int64, secret, relax bool) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, ctxErrWrap(name, err, "reading the %s file was canceled", field)
	}
	p, err := expandHome(path)
	if err != nil {
		return nil, err
	}
	f, err := openKeyFile(p)
	if err != nil {
		return nil, ctxErrWrap(name, err, "cannot read %s file %s", field, p)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, ctxErrWrap(name, err, "cannot read %s file %s", field, p)
	}
	if !fi.Mode().IsRegular() {
		return nil, ctxErr(name, "%s file %s is not a regular file", field, p)
	}
	if secret && !relax && runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			return nil, ctxErr(name, "%s file %s is accessible by group or others (mode %04o): it must be mode 0600 (chmod 600), or pass WithInsecureKeyFilePermissions",
				field, p, perm)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, ctxErrWrap(name, err, "reading the %s file was canceled", field)
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		zero(b)
		return nil, ctxErrWrap(name, err, "cannot read %s file %s", field, p)
	}
	if int64(len(b)) > limit {
		zero(b)
		return nil, ctxErr(name, "%s file %s is larger than the %d byte limit", field, p, limit)
	}
	return b, nil
}

func zero(b []byte) { clear(b) }

func credsAuth(ctx context.Context, name, path string, relax bool) (auth, error) {
	b, err := readKeyFile(ctx, name, "creds", path, maxKeyFileSize, true, relax)
	if err != nil {
		return auth{}, err
	}
	defer zero(b)
	s, err := identity.SignerFromCreds(b)
	if err != nil {
		return auth{}, ctxErrWrap(name, err, "creds file is not usable")
	}
	return auth{opt: nats.UserJWT(func() (string, error) { return s.JWT(), nil }, s.Sign), signer: s}, nil
}

func nkeyFileAuth(ctx context.Context, name, path string, relax bool) (auth, error) {
	b, err := readKeyFile(ctx, name, "nkey", path, maxKeyFileSize, true, relax)
	if err != nil {
		return auth{}, err
	}
	defer zero(b)
	return seedAuth(name, b, "")
}

// seedAuth builds the authentication for a user seed, with a user JWT when
// jwt is not empty.
func seedAuth(name string, seed []byte, jwt string) (auth, error) {
	if jwt == "" {
		s, err := identity.SignerFromSeed(seed)
		if err != nil {
			return auth{}, ctxErrWrap(name, err, "user seed is not usable")
		}
		return auth{opt: nats.Nkey(s.PublicKey(), s.Sign), signer: s}, nil
	}
	s, err := identity.SignerFromSeedAndJWT(seed, jwt)
	if err != nil {
		return auth{}, ctxErrWrap(name, err, "user seed and user_jwt are not usable")
	}
	return auth{opt: nats.UserJWT(func() (string, error) { return s.JWT(), nil }, s.Sign), signer: s}, nil
}

// snapshot is one read of a context: the connection and the auth behind it.
type snapshot struct {
	name   string
	conn   Connection
	signer identity.Signer // nil when the context has no user seed
}

func loadSnapshot(ctx context.Context, selector string, opts []ContextOption) (*snapshot, error) {
	relax := newContextConfig(opts).insecureKeyFiles
	cf, err := ReadContext(selector, opts...)
	if err != nil {
		return nil, err
	}
	name := cf.Name
	rawURL, err := cf.field("url")
	if err != nil {
		return nil, err
	}
	if rawURL == "" {
		return nil, ctxErr(name, "missing `url`")
	}
	ua, err := parseURL(rawURL)
	if err != nil {
		var ce *ContextError
		if errors.As(err, &ce) {
			ce.Context = name
		}
		return nil, err
	}
	out := &snapshot{name: name, conn: Connection{URL: strings.Join(ua.servers, ",")}}

	creds, err := cf.field("creds")
	if err != nil {
		return nil, err
	}
	nkey, err := cf.field("nkey")
	if err != nil {
		return nil, err
	}
	userJWT, err := cf.field("user_jwt")
	if err != nil {
		return nil, err
	}
	userSeed, err := cf.field("user_seed")
	if err != nil {
		return nil, err
	}
	token, err := cf.field("token")
	if err != nil {
		return nil, err
	}
	user, err := cf.field("user")
	if err != nil {
		return nil, err
	}
	password, err := cf.field("password")
	if err != nil {
		return nil, err
	}

	// Precedence: creds > nkey > user_jwt (+user_seed) > a lone user_seed >
	// user/password > token, and any explicit field wins over URL userinfo. A
	// lone user_seed connects as NKEY auth only when no user/password, token or
	// URL userinfo is present; beside one of those the other credential wins and
	// SignerFromContext refuses, so the signer never names a different user than
	// the connection (see loneSeed, shared by both).
	loneSeedValue, loneOther, err := cf.loneSeed()
	if err != nil {
		return nil, err
	}
	var authOpts []nats.Option
	switch {
	case creds != "":
		a, err := credsAuth(ctx, name, creds, relax)
		if err != nil {
			return nil, err
		}
		authOpts, out.signer = []nats.Option{a.opt}, a.signer
	case nkey != "":
		a, err := nkeyFileAuth(ctx, name, nkey, relax)
		if err != nil {
			return nil, err
		}
		authOpts, out.signer = []nats.Option{a.opt}, a.signer
	case userJWT != "":
		if userSeed != "" {
			seed := []byte(userSeed)
			a, err := seedAuth(name, seed, userJWT)
			zero(seed)
			if err != nil {
				return nil, err
			}
			authOpts, out.signer = []nats.Option{a.opt}, a.signer
		} else {
			// No seed: the CONNECT signature stays empty. Works only against
			// a server that does not require nonce signing.
			j := userJWT
			authOpts = []nats.Option{nats.UserJWT(
				func() (string, error) { return j, nil },
				func([]byte) ([]byte, error) { return nil, nil },
			)}
		}
	case loneSeedValue != "" && loneOther == "":
		seed := []byte(loneSeedValue)
		a, err := seedAuth(name, seed, "")
		zero(seed)
		if err != nil {
			return nil, err
		}
		authOpts, out.signer = []nats.Option{a.opt}, a.signer
	case user != "" || password != "":
		authOpts = []nats.Option{nats.UserInfo(user, password)}
	case token != "":
		authOpts = []nats.Option{nats.Token(token)}
	default:
		authOpts = ua.options()
	}
	out.conn.Options = append(out.conn.Options, authOpts...)

	tlsOpts, err := tlsOptions(ctx, cf, relax)
	if err != nil {
		wipeSigner(out.signer)
		return nil, err
	}
	out.conn.Options = append(out.conn.Options, tlsOpts...)

	prefix, err := cf.field("inbox_prefix")
	if err != nil {
		wipeSigner(out.signer)
		return nil, err
	}
	if prefix != "" {
		out.conn.Options = append(out.conn.Options, nats.CustomInboxPrefix(prefix))
	}
	return out, nil
}

func wipeSigner(s identity.Signer) {
	if w, ok := s.(identity.SignerWiper); ok {
		w.Wipe()
	}
}

// tlsOptions builds the nats.go options for cert, key, ca and tls_first. The
// files are read once, here, so a later rotation cannot split one connection's
// material.
func tlsOptions(ctx context.Context, cf ContextFile, relax bool) ([]nats.Option, error) {
	cert, err := cf.field("cert")
	if err != nil {
		return nil, err
	}
	key, err := cf.field("key")
	if err != nil {
		return nil, err
	}
	ca, err := cf.field("ca")
	if err != nil {
		return nil, err
	}
	first, err := cf.boolField("tls_first")
	if err != nil {
		return nil, err
	}
	if cert == "" && key == "" && ca == "" && !first {
		return nil, nil
	}
	if (cert == "") != (key == "") {
		return nil, ctxErr(cf.Name, "`cert` and `key` must be given together")
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cert != "" {
		certPEM, err := readKeyFile(ctx, cf.Name, "cert", cert, maxKeyFileSize, false, relax)
		if err != nil {
			return nil, err
		}
		keyPEM, err := readKeyFile(ctx, cf.Name, "key", key, maxKeyFileSize, true, relax)
		if err != nil {
			return nil, err
		}
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		zero(keyPEM)
		if err != nil {
			// tls errors do not quote key bytes, but keep the text out anyway.
			return nil, ctxErr(cf.Name, "cert and key files are not a valid PEM key pair")
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	if ca != "" {
		caPEM, err := readKeyFile(ctx, cf.Name, "ca", ca, maxCABundleSize, false, relax)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(bytes.TrimSpace(caPEM)) {
			return nil, ctxErr(cf.Name, "ca file holds no PEM certificate")
		}
		cfg.RootCAs = pool
	}
	opts := []nats.Option{nats.Secure(cfg)}
	if first {
		opts = append(opts, nats.TLSHandshakeFirst())
	}
	return opts, nil
}

// LoadContext reads the named context (see [ReadContext]) and returns the
// server URL and the options for it: credentials by the precedence in the
// package documentation, TLS material (read once, now), and the inbox prefix.
// The URL's userinfo is used only when the context has no explicit auth field.
//
// Key files are read once. The returned options keep the decoded key in memory
// for reconnects; use [ResolveBundle] when the program needs to [Bundle.Wipe]
// it. See parity row 1.4; outside SAP (deviation D-1).
func LoadContext(selector string, opts ...ContextOption) (Connection, error) {
	s, err := loadSnapshot(context.Background(), selector, opts)
	if err != nil {
		return Connection{}, err
	}
	return s.conn, nil
}

// SignerFromContext returns the sender-identity signer for the user seed a
// context holds: its creds file, else its nkey file, else its inline user_seed
// (with user_jwt when present; a lone user_seed is also what LoadContext
// connects with, as NKEY auth). It reads the context and that one key source,
// and builds no connection options. It selects the identity [LoadContext]
// connects with: a lone inline user_seed is refused, with an error
// matching [identity.ErrIdentityMismatch], when the context also holds a
// user/password, a token or URL userinfo, since the connection would then
// authenticate as that credential. Key files must pass the permission check
// ([WithInsecureKeyFilePermissions]). A context without a user seed fails with
// an error matching [ErrNoUserSeed] and [ErrContext]. To connect and sign from
// one read, use [ResolveBundle] instead. See SAP §13.6; parity row 10.10;
// outside SAP (deviation D-1).
func SignerFromContext(selector string, opts ...ContextOption) (identity.Signer, error) {
	cf, err := ReadContext(selector, opts...)
	if err != nil {
		return nil, err
	}
	creds, err := cf.field("creds")
	if err != nil {
		return nil, err
	}
	nkey, err := cf.field("nkey")
	if err != nil {
		return nil, err
	}
	userSeed, err := cf.field("user_seed")
	if err != nil {
		return nil, err
	}
	userJWT, err := cf.field("user_jwt")
	if err != nil {
		return nil, err
	}
	relax := newContextConfig(opts).insecureKeyFiles
	// This exported function takes no context, so the key-file read has none to honor.
	ctx := context.Background()
	var a auth
	switch {
	case creds != "":
		a, err = credsAuth(ctx, cf.Name, creds, relax)
	case nkey != "":
		a, err = nkeyFileAuth(ctx, cf.Name, nkey, relax)
	case userSeed != "":
		if userJWT == "" {
			// LoadContext connects with a lone seed as NKEY auth. Beside another
			// credential it would connect as that credential's user, so the
			// signer would name a different identity (SAP §13.4).
			if _, other, err := cf.loneSeed(); err != nil {
				return nil, err
			} else if other != "" {
				return nil, ctxErrWrap(cf.Name, identity.ErrIdentityMismatch,
					"user_seed would sign as a different user than %s, which LoadContext connects with: add user_jwt, or remove the other credential", other)
			}
		}
		seed := []byte(userSeed)
		a, err = seedAuth(cf.Name, seed, userJWT)
		zero(seed)
	default:
		return nil, noSeedError(cf.Name)
	}
	if err != nil {
		return nil, err
	}
	return a.signer, nil
}

// loneSeed is the one decision LoadContext and SignerFromContext share about an
// inline user_seed. seed is the inline user_seed when the context has no creds,
// nkey or user_jwt (else ""). other names the credential (never its value)
// that would win over the seed, "" when the seed is the connection's credential.
func (c ContextFile) loneSeed() (seed, other string, err error) {
	for _, k := range []string{"creds", "nkey", "user_jwt"} {
		v, err := c.field(k)
		if err != nil {
			return "", "", err
		}
		if v != "" {
			return "", "", nil
		}
	}
	seed, err = c.field("user_seed")
	if err != nil || seed == "" {
		return "", "", err
	}
	other, err = c.connectsWithOtherCredential()
	if err != nil {
		return "", "", err
	}
	return seed, other, nil
}

// connectsWithOtherCredential names (without its value) the credential
// LoadContext would authenticate with when no creds, nkey or user_jwt applies:
// user/password, token or URL userinfo, in that order. "" means none.
func (c ContextFile) connectsWithOtherCredential() (string, error) {
	user, err := c.field("user")
	if err != nil {
		return "", err
	}
	password, err := c.field("password")
	if err != nil {
		return "", err
	}
	token, err := c.field("token")
	if err != nil {
		return "", err
	}
	switch {
	case user != "" || password != "":
		return "the user/password field", nil
	case token != "":
		return "the token field", nil
	}
	rawURL, err := c.field("url")
	if err != nil || rawURL == "" {
		return "", nil // LoadContext reports a missing or bad url itself
	}
	if ua, err := parseURL(rawURL); err == nil && (ua.hasUserPass || ua.hasToken) {
		return "the credential in the url", nil
	}
	return "", nil
}

type noSeed struct{ *ContextError }

func (noSeed) Is(target error) bool { return target == ErrNoUserSeed }

// Unwrap lets errors.Is see both ErrNoUserSeed (via Is) and the embedded
// ContextError chain.
func (e noSeed) Unwrap() error { return e.ContextError }

func noSeedError(name string) error {
	msg := "has no creds, nkey or user_seed: nothing to sign with"
	if name == "" {
		msg = "source has no user seed: signed identity needs creds or nkey"
	}
	return noSeed{ctxErr(name, "%s", msg)}
}
