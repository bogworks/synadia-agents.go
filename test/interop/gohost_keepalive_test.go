// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package interop_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bogworks/synadia-agents.go/host"
	"github.com/bogworks/synadia-agents.go/test/interop"
)

// TestGoHost_KeepAlive_DefaultTimeouts is an experiment on the callers' own
// defaults (60s inactivity, 600s ceiling; the peers otherwise override both).
// The handler is silent for 70s: without keep-alives the official callers
// should give up on inactivity (the "off" case sets keep-alives off explicitly),
// with the host's default keep-alive (30s) the acks should hold the stream open
// (the "on" case passes no keep-alive option). It is the evidence for the Go
// host's default; see host.WithKeepalive. It runs only with SAP_INTEROP_LONG=1 and not under -short.
func TestGoHost_KeepAlive_DefaultTimeouts(t *testing.T) {
	if testing.Short() {
		t.Skip("long keep-alive experiment skipped in -short mode")
	}
	if os.Getenv("SAP_INTEROP_LONG") != "1" {
		t.Skip("set SAP_INTEROP_LONG=1 to run the 70s keep-alive experiment")
	}
	const silence = 70 * time.Second
	for _, ka := range []bool{false, true} {
		name := "KeepAliveOff"
		if ka {
			name = "KeepAliveOn"
		}
		t.Run(name, func(t *testing.T) {
			eachCaller(t, func(t *testing.T, e *interop.Env, impl interop.Impl) {
				srv := interop.StartServer(t, e)
				var opts []host.Option
				if !ka {
					opts = append(opts, host.WithKeepalive(0))
				}
				startGoHost(t, srv.URL, "", false, hostMode{kind: "slow", slowDelay: silence}, opts...)
				start := time.Now()
				proc := e.StartCaller(t, impl, srv.URL, interop.Peer{
					Prompt: "wait for it",
					Extra:  map[string]string{"SAP_DEFAULT_TIMEOUTS": "1"},
				})
				exit := proc.Wait(t, 3*time.Minute)
				res := &interop.CallerResult{Process: proc, Exit: exit, Lines: proc.Lines()}
				elapsed := time.Since(start).Round(time.Second)
				mustExit0(t, res)
				errLine, errored := res.Find("error")
				t.Logf("RESULT caller=%s keepalive=%v elapsed=%s acks=%d error=%v text=%q", impl, ka, elapsed, len(res.All("status")), errLine, res.Text())
				if ka {
					if errored {
						t.Fatalf("keep-alives on but caller errored after %s: %v", elapsed, errLine)
					}
					if res.Text() != "wait for it" {
						t.Fatalf("text = %q", res.Text())
					}
					return
				}
				if !errored {
					t.Fatalf("keep-alives off: caller did not time out (elapsed %s, lines %v)", elapsed, res.Lines)
				}
				if msg := strings.ToLower(errLine.Str("message")); !strings.Contains(msg, "timeout") && !strings.Contains(msg, "timed out") && !strings.Contains(msg, "stall") && !strings.Contains(msg, "inactiv") {
					t.Fatalf("unexpected error: %v", errLine)
				}
				if elapsed >= silence {
					t.Fatalf("caller gave up only after %s, expected the 60s default inactivity timeout", elapsed)
				}
			})
		})
	}
}
