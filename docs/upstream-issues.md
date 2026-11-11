# Candidate upstream issues

This is a list of candidate issues for Synadia, found while building this SDK against SAP 0.3 and the
official SDKs. **Nothing here has been filed.** Randy Bias decides which to file, when, and where.

Citations: `SAP:Lnnn` is a line of `core-protocol.md` at spec commit `c48cea5f07965cc8fd4054e120459f1833b3ed2b`.
`upstream file:line` refers to the `synadia-agents` repository at commit `a1bfb47`. Every citation was checked
against that source. Summaries are in our own words; the spec is not quoted at length.

Each issue is marked one of: **spec ambiguity** (the text allows two readings, or is silent where an
implementer must choose), **spec/SDK inconsistency** (the spec and the official SDKs disagree), or
**SDK bug**.

| # | Title | Kind |
|---|-------|------|
| 1 | Instance name is the 5th token, or the 4th | spec ambiguity |
| 2 | `attachments_ok` is a boolean, or the string "true" | spec ambiguity |
| 3 | Owner subject token: SHOULD match, or matches | spec ambiguity |
| 4 | Empty `prompt` in a JSON envelope: who rejects it | spec ambiguity |
| 5 | Plain-text discrimination: whitespace, leading whitespace, "{"-prefixed text | spec ambiguity |
| 6 | `max_payload`: unit size, and whether headers count | spec ambiguity |
| 7 | Oversize requests: agent MAY enforce, or checklist "rejects" | spec ambiguity |
| 8 | Heartbeat optional fields vs checklist "all §8.3 fields" | spec ambiguity |
| 9 | Subscribe-before-ping: SHOULD, or checklist requirement | spec ambiguity |
| 10 | `status` request with a failing signature: 401 or never rejected | spec ambiguity |
| 11 | Version compatibility: MINOR is compatible, or pin exactly in 0.x | spec ambiguity |
| 12 | Stale "v0.1" wording in a 0.3 document | spec ambiguity |
| 13 | Caller behaviour on a non-JSON response chunk | spec ambiguity |
| 14 | Error code when a mid-stream query times out | spec ambiguity |
| 15 | `min_sender_trust` in the §8.3 heartbeat example | spec ambiguity |
| 16 | `$G` account token vs "tokens MUST NOT begin with `$`" | spec ambiguity |
| 17 | "Do NOT rely on the framework default queue group" is not a keyword form | spec ambiguity |
| 18 | "Exactly one ack" vs periodic ack keep-alives in both official hosts | spec/SDK inconsistency |
| 19 | Python host answers invalid base64 with 500, not 400 | SDK bug |
| 20 | Python `interval_s` is an `int` | SDK bug |
| 21 | Python client flattens service errors to `ProtocolError` text | SDK bug |
| 22 | AgentTools result schemas do not admit the `{"error"}` refusal | SDK bug |

One candidate from the conformance catalog author was dropped after checking the spec: "claimed sender on a
signed endpoint (SAP:L946, SAP:L971)". The text is consistent there: a refused claimed or absent sender is
`401` (nothing was authenticated) and a refused verified sender is `403`. There is no ambiguity to report.

---

## 1. Instance name is the 5th token, or the 4th

**Kind:** spec ambiguity (internal contradiction).

**Spec.** The subject is `agents.{verb}.{agent}.{owner}.{name}` (SAP:L50), so the instance name is the 5th token.
SAP:L169 and SAP:L515 say so ("5th token"). SAP:L222 (filter on the owner and "the 4th token") and SAP:L230
(derive the instance name from "the 4th token of the endpoint's subject") say 4th. In the verb-first layout the
4th token is the owner.

**Observed.** The 4th-token wording is the 0.2 layout (`agents.{a}.{o}.{n}`, SAP:L658) left behind by the move to
verb-first in 0.3.

**Impact.** An implementer who follows SAP:L230 derives the owner as the instance name.

**Proposed resolution.** Change "4th" to "5th" at SAP:L222 and SAP:L230. At SAP:L222 the filter should name the
3rd, 4th and 5th tokens.

## 2. `attachments_ok` is a boolean, or the string "true"

**Kind:** spec ambiguity.

**Spec.** The endpoint metadata table types `attachments_ok` as a boolean (SAP:L89, SAP:L92) and the examples at
SAP:L86 and SAP:L989 write `true`. The heartbeat example writes `"attachments_ok": "true"`, a string (SAP:L498).
The same example also carries `min_sender_trust` (see issue 15).

