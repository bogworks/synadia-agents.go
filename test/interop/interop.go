// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package interop is the test harness for running this SDK against the official
// Synadia Agents SDKs (TypeScript and Python) over a real nats-server (AT-003).
//
// The official SDKs are fetched and built at a pinned commit by setup.sh into a
// cache directory. The harness starts an embedded nats-server, and spawns small
// peer programs (peers/) that are thin drivers over the official SDKs. A peer is
// either a host (an agent that serves until stopped) or a caller (a one-shot
// that discovers, prompts and exits). Each peer reports what it saw as JSON lines
// on stdout, which the tests read through Line.
//
// A test that needs the peers calls [Require] first. When setup.sh has not been
// run, Require skips the test, so go test ./... stays green on a machine without
// node or python; set SAP_INTEROP_REQUIRED=1 (as CI does) to fail instead, since
// AT-003 treats a skipped scenario as a failure.
//
// Go host with an official caller:
//
//	e := interop.Require(t)
//	srv := interop.StartServer(t, e)
//	// start the Go host on srv.URL, then:
//	res := e.RunCaller(t, interop.TypeScript, srv.URL, interop.Peer{Agent: "interop", Owner: "interop", Name: "main", Prompt: "hello"})
//
// Go caller with an official host:
//
//	host := e.StartHost(t, interop.Python, srv.URL, interop.Peer{Mode: "echo"})
//	// host.Subject is the prompt subject; point the Go caller at srv.URL.
package interop

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

//go:embed PINNED
var pinned string

// Impl names an official SDK implementation.
type Impl string

// The official implementations the peers wrap.
const (
	TypeScript Impl = "ts"
	Python     Impl = "py"
)

// Env is a built interop cache: the official SDKs at the pinned commit.
type Env struct {
	// Cache is the cache root; Root is Cache/<pinned sha>.
	Cache, Root string
	// Src is the upstream checkout inside Root (never inside this repository).
	Src string
	// SHA is the pinned upstream commit.
	SHA string
	// Versions holds versions.txt (node, python, package versions) as key to value;
	// repeated keys (ts_dep, py_pkg) are joined with ", ".
	Versions map[string]string

	node, python, peers string
}

// Pin returns a value from PINNED (for example "UPSTREAM_SHA").
func Pin(key string) string {
	for _, l := range strings.Split(pinned, "\n") {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Require returns the interop environment, or skips the test when setup.sh has
// not been run (or fails it when SAP_INTEROP_REQUIRED=1). The cache is
// $SAP_INTEROP_CACHE, else $HOME/.cache/synadia-agents-interop.
func Require(tb testing.TB) *Env {
	tb.Helper()
	e, err := load()
	if err != nil {
		if os.Getenv("SAP_INTEROP_REQUIRED") == "1" {
			tb.Fatalf("interop environment missing (SAP_INTEROP_REQUIRED=1): %v", err)
		}
		tb.Skipf("interop skipped: %v (run test/interop/setup.sh, then export SAP_INTEROP_CACHE)", err)
	}
	return e
}

func load() (*Env, error) {
	cache := os.Getenv("SAP_INTEROP_CACHE")
	if cache == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, errors.New("no SAP_INTEROP_CACHE and no home directory")
		}
		cache = filepath.Join(home, ".cache", "synadia-agents-interop")
	}
	sha := Pin("UPSTREAM_SHA")
	if sha == "" {
		return nil, errors.New("PINNED has no UPSTREAM_SHA")
	}
	root := filepath.Join(cache, sha)
	if _, err := os.Stat(filepath.Join(root, "ready")); err != nil { //nolint:gosec // G703: root is the harness's own working directory
		return nil, fmt.Errorf("no interop cache at %s", root)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, errors.New("node is not on PATH")
	}
	_, self, _, _ := runtime.Caller(0)
	e := &Env{
		Cache: cache, Root: root, Src: filepath.Join(root, "src"), SHA: sha,
		Versions: map[string]string{},
		node:     node,
		python:   filepath.Join(root, "venv", "bin", "python"),
		peers:    filepath.Join(filepath.Dir(self), "peers"),
	}
	if raw, err := os.ReadFile(filepath.Join(root, "versions.txt")); err == nil { //nolint:gosec // G304: path under the harness's own working directory
		for _, l := range strings.Split(string(raw), "\n") {
			if k, v, ok := strings.Cut(l, "="); ok {
				if old := e.Versions[k]; old != "" {
					v = old + ", " + v
				}
				e.Versions[k] = v
			}
		}
	}
	return e, nil
}

