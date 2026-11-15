---
status: accepted
date: 2026-11-15
decision-makers: Project maintainers
---

# The SDK carries no security stance; a posture belongs to the application

## Context and Problem Statement

ADR-0003 and then ADR-0004 added a security stance to the SDK: an `identity.Stance` of one setting per identity check,
taken through `host.WithStance`, `caller.WithStance`, `caller.WithTrackerStance` and `connect.WithStance`, with SDK
defaults and a `RequireSigner` check that made `host.New` and `caller.New` fail when signing was on and no signer was
given. The maintainer has ruled that a security stance is not the SDK's concept: one SDK of the Synadia family cannot
change how the family's SDKs work for its language alone, and a posture belongs to the application and is expressed by how
it uses the SDK. Neither ADR has been part of a release. Does the SDK keep a stance, and where does
a posture live?

## Decision Drivers

* The Synadia SDKs (TypeScript and Python) expose SAP's checks one by one and hold no posture.
* The sender identity extension is optional in SAP, so a posture is policy, not protocol.
* Nothing released may change: v0.1.0 has no stance.
* A second way to set each check, beside its own option, is surface the SDK has to keep.

## Considered Options

* Keep the stance of ADR-0004.
* Remove the stance and keep the per-check options of v0.1.0.

## Decision Outcome

Chosen option: "Remove the stance and keep the per-check options of v0.1.0".

The SDK carries no security stance. It exposes the checks of SAP as separate options, and an application expresses its
posture by setting them. The stance is removed before any release, so nothing released changes: every option has the
meaning and default it has in v0.1.0.

The options, and their defaults:

* `host.WithMinSenderTrust`: `identity.TrustAny`; `identity.TrustSigned` makes the prompt endpoint require a signed
  sender.
* `host.WithAdmission`: no hook, so every sender the trust level admits is served.
* `host.WithOperatorAttested`: off.
* `host.WithReplySubjectPolicy`: the default policy refuses `$` subjects, wildcards and protocol subjects.
* `host.WithIdentity` and `caller.WithIdentity`: no identity; a `Signer` makes the program sign.
* `caller.WithTrustPolicy`: no policy, so a client trusts every discovery reply.
* `caller.WithHeartbeatVerification`, through `caller.WithTracker`: the tracker verifies nothing.
* `connect.WithSignedIdentity`: no signer.

Removed from the public API: `identity.Stance`, `identity.Setting` with `Unset`, `On` and `Off`, `Stance.Resolved`, the
constants `DefaultSignedSenders`, `DefaultVerifiedDiscovery`, `DefaultHeartbeatVerification`, `DefaultSigning` and
`DefaultRequireSigner`, and the options `host.WithStance`, `caller.WithStance`, `caller.WithTrackerStance` and
`connect.WithStance`. The package `internal/stance` is removed with them, and so is the once-per-process warning for an
unchosen stance. `host.New` and `caller.New` do not check for a signer, as in v0.1.0.
[docs/secure-configuration.md](../secure-configuration.md) describes how to set each check.

### Consequences

* Good, because the SDK matches the Synadia SDKs and holds no policy of its own.
* Good, because behaviour equals v0.1.0 and the public surface is smaller.
* Good, because an application sets its posture in one place of its own, and the SDK does not change when the posture
  does.
* Bad, because a program that wants every check sets each option, in three packages, and one it forgets stays open.
* Neutral, because ADR-0003 and ADR-0004 stay as the record of a stance that was never released.

### Confirmation

The tests of v0.1.0 for each option cover its meaning and default. `TestProtocolMapping_Complete` fails when the mapping
document and the exported API differ, so no stance name can linger in the API without a mapping row.

## Pros and Cons of the Options

### Keep the stance of ADR-0004

* Good, because one value sets a posture across packages.
* Bad, because the SDK interprets policy that belongs to the application, and it adds a public type and four options.

### Remove the stance and keep the per-check options of v0.1.0

* Good, because it is the model of the other SDKs and changes nothing released.
* Bad, because a program sets each check itself.

## More Information

* [docs/secure-configuration.md](../secure-configuration.md).
* Supersedes [ADR 0004](0004-a-security-stance-holds-concrete-per-check-settings.md).