**Observed.** NATS micro endpoint metadata is a string-to-string map, so a registered endpoint can only carry the
string. A heartbeat copies the metadata "verbatim" (SAP:L513), so it carries the string too.

**Impact.** A reader cannot tell whether a caller must accept both forms, or which form a host must write. A
strict boolean parser rejects every heartbeat that copies a registration.

**Proposed resolution.** State that metadata values are strings on the wire, that the boolean in the table means
the strings `"true"` and `"false"`, and that callers accept exactly those. Make the examples at SAP:L86 and
SAP:L989 agree.

## 3. Owner subject token: SHOULD match, or matches

**Kind:** spec ambiguity.

**Spec.** The owner token in the subject "SHOULD match `metadata.owner`" (SAP:L58). The metadata table says
`owner` "Matches the 4th subject token" (SAP:L165), in the indicative.

**Impact.** A conformance check cannot decide whether a mismatch is a failure, a warning, or fine.

**Proposed resolution.** Use one strength at both places. If a mismatch must stay legal (for example a subject
override), say what a caller does with it: which of the two is the owner.

## 4. Empty `prompt` in a JSON envelope: who rejects it

**Kind:** spec ambiguity.

**Spec.** The `prompt` field "MUST be non-empty" (SAP:L258). A zero-byte payload "MUST be rejected with `400`"
(SAP:L284). The error taxonomy lists "empty payload" under `400` (SAP:L599) and the checklist says an agent rejects
"empty payloads" (SAP:L673). Nothing says what an agent does with `{"prompt":""}`, or whether the MUST binds the
sender, the agent, or both.

**Observed.** The official SDKs differ. The TypeScript envelope decoder refuses an empty `prompt` as a protocol
error (`client-sdk/typescript/src/prompt/envelope.ts:170-173`, whose comment says the reference agents return
`400` on empty). The Python `Envelope` declares `prompt: str` with no minimum length
(`client-sdk/python/src/synadia_ai/agents/envelope.py:84`) and `decode` rejects only a zero-byte payload
(`envelope.py:117-143`). So a Python host accepts `{"prompt":""}` and a TypeScript host answers `400`. The
Python client does refuse an empty prompt locally (`client-sdk/python/src/synadia_ai/agents/errors.py:88-92`,
"§5.1 requires non-empty text").

**Impact.** Interop differs on the same input, and a conformance test cannot assert either result.

**Proposed resolution.** Say that an agent rejects a JSON envelope whose `prompt` is empty with `400`, state
whether a whitespace-only prompt counts as empty, and add it to the taxonomy row and the checklist.

## 5. Plain-text discrimination: whitespace, leading whitespace, "{"-prefixed text

**Kind:** spec ambiguity.

**Spec.** The agent skips leading whitespace, defined as the four bytes `0x09 0x0A 0x0D 0x20` (SAP:L280). If the
next byte is `{` it parses JSON and answers `400` on failure or on a missing `prompt` (SAP:L281). Otherwise it
promotes "the original payload" to the prompt (SAP:L282).

Open points:

- Whether the promoted prompt keeps its leading whitespace ("original payload") or has it stripped.
- A payload of only whitespace is not zero bytes (SAP:L284) but has no next byte. It reaches step 3 and becomes a
  prompt that is non-empty by bytes and empty by content (see issue 4).
- Other Unicode whitespace (for example U+00A0, or a BOM) is not skipped, so a payload that starts with a BOM
  and `{` is text.
- Text that begins with `{` and is not JSON (for example `{curly braces} are...`) is a `400` by the letter of
  SAP:L281. The shorthand therefore cannot carry such text, and there is no escape.

**Impact.** Two compliant hosts can hand different prompt text to the application, and a user who types a
prompt starting with `{` gets a `400` with no way around it other than a JSON envelope.

**Proposed resolution.** State that the promoted prompt is the payload unchanged, that a payload with only
whitespace is treated as empty, and that the `{` rule is deliberate (and say so, naming the JSON envelope as the
way to send such text).

## 6. `max_payload`: unit size, and whether headers count

**Kind:** spec ambiguity.

**Spec.** `max_payload` is "a positive integer followed by `B`, `KB`, `MB`, or `GB`" (SAP:L91) and is the
"maximum single-message request payload size". The caller compares the "final encoded payload byte size" with it
(SAP:L291). SAP:L887 (§13.5.1) says headers count toward the server's `max_payload` and that callers include the
header bytes in "the local check of §5.4".