// Server is an embedded nats-server.
type Server struct {
	*server.Server
	// URL is the client URL, for example nats://127.0.0.1:41234.
	URL string
}

// ServerOption configures StartServer.
type ServerOption func(*serverConfig)

type serverConfig struct{ conf string }

// WithFixtureConfig starts the server from one of the upstream identity fixtures
// (test-fixtures/identity/<name>, for example "nkey-noaccounts.conf" or
// "accounts.conf"), so nkey users can authenticate and sign (SAP §13).
func WithFixtureConfig(name string) ServerOption {
	return func(c *serverConfig) { c.conf = name }
}

// StartServer starts an embedded nats-server on a free loopback port and stops it
// when the test ends. Without options it is a plain, unauthenticated server.
func StartServer(tb testing.TB, e *Env, opts ...ServerOption) *Server {
	tb.Helper()
	var cfg serverConfig
	for _, o := range opts {
		o(&cfg)
	}
	so := &server.Options{}
	if cfg.conf != "" {
		var err error
		so, err = server.ProcessConfigFile(filepath.Join(e.Src, "test-fixtures", "identity", cfg.conf))
		if err != nil {
			tb.Fatalf("interop: fixture config %s: %v", cfg.conf, err)
		}
	}
	so.Host, so.Port = "127.0.0.1", -1
	so.NoLog, so.NoSigs = true, true
	srv, err := server.NewServer(so)
	if err != nil {
		tb.Fatalf("interop: nats-server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		srv.Shutdown()
		tb.Fatal("interop: nats-server did not become ready")
	}
	tb.Cleanup(srv.Shutdown)
	return &Server{Server: srv, URL: srv.ClientURL()}
}

// Seed writes the named test user's nkey seed (alice, bob, carol, dave or erin,
// from the upstream keys.json) to a 0600 file in the test's temp directory and
// returns its path, which is what Peer.Seed and the official SDKs take.
// The users are test-only throwaways committed upstream on purpose.
func (e *Env) Seed(tb testing.TB, user string) string {
	tb.Helper()
	seed, _ := e.keys(tb, user)
	path := filepath.Join(tb.TempDir(), user+".nk")
	if err := os.WriteFile(path, []byte(seed+"\n"), 0o600); err != nil {
		tb.Fatal(err)
	}
	return path
}

// PublicKey returns the named test user's public nkey.
func (e *Env) PublicKey(tb testing.TB, user string) string {
	tb.Helper()
	_, pub := e.keys(tb, user)
	return pub
}

func (e *Env) keys(tb testing.TB, user string) (seed, pub string) {
	tb.Helper()
	raw, err := os.ReadFile(filepath.Join(e.Src, "test-fixtures", "identity", "keys.json"))
	if err != nil {
		tb.Fatal(err)
	}
	var doc struct {
		Users map[string]struct{ Public, Seed string }
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		tb.Fatal(err)
	}
	u, ok := doc.Users[user]
	if !ok {
		tb.Fatalf("interop: no test user %q in keys.json", user)
	}
	return u.Seed, u.Public
}

// Attachment is a prompt attachment for a caller peer.
type Attachment struct {
	Filename string
	Data     []byte
}

// Peer configures a peer program. A zero field means "the peer's default".
type Peer struct {
	// Agent, Owner and Name are the subject tokens a host serves under
	// (defaults interop, interop, main) or the discovery filter of a caller
	// (empty means match any).
	Agent, Owner, Name string

	// Host: Mode is one of echo, query, error, attachments, slow, signed.
	Mode string
	// Host: Chunks is how many response chunks echo splits the prompt into.
	Chunks int
	// Host: ErrorCode and ErrorDesc are what error mode rejects with.
	ErrorCode int
	ErrorDesc string
	// Host: SlowDelay is the slow-mode handler delay; KeepAlive its ack cadence.
	SlowDelay, KeepAlive time.Duration
	// Host: Heartbeat is the heartbeat interval.
	Heartbeat time.Duration
	// Host: MinSenderTrust is "any" or "signed" (signed mode defaults to signed).
	MinSenderTrust string

	// Caller: Prompt is the prompt text; empty means discover only (plus
	// WatchHeartbeat and Status, when set).
	Prompt string
	// Caller: Attachments are sent with the prompt.
	Attachments []Attachment
	// Caller: QueryAnswer answers any mid-stream query (default "yes").
	QueryAnswer string
	// Caller: Status also calls the status endpoint.
	Status bool
	// Caller: WatchHeartbeat observes heartbeats this long before prompting.
	WatchHeartbeat time.Duration
	// Caller: Timeout is the inactivity timeout and ceiling (default 10s).
	Timeout time.Duration

	// Seed is a seed file (see Env.Seed): the peer authenticates as that nkey user.
	Seed string
	// Sign makes the peer sign sender identity (SAP §13); it needs Seed.
	Sign bool

	// Extra overrides or adds raw environment variables.
	Extra map[string]string
}

func (p Peer) env(e *Env, url string) []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SAP_") {
			out = append(out, kv)
		}
	}
	set := func(k, v string) {
		if v != "" {
			out = append(out, k+"="+v)
		}
	}
	secs := func(d time.Duration) string {
		if d == 0 {
			return ""
		}
		return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
	}
	ms := func(d time.Duration) string {
		if d == 0 {
			return ""
		}
		return strconv.FormatInt(d.Milliseconds(), 10)
	}
	set("SAP_INTEROP_SRC", e.Src)
	set("SAP_NATS_URL", url)
	set("SAP_AGENT", p.Agent)
	set("SAP_OWNER", p.Owner)
	set("SAP_NAME", p.Name)
	set("SAP_MODE", p.Mode)
	if p.Chunks > 0 {
		set("SAP_CHUNKS", strconv.Itoa(p.Chunks))
	}
	if p.ErrorCode > 0 {
		set("SAP_ERROR_CODE", strconv.Itoa(p.ErrorCode))
	}
	set("SAP_ERROR_DESC", p.ErrorDesc)
	set("SAP_SLOW_MS", ms(p.SlowDelay))
	set("SAP_KEEPALIVE_S", secs(p.KeepAlive))
	set("SAP_HEARTBEAT_S", secs(p.Heartbeat))
	set("SAP_MIN_SENDER_TRUST", p.MinSenderTrust)
	set("SAP_PROMPT", p.Prompt)
	if len(p.Attachments) > 0 {
		type att struct {
			Filename string `json:"filename"`
			B64      string `json:"content_b64"`
		}
		var list []att
		for _, a := range p.Attachments {
			list = append(list, att{a.Filename, base64.StdEncoding.EncodeToString(a.Data)})
		}
		raw, _ := json.Marshal(list)
		set("SAP_ATTACHMENTS_JSON", string(raw))
	}
	set("SAP_QUERY_ANSWER", p.QueryAnswer)
	if p.Status {
		set("SAP_STATUS", "1")
	}
	set("SAP_WATCH_HEARTBEAT_S", secs(p.WatchHeartbeat))
	set("SAP_TIMEOUT_S", secs(p.Timeout))
	set("SAP_SEED_FILE", p.Seed)
	if p.Sign {
		set("SAP_SIGN", "1")
	}
	for k, v := range p.Extra {
		out = append(out, k+"="+v)
	}
	return out
}

