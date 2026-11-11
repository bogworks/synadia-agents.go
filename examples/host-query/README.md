# host-query

An agent (`quiz`/`example`/`default`) that asks the caller `continue? (yes/no)` in the middle of its
reply (SAP §7) and goes on according to the answer. If no answer comes it carries on with a default.

```sh
go run ./examples/host-query -url nats://127.0.0.1:4222
go run ./examples/caller-basic -agent quiz -prompt job -answer yes
```

It uses `Responder.Ask` and `host.ErrQueryTimeout`; the caller side answers with `Query.Reply`.
