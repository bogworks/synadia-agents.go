// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package connect

import (
	"os"
	"syscall"
)

// openKeyFile opens path for reading without blocking on a FIFO (a plain open
// of a FIFO waits for a writer) and without becoming a controlling terminal.
// The caller checks the opened descriptor is a regular file.
func openKeyFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0) //nolint:gosec // G304: a credential path named by the user's own NATS context file
}
