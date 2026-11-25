---
status: accepted
date: 2026-11-25
decision-makers: Project maintainers (issue #81)
---

# The client id is shared by a client's requests and is not a secret

## Context and Problem Statement

[ADR-0005](0005-a-reply-route-token-carries-at-least-128-bits.md) gives each reply subject the layout `{prefix}.{client-id}.{route-token}` and keeps the client id at 16 characters, shared by every request of one `Client`. Every agent that a client prompts sees that client id. A security read asked whether a shared, disclosed client id is a risk. If it is not, should it stay as it is?

## Decision Drivers

* A party that has seen the client id must not be able to answer, or read, another request's replies.
* One wildcard subscription per client, and the reply layout, stay as they are.
* No protocol change: SAP does not specify reply subjects.

## Considered Options

* Keep one shared client id per `Client` (chosen).
* Make the client id secret, with a fresh one per request.

## Decision Outcome

Chosen: keep one shared client id per `Client`, and treat it as public. A party that has seen it can do two things: name the client's reply namespace, and link the client's requests to one another. It cannot answer a request. A NATS publish reaches only an exact subject, so it would have to name a 130-bit route token that it has not seen, and the mux drops and counts a reply whose token matches no route. It cannot read the replies either, unless NATS grants it a subscription on that namespace. The secret part of a reply subject is the route token. The client id only groups one client's routes under one subscription.

### Consequences

* Good, because the layout and the single subscription per client are unchanged.
* Good, because a later security read finds the reason recorded here and does not need to reopen it.
* Bad, because agents can link the requests of one `Client`. A program that wants requests that cannot be linked uses one `Client` per request or per agent, as ADR-0005 says.
* Neutral: who can read replies is decided by NATS subscribe permissions, not by either token.

### Confirmation

`TestMux_RoutesByTokenAndCountsStrays` (caller) checks that a reply whose token matches no route is dropped and counted in `Stats.DroppedReplies`. `TestMux_RouteTokenEntropy` (caller) checks that a route token carries at least 128 bits.

## Pros and Cons of the Options

### Keep one shared client id per `Client`

* Good, because nothing changes, and the security rests on the token that is secret.
* Bad, because a client's requests can be linked.

### A secret client id, fresh per request

* Good, because requests cannot be linked.
* Bad, because it needs one subscription per request, or a different layout, which changes the routing that ADR-0005 kept.
* Bad, because it adds nothing against forged or read replies: the route token and NATS permissions already prevent both.

## More Information

Issue #81 and ADR-0005. The v0.1.1 release evidence, `docs/releases/v0.1.1.md`, gives the same reason.