// Line is one JSON line a peer printed. Peers print a "type" on every line.
type Line map[string]any

// Type is the line's "type".
func (l Line) Type() string { s, _ := l["type"].(string); return s }

// Str returns a string field, or "".
func (l Line) Str(key string) string { s, _ := l[key].(string); return s }

// Int returns a numeric field, or 0 (JSON numbers decode as float64).
func (l Line) Int(key string) int { f, _ := l[key].(float64); return int(f) }

// Bool returns a boolean field, or false.
func (l Line) Bool(key string) bool { b, _ := l[key].(bool); return b }

// List returns an array field as lines (objects only).
func (l Line) List(key string) []Line {
	var out []Line
	arr, _ := l[key].([]any)
	for _, v := range arr {
		if m, ok := v.(map[string]any); ok {
			out = append(out, Line(m))
		}
	}
	return out
}

// Process is a running peer.
type Process struct {
	// Subject is the prompt subject a host reported in its ready line.
	Subject string

	name string
	cmd  *exec.Cmd

	mu     sync.Mutex
	lines  []Line
	raw    []string
	notify chan struct{}
	done   chan struct{}
	exit   int
	stderr bytes.Buffer
}

func (e *Env) spawn(tb testing.TB, role string, impl Impl, p Peer, url string) *Process {
	tb.Helper()
	var name string
	var args []string
	switch impl {
	case TypeScript:
		name, args = e.node, []string{filepath.Join(e.peers, "ts", role+".mjs")}
	case Python:
		name, args = e.python, []string{filepath.Join(e.peers, "py", role+".py")}
	default:
		tb.Fatalf("interop: unknown implementation %q", impl)
	}
	cmd := exec.Command(name, args...) //nolint:gosec // G204: the harness runs the host binary it was told to run
	cmd.Env = append(p.env(e, url), "PYTHONDONTWRITEBYTECODE=1", "PYTHONUNBUFFERED=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	proc := &Process{name: fmt.Sprintf("%s-%s", role, impl), cmd: cmd, notify: make(chan struct{}), done: make(chan struct{})}
	cmd.Stderr = &lockedWriter{p: proc}
	out, err := cmd.StdoutPipe()
	if err != nil {
		tb.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		tb.Fatalf("interop: start %s: %v", proc.name, err)
	}
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var l Line
			if json.Unmarshal(sc.Bytes(), &l) != nil {
				l = Line{"type": "unparsed", "raw": sc.Text()}
			}
			proc.mu.Lock()
			proc.lines = append(proc.lines, l)
			proc.raw = append(proc.raw, sc.Text())
			close(proc.notify)
			proc.notify = make(chan struct{})
			proc.mu.Unlock()
		}
		err := cmd.Wait()
		proc.mu.Lock()
		if err != nil {
			proc.exit = -1
			if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
				proc.exit = ee.ExitCode()
			}
		}
		close(proc.done)
		close(proc.notify)
		proc.notify = make(chan struct{})
		proc.mu.Unlock()
	}()
	tb.Cleanup(func() { proc.Stop(); proc.record(tb) })
	return proc
}

