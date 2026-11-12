---
status: accepted
date: 2026-11-12
decision-makers: Project maintainers
---

# A security stance holds concrete per-check settings

## Context and Problem Statement

In v0.1.0 every identity check is opt-in. A host serves any sender until it is given `WithMinSenderTrust`, a client trusts
any discovery reply until it is given `WithTrustPolicy`, a heartbeat tracker verifies nothing until it is given
`WithHeartbeatVerification`, and a connection yields a signer only with `WithSignedIdentity`. A program that wants the
secure posture must find and set four options in three packages, and one it forgets stays open. The product owner first
ruled: "Signed on strict mode, unsigned on relaxed. Separate override setting for this in case people want relaxed mode
with signing." A later ruling (see Changes from ADR-0003) removed named modes from the SDK. How does a program choose its posture
once, and what applies when it chooses nothing?

## Decision Drivers

* A program chooses one posture in one place, and cannot forget one of the checks.
* Signing is separable from the other checks.
* An existing explicit option keeps its meaning, and the order of options does not matter.
* The default of v0.1.0 does not change silently, and it can change later in one line.
* SAP makes the extension optional, so none of this is wire behaviour.

## Considered Options

* Keep four independent opt-in options and document them together.
* Make strict the default now.
* A stance of concrete per-check settings, each with its own documented default.

## Decision Outcome

Chosen option: "A stance of concrete per-check settings, each with its own documented default".

`identity.Stance` carries a `Setting` (`Unset`, `On` or `Off`) for each of five checks. `Unset`, the zero value, means
the SDK default of that check applies:

* signed senders: a host's `min_sender_trust` becomes `signed`;
* verified discovery: a client trusts any agent whose `id_sig` verifies, as a trust policy that accepts every verified
  signer does, and it gates `Agent.Prompt` and `Agent.Status` with `ErrUntrustedAgent`;
* heartbeat verification: a tracker verifies every heartbeat that carries an `Agent-Sender` header and discards one that
  fails, and still keeps one with no header;
* signing: `connect.ResolveBundle` yields the signer from the credentials' user seed, and `host.New` and `caller.New`
  need a `WithIdentity` with a `Signer`;
* require signer: what `host.New` and `caller.New` do when signing is on and no `Signer` was given. On fails; Off
  succeeds and logs one warning to the configured logger.

The defaults are the exported constants `identity.DefaultSignedSenders`, `DefaultVerifiedDiscovery`,
`DefaultHeartbeatVerification` and `DefaultSigning` (all off, the behaviour of v0.1.0) and `DefaultRequireSigner` (on).
They live in `identity/stance.go` and nowhere else. `Stance.Resolved` returns a stance with every `Unset` replaced by its
default.

An explicit option of a package (`host.WithMinSenderTrust`, `caller.WithTrustPolicy`, `caller.WithHeartbeatVerification`,
`connect.WithSignedIdentity`) wins over the stance for its check, whatever the order. On a bundle, `Signing: On` behaves
like `WithSignedIdentity`; `Unset` and `Off` yield no signer.

Each package takes the stance through one option (`host.WithStance`, `caller.WithStance`, `caller.WithTrackerStance`,
`connect.WithStance`), and all resolution lives in `internal/stance`. When a program sets no check and no explicit option
for the check, a logger it configured gets one warning per process and package; nothing is logged without a logger, and
never to the default logger.

The public names and the source of the setting (an option today; a file or the environment later) are not final. They are
kept behind the one `identity.Stance` type so that they can change cheaply. The defaults are not final either.

### Consequences

* Good, because one value sets the posture of a program across connect, host and caller.
* Good, because signing can be on without refusing unsigned senders, and the reverse.
* Good, because the defaults are those of v0.1.0 and each changes in one constant.
* Good, because the SDK holds no policy beyond the per-check defaults.
* Bad, because the defaults stay open until decided, so a program that needs strictness must say so, check by check.
* Bad, because a new option set exists beside the old ones, and the precedence rule must be learned.
* Neutral, because the names may still change before release.

### Confirmation

Table tests in `internal/stance`, `host`, `caller` and `connect` cover each check unset, on and off, the signer
requirement with `RequireSigner` on and off, precedence of the explicit options in both orders, invalid values, and the
warnings.
`test/host` and `test/caller` run the host and caller checks against an embedded server.

## Pros and Cons of the Options

### Keep four independent opt-in options and document them together

* Good, because nothing changes.
* Bad, because a program can forget one check, and the posture is spread over three packages.

### Make strict the default now

* Good, because a program is secure without doing anything.
* Bad, because it breaks every v0.1.0 program that talks to an unsigned peer, and the owner has not decided it.

### A stance of concrete per-check settings, each with its own documented default

* Good, because the SDK interprets no named profile; the deployment decides policy and passes settings.
* Good, because it leaves each default open.
* Bad, because it adds a public type and four options.

## More Information

* [docs/secure-configuration.md](../secure-configuration.md).

## Changes from ADR-0003

ADR-0003 chose "a profile type with per-check overrides", with `Relaxed` and `Strict`
profiles and a relaxed default. The owner ruled that the SDK never interprets named profiles. The deployment around the SDK
decides policy and passes concrete settings. The code was unreleased, so the change is breaking and clean:

* `Profile`, `ProfileDefault`, `Relaxed`, `Strict`, `DefaultProfile` and `Stance.Profile` are removed. `Inherit` is
  renamed `Unset`.
* `RequireSigner` is split out from `Signing`. Signing says the program signs; `RequireSigner` says whether a missing
  signer is an error (the default) or one warning.
* Every check has its own default constant of type `Setting`, never `Unset`, in `identity/stance.go`.
* The warning for an unchosen stance fires when no check is set, and no longer names a profile.
* `Stance.Resolved` returns the effective settings.

The names `host.WithStance`, `caller.WithStance`, `caller.WithTrackerStance` and `connect.WithStance` and the rule that an
explicit option wins over the stance are unchanged. A deployment that wants a strict posture sets `SignedSenders`,
`VerifiedDiscovery`, `HeartbeatVerification` and `Signing` to `On` itself.
