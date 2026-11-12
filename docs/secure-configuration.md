# Secure configuration

In v0.1.0 none of these settings is on by default: you opt in to each one. A [security stance](#security-stance-strict-and-relaxed) turns the main ones on together.

This page lists the options that make a caller and a host refuse senders and agents they cannot verify. The defaults are open: a host serves any sender, and a client trusts any discovery reply.

The examples are in `host/example_secure_test.go` and `caller/example_secure_test.go`. They need a NATS server, so they compile but do not run in the root module. The `TestSecureExamples_*` tests in `test/examples` run the same code against an embedded server, and `TestSecureExamples_MatchRunners` fails if an example and the code run diverge.

## Caller: trust policy

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

## Security stance: strict and relaxed

A stance bundles four identity checks under one name, so a program chooses its posture once. `identity.Stance` holds a `Profile` and an optional override for each check. Each package takes it through its own option: `host.WithStance`, `caller.WithStance`, `caller.WithTrackerStance` and `connect.WithStance`.

| Check | Setting | Where | What it does |
|---|---|---|---|
| Signed senders | `SignedSenders` | host | The prompt endpoint's `min_sender_trust` becomes `signed` (SAP §13.9). An unsigned prompt is refused with 401. |
| Verified discovery | `VerifiedDiscovery` | caller | The client trusts any agent whose `id_sig` verifies, as a trust policy that accepts every verified signer does. It gates `Agent.Prompt` and `Agent.Status` with `caller.ErrUntrustedAgent` too. |
| Heartbeat verification | `HeartbeatVerification` | heartbeat tracker | The tracker verifies every heartbeat that carries an `Agent-Sender` header and discards one that fails. A heartbeat with no header is kept. |
| Signing | `Signing` | connect, caller, host | `connect.ResolveBundle` yields the signer when the credentials hold a user seed. `caller.New` and `host.New` fail unless `WithIdentity` gave a `Signer`. |

`identity.Strict` turns all four on. `identity.Relaxed` turns all four off, which is exactly v0.1.0. Relaxed is the default, so a program that sets nothing behaves as before.

```go
s := identity.Stance{Profile: identity.Strict}
b, err := connect.ResolveBundle(ctx, src, connect.WithStance(s))
// dial b, then:
h, err := host.New(nc, subject, handler, host.WithStance(s),
	host.WithIdentity(host.IdentityOptions{Signer: b.Signer}))
c, err := caller.New(nc, caller.WithStance(s),
	caller.WithIdentity(caller.IdentityOptions{Signer: b.Signer}))
```

Signing is a separate setting so that a program can sign without refusing anyone: `Profile: identity.Relaxed, Signing: identity.On` signs, requires a signer, and still serves unsigned senders. The reverse works too: `Profile: identity.Strict, Signing: identity.Off` lets `New` succeed without a signer, which suits a program that only verifies. A signer that the profile alone asks for is not an error on a bundle: a source with no user seed yields no signer. Only `connect.WithSignedIdentity` or `Signing: identity.On` fails with `connect.ErrNoUserSeed`.

Each of the four checks can be overridden with `identity.On` or `identity.Off`. `identity.Inherit`, the zero value, takes the check from the profile. A value outside the constants is refused by the option.

An explicit option always wins over the stance for its own check, whatever the order of the options: `host.WithMinSenderTrust`, `caller.WithTrustPolicy`, `caller.WithHeartbeatVerification` and `connect.WithSignedIdentity`. `caller.New` passes its stance to the tracker it builds unless `caller.WithTracker` sets one.

When a program chooses no stance and sets no explicit option for the check (`WithMinSenderTrust` on a host, `WithTrustPolicy` on a client), a logger set with `WithLogger` gets one warning per process that says the default profile applies. The SDK never logs to the default logger, so a program with no logger sees nothing.

The default profile is relaxed today, and it may change in a later release. Pass a stance to be sure of the posture you get. See [ADR-0003](decisions/0003-a-security-stance-selects-strict-or-relaxed-identity-checks.md).

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
