// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package runner

// gates maps a needs-configuration entry id to the configuration keys its
// check needs. An entry that is not here is never configuration-missing: its
// check runs whenever the entry's capability lets it run.
//
// The mapping is the runner's side of the catalog's fixed classification: the
// catalog says an entry needs configuration, and this table says which.
var gates = buildGates()

func buildGates() map[string][]string {
	g := map[string][]string{}
	set := func(keys []string, ids ...string) {
		for _, id := range ids {
			g[id] = keys
		}
	}
	trust := []string{KeyTrust}
	set([]string{KeySession}, "SAP-3.2-06", "SAP-3.2-07", "SAP-8.3-03")
	set([]string{KeyInst}, "SAP-3.4-01", "SAP-3.4-02", "SAP-3.4-04")
	set([]string{KeyOversize}, "SAP-5.4-04", "SAP-9.2-02")
	set([]string{KeySlow}, "SAP-6.4-04")
	set([]string{KeyQuery}, "SAP-7-01", "SAP-7.1-01", "SAP-7.1-02", "SAP-7.1-03", "SAP-7.1-04",
		"SAP-7.3-01", "SAP-7.3-02", "SAP-7.3-03")
	// Checks that need the runner to have answered a query also need the
	// operator's permission to publish to the agent's reply subjects.
	set([]string{KeyQuery, KeyAnswer}, "SAP-7.3-04", "SAP-7.3-05")
	// Sender identity (SAP §13): the operator says what the endpoint declares.
	set(trust,
		"SAP-13.1-01", "SAP-13.1-02", "SAP-13.5.1-03", "SAP-13.5.1-04", "SAP-13.5.1-05", "SAP-13.5.1-06",
		"SAP-13.7-01", "SAP-13.7-02", "SAP-13.7-07", "SAP-13.7-08", "SAP-13.7-10",
		"SAP-13.7-11", "SAP-13.7-12", "SAP-13.7.3-02",
		"SAP-13.9-01", "SAP-13.9-02", "SAP-13.9-03", "SAP-13.9-05", "SAP-13.9-06", "SAP-13.9-07",
		"SAP-13.10-01", "SAP-13.10-02", "SAP-13.10-03",
		"SAP-13.11-01", "SAP-13.11-02", "SAP-13.11-03", "SAP-13.11-04", "SAP-13.11-05", "SAP-13.11-06", "SAP-13.11-08",
		"SAP-13.13-02", "SAP-13.13-03")
	// Positive controls and valid-signature probes need a seed the host admits.
	seeded := []string{KeyTrust, KeySeed}
	set(seeded,
		"SAP-13.5-08", "SAP-13.7-03", "SAP-13.7-04", "SAP-13.7-05", "SAP-13.7-06",
		"SAP-13.7.1-01", "SAP-13.7.1-02", "SAP-13.7.1-05")
	set([]string{KeyTrust, KeyRefused}, "SAP-13.7-13", "SAP-13.7.3-01")
	return g
}
