<!--
Thank you for contributing. See CONTRIBUTING.md before you open a pull request.
For a large change, open an issue first and link it here.
-->

## What and why

<!-- Describe the change and the reason for it. Link the issue, if any. -->

## SAP reference

<!-- The SAP section(s) this change implements or touches, e.g. "SAP section 5.2". -->

## Checklist

- [ ] Optional: commits carry a `Signed-off-by:` line (`git commit -s`). See the DCO section of CONTRIBUTING.md.
- [ ] Commit subjects are prefixed by the package, for example `identity: ...`.
- [ ] `go test -race ./...` passes in the root module and in `test/`.
- [ ] `go vet ./...` and `gofmt -l .` are clean.
- [ ] New exported identifiers have a godoc comment that cites the SAP section.
- [ ] No new dependency (or the reason for one is recorded in this description).
- [ ] `CHANGELOG.md` has an entry under Unreleased, where the change is user-visible.
