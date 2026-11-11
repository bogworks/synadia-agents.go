// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package conformance holds the SAP conformance catalog and the checker that
// keeps it honest.
//
// The catalog (catalog.json) lists every normative statement of the pinned SAP
// 0.3 revision as an atomic obligation, with the tests that cover it. The
// checker loads the catalog, validates its structure, and verifies that each
// linked test exists and names the obligation it covers. See README.md for the
// file format and the rules.
package conformance

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

//go:embed catalog.json
var embedded []byte

// Levels an entry may carry. Level is the strength the spec states. WIRE marks
// a normative wire rule that the spec states without an RFC 2119 keyword.
const (
	LevelMust      = "MUST"
	LevelMustNot   = "MUST NOT"
	LevelShould    = "SHOULD"
	LevelShouldNot = "SHOULD NOT"
	LevelMay       = "MAY"
	LevelRecommend = "RECOMMENDED"
	LevelWire      = "WIRE"
)

// Roles an entry may carry: who must behave as the entry says.
const (
	RoleCaller   = "caller"
	RoleHost     = "host"
	RoleBoth     = "both"
	RoleOperator = "operator"
)

// Capabilities an entry may carry. The capability is fixed per entry; a
// runner's result for an entry is a separate thing (pass, fail or
// configuration missing).
const (
	CapBlackBox = "black-box"
	CapNeedsCfg = "needs-configuration"
	CapSDK      = "sdk-internal"
	CapNA       = "not-applicable"
)

// Statuses an entry may carry.
const (
	StatusPlanned = "planned"
	StatusCovered = "covered"
	StatusNA      = "not-applicable"
	// StatusDeferred marks an obligation that no test covers yet and that the
	// catalog says so openly: it needs a deferred_reason and a tracking note.
	// The coverage gate accepts it; it never accepts StatusPlanned.
	StatusDeferred = "deferred"
)

// Spec identifies the pinned revision of the protocol document.
type Spec struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Repo    string `json:"repo"`
	Commit  string `json:"commit"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Lines   int    `json:"lines"`
}

// Entry is one atomic obligation.
type Entry struct {
	ID                  string   `json:"id"`
	Section             string   `json:"section"`
	SpecLine            int      `json:"spec_line"`
	Level               string   `json:"level"`
	Role                string   `json:"role"`
	Summary             string   `json:"summary"`
	Capability          string   `json:"capability"`
	NotApplicableReason string   `json:"not_applicable_reason,omitempty"`
	SameAs              string   `json:"same_as,omitempty"`
	Tests               []string `json:"tests"`
	FaultID             string   `json:"fault_id,omitempty"`
	Mutation            string   `json:"mutation,omitempty"`
	AssertionNote       string   `json:"assertion_note,omitempty"`
	DeferredReason      string   `json:"deferred_reason,omitempty"`
	Tracking            string   `json:"tracking,omitempty"`
	Status              string   `json:"status"`
}

// IsStrict reports whether the entry is a MUST or MUST NOT, the strengths that
// need a fault check.
func (e Entry) IsStrict() bool { return e.Level == LevelMust || e.Level == LevelMustNot }

// Catalog is the parsed catalog file.
type Catalog struct {
	CatalogVersion int     `json:"catalog_version"`
	Spec           Spec    `json:"spec"`
	Entries        []Entry `json:"entries"`
}

// Parse decodes catalog JSON. Unknown fields are an error, so a typo in a field
// name cannot silently drop data.
func Parse(data []byte) (*Catalog, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Catalog
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("conformance: decode catalog: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("conformance: trailing data after catalog")
	}
	return &c, nil
}

// Default returns the catalog embedded in this package.
func Default() (*Catalog, error) { return Parse(embedded) }

// Load reads a catalog from a file.
func Load(path string) (*Catalog, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: Load reads the catalog path the caller supplied
	if err != nil {
		return nil, fmt.Errorf("conformance: %w", err)
	}
	return Parse(data)
}

// ByID returns the entry with the given id.
func (c *Catalog) ByID(id string) (Entry, bool) {
	for _, e := range c.Entries {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// AnyPlanned reports whether any entry still has status "planned".
func (c *Catalog) AnyPlanned() bool {
	for _, e := range c.Entries {
		if e.Status == StatusPlanned {
			return true
		}
	}
	return false
}

// Summary counts the catalog's entries.
type Summary struct {
	Total      int
	ByLevel    map[string]int
	ByRole     map[string]int
	ByCap      map[string]int
	ByStatus   map[string]int
	StrictCaps map[string]int // MUST and MUST NOT entries by capability
}

// Summarize counts entries by level, role, capability and status.
func (c *Catalog) Summarize() Summary {
	s := Summary{
		Total:      len(c.Entries),
		ByLevel:    map[string]int{},
		ByRole:     map[string]int{},
		ByCap:      map[string]int{},
		ByStatus:   map[string]int{},
		StrictCaps: map[string]int{},
	}
	for _, e := range c.Entries {
		s.ByLevel[e.Level]++
		s.ByRole[e.Role]++
		s.ByCap[e.Capability]++
		s.ByStatus[e.Status]++
		if e.IsStrict() {
			s.StrictCaps[e.Capability]++
		}
	}
	return s
}

var (
	idRE      = regexp.MustCompile(`^SAP-([0-9A-Z]+(?:\.[0-9]+)*)-([0-9]{2,})$`)
	sectionRE = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]+)*|[A-Z](?:\.[0-9]+[a-z]?)?)$`)
	hexRE     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	testRE    = regexp.MustCompile(`^(?:[A-Za-z0-9_./-]+:)?Test[A-Za-z0-9_]*$`)
	faultRE   = regexp.MustCompile(`^F-[A-Z0-9-]+$`)
	// idScanRE finds obligation ids in test source.
	idScanRE = regexp.MustCompile(`SAP-[0-9A-Z]+(?:\.[0-9]+)*-[0-9]{2,}`)
)

