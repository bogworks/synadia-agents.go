// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package connect

import "os"

// openKeyFile opens path for reading. The caller checks the opened descriptor
// is a regular file.
func openKeyFile(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: a credential path named by the user's own NATS context file
}
