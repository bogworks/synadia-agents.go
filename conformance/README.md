# SAP conformance catalog

`catalog.json` lists every normative statement of the Synadia Agent Protocol
(SAP) 0.3 as an atomic obligation, and ties each one to the Go tests that cover
it. The `conformance` package checks the catalog and the links.

The pinned spec is `core-protocol.md` at commit
`c48cea5f07965cc8fd4054e120459f1833b3ed2b` of
`github.com/synadia-ai/synadia-agent-sdk-docs`. The catalog records its SHA-256
and line count. The spec repository states no licence, so the catalog cites it
by line and paraphrases; it never copies the spec's text.

## An entry

| Field | Meaning |
|---|---|
| `id` | Stable id, `SAP-<section>-<nn>`, for example `SAP-6.4-02`. Never reused. |
| `section` | The spec section (`6.4`, `13.7.1`) or an appendix letter. |
| `spec_line` | Line number in `core-protocol.md` at the pinned commit. |
| `level` | `MUST`, `MUST NOT`, `SHOULD`, `SHOULD NOT`, `MAY`, `RECOMMENDED`, or `WIRE`. `WIRE` is a rule the spec states without an RFC 2119 keyword (a subject format, a header name, a field shape). |
| `role` | `caller`, `host`, `both`, or `operator` (deployment practice). |
| `summary` | A paraphrase of one testable behaviour. |
| `capability` | Fixed per entry; see below. |
| `not_applicable_reason` | Required when, and only when, the capability is `not-applicable`. |
| `same_as` | Optional. The spec restates this obligation elsewhere (the section 12 checklist, say); the entry takes its tests and fault check from the canonical entry named here. |
| `tests` | Go test names that cover it: `TestXxx`, or `dir:TestXxx` to name a directory. |
| `fault_id`, `mutation` | A targeted fault check: a behavioural mutation of the SDK that breaks exactly this obligation, and its id. Both or neither. |
| `assertion_note` | A reviewed note naming the assertion that checks the obligation, for the rare case where no fault can be injected. |
| `deferred_reason`, `tracking` | Required when, and only when, the status is `deferred`: why no test covers the obligation yet, and where the missing test is tracked. |
| `status` | `covered`, `not-applicable`, or `deferred`. `planned` is a working state: the gate fails on it. |

A sentence that holds two obligations is two entries. An entry that restates
another points at it with `same_as` rather than being dropped, so every keyword
occurrence in the spec still has an entry at its line.

`catalog.schema.json` is a JSON Schema for the file. The Go checker is the
authority: it checks more than the schema can (ids unique, lines in range,
duplicates, links to tests).

## Capability and result are different things

`capability` says what kind of check an obligation allows. It is a property of
the obligation and does not change from run to run:

- `black-box`: an external runner observes it over NATS (a message published, a
  reply, a header, a registration record, a heartbeat or its cadence).
- `needs-configuration`: observable only with a fixture or configuration the
  operator supplies, such as an identity seed, an account import, a
  session-aware harness, or a host that issues queries.
- `sdk-internal`: observable only inside the SDK (a caller's local decision that
  puts nothing on the wire, a parser, a timeout).
- `not-applicable`: with a reason (operator-only, a deferred feature,
  informative text).

A *result* belongs to one run of a runner: `pass`, `fail`, or
`configuration-missing`. The catalog never stores results. The `sapconform`
runner covers host obligations only; caller obligations are classified by what
they put on the wire, but the runner has no driver for a caller under test.

The runner never silently discards a responder. A reply on a discovery or probe
subject that is malformed, off-spec, wrong-service, duplicated or unparseable
becomes a reported target, a finding on the check it breaks (SAP-3.1-01,
SAP-4-01, SAP-4.1-01, SAP-8.7-07), or, when it cannot be attributed to an
instance, a note that makes the run incomplete and fails the default and strict
verdicts. With an agent selected, other agents' unsolicited records are out of
scope; a discover-all run checks every responder.

