---
status: accepted
date: 2026-11-11
decision-makers: Project maintainers
---

# Record architecture decisions

## Context and Problem Statement

The SDK implements a wire protocol and makes choices where the protocol leaves room. Those choices, and the reasons for
them, are easy to lose in pull request threads. Contributors need to find them and to see when a change reverses one.

## Decision Drivers

* A reader can find why something is the way it is.
* A change that makes or reverses a decision says so.
* The record is cheap to write and reviewed with the code.

## Considered Options

* Record decisions as MADR files in `docs/decisions/`, checked by a linter.
* Keep decisions in pull request descriptions and issues.
* Keep decisions in a single design document.

## Decision Outcome

Chosen option: "MADR files in `docs/decisions/`, checked by a linter", because the records live with the code, are
reviewed in the same pull request, and a check can enforce their form.

### Consequences

* Good, because a decision and its reasons are versioned with the code.
* Good, because an accepted record cannot be silently rewritten; a new record supersedes it.
* Bad, because a pull request must state a decision or say there is none, which is a small cost on every change.

### Confirmation

The `adr-lint` workflow lints the records, refuses a change to an accepted record, and requires the pull request body to
carry one `Decision: none` or `Decision: ADR-NNNN` line. The linter's tests are in `internal/cmd/adrlint`.

## Pros and Cons of the Options

### MADR files in `docs/decisions/`, checked by a linter

* Good, because it is versioned, reviewable and enforceable.
* Bad, because it needs a linter and a convention to maintain.

### Pull request descriptions and issues

* Good, because there is nothing to maintain.
* Bad, because the reasons are hard to find later and are not versioned with the code.

### A single design document

* Good, because it is one place to read.
* Bad, because it drifts, and a reversal is not visible in review.

## More Information

* [MADR 4.0](https://adr.github.io/madr/)
* [README](README.md) describes the rules the linter applies.