Open points: whether `KB` is 1000 or 1024 bytes, and whether the endpoint's `max_payload` (a different quantity
from the server's) counts headers, since SAP:L291 says payload and SAP:L887 says headers too. The two limits are
both called `max_payload`.

**Impact.** A request near the limit passes in one SDK and fails in another. A caller that counts headers (as
SAP:L887 requires of a §13 caller) is stricter than one that does not.

**Proposed resolution.** Define the units (binary or decimal), say whether the endpoint limit counts header
bytes in every mode, and use different names for the server limit and the endpoint's declared limit.

## 7. Oversize requests: agent MAY enforce, or checklist "rejects"

**Kind:** spec ambiguity.

**Spec.** Agents "MAY additionally enforce server-side and respond with `400`" (SAP:L293). The checklist says an
agent "Rejects ... oversize requests ... with status `400`" (SAP:L673), and the taxonomy lists the oversize
request under `400` (SAP:L599).

**Observed.** The official Python host does enforce it (`agent-sdk/python/src/synadia_ai/agent_service/service.py:921-939`).

**Impact.** It is not clear whether an agent that never checks is compliant.

**Proposed resolution.** Pick one. If the check is optional, remove "oversize" from the checklist item; if it is
required, change the MAY to a MUST.

## 8. Heartbeat optional fields vs checklist "all §8.3 fields"

**Kind:** spec ambiguity.

**Spec.** The payload table marks `protocol_version` and `endpoints` as not required (SAP:L512-L513), and
`session` only "when session-aware" (SAP:L508). The checklist says an agent publishes heartbeats "with all §8.3
fields" (SAP:L677).

**Impact.** A host that omits the optional declarations is compliant by the table and non-compliant by the
checklist.

**Proposed resolution.** Change the checklist to "with every required §8.3 field, and the declarations of
SAP:L517 when it makes them".

## 9. Subscribe-before-ping: SHOULD, or checklist requirement

**Kind:** spec ambiguity.

**Spec.** Callers SHOULD subscribe to the heartbeat wildcard before the first `$SRV.PING.agents` (SAP:L533). The
caller checklist lists "Subscribes ... before initial `$SRV.PING.agents`" as a compliance item, without a
keyword (SAP:L698).

**Impact.** A caller that pings first is compliant by §8.5 and not by §12.

**Proposed resolution.** Align the checklist with the SHOULD ("SHOULD subscribe ...") or raise §8.5 to MUST.

## 10. `status` request with a failing signature: 401 or never rejected

**Kind:** spec ambiguity (internal contradiction).

**Spec.** A failing signature is rejected with `401` "whatever the endpoint's `min_sender_trust`" (SAP:L938) and
the checklist repeats it for "every `prompt` and `status` request" (SAP:L686). But the `status` endpoint "MUST
NOT" reject a request on identity grounds: a failing classification is logged and the reply sent (SAP:L1003,
checklist SAP:L688).

**Impact.** A `status` request with a bad `Agent-Sender` signature gets `401` by one reading and a normal reply
by the other.

**Proposed resolution.** Say in SAP:L938 and SAP:L686 that the `401` applies to `prompt` only, and that `status`
follows SAP:L1003.

## 11. Version compatibility: MINOR is compatible, or pin exactly in 0.x

**Kind:** spec ambiguity.

**Spec.** "Same MAJOR, different MINOR": callers SHOULD treat the agent as compatible (SAP:L648). Then: the 0.x
line is unstable, MINOR bumps MAY break, callers SHOULD pin an exact MAJOR.MINOR until 1.0 (SAP:L651). Every
version of 0.x has MAJOR 0, so the first rule applies to all of them and the second says the opposite.

**Impact.** A caller cannot decide whether a 0.2 agent is compatible with a 0.3 caller. SAP:L657 says they are
not wire-compatible.

**Proposed resolution.** Say that SAP:L648 applies from 1.0 and that in 0.x only an equal MAJOR.MINOR is
compatible. The rule at SAP:L651 then governs.

## 12. Stale "v0.1" wording in a 0.3 document

**Kind:** spec ambiguity (editorial).

