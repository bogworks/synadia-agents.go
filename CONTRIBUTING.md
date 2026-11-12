# Contributing

Thank you for your interest in the SAP Go SDK. This guide follows the norms of
[nats-io/nats.go](https://github.com/nats-io/nats.go/blob/main/CONTRIBUTING.md).

## Code of Conduct

This project follows the [CNCF Code of Conduct](CODE_OF_CONDUCT.md). By taking part you agree to it.

## Report a problem or propose a change

- **Bugs and small fixes.** Open an issue, or go straight to a pull request for an obvious fix.
- **Large changes.** Open an issue first and agree the approach before you write the code. This
  covers a new public API, a change to behaviour that SAP leaves open, and any new dependency.
- **Security problems.** Do not open an issue. Follow [SECURITY.md](SECURITY.md).

## The Developer Certificate of Origin

A sign-off is welcome but not required. If you want to certify the
[Developer Certificate of Origin](https://developercertificate.org/) for a commit, add a
`Signed-off-by` line:

```
Signed-off-by: Your Name <you@example.com>
```

Add it with `git commit -s`. No CI check requires it, and a pull request is not refused for lacking
it.

## Rules for the code

- **No non-essential dependencies.** At run time the SDK depends only on
  `github.com/nats-io/nats.go` and `github.com/nats-io/nkeys`, plus the standard library. A further
  dependency needs a recorded reason in the issue and the pull request. Anything that needs a live
  NATS server in a test goes in the separate `test/` module.
- **Follow the spec.** SAP is the authority for wire behaviour. Every exported identifier has a
  godoc comment that cites its SAP section. Do not turn a SHOULD or a MAY of the spec into a MUST;
  where the SDK chooses, say in the comment that it is SDK policy.
- **Quiet by default.** The SDK never writes to stdout, stderr or a global logger. It logs only
  through an optional `*slog.Logger`, and never logs seeds, credentials or payloads.
- **Context and errors.** Blocking calls take a `context.Context`. Wrap errors with `%w`, and keep
  sentinel and typed errors usable with `errors.Is` and `errors.As`.
- **Licence header.** Each Go file starts with:

  ```go
  // Copyright 2026 The synadia-agents.go Authors
  // SPDX-License-Identifier: Apache-2.0
  ```

## Tests

- Write table-driven tests, and name them so the conformance catalog can refer to them, for
  example `TestSubject_PromptSubjectFormat`.
- Run the tests with the race detector, in both modules:

  ```sh
  go test -race ./...
  (cd test && go test -race ./...)
  ```

- `go vet ./...` must be clean and `gofmt -l .` must print nothing. CI also runs `golangci-lint`
  (see `.golangci.yml`), `govulncheck`, a `go mod tidy` check and the conformance catalog check, on
  the two latest Go releases. A documentation-only pull request (every file is `*.md`, under `docs/`,
  or `LICENSE`, `CODE_OF_CONDUCT` or `.gitignore`) skips those jobs, which are work, not checks: `pr-check` and
  `interop-check` still report and pass, and the tests that read documentation still run (docs-guard) (`.github/scripts/changes.sh`).

## Commits and pull requests

- Prefix the commit subject with the package, as `synadia-io/orbit.go` does:
  `identity: reject a seed with the wrong prefix`, `caller: honour the context on Send`,
  `docs: ...`, `ci: ...`. Use the imperative mood and keep the subject short.
- Explain the why in the body. Reference the SAP section where one applies.
- Keep a pull request to one concern. Fill in the pull request template, and add a line to
  `CHANGELOG.md` under Unreleased for a user-visible change.

## Licence

By contributing you agree that your contribution is licensed under the [Apache License 2.0](LICENSE).
