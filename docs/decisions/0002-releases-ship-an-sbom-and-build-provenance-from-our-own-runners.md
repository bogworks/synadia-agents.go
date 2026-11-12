---
status: accepted
date: 2026-11-12
decision-makers: Project maintainers
---

# Releases ship an SBOM and build provenance from our own runners

## Context and Problem Statement

A release of this library is a tag. Users also download the `sapconform` binaries and the source. Nothing so far lets a
consumer check that those files came from the tagged source through our workflow, or list the modules inside them. All
our CI runs on self-hosted runners and no GitHub-hosted runner is allowed. Does a release carry an SBOM and signed build
provenance, and what does that provenance claim when our own runners make it?

## Decision Drivers

* A consumer can verify where a release file came from and what went into it.
* The attestation must not claim more than is true.
* Releases must be possible with the self-hosted runners we have, and with no GitHub-hosted runner.
* The same policy as our other projects.

## Considered Options

* Ship no SBOM and no provenance; publish checksums only.
* Attest provenance from a hosted or separately trusted builder.
* Attest provenance and an SBOM from our own self-hosted runners, and say only that we built them.

## Decision Outcome

Chosen option: "Attest provenance and an SBOM from our own self-hosted runners, and say only that we built them", the
same policy as our other projects. A refusal of attestations from our own runners would make every release fail
verification, because there is no other runner. So the release workflow (`.github/workflows/release.yml`) accepts them
and states the claim.

On a `v*` tag it builds the `cmd/sapconform` binaries (linux/amd64, linux/arm64, darwin/arm64, no cgo, `-trimpath`), a
`git archive` source archive, a CycloneDX SBOM of the module and `SHA256SUMS`. It attests build provenance for each file
with `actions/attest-build-provenance` and attaches all of it to a GitHub release. A consumer checks a file with
`gh attestation verify <file> --repo bogworks/synadia-agents.go`.

The attestation claims that this workflow, at this tag, on a runner the maintainers operate, built and attested these
exact bytes, recorded in Sigstore. It does not claim the runner was uncompromised or ephemeral: the runner also runs
pull request jobs from this repository, so a consumer decides whether to trust the maintainers' runners. This is
SLSA build level 2 at most. A trusted-builder mechanism is a later decision.

### Consequences

* Good, because every release file can be verified against our workflow, repository and ref.
* Good, because the SBOM lists the modules a release depends on.
* Bad, because the signature comes from a runner that also runs pull request jobs; a compromise of it could produce a
  genuine attestation for bad bytes, and the attestation cannot show otherwise.
* Bad, because consumers must decide themselves whether to trust our runners.
* Neutral, because the release workflow cannot be run before a tag; it is checked by the workflow guard test and
  exercised at the first tag.

### Confirmation

`test/ci/workflows_test.go` runs on `release.yml` like every workflow: self-hosted labels only, actions pinned by full
SHA, `cache: false`, read-only top-level permissions. It allows `contents: write` only for the `release` job and
`id-token` and `attestations` write only for the `attest` job. RELEASING.md lists the verification command.

## Pros and Cons of the Options

### Ship no SBOM and no provenance; publish checksums only

* Good, because there is nothing to maintain.
* Bad, because a checksum shows only that a file is intact, not where it was built.

### Attest provenance from a hosted or separately trusted builder

* Good, because the builder is not one we operate, so the claim is stronger.
* Bad, because it needs a runner or reusable workflow we do not have and may not use.

### Attest provenance and an SBOM from our own self-hosted runners, and say only that we built them

* Good, because it works with the runners we have and the claim is true.
* Bad, because the claim is limited to "we built it".

## More Information

* [RELEASING.md](../../RELEASING.md) and [docs/releases/v0.1.0.md](../releases/v0.1.0.md).
