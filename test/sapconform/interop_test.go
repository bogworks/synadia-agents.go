// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package sapconform_test

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/bogworks/synadia-agents.go/conformance/runner"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/bogworks/synadia-agents.go/test/interop"
)

// The official hosts are the reference the runner is judged against: a check
// that an official host fails is either a runner bug or a finding about the
// official SDK. Each such entry is listed below with the reason, so a new
// failure is a test failure and a listed one stays visible.

// knownFindings maps an implementation to the entries it is known to fail.
var knownFindings = map[interop.Impl]map[string]string{
	interop.TypeScript: {},
	interop.Python: {
		// The peer's handler decodes attachment content with the official Python
		// SDK's own helper, and the SDK does not validate base64 at the envelope
		// boundary, so a handler that decodes it fails with a generic 500.
		"SAP-9.2-01": "invalid base64 in an attachment is answered 500, not 400 (official Python SDK 400 rule is left to the handler)",
	},
}

func officialConfig(nc *nats.Conn) runner.Config {
	return runner.Config{
		Conn:                nc,
		DiscoverWait:        time.Second,
		HeartbeatWindow:     4 * time.Second,
		StreamTimeout:       10 * time.Second,
		PostTerminatorGrace: 400 * time.Millisecond,
		AckDeadline:         time.Second,
	}
}

func summarize(t *testing.T, label string, res results) {
	t.Helper()
	var fails []string
	for _, id := range res.ids(runner.Fail) {
		fails = append(fails, fmt.Sprintf("%s(%s)", id, res[id].Level))
	}
	t.Logf("%s: pass %d, fail %d, policy %d, not-applicable %d, configuration-missing %d, not-run %d",
		label, len(res.ids(runner.Pass)), len(res.ids(runner.Fail)), len(res.ids(runner.Policy)),
		len(res.ids(runner.NotApplicable)), len(res.ids(runner.ConfigMissing)), len(res.ids(runner.NotRun)))
	t.Logf("%s: FAIL %v", label, fails)
	t.Logf("%s: policy %v", label, res.ids(runner.Policy))
	for _, id := range append(res.ids(runner.Fail), res.ids(runner.Policy)...) {
		t.Logf("%s: %s [%s] %s: %s", label, id, res[id].Level, res[id].Result, res[id].Detail)
	}
	if os.Getenv("SAPCONFORM_LOG_ALL") != "" {
		var all []string
		for id, e := range res {
			if e.Role == "host" || e.Role == "both" {
				all = append(all, fmt.Sprintf("%s %s %s", id, e.Capability, e.Result))
			}
		}
		sort.Strings(all)
		t.Logf("%s:\n%s", label, strings.Join(all, "\n"))
	}
}

func judge(t *testing.T, impl interop.Impl, res results) {
	t.Helper()
	known := knownFindings[impl]
	for _, id := range res.ids(runner.Fail) {
		e := res[id]
		if why, ok := known[id]; ok {
			t.Logf("known finding for %s: %s: %s", impl, id, why)
			continue
		}
		if e.Level == "MUST" || e.Level == "MUST NOT" {
			t.Errorf("%s: official %s host fails %s (%s): %s", id, impl, id, e.Summary, e.Detail)
		}
	}
}

// TestOfficialHosts_Echo runs sapconform against the official TypeScript and
// Python hosts in echo mode (AT-006).
func TestOfficialHosts_Echo(t *testing.T) {
	t.Parallel()
	e := interop.Require(t)
	for _, impl := range []interop.Impl{interop.TypeScript, interop.Python} {
		t.Run(string(impl), func(t *testing.T) {
			t.Parallel()
			srv := interop.StartServer(t, e)
			e.StartHost(t, impl, srv.URL, interop.Peer{Mode: "echo", Heartbeat: time.Second})
			nc, err := nats.Connect(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(nc.Close)
			cfg := officialConfig(nc)
			// The official hosts declare min_sender_trust even in echo mode.
			cfg.ExpectMinSenderTrust = "any"
			f := false
			cfg.SessionAware = &f
			_, res := runOnce(t, cfg)
			summarize(t, string(impl)+" echo", res)
			judge(t, impl, res)
		})
	}
}

// TestOfficialHosts_Signed runs sapconform with the section 13 configuration
// against the official hosts in signed mode, behind a server that authenticates
// by NKEY.
func TestOfficialHosts_Signed(t *testing.T) {
	t.Parallel()
	e := interop.Require(t)
	for _, impl := range []interop.Impl{interop.TypeScript, interop.Python} {
		t.Run(string(impl), func(t *testing.T) {
			t.Parallel()
			srv := interop.StartServer(t, e, interop.WithFixtureConfig("nkey-noaccounts.conf"))
			hostSeed := e.Seed(t, "alice")
			e.StartHost(t, impl, srv.URL, interop.Peer{Mode: "signed", Seed: hostSeed, Sign: true, Heartbeat: time.Second})

			// The fixture server admits one user, so the runner connects, and signs,
			// as the host's own user.
			raw, err := os.ReadFile(hostSeed) //nolint:gosec // the test reads a fixture or temp path it built itself
			if err != nil {
				t.Fatal(err)
			}
			kp, err := nkeys.FromSeed([]byte(strings.TrimSpace(string(raw))))
			if err != nil {
				t.Fatal(err)
			}
			pub, _ := kp.PublicKey()
			nc, err := nats.Connect(srv.URL, nats.Nkey(pub, func(nonce []byte) ([]byte, error) { return kp.Sign(nonce) }))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(nc.Close)
			signer, err := identity.SignerFromSeed([]byte(strings.TrimSpace(string(raw))))
			if err != nil {
				t.Fatal(err)
			}
			cfg := officialConfig(nc)
			cfg.ExpectMinSenderTrust = "signed"
			cfg.SenderSigner = signer
			// The official hosts admit every verified sender, so there is no refused
			// identity to supply: RefusedSigner stays unset and the 403 entries are
			// configuration-missing.
			f := false
			cfg.SessionAware = &f
			_, res := runOnce(t, cfg)
			summarize(t, string(impl)+" signed", res)
			judge(t, impl, res)
		})
	}
}
