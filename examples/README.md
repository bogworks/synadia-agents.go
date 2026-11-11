# Examples

Runnable programs that use only the standard library and this SDK. Each is its own `main` package;
`go build ./examples/...` builds them all, and `test/examples` runs them against an embedded server.

| Program | Shows |
|---|---|
| [`host-echo`](host-echo) | the smallest host: answer every prompt |
| [`caller-basic`](caller-basic) | discover, status, prompt, read the stream, answer a question |
| [`host-query`](host-query) | an agent that asks the caller a question mid-stream (SAP §7) |
| [`signed`](signed) | the sender identity of SAP §13 on both sides |
| [`extension-points`](extension-points) | every extension point the SDK offers a program built on it |

Start any NATS server (`nats-server`), then run a host in one terminal and a caller in another.
