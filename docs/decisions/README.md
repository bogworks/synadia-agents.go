# Decisions

Architecture decision records (ADRs) for this repository, in [MADR 4.0](https://adr.github.io/madr/) format.
[0001](0001-record-architecture-decisions.md) says how they work: one decision per file, never rewritten once
accepted, superseded by a new ADR instead.

## Rules

- A file is `NNNN-kebab-title.md` with four digits. Numbers are unique; gaps are allowed.
- Front matter holds `status` (`proposed`, `accepted`, `rejected`, `deprecated` or `superseded by ADR-NNNN`,
  optionally followed by `; a note`), `date` (`YYYY-MM-DD`) and `decision-makers`.
- The first heading is the title. The sections follow in this order: Context and Problem Statement, Decision Drivers,
  Considered Options, Decision Outcome (with `### Consequences` and `### Confirmation`), Pros and Cons of the
  Options, More Information.
- [`NEXT`](NEXT) holds the next free number. A pull request that adds an ADR takes that number and bumps `NEXT` in the
  same pull request.
- An accepted ADR is immutable. Only its status (to `superseded by ADR-NNNN` or `deprecated`) and one
  `Superseded by [..](..)` line may change.

## Pull requests

The pull request body carries exactly one line, `Decision: none` or `Decision: ADR-NNNN`. An ADR number must name a
record the pull request adds or changes. `Decision: none` on a change to a protected path (the records themselves, the
merge queue, the workflows and actions, the linter and the CI guard test) needs the `decision-confirmed` label, which a
reviewer applies; a push removes the label again.

The `adr-lint` workflow runs the linter built from the base branch. Run `go run ./internal/cmd/adrlint docs` locally
before pushing.

## Records

| ADR | Title | Status | Date |
|---|---|---|---|
| [0001](0001-record-architecture-decisions.md) | Record architecture decisions | accepted | 2026-11-11 |
| [0002](0002-releases-ship-an-sbom-and-build-provenance-from-our-own-runners.md) | Releases ship an SBOM and build provenance from our own runners | accepted | 2026-11-12 |
| [0003](0003-a-security-stance-selects-strict-or-relaxed-identity-checks.md) | A security stance selects strict or relaxed identity checks | accepted | 2026-11-12 |
