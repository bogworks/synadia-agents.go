// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// Logging a SenderHeader through slog must render the nonce and signature as
// [redacted] and never emit their raw values; the other fields stay visible.
func TestSenderHeader_LogValueRedacts(t *testing.T) {
	const (
		nonce = "rawNonce_0123456789"
		sig   = "rawSigBase64urlValue-xyz"
	)
	h := SenderHeader{
		Account: "ACCT1",
		User:    "USER1",
		Sub:     "agents.sub.one",
		TS:      "2026-11-11T00:00:00Z",
		Nonce:   nonce,
		Sig:     sig,
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("request", "sender", h)
	out := buf.String()

	for _, secret := range []string{nonce, sig} {
		if strings.Contains(out, secret) {
			t.Errorf("log output leaks %q: %s", secret, out)
		}
	}
	for _, want := range []string{"nonce:[redacted]", "sig:[redacted]", "ACCT1", "USER1", "agents.sub.one", "2026-11-11T00:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q: %s", want, out)
		}
	}
}
