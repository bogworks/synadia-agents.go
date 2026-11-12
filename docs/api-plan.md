# Go public API plan

> **This is the original plan, written before the code. The code differs from it in places, and `go doc -all`
> of each package is authoritative.** [`protocol-mapping.md`](protocol-mapping.md) maps every real identifier
> to its SAP section and [`parity.md`](parity.md) names the real counterpart of every upstream feature. The
> plan is kept for the reasons behind the shape. Differences to expect:
>
> - The sender gate is `identity.Gate` (with `identity.GateOptions`, `identity.Admission`), not `host.SenderGate`;
>   the nonce set is `identity.NonceSet`, not `host.NonceCache`; `identity.Header` is `identity.SenderHeader`.
> - Subjects are `agents.NewAgentSubject` and `agents.AgentSubject`; the record is `agents.AgentInfo`; discovery
>   filters are `caller.Filter`; stream items are the `caller.Event` set (`Response`, `Status`, `*Query`).
> - A caller has ONE multiplexed reply subscription, demultiplexed by reply token; a late reply is dropped and
>   counted (`caller.Stats.DroppedReplies`).
> - A host serves at most `host.DefaultMaxConcurrentPrompts` (64) prompts at once, queueing or answering 429
>   (`host.WithBusyPolicy`); status is always answered.
> - There is no catch-all root error sentinel (`agents.ErrAgent`); errors match on their own, and
>   `agents.ErrLocal` groups the pre-publish refusals.
> - Streamed chunks are read with `caller.Stream.Events` (an `iter.Seq2`); the terminator is `Stream.Terminated`.
> - `interval_s` is a float number of seconds; a sub-second heartbeat is opt-in.

Proposal for the public API of `github.com/bogworks/synadia-agents.go`, mapping every row of `docs/parity.md` to a Go
identifier. It follows the work contract `sap-go-sdk` (REQ-003 to REQ-009, REQ-016, REQ-019, REQ-020) and the SAP 0.3
specification (cited `SAP:Lnnn` for `core-protocol.md` at `c48cea5f`). Nothing here is code: it is the shape the code
is written to. The parity matrix names these identifiers; AT-011 fails a row whose identifier does not exist.

Go 1.26 module (`go.mod` says 1.26). Runtime dependencies stay `nats.go` v1.55.0 and `nkeys` v0.4.17, plus the
standard library. Features used from the standard library: `iter` (1.23), `os.Root` (1.24), `sync.WaitGroup.Go` and
`testing/synctest` (1.25), `log/slog`, `context.AfterFunc`. REQ-020 says the `go` directive is the older of the two
most recent releases at the first tag, so the directive must be re-checked against that rule when the first tag is cut.

## 1. Packages

| Package | Import path | Holds | Imports (module-internal) |
|---|---|---|---|
| `agents` | `github.com/bogworks/synadia-agents.go` | Shared protocol types: subjects, tokens, envelope, chunks, heartbeat payload, discovery records, errors, byte sizes, version, attachment saving | `identity`, `internal/errbase` |
| `identity` | `.../identity` | SAP §13: agent ID, signers, `Agent-Sender` header, signing, verification, own-identity lookup, `id_sig`, `Nats-Request-Info` | `internal/errbase` |
| `caller` | `.../caller` | The caller side: `Client`, `Agent`, `Stream`, `Query`, heartbeat tracker, interceptors, sender resolver | `agents`, `identity` |
| `host` | `.../host` | The host side: `Host`, `Request`, `Responder`, interceptors, sender gate, nonce cache, heartbeat signing | `agents`, `identity`, `caller` (for the sender resolver) |
| `tools` | `.../tools` | The AgentTools layer (parity section 12) | `agents`, `caller`, `host`, `identity` |
| `connect` | `.../connect` | **Addition.** NATS CLI context, URL parsing, connection bundle, reconnect defaults (parity section 1, deviation D-1, D-2) | `identity` |
| `agentstest` | `.../agentstest` | **Addition.** `ReferenceAgent`, the spec-compliant test counterparty (parity 7.33) | `agents`, `host` |
| `cmd/sapconform` | `.../cmd/sapconform` | The conformance runner (REQ-013); a thin `main` over `internal/conform` | `agents`, `caller`, `identity` |
| `internal/errbase` | n/a | The one sentinel behind `agents.ErrAgent`, so `identity` errors match it without importing `agents` | none |
| `internal/conform` | n/a | Catalog-driven checks; the runner's checks are the catalog's checks (REQ-013) | `agents`, `caller`, `identity` |

Why two packages beyond the contract's list: `connect` is code the official SDKs ship that is not protocol (kept out of
the root so a consumer that wants only SAP does not link it), and `agentstest` is the Go counterpart of TS
`@synadia-ai/agent-service/testing`, which the interop and conformance runs need. Both are flagged for the
coordinator in section 12.

`identity` is a leaf (it imports neither `agents` nor `caller`) so that §13 can be reused alone, as the TS codec is.
The consequence is that identity-related values that name an agent record (`resolve()` on a verified sender, the
reverse-lookup index) live in `caller` and `host`, not in `identity`.

## 2. Cross-cutting decisions

### 2.1 Options: functional options, not an Options struct (recommendation)

Constructors take required arguments positionally and everything else as functional options, as `nats.go` does
(`nats.Connect(url, opts ...nats.Option)`, `js.Publish(subj, data, opts ...PubOpt)`):

```go
c, err := caller.New(nc, caller.WithIdentity(identity.Options{Signer: s}), caller.WithLogger(l))
h, err := host.New(nc, subject, handler, host.WithMaxPayload(1<<20))
st, err := agent.Prompt(ctx, "hi", caller.WithAttachments(a))
```

Reasons:

1. **Additive without breaking.** A new knob is a new function; an `Options` struct also grows compatibly, but a
   caller who builds the struct positionally or compares it breaks, and every exported field becomes a promise. NFR-003
   (stable within a minor version) is cheaper to keep with functions.
2. **Validation has a home.** Each option can validate and return an error from `New`, so the checks the upstream
   constructors do (`heartbeatIntervalS > 0`, `keepalive > 0 or null`, `replayWindowMs > 0`, concurrency `>= 1`) happen
   before any registration, and `New` returns a typed error rather than panicking.
3. **Zero value is usable.** No option means the documented SAP-recommended default; nothing needs "unset" markers
   (`*bool`, `-1`).
4. **Per-call and per-client options share one idiom.** `PromptOption` and `Option` look alike, as `nats.Option` and
   `nats.SubOpt` do.
5. **It is what `nats.go` and `orbit.go` readers expect**, which matters for acceptance upstream.

The counter-case is `micro.Config` (a struct) in `nats.go/micro`. We use it where we pass through to `micro`
(`host.Endpoint`), and keep one style for our own surface. Where an option needs several related fields it takes a small
named struct (`identity.Options`, `host.IdentityOptions`, `agents.Filter`), as `micro.EndpointConfig` does.

### 2.2 `context.Context` on every blocking call (REQ-003)

First parameter, always. Rules:

- A cancelled or expired `ctx` ends the call and returns `ctx.Err()` wrapped with `%w` (so `errors.Is(err,
  context.Canceled)` and `context.DeadlineExceeded` work).
- The `ctx` passed to `Prompt` governs the **life of the stream**, not only the publish: cancelling it drops the reply
  registration and the iterator yields `ctx.Err()` (parity 3.7). This replaces the TS `AbortSignal` and the Python
  task cancellation.
- The `ctx` reaches prompt interceptors (parity 3.10, 11.9) and, on the host, the handler (`host.PromptHandler`), which
  is cancelled when the host stops.
- No blocking call reads a package-global timeout; defaults are named constants that an option overrides.

### 2.3 Streamed chunks: `iter.Seq2[Message, error]` (recommendation)

`(*caller.Stream).Messages() iter.Seq2[caller.Message, error]`, consumed with `for msg, err := range stream.Messages()`.

Why an iterator and not a channel, a callback or `Next(ctx)`:

1. **`break` is deterministic cleanup.** Leaving the loop makes `yield` return false; the stream unsubscribes before
   `Messages` returns. This is the TS `for await ... break` behaviour, and it fixes the Python caveat (a bare `break`
   defers cleanup to the garbage collector; `aclosing()` is needed). A channel makes the consumer remember to cancel
   and leaves a producer goroutine behind if it forgets (NFR-002: no leaked goroutine).
2. **Errors arrive in order with items.** A `Seq2` yields `(msg, nil)` then at most one `(nil, err)`, then ends. A
   channel needs a second channel or a wrapper type, and a callback API inverts control so the handler cannot call
   `q.Reply` on its own schedule.
