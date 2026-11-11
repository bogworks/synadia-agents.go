# Mapping from mash's `internal/sap` to this SDK

> **This file is not part of the upstream landing.** It is the one document that names mash. It records,
> for a later rebuild of mash on this SDK, which SDK call replaces each piece of mash's SAP code and which
> pieces stay in mash. Nothing here changes mash or this SDK. The SDK has no knowledge of mash, and every
> extension point named below is generic and protocol-level: none is shaped around mash. Leave this file
> (and its exemption in `.github/workflows/go-sdk.yml`) out of any upstream pull request.

Source read: mash `origin/main`, `internal/sap/{subjects,identity,sender}.go`. SDK names are from
`go doc -all` of this module; `agents` is the root package.

## 1. What the SDK replaces

### Subjects (`subjects.go`, `identity.go`)

| mash | SDK | Notes |
|---|---|---|
| `AgentToken` (`cc`), `AgentName` (`claude-code`) | `agents.NewAgentSubject("claude-code", owner, name, agents.WithSubjectToken("cc"))` | The 3rd token and `metadata.agent` are separate in SAP Appendix C; `WithSubjectToken` is the abbreviation. |
| `ServiceName`, `QueueGroup` (`agents`) | `agents.ServiceName`, `agents.QueueGroup`; the host applies both | A host cannot register under another name. |
| `ProtocolVersion` | `agents.ProtocolVersion`, `agents.SpecVersion`, `agents.SpecCommit`, `agents.CheckProtocolVersion` | |
| `Subjects{Prompt, Status, HB}` and `For(owner, name)` | `AgentSubject.Prompt()`, `.Status()`, `.Heartbeat()` of `agents.NewAgentSubject(...)` | The mail field is mash-only (section 2). |
| `EndpointIdentity` (`New`) | `agents.AgentSubject` from `agents.NewAgentSubject` | The SDK checks SAP's MUSTs only. mash's stricter rule stays in mash (section 2); mash runs it first. |
| `Legacy(owner, name)`, `IsLegacy` | `agents.NewAgentSubject("claude-code", owner, name, agents.WithSubjectToken("cc"))` | The legacy form is the `cc` token. The legacy-versus-new rule is mash's. |
| `EndpointIdentity.Validate`, `Subjects()`, `String()` | `AgentSubject.Warnings()`, `.Prompt()`, `.Status()`, `.Heartbeat()`, `.Token()` | |
| `KeyFromSubject` (subject to directory key) | `agents.ParseSubject(subject)` returns the verb and an `AgentSubject`; the flat key is built by mash from `.Agent()`, `.Owner()`, `.Name()` | The flat key is mash's directory convention, not SAP's. |
| `Flat`, `FromFlat`, `Overlaps`, `subjectMatches` | stay in mash (directory keys, admission of overlapping identities) | `agents.FilterSubject` builds the wildcard subscriptions SAP defines. |

### Sender identity (`sender.go`), SAP section 13

| mash | SDK | Notes |
|---|---|---|
| `HeaderName` | `identity.HeaderName` | |
| `Identity`, `NewIdentity(seed, account)` | `identity.SignerFromSeed(seed)` for the key; `identity.NewAgentID(account, signer.PublicKey())` for the ID | The SDK separates the ID (`identity.AgentID`) from the signing key (`identity.Signer`). |
| `IdentityFromCreds`, `IdentityFromCredsAt` | `identity.SignerFromCreds(creds)` (a `JWTSigner`, which also carries the ID), `identity.IdentityFromJWT`, or `connect.ResolveBundle` with `connect.WithSignedIdentity` to read the credentials once | The seed never appears in an error. |
| `Identity.ID()` (section 13.3) | `identity.AgentID.String()`, `identity.ParseAgentID` | |
| `SenderHeader`, `Encode` | `identity.SenderHeader`, `.Encode()`, `.Set(hdr)`, `identity.ParseHeader` | |
| `signedInput` | `identity.SignedInput` | |
| `Identity.Sign(subject, payload, name, now, nonce)` | `identity.SignHeader(identity.SignOptions{Signer, ID, Subject, Payload, Name, TS, Nonce})`; on a connection, `caller.Client.SignSender` or `PublishSigned` / `RequestSigned` with `caller.WithNonce`, `caller.WithSub`, `caller.WithHeaders` | `PublishSigned` also sets `Nats-Msg-Id` to the nonce (section 13.8). |
| `Identity.IDSig`, `idInput` | `identity.SignAgentID`, `identity.AgentIDSignedInput`; `identity.RegistrationMetadata` builds all three keys | A host built with `host.WithIdentity` does this itself. |
| `VerifyIDSig` | `identity.VerifyAgentID(metadata, promptSubject)`; `caller.WithRequireVerifiedIdentity` enforces it on discovery | |
| `parse`, `validAccount` | `identity.ParseHeader`, `identity.NewAgentID`, `identity.MaxAccountLen` | |
| `VerifyStored` | `identity.VerifyStored(msg, identity.StoredOptions{...})` | Authorship only, as section 13.8 says. |
| `LiveSkew` | `identity.DefaultReplayWindow` (`identity.LiveOptions.ReplayWindow`) | |
| `VerifyLive`, `NonceSet`, `NewNonceSet` | `identity.VerifyLive(msg, identity.LiveOptions{Nonces, ...})`, `identity.NonceSet`, `identity.NewNonceSet` | The SDK refuses live verification without a replay set and bounds it (size, per sender). |
| `Trust` (`verified`, `claimed`, `absent`) | `identity.Trust` (`TrustVerified`, `TrustClaimed`, `TrustAbsent`), `identity.TrustOf(sender)` | |
| `TrustForged`, `TrustMalformed` | the `*identity.Error` of a failed verification, matching `identity.ErrVerification` or `identity.ErrMalformedHeader`; its `Code` is the section 9.2 status | The SDK reports a refusal as an error and not as a trust class. |
| `TrustUnknownSigner` | `caller.Client.ResolveSender` / `host.Request.ResolveSender` (`identity.SenderResolver`) returning `ok == false` | Whether an unregistered signer is acceptable is the program's policy. |
| `TrustStale` | `identity.VerifyStored` plus a comparison of `Sender.Header.TS` with the arrival time | A stale stored message verifies by design (section 13.8). |