**Spec.** SAP:L303 ("v0.1 implementations SHOULD structure attachment handling..."), SAP:L338 ("v0.1 defines
`response`, `status`, `query`"), SAP:L374 ("v0.1 defines one status value") and SAP:L409 ("v0.1 operates on core
NATS only") use the old version.

**Impact.** A reader may take the chunk types and the single status value to be a 0.1 list that 0.3 has
extended, and a SHOULD aimed at "v0.1 implementations" is not clearly aimed at anyone.

**Proposed resolution.** Replace "v0.1" with "this version" or "0.3".

## 13. Caller behaviour on a non-JSON response chunk

**Kind:** spec ambiguity.

**Spec.** Every non-terminating chunk MUST be a JSON object with a `type` (SAP:L341), and unknown chunk types are
ignored (SAP:L343, SAP:L406). A message that is not JSON at all (plain text, a JSON array, an object without
`type`, or a `type` that is not a string) is neither a chunk of an unknown type nor described anywhere. The
caller checklist is silent too.

**Impact.** One caller ends the stream with an error, another skips the message and goes on.

**Proposed resolution.** Say what a caller does: skip it as it would an unknown type, or fail the stream with a
protocol error. The first is more in the spirit of SAP:L406.

## 14. Error code when a mid-stream query times out

**Kind:** spec ambiguity.

**Spec.** When the caller does not answer a query in time the agent MAY terminate the stream "with an error per
§9" (SAP:L456). The taxonomy (SAP:L597-L605) has no timeout code. The nearest are `400`, `409`, `429` and `500`.

**Impact.** Agents pick different codes for the same event and a caller cannot tell a query timeout from a
failure of the agent.

**Proposed resolution.** Name the code (for example `500` or a new `408`/`504` row, as SAP:L607 allows for new
codes) and the machine-readable `error` string for the body.

## 15. `min_sender_trust` in the §8.3 heartbeat example

**Kind:** spec ambiguity (editorial).

**Spec.** The heartbeat example at SAP:L498 carries `"min_sender_trust": "signed"` in the endpoint metadata,
before the key is defined (SAP:L984, in the optional chapter 13). The endpoint metadata table of §2.1 does not
list it (SAP:L89-L92), and the checklist for a non-§13 agent does not mention it. SAP:L517 explains the key
only in passing.

**Impact.** A reader of §1-§12 meets an undefined key, and cannot tell whether a non-§13 host may or must
leave it out of a copied registration.

**Proposed resolution.** Remove the key from the SAP:L498 example or add a note there that it appears only on a
§13 agent (SAP:L984), and list it as an optional key in §2.1.

## 16. `$G` account token vs "tokens MUST NOT begin with `$`"

**Kind:** spec ambiguity.

**Spec.** "Tokens MUST NOT begin with `$`" (SAP:L98). The agent ID of a server with no accounts is `$G.U...`
(SAP:L801, SAP:L845), and the regex accepts `\$G` (SAP:L783). SAP:L810 says `$G` "is a legal NATS token, not a
§2.2-conformant one", and SAP:L812 notes it is not a valid KV key.

**Impact.** The MUST NOT is unqualified, and §13.3.2 breaks it on purpose. A checker applying SAP:L98 to
subject tokens that carry an agent ID rejects `$G`.

**Proposed resolution.** At SAP:L98 say that the rule covers the tokens of §2 and exempts `$G` as an agent-ID
account, or reword SAP:L810 to say it is a stated exception to SAP:L98.

## 17. "Do NOT rely on the framework default queue group" is not a keyword form

**Kind:** spec ambiguity (editorial).

**Spec.** SAP:L180 says "Do NOT rely on the framework's default queue group". SAP:L28 defines the RFC 2119
keywords, and "Do NOT" is not one of them. The paragraph's real requirement is already in SAP:L175 (MUST
register the queue group `"agents"`).

**Impact.** It is not clear whether SAP:L180 adds a requirement. A tool that scans the spec for normative
keywords misses it or misreads it.

**Proposed resolution.** Write "MUST set the queue group explicitly" or make it plain advice ("do not rely on").

## 18. "Exactly one ack" vs periodic ack keep-alives in both official hosts

**Kind:** spec/SDK inconsistency.

**Spec.** Agents MUST emit exactly one `ack` status chunk, as the first message (SAP:L380). The checklist says
"an `ack` chunk as the first message" (SAP:L675).

**Observed.** Both official hosts keep sending `ack` chunks for as long as the handler runs, by default.

- TypeScript: `DEFAULT_KEEPALIVE_INTERVAL_S = 30` (`agent-sdk/typescript/src/service.ts:133`), applied as the
  default at `service.ts:556-559`, and the interval timer that re-sends the ack at `service.ts:1191-1198`. The
  comment at `service.ts:1186-1189` itself says the spec "mandates only the leading ack" and calls periodic acks
  "a valid wire shape".