3. **No goroutine per stream.** The iterator pulls from the stream's bounded queue on the consumer's goroutine; the
   only goroutine the SDK owns is the connection's NATS callback.
4. **Pull style is one call away.** `next, stop := iter.Pull2(stream.Messages())` gives `Next`-style code and `select`
   integration where wanted; `defer stop()`.
5. **Go 1.26 has it in the standard library**, and `slices.Collect`-style helpers compose.

Shape of the sequence: items are yielded in wire order. The sequence ends after one of: the zero-byte terminator
(no error; `Stream.Terminated()` is true), an error frame (yielded as `*agents.ServiceError`; the sequence then ends,
consuming the trailing terminator internally), the inactivity timeout, the max wait, `ctx` cancellation,
`Stream.Close()`, or client close. A stream is single-use; a second `Messages()` call yields
`(nil, caller.ErrStreamConsumed)` once.

No synthetic `done` item is yielded (TS does, Python does not; SAP:L384 makes it a MAY). The end of the sequence with a
nil error is the done signal and `Terminated()` records it (parity 5.18).

### 2.4 Errors

Sentinels plus typed errors, wrapped with `%w`, always usable with `errors.Is` and `errors.As` (REQ-007):

- Sentinel per class (`agents.ErrPromptEmpty`, `agents.ErrService`, ...) for `errors.Is`.
- A typed error where there is data (`*agents.ServiceError`, `*agents.PayloadTooLargeError`, ...) for `errors.As`.
- Typed errors implement `Is(target error) bool` so `errors.Is(err, agents.ErrPayloadTooLarge)` is true for the typed
  value, and `errors.Is(err, agents.ErrAgent)` is true for every SDK error.
- **Local versus remote** (REQ-007): a local refusal matches `agents.ErrValidation` (empty prompt, attachments not
  supported, payload too large, query already replied) or `agents.ErrVersionMismatch`, `identity.ErrSignatureRequired`;
  a remote error is `*agents.ServiceError` carrying the wire code, the `Nats-Service-Error` text and the optional body.
- No error contains a seed, credential or payload (NFR-001). Seeds are never accepted as an `error` argument or
  formatted with `%v`.

### 2.5 Logging (REQ-020)

`*slog.Logger` option on `Client`, `Host`, `Tools`. `nil` is silent; the SDK installs no handler, never touches
`slog.Default()`, and never writes to stdout or stderr. Application-code failures (interceptors, providers) are logged
with a fixed message and no exception text, as upstream does. Payloads, nonces, signatures and seeds are never logged.

### 2.6 No global state

No `init()` side effects, no package-level mutable variables. The TS and Python per-connection memo of the own
identity is module-global (weak-keyed); in Go it lives in a `*identity.SelfID` owned by the `Client` or `Host` and
invalidated by `nc.StatusChanged` (section 6.3). Constants are `const`; lists such as the reserved verbs are functions
returning copies.

## 3. Package `agents` (root): shared types

```go
package agents

// Constants. See SAP §2, §3.1, §3.3, §8.1, §8.7, §13.9.
const (
    ServiceName            = "agents"
    PromptEndpointName     = "prompt"
    StatusEndpointName     = "status"
    PromptQueueGroup       = "agents"
    StatusQueueGroup       = "agents"
    SubjectRoot            = "agents"
    VerbPrompt             = "prompt"
    VerbHeartbeat          = "hb"
    VerbStatus             = "status"
    VerbAttachments        = "attachments"
    HeartbeatSubject       = "agents.hb.*.*.*"
    MinSenderTrustKey      = "min_sender_trust"
    InboxPrefix            = "_INBOX.agents"
    StatusAck              = "ack"
    DefaultLivenessSlack   = 3
    HeaderServiceError     = "Nats-Service-Error"
    HeaderServiceErrorCode = "Nats-Service-Error-Code"
    SDKProtocolString      = "0.3"
)

func ReservedVerbs() []string
func NewInbox() string // "_INBOX.agents.<22 random chars>", from crypto/rand
```

### 3.1 Subjects and tokens (parity 13.5 to 13.10, 7.6, 7.7)

```go
// AgentSubject is the per-instance address of an agent: SAP §2, §3.2. Its fields are unexported; the
// only constructors (NewAgentSubject, ParseSubject) validate, so a non-zero value always builds valid subjects.
type AgentSubject struct{ /* agent, owner, name, token */ }

type SubjectOption func(*subjectConfig)

func WithSubjectToken(token string) SubjectOption

// NewAgentSubject enforces SAP's MUSTs (non-empty, no leading "$", no whitespace, ".", "*", ">" or NUL).
// SHOULD violations are reported by Warnings, not refused (REQ-009).
func NewAgentSubject(agent, owner, name string, opts ...SubjectOption) (AgentSubject, error)

func (s AgentSubject) Agent() string  // metadata.agent
func (s AgentSubject) Owner() string  // metadata.owner
func (s AgentSubject) Name() string   // 5th subject token
func (s AgentSubject) Token() string  // 3rd subject token on the wire; defaults to Agent
func (s AgentSubject) IsZero() bool
func (s AgentSubject) Prompt() string // "" on the zero value
func (s AgentSubject) Heartbeat() string
func (s AgentSubject) Status() string
func (s AgentSubject) Attachments() string
func (s AgentSubject) Subject(v Verb) (string, error) // error unless v is one of the four Verb constants
func (s AgentSubject) Warnings() []string

func ParseSubject(subject string) (Verb, AgentSubject, error)
func ParseAgentSubject(subject string, verb Verb) (AgentSubject, error)
func IsHeartbeatSubject(subject string) bool
func ValidateToken(role, token string) error

// Service error codes: immutable sentinels, matched by code.
var ErrBadRequest, ErrUnauthorized, ErrForbidden, ErrNotFound, ErrConflict, ErrRateLimited, ErrInternal error
func CodeOf(err error) (int, bool)
func IsCode(err error, code int) bool

// Local limits. Both capability keys are mandatory (SAP §2.1); a record without valid ones is unusable.
func (e EndpointInfo) PromptLimits() (PromptLimits, error) // *CapabilityError, matches ErrInvalidCapability
func (l PromptLimits) Prepare(env Envelope, senderHeaderBytes int, connMaxPayload int64) ([]byte, error)
```

