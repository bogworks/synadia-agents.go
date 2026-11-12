// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package connect_test

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/bogworks/synadia-agents.go/connect"
)

func ExampleReadContext() {
	// A NATS CLI context is a JSON file in <config dir>/context/<name>.json.
	dir, err := os.MkdirTemp("", "nats-config")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	if err := os.Mkdir(filepath.Join(dir, "context"), 0o700); err != nil {
		panic(err)
	}
	body := `{"url": "nats://127.0.0.1:4222"}`
	if err := os.WriteFile(filepath.Join(dir, "context", "dev.json"), []byte(body), 0o600); err != nil {
		panic(err)
	}

	cf, err := connect.ReadContext("dev", connect.WithConfigDir(dir))
	if err != nil {
		panic(err)
	}
	fmt.Println(cf.Name, cf.Fields["url"])
	// Output:
	// dev nats://127.0.0.1:4222
}
