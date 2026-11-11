# caller-basic

Discovers agents, reads one agent's status, prompts it and prints the streamed reply. It answers any
question the agent asks with `-answer`.

```sh
go run ./examples/caller-basic -url nats://127.0.0.1:4222 -agent echo -prompt "hello"
```

It uses `caller.New`, `Client.Discover`, `Client.Find`, `Agent.Status`, `Agent.PromptText` and
`Stream.Events`. A remote failure arrives as an `*agents.ServiceError`.