// MaxSummaryLen caps a summary, which must be a paraphrase and not a quotation.
const MaxSummaryLen = 220

// maxVerbatimRun is the number of consecutive words a summary may share with
// the spec line it cites before it counts as copying.
const maxVerbatimRun = 8

var (
	validLevels = set(LevelMust, LevelMustNot, LevelShould, LevelShouldNot, LevelMay, LevelRecommend, LevelWire)
	validRoles  = set(RoleCaller, RoleHost, RoleBoth, RoleOperator)
	validCaps   = set(CapBlackBox, CapNeedsCfg, CapSDK, CapNA)
	validStatus = set(StatusPlanned, StatusCovered, StatusNA, StatusDeferred)
)

func set(v ...string) map[string]bool {
	m := make(map[string]bool, len(v))
	for _, s := range v {
		m[s] = true
	}
	return m
}

// Validate checks the catalog's structure and returns every problem found. It
// reads nothing outside the catalog: it needs no tests and no spec file.
func Validate(c *Catalog) []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.CatalogVersion != 1 {
		add("catalog_version is %d, want 1", c.CatalogVersion)
	}
	if c.Spec.Name == "" || c.Spec.Version == "" || c.Spec.Repo == "" || c.Spec.Path == "" {
		add("spec: name, version, repo and path are required")
	}
	if len(c.Spec.Commit) != 40 {
		add("spec.commit must be a full 40-character commit id")
	}
	if !hexRE.MatchString(c.Spec.SHA256) {
		add("spec.sha256 must be 64 lower-case hex characters")
	}
	if c.Spec.Lines <= 0 {
		add("spec.lines must be positive")
	}
	if len(c.Entries) == 0 {
		add("catalog has no entries")
	}

	ids := map[string]int{}
	dupKey := map[string]string{}
	for i, e := range c.Entries {
		at := fmt.Sprintf("entry %d (%s)", i, e.ID)
		m := idRE.FindStringSubmatch(e.ID)
		switch {
		case m == nil:
			add("%s: id does not match SAP-<section>-<nn>", at)
		case m[1] != e.Section:
			add("%s: id section %q differs from section %q", at, m[1], e.Section)
		}
		if prev, ok := ids[e.ID]; ok {
			add("%s: duplicate id, first at entry %d", at, prev)
		}
		ids[e.ID] = i
		if !sectionRE.MatchString(e.Section) {
			add("%s: section %q is not a section number or appendix letter", at, e.Section)
		}
		if e.SpecLine < 1 || (c.Spec.Lines > 0 && e.SpecLine > c.Spec.Lines) {
			add("%s: spec_line %d outside 1..%d", at, e.SpecLine, c.Spec.Lines)
		}
		if !validLevels[e.Level] {
			add("%s: level %q is not one of the allowed levels", at, e.Level)
		}
		if !validRoles[e.Role] {
			add("%s: role %q is not caller, host, both or operator", at, e.Role)
		}
		if !validCaps[e.Capability] {
			add("%s: capability %q is not allowed", at, e.Capability)
		}
		if !validStatus[e.Status] {
			add("%s: status %q is not allowed", at, e.Status)
		}
		s := strings.TrimSpace(e.Summary)
		if s == "" {
			add("%s: summary is empty", at)
		}
		if len(e.Summary) > MaxSummaryLen {
			add("%s: summary is %d bytes; a paraphrase is at most %d", at, len(e.Summary), MaxSummaryLen)
		}
		if e.Summary != s {
			add("%s: summary has leading or trailing space", at)
		}

		// Not-applicable is a capability, and it needs its reason.
		if (e.Capability == CapNA) != (e.NotApplicableReason != "") {
			add("%s: not_applicable_reason must be present exactly when capability is %s", at, CapNA)
		}
		if (e.Capability == CapNA) != (e.Status == StatusNA) {
			add("%s: status %s goes with capability %s and with no other", at, StatusNA, CapNA)
		}
		if e.Capability == CapNA && (len(e.Tests) > 0 || e.SameAs != "") {
			add("%s: a not-applicable entry has no tests and no same_as", at)
		}
		if e.Status == StatusCovered && len(e.Tests) == 0 && e.SameAs == "" {
			add("%s: status covered needs tests or same_as", at)
		}
		if (e.Status == StatusDeferred) != (e.DeferredReason != "") || (e.Status == StatusDeferred) != (e.Tracking != "") {
			add("%s: deferred_reason and tracking must be present exactly when status is %s", at, StatusDeferred)
		}
		if (e.FaultID == "") != (e.Mutation == "") {
			add("%s: fault_id and mutation go together", at)
		}
		if e.FaultID != "" && !faultRE.MatchString(e.FaultID) {
			add("%s: fault_id %q must look like F-NAME", at, e.FaultID)
		}
		if e.SameAs != "" && (e.FaultID != "" || e.AssertionNote != "") {
			add("%s: a same_as entry takes its fault check from its target", at)
		}
		seen := map[string]bool{}
		for _, tn := range e.Tests {
			if !testRE.MatchString(tn) {
				add("%s: test name %q is not TestXxx or dir:TestXxx", at, tn)
			}
			if seen[tn] {
				add("%s: test %q listed twice", at, tn)
			}
			seen[tn] = true
		}

		// One obligation, one entry: the same paraphrase at the same line, level
		// and role twice is a duplicate.
		k := fmt.Sprintf("%d|%s|%s|%s", e.SpecLine, e.Level, e.Role, normalize(e.Summary))
		if prev, ok := dupKey[k]; ok {
			add("%s: duplicate obligation of %s", at, prev)
		} else {
			dupKey[k] = e.ID
		}
	}

	for i, e := range c.Entries {
		if e.SameAs == "" {
			continue
		}
		at := fmt.Sprintf("entry %d (%s)", i, e.ID)
		j, ok := ids[e.SameAs]
		switch {
		case !ok:
			add("%s: same_as %q names no entry", at, e.SameAs)
		case e.SameAs == e.ID:
			add("%s: same_as names itself", at)
		case c.Entries[j].SameAs != "":
			add("%s: same_as chains through %s; point at the canonical entry", at, e.SameAs)
		}
	}
	return errs
}