- Python: `DEFAULT_KEEPALIVE_INTERVAL_S: float = 30.0` (`agent-sdk/python/src/synadia_ai/agent_service/service.py:175`),
  the constructor default at `service.py:433`, `_ack_and_keep_alive` at `service.py:1119-1123` and `_keepalive_loop`
  at `service.py:1178-1196`.

The spec's own rationale for the ack (SAP:L378, SAP:L402) is to reset the caller's inactivity timeout, which is
exactly what the periodic acks do for a handler that is quiet for longer than 60 seconds.

**Impact.** Taken literally, both official hosts are non-compliant by default, and a strict caller or checker
that counts acks fails them. This SDK follows the official SDKs' default, not the letter: keep-alives are on at 30 s unless
`host.WithKeepalive(0)` or `host.WithoutKeepalive` turns them off (`docs/parity.md` row 5.29), so the Go host is
non-compliant with SAP:L380 by default, as the official hosts are. The reason is measured: with keep-alives off, the
official TypeScript and Python callers stall at their 60 s default inactivity timeout on a long-running Go-host
prompt, and with them on they succeed (`test/interop/gohost_keepalive_test.go`, run with `SAP_INTEROP_LONG=1`).
Turn keep-alives off for strict SAP:L380 behaviour.

**Proposed resolution.** Amend SAP:L380 to "exactly one ack first; further `ack` chunks MAY follow as
keep-alives while the request is in flight" and say in SAP:L378 that callers treat each as resetting the
timer. If instead the periodic ack is not intended, the official hosts should default it off.

## 19. Python host answers invalid base64 with 500, not 400

**Kind:** SDK bug (conformance id SAP-9.2-01).

**Spec.** Invalid base64 is a `400` (SAP:L599; checklist SAP:L673). The content must be standard-alphabet,
padded, with no whitespace (SAP:L272).

**Observed.** The Python `Attachment.content` is a plain `str` (`client-sdk/python/src/synadia_ai/agents/envelope.py:44`)
and the `Envelope` model (`envelope.py:68-90`) and `decode` (`envelope.py:117-143`) never check it. Base64 is
decoded only later, when a handler calls `Attachment.to_bytes()` (`envelope.py:57-65`), where
`base64.b64decode(..., validate=True)` raises `binascii.Error`. The host maps an exception that is not a
`ProtocolError` or a `RequestRejectedError` to `500` with the text "handler error"
(`agent-sdk/python/src/synadia_ai/agent_service/service.py:1159-1161`). The host's own envelope step
(`service.py:907-950`) checks size and `attachments_ok` but not base64. The TypeScript decoder checks it while
decoding and raises a protocol error (`client-sdk/typescript/src/prompt/envelope.ts:245-253`), so a TypeScript host
answers `400`. Our conformance run against the Python host reproduces this: `test/sapconform/interop_test.go`
lists SAP-9.2-01 as a known finding.

**Impact.** A caller that sends bad base64 to a Python host sees `500` ("internal error") instead of `400`
("fix your request"), and a handler that never decodes the content takes the garbage silently.

**Proposed resolution.** Validate attachment content in the Python `Attachment` (a pydantic validator) or in
`decode`, raising `ProtocolError`, as the TypeScript decoder does. Add a vector for it to
`test-fixtures/attachments`.

## 20. Python `interval_s` is an `int`

**Kind:** SDK bug (type narrower than the spec).

**Spec.** `interval_s` is a `number`, greater than 0, recommended at least 1 (SAP:L511). The spec does not say
integer. The status reply uses "exactly the §8.3 schema" (SAP:L551).

**Observed.** The Python client's `HeartbeatPayload.interval_s` is `int`
(`client-sdk/python/src/synadia_ai/agents/heartbeat.py:92`), validated by pydantic, whose integer type refuses a
number with a fractional part. A heartbeat with `"interval_s": 0.5` or `2.5` therefore fails validation, and the
listener drops it (the handler at `heartbeat.py:262-284`). `Agent.status()` validates the reply with the same
model and turns the failure into `ProtocolError("status reply is not a §8.3 heartbeat payload")`
(`client-sdk/python/src/synadia_ai/agents/agent.py:541-543`), so a fractional interval is reported as a protocol
violation. The Python host has the same narrowing: `heartbeat_interval_s: int = 30`
(`agent-sdk/python/src/synadia_ai/agent_service/service.py:430`) and `interval_s: int` in
`agent-sdk/python/src/synadia_ai/agent_service/heartbeat.py:109`, `:149`, `:180`, so a Python host cannot be
configured with a fractional or sub-second cadence. TypeScript takes a number (`intervalS: number`,
`client-sdk/typescript/src/heartbeat/tracker.ts:13`). We did not find code in the Python host that rounds a
fractional value; the narrowing is the type. This was read from source and not run against a live Python install.

