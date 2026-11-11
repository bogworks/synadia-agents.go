// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package identity implements the optional sender identity extension of the
// Synadia Agents Protocol (SAP §13).
//
// A caller proves which NATS user sent a request by signing it with the user's
// NKEY seed and attaching the result as an Agent-Sender header (SAP §13.5,
// §13.6). A host verifies that header (SAP §13.7), classifies the sender as
// verified, claimed or absent (SAP §13.1) and applies its admission policy.
// An agent proves which NATS user registered it with an id_sig in its service
// metadata (SAP §13.10).
//
// The package has four groups of API:
//
//   - Identity values: [AgentID] (SAP §13.3), [IdentityFromUserInfoReply] and
//     [IdentityFromJWT] (SAP §13.4), and [SelfIDResolver], which learns a
//     connection's own identity and retries after a failure.
//   - Caller side: [Signer], [SignHeader], [ClaimHeader], [SenderHeader] and
//     the size helpers that let a caller count header bytes against
//     max_payload (SAP §13.5.1), and [CheckCanSend] for the local refusal of
//     SAP §13.9.
//   - Verification: [VerifyLive] (SAP §13.7) and [VerifyStored] (SAP §13.8)
//     are separate APIs with separate rules. Live verification applies the
//     timestamp window and consults a nonce set. Stored verification checks
//     authorship only.
//   - Host side: [Gate] runs the classification, the min_sender_trust check,
//     the replay set ([NonceSet]) and the admission hook, and always leaves
//     status requests answerable.
//
// The package depends on github.com/nats-io/nkeys and, for message and
// connection types, github.com/nats-io/nats.go. It does not read key material
// from fixed paths and never writes any. To rotate a key, restart the instance
// with the new key (SDK policy); there is no live rotation.
//
// Seeds, signatures and nonces are never logged.
package identity