The host side of a request (classify, replay check, 401/403) is `host.New` with `host.WithMinSenderTrust`, or
`identity.NewGate` for a program that receives messages another way (`identity.Gate.Classify`, `AdmitPrompt`).
The caller side signs with `caller.WithIdentity`.

## 2. What stays in mash

Each item is outside SAP 0.3. The SDK is not changed for it. Where the SDK has a generic extension point a
follow-on rebuild would use, it is named; "none needed" means the SDK's ordinary API is enough.

| mash piece | Why it stays | SDK extension point it would use |
|---|---|---|
| The `mail` verb: `Subjects.Mail`, `MailFilter`, `MailFilterZone`, `SplitMail`, `InboxStream` | `mail` is not a SAP verb, and the SDK refuses to build one (`agents.ParseSubject` rejects it). | Mail is published by mash's own code with a signed header from `caller.Client.SignSender` (or `PublishSigned`), so the signing, nonce and `Nats-Msg-Id` rules are the SDK's. For a micro endpoint on a mail subject: `host.WithEndpoint` (an extra micro endpoint, subject advertised verbatim, never interpreted). |
| Zones: `DefaultZone`, `MaxZoneLen`, `ValidZone`, `ForZone`, `ZoneOfMailFilter` | A sixth subject token on mail only. SAP defines five tokens. | None needed. Zone gating is admission policy: the verified-sender admission hook (`host.WithAdmission`, `identity.GateOptions.Accept`). |
| The stricter token rule: `MaxTokenLen`, `MaxIdentityLen`, `checkToken`, `checkTotal`, `errLegacyAgent` | SAP's MUSTs are looser. The SDK never makes a SHOULD or MAY a MUST. | None needed. mash validates first, then calls `agents.NewAgentSubject`. `agents.ValidateTokenStrict` is SAP's own SHOULDs and is a different rule. |
| `SignInstance` / `VerifyInstance` (`Mash-Sender-Instance`) | A mash header bound to the message's signed fields. | Per-instance tokens are `agents.NewAgentSubject` per instance. To sign a binding: `identity.Signer.Sign` (the interface signs arbitrary bytes under the connection key) over the fields of the signed `identity.SenderHeader` (from `caller.Client.SignSender` then `identity.ParseHeader`, or from `identity.SignHeader`). The extra header is added at publish, or by the outgoing-header hook `caller.WithRequestHeaders` when it does not depend on the signed fields. |
| `SignUrgent` / `VerifyUrgent` (`Mash-Urgent`) | As above. | As above; on receipt, `identity.Sender.Header` (verified fields) with the received-header access (`host.Request.Header`, `host.RequestInfo.Header`, `identity.Message.Header`). |
| `SignInReplyTo` / `VerifyInReplyTo` (`Mash-In-Reply-To`) | As above. | As above. |
| `SignVia` / `VerifyVia` (`Mash-Via`) | As above. | As above. |
| JetStream inboxes: `InboxPrefix`, `InboxStream`, the per-endpoint `_INBOX.<name>` rule | JetStream and mash's account permissions are not part of SAP. | `caller.WithInboxPrefix` sets the caller's reply prefix; `connect.Connection.Options` and `nats.CustomInboxPrefix` set the connection's; `host.WithReplySubjectPolicy` bounds where a host may publish a query. |
| Directory keys: `Flat`, `FromFlat`, `Overlaps`, `KeyFromSubject` | mash's directory and admission conventions. | `agents.ParseSubject`, `agents.AgentSubject` accessors, `agents.FilterSubject`. |

### Extension points the rebuild can rely on

These are the REQ-016 points, each generic and exercised by `examples/extension-points`:

| Need | SDK |
|---|---|
| Per-instance agent, owner and name tokens | `agents.NewAgentSubject` (and `agents.WithSubjectToken`) per `host.New` |
| Extra micro endpoints | `host.WithEndpoint(host.Endpoint{...})` |
| Admission hook for verified senders | `host.WithAdmission(func(ctx, *identity.Sender, *host.RequestInfo) (bool, error))` |
| Received-header access | `host.Request.Header`, `host.RequestInfo.Header`, `caller.Event.Header()`, `identity.Message.Header` |
| Outgoing-header hook | `caller.WithRequestHeaders`; `caller.Extras.Headers` through `caller.WithInterceptors` |

One limit to know. The outgoing-header hook runs before the message is signed, and it may not set a header SAP
defines (`caller.ReservedHeader`). A header that must be signed over the final nonce and timestamp therefore
cannot be added to a prompt by the hook. From what `internal/sap` shows, mash puts such headers on messages it
publishes itself (mail), where it signs first with `SignSender` and adds them. If mash needs them on prompts
too, that is a question for the follow-on contract, and the place for an answer is a hook that sees the final
signed header, not a mash-specific API.

## 3. Checks this file relies on

- The pieces named in section 1 exist, with those names, in the SDK (`go doc -all`).
- mash is unchanged: this mapping is a reading of `origin/main` and edits nothing there.
- The SDK holds no mash name or identifier: `.github/workflows/go-sdk.yml` fails if any file but this one
  contains one.
