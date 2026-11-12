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

## Security stance

A stance gathers the identity checks in one value, so a program chooses its posture once. `identity.Stance` holds one setting per check, and the SDK never interprets a named profile: the program, or the deployment around it, decides its policy and passes concrete settings. Each package takes the stance through its own option: `host.WithStance`, `caller.WithStance`, `caller.WithTrackerStance` and `connect.WithStance`.

Each setting is `identity.On`, `identity.Off` or `identity.Unset`. `Unset`, the zero value, means the SDK default of that check applies.

| Check | Setting | Default | Where | What it does |
|---|---|---|---|---|
| Signed senders | `SignedSenders` | `identity.DefaultSignedSenders` (off) | host | The prompt endpoint's `min_sender_trust` becomes `signed` (SAP §13.9). An unsigned prompt is refused with 401. |
| Verified discovery | `VerifiedDiscovery` | `identity.DefaultVerifiedDiscovery` (off) | caller | The client trusts any agent whose `id_sig` verifies, as a trust policy that accepts every verified signer does. It gates `Agent.Prompt` and `Agent.Status` with `caller.ErrUntrustedAgent` too. |
| Heartbeat verification | `HeartbeatVerification` | `identity.DefaultHeartbeatVerification` (off) | heartbeat tracker | The tracker verifies every heartbeat that carries an `Agent-Sender` header and discards one that fails. A heartbeat with no header is kept. |
| Signing | `Signing` | `identity.DefaultSigning` (off) | connect, caller, host | The program signs. `connect.ResolveBundle` yields the signer from the credentials' user seed, as `connect.WithSignedIdentity` does, and fails with `connect.ErrNoUserSeed` when there is none. `Unset` and `Off` yield no signer. |
| Require signer | `RequireSigner` | `identity.DefaultRequireSigner` (on) | caller, host | With signing on and no `Signer` in `WithIdentity`, `On` makes `caller.New` and `host.New` fail, and `Off` lets them succeed and log one warning to the logger set with `WithLogger`. It has no effect when signing is off, and none on connect. |

```go
s := identity.Stance{
	SignedSenders:         identity.On,
	VerifiedDiscovery:     identity.On,
	HeartbeatVerification: identity.On,
	Signing:               identity.On,
}
b, err := connect.ResolveBundle(ctx, src, connect.WithStance(s))
// dial b, then:
h, err := host.New(nc, subject, handler, host.WithStance(s),
	host.WithIdentity(host.IdentityOptions{Signer: b.Signer}))
c, err := caller.New(nc, caller.WithStance(s),
	caller.WithIdentity(caller.IdentityOptions{Signer: b.Signer}))
```

Signing is separate from the checks so that a program can sign without refusing anyone: `Signing: identity.On` alone signs, needs a signer, and still serves unsigned senders. The reverse works too: `SignedSenders: identity.On` with signing off needs no signer, which suits a program that only verifies. A program that signs only when it can sets `RequireSigner: identity.Off`: `New` succeeds without a signer and logs one warning, and a caller then fails each request to an endpoint that requires signed senders with `identity.ErrCannotSign`.

`Stance.Resolved` returns the stance with every `Unset` replaced by its default, which is the way to print the settings in force. A value outside the constants is refused by the option.

An explicit option always wins over the stance for its own check, whatever the order of the options: `host.WithMinSenderTrust`, `caller.WithTrustPolicy`, `caller.WithHeartbeatVerification` and `connect.WithSignedIdentity`. `caller.New` passes its stance to the tracker it builds unless `caller.WithTracker` sets one.

When a program sets no check and no explicit option for the check (`WithMinSenderTrust` on a host, `WithTrustPolicy` on a client), a logger set with `WithLogger` gets one warning per process that says the SDK defaults apply. The SDK never logs to the default logger, so a program with no logger sees nothing.

The defaults are those of v0.1.0 today, except `RequireSigner`, and they may change in a later release. Set a check to be sure of the posture you get. See [ADR-0003](decisions/0003-a-security-stance-holds-concrete-per-check-settings.md).

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