**Impact.** A TypeScript or Go host with a fractional interval is invisible to Python callers (their tracker
drops the beat), and `status()` raises. This SDK keeps sub-second and fractional intervals opt-in for that
reason (`host.WithSubSecondHeartbeats`, `docs/parity.md` D-9).

**Proposed resolution.** Type `interval_s` as `float` (or `int | float`) in the Python client and host, matching
the spec's "number". Alternatively state in SAP:L511 that `interval_s` is an integer, and say what a receiver
does with a fractional one.

## 21. Python client flattens service errors to `ProtocolError` text

**Kind:** SDK bug (information loss; the two client SDKs differ).

**Spec.** An error response has a numeric code in `Nats-Service-Error-Code`, a description, and an optional JSON
body with a machine-readable `error` and extra fields (SAP:L572-L593). The code and the body are the contract a
caller acts on (for example `401` against `403` against `429` with `retry_after_s`).

**Observed.** In the Python client both places that see an error header raise
`ProtocolError(f"service error {code}: {desc}")`, once for a prompt stream
(`client-sdk/python/src/synadia_ai/agents/agent.py:788-791`) and once for `status()` (`agent.py:536-539`). The
code and the description exist only inside the message text, and the body is never read. `ProtocolError` is
defined as a malformed-wire error (`client-sdk/python/src/synadia_ai/agents/errors.py:16-20`), so a refusal such
as `403` shares a type with a garbled frame. The TypeScript client keeps the code, description and body in a
`ServiceError`, as `docs/parity.md` records.

**Impact.** A Python caller has to parse its own exception message to tell an authorization refusal from a rate
limit, and cannot read `retry_after_s`.

**Proposed resolution.** Add a `ServiceError(NatsAgentError)` carrying `code`, `description` and `body` and raise it
at both places. Keep `ProtocolError` for malformed frames. To stay source-compatible, `ServiceError` could subclass
`ProtocolError` for one release.

## 22. AgentTools result schemas do not admit the `{"error"}` refusal

**Kind:** SDK bug (schema fixture and contract disagree).

**Contract.** `docs/agent-tools.md:65-70` defines a refusal as `{"error": "..."}` alone, with no `call_id` or
`state`, and says refusals cover bad arguments, an unknown address, an unknown `call_id`, a loop, and so on. Section
3.7 (`docs/agent-tools.md:181-186`) says anything the model can act on "comes back in words: a refusal, or a
`failed` call with `error`", and a host-level tool failure is for bugs only. The result table lists only success
shapes for `discover_agents`, `cancel_agent` and `list_agent_calls` (`docs/agent-tools.md:78-85`), and the refusal
rule applies to any tool, since a tool can get bad arguments.

**Observed.** Only `prompt_agent.result.json` has a `oneOf` that includes the refusal shape
(`test-fixtures/agent-tools/prompt_agent.result.json:6`, with an `error`-only branch at `:13-17`). The other
three do not:

- `discover_agents.result.json` requires `agents` with `additionalProperties: false`
  (`test-fixtures/agent-tools/discover_agents.result.json:6-9`), so `{"error": "..."}` fails validation.
- `list_agent_calls.result.json` requires `calls` with `additionalProperties: false`
  (`test-fixtures/agent-tools/list_agent_calls.result.json:6-9`), so a refusal fails validation.
- `cancel_agent.result.json` admits the `{"call_id", "error"}` entry for an untracked id inside `calls`
  (`test-fixtures/agent-tools/cancel_agent.result.json:46-60`) but not a whole-call refusal such as a malformed
  `call_ids` argument.

**Impact.** A host that validates tool results against these schemas, as the README invites, rejects a correct
refusal from three of the six tools, and a conformance test built on them fails an implementation that follows
the contract.

**Proposed resolution.** Make the root of the three schemas a `oneOf` of the success object and the
`{"error": string}` refusal, as `prompt_agent.result.json` does, or share one refusal definition by `$ref`. Add a
fixture of a refusal per tool.