type lockedWriter struct{ p *Process }

func (w *lockedWriter) Write(b []byte) (int, error) {
	w.p.mu.Lock()
	defer w.p.mu.Unlock()
	return w.p.stderr.Write(b)
}

// Lines returns a snapshot of every line printed so far.
func (p *Process) Lines() []Line {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Line(nil), p.lines...)
}

// Stderr returns what the peer wrote to stderr so far (diagnostics only).
func (p *Process) Stderr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stderr.String()
}

// WaitFor returns the first line (from the start of the output) for which match
// is true, waiting up to timeout; it fails the test on timeout or if the peer
// exits first.
func (p *Process) WaitFor(tb testing.TB, timeout time.Duration, match func(Line) bool) Line {
	tb.Helper()
	deadline := time.After(timeout)
	for {
		p.mu.Lock()
		for _, l := range p.lines {
			if match(l) {
				p.mu.Unlock()
				return l
			}
		}
		notify := p.notify
		p.mu.Unlock()
		select {
		case <-notify:
		case <-p.done:
			// One more pass: lines may have landed before exit.
			for _, l := range p.Lines() {
				if match(l) {
					return l
				}
			}
			tb.Fatalf("interop: %s exited before the awaited line; stderr:\n%s", p.name, p.Stderr())
		case <-deadline:
			tb.Fatalf("interop: %s: no matching line within %s; got %d lines; stderr:\n%s", p.name, timeout, len(p.Lines()), p.Stderr())
		}
	}
}

