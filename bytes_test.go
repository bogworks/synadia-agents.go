// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import "testing"

// SAP:L91 (§2.1): "a positive integer followed by B, KB, MB, or GB".
func TestSize_Parse(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1MB", 1 << 20, true}, {"512KB", 512 << 10, true}, {"4GB", 4 << 30, true}, {"100B", 100, true},
		{"1mb", 1 << 20, true}, {" 2 kb ", 2 << 10, true}, {"2\tMB", 2 << 20, true},
		{"0B", 0, false}, {"-1B", 0, false}, {"1", 0, false}, {"MB", 0, false}, {"1.5MB", 0, false},
		{"1TB", 0, false}, {"", 0, false}, {"9223372036854775807GB", 0, false}, {"99999999999999999999B", 0, false},
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("ParseSize(%q) = %d, %v", c.in, got, err)
		}
	}
}

func TestSize_FormatRoundTrip(t *testing.T) {
	for in, want := range map[int64]string{8 << 20: "8MB", 1 << 30: "1GB", 1536: "1536B", 2048: "2KB", 100: "100B", 3 << 30: "3GB", -5: "0B"} {
		if got := FormatSize(in); got != want {
			t.Errorf("FormatSize(%d) = %q want %q", in, got, want)
		}
		if in > 0 {
			if back, err := ParseSize(FormatSize(in)); err != nil || back != in {
				t.Errorf("round trip %d: %d %v", in, back, err)
			}
		}
	}
}
