---
status: superseded by ADR-0004
date: 2026-11-12
decision-makers: Project maintainers
---

# A security stance selects strict or relaxed identity checks

## Context and Problem Statement

In v0.1.0 every identity check is opt-in. A host serves any sender until it is given `WithMinSenderTrust`, a client trusts
any discovery reply until it is given `WithTrustPolicy`, a heartbeat tracker verifies nothing until it is given
`WithHeartbeatVerification`, and a connection yields a signer only with `WithSignedIdentity`. A program that wants the
secure posture must find and set four options in three packages, and one it forgets stays open. The product owner ruled:
"Signed on strict mode, unsigned on relaxed. Separate override setting for this in case people want relaxed mode with
signing." How does a program choose its posture once, and what applies when it chooses nothing?

## Decision Drivers

* A program chooses one posture in one place, and cannot forget one of the checks.
* Strict means signed and relaxed means unsigned, with signing separable from the rest.
* An existing explicit option keeps its meaning, and the order of options does not matter.
* The default of v0.1.0 does not change silently, and it can change later in one line.
* SAP makes the extension optional, so none of this is wire behaviour.

## Considered Options

* Keep four independent opt-in options and document them together.
* Make strict the default now.
* A profile type with per-check overrides and a separate signing override, relaxed by default for now.

## Decision Outcome

Chosen option: "A profile type with per-check overrides and a separate signing override, relaxed by default for now".

`identity.Stance` carries a `Profile` (`Relaxed` or `Strict`, with the zero value meaning none was chosen) and a `Setting`
(`Inherit`, `On` or `Off`) for each of four checks:

* signed senders: a host's `min_sender_trust` becomes `signed`;
* verified discovery: a client trusts any agent whose `id_sig` verifies, as a trust policy that accepts every verified
  signer does, and it gates `Agent.Prompt` and `Agent.Status` with `ErrUntrustedAgent`;
* heartbeat verification: a tracker verifies every heartbeat that carries an `Agent-Sender` header and discards one that
  fails, and still keeps one with no header;
* signing, the separate override the product owner asked for: `connect.ResolveBundle` yields the signer when the
  credentials hold a user seed, and `caller.New` and `host.New` fail when no `WithIdentity` with a `Signer` was given.

Strict turns all four on and relaxed turns all four off, which is v0.1.0. An explicit `On` or `Off` wins over the profile.
An explicit option of a package (`host.WithMinSenderTrust`, `caller.WithTrustPolicy`, `caller.WithHeartbeatVerification`,
`connect.WithSignedIdentity`) wins over the stance for its check, whatever the order. A signing that came only from the
profile and finds no user seed yields no signer and no error on a bundle; `WithSignedIdentity` and an explicit
`Signing: On` still fail with `ErrNoUserSeed`.

Each package takes the stance through one option (`host.WithStance`, `caller.WithStance`, `caller.WithTrackerStance`,
`connect.WithStance`), and all resolution lives in `internal/stance`. The default is `identity.DefaultProfile`, relaxed.
When a program chooses no stance and sets no explicit option for the check, a logger it configured gets one warning per
process and package; nothing is logged without a logger, and never to the default logger.

The public names and the source of the setting (an option today; a file or the environment later) are not final. They are
kept behind the one `identity.Stance` type so that they can change cheaply. The default is not decided either.

### Consequences

* Good, because one value sets the posture of a program across connect, host and caller.
* Good, because signing can be on without refusing unsigned senders, and the reverse.
* Good, because the default of v0.1.0 is unchanged and flipping it is one constant.
* Bad, because the default stays open until it is decided, so a program that needs strictness must say so.
* Bad, because a new option set exists beside the old ones, and the precedence rule must be learned.
* Neutral, because the names may still change before release.

### Confirmation

Table tests in `internal/stance`, `host`, `caller` and `connect` cover both profiles for each check, the signing override,
each check set off under strict, precedence of the explicit options in both orders, invalid values, and the warning.
`test/host` and `test/caller` run the host and caller checks against an embedded server.

## Pros and Cons of the Options

### Keep four independent opt-in options and document them together

* Good, because nothing changes.
* Bad, because a program can forget one check, and the posture is spread over three packages.

### Make strict the default now

* Good, because a program is secure without doing anything.
* Bad, because it breaks every v0.1.0 program that talks to an unsigned peer, and the owner has not decided it.

### A profile type with per-check overrides and a separate signing override, relaxed by default for now

* Good, because it gives the owner's strict and relaxed modes and the separate signing override.
* Good, because it leaves the default open.
* Bad, because it adds a public type and four options.

## More Information

* [docs/secure-configuration.md](../secure-configuration.md).

Superseded by [ADR 0004](0004-a-security-stance-holds-concrete-per-check-settings.md)
