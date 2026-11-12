# host-echo

Serves an agent (`echo`/`example`/`default`) that answers each prompt with `echo: <prompt>`.

```sh
go run ./examples/host-echo -url nats://127.0.0.1:4222
```

It uses `agents.NewAgentSubject`, `host.New`, `Host.Start` and `Responder.Send`. The host sends the ack
and the terminator itself (SAP §6). Interrupt it to stop; `Host.Shutdown` drains the service.