`AgentSubject` carries no escaping and no hidden transformation (deviation D-4: Python's base64url escape is not ported).

### 3.2 Byte sizes and protocol version (parity 13.3, 13.4, 13.11)

```go
func ParseByteSize(s string) (int64, error)  // \d+(B|KB|MB|GB), base 1024, case-insensitive
func FormatByteSize(n int64) (string, error)

type ProtocolVersion struct{ Major, Minor int }
func SDKProtocolVersion() ProtocolVersion
func ParseProtocolVersion(s string) (ProtocolVersion, error) // drops patch and pre-release
type VersionCompatibility int
const ( Compatible VersionCompatibility = iota; MinorDrift; Incompatible )
func CompareProtocolVersion(agent, sdk ProtocolVersion) VersionCompatibility
```

### 3.3 Envelope and attachments (parity 3.15 to 3.26)

```go
// Attachment is a request-side attachment: decoded bytes. SAP §5.2.
type Attachment struct {
    Filename string
    Content  []byte
}

// WireAttachment is an attachment as it arrived in a chunk: raw base64, not validated (SAP §6.3, §7.1).
type WireAttachment struct{ Filename, Content string }
func (a WireAttachment) Bytes() ([]byte, error) // strict §5.2

// Envelope is the JSON request envelope. SAP §5.1, §5.6.
type Envelope struct {
    Prompt      string
    Attachments []Attachment
    Extras      map[string]json.RawMessage // unknown top-level fields, preserved verbatim (§5.6)
}

func EncodeEnvelope(e Envelope) ([]byte, error)            // refuses a reserved name in Extras
func EncodedEnvelopeSize(e Envelope) (int, error)
func DecodeEnvelope(data []byte) (Envelope, error)         // §5.3: "{" discriminates; errors wrap ErrProtocol
func IsEnvelopeField(name string) bool
func DecodeStrictBase64(s string) ([]byte, error)          // standard alphabet, padded, no whitespace
```

`DecodeEnvelope` follows the TypeScript decode (it is the stricter, spec-aligned one): it rejects a zero-byte payload,
an empty `prompt`, an unsafe attachment filename, and attachment content that is not strict base64, each as an error
that wraps `ErrProtocol` so the host answers 400. Invalid UTF-8 plain text is also an error (Python's behaviour; TS
substitutes U+FFFD silently, which would turn corrupt input into a prompt).

### 3.4 Chunks (parity 5.8, 5.9, 5.26, 5.27)

```go
// Chunk is a reply chunk. SAP §6.2. Sealed: only this package implements it.
type Chunk interface{ chunkType() string }

type ResponseChunk struct {
    Text        string
    Attachments []WireAttachment
}
type StatusChunk struct{ Status string }
type QueryChunk struct {
    ID, ReplySubject, Prompt string
    Attachments              []WireAttachment
}

func EncodeChunk(c Chunk) ([]byte, error) // a string-only response is the bare-string form
// DecodeChunk returns (nil, nil) for an unknown "type" (SAP:L398, MUST ignore); an error wraps ErrProtocol
// for malformed JSON, a non-object, a missing "type" or a malformed known chunk.
func DecodeChunk(data []byte) (Chunk, error)

func SplitResponseText(text string, maxPayload int, opts ...SplitOption) []string
type SplitOption func(*splitConfig)
func WithReserveBytes(n int) SplitOption
func WithSafetyDivisor(n int) SplitOption
```

### 3.5 Heartbeat payload (parity 8.2 to 8.7)

```go
type HeartbeatEndpoint struct {
    Subject  string
    Metadata map[string]string
}

type HeartbeatPayload struct {
    Agent, Owner    string
    Session         string // "" when absent
    InstanceID      string
    TS              string // YYYY-MM-DDTHH:MM:SSZ
    IntervalS       float64
    ProtocolVersion string // "" when absent
    Endpoints       map[string]HeartbeatEndpoint // nil when absent
    Extras          map[string]json.RawMessage   // unknown fields
}

type HeartbeatOptions struct {
    Session         string
    Extras          map[string]any
    ProtocolVersion string
    Endpoints       map[string]HeartbeatEndpoint
}

func BuildHeartbeat(s Subject, interval time.Duration, instanceID string, now time.Time, o HeartbeatOptions) HeartbeatPayload
func EncodeHeartbeat(p HeartbeatPayload) ([]byte, error)
// DecodeHeartbeat returns ok=false when a required field is missing or wrong; a malformed optional field
// (protocol_version, endpoints) is ignored and the beat kept (SAP:L517).
func DecodeHeartbeat(data []byte) (HeartbeatPayload, bool)
```

### 3.6 Discovery records (parity 2.9 to 2.17)

```go
type MinSenderTrust string
const ( TrustAny MinSenderTrust = "any"; TrustSigned MinSenderTrust = "signed" )
// ParseMinSenderTrust: absent -> ("", false); "any" -> TrustAny; anything else -> TrustSigned (SAP:L1001).
func ParseMinSenderTrust(raw string, present bool) (MinSenderTrust, bool)

type Endpoint struct {
    Name, Subject, QueueGroup string
    Metadata                  map[string]string
    MaxPayload                int64 // 0 = undeclared or unparseable; raw string stays in Metadata
    AttachmentsOK             *bool // nil = undeclared
    MinSenderTrust            MinSenderTrust // "" = absent (a 0.3 agent without §13)
}

type Info struct {
    InstanceID, Agent, Owner, Name, Session, ProtocolVersion, Description, Version string
    Metadata                map[string]string
    Endpoints               []Endpoint
    PromptEndpoint          Endpoint
    SupportsSenderIdentity  bool            // min_sender_trust present
    Identity                identity.AgentID // zero when absent or malformed
    IDSigVerified           bool
}
func (i Info) PromptSubject() string
func (i Info) EffectiveMinSenderTrust() MinSenderTrust // absent -> TrustAny

// InfoFromMicro builds a record from nats.go's micro.Info; ok=false when it is not an agent
// (wrong service name, missing agent/owner/protocol_version, no prompt endpoint).
func InfoFromMicro(in micro.Info) (Info, bool)

type Filter struct{ Agent, Owner, Name, Session, ProtocolVersion string } // AND, exact; "" = any
func (f Filter) Match(i Info) bool
```

### 3.7 Saving received attachments (parity 4.1 to 4.7)

```go
type SavedAttachment struct {
    Filename  string // as the sender gave it
    SizeBytes int64  // decoded; 0 when the content was invalid
    Path      string // absolute; "" when not saved
    Skipped   SkipReason
}
type SkipReason string
const ( NotSkipped SkipReason = ""; SkippedOverLimit SkipReason = "over_limit"; SkippedInvalidContent SkipReason = "invalid_content" )

type SaveOption func(*saveConfig)
func WithMaxTotalBytes(n int64) SaveOption // default DefaultSaveMaxTotalBytes; NoLimit disables
const ( DefaultSaveMaxTotalBytes int64 = 64 << 20; NoLimit int64 = -1 )

// SaveAttachments writes into dir, created 0700 if missing; files 0600, created exclusively, never following a link
// (os.Root). Bad content never returns an error; a real I/O error does.
func SaveAttachments(ctx context.Context, dir string, atts []WireAttachment, opts ...SaveOption) ([]SavedAttachment, error)
```

### 3.8 Errors (parity section 9)

```go
var (
    ErrAgent, ErrValidation, ErrPromptEmpty, ErrAttachmentsNotSupported, ErrPayloadTooLarge,
    ErrService, ErrProtocol, ErrStreamStalled, ErrStreamMaxWait, ErrClosed, ErrAgentNotFound,
    ErrQueryTimeout, ErrQueryAlreadyReplied, ErrInvalidToken, ErrInvalidSize,
    ErrInvalidProtocolVersion, ErrVersionMismatch, ErrReservedHeader, ErrNoStatusEndpoint error
)

type ServiceError struct {
    Code        int    // Nats-Service-Error-Code
    Description string // Nats-Service-Error
    Body        *ErrorBody
}
type ErrorBody struct {
    Error, Message string
    Extra          map[string]json.RawMessage // callers MUST tolerate unknown fields
}
// ServiceErrorFromMsg: an empty or non-JSON body leaves Body nil and the header text is the description (SAP:L593).
func ServiceErrorFromMsg(m *nats.Msg) *ServiceError
func IsServiceError(m *nats.Msg) bool // has Nats-Service-Error-Code

type PayloadTooLargeError struct{ Limit, Actual, HeaderBytes int }
type StreamStalledError struct{ Timeout time.Duration; ReplySubject string }
type StreamMaxWaitError struct{ MaxWait time.Duration }
type ProtocolError struct{ Msg string; Err error }
type QueryAlreadyRepliedError struct{ ID string }
type VersionMismatchError struct{ Agent, SDK string }
```

`StreamStalledError` and `StreamMaxWaitError` match `ErrAgent` and their own sentinel but not `ErrProtocol`: a timeout is not
a malformed peer. (Python subclasses them from `ProtocolError`; TypeScript does not.)

## 4. Package `caller`

### 4.1 Client

```go
type Client struct{ /* unexported; safe for concurrent use (REQ-019) */ }

type Option func(*config) error

func New(nc *nats.Conn, opts ...Option) (*Client, error)

func WithStreamInactivityTimeout(d time.Duration) Option // default DefaultStreamInactivityTimeout = 60s
func WithClientMaxWait(d time.Duration) Option           // default DefaultMaxWait = 10m; d <= 0 is an error
func WithLogger(l *slog.Logger) Option
func WithIdentity(o identity.Options) Option             // omitted: no lookup, no header (parity 10.15)
func WithResolveTTL(d time.Duration) Option              // default 10s; 0 enumerates every call
func WithInterceptors(i ...Interceptor) Option
func WithRequestHeaders(fn func(ctx context.Context, p *PromptInfo) (map[string]string, error)) Option
func WithProtocolPolicy(p ProtocolPolicy) Option         // default RequireExact (REQ-002)
func WithOfflineSlack(n float64) Option                  // default agents.DefaultLivenessSlack (3)
func WithStreamBuffer(n int) Option                      // per-stream queue bound; default 1024

type ProtocolPolicy int
const ( RequireExact ProtocolPolicy = iota; RequireSameMajor; AcceptAny )

const (
    DefaultStreamInactivityTimeout = 60 * time.Second
    DefaultMaxWait                 = 10 * time.Minute
    DefaultStatusTimeout           = 2 * time.Second
    DefaultDiscoverStall           = 750 * time.Millisecond
    DefaultDiscoverMaxWait         = 2 * time.Second
    DefaultRequestSignedTimeout    = 2 * time.Second
)
```

Discovery, tracking, liveness:

```go
type DiscoverOption func(*discoverConfig)
func WithDiscoverTimeout(d time.Duration) DiscoverOption // timer strategy: wait exactly d
func WithDiscoverStall(d time.Duration) DiscoverOption
func WithDiscoverMaxWait(d time.Duration) DiscoverOption
func WithFilter(f agents.Filter) DiscoverOption

// Discover publishes $SRV.INFO.agents (SAP §4.1; the official SDKs do the same), after starting the heartbeat
// tracker and flushing it (§8.5). No responders is an empty slice, not an error.
func (c *Client) Discover(ctx context.Context, opts ...DiscoverOption) ([]*Agent, error)
func (c *Client) LookupInstance(ctx context.Context, instanceID string, timeout ...time.Duration) (*Agent, error) // ErrAgentNotFound
func (c *Client) Ping(ctx context.Context, instanceID string, timeout ...time.Duration) (bool, error)
func (c *Client) StartTracking(ctx context.Context) error
func (c *Client) Liveness(instanceID string) (Liveness, bool)
func (c *Client) OnHeartbeat(instanceID string, fn func(agents.HeartbeatPayload)) (cancel func())
func (c *Client) OnAnyHeartbeat(fn func(agents.HeartbeatPayload)) (cancel func())
```

Identity and signing (parity 10.18, 10.41, 10.44 to 10.46):

```go
func (c *Client) SelfID(ctx context.Context) (identity.AgentID, error)
func (c *Client) RefreshSelfID(ctx context.Context) (identity.AgentID, error)
func (c *Client) ResolveSender(ctx context.Context, id identity.AgentID) (*agents.Info, error) // nil, nil: not a reachable agent

type SignOption func(*signConfig)
func WithSub(sub string) SignOption          // only behind a rename by the caller's own account
func WithNonce(nonce string) SignOption
func WithHeaders(h nats.Header) SignOption

func (c *Client) SignSender(ctx context.Context, subject string, payload []byte, opts ...SignOption) (string, error)
func (c *Client) PublishSigned(ctx context.Context, subject string, payload []byte, opts ...SignOption) error // Nats-Msg-Id = nonce
func (c *Client) RequestSigned(ctx context.Context, subject string, payload []byte, timeout time.Duration, opts ...SignOption) (*nats.Msg, error)
```

Lifecycle:

```go
func (c *Client) Connection() *nats.Conn
func (c *Client) IsClosed() bool
func (c *Client) Done() <-chan struct{} // closed on Close
func (c *Client) Close(ctx context.Context) error // see section 9
func (c *Client) Drain(ctx context.Context) error
```

### 4.2 Agent and Prompt

```go
type Agent struct{ /* immutable; safe for concurrent use */ }

func (a *Agent) Info() agents.Info
func (a *Agent) InstanceID() string
func (a *Agent) PromptSubject() string
func (a *Agent) Client() *Client

type PromptOption func(*promptConfig)
func WithAttachments(a ...agents.Attachment) PromptOption
func WithInactivityTimeout(d time.Duration) PromptOption
func WithMaxWait(d time.Duration) PromptOption
func WithSubject(subject string) PromptOption       // publish elsewhere (remapping import)
func WithSignedSubject(sub string) PromptOption     // sign a different subject

func AttachmentFromFile(path string) (agents.Attachment, error) // base name only

// Prompt performs the §5.4 local checks (empty prompt, attachments_ok, max_payload including the Agent-Sender
// header bytes, min_sender_trust against the caller's ability to sign, protocol policy) and returns before any
// publish when one fails. The publish happens when Messages is first ranged over, signed at that moment so ts and
// nonce are fresh; an error found then (identity, exact size) is the first yielded error.
func (a *Agent) Prompt(ctx context.Context, text string, opts ...PromptOption) (*Stream, error)
func (a *Agent) PromptEnvelope(ctx context.Context, e agents.Envelope, opts ...PromptOption) (*Stream, error)

type StatusOption func(*statusConfig)
func WithStatusTimeout(d time.Duration) StatusOption
func WithStatusSubject(subject string) StatusOption
func WithStatusSignedSubject(sub string) StatusOption
func (a *Agent) Status(ctx context.Context, opts ...StatusOption) (agents.HeartbeatPayload, error)
```

`Prompt` does not publish until iteration (TS does the same: `requestMany` runs inside the iterator) so that the signed
header's `ts` is fresh. The subscription for the reply is registered, and the connection flushed once at `Client`
start, before the first publish (§6.1).

### 4.3 Stream and messages

```go
type Stream struct{ /* single consumer */ }

func (s *Stream) Messages() iter.Seq2[Message, error]
func (s *Stream) Close() error          // drop the reply registration; idempotent; safe concurrent with Messages
func (s *Stream) Subject() string
func (s *Stream) Terminated() bool      // the zero-byte terminator was observed
func (s *Stream) Err() error            // the error that ended the sequence, if any

var ErrStreamConsumed error

// Message is sealed: Response, Status or *Query.
type Message interface {
    Header() nats.Header // read-only view of the received message's headers (REQ-016)
    isMessage()
}
type Response struct {
    Text        string
    Attachments []agents.WireAttachment
    // Header() via embedded meta
}
type Status struct{ Value string /* any token passes through; "ack" is agents.StatusAck */ }

type Query struct {
    ID, Prompt, ReplySubject string
    Attachments              []agents.WireAttachment
}
// Reply publishes plain text (the §5.3 shorthand). Exactly once: a second call returns *QueryAlreadyRepliedError.
func (q *Query) Reply(ctx context.Context, answer string) error
func (q *Query) ReplyEnvelope(ctx context.Context, e agents.Envelope) error
```

A malformed chunk of a known type is dropped and the stream continues (TS behaviour, parity 5.10); an unknown `type` and an
unknown status value are ignored (MUSTs, SAP:L398, L376). An error-headered message is inspected **before** the body of
every message (SAP §9) and ends the sequence with `*agents.ServiceError`; the terminator that follows is consumed.

### 4.4 Interceptors and the outgoing-header hook (REQ-016)

```go
type Interceptor interface {
    // BeforePrompt is phase one: what the prompt carries. No side effects. An error fails the prompt at the
    // first iteration and nothing is sent.
    BeforePrompt(ctx context.Context, p *PromptInfo) (*Extras, error)
}
// PublishInterceptor is the optional phase two, found by type assertion. It runs after signing and the size check,
// immediately before the publish. An error is logged and the prompt still goes out.
type PublishInterceptor interface {
    BeforePublish(ctx context.Context, p *PromptInfo, extras *Extras)
}
type InterceptorFunc func(ctx context.Context, p *PromptInfo) (*Extras, error)

type PromptInfo struct {
    Agent          *Agent
    Prompt         string
    EnvelopeExtras map[string]json.RawMessage // read-only copy; empty for a text prompt
    Connection     *nats.Conn
    Signing        Signing
}
type Extras struct {
    Fields  map[string]any    // extra top-level envelope fields; "prompt" and "attachments" refused
    Headers map[string]string // extra message headers; SAP-owned headers refused
    State   any               // handed back to BeforePublish; never sent
}
type Signing interface {
    CanSign() bool
    SelfID(ctx context.Context) (identity.AgentID, error)
    PublishSigned(ctx context.Context, subject string, payload []byte, opts ...SignOption) error
}
```

`WithRequestHeaders(fn)` is the narrow form of the same hook: it returns headers to add and nothing else. A header the
protocol defines or relies on cannot be set by either form: `Agent-Sender`, `Nats-Service-Error`,
`Nats-Service-Error-Code`, `Nats-Request-Info`, `Nats-Msg-Id` (compared case-insensitively). Setting one fails the
prompt with `agents.ErrReservedHeader` before anything is published. Upstream refuses only `Agent-Sender`; the wider
set is SDK policy (parity 11.7).

### 4.5 Heartbeat tracker (REQ-005, parity section 8)

```go
type HeartbeatTracker struct{ /* safe for concurrent use */ }
type TrackerOption func(*trackerConfig)
func WithTrackerSlack(n float64) TrackerOption // offline = n x interval_s; default 3
func WithTrackerLogger(l *slog.Logger) TrackerOption

func NewHeartbeatTracker(nc *nats.Conn, opts ...TrackerOption) *HeartbeatTracker
func (t *HeartbeatTracker) Start(ctx context.Context) error // subscribe then flush (SAP §8.5); idempotent
func (t *HeartbeatTracker) Stop(ctx context.Context) error  // drain the subscription
func (t *HeartbeatTracker) Started() bool
func (t *HeartbeatTracker) Subject() string
func (t *HeartbeatTracker) Liveness(instanceID string, now ...time.Time) (Liveness, bool)
func (t *HeartbeatTracker) OnHeartbeat(instanceID string, fn func(agents.HeartbeatPayload)) (cancel func())
func (t *HeartbeatTracker) OnAnyHeartbeat(fn func(agents.HeartbeatPayload)) (cancel func())

type Liveness struct {
    InstanceID string
    LastSeen   time.Time
    Interval   time.Duration
    Online     bool // now - LastSeen <= slack x Interval
}
```

Listeners run synchronously on the subscription's goroutine, each under `recover`, in registration order, and must not
block. The tracker keys on the payload's `instance_id`, not the subject. A malformed heartbeat is dropped (debug log).

### 4.6 Sender resolver (parity 10.41)

```go
type SenderResolver struct{ /* safe for concurrent use */ }
func NewSenderResolver(nc *nats.Conn, ttl time.Duration, opts ...ResolverOption) *SenderResolver
func (r *SenderResolver) Resolve(ctx context.Context, id identity.AgentID) (*agents.Info, error)
func (r *SenderResolver) Invalidate()
```

Enumerates `$SRV.INFO.agents`, indexes by `(account, user_nkey)`, drops any instance whose `id_sig` fails, keeps the
first verified one. It identifies and never authorizes.

## 5. Package `host`

### 5.1 Host

```go
type Host struct{ /* safe for concurrent use */ }

// PromptHandler serves one prompt. Returning nil ends the stream with the terminator. Returning an error answers
// with a §9 error frame then the terminator: *RejectError uses its code; an error wrapping agents.ErrProtocol is 400;
// any other error is 500 "handler error" and its text never reaches the caller. ctx ends when the host stops.
type PromptHandler func(ctx context.Context, req *Request, w *Responder) error

func New(nc *nats.Conn, subject agents.Subject, handler PromptHandler, opts ...Option) (*Host, error)

func (h *Host) Start(ctx context.Context) error // register, flush, publish the first heartbeat
func (h *Host) Stop(ctx context.Context) error  // graceful; see section 9
func (h *Host) Close(ctx context.Context) error // stop at once, wait for exit until ctx; see section 9
func (h *Host) InstanceID() string              // after Start
func (h *Host) Subject() agents.Subject
func (h *Host) Identity() (identity.AgentID, bool)
func (h *Host) MinSenderTrust() agents.MinSenderTrust
func (h *Host) OperatorAttested() bool
func (h *Host) Service() micro.Service          // escape hatch for runtime-dynamic endpoints (TS `service`)
```

Options:

```go
type Option func(*config) error

func WithDescription(s string) Option                 // default "<agent> agent <name>"
func WithVersion(v string) Option                     // service.version; default "0.0.1"
func WithSession(s string) Option                     // metadata.session, only when set
func WithMaxPayload(n int64) Option                   // default nc.MaxPayload(); clamped down to it with a warning
func WithAttachmentsOK(b bool) Option                 // default true
func WithHeartbeatInterval(d time.Duration) Option    // default 30s; d < 1s needs WithSubSecondHeartbeats
func WithSubSecondHeartbeats() Option
func WithKeepalive(d time.Duration) Option            // default 30s; d <= 0 is an error
func WithoutKeepalive() Option
func WithExtraMetadata(m map[string]string) Option    // required and identity keys win
func WithEndpoint(e Endpoint) Option                  // repeatable; extra micro endpoints (REQ-016)
func WithLogger(l *slog.Logger) Option
func WithIdentity(o IdentityOptions) Option           // omitted: no self lookup, no registration metadata
func WithMinSenderTrust(t agents.MinSenderTrust) Option // default TrustAny; always advertised on prompt
func WithReplayWindow(d time.Duration) Option         // default 30s; also the ts skew
func WithMaxNonces(n int) Option                      // default DefaultNonceCacheMaxEntries; fail closed at the cap
func WithAdmission(f AcceptSenderFunc) Option         // REQ-016
func WithResolveTTL(d time.Duration) Option
func WithOperatorAttested(b bool) Option
func WithAccountTokenPosition(pos int) Option
func WithInterceptors(i ...Interceptor) Option
func WithHeartbeatExtras(f func() map[string]any) Option
func WithMaxConcurrentPrompts(n int) Option           // REQ-019; default DefaultMaxConcurrentPrompts
func WithBusyPolicy(p BusyPolicy) Option              // Queue (default) or Reject429

type IdentityOptions struct{ Signer identity.Signer } // zero Signer: unsigned registration, best effort

const (
    DefaultHeartbeatInterval      = 30 * time.Second
    DefaultKeepalive              = 30 * time.Second
    DefaultAttachmentsOK          = true
    DefaultMaxConcurrentPrompts   = 64
    DefaultNonceCacheMaxEntries   = 100_000
    DefaultMinSenderTrust         = agents.TrustAny
)
```

`New` validates everything the upstream constructors do and returns a typed error before anything is registered. `Start`
registers a micro service named `agents` (version, description, metadata of §3.2), registers `prompt` and `status`
with `micro.WithEndpointQueueGroup("agents")` set explicitly (SAP:L175, L547), then the extra endpoints in order,
flushes, and publishes the first heartbeat; "started" means registered at the server.

### 5.2 Extra endpoints (REQ-004, REQ-016)

```go
type Endpoint struct {
    Name       string            // must not be "prompt" or "status", nor repeat
    Subject    string            // advertised verbatim; the host does not prefix it
    QueueGroup string            // "" selects micro's default "q", as both upstream hosts do
    Handler    micro.Handler
    Metadata   map[string]string
}
```

### 5.3 Request and Responder

```go
type Request struct {
    Envelope agents.Envelope
    Sender   identity.SenderInfo // nil when no Agent-Sender header (or an unknown v)
    Subject  string              // the subject it arrived on
    Header   nats.Header         // a clone of the received headers (REQ-016)
}
func (r *Request) ResolveSender(ctx context.Context) (*agents.Info, error)

type Responder struct{ /* serialises sends; safe for concurrent use */ }
func (w *Responder) Send(ctx context.Context, text string) error // a response chunk, bare-string form
func (w *Responder) SendChunk(ctx context.Context, c agents.Chunk) error
// Ask publishes a query chunk on a fresh _INBOX.agents subject (subscribed and flushed first) and waits for one reply.
func (w *Responder) Ask(ctx context.Context, prompt string, timeout time.Duration, opts ...AskOption) (agents.Envelope, error)
func (w *Responder) AskEnvelope(ctx context.Context, e agents.Envelope, timeout time.Duration, opts ...AskOption) (agents.Envelope, error)
func WithAskAttachments(a ...agents.Attachment) AskOption
// CallerListening probes whether the caller still listens (a keep-alive ack with its own reply inbox; a
// no-responders status means gone). A best guess, not a promise.
func (w *Responder) CallerListening(ctx context.Context, timeout time.Duration) (bool, error) // default 500ms
```

`Ask` returns `agents.ErrQueryTimeout` on timeout and `ctx.Err()` on cancellation. The host sends nothing after the
terminator: `Responder` becomes inert once the handler returns, and a late `Send` returns an error.

### 5.4 Errors from the host: `Reject`

```go
type RejectError struct {
    Code        int    // one of 400, 401, 403, 404, 409, 429, 500 (SAP §9.2)
    Description string // single line, at most 200 characters on the wire
}
var ( ErrRejected, ErrInvalidCode error )
func Reject(code int, description string) error // ErrInvalidCode when code is not in the §9.2 table
```

Only §9.2 codes can be sent (REQ-004, SAP:L607); upstream accepts any code 400 to 599 (deviation D-8).

### 5.5 Host interceptors

```go
type Interceptor interface {
    // AroundRequest runs after envelope decode and sender classification, before the ack. next acks, runs the
    // interceptors after this one and the handler. Call next exactly once or return an error: returning without it
    // answers 500; a second call returns an error. An error returned after next succeeded leaves the reply standing
    // and logs a fixed line. A context passed to next reaches the handler.
    AroundRequest(ctx context.Context, info *RequestInfo, next CallNext) error
}
type CallNext func(ctx context.Context) error
type RequestInfo struct {
    Envelope agents.Envelope // unknown fields visible in Extras (§5.6)
    Sender   identity.SenderInfo
    Subject  string
    Header   nats.Header
}
```

The first interceptor listed is the outermost. An interceptor that refuses before `next` leaves the caller an error
frame and the terminator, and no ack.

### 5.6 Sender gate and nonce cache (parity 10.50 to 10.55)

```go
// AcceptSenderFunc is the admission hook (SAP §13.7.3; REQ-016). It runs for every classified prompt request
// (never status), after classification and before the ack. A false for a verified sender is 403; for a claimed or
// absent one 401 "signature required"; an error is 500 and the request is never served. The hook decides policy;
// the SDK decides none. It receives the headers and subject so an application can look at more than the sender.
type AcceptSenderFunc func(ctx context.Context, sender identity.SenderInfo, req *RequestInfo) (bool, error)

type SenderGate struct{ /* safe for concurrent use */ }
type SenderGateOptions struct {
    MinSenderTrust       agents.MinSenderTrust
    ReplayWindow         time.Duration
    AccountTokenPosition int
    Accept               AcceptSenderFunc
    NonceCache           *NonceCache
    OperatorAttested     bool
    Resolver             func(ctx context.Context, id identity.AgentID) (*agents.Info, error)
    Logger               *slog.Logger
}
func NewSenderGate(o SenderGateOptions) (*SenderGate, error)
func (g *SenderGate) AdmitPrompt(ctx context.Context, msg ClassifiableMsg) (SenderAdmission, error)
func (g *SenderGate) ClassifyStatus(ctx context.Context, msg ClassifiableMsg) (identity.SenderInfo, error) // never refuses
func (g *SenderGate) Classify(ctx context.Context, msg ClassifiableMsg) (identity.SenderInfo, *SenderRejection, error)
func (g *SenderGate) NonceCache() *NonceCache
func (g *SenderGate) MinSenderTrust() agents.MinSenderTrust
func (g *SenderGate) OperatorAttested() bool

type ClassifiableMsg struct { Subject string; Data []byte; Header nats.Header }
type SenderAdmission struct { Sender identity.SenderInfo; Rejection *SenderRejection }
type SenderRejection struct { Code int; Description, Detail string } // Detail is log-only, never on the wire

// NonceCache is bounded and fails closed. Entries expire at ts+window. Record is the authoritative check-and-set.
type NonceCache struct{ /* mutex-guarded */ }
type NonceResult int
const ( NonceRecorded NonceResult = iota; NonceReplay; NonceFull )
func NewNonceCache(window time.Duration, maxEntries int, l *slog.Logger) *NonceCache
func (c *NonceCache) Has(user, nonce string, now time.Time) bool
func (c *NonceCache) Record(user, nonce string, ts, now time.Time) NonceResult
func (c *NonceCache) Size() int
```

`NonceFull` is a refusal as a replay (401 "sender rejected"), no old entry is dropped, and the SDK logs it. Upstream
evicts the oldest buckets and warns (deviation D-10).

Dispatch order for a `prompt`: envelope 400, header parse 400, ts / sub / operator cross-check / nonce lookup /
signature 401, `signed` with an unsigned request 401, nonce record, admission hook 403 or 401 or 500, interceptors,
ack. A `status` request is classified only: a failure is logged and the reply is sent.

### 5.7 Heartbeat signing helpers

```go
type HeartbeatSigner struct { ID identity.AgentID; Signer identity.Signer }
type SignHeartbeatOptions struct {
    Sender  HeartbeatSigner
    Subject string // the heartbeat subject as published
    TS      string // the heartbeat's own ts
    Data    []byte // the exact payload bytes published
    Nonce   string // tests and vectors; default fresh
}
func SignHeartbeatHeader(ctx context.Context, o SignHeartbeatOptions) (identity.Header, error)
func SignHeartbeat(ctx context.Context, o SignHeartbeatOptions) (nats.Header, error)
```

## 6. Package `identity` (SAP §13)

A leaf package. Everything is pure except `SelfID` (uses the connection).

### 6.1 Agent ID, signers, creds

```go
type AgentID struct{ /* account, user */ }
func NewAgentID(account, user string) (AgentID, error)
func ParseAgentID(s string) (AgentID, error)
func (id AgentID) Account() string
func (id AgentID) User() string
func (id AgentID) String() string
func (id AgentID) IsZero() bool
const AgentIDPattern = `^(A[A-Z2-7]{55}|[A-Za-z0-9_-]+|\$G)\.U[A-Z2-7]{55}$`

type Signer interface {
    PublicKey() string
    JWT() string // "" when none
    Sign(ctx context.Context, data []byte) ([]byte, error)
}
type NKeySigner struct{ /* seed held privately */ }
func SignerFromSeed(seed []byte, jwt string) (*NKeySigner, error) // SU... text or raw seed
func SignerFromCreds(creds []byte) (*NKeySigner, error)
func SignerFromCredsFile(path string) (*NKeySigner, error)
func (s *NKeySigner) Wipe()
// String, GoString, LogValue and MarshalJSON all redact: only the public key is ever shown.

type Creds struct{ JWT, Seed string }
func ParseCreds(text []byte) (Creds, error)
func DecodeJWTPayload(jwt string) (map[string]any, error)
func IdentityFromJWT(jwt string) (AgentID, error)

type Options struct {
    Signer               Signer
    Name                 string // display name, at most 64 characters, no control characters
    DisableUnsignedClaim bool   // default false: send an unsigned claim when the identity is known and there is no signer
}
```

### 6.2 `Agent-Sender` header

```go
const (
    HeaderName                 = "Agent-Sender"
    HeaderVersion              = 1
    SignedInputTag             = "AGENT-SENDER-V1"
    MaxHeaderValueBytes        = 2048
    MaxNameLength              = 64
    HeaderFramingBytes         = 28
    DefaultReplayWindow        = 30 * time.Second
    RejectedDescription        = "sender rejected"
    SignatureRequiredDescription = "signature required"
    MsgIDHeader                = "Nats-Msg-Id"
)

type Header struct {
    V                                int
    Account, User, Name              string
    Sub, TS, Nonce, Sig              string
}
// String, MarshalJSON and LogValue redact Nonce and Sig.
func ParseHeader(value string) (*Header, error)  // nil, nil for an unknown v; *MalformedHeaderError otherwise
func (h Header) Serialize() string
func ValidateName(name string) error
func EncodedHeaderLength(value string) int
func MaxHeaderBytes(subject, name string) int    // a sound upper bound
func ExpectedHeaderBytes(o ExpectedOptions) int
func ReadHeaderValue(h nats.Header) (string, bool)
func FormatTimestamp(t time.Time) string
func ParseTimestamp(s string) (time.Time, error)
func IsValidNonce(s string) bool

type SignedInputFields struct{ Account, User, Subject, TS, Nonce, PayloadSHA256Hex string }
func BuildSignedInput(f SignedInputFields) []byte

type SignOptions struct {
    Signer  Signer
    ID      AgentID
    Name    string
    Sub     string
    Payload []byte
    TS      time.Time // zero = now
    Nonce   string    // "" = fresh
}
func SignHeader(ctx context.Context, o SignOptions) (Header, error)
func ClaimHeader(id AgentID, name string) Header
```

### 6.3 Verification and own identity

```go
type Trust int
const ( TrustAbsent Trust = iota; TrustClaimed; TrustVerified )

// SenderInfo is a VerifiedSender or a ClaimedSender. A nil SenderInfo means absent.
type SenderInfo interface { Trust() Trust; DisplayName() string; Header() Header }
type VerifiedSender struct {
    ID              AgentID
    AccountAttested bool // only under operator-attested mode with a matching server stamp
    // name and header via the interface
}
type ClaimedSender struct{ Account, User string /* deliberately no AgentID */ }
func FormatSender(s SenderInfo) string

type VerifyMode int
const ( Live VerifyMode = iota; Stored )

type VerifyOptions struct {
    Mode                 VerifyMode
    AccountTokenPosition int                           // 1-based; 0 = none
    ReplayWindow         time.Duration                 // Live; default DefaultReplayWindow
    Now                  func() time.Time
    NonceSeen            func(user, nonce string) bool // Live: lookup only; recording is the receiver's job
    OperatorAttested     bool
    Header               nats.Header                   // consulted only under OperatorAttested (Nats-Request-Info)
}
// VerifyHeader checks, cheapest first: ts window (Live), sub acceptance, operator cross-check, nonce lookup
// (Live), signature (nkeys verify). An unsigned header returns a ClaimedSender without checks.
// Errors are *VerificationError carrying the wire code (401 or 403) and a log-only detail.
func VerifyHeader(h Header, arrivalSubject string, payload []byte, o VerifyOptions) (SenderInfo, error)

type Message struct{ Subject string; Data []byte; Header nats.Header }
// VerifySender reads and parses the header; (nil, nil) for none or an unknown v; *MalformedHeaderError -> 400.
// Stored mode proves authorship only: no time window, no nonce set, redelivery accepted (SAP §13.8).
func VerifySender(msg Message, mode VerifyMode, o ...VerifyOption) (SenderInfo, error)

func CheckSubjectAcceptance(account, sub, arrival string, accountTokenPosition int) error
func NormalizeAccountTokenPosition(p int) (int, error)

const RequestInfoHeader = "Nats-Request-Info"
type RequestInfoStamp struct{ Account, User string }
func ParseRequestInfo(value string) (*RequestInfoStamp, error)
func ReadRequestInfo(h nats.Header) (*RequestInfoStamp, bool, error)

// id_sig (§13.10)
const AgentIDSignedInputTag = "AGENT-ID-V1"
const ( MetadataUserNKey = "user_nkey"; MetadataAccount = "account"; MetadataIDSig = "id_sig" )
func BuildAgentIDSignedInput(f AgentIDSignedInputFields) []byte
func SignAgentID(ctx context.Context, s Signer, id AgentID, agent, owner, promptSubject string) (string, error)
func VerifyAgentID(metadata map[string]string, promptSubject string) bool

// Own identity (§13.4)
const ( UserInfoSubject = "$SYS.REQ.USER.INFO"; SelfIDTimeout = 2 * time.Second; SelfIDNegativeTTL = 30 * time.Second )
type SelfID struct{ /* per connection; owned by a Client or Host */ }
func NewSelfID(nc *nats.Conn, signer Signer, l *slog.Logger) *SelfID
func (s *SelfID) ID(ctx context.Context) (AgentID, error)      // memoised; a failure is retried after SelfIDNegativeTTL
func (s *SelfID) Refresh(ctx context.Context) (AgentID, error) // discard and look up again
func (s *SelfID) Peek() (AgentID, error, bool)
func (s *SelfID) Close()
func IdentityFromUserInfoReply(reply []byte) (AgentID, error)
```

`SelfID` retries an unknown identity after `SelfIDNegativeTTL` (SAP:L851) and never keeps a failure for the connection's
life. It watches `nc.StatusChanged(nats.CONNECTED)` to drop the memo on reconnect, because the identity can differ
across a reconnect (TS clears on reconnect; Python re-looks-up on every request, parity 10.14). It starts one goroutine
that exits on `Close`. A connection whose identity is none or unknown sends no `Agent-Sender` and fails locally against a
`signed` endpoint.

Error types: `ErrIdentity` (base), `*NoIdentityError`, `*UnavailableError`, `*MismatchError`, `*InvalidAgentIDError`,
`*MalformedHeaderError`, `*SignatureRequiredError{Subject}` (code 401), `*VerificationError{Code, Description, Detail}`.
Each matches `ErrIdentity` and `agents.ErrAgent` through `internal/errbase`.

## 7. Package `tools` (AgentTools; parity section 12)

```go
type Tools struct{ /* safe for concurrent use */ }
type Option func(*config) error

func New(client *caller.Client, opts ...Option) (*Tools, error) // refuses a nonsensical tool set (parity 12.4)

func WithTools(names ...string) Option
func WithSelfAddress(addr string) Option
func WithDiscoverTimeout(d time.Duration) Option
func WithMaxWait(d time.Duration) Option         // default 10 min
func WithMaxWaitAgent(d time.Duration) Option    // default: the runtime limit
func WithMaxCalls(n int) Option                  // default DefaultMaxCalls = 256
func WithAttachmentRoots(roots ...string) Option
func WithStagingDir(dir string) Option
func WithMaxSavedBytesPerCall(n int64) Option    // default 64 MiB
func WithOnSettled(f func(CallResult, SettledInfo)) Option
func WithExtensions(e ...Extension) Option
func WithLogger(l *slog.Logger) Option

const DefaultMaxCalls = 256
const QuestionRefusal = "no: the caller stopped waiting for this prompt and cannot answer" // the upstream constant
const ( ToolDiscoverAgents = "discover_agents"; ToolPromptAgent = "prompt_agent"; ToolWaitAgent = "wait_agent";
        ToolAnswerAgent = "answer_agent"; ToolCancelAgent = "cancel_agent"; ToolListAgentCalls = "list_agent_calls" )
func ToolNames() []string
func BlockingTools() []string

type Definition struct {
    Name        string
    Description string
    Parameters  json.RawMessage // JSON Schema
}
func ToolDefinitions() []Definition             // go:embed of the vendored fixtures
func (t *Tools) Definitions() []Definition      // only the offered tools, in section-1 order

type ExecuteOption func(*executeConfig)
func WithToolCallID(id string) ExecuteOption
type Result = map[string]any                    // JSON-serialisable; a refusal is {"error": "..."}
func (t *Tools) Execute(ctx context.Context, name string, args json.RawMessage, opts ...ExecuteOption) (Result, error)

func (t *Tools) Interceptor() host.Interceptor  // opens the prompt scope around the handler
func (t *Tools) RunInPromptScope(ctx context.Context, callerID string, fn func(ctx context.Context) error) error
func (t *Tools) Close(ctx context.Context) error
func ToolCallIDFromContext(ctx context.Context) (string, bool)

type CallState string
const ( StateRunning CallState = "running"; StateInputRequired CallState = "input_required"; StateCompleted CallState = "completed";
        StateFailed CallState = "failed"; StateCancelled CallState = "cancelled"; StateExpired CallState = "expired" )
type CallResult map[string]any
type ReturnedFile struct{ Filename string; SizeBytes int64; Path *string; Skipped string }
type SettledInfo struct{ Awaited bool }

// Extension hooks are optional interfaces, found by type assertion.
type Extension interface{ Name() string }
type DiscoveryFields interface { DiscoveryFields(ctx context.Context, a *caller.Agent) (map[string]any, error) }
type PromptRewriter  interface { RewritePrompt(ctx context.Context, p *PromptContext) (*PromptRewrite, error) }
type ReplyLooker     interface { LookReply(ctx context.Context, r *ReplyContext) (map[string]any, error) }
```

`Execute` returns a Go error only for a bug at the host level (parity 12.7: "errors are results"). The scope is carried in
the `context.Context`: the interceptor puts it in the `ctx` it passes to `next`, so tool calls made from the handler see
their prompt's scope, and `Execute` outside any scope uses the helper's shared one.

## 8. `connect`, `agentstest`, `cmd/sapconform`

### 8.1 `connect` (OUTSIDE SAP; deviations D-1, D-2)

```go
type ContextFile struct{ Name, Path string; Fields map[string]any }
func ReadContext(selector string) (ContextFile, error)         // "current" resolves via $NATS_CONTEXT or context.txt
func LoadContext(selector string) (Connection, error)
func ParseURL(url string) (Connection, error)                  // userinfo: token, or user:pass; credentials redacted in errors

type Connection struct{ URL string; Options []nats.Option }

type Source struct{ Context, URL, Creds, NKey string }         // exactly one source; Validate() refuses ambiguity
type BundleOption func(*bundleConfig)
func WithSignedIdentity() BundleOption                         // derive a Signer from the same credential read
func ResolveBundle(ctx context.Context, src Source, opts ...BundleOption) (*Bundle, error)
type Bundle struct{ Connection; Signer identity.Signer }
func (b *Bundle) Wipe()                                        // call only after the connection is closed

func ReconnectDefaults() []nats.Option                         // MaxReconnects(-1), ReconnectWait(2s), ReconnectJitter(200ms, 1s), RetryOnFailedConnect(true)
func WithAgentReconnectDefaults(user ...nats.Option) []nats.Option // defaults first, the caller's options override
func SignerFromContext(selector string) (identity.Signer, error)

var ErrContext error
```

Nothing in `connect` runs unless the program calls it, and nothing is read from a path the program did not name (a
context name, a creds path). `cmd/sapconform` and the examples use it.

### 8.2 `agentstest`

```go
type ReferenceAgent struct{ /* wraps host.Host */ }
type ReferenceOption func(*refConfig)
// RawHandler gets the raw micro request plus the classified sender, like the TS ReferenceAgentPromptHandler.
type RawHandler func(req micro.Request, sender identity.SenderInfo) error
func NewReferenceAgent(nc *nats.Conn, subject agents.Subject, opts ...ReferenceOption) (*ReferenceAgent, error)
func WithRawHandler(h RawHandler) ReferenceOption
func WithHost(opts ...host.Option) ReferenceOption // the reference agent defaults to a 1s heartbeat (the floor without the sub-second opt-in)
func (r *ReferenceAgent) Start(ctx context.Context) error
func (r *ReferenceAgent) Stop(ctx context.Context) error
func (r *ReferenceAgent) InstanceID() string
func (r *ReferenceAgent) PromptSubject() string
func (r *ReferenceAgent) HeartbeatSubject() string
func (r *ReferenceAgent) StatusSubject() string
func (r *ReferenceAgent) SenderGate() *identity.Gate
```

### 8.3 `cmd/sapconform` (REQ-013)

```
sapconform [-nats URL] [-context NAME] [-creds FILE] [-nkey FILE]
           [-config FILE] [-agent A -owner O -name N | -instance ID]
           [-json] [-timeout 30s] [-list]
```

- Black box over NATS only; needs nothing of the system under test beyond access.
- `-list` prints every host-role catalog obligation with its fixed capability group (checkable / needs configuration /
  not checkable, with the reason) and exits without connecting.
- A run gives every checkable or needs-configuration obligation one result: pass, fail, or configuration missing, each
  citing the obligation id and spec line. The report states it covers host obligations only.
- The checks are the catalog's checks in `internal/conform`, not a second list. Exit 0 iff no obligation failed.

## 9. Close, Drain, Stop semantics

| Call | Effect | Idempotent | Blocks until |
|---|---|---|---|
| `Stream.Close()` | Drops the reply registration (a dropped subscription is the SAP cancel, §6.7); buffered items discarded; the ranging loop ends; no wire message sent | yes | immediate |
| `Client.Close(ctx)` | Marks closed; every in-flight stream ends with `agents.ErrClosed` (close wins over buffered items, as Python's); stops the tracker (drains its subscription); drains the reply-mux subscription; stops the identity watcher; waits for every SDK goroutine to exit. Does not close `nc`. Bounded by `ctx`; without a deadline, the drain is bounded by `DefaultCloseTimeout`. On `ctx` done it returns `ctx.Err()` (wrapped), the subscriptions are unsubscribed anyway, and a later `Close` waits again | yes | goroutines exited or ctx done |
| `Client.Drain(ctx)` | Refuses new prompts with `ErrClosed`; waits for in-flight streams to end on their own; then `Close(ctx)`. On `ctx` done it forces `Close(ctx)` and returns `ctx.Err()` | yes | streams ended or ctx done |
| `HeartbeatTracker.Stop(ctx)` | Drains the subscription (pending beats are delivered), waits for it to close | yes | closed or ctx done |
| `Host.Start(ctx)` | Registers, flushes, publishes the first heartbeat, starts the heartbeat goroutine | no (second call errors) | flushed |
| `Host.Stop(ctx)` | Stops the heartbeat goroutine (a beat being signed is abandoned, a slow signer never holds teardown); stops accepting (micro drains endpoint subscriptions); waits for in-flight handlers until `ctx` is done, then cancels their contexts. A cancelled handler's stream still gets an error frame (500 "service stopping") and the terminator, so no caller waits out an inactivity timeout. Does not close `nc` | yes | handlers returned or ctx done |
| `Host.Close(ctx)` | Cancels the handlers' contexts, stops the service and heartbeat at once, then waits for the heartbeat and prompt workers to exit until `ctx` is done. On `ctx` done it returns `ctx.Err()` (wrapped); a later `Close` or `Stop` waits for the real completion | yes | workers exited or ctx done |
| `Responder.Ask` | Subscribes with `AutoUnsubscribe(1)` and unsubscribes on every exit path | n/a | reply, timeout, or ctx |

Every subscription the SDK creates is drained or unsubscribed on the corresponding close; leak checks (`goleak`-style,
without the dependency: a `runtime.NumGoroutine` and subscription-count check in the test module) run in the tests
(NFR-002). A handler that ignores its `ctx` can outlive `Stop(ctx)`: that is documented, and `Stop` returns
`ctx.Err()` in that case.

## 10. REQ-016 extension points, mapped

| REQ-016 extension point | Surface | Grounding | Notes |
|---|---|---|---|
| Per-instance agent, owner, name tokens | `agents.NewAgentSubject(agent, owner, name, ...)` passed to `host.New`; `agents.WithSubjectToken` | SAP §2, §3.2; REQ-009 | Only SAP's MUSTs enforced; SHOULDs in `Subject.Warnings()`. One process may run many `Host`s on one connection, each with its own subject. |
| Extra micro endpoints the SDK does not interpret | `host.WithEndpoint(host.Endpoint{...})`; `host.Host.Service()` | SAP §3; REQ-004 | Added after `prompt` and `status`, before the first heartbeat; collision with the protocol endpoints refused at `New`. |
| Admission hook for verified senders | `host.WithAdmission(host.AcceptSenderFunc)` | SAP §13.7.3, §9.2 (403) | Verified false is 403; claimed or absent false is 401; error is 500. Receives the headers and subject. |
| Read access to the headers of received messages | `host.Request.Header`, `host.RequestInfo.Header`, `caller.Message.Header()`, `identity.Message.Header` | SAP §9 (error headers on every message), §13.5 | A clone or read-only view: mutation does not affect SDK state. |
| Outgoing-header hook that cannot override SAP headers | `caller.WithRequestHeaders(fn)`; `caller.Extras.Headers` via `caller.Interceptor` | SDK policy (REQ-016 MUST, Confirmed in scope at ratification) | A header SAP defines or relies on fails with `agents.ErrReservedHeader` before any publish. The list is in section 4.4. |

No extension point is named for, or shaped around, one consumer (REQ-016); each is a generic protocol-level hook. The
example `examples/extend` (AT-010) uses all five on the public API only.

## 11. REQ-019 concurrency

**Caller.**

- `*caller.Client` and `*caller.Agent` are safe for concurrent use; `Agent` is immutable.
- Many prompts run at once on one `nc`. Each prompt gets its own reply subject `_INBOX.agents.<random>`, its own
  bounded queue (`WithStreamBuffer`, default 1024) and its own `Stream`. A queue that overflows ends that stream with a
  slow-consumer error instead of growing without bound or blocking other streams.
- **One mux subscription per `Client`** (`_INBOX.agents.<client-id>.>`) routes by the reply token, as both upstream SDKs
  do (TS `requestMany`, Python `MuxInbox`). The alternative, a subscription and a flush round trip per prompt, matches
  REQ-019's wording ("its own reply subscription") literally but costs a flush per prompt. Recommendation: the mux; each
  prompt still has its own reply subject and its own stream. Flagged in section 12.
- `Stream`: one consumer ranges over `Messages()`; `Close()` and `Terminated()` are safe from any goroutine.
  `Query.Reply` is safe from any goroutine and enforces exactly-once with an atomic flag.
- `HeartbeatTracker`: a `sync.RWMutex` over the entry map; listeners are copied before dispatch so a listener may
  unsubscribe from inside its own callback.
- `SelfID`: a `sync.Mutex` plus a single in-flight lookup shared by waiters (singleflight by hand, no dependency).
- `SenderResolver`: an in-flight enumeration shared by concurrent callers.

**Host.**

- A handler runs once per request, on its own goroutine, bounded by `WithMaxConcurrentPrompts` (documented default 64;
  upstream: TS unbounded, Python 1). The `nats.go` micro callback runs on the subscription's goroutine, so the host
  hands each request to a goroutine itself; this is the one place Go differs structurally from the async upstreams.
- With every slot busy the host applies the busy policy: `Queue` (default) holds the endpoint callback, which
  back-pressures that subscription as Python does at its default; `Reject429` answers `429` and the terminator at once
  (a §9.2 code).
- `status` is served inline, outside the prompt limit, so a liveness probe always gets an answer.
- `Responder` serialises its sends with a mutex: concurrent `Send` and the keep-alive ticker never interleave a chunk
  after the terminator.
- Shared state under the race detector: `NonceCache` (one mutex, O(1) amortised via second-buckets as upstream),
  `SenderGate`, `HeartbeatTracker`, `SelfID`. Tests run with `-race` (AT-001).

## 12. Open points for the coordinator

1. **Packages beyond the contract's list**: `connect` and `agentstest` (and `internal/*`). Dropping `connect` removes
   parity rows 1.3 to 1.13 and 10.10 (deviation D-1 and D-2).
2. **Mux versus per-prompt subscription** (section 11): the mux is recommended; REQ-019's wording reads as per-prompt.
3. **`DefaultMaxConcurrentPrompts = 64`** and the `Queue` default are proposals; upstream disagrees with itself (TS
   unbounded, Python 1).
4. **Discovery uses `$SRV.INFO.agents` only**, as both official callers do; REQ-003's text says PING and INFO. `Ping` keeps
   PING.
5. **Header-hook reserved set** is wider than upstream's (`Agent-Sender` only).
6. **`IntervalS float64`** and the sub-second opt-in interplay with Python callers (deviation D-9).
