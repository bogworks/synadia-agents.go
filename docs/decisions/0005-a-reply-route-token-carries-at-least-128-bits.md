---
status: accepted
date: 2026-11-24
decision-makers: Project maintainers (issue #81)
---

# A reply route token carries at least 128 bits

## Context and Problem Statement

A caller's reply subject is `{prefix}.{client-id}.{route-token}`: one subscription per `Client` on `{prefix}.{client-id}.>`, and one route token per prompt, discovery or request. SAP leaves the form of a reply subject to the caller. Both tokens were 16 base32 characters, 80 bits from crypto/rand. Every request of a `Client` shares its client id, and every agent the client prompts sees it in the reply subject. So, to a party that has seen one reply subject, the only unknown part of another request's reply subject is its route token. How much randomness must a route token carry, and does the client id need any?

## Decision Drivers

* A party that has seen one reply subject must not be able to guess another and inject replies into it.
* The subject layout and the single-subscription routing stay as they are.
* No protocol change: SAP does not specify reply subjects.

## Considered Options

* Keep 16-character route tokens (80 bits).
* Route tokens of at least 128 bits; the client id unchanged (chosen).
* Lengthen both tokens.
* Drop the client-id token, one token per reply subject.

## Decision Outcome

Chosen: a route token is 26 base32 characters, 130 bits from crypto/rand (`routeTokenLen` in `caller/mux.go`). The client id stays 16 characters. It is disclosed by design, so it is not what keeps one reply stream from another. The layout and the token count are unchanged.

### Consequences

* Good, because forging a reply now needs a 130-bit guess.
* Good, because nothing that parses reply subjects changes, and SAP is untouched.
* Bad, because reply subjects are 10 characters longer.
* Neutral: agents can still link the requests of one `Client` through its client id. That is inherent in one subscription per client. A program that wants requests it cannot link uses one `Client` per request or per agent.
* Neutral: who can read replies is decided by NATS subscribe permissions, not by either token.

### Confirmation

`TestMux_RouteTokenEntropy` (caller) checks that `routeTokenLen*5 >= 128`, that each route subject is `{prefix}.{client-id}.{token}` with a token of exactly `routeTokenLen` lower-case base32 characters, and that 2000 tokens do not repeat.

## Pros and Cons of the Options

### Keep 16-character route tokens

* Good, because nothing changes.
* Bad, because 80 bits is below the common 128-bit target for an unguessable reply inbox.

### Route tokens of at least 128 bits, client id unchanged

* Good, because it closes the guessing gap at the only token that is secret.
* Bad, because subjects grow by 10 characters.

### Lengthen both tokens

* Good, because the two tokens are symmetric.
* Bad, because a longer client id buys nothing: every prompted agent sees it.

### One token per reply subject

* Good, because subjects are shorter.
* Bad, because it changes the layout and the single-subscription routing (one wildcard subscription per client), so it is a larger change than the problem needs.

## More Information

Issue #81, and its comment on the client id.
