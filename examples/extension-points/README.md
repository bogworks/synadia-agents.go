# extension-points

Uses every extension point the SDK offers a program built on it. The "tenant" convention is invented
for the example and is not part of the protocol.

| Extension point | Used as |
|---|---|
| per-instance tokens | two instances with their own owner and name, and `agents.WithSubjectToken` |
| extra micro endpoint | `host.WithEndpoint`, advertised in the discovery record, never interpreted |
| verified-sender admission hook | `host.WithAdmission` decides who may prompt (here by a header, as the example has no keys) |
| received-header access | `host.Request.Header` in the handler |
| outgoing-header hook | `caller.WithRequestHeaders` adds the header to every prompt |

```sh
go run ./examples/extension-points -url nats://127.0.0.1:4222
```

The program prompts each instance twice: once with the hook installed (served) and once without it
(refused, 401).
