// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package conformance

import _ "embed"

//go:embed catalog.json
var catalogJSON []byte

// CatalogJSON returns a copy of the catalog.json this build embeds. A runner
// that must report which catalog it ran against reads the same bytes the
// checker validates, so the runner's checks are the catalog's checks and not a
// second list (REQ-013).
func CatalogJSON() []byte { return append([]byte(nil), catalogJSON...) }
