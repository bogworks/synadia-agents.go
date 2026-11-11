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
      testing". Record the result (faults run, killed, seconds) in the release
      notes.
- [ ] The catalog result for the release is recorded: the CI catalog job, which
      fetches the spec pinned in `conformance/catalog.json`, verifies its sha256
      and runs `ValidateSpec` (`SAP_REQUIRE_SPEC=true`). Locally, set
      `SAP_SPEC_PATH` to that pinned `core-protocol.md`.
- [ ] `CHANGELOG.md`: move Unreleased to the new version and date.
- [ ] Tag the release (`vX.Y.Z`) on the checked commit and push the tag.