func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

var kwRE = regexp.MustCompile(`\b(MUST NOT|MUST|SHOULD NOT|SHOULD|RECOMMENDED|MAY)\b`)

// ValidateSpec checks the catalog against the spec file it pins. It fails when
// the spec's hash differs, when an RFC 2119 keyword occurrence in the spec has
// no entry at its line, when an entry claims a keyword its line lacks, and when
// a summary copies the spec's words.
//
// A line holding an entry that is both WIRE level and not-applicable (the
// definition of the keywords in section 1.1) covers every keyword on its line.
func ValidateSpec(c *Catalog, spec []byte) []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	sum := sha256.Sum256(spec)
	if got := hex.EncodeToString(sum[:]); got != c.Spec.SHA256 {
		add("spec hash is %s, catalog pins %s: regenerate the catalog", got, c.Spec.SHA256)
	}
	text := strings.TrimSuffix(string(spec), "\n")
	lines := strings.Split(text, "\n")
	if len(lines) != c.Spec.Lines {
		add("spec has %d lines, catalog says %d", len(lines), c.Spec.Lines)
	}

	have := map[string]int{}
	blanket := map[int]bool{}
	for _, e := range c.Entries {
		have[fmt.Sprintf("%d|%s", e.SpecLine, e.Level)]++
		if e.Level == LevelWire && e.Capability == CapNA {
			blanket[e.SpecLine] = true
		}
	}
	for i, l := range lines {
		n := i + 1
		occ := map[string]int{}
		for _, m := range kwRE.FindAllString(l, -1) {
			occ[m]++
		}
		if blanket[n] {
			continue
		}
		kws := make([]string, 0, len(occ))
		for k := range occ {
			kws = append(kws, k)
		}
		sort.Strings(kws)
		for _, k := range kws {
			if got := have[fmt.Sprintf("%d|%s", n, k)]; got < occ[k] {
				add("spec line %d has %d %s, catalog has %d entries for it", n, occ[k], k, got)
			}
		}
	}
	for _, e := range c.Entries {
		if e.SpecLine < 1 || e.SpecLine > len(lines) {
			continue
		}
		l := lines[e.SpecLine-1]
		if e.Level != LevelWire && !strings.Contains(l, e.Level) {
			add("%s: spec line %d does not contain %s", e.ID, e.SpecLine, e.Level)
		}
		if run := sharedRun(e.Summary, l); run >= maxVerbatimRun {
			add("%s: summary shares %d consecutive words with spec line %d; paraphrase it", e.ID, run, e.SpecLine)
		}
	}
	return errs
}

var wordRE = regexp.MustCompile(`[a-z0-9$_.{}-]+`)

// sharedRun returns the longest run of consecutive words common to a and b.
func sharedRun(a, b string) int {
	wa := wordRE.FindAllString(strings.ToLower(a), -1)
	wb := wordRE.FindAllString(strings.ToLower(b), -1)
	best := 0
	for i := range wa {
		for j := range wb {
			k := 0
			for i+k < len(wa) && j+k < len(wb) && wa[i+k] == wb[j+k] {
				k++
			}
			if k > best {
				best = k
			}
		}
	}
	return best
}
