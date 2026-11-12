# Changelog

All notable changes to this project are recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Each release states the
SAP revision it implements and its conformance catalog result.

## [Unreleased]

### Added

- A security stance, `identity.Stance`, that selects the strict or relaxed profile of four identity checks (signed
  senders on a host, verified discovery and heartbeat verification on a caller, and signing), with a separate
  `Signing` override and a per-check setting, through `host.WithStance`, `caller.WithStance`,
  `caller.WithTrackerStance` and `connect.WithStance`. The default profile is relaxed, the behaviour of v0.1.0, and it
  may change; a program that chooses nothing and has a logger gets one warning. See ADR-0003.

## [0.1.0] - 2026-11-12

The first release. SAP revision: 0.3 (spec commit `c48cea5f`). Conformance: every obligation in
`conformance/catalog.json` is covered by a linked test, and the mutation run kills every fault in the catalog (the
result is in [docs/releases/v0.1.0.md](docs/releases/v0.1.0.md)). The SDK is tested against the official
TypeScript and Python SDKs in `test/interop`.

### Added

- Go SDK for the Synadia Agents Protocol (SAP) 0.3, spec commit `c48cea5f`:
  - the root package `agents`: subjects and token validation, headers, the prompt envelope, response chunks,
    errors, heartbeats, attachments, discovery records and version checks;
  - package `caller`: discovery, direct lookup and ping, prompts with a streamed reply (`iter.Seq2`), mid-stream
    queries, status, a heartbeat tracker, one multiplexed reply subscription per client, interceptors and an
    outgoing-header hook;
  - package `host`: registration as a NATS micro service, prompt and status endpoints, replies, queries,
    heartbeats, extra endpoints, a verified-sender admission hook, interceptors, bounded concurrency;
  - a lifecycle as `net/http.Server` has: `host.Host`, `caller.Client` and `tools.Tools` each have `Close() error`
    (an `io.Closer`, bounded by the package's `DefaultCloseTimeout`) and `Shutdown(ctx) error` (the same stop with a
    deadline the caller sets). `host.Host.Close` stops at once; `host.Host.Shutdown` lets the handlers in flight
    finish. The stop is requested at once and the context bounds only the wait: when the deadline ends first, the
    context error is returned wrapped, and a later `Close` or `Shutdown` still waits for the real exit. A
    `host.Host.Start` that sees the stop request tears down and returns `host.ErrStopped`;
  - package `identity`: the section 13 sender identity (agent IDs, the `Agent-Sender` header, signing,
    live and stored verification, replay protection, `id_sig` registration, signed heartbeats);
  - package `connect`: NATS CLI contexts, credential files and reconnect defaults (outside SAP); `LoadContext` and
    `SignerFromContext` take a leading `ctx`, which a key-file read honors;
  - package `tools`: the agent tool layer of the official SDKs (outside SAP);
  - package `agentstest`: a reference agent for tests;
  - package `conformance` and the command `cmd/sapconform`: the conformance catalog and a host-side runner;
  - runnable examples in `examples/`: `host-echo`, `caller-basic`, `host-query`, `signed`, `extension-points`;
  - `Example` functions for the main paths in `agents`, `caller`, `host`, `identity` and `connect`; the README quick
    start is `caller`'s, and a test fails when the two differ;
  - documentation: `docs/protocol-mapping.md` (every exported identifier mapped to its SAP section, checked by
    a test), `docs/parity.md` (every feature of the official TypeScript and Python SDKs and its Go
    counterpart) and `docs/api-plan.md` (the original plan);
  - tests, including interoperability with the official TypeScript and Python SDKs in `test/interop`.
- CI on self-hosted runners: build, test, vet, lint, `govulncheck`, `go mod tidy`, an independence check, the
  catalog check and the mutation run, gathered in one required `pr-check` job; a separate `interop` workflow runs
  the official TypeScript and Python SDKs against this one.
- A documentation-only pull request skips the heavy CI jobs; every required check still reports, a cheap
  `docs-guard` job runs the tests that read documentation, and merge-queue runs and pushes to `main` run in full.
- A release workflow (`.github/workflows/release.yml`) that, on a `v*` tag, builds the `sapconform` binaries for
  linux/amd64, linux/arm64 and darwin/arm64, a source archive, an SBOM and `SHA256SUMS`, attests build provenance
  for each, and attaches them to a GitHub release (ADR 0002).
- The host sends a keep-alive ack every 30 s (`host.DefaultKeepalive`) by default, as the official hosts do,
  because the official callers' default 60 s inactivity timeout otherwise stalls long-running prompts;
  `host.WithKeepalive(0)` or `host.WithoutKeepalive` turns it off for strict SAP:L380 behaviour (a recorded
  deviation, `docs/upstream-issues.md` issue 18).
