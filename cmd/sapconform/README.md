# sapconform

`sapconform` checks a Synadia Agents Protocol (SAP) 0.3 agent, the host side,
against the conformance catalog in [`conformance/catalog.json`](../../conformance/catalog.json).
It is a black box: it needs NATS access to the system under test and nothing
else. It never touches the agent's process, files or code.

It covers **host obligations only**. It makes no claim about caller
conformance, and every report says so.

```
go run github.com/bogworks/synadia-agents.go/cmd/sapconform --context my-nats
```

## What it checks

The checks are the catalog's checks, not a second list. The catalog is embedded
in the binary. For every entry whose role is `host` or `both`, the entry's
capability, which is fixed in the catalog, decides what happens:

| Capability | What the runner does |
|---|---|
| `black-box` | Runs a check on every run. |
| `needs-configuration` | Runs the check when you supplied the configuration it needs (listed below); otherwise the result is `configuration-missing`. |
| `sdk-internal` | `not-run`: nothing a NATS client can see. |
| `not-applicable` | Reported with the catalog's reason. |

Entries for callers and operators are listed as `not-run`.

Each checked entry gets one result for the run. This is separate from the
capability:

| Result | Meaning |
|---|---|
| `pass` | The obligation was observed to hold. |
| `fail` | The agent was observed to break it. |
| `configuration-missing` | The entry needs configuration you did not supply. The detail names the flag. |
| `policy` | The agent made a choice the spec leaves to it, for example serving a request the spec does not oblige it to refuse with 400. Never a failure. |
| `not-applicable` | This agent or this run does not exercise the behaviour: it never emits a query, it accepts attachments, and so on. |
| `not-run` | Not a host obligation, observable only inside an SDK, or not implemented in this version. |

The exit status is 0 only when no `MUST` or `MUST NOT` entry has the result
`fail`. It is 1 when one does, or when the run is incomplete (see Limits), and 2
when the run could not happen (bad usage, no connection, no agent matched).

There are two verdicts, and the JSON report carries both under `verdict`
(`default_ok`, `strict_ok`, `failed`, `strict_failed`):

- default: no `MUST` or `MUST NOT` entry failed;
- strict (`--strict` makes it the exit status): no entry of any level failed,
  and no entry you supplied configuration for is `configuration-missing`. An
  entry whose configuration you never supplied does not count against it.

Exit 0 without `--strict` means no MUST-level failure. It does not mean full
conformance: SHOULD and WIRE entries may have failed and configuration may be
missing. Automation that wants that claim should gate on `--strict`.

## What it sends

The runner sends real prompts. Each is a request your agent will handle:

- up to five well-formed prompts (plain text, a JSON envelope with an unknown
  field, a JSON envelope after leading whitespace, text that starts with a
  bracket, and one with an attachment when the endpoint accepts them). Set the
  text with `--probe-prompt`; the default asks the agent to reply with the word
  ok;
- up to eight malformed requests that the spec says to refuse with 400 (a
  zero-byte payload, bad JSON, no prompt, an empty prompt, bad base64, and so
  on);
- two status requests, a service ping and an info request;
- the signed and unsigned requests of the sender-identity extension, when you
  configure it.

It also subscribes to `agents.hb.*.*.*` before it discovers anything, and
observes heartbeats for a window.

## Flags

Connection. Give one of `--context` or `--url`. With neither, the URL is
`$NATS_URL`, else `nats://127.0.0.1:4222`.

| Flag | Meaning |
|---|---|
| `--context NAME` | A NATS CLI context (`current` for the selected one). |
| `--url URL` | The server URL. |
| `--creds FILE` | A credentials file. |
| `--nkey FILE` | A user seed file. |

Target. With none of these, every agent that answers `$SRV.INFO.agents` is a
target and is checked in turn. Instances that share a prompt subject are one
target.

| Flag | Meaning |
|---|---|
| `--agent TOKEN` | The agent: the subject token or `metadata.agent`. |
| `--owner TOKEN` | The owner. |
| `--name TOKEN` | The instance name, the last token of the prompt subject. |

Output and timing.

| Flag | Meaning |
|---|---|
| `--json` | Print one JSON report on stdout instead of the summary. |
| `-v` | List every entry in the summary, not only the ones that need attention. |
| `--config FILE` | A JSON file with any of the settings (see below). A flag overrides the file. |
| `--discover-wait D` | Length of the discovery round. Default 2s. |
| `--heartbeat-window D` | How long to observe heartbeats. Default automatic: about two declared intervals, between 3s and 70s, ending early once every instance has been heard three times. Set it to shorten the run against an agent with a long interval; the cadence check then reports `not-applicable` when it hears fewer than two beats. |
| `--stream-timeout D` | The inactivity timeout of one prompt stream. Default 30s. |
| `--probe-prompt TEXT` | The text of the well-formed probes. |
| `--version` | Print the version. |

### Configuration that enables entries

An entry that needs one of these reports `configuration-missing` without it.

