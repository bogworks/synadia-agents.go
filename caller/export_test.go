// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller

import "sync"

// resetStanceWarning re-arms the once-per-process unchosen-stance warning.
func resetStanceWarning() { warnStanceOnce = sync.Once{} }
