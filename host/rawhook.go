// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"errors"

	"github.com/bogworks/synadia-agents.go/internal/hosttest"
)

// WithRawPromptHandler installs the raw prompt handler, a test-fixture hook. Its
// parameter type lives in an internal package, so only this module's
// fixtures (agentstest and the tests) can call it; it is not part of the
// supported API. The handler replaces the prompt pipeline after sender
// admission: no envelope decode, limit check, interceptor, ack, keep-alive,
// error mapping or terminator is applied, and the handler passed to [New] is
// not called and may be nil. The zero [hosttest.Raw] fails [New].
func WithRawPromptHandler(r hosttest.Raw) Option {
	return func(c *config) error {
		if r.Handler() == nil {
			return errors.Join(ErrConfig, errors.New("nil raw prompt handler"))
		}
		c.raw = r.Handler()
		return nil
	}
}
