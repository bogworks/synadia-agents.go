# Changelog

All notable changes to this project are recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Each release states the
SAP revision it implements and its conformance catalog result.

## [Unreleased]

### Added

- Go SDK for the Synadia Agents Protocol (SAP) 0.3, spec commit `c48cea5f`:
  - the root package `agents`: subjects and token validation, headers, the prompt envelope, response chunks,
    errors, heartbeats, attachments, discovery records and version checks;
  - package `caller`: discovery, direct lookup and ping, prompts with a streamed reply (`iter.Seq2`), mid-stream
    queries, status, a heartbeat tracker, one multiplexed reply subscription per client, interceptors and an
    outgoing-header hook;
  - package `host`: registration as a NATS micro service, prompt and status endpoints, replies, queries,
    heartbeats, extra endpoints, a verified-sender admission hook, interceptors, bounded concurrency;
  - package `identity`: the section 13 sender identity (agent IDs, the `Agent-Sender` header, signing,
    live and stored verification, replay protection, `id_sig` registration, signed heartbeats);
  - package `connect`: NATS CLI contexts, credential files and reconnect defaults (outside SAP);
  - package `tools`: the agent tool layer of the official SDKs (outside SAP);
  - package `agentstest`: a reference agent for tests;
  - package `conformance` and the command `cmd/sapconform`: the conformance catalog and a host-side runner;
  - runnable examples in `examples/`: `host-echo`, `caller-basic`, `host-query`, `signed`, `extension-points`;
  - documentation: `docs/protocol-mapping.md` (every exported identifier mapped to its SAP section, checked by
    a test), `docs/parity.md` (every feature of the official TypeScript and Python SDKs and its Go
    counterpart) and `docs/api-plan.md` (the original plan);
  - tests, including interoperability with the official TypeScript and Python SDKs in `test/interop`.
- CI on self-hosted runners: build, test, vet, lint, `govulncheck`, `go mod tidy`, an independence check and the
  catalog check, gathered in one required `pr-check` job.
  - the host sends a keep-alive ack every 30 s (`host.DefaultKeepalive`) by default, as the official hosts do,
    because the official callers' default 60 s inactivity timeout otherwise stalls long-running prompts;
    `host.WithKeepalive(0)` or `host.WithoutKeepalive` turns it off for strict SAP:L380 behaviour (a recorded
    deviation, `docs/upstream-issues.md` issue 18).