// WaitForType is WaitFor with a match on the line's type.
func (p *Process) WaitForType(tb testing.TB, timeout time.Duration, typ string) Line {
	tb.Helper()
	return p.WaitFor(tb, timeout, func(l Line) bool { return l.Type() == typ })
}

// Wait waits for the peer to exit and returns its exit code; it fails the test
// on timeout.
func (p *Process) Wait(tb testing.TB, timeout time.Duration) int {
	tb.Helper()
	select {
	case <-p.done:
	case <-time.After(timeout):
		tb.Fatalf("interop: %s did not exit within %s; stderr:\n%s", p.name, timeout, p.Stderr())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exit
}

// Stop ends the peer: SIGTERM, then SIGKILL after three seconds. It is safe to
// call more than once; the harness also calls it when the test ends.
func (p *Process) Stop() {
	select {
	case <-p.done:
		return
	default:
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	}
}

// record writes the peer's lines to $SAP_INTEROP_EVIDENCE/<test>/<peer>.jsonl
// when that variable names a directory (the transcript AT-003 asks a run to keep).
func (p *Process) record(tb testing.TB) {
	tb.Helper()
	dir := os.Getenv("SAP_INTEROP_EVIDENCE")
	if dir == "" {
		return
	}
	dir = filepath.Join(dir, strings.ReplaceAll(tb.Name(), "/", "_"))
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: test fixture directory, not secret
		tb.Logf("interop: evidence: %v", err)
		return
	}
	p.mu.Lock()
	body := strings.Join(p.raw, "\n") + "\n"
	p.mu.Unlock()
	if err := os.WriteFile(filepath.Join(dir, p.name+".jsonl"), []byte(body), 0o644); err != nil { //nolint:gosec // G306: test fixture file, not secret
		tb.Logf("interop: evidence: %v", err)
	}
}

// StartHost starts an official host (TypeScript AgentService or Python
// AgentService) against url and waits (30s) for its ready line. The host serves
// until the test ends or Stop is called. Process.Subject is its prompt subject.
func (e *Env) StartHost(tb testing.TB, impl Impl, url string, p Peer) *Process {
	tb.Helper()
	proc := e.spawn(tb, "host", impl, p, url)
	ready := proc.WaitForType(tb, 30*time.Second, "ready")
	proc.Subject = ready.Str("subject")
	return proc
}

// StartCaller starts an official caller without waiting for it; use Wait and
// WaitFor to follow it.
func (e *Env) StartCaller(tb testing.TB, impl Impl, url string, p Peer) *Process {
	tb.Helper()
	return e.spawn(tb, "caller", impl, p, url)
}

// CallerResult is what a finished caller peer printed.
type CallerResult struct {
	*Process
	// Exit is the peer's exit code: 0 stream ended (done or error), 2 no agent matched,
	// anything else a harness failure.
	Exit  int
	Lines []Line
}

// RunCaller runs an official caller to completion (it fails the test after 60s)
// and returns its lines.
func (e *Env) RunCaller(tb testing.TB, impl Impl, url string, p Peer) *CallerResult {
	tb.Helper()
	proc := e.StartCaller(tb, impl, url, p)
	exit := proc.Wait(tb, 60*time.Second)
	return &CallerResult{Process: proc, Exit: exit, Lines: proc.Lines()}
}

// Text is the concatenation of every response chunk's text.
func (r *CallerResult) Text() string {
	var b strings.Builder
	for _, l := range r.Lines {
		if l.Type() == "response" {
			b.WriteString(l.Str("text"))
		}
	}
	return b.String()
}

// Find returns the first line of the given type, and whether there was one.
func (r *CallerResult) Find(typ string) (Line, bool) {
	for _, l := range r.Lines {
		if l.Type() == typ {
			return l, true
		}
	}
	return nil, false
}

// All returns every line of the given type.
func (r *CallerResult) All(typ string) []Line {
	var out []Line
	for _, l := range r.Lines {
		if l.Type() == typ {
			out = append(out, l)
		}
	}
	return out
}
