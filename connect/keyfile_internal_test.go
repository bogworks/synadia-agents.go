// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package connect

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeKey(t *testing.T, n int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(p, bytes.Repeat([]byte("a"), n), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadKeyFile_FIFORefusedPromptly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs on windows")
	}
	p := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readKeyFile(context.Background(), "c", "creds", p, maxKeyFileSize, true, false)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v, want not a regular file", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readKeyFile blocked on a FIFO")
	}
}

func TestReadKeyFile_SizeCap(t *testing.T) {
	b, err := readKeyFile(context.Background(), "c", "creds", writeKey(t, maxKeyFileSize), maxKeyFileSize, true, false)
	if err != nil || len(b) != maxKeyFileSize {
		t.Fatalf("exactly the cap: len=%d err=%v", len(b), err)
	}
	_, err = readKeyFile(context.Background(), "c", "creds", writeKey(t, maxKeyFileSize+1), maxKeyFileSize, true, false)
	if err == nil || !strings.Contains(err.Error(), "65536 byte limit") {
		t.Fatalf("cap+1: err = %v, want the limit error", err)
	}
}

func TestReadKeyFile_CABundleLimit(t *testing.T) {
	b, err := readKeyFile(context.Background(), "c", "ca", writeKey(t, 200<<10), maxCABundleSize, false, false)
	if err != nil || len(b) != 200<<10 {
		t.Fatalf("200 KiB CA bundle: len=%d err=%v", len(b), err)
	}
	_, err = readKeyFile(context.Background(), "c", "ca", writeKey(t, maxCABundleSize+1), maxCABundleSize, false, false)
	if err == nil || !strings.Contains(err.Error(), "1048576 byte limit") {
		t.Fatalf("CA limit+1: err = %v, want the limit error", err)
	}
}

func TestReadKeyFile_DirectoryRefused(t *testing.T) {
	_, err := readKeyFile(context.Background(), "c", "ca", t.TempDir(), maxCABundleSize, false, false)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v, want not a regular file", err)
	}
}

func TestReadKeyFile_CanceledContextDoesNotOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := readKeyFile(ctx, "c", "creds", filepath.Join(t.TempDir(), "missing"), maxKeyFileSize, true, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled (and not a missing-file error)", err)
	}
}
