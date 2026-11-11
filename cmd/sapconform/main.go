// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Command sapconform runs the SAP conformance catalog against an agent over
// NATS, as a black box. See README.md. It covers host obligations only.
package main

import (
	"os"
)

func main() {
	os.Exit(Main(os.Args[1:], os.Stdout, os.Stderr))
}
