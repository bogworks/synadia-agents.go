// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseSize parses the max_payload grammar: a positive integer followed by
// B, KB, MB or GB (SAP §2.1). Units are read case-insensitively and are
// powers of 1024; SAP is silent on both, and the official SDKs make the same
// choice (SDK policy). Whitespace around the number and between number and
// unit is accepted ("2 kb"), as both official SDKs' parsers do
// (client-sdk/typescript/src/bytes.ts: /^\s*(\d+)\s*(B|KB|MB|GB)\s*$/i;
// client-sdk/python/src/synadia_ai/agents/_bytes.py: the same pattern); SAP
// §2.1 gives only the grammar, so this is SDK policy. A zero, negative, overflowing or malformed value is an
// error, and the caller then treats the limit as undeclared.
func ParseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, &SizeError{Input: s, Reason: "expected e.g. 1MB, 512KB, 4GB"}
	}
	unit := strings.ToUpper(strings.TrimSpace(t[i:]))
	var mult int64
	switch unit {
	case "B":
		mult = 1
	case "KB":
		mult = 1 << 10
	case "MB":
		mult = 1 << 20
	case "GB":
		mult = 1 << 30
	default:
		return 0, &SizeError{Input: s, Reason: "expected e.g. 1MB, 512KB, 4GB"}
	}
	n, err := strconv.ParseInt(t[:i], 10, 64)
	if err != nil {
		return 0, &SizeError{Input: s, Reason: "number out of range"}
	}
	if n == 0 {
		return 0, &SizeError{Input: s, Reason: "must be positive"}
	}
	if n > math.MaxInt64/mult {
		return 0, &SizeError{Input: s, Reason: "overflows"}
	}
	return n * mult, nil
}

// FormatSize formats a byte count in the max_payload grammar, using the
// largest unit that divides it evenly (8388608 becomes "8MB"). A count that is
// not positive formats as "0B", which ParseSize refuses. See SAP §2.1.
func FormatSize(n int64) string {
	if n < 0 {
		n = 0
	}
	for _, u := range []struct {
		name string
		mult int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}} {
		if n >= u.mult && n%u.mult == 0 {
			return fmt.Sprintf("%d%s", n/u.mult, u.name)
		}
	}
	return fmt.Sprintf("%dB", n)
}
