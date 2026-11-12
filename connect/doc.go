// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Package connect holds opt-in helpers that turn a NATS CLI context, a NATS
// URL or a credentials file into [nats.Option] values, plus reconnect defaults
// suited to long-running agents.
//
// This package is OUTSIDE the Synadia Agents Protocol: SAP §10.1 delegates
// authentication and connection to NATS, and the SDK never opens a connection
// itself. The official TypeScript and Python SDKs ship the same helpers, and
// this package is their Go counterpart (parity rows 1.3 to 1.13 and 10.10,
// deviations D-1 and D-2). A program that wants only SAP does not import it.
//
// Nothing here runs unless the program calls it, and nothing is read from a
// path the program did not name: a context is read only when the program names
// it (or passes "current", which the program chose to ask for), and a context's
// key files are read only because that context names them. No key material is
// read from a fixed path. A key file must be a regular file of at most 64 KiB
// (1 MiB for a ca bundle): a FIFO, device or directory is refused without
// blocking, and a larger file is refused unread.
//
// # Context lookup
//
// The configuration directory follows the official SDKs, which follow the NATS
// CLI: $NATS_CONFIG_HOME when set (a leading "~/" is expanded), else
// $XDG_CONFIG_HOME/nats, else ~/.config/nats. A context named N is the file
// <dir>/context/N.json. The name "current" resolves through $NATS_CONTEXT, then
// the first line of <dir>/context.txt. [WithConfigDir] replaces the lookup for
// a program or test that has its own directory.
//
// # Supported context fields
//
//   - url: required. Userinfo is parsed as [ParseURL] does and is overridden
//     by any explicit auth field.
//   - creds: path of a .creds file. Highest precedence.
//   - nkey: path of a user seed file.
//   - user_jwt, user_seed: an inline user JWT, with an optional inline seed
//     that signs the server nonce. Without a seed the signature is empty and
//     the connection works only where the server does not require nonce
//     signing. A user_seed without user_jwt and without user/password, token
//     or URL userinfo authenticates as NKEY auth with that seed; beside one of
//     those the other credential is used, and SignerFromContext refuses the
//     seed.
//   - user, password: basic authentication.
//   - token: token authentication.
//   - cert, key, ca: paths of a PEM client certificate, its key and a root CA
//     file. cert and key must be given together.
//   - tls_first: when true, TLS handshake before the server's INFO.
//   - inbox_prefix: reply-inbox prefix for the connection.
//
// Precedence inside a context: creds, then nkey, then user_jwt, then
// user/password, then token. Paths beginning with "~/" expand to the home
// directory, as the NATS CLI does; any other relative path resolves against the
// working directory, not the context file's directory.
//
// Not supported, each ignored silently as in the official SDKs: nsc:// URLs
// (the SDK does not link the nsc tooling, so the URL is rejected as an
// unsupported scheme), socks_proxy (nats.go has no SOCKS dialer), the
// jetstream_* fields (not used by SAP), color_scheme and windows_* (CLI-only).
// A known field of the wrong JSON type is an error rather than being ignored,
// because ignoring a malformed credential field would silently connect
// without it (SDK policy, stricter than upstream).
//
// # Secrets
//
// No error or log line contains a seed, a credential, a token, a password or a
// URL's userinfo. [Bundle] and the signers print redacted forms. Go cannot
// guarantee zeroing of copies that nats.go or the runtime make; see
// [Bundle.Wipe].
package connect
