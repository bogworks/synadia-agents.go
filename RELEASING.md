# Releasing

Checklist for a release, in order. Do not tag until every box is ticked.

- [ ] CI is green on the commit to release: lint, `go test -race ./...` in both
      modules, the conformance catalog check, and the interoperability tests
      (the `interop` workflow, `interop-check` green, with
      `SAP_INTEROP_REQUIRED=1` so no scenario is skipped).
- [ ] Mutation run passes (required before a release, AT-002; CI runs it on
      every PR as the `mutate` job): `go run ./internal/cmd/mutate`. Every
      fault in `conformance/catalog.json` is killed by a linked test, and no mutation is reported `timeout` or
      `ERROR`, and the summary line shows 0 timeout, 0 survived or errored. See [conformance/README.md](conformance/README.md), "Mutation
      testing". Record the result (faults run, killed, timeout, survived, seconds), the commit,
      the Go version and the date in [docs/releases/](docs/releases/), one file
      per release (`vX.Y.Z.md`), and in the release notes. Run it before the tag
      and again on the tagged commit; the second run is the final one.
- [ ] The catalog result for the release is recorded: the CI catalog job, which
      fetches the spec pinned in `conformance/catalog.json`, verifies its sha256
      and runs `ValidateSpec` (`SAP_REQUIRE_SPEC=true`). Locally, set
      `SAP_SPEC_PATH` to that pinned `core-protocol.md`.
- [ ] `CHANGELOG.md`: move Unreleased to the new version and date.
- [ ] Tag the release (`vX.Y.Z`) on the checked commit and push the tag.
- [ ] The `release` workflow (`.github/workflows/release.yml`, ADR 0002) is green and the
      release holds the `sapconform` binaries, the source archive, the CycloneDX
      SBOM and `SHA256SUMS`.

## What a release carries

Pushing a `v*` tag runs the release workflow on our self-hosted runners. It
needs a dated `## [X.Y.Z] - YYYY-MM-DD` section in `CHANGELOG.md`, builds the
files below, attests build provenance for each (`actions/attest-build-provenance`)
and attaches them to a GitHub release:

- `sapconform-linux-amd64`, `sapconform-linux-arm64`, `sapconform-darwin-arm64`
- `synadia-agents.go-vX.Y.Z.tar.gz`, a `git archive` of the tag
- `synadia-agents.go-vX.Y.Z.cdx.json`, the CycloneDX SBOM of the module
- `SHA256SUMS`

Verify a downloaded file, and its provenance, with:

```
sha256sum -c SHA256SUMS --ignore-missing
gh attestation verify <file> --repo bogworks/synadia-agents.go
```

The attestation says only that this workflow, at this tag, on a runner the
maintainers operate, built those bytes. It does not say the runner was
uncompromised (ADR 0002). The workflow cannot be run before a tag; its YAML is
checked by `test/ci/workflows_test.go`.

Evidence for each release (the mutation run, before and at the tag) is in
[docs/releases/](docs/releases/), for example [v0.1.0](docs/releases/v0.1.0.md).