| Flag | Enables |
|---|---|
| `--expect-min-sender-trust none\|any\|signed` | The sender-identity extension (SAP section 13): declaration, classification, the 400, 401 refusals, `status` always answerable, `id_sig`, signed heartbeats. Say what the agent declares in its prompt endpoint metadata; `none` says it does not implement the extension. |
| `--sender-seed FILE` | Checks that need a sender the agent admits: a valid signature, a fractional-second `ts`, the `sub`, `ts` and nonce checks, `sub` as a pattern. Also signs every well-formed probe, which a `signed` endpoint needs. |
| `--sender-from-connection` | Signs with the seed of the connection's own credentials instead of `--sender-seed`. |
| `--sender-account NAME` | The sender's account. Default: asked of the server (`$SYS.REQ.USER.INFO`), used when the connection's user is the sender. |
| `--refused-seed FILE` | A seed of an identity the agent verifies and then refuses: the 403 entries. |
| `--session-aware[=false]` | Whether the harness is session-aware: the `metadata.session` and heartbeat `session` entries. |
| `--expect-instances N` | You started N instances of one logical agent (2 or more): the multiple-instance entries. |
| `--probe-oversize` | Sends a request one byte over the declared `max_payload`. |
| `--query-prompt TEXT` | A prompt that makes the agent ask a mid-stream query: the query entries. The runner observes the query and answers none unless `--answer-query-subject-prefix` allows it; it also leaves a query unanswered to see what the agent does. |
| `--answer-query-subject-prefix P` | Repeatable. The runner answers a query whose `reply_subject` starts with `P`. The agent chose that subject and the runner publishes with your credentials, so by default it publishes nothing to it. Use a prefix that cannot reach anything you care about (for example `_INBOX.`). Without it the entries that need an answered query are `configuration-missing`. |
| `--query-unanswered-wait D` | How long to wait for the agent's own query timeout. Default 15s. |
| `--slow-prompt TEXT` | A prompt that makes the agent work before its first response chunk: the ack-first entry. It passes when the ack is the first message; when the content followed is in the detail. |
| `--ack-deadline D` | A diagnostic threshold for the ack on that prompt. Default 1s. The spec fixes no duration and the runner cannot see when the agent began its work, so a later ack is noted in the detail and is never a failure. |

### Limits

The agent under test, and anything else on the subject space, is untrusted
traffic, so a run is bounded. A stream over a cap is closed and the checks that
used it fail with the cap named; a run over its deadline or cap is `incomplete`
and passes no verdict.

| Flag | Bounds | Default |
|---|---|---|
| `--max-duration D` | the whole run | 5m |
| `--max-stream-messages N` | messages one prompt stream may deliver | 1000 |
| `--max-stream-bytes N` | bytes one prompt stream may deliver | 4194304 |
| `--max-discovery N` | replies read per discovery round | 256 |
| `--max-targets N` | targets checked per run | 32 |
| `--max-heartbeats N` | heartbeats retained per run | 5000 |
| `--max-info-probes N` | INFO probes started because a heartbeat named a new instance | 64 |

### The configuration file

Every setting is also a key in a JSON file for `--config`. Unknown keys are an
error. Seeds are named by file path and are never printed.

```json
{
  "context": "staging",
  "agent": "my-agent",
  "owner": "me",
  "expect_min_sender_trust": "signed",
  "sender_seed": "/secure/path/sender.nk",
  "session_aware": false,
  "timeouts": { "heartbeat_window": "10s", "stream": "30s", "max_duration": "5m" },
  "limits": { "stream_messages": 1000 },
  "answer_query_subject_prefixes": ["_INBOX."],
  "strict": true
}
```

When the endpoint declares `min_sender_trust: signed` and no usable sender seed
is given, every check that needs a served stream reports `configuration-missing`
and says so, instead of failing the agent for refusing an unsigned request.

## The report

`--json` prints one report:

```
tool, tool_version, scope, catalog_version, spec_version, spec_commit,
started_at, duration, selector, configuration_supplied, incomplete, notes, verdict,
targets: [ { target: {prompt_subject, agent, owner, name, instance_ids},
             counts,
             entries: [ {id, section, spec_line, level, role, capability,
                         summary, result, detail, duration_ms,
                         config_supplied} ] } ]
```

Every catalog entry appears once per target, with its capability and this run's
result, and each cites its id and its spec line. The human summary prints the
same counts and lists what needs attention: failures first (a `***` marks one
that makes the run fail), then entries with missing configuration, policy
choices and not-applicable entries.

## What a black box cannot see

Some entries cannot be observed from outside, or need an environment the
runner does not build. The runner says so in the result and does not guess:

- the account-token and renamed-import forms of the sender `sub` rule need an
  account layout of exports and imports;
- how the NATS server load-balances instances in the queue group;
- whether the order of a stream's content matches the order the agent
  produced it (the runner checks the framing order: ack first, terminator last);
- the order of startup (the runner checks that the service record is
  answerable when it first hears a heartbeat).
