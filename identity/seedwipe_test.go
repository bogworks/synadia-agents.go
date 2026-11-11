// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"bytes"
	"strings"
	"testing"
)

func decoratedSeed(seed string) []byte {
	return []byte("-----BEGIN USER NKEY SEED-----\n" + seed + "\n------END USER NKEY SEED------\n")
}

// TestWipeBytes_Zeroes checks the helper every temporary seed buffer goes
// through. Go cannot promise that no other copy exists (the runtime may have
// moved or copied a buffer, and strings are immutable), so this proves only
// that the buffers this package owns are cleared.
func TestWipeBytes_Zeroes(t *testing.T) {
	b := []byte(aliceSeed)
	wipeBytes(b)
	if !bytes.Equal(b, make([]byte, len(aliceSeed))) {
		t.Fatalf("buffer not zeroed: %q", b)
	}
}

func TestSeedFromDecorated_ReturnsPrivateCopy(t *testing.T) {
	in := decoratedSeed(aliceSeed)
	orig := bytes.Clone(in)
	got, err := seedFromDecorated(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != aliceSeed {
		t.Fatalf("seed = %q", got)
	}
	if !bytes.Equal(in, orig) {
		t.Error("the caller's buffer was modified")
	}
	// The copy is the caller's to wipe, and wiping it leaves the input intact.
	wipeBytes(got)
	if !bytes.Equal(in, orig) || strings.Contains(string(got), "SU") {
		t.Error("copy aliases the input or was not wiped")
	}
}

func TestSignerFromSeed_DecoratedAndCredsStillWork(t *testing.T) {
	s, err := SignerFromSeed(decoratedSeed(aliceSeed))
	if err != nil || s.PublicKey() != alicePub {
		t.Fatalf("decorated: %v", err)
	}
	for _, bad := range [][]byte{
		[]byte("-----BEGIN USER NKEY SEED-----\nSUNOTASEED\n------END USER NKEY SEED------\n"),
		[]byte("-----BEGIN USER NKEY SEED-----\n------END USER NKEY SEED------\n"),
	} {
		if _, err := SignerFromSeed(bad); err == nil || strings.Contains(err.Error(), "NOTASEED") {
			t.Errorf("bad decorated seed: err = %v", err)
		}
	}
}