A check's result only gets worse, never better: fail, then configuration-missing,
then not-run, not-applicable, policy and pass, worst first. Everything recorded
for one entry (what the check observed, a missing seed, a resource cap, each
instance's outcome) merges through one function, so a forbidden signed status
reply the runner saw stays a fail when a later probe needs a seed it lacks. The
winner keeps its detail and a worse-or-equal gap's detail is appended; a tie
keeps the first recorded. A source-scan test rejects a direct write of a result.

## How entries link to tests

The link is checked both ways.

1. The entry lists the test in `tests`.
2. The test's doc comment or body names the obligation id, for example:

   ```go
   // TestAck_FirstMessage asserts SAP-6.4-02 and SAP-6.4-03.
   func TestAck_FirstMessage(t *testing.T) { ... }
   ```

A listed test that does not exist, or does not name the id, fails the check. So
does a test that names an id the catalog lacks, names an id for which the
catalog does not list that test, or names a not-applicable id. A test that
names more than 8 obligations is flagged for review, because one broad test
usually proves indexing and not testing.

A linked test must contain an assertion: `t.Error`, `t.Errorf`, `t.Fatal`,
`t.Fatalf`, `t.Fail`, `t.FailNow`, a `require` or `assert` call, or a call to a
helper in the same directory that does (followed to any depth, by name). A test
that only names an id, logs or skips asserts nothing and fails the check. Tests
other packages own carry the link as a `// catalog: SAP-x.y-nn` comment line in
their doc comment.

Every `MUST` and `MUST NOT` needs a targeted fault check (`fault_id` plus
`mutation`) or a reviewed `assertion_note`. An assertion note names the
asserting test and its file, as `file_test.go:TestName asserts <what>.`, and the
checker requires that form for one of the entry's linked tests. The run of each fault check at each
release is recorded with the release, not in this file. The fault check is
real: see "Mutation testing" below.

## The checker

`go test ./conformance` runs:

- `TestCatalog_WellFormed`: always enforced. Ids, sections, lines, levels,
  roles, capabilities, reasons, duplicates, `same_as`, test-name syntax. It also
  checks the catalog against the pinned `core-protocol.md`: the spec's hash,
  that every RFC 2119 keyword occurrence in the spec has an entry at its line,
  that each entry's line holds its keyword, and that no summary shares eight
  words in a row with the line it cites. The spec is read from
  `conformance/testdata/spec/core-protocol.md`, or from `SAP_SPEC_PATH` if set.
  The upstream docs repository has no license that permits redistribution, so
  the file is not committed; the CI catalog job fetches it at the catalog's
  pinned commit and verifies `spec.sha256` from `catalog.json`. A missing spec
  skips this part with a message, unless `SAP_REQUIRE_SPEC=true` (the catalog
  job sets it), which makes it fail.
- `TestCatalog_Complete`: runs in every ordinary test run, with no switch. It
  fails on any `planned` entry, and enforces coverage: tests that exist, name
  their ids and contain an assertion, a fault check or an assertion note for
  every `MUST` and `MUST NOT`, and the reverse link. An obligation nothing covers
  is `deferred` with a reason and a tracking note, never silently planned.

The package has no dependencies beyond the standard library. Tests are found by
parsing every `*_test.go` file under the repository root with `go/parser`,
across modules, so tests in `test/` count.

To move to a later spec revision, regenerate the extraction and review the
catalog diff; a changed spec hash fails the check until you do.

## Mutation testing

A `fault_id` is not a label. `conformance/mutations/<fault_id>.patch` is a
unified diff against the repository that introduces the fault the entry's
`mutation` text describes (omit the ack, accept a replayed nonce). Each patch is
minimal and edits SDK source only, never a test.

```sh
go run ./internal/cmd/mutate [-only id,id] [-j N] [-timeout 2m]
```

For each fault, the command copies the repository (both modules) to a temporary
directory, applies the patch to the copy with `git apply`, runs only the tests
the catalog links to the fault (`go test -json -count=1 -run '^(TestA|TestB)$'`
in the package and module that hold them), and requires at least one of them to
fail. The tree itself is never modified. Before the first mutation it runs the
same tests on an unmutated copy and stops if any fails, because a test that
already fails would "kill" every mutation it is linked to.

A patch that does not apply, a mutated copy that does not compile, a mutation
that every linked test survives, and a mutation whose linked test ran into
`-timeout` are all failures, and the exit status is non-zero. A hang is not a
kill: it is reported with its own status, `timeout`, and its own count in the
summary line (`57 mutations, 56 killed, 1 timeout, 0 survived or errored`). Fix
the test so it fails fast.
Entries that share a `fault_id` share its patch, and the tests of all of them
are run.

Expected runtime: about a minute for the 57 faults on a 16-core machine with a
warm Go build cache (`-j` defaults to half the CPUs), longer on a cold cache.

A full run is a release step and is required before a release (AT-002); it is
too slow for every pull request. The cheap half runs in every `go test
./conformance`: `TestMutations_PatchPerFault` requires a patch for every
`fault_id` and a `fault_id` for every patch, and `TestMutations_PatchShape`
requires each patch to be a unified diff of non-test source. See
[RELEASING.md](../RELEASING.md).

### Adding or changing a fault

1. Add `fault_id` and `mutation` to the entry, and link the tests that must catch
   it in `tests`.
2. Write the patch: copy the tree, make the smallest change that breaks exactly
   this obligation, and `diff -u` it with `a/` and `b/` path prefixes into
   `mutations/<fault_id>.patch`. Keep the context to three lines so unrelated
   edits do not stop it applying.
3. Run `go run ./internal/cmd/mutate -only <fault_id>` and confirm the right test
   kills it. A mutation that survives means the test does not assert the
   obligation: fix the test, not the patch.

A fault that cannot be a code change (a purely observational obligation) uses an
`assertion_note` instead of a patch.
