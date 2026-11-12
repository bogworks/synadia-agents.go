# synadia-agents.go

A Go SDK for the [Synadia Agents Protocol (SAP)](https://github.com/synadia-ai/synadia-agent-sdk-docs) 0.3.

It covers both sides of the protocol:

- a **caller**, which discovers agents, prompts them and reads their streamed replies;
- a **host**, which serves an agent on NATS and answers those prompts;
- the **sender identity** of SAP section 13, which signs and verifies the identity carried on each message (package `identity`);
- a **conformance suite** (package `conformance`), a catalog that ties each SAP requirement to the tests that show it.

## Status of this project

This is an independent implementation. It is written to be offered upstream to Synadia, and it is not published or endorsed by Synadia.

## Supported versions

| | |
|---|---|
| SAP revision | 0.3, spec commit `c48cea5f07965cc8fd4054e120459f1833b3ed2b` (`c48cea5f`) |
| Go | 1.26 or later (the `go` directive is 1.26); CI builds with 1.27.4 and also tests 1.26.x |
| Runtime dependencies | `github.com/nats-io/nats.go` and `github.com/nats-io/nkeys` only |

## Install

```sh
go get github.com/bogworks/synadia-agents.go
```

```go
import agents "github.com/bogworks/synadia-agents.go"
```

The module path is provisional and changes if the SDK moves upstream.

> **Security:** in v0.1.0 the defaults are open. A host serves any sender, and a client trusts any
> discovery reply. Sender verification, admission and the agent trust policy are opt-in. See
> [Secure configuration](docs/secure-configuration.md) before you run this outside a trusted network.

## Quick start

The quick start is [`Example_quickStart`](https://pkg.go.dev/github.com/bogworks/synadia-agents.go/caller#example-package-quickStart)
in package `caller`. It serves an echo agent, discovers it, prompts it and reads the reply as it streams. It needs a
NATS server at `nats://127.0.0.1:4222`. A test in the `test/` module runs the same code against an embedded server, and
checks that the code below is the Example verbatim:

```go
func Example_quickStart() {
	nc, err := nats.Connect(nats.DefaultURL)
	if err != nil {
		log.Fatal(err)
	}
	defer nc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Host: register an agent. The host sends the ack first and the
	// terminator last.
	subject, err := agents.NewAgentSubject("echo", "me", "default")
	if err != nil {
		log.Fatal(err)
	}
	h, err := host.New(nc, subject, func(ctx context.Context, req *host.Request, w *host.Responder) error {
		return w.Send(ctx, "echo: "+req.Envelope.Prompt)
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := h.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer h.Close()

	// Caller: discover the agent (SAP section 4), prompt it, read the stream.
	c, err := caller.New(nc)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	agent, err := c.Find(ctx, caller.Filter{Agent: "echo"})
	if err != nil {
		log.Fatal(err)
	}
	stream, err := agent.PromptText(ctx, "hello")
	if err != nil {
		log.Fatal(err)
	}
	defer stream.Close()
	for ev, err := range stream.Events() { // ack, response and query events
		if err != nil {
			log.Fatal(err) // *agents.ServiceError, a timeout, ...
		}
		if r, ok := ev.(caller.Response); ok {
			fmt.Print(r.Text)
		}
	}
}
```

More runnable examples are on pkg.go.dev, and run under `go test`:
[`agents`](https://pkg.go.dev/github.com/bogworks/synadia-agents.go#pkg-examples) (subjects, envelopes, chunks, sizes, errors),
[`identity`](https://pkg.go.dev/github.com/bogworks/synadia-agents.go/identity#pkg-examples) and
[`connect`](https://pkg.go.dev/github.com/bogworks/synadia-agents.go/connect#pkg-examples).

A host registers an agent and answers prompts. The host sends the ack first and the terminator last:

```go
package main

import (
	"context"
	"log"
	"os"
	"os/signal"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/nats-io/nats.go"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	nc, err := nats.Connect(nats.DefaultURL)
	if err != nil {
		log.Fatal(err)
	}
	defer nc.Close()

	subject, err := agents.NewAgentSubject("echo", "me", "default")
	if err != nil {
		log.Fatal(err)
	}
	h, err := host.New(nc, subject, func(ctx context.Context, req *host.Request, w *host.Responder) error {
		return w.Send(ctx, "echo: "+req.Envelope.Prompt)
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := h.Start(ctx); err != nil {
		log.Fatal(err)
	}
	<-ctx.Done()
	_ = h.Shutdown(context.Background())
}
```

More in [`examples/`](examples/): a caller, an echo host, a host that asks a question mid-stream,
signed identity on both sides (SAP section 13) and the SDK's extension points. Every identifier is mapped to
its SAP section in [`docs/protocol-mapping.md`](docs/protocol-mapping.md), and every feature of the official
SDKs to its Go counterpart in [`docs/parity.md`](docs/parity.md).
For the options that refuse unverified senders and agents, see [`docs/secure-configuration.md`](docs/secure-configuration.md).

## Conformance

The conformance catalog is in [`conformance/`](conformance/). It lists each SAP 0.3 requirement and
the tests that cover it, and `go test ./conformance/... -run TestCatalog` checks that the catalog and
the tests agree. The command `sapconform` ([`cmd/sapconform`](cmd/sapconform)) runs the host-side checks of
the catalog against a live agent:

```sh
go run github.com/bogworks/synadia-agents.go/cmd/sapconform --context my-nats
```

Interoperability with the official TypeScript and Python SDKs is tested in [`test/interop`](test/interop):
the Go caller against the official hosts, and the official callers against the Go host.
This README will state the catalog result for each release.

## Development

```sh
go test -race ./...
(cd test && go test -race ./...)
```

The integration tests, which start an embedded NATS server, live in the separate `test/` module so
that the SDK itself keeps to its two dependencies. See [CONTRIBUTING.md](CONTRIBUTING.md) for the
rules, [SECURITY.md](SECURITY.md) to report a vulnerability, and [CHANGELOG.md](CHANGELOG.md) for
what changed.

## Licence

Apache License 2.0. See [LICENSE](LICENSE).
