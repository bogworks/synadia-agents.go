# signed

The sender identity of SAP §13 on both sides. The host advertises and enforces
`min_sender_trust: signed` and registers its own signed identity; the caller signs every prompt and
status request. The connection must authenticate with a user NKEY (a seed file, a `.creds` file or a
NATS CLI context), because the signature is made with the key the server knows the user by.

```sh
go run ./examples/signed -url nats://127.0.0.1:4222 -nkey ./user.nk            # both sides in one process
go run ./examples/signed -mode host   -context my-context
go run ./examples/signed -mode caller -context my-context
```

It uses `connect.ResolveBundle` with `WithSignedIdentity` (one read of the credentials gives both the
connection options and the signer), `host.WithIdentity`, `host.WithMinSenderTrust` and
`caller.WithIdentity`. The seed is read from the file you name and is never printed.
