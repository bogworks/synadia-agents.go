# Secure configuration

In v0.1.0 none of these settings is on by default: you opt in to each one.

This page lists the options that make a caller and a host refuse senders and agents they cannot verify. The defaults are open: a host serves any sender, and a client trusts any discovery reply.

The SDK carries no security stance. It exposes the checks of SAP as separate options, and the posture of a program, which of them to turn on, belongs to the application. [ADR-0006](decisions/0006-the-sdk-carries-no-security-stance-a-posture-belongs-to-the-application.md) records this. A program that wants every check sets each option below itself. The table lists them:

| Check | Option | Default |
|---|---|---|
| Sign requests and heartbeats | `connect.WithSignedIdentity`, then `host.WithIdentity` or `caller.WithIdentity` with the `Signer` | no signer |
| Host serves only signed senders | `host.WithMinSenderTrust(identity.TrustSigned)` | `identity.TrustAny` |
| Host decides which senders may prompt | `host.WithAdmission` | every sender the trust level admits |
| Client trusts only verified agents | `caller.WithTrustPolicy` (Go only) | every discovery reply |
| Tracker verifies heartbeats | `caller.WithHeartbeatVerification` (through `caller.WithTracker`) (Go only) | verifies nothing |

**Go only.** `caller.WithTrustPolicy` and `caller.WithHeartbeatVerification` have no counterpart in the TypeScript and Python SDKs. Their discovery records report whether `id_sig` verified, but offer no filter, allowlist or refusal at prompt time, and their trackers do not verify heartbeats. SAP requires neither: which verified senders to accept remains the deployment's decision (SAP §10.1), and §13.11 requires no subscriber to verify heartbeats. A program that must behave the same in every language makes these checks in its own code.
| Operator attests service imports | `host.WithOperatorAttested(true)` | off |

The examples are in `host/example_secure_test.go` and `caller/example_secure_test.go`. They need a NATS server, so they compile but do not run in the root module. The `TestSecureExamples_*` tests in `test/examples` run the same code against an embedded server, and `TestSecureExamples_MatchRunners` fails if an example and the code run diverge.

## Caller: trust policy (Go only)

By default a client does not verify the agents it discovers. `caller.WithTrustPolicy` makes verified discovery the default:

```go
c, err := caller.New(nc, caller.WithTrustPolicy(caller.TrustPolicy{
	Agents: trusted, // []identity.AgentID
	Accept: func(id identity.AgentID, info agents.AgentInfo) bool {
		return info.Owner == "example"
	},
}))
```

`Client.Discover`, `Client.Find`, `Client.ListAgents` and `Client.LookupInstance` then return only records whose `id_sig` verifies over the record's own prompt subject and whose signer passes the policy. `Agents` is an allow list of signers. `Accept`, when set, must also return true and must not block. A policy with neither field is a configuration error. Dropped records are counted in `Stats.RejectedRecords`. `Agent.Prompt` and `Agent.Status` return `caller.ErrUntrustedAgent` for an agent discovery did not admit.

The policy checks who registered the agent. A valid `id_sig` identifies the registrant. It does not prove that the discovery reply came from them, and it says nothing about what the agent does.

## Host: sender trust and admission

`host.WithMinSenderTrust(identity.TrustSigned)` makes the prompt endpoint serve only requests with a valid signature. Unsigned and claimed senders are refused with 401. The default is `identity.TrustAny`. A host also needs `host.WithIdentity` to register its own identity.

A verified sender is not an authorized sender. `host.WithAdmission` takes an `AcceptSenderFunc` that decides which senders may prompt:

```go
host.WithAdmission(func(ctx context.Context, s *identity.Sender, _ *host.RequestInfo) (bool, error) {
	return s != nil && s.Trust == identity.TrustVerified && slices.Contains(allowed, s.ID), nil
})
```

The hook runs for every classified prompt request and never for status. Returning false is answered 403 for a verified sender and 401 for a claimed or absent one. Returning an error is answered 500 and the request is not served. `s` is nil for an absent sender. A claimed sender has no `ID`, so never grant on one. See `ExampleWithAdmission`.

## Caller: heartbeat verification (Go only)

By default the heartbeat tracker verifies nothing. `caller.WithHeartbeatVerification(opts, requireSigned)` makes it verify the `Agent-Sender` header of every heartbeat as a live message and discard a heartbeat whose signature fails. With `requireSigned`, a heartbeat that is not verified (absent or claimed) is discarded too. `opts` must carry a replay set (`identity.LiveOptions.Nonces`). A client passes it to its tracker with `caller.WithTracker`.

## Signing

`connect.WithSignedIdentity` makes `connect.ResolveBundle` return a `Signer` made from the user seed of the same credential read as the connection, and fails with `connect.ErrNoUserSeed` when the source holds none. Pass `Bundle.Signer` to `host.WithIdentity` or `caller.WithIdentity`. `host.New` and `caller.New` do not check that a signer was given. A caller that has none refuses locally to send to an endpoint that requires a signed sender.

## Extra endpoints are outside the gate

An endpoint added with `host.WithEndpoint` does not pass through `WithMinSenderTrust`, `WithAdmission` or the replay set. Its `Handler` receives the raw request. It must do its own signature verification, replay protection and sender checks.

`identity.VerifyLive` classifies a request and verifies its `Agent-Sender` header. It requires an `identity.NonceSet` in `identity.LiveOptions.Nonces`, records the nonce atomically after the signature verifies, and refuses a replay with 401. `identity.TrustOf` returns the trust class. To map a verified sender back to a registered agent, use `identity.NewSenderResolver` with `identity.ConnLister`. `identity.NewGate` and `Gate.AdmitPrompt` apply the same order of checks as the host, including a minimum trust and an admission function, if a handler needs them.

```go
sender, err := identity.VerifyLive(identity.Message{
	Subject: req.Subject(), Data: req.Data(), Header: nats.Header(req.Headers()),
}, identity.LiveOptions{Nonces: nonces})
if err != nil || identity.TrustOf(sender) != identity.TrustVerified {
	_ = req.Error("401", "signature required", nil)
	return
}
if !slices.Contains(allowed, sender.ID) {
	_ = req.Error("403", "sender refused", nil)
	return
}
```

Create one `NonceSet` with `identity.NewNonceSet` and share it among the handlers. See `ExampleWithEndpoint`.

## Operator attestation

`host.WithOperatorAttested(true)` asserts that every request reaching the host crossed a service import. The server's stamp (an inserted account token, or `Nats-Request-Info`) is then taken as the server's, not a peer's. The host cross-checks it against the signed header, refuses a mismatch with 401, and sets `Sender.AccountAttested` on agreement. It is off by default.

The SDK cannot verify the claim. If a request can reach the host without crossing an import, a peer can forge the stamp. The operator is responsible for the NATS account setup the claim describes. Endpoints that call `identity.VerifyLive` set `LiveOptions.OperatorAttested` under the same condition.

## Related settings

- `connect.WithInsecureKeyFilePermissions` is off by default. On Unix, a creds, seed or TLS key file that group or others can access is refused. Use the option only where the file system cannot hold modes.
- `tools.WithAttachmentRoots` names the directories files may be sent from. By default there are none, so only returned files can be sent.
- `host.WithReplySubjectPolicy` vets the reply subject of each prompt and status request. The default refuses `$` subjects, wildcards and protocol subjects, and drops the request unanswered. A caller chooses where the host replies, so limit the host's NATS publish permissions to the inboxes it must answer.
