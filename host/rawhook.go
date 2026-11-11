// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"errors"

	"github.com/bogworks/synadia-agents.go/internal/hosttest"
)

// The raw prompt handler is a test-fixture hook, reachable only through the
// internal package hosttest (agentstest and this module's tests). It replaces
// the prompt pipeline after sender admission: no envelope decode, limit check,
// interceptor, ack, keep-alive, error mapping or terminator is applied, and the
// handler passed to [New] is not called and may be nil. The assignment runs at
// package variable initialization and has no other effect.
var _ = func() bool {
	hosttest.RawOption = func(f hosttest.RawHandler) any {
		return Option(func(c *config) error {
			if f == nil {
				return errors.Join(ErrConfig, errors.New("nil raw prompt handler"))
			}
			c.raw = f
			return nil
		})
	}
	return true
}()
