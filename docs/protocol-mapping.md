# Protocol mapping

This file maps every exported identifier of the module to the section of the Synadia Agents Protocol
(SAP) 0.3 that it implements. The SAP text is at spec commit `c48cea5f07965cc8fd4054e120459f1833b3ed2b`;
section numbers (`§5.2`) refer to its `core-protocol.md`.

This file is generated. Regenerate it, from the repository root, with:

    go run ./internal/cmd/mapgen

(or `go generate ./internal/cmd/mapgen`). The section of a row comes from the SAP citations in the godoc
of the identifier; identifiers whose godoc cites none are classified by the small rules table in
`internal/cmd/mapgen/main.go`. Packages under `internal/` are not part of the API and are not mapped.

How to read it:

- **Section** is the part of SAP the identifier implements or is bounded by. "SDK policy" marks a choice the
  SDK makes where the spec leaves it open (a default, a bound, a timeout); the spec is never made stricter
  than its MUSTs by it, and a SHOULD or MAY is never turned into a MUST. "Outside SAP" marks code that SAP
  does not cover at all.
- Methods are listed as `package.Type.Method`. Fields of a struct and the enumerated values of a type are
  documented on the type; the row of the type stands for its fields.
- The godoc of each identifier (`go doc -all`) cites the same sections and is the authority for behavior.
  The completeness of this file is checked by `test/docs`: it fails when an exported identifier of a package
  of this module has no row here.

The protocol reading itself, in order, is: subjects (§2), registration (§3), discovery (§4), the request (§5),
the response stream (§6), mid-stream queries (§7), heartbeat and status (§8), errors (§9), security (§10),
versioning (§11) and the sender identity extension (§13).

## Package `agents` (root): the shared protocol types

Everything both sides share: subjects, headers, envelope, chunks, errors, heartbeats, attachments, discovery records and versions. It needs no connection.

| Identifier | Kind | Section | Note |
|---|---|---|---|
| `agents.Attachment` | type | §5.2, §5.6 | Attachment is a file carried in a request, a response chunk or a query chunk: {"filename", "content"} with... |
| `agents.NewAttachment` | func | §5.2 | NewAttachment encodes data as an attachment named filename. |
| `agents.Attachment.Bytes` | method | §4, §5.2 | Bytes decodes the content. |
| `agents.Attachment.Validate` | method | §5.2 | Validate checks the attachment as a sender: the filename is not empty and the content is strict base64. |
| `agents.Attachment.MarshalJSON` | method | §5.2, §5.6 | MarshalJSON writes {"filename", "content"} followed by the extra fields. |
| `agents.Attachment.UnmarshalJSON` | method | §5.2, §5.6 | UnmarshalJSON reads an attachment object. |
| `agents.Base64DecodedSize` | func | §4, §5.2 | Base64DecodedSize returns the number of bytes strict base64 content decodes to, and false when s is not str... |
| `agents.DefaultSaveMaxTotalBytes` | const | §5.2 (saving is SDK policy) | DefaultSaveMaxTotalBytes is the default cap on decoded bytes one [SaveAttachments] call writes: 64 MiB. |
| `agents.SafeFileName` | func | §5.2 | SafeFileName turns an untrusted attachment name into a name that is safe to create in a directory, the same... |
| `agents.SkipReason` | type | §5.2 (saving is SDK policy) | SkipReason says why [SaveAttachments] did not save an attachment. |
| `agents.SkipOverLimit` | const | §5.2 (saving is SDK policy) | SkipOverLimit means saving the attachment would pass MaxTotalBytes. |
| `agents.SkipInvalidContent` | const | §5.2 (saving is SDK policy) | SkipInvalidContent means the content is not strict base64. |
| `agents.SavedAttachment` | type | §5.2 (saving is SDK policy) | SavedAttachment is the outcome for one input attachment, in input order. |
| `agents.SaveOptions` | type | §5.2 (saving is SDK policy) | SaveOptions tunes [SaveAttachments]. |
| `agents.SaveAttachments` | func | §5.2 | SaveAttachments decodes received attachments and writes each into dir (created, mode 0700, if missing), ret... |
| `agents.ParseSize` | func | §2.1 | ParseSize parses the max_payload grammar: a positive integer followed by B, KB, MB or GB (SAP §2.1). |
| `agents.FormatSize` | func | §2.1 | FormatSize formats a byte count in the max_payload grammar, using the largest unit that divides it evenly (... |
| `agents.ChunkResponse` | const | §6.2 | Chunk types. |
| `agents.ChunkStatus` | const | §6.2 | Chunk types. |
| `agents.ChunkQuery` | const | §6.2 | Chunk types. |
| `agents.StatusAck` | const | §6.4 | StatusAck is the one defined status value. |
| `agents.DefaultInactivityTimeout` | const | §6.6 | DefaultInactivityTimeout is the per-stream inactivity timeout SAP recommends: 60 seconds since the last obs... |
| `agents.Chunk` | type | §6.2 | Chunk is a decoded stream message: a [ResponseChunk], a [StatusChunk], a [QueryChunk] or an [UnknownChunk]. |
| `agents.Chunk.Type` | method | §6.2 | Type returns the wire discriminator. |
| `agents.ResponseChunk` | type | §6.3 | ResponseChunk is content from the agent. |
| `agents.StatusChunk` | type | §6.4 | StatusChunk is a lifecycle signal; Status is "ack" or a value to ignore. |
| `agents.QueryChunk` | type | §7.2, §7.1 | QueryChunk is a mid-stream question. |
| `agents.UnknownChunk` | type | §6.2, §6.6 | UnknownChunk is a well-formed chunk of a type this SDK does not know. |
| `agents.ResponseChunk.Type` | method | §6.3 | Type returns ChunkResponse. |
| `agents.StatusChunk.Type` | method | §6.4 | Type returns ChunkStatus. |
| `agents.QueryChunk.Type` | method | §7.2, §7.1 | Type returns ChunkQuery. |
| `agents.UnknownChunk.Type` | method | §6.2, §6.6 | Type returns the unrecognized wire type. |
| `agents.DecodeChunk` | func | §6.2, §6.4, §7.1 | DecodeChunk parses a non-terminating stream message body, a JSON object {"type", "data"}. |
| `agents.EncodeChunk` | func | §6.3 | EncodeChunk serializes a chunk for publication. |
| `agents.AckChunk` | func | §6.4 | AckChunk returns the encoded ack status chunk, {"type":"status","data":"ack"}, the first message of every r... |
| `agents.ServiceName` | const | §3.1 | ServiceName is the micro service name every agent registers under. |
| `agents.QueueGroup` | const | §3.3, §8.7 | QueueGroup is the queue group the prompt and status endpoints MUST be registered with, set explicitly: the... |
| `agents.EndpointPrompt` | const | §2 | EndpointPrompt is the required prompt endpoint's name, equal to its verb. |
| `agents.EndpointStatus` | const | §8.7 | EndpointStatus is the required status endpoint's name. |
| `agents.EndpointAttachments` | const | §2, §5.5 | EndpointAttachments is the name the future attachments endpoint MUST carry. |
| `agents.MetaAgent` | const | §2.1, §3.2, §13.9 | Metadata keys. |
| `agents.MetaOwner` | const | §2.1, §3.2, §13.9 | Metadata keys. |
| `agents.MetaSession` | const | §2.1, §3.2, §13.9 | Metadata keys. |
| `agents.MetaProtocolVersion` | const | §2.1, §3.2, §13.9 | Metadata keys. |
| `agents.MetaMaxPayload` | const | §2.1, §3.2, §13.9 | Metadata keys. |
| `agents.MetaAttachmentsOK` | const | §2.1, §3.2, §13.9 | Metadata keys. |
| `agents.MetaMinSenderTrust` | const | §13.9 | MetaMinSenderTrust is the prompt endpoint metadata key whose presence advertises the sender-identity extens... |
| `agents.ServiceMetadata` | type | §3.2 | ServiceMetadata is the required service metadata of an agent. |
| `agents.ServiceMetadata.Map` | method | §3.2 | Map returns the metadata as the micro service registration carries it. |
| `agents.PromptEndpointMetadata` | type | §2.1 | PromptEndpointMetadata is the endpoint metadata the prompt endpoint MUST declare: max_payload and attachmen... |
| `agents.PromptEndpointMetadata.Map` | method | §2.1 | Map returns the metadata as the registration carries it: max_payload in the "1MB" grammar and attachments_o... |
| `agents.PromptEndpointMetadata.Limits` | method | §2.1 | Limits returns the [PromptLimits] a caller derives from this metadata. |
| `agents.SenderTrust` | type | §13.9 | SenderTrust is the level a prompt endpoint requires of senders (optional extension). |
| `agents.TrustAny` | const | §13.9 | TrustAny accepts any sender. |
| `agents.TrustSigned` | const | §13.9 | TrustSigned requires a verified sender. |
| `agents.ParseSenderTrust` | func | §13.9 | ParseSenderTrust reads a min_sender_trust value: "any" is TrustAny, and "signed" and every unknown value ar... |
| `agents.EndpointInfo` | type | §4.3 | EndpointInfo is one endpoint of a service record, as the micro framework reports it in $SRV.INFO. |
| `agents.EndpointInfo.MaxPayload` | method | §2.1 | MaxPayload returns max_payload in bytes. |
| `agents.EndpointInfo.AttachmentsOK` | method | §2.1 | AttachmentsOK returns attachments_ok. |
| `agents.EndpointInfo.MinSenderTrust` | method | §4.3 | MinSenderTrust returns the endpoint's min_sender_trust. |
| `agents.EndpointInfo.PromptLimits` | method | §2.1, §5.4 | PromptLimits returns what a caller enforces locally before publishing to this prompt endpoint. |
| `agents.ServiceInfo` | type | §4 | ServiceInfo is the micro $SRV.INFO record of a service instance. |
| `agents.DecodeServiceInfo` | func | §3, §4.1 | DecodeServiceInfo parses an $SRV.INFO response. |
| `agents.ServicePing` | type | §4, §8.4 | ServicePing is the micro $SRV.PING response of a service instance. |
| `agents.DecodeServicePing` | func | §8.4 | DecodeServicePing parses an $SRV.PING response. |
| `agents.AgentInfo` | type | §4.3 | AgentInfo is the view of an agent that a caller assembles from a service record. |
| `agents.NewAgentInfo` | func | §3.1, §3.2, §4.3, §8.7 | NewAgentInfo reads a service record as an agent. |
| `agents.AgentInfo.CheckVersion` | method | §4.3 | CheckVersion applies a [VersionPolicy] to the agent's protocol version. |
| `agents.AgentInfo.PromptLimits` | method | §4.3 | PromptLimits returns the limits of the prompt endpoint; see [EndpointInfo.PromptLimits]. |
| `agents.AgentInfo.Endpoint` | method | §4.3 | Endpoint returns the endpoint with the given name, if registered. |
| `agents.AgentInfo.SupportsSenderIdentity` | method | §13.9 | SupportsSenderIdentity reports whether the prompt endpoint declares min_sender_trust, the feature-detection... |
| `agents.Heartbeat.PromptEndpoint` | method | §8.3 | PromptEndpoint returns the declared prompt endpoint of a heartbeat as an [EndpointInfo], and false when the... |
| `agents.ErrRecordInconsistent` | var | §2, §4.3 | ErrRecordInconsistent matches the error of [AgentInfo.CheckSubjectConsistency]: a discovery record whose de... |
| `agents.ErrAgentTokenUnusual` | var | §2 | ErrAgentTokenUnusual matches the error of [AgentInfo.CheckAgentToken]: the third token of a default-form en... |
| `agents.AgentInfo.CheckAgentToken` | method | §2, §3.2, §4.3 | CheckAgentToken reports whether the third token of the record's default-form prompt and status subjects is... |
| `agents.AgentInfo.CheckSubjectConsistency` | method | §3.2, §2, §13.10 | CheckSubjectConsistency reports whether the record's endpoint subjects agree with its owner. |
| `agents.Envelope` | type | §7.2, §5.1, §5.6 | Envelope is the payload of a prompt request: a prompt and optional attachments. |
| `agents.Envelope.Validate` | method | §5.1, §5.2 | Validate applies the local MUSTs of a sender: a non-empty, valid UTF-8 prompt (SAP §5.1, [ErrPromptEmpty],... |
| `agents.Envelope.Encode` | method | §5.1 | Encode validates the envelope and serializes it as the JSON form of SAP §5.1. |
| `agents.Envelope.EncodedSize` | method | §5.4 | EncodedSize returns the byte size of the encoded envelope, the number the max_payload check of SAP §5.4 com... |
| `agents.EncodePlainPrompt` | func | §5.1, §5.3 | EncodePlainPrompt returns the plain-text shorthand of SAP §5.1 for a prompt without attachments: the prompt... |
| `agents.DecodeEnvelope` | func | §5.3, §5.1, §5.2, §5.6 | DecodeEnvelope parses a received prompt payload by the SAP §5.3 rule: a zero-byte payload is invalid; after... |
| `agents.PromptLimits` | type | §2.1 | PromptLimits is what a caller enforces locally before it publishes, read from a prompt endpoint's metadata... |
| `agents.PromptLimits.Validate` | method | §2.1, §5.4 | Validate reports a [*CapabilityError] when MaxPayload is not positive or AttachmentsOK is unset: the endpoi... |
| `agents.PromptLimits.CheckAttachments` | method | §5.4 | CheckAttachments fails with [ErrAttachmentsNotSupported] when any attachment is present and the endpoint de... |
| `agents.PromptLimits.CheckSize` | method | §5.4, §13.5.1 | CheckSize makes the two size checks of SAP §5.4, each ignored when its limit is zero. |
| `agents.PromptLimits.Prepare` | method | §5.4 | Prepare runs every local check of SAP §5.4 for a request: it requires valid limits ([PromptLimits.Validate]... |
| `agents.PromptLimits.CheckInbound` | method | §5.4, §9.2 | CheckInbound is the host's optional server-side enforcement of the same limits on a received request: attac... |
| `agents.ErrLocal` | var | §5.4 | ErrLocal matches every refusal the SDK makes locally, before anything is published: an invalid token, an em... |
| `agents.ErrInvalidToken` | var | §2.2 | ErrInvalidToken matches a [*TokenError] for a token that breaks a SAP MUST. |
| `agents.ErrTokenNotRecommended` | var | §2.2 | ErrTokenNotRecommended matches a [*TokenError] from strict validation, for a token that breaks a SAP SHOULD. |
| `agents.ErrInvalidSubject` | var | §2 | ErrInvalidSubject matches a [*SubjectError]. |
| `agents.ErrPromptEmpty` | var | §5.1 | ErrPromptEmpty is returned when a prompt is empty. |
| `agents.ErrInvalidUTF8` | var | §5.1 | ErrInvalidUTF8 is returned when text that the protocol carries as UTF-8 (a prompt, a JSON string) is not va... |
| `agents.ErrInvalidExtra` | var | §5.6 | ErrInvalidExtra matches an encoder failure caused by an Extra value that is not exactly one valid JSON value. |
| `agents.ErrInvalidCapability` | var | §2.1 | ErrInvalidCapability matches a [*CapabilityError]: a prompt endpoint whose max_payload or attachments_ok is... |
| `agents.ErrAttachmentsNotSupported` | var | §5.4 | ErrAttachmentsNotSupported is returned when attachments are present and the endpoint declared attachments_o... |
| `agents.ErrPayloadTooLarge` | var | §5.4 | ErrPayloadTooLarge matches a [*PayloadTooLargeError]. |
| `agents.ErrInvalidAttachment` | var | §5.2 | ErrInvalidAttachment matches a [*AttachmentError]. |
| `agents.ErrVersionMismatch` | var | §11.2 | ErrVersionMismatch matches a [*VersionError]. |
| `agents.ErrProtocol` | var | §6.6, §9 | ErrProtocol matches a [*ProtocolError]: a received payload that could not be interpreted as the protocol de... |
| `agents.ErrNotAgent` | var | §3.1, §4.3 | ErrNotAgent matches a [*NotAgentError]: a service record that is not an SAP agent. |
| `agents.ErrStreamStalled` | var | §6.6 | ErrStreamStalled matches a [*StreamStalledError]. |
| `agents.TokenError` | type | §2.2 | TokenError reports a subject token that a SAP rule refuses. |
| `agents.TokenError.Error` | method | §2.2 |  |
| `agents.TokenError.Is` | method | §2.2 | Is matches [ErrLocal] and either [ErrInvalidToken] or [ErrTokenNotRecommended]. |
| `agents.SubjectError` | type | §2 | SubjectError reports a string that is not a valid SAP subject. |
| `agents.SubjectError.Error` | method | §2 |  |
| `agents.SubjectError.Is` | method | §2 | Is matches [ErrInvalidSubject] and [ErrLocal]. |
| `agents.CapabilityError` | type | §2.1, §5.4 | CapabilityError reports a prompt endpoint whose required capability metadata (max_payload, attachments_ok)... |
| `agents.CapabilityError.Error` | method | §2.1, §5.4 |  |
| `agents.CapabilityError.Is` | method | §2.1, §5.4 | Is matches [ErrInvalidCapability] and [ErrLocal]. |
| `agents.PayloadTooLargeError` | type | §13.5.1, §5.4 | PayloadTooLargeError reports a request that does not fit a size limit. |
| `agents.PayloadTooLargeError.Error` | method | §13.5.1, §5.4 |  |
| `agents.PayloadTooLargeError.Is` | method | §13.5.1, §5.4 | Is matches [ErrPayloadTooLarge] and [ErrLocal]. |
| `agents.AttachmentError` | type | §5.2 | AttachmentError reports an attachment that breaks SAP §5.2. |
| `agents.AttachmentError.Error` | method | §5.2 |  |
| `agents.AttachmentError.Is` | method | §5.2 | Is matches [ErrInvalidAttachment] and [ErrLocal]. |
| `agents.VersionError` | type | §11 | VersionError reports a protocol version the SDK refuses. |
| `agents.VersionError.Error` | method | §11 |  |
| `agents.VersionError.Is` | method | §11 | Is matches [ErrVersionMismatch] and [ErrLocal]. |
| `agents.ProtocolError` | type | §6.6, §9 | ProtocolError reports a received payload that could not be interpreted as the protocol defines it (malforme... |
| `agents.ProtocolError.Error` | method | §6.6, §9 |  |
| `agents.ProtocolError.Unwrap` | method | §6.6, §9 | Unwrap returns the underlying cause, if any. |
| `agents.ProtocolError.Is` | method | §6.6, §9 | Is matches [ErrProtocol]. |
| `agents.NotAgentError` | type | §3.1, §4.3 | NotAgentError reports a service record that is not an SAP agent. |
| `agents.NotAgentError.Error` | method | §3.1, §4.3 |  |
| `agents.NotAgentError.Is` | method | §3.1, §4.3 | Is matches [ErrNotAgent]. |
| `agents.StreamStalledError` | type | §6.6 | StreamStalledError reports that a stream was silent for longer than the inactivity timeout. |
| `agents.StreamStalledError.Error` | method | §6.6 |  |
| `agents.StreamStalledError.Is` | method | §6.6 | Is matches [ErrStreamStalled]. |
| `agents.CodeBadRequest` | const | §9.2 | Status codes of the SAP error taxonomy. |
| `agents.CodeUnauthorized` | const | §9.2 | Status codes of the SAP error taxonomy. |
| `agents.CodeForbidden` | const | §9.2 | Status codes of the SAP error taxonomy. |
| `agents.CodeNotFound` | const | §9.2 | Status codes of the SAP error taxonomy. |
| `agents.CodeConflict` | const | §9.2 | Status codes of the SAP error taxonomy. |
| `agents.CodeRateLimited` | const | §9.2 | Status codes of the SAP error taxonomy. |
| `agents.CodeInternal` | const | §9.2 | Status codes of the SAP error taxonomy. |
| `agents.KnownCode` | func | §9.2 | KnownCode reports whether code is in the SAP §9.2 taxonomy. |
| `agents.ErrorBody` | type | §9.1 | ErrorBody is the optional JSON body of an error response. |
| `agents.ServiceError` | type | §9 | ServiceError is an error response as SAP §9 defines it: a status code, the short Nats-Service-Error descrip... |
| `agents.ErrBadRequest` | var | §9.2 | Sentinels for the §9.2 codes. |
| `agents.ErrUnauthorized` | var | §9.2 | Sentinels for the §9.2 codes. |
| `agents.ErrForbidden` | var | §9.2 | Sentinels for the §9.2 codes. |
| `agents.ErrNotFound` | var | §9.2 | Sentinels for the §9.2 codes. |
| `agents.ErrConflict` | var | §9.2 | Sentinels for the §9.2 codes. |
| `agents.ErrRateLimited` | var | §9.2 | Sentinels for the §9.2 codes. |
| `agents.ErrInternal` | var | §9.2 | Sentinels for the §9.2 codes. |
| `agents.NewServiceError` | func | §9.2 | NewServiceError builds an error response with the given code and description. |
| `agents.ServiceError.Message` | method | §9.1 | Message returns the human-readable detail: the body's message when the body carries one, else the header de... |
| `agents.ServiceError.Error` | method | §9 |  |
| `agents.ServiceError.Is` | method | §9 | Is reports whether target is one of the Err... |
| `agents.ServiceError.Headers` | method | §9.1 | Headers returns the NATS headers of an error response: the code and the sanitized description. |
| `agents.ServiceError.Payload` | method | §9.1 | Payload returns the JSON body of the error response, or nil when there is no body. |
| `agents.IsCode` | func | §9.2 | IsCode reports whether err's chain holds a [*ServiceError] with the given code. |
| `agents.CodeOf` | func | §9.1 | CodeOf returns the status code of the first [*ServiceError] in err's chain. |
| `agents.SanitizeErrorDescription` | func | §9.1 | SanitizeErrorDescription makes a description safe for a NATS header: line breaks are flattened (a newline i... |
| `agents.ParseServiceError` | func | §9.1, §6.5 | ParseServiceError reads an error response from a received message's headers and body. |
| `agents.SizeError` | type | §2.1 | SizeError reports a max_payload value that is not in the SAP §2.1 grammar. |
| `agents.SizeError.Error` | method | §2.1 |  |
| `agents.HeaderServiceErrorCode` | const | §9.1 | HeaderServiceErrorCode carries the numeric status code of an error response, as a string. |
| `agents.HeaderServiceError` | const | §9.1 | HeaderServiceError carries the short human-readable description of an error response. |
| `agents.HeaderAgentSender` | const | §13.5 | HeaderAgentSender carries the sender identity of a request or heartbeat (optional extension). |
| `agents.HeaderRequestInfo` | const | §13.1 | HeaderRequestInfo is the header the NATS server stamps on a request that crosses an account boundary. |
| `agents.Header` | type | §9, §13.5 | Header is the shape of NATS message headers (nats.Header and http.Header both have this underlying type, so... |
| `agents.HeaderValue` | func | §13.5.1 | HeaderValue returns the first value of the header with exactly this name and whether it is present. |
| `agents.IsErrorSignal` | func | §9.1, §12 | IsErrorSignal reports whether a received message carries the error code header, that is, whether it is an e... |
| `agents.IsTerminator` | func | §6.5, §9.3 | IsTerminator reports whether a received message is the end-of-stream terminator: a zero-byte body carrying... |
| `agents.DefaultHeartbeatInterval` | const | §8.2 | DefaultHeartbeatInterval is the recommended cadence, 30 seconds. |
| `agents.MinRecommendedHeartbeatInterval` | const | §8.2 | MinRecommendedHeartbeatInterval is the floor below which SAP says heartbeats SHOULD NOT be sent on shared i... |
| `agents.OfflineThresholdFactor` | const | §8.2 | OfflineThresholdFactor is the recommended offline threshold: 3 times interval_s since the last observed hea... |
| `agents.MaxHeartbeatInterval` | const | §8.3 | MaxHeartbeatInterval is the ceiling this SDK applies to a heartbeat cadence: 24 hours. |
| `agents.Heartbeat` | type | §8.3, §8.7 | Heartbeat is the payload of a heartbeat, published on agents.hb.{agent}.{owner}.{name}, and of a status rep... |
| `agents.HeartbeatEndpoint` | type | §2.1, §8.3 | HeartbeatEndpoint is one declared endpoint of a heartbeat: its registered subject and its endpoint metadata... |
| `agents.HeartbeatEndpoint.MarshalJSON` | method | §2.1, §8.3 | MarshalJSON writes nil Metadata as {} and never as null: SAP §2.1 endpoint metadata is an object, and a rec... |
| `agents.Heartbeat.Interval` | method | §8.3 | Interval returns IntervalS as a duration. |
| `agents.Heartbeat.OfflineAfter` | method | §8.2 | OfflineAfter returns the recommended offline threshold for this instance: [OfflineThresholdFactor] times it... |
| `agents.Heartbeat.Time` | method | §8.3, §8.7 | Time parses TS, which [DecodeHeartbeat] and [Heartbeat.Validate] guarantee is an RFC 3339 UTC timestamp. |
| `agents.Heartbeat.Stamp` | method | §8.3, §8.7 | Stamp sets TS to t in UTC, in the form of SAP's example ("2026-04-21T14:23:01Z"). |
| `agents.Heartbeat.SetInterval` | method | §8.3, §8.7 | SetInterval sets IntervalS from d. |
| `agents.Heartbeat.Validate` | method | §8.3 | Validate checks a heartbeat about to be sent: agent, owner and instance_id are not empty, ts is an RFC 3339... |
| `agents.ValidateInterval` | func | §8.2, §8.3 | ValidateInterval applies the SAP §8.2 advice to a configured cadence. |
| `agents.EncodeHeartbeat` | func | §8.3, §8.7 | EncodeHeartbeat validates and serializes a heartbeat or status reply. |
| `agents.DecodeHeartbeat` | func | §8.3, §8.7 | DecodeHeartbeat parses a heartbeat or status reply. |
| `agents.SubjectRoot` | const | §2 | SubjectRoot is the fixed first token of every protocol subject, reserved for the protocol. |
| `agents.Verb` | type | §2 | Verb is the endpoint discriminator, the second token of a protocol subject. |
| `agents.VerbPrompt` | const | §2 | The reserved verbs. |
| `agents.VerbHeartbeat` | const | §2 | The reserved verbs. |
| `agents.VerbStatus` | const | §2 | The reserved verbs. |
| `agents.VerbAttachments` | const | §2 | The reserved verbs. |
| `agents.Verb.Valid` | method | §2 | Valid reports whether v is one of the four reserved verbs. |
| `agents.SubjectPingAll` | const | §4 | SubjectPingAll is the general discovery subject: every agent instance answers once. |
| `agents.SubjectInfoAll` | const | §4.1 | SubjectInfoAll is the general info subject: every instance answers with its full service info. |
| `agents.SubjectHeartbeatAll` | const | §2, §8.5 | SubjectHeartbeatAll is the wildcard for every heartbeat. |
| `agents.SubjectAll` | const | §2.4 | SubjectAll matches every protocol subject. |
| `agents.AgentSubject` | type | §2, §4.3 | AgentSubject names one agent instance: the owner and instance name, the agent identifier and the token used... |
| `agents.AgentSubject.Agent` | method | §2, §4.3 | Agent returns the canonical harness identifier (metadata.agent). |
| `agents.AgentSubject.Owner` | method | §2, §4.3 | Owner returns the operator or account (metadata.owner), the 4th token. |
| `agents.AgentSubject.Name` | method | §2, §4.3 | Name returns the instance name, the 5th token, which lives only in the subject. |
| `agents.AgentSubject.Token` | method | §2 | Token returns the 3rd subject token: the agent identifier unless an abbreviation is in use ("cc" for "claud... |
| `agents.AgentSubject.IsZero` | method | §2, §4.3 | IsZero reports whether s is the empty value, which names no instance. |
| `agents.SubjectOption` | type | §2.2 | SubjectOption customizes [NewAgentSubject]. |
| `agents.WithSubjectToken` | func | §2 | WithSubjectToken sets the 3rd subject token apart from the agent identifier, for the conventional abbreviat... |
| `agents.NewAgentSubject` | func | §2.2 | NewAgentSubject validates the three identifying tokens against the SAP §2.2 MUSTs (and no more) and returns... |
| `agents.AgentSubject.Subject` | method | §2 | Subject returns agents.{verb}.{token}.{owner}.{name}. |
| `agents.AgentSubject.Prompt` | method | §2 | Prompt returns the default prompt subject, agents.prompt.{agent}.{owner}.{name}. |
| `agents.AgentSubject.Heartbeat` | method | §8.1 | Heartbeat returns the heartbeat subject, agents.hb.{agent}.{owner}.{name}. |
| `agents.AgentSubject.Status` | method | §8.7 | Status returns the default status subject, agents.status.{agent}.{owner}.{name}. |
| `agents.AgentSubject.Attachments` | method | §5.5 | Attachments returns the default subject reserved for the future attachments endpoint. |
| `agents.AgentSubject.Warnings` | method | §2.2 | Warnings returns the SAP §2.2 SHOULDs the identifying tokens and the longest subject do not meet. |
| `agents.ParseSubject` | func | §2 | ParseSubject parses agents.{verb}.{agent}.{owner}.{name} into its verb and an [AgentSubject]. |
| `agents.ParseAgentSubject` | func | §2 | ParseAgentSubject is [ParseSubject] that also requires the given verb. |
| `agents.IsHeartbeatSubject` | func | §8.1 | IsHeartbeatSubject reports whether subject is agents.hb.{agent}.{owner}.{name}. |
| `agents.InstanceNameFromSubject` | func | §4.3 | InstanceNameFromSubject returns the instance name, the 5th token, of an endpoint subject that follows the d... |
| `agents.FilterSubject` | func | §2.4 | FilterSubject builds a wildcard subject for subscribing to one verb across agents. |
| `agents.InfoSubject` | func | §4, §4.2 | InfoSubject returns $SRV.INFO.agents.{instance_id}, the direct lookup of one instance. |
| `agents.PingSubject` | func | §4, §8.4 | PingSubject returns $SRV.PING.agents.{instance_id}, the micro service ping of one instance. |
| `agents.InstanceIDFromInfoSubject` | func | §4.2 | InstanceIDFromInfoSubject returns the instance id of a $SRV.INFO.agents.{instance_id} subject. |
| `agents.ValidatePublishSubject` | func | §7.1 | ValidatePublishSubject checks that s is syntactically fit to publish to: not empty, no empty token, no wild... |
| `agents.RoleAgent` | const | §2 | Token roles, used in error messages. |
| `agents.RoleOwner` | const | §2 | Token roles, used in error messages. |
| `agents.RoleName` | const | §2 | Token roles, used in error messages. |
| `agents.RoleSubjectToken` | const | §2 | Token roles, used in error messages. |
| `agents.RoleInstanceID` | const | §2 | Token roles, used in error messages. |
| `agents.MaxRecommendedTokenLen` | const | §2.2 | MaxRecommendedTokenLen is the longest token SAP recommends. |
| `agents.MaxRecommendedSubjectLen` | const | §2.2 | MaxRecommendedSubjectLen is the bound a fully qualified subject SHOULD stay under. |
| `agents.ValidateToken` | func | §2.2 | ValidateToken enforces the MUSTs SAP puts on a subject token and nothing stricter: the token is not empty,... |
| `agents.TokenWarnings` | func | §2.2 | TokenWarnings returns the SAP §2.2 SHOULDs the token does not meet: the character set a-z 0-9 - _, and a le... |
| `agents.IsRecommendedToken` | func | §2.2 | IsRecommendedToken reports whether token is within SAP's SHOULD-level form (a-z 0-9 - _, 1 to 63 characters). |
| `agents.ValidateTokenStrict` | func | §2.2 | ValidateTokenStrict is the opt-in strict mode: the MUSTs of [ValidateToken] and then the SHOULDs of [TokenW... |
| `agents.SubjectWarnings` | func | §2.2 | SubjectWarnings returns the SAP §2.2 SHOULD that a fully qualified subject stay under 256 characters, when... |
| `agents.ProtocolVersion` | const | §3.2, §11.3 | ProtocolVersion is the MAJOR.MINOR protocol version this SDK implements and that a host declares as metadat... |
| `agents.ProtocolMajor` | const | §11.1 | ProtocolMajor is the MAJOR part of [ProtocolVersion]. |
| `agents.ProtocolMinor` | const | §11.1 | ProtocolMinor is the MINOR part of [ProtocolVersion]. |
| `agents.SpecVersion` | const | §1.2 | SpecVersion is the full version of the specification the SDK was built against. |
| `agents.SpecCommit` | const | §1.2 | SpecCommit is the commit of core-protocol.md in synadia-ai/synadia-agent-sdk-docs that the SDK is pinned to. |
| `agents.Version` | type | §11.1 | Version is a protocol version reduced to its MAJOR.MINOR prefix, the only part with compatibility meaning. |
| `agents.SDKVersion` | func | §11.3 | SDKVersion returns the protocol version this SDK implements. |
| `agents.Version.String` | method | §11.1 | String returns the MAJOR.MINOR form. |
| `agents.ParseVersion` | func | §11.1 | ParseVersion parses a protocol version string. |
| `agents.Compatibility` | type | §11.2 | Compatibility is how two protocol versions relate. |
| `agents.Compatible` | const | §11.2 | Compatible means the same MAJOR.MINOR: full interoperability. |
| `agents.MinorDrift` | const | §11.2 | MinorDrift means the same MAJOR and a different MINOR: callers SHOULD treat the agent as compatible for the... |
| `agents.Incompatible` | const | §11.2 | Incompatible means a different MAJOR: no interoperability guarantee. |
| `agents.Compatibility.String` | method | §11.2 | String returns a short name for c. |
| `agents.Version.Compare` | method | §11.2 | Compare reports how an agent's version v relates to the SDK's own. |
| `agents.VersionPolicy` | type | §11.2 | VersionPolicy decides which agent protocol versions a caller accepts. |
| `agents.VersionDefault` | const | §11.2 | VersionDefault is the SDK default: an exact MAJOR.MINOR match while the protocol is 0.x, and the same MAJOR... |
| `agents.VersionExact` | const | §11.2 | VersionExact accepts only the SDK's own MAJOR.MINOR. |
| `agents.VersionSameMajor` | const | §11.2 | VersionSameMajor accepts any MINOR within the SDK's MAJOR. |
| `agents.VersionPolicy.Accepts` | method | §11.2 | Accepts reports whether the policy accepts an agent whose version relates to the SDK's as c. |
| `agents.CheckProtocolVersion` | func | §11.2, §3.2, §11.1 | CheckProtocolVersion applies SAP §11.2 to an agent's declared metadata.protocol_version. |

## Package `caller`: the caller side

Discovery, prompting, the reply stream, queries, status, heartbeat tracking and the caller half of the sender identity.

| Identifier | Kind | Section | Note |
|---|---|---|---|
| `caller.Agent` | type | §2, §4.3 | Agent is a discovered agent instance: an immutable handle over the service record it was built from. |
| `caller.Agent.Info` | method | §2, §4.3 | Info returns a copy of the service record the handle was built from. |
| `caller.Agent.InstanceID` | method | §2, §4.3 | InstanceID returns the service id of the instance, the heartbeat's instance_id. |
| `caller.Agent.PromptSubject` | method | §4.3 | PromptSubject returns the prompt endpoint's subject, verbatim from the discovery record. |
| `caller.Agent.Client` | method | §2, §4.3 | Client returns the client the handle belongs to. |
| `caller.Agent.MinSenderTrust` | method | §13.9 | MinSenderTrust returns what the prompt endpoint requires of senders (SAP §13.9). |
| `caller.Agent.CheckVersion` | method | §11.2 | CheckVersion applies the client's version policy to the agent's declared protocol_version. |
| `caller.PromptOption` | type | §5, §6.6 | PromptOption configures one [Agent.Prompt]. |
| `caller.WithPromptInactivityTimeout` | func | §6.6 | WithPromptInactivityTimeout overrides the client's inactivity timeout for one prompt. |
| `caller.WithPromptMaxWait` | func | §5, §6.6 | WithPromptMaxWait overrides the client's stream ceiling for one prompt. |
| `caller.WithPromptSubject` | func | §13.6.2 | WithPromptSubject publishes to subject instead of the one in the discovery record. |
| `caller.WithSignedSubject` | func | §13.6.2 | WithSignedSubject signs sub instead of the subject published to: the exporter's subject behind an import th... |
| `caller.WithPromptBuffer` | func | §5, §6.6 | WithPromptBuffer overrides the client's stream buffer bounds for one prompt; zero keeps the client's value... |
| `caller.WithPromptQueryReplyPolicy` | func | §5, §6.6 | WithPromptQueryReplyPolicy overrides the client's query reply policy for the queries of one prompt. |
| `caller.Agent.PromptText` | method | §2, §4.3 | PromptText prompts the agent with text. |
| `caller.Agent.Prompt` | method | §5.4, §13.5.1, §13.9, §4.3 | Prompt sends the envelope to the agent and returns its reply stream. |
| `caller.StatusOption` | type | §8.7 | StatusOption configures one [Agent.Status]. |
| `caller.WithStatusTimeout` | func | §8.7 | WithStatusTimeout sets how long Status waits. |
| `caller.WithStatusSubject` | func | §8.7 | WithStatusSubject sends the request to subject instead of the one in the discovery record, behind a renamin... |
| `caller.WithStatusSignedSubject` | func | §13.6.2 | WithStatusSignedSubject signs sub instead of the subject published to (SAP §13.6.2). |
| `caller.Agent.Status` | method | §8.7, §13.5 | Status asks the agent's status endpoint for a point-in-time heartbeat (SAP §8.7). |
| `caller.AttachmentFromFile` | func | §5.2 | AttachmentFromFile reads a regular file into an attachment named by its base name only, so no directory of... |
| `caller.Client` | type | §5, §6 (SDK policy: lifecycle and counters) | Client is the caller side of SAP over one NATS connection: it discovers agents, prompts them and tracks the... |
| `caller.New` | func | §10.1 | New returns a Client on nc. |
| `caller.Client.Connection` | method | §5, §6 (SDK policy: lifecycle and counters) | Connection returns the NATS connection the client uses. |
| `caller.Client.Tracker` | method | §8.5 | Tracker returns the heartbeat tracker the client owns. |
| `caller.Client.IsClosed` | method | §5, §6 (SDK policy: lifecycle and counters) | IsClosed reports whether Close has been called. |
| `caller.Client.Done` | method | §5, §6 (SDK policy: lifecycle and counters) | Done returns a channel closed when the client is closed. |
| `caller.Stats` | type | §5, §6 (SDK policy: lifecycle and counters) | Stats is a snapshot of the client's counters. |
| `caller.Client.Stats` | method | §5, §6 (SDK policy: lifecycle and counters) | Stats returns the client's counters. |
| `caller.Client.Close` | method | §5, §6 (SDK policy: lifecycle and counters) | Close ends every stream still running with [ErrClosed], stops the heartbeat tracker, drains the reply subsc... |
| `caller.Client.Shutdown` | method | §5, §6 (SDK policy: lifecycle and counters) | Shutdown is [Client.Close] with a deadline the caller controls, as [net/http.Server.Shutdown] is to Close. |
| `caller.Client.Drain` | method | §5, §6 (SDK policy: lifecycle and counters) | Drain refuses new prompts with [ErrClosed], waits for the streams in flight to end on their own, and then c... |
| `caller.Client.ObserveError` | method | §13.4 | ObserveError feeds an asynchronous NATS error to the identity lookup, so a permission that blocks $SYS.REQ.... |
| `caller.Filter` | type | §4.2 | Filter selects agents on the caller's side: every non-empty field must equal the record's. |
| `caller.DiscoverOption` | type | §4.1, §4.2 | DiscoverOption configures [Client.Discover], [Client.ListAgents] and [Client.PingAll]. |
| `caller.WithDiscoverTimeout` | func | §4.1, §4.2 | WithDiscoverTimeout waits exactly d and returns every responder seen in that window (the timer strategy). |
| `caller.WithDiscoverStall` | func | §4.1, §4.2 | WithDiscoverStall sets the idle window after the latest reply that ends a discovery. |
| `caller.WithDiscoverMaxWait` | func | §4.1, §4.2 | WithDiscoverMaxWait sets the absolute ceiling of a discovery that has no fixed timeout. |
| `caller.WithDiscoverMaxRecords` | func | §4.1, §4.2 | WithDiscoverMaxRecords bounds the records one [Client.Discover], [Client.Find], [Client.ListAgents] or [Cli... |
| `caller.WithFilter` | func | §4.1, §4.2 | WithFilter keeps only the agents the filter matches. |
| `caller.WithCompatibleOnly` | func | §4.1, §4.2 | WithCompatibleOnly drops agents whose protocol version the client's policy refuses. |
| `caller.WithRequireVerifiedIdentity` | func | §13.10, §13.9 | WithRequireVerifiedIdentity keeps only the agents whose discovery record carries a valid id_sig (SAP §13.10... |
| `caller.WithRequireVerifiedIdentityFunc` | func | §13.10 | WithRequireVerifiedIdentityFunc is [WithRequireVerifiedIdentity] with a predicate: an agent is kept only wh... |
| `caller.Client.StartTracking` | method | §8.5 | StartTracking starts the client's heartbeat tracker. |
| `caller.Client.ListAgents` | method | §4.3, §12 | ListAgents enumerates $SRV.INFO.agents and returns the service record of every SAP agent that answered, onc... |
| `caller.Client.Discover` | method | §4.1, §8.5 | Discover finds the agents reachable on the connection and returns a handle for each. |
| `caller.Client.Find` | method | §4.2 | Find returns the first agent the filter matches, discovering broadly and narrowing here (SAP §4.2). |
| `caller.Client.LookupInstance` | method | §4.2 | LookupInstance fetches the record of one instance with $SRV.INFO.agents.{instance_id} (SAP §4.2). |
| `caller.Client.Ping` | method | §8.4 | Ping reports whether one instance answers the micro service ping $SRV.PING.agents.{instance_id}, a point-in... |
| `caller.Client.PingAll` | method | §4, §8.4 | PingAll sends the general ping $SRV.PING.agents, to which every agent instance answers once (SAP §4), and r... |
| `caller.ErrClosed` | var | SDK policy (§6) | ErrClosed is returned by a [Client] that has been closed or drained, and ends every stream that was still r... |
| `caller.ErrStreamConsumed` | var | SDK policy (§6) | ErrStreamConsumed is yielded when [Stream.Events] is ranged over a second time: a stream has one consumer a... |
| `caller.ErrAgentNotFound` | var | §4.2 | ErrAgentNotFound is returned by [Client.LookupInstance] and [Client.Find] when no agent answers or the answ... |
| `caller.ErrSlowConsumer` | var | SDK policy (§6.6) | ErrSlowConsumer matches a [*SlowConsumerError]: the program did not read a stream fast enough and its bound... |
| `caller.ErrMaxWait` | var | §6.6 | ErrMaxWait matches a [*MaxWaitError]: the stream ran longer than its ceiling without ending (SDK policy; SA... |
| `caller.ErrReservedHeader` | var | §9, §13.5 (extension point) | ErrReservedHeader is returned, before anything is published, when an outgoing-header hook or interceptor se... |
| `caller.ErrInvalidHeader` | var | §9, §13.5 (extension point) | ErrInvalidHeader is returned, before anything is published, for a header name or value that cannot be frame... |
| `caller.ErrReservedField` | var | §5.6 | ErrReservedField is returned, before anything is published, when an interceptor tries to set the envelope's... |
| `caller.ErrQueryAlreadyReplied` | var | §7.2 | ErrQueryAlreadyReplied matches a [*QueryAlreadyRepliedError]. |
| `caller.ErrQueryReplyRefused` | var | §7.1 | ErrQueryReplyRefused matches a [*QueryReplyRefusedError]: the reply subject of a query was refused by the q... |
| `caller.ErrUntrustedAgent` | var | §13.10 | ErrUntrustedAgent is returned by [Agent.Prompt] and [Agent.Status], before anything is published, when the... |
| `caller.ErrNoSigner` | var | §13.5, §13.6 | ErrNoSigner is returned by the signing helpers when the client has no signer. |
| `caller.SlowConsumerError` | type | SDK policy (§6.6) | SlowConsumerError reports a stream whose bounded buffer overflowed. |
| `caller.SlowConsumerError.Error` | method | SDK policy (§6.6) |  |
| `caller.SlowConsumerError.Is` | method | SDK policy (§6.6) | Is matches [ErrSlowConsumer]. |
| `caller.MaxWaitError` | type | SDK policy (§6.6) | MaxWaitError reports a stream that did not end within its ceiling. |
| `caller.MaxWaitError.Error` | method | SDK policy (§6.6) |  |
| `caller.MaxWaitError.Is` | method | SDK policy (§6.6) | Is matches [ErrMaxWait]. |
| `caller.QueryAlreadyRepliedError` | type | §7.2 | QueryAlreadyRepliedError is returned by the second call to [Query.Reply] or [Query.ReplyEnvelope]: a query... |
| `caller.QueryAlreadyRepliedError.Error` | method | §7.2 |  |
| `caller.QueryAlreadyRepliedError.Is` | method | §7.2 | Is matches [ErrQueryAlreadyReplied] and [agents.ErrLocal]. |
| `caller.QueryReplyRefusedError` | type | §7.1 | QueryReplyRefusedError is returned by [Query.Reply] and [Query.ReplyEnvelope] when the query reply policy r... |
| `caller.QueryReplyRefusedError.Error` | method | §7.1 |  |
| `caller.QueryReplyRefusedError.Unwrap` | method | §7.1 | Unwrap returns the policy's error. |
| `caller.QueryReplyRefusedError.Is` | method | §7.1 | Is matches [ErrQueryReplyRefused] and [agents.ErrLocal]. |
| `caller.PromptInfo` | type | §5.6 (extension point) | PromptInfo is what an interceptor or the header hook sees of one prompt before it goes out. |
| `caller.Extras` | type | §5.6 (extension point) | Extras are the additions one interceptor makes to a prompt. |
| `caller.Interceptor` | type | §5.6 (extension point) | Interceptor extends every prompt of a [Client]. |
| `caller.Interceptor.BeforePrompt` | method | §5.6 (extension point) | BeforePrompt is phase one: it returns what the prompt should carry in addition. |
| `caller.PublishInterceptor` | type | §5.6 (extension point) | PublishInterceptor is the optional second phase, found by type assertion. |
| `caller.PublishInterceptor.BeforePublish` | method | §5.6 (extension point) |  |
| `caller.InterceptorFunc` | type | §5.6 (extension point) | InterceptorFunc adapts a function to an [Interceptor]. |
| `caller.InterceptorFunc.BeforePrompt` | method | §5.6 (extension point) | BeforePrompt calls f. |
| `caller.Event` | type | §6.2, §6.4 | Event is one item of a response stream: a [Response], a [Status] or a *[Query]. |
| `caller.Event.Header` | method | §9 | Header returns a copy of the headers the message arrived with: SAP §9 has callers inspect them on every mes... |
| `caller.Response` | type | §6.3 | Response is a response chunk: content from the agent, in either wire form (a bare string or {text, attachme... |
| `caller.Response.SaveAttachments` | method | §6.3 | SaveAttachments writes the response's attachments into dir with [agents.SaveAttachments], which sanitizes e... |
| `caller.Status` | type | §6.4 | Status is a status chunk. |
| `caller.Query` | type | §7.1, §7.3 | Query is a mid-stream question from the agent (SAP §7.1). |
| `caller.Query.SaveAttachments` | method | §7.1, §7.3 | SaveAttachments writes the query's attachments into dir; see [Response.SaveAttachments]. |
| `caller.Query.Reply` | method | §5.1 | Reply answers the query with plain text, the shorthand of SAP §5.1. |
| `caller.Query.ReplyEnvelope` | method | §7.2 | ReplyEnvelope answers the query with an envelope: a prompt and optional attachments (SAP §7.2). |
| `caller.DefaultInboxPrefix` | const | SDK policy (§5.1, §6.6) | DefaultInboxPrefix is the reply-inbox prefix every SDK caller uses, so one permission covers all of them. |
| `caller.DefaultMaxWait` | const | SDK policy (§5.1, §6.6) | DefaultMaxWait is the ceiling on one stream, 10 minutes, the value both official callers use. |
| `caller.DefaultStatusTimeout` | const | SDK policy (§8.7) | DefaultStatusTimeout is how long a status request waits for its reply. |
| `caller.DefaultLookupTimeout` | const | SDK policy (§4.1, §4.2) | DefaultLookupTimeout is how long an instance lookup or ping waits. |
| `caller.DefaultDiscoverStall` | const | SDK policy (§4.1, §4.2) | DefaultDiscoverStall ends a discovery that has had no reply for this long once one has arrived, the value t... |
| `caller.DefaultDiscoverMaxWait` | const | SDK policy (§4.1, §4.2) | DefaultDiscoverMaxWait is the absolute ceiling of a discovery. |
| `caller.DefaultStreamBuffer` | const | SDK policy (§5.1, §6.6) | DefaultStreamBuffer is the most unread messages one stream holds before it ends with a [*SlowConsumerError]. |
| `caller.DefaultStreamBufferBytes` | const | SDK policy (§5.1, §6.6) | DefaultStreamBufferBytes is the most unread bytes one stream holds. |
| `caller.DefaultDiscoverBuffer` | const | SDK policy (§4.1, §4.2) | DefaultDiscoverBuffer is the most replies one discovery buffers. |
| `caller.DefaultDiscoverMaxRecords` | const | SDK policy (§4.1, §4.2) | DefaultDiscoverMaxRecords is the most records one discovery keeps. |
| `caller.DefaultCloseTimeout` | const | SDK policy (§5.1, §6.6) | DefaultCloseTimeout bounds how long [Client.Close] waits for the client to shut down, and how long [Client.... |
| `caller.DefaultFlushTimeout` | const | SDK policy (§5.1, §6.6) | DefaultFlushTimeout bounds the flush that follows a subscription when the caller's context has no deadline. |
| `caller.Option` | type | SDK policy | Option configures a [Client]. |
| `caller.WithLogger` | func | SDK policy | WithLogger sets the logger. |
| `caller.WithInboxPrefix` | func | §5 (reply subject; SDK policy) | WithInboxPrefix sets the prefix of the client's reply subjects, replacing [DefaultInboxPrefix]. |
| `caller.WithVersionPolicy` | func | §11.2 | WithVersionPolicy sets which protocol versions the client accepts from an agent. |
| `caller.IdentityOptions` | type | §13 | IdentityOptions configure the optional sender identity extension (SAP §13). |
| `caller.WithIdentity` | func | §13.4, §13.9 | WithIdentity enables the sender identity extension. |
| `caller.WithInterceptors` | func | §5.6 (extension point) | WithInterceptors installs prompt interceptors, run in order for every prompt. |
| `caller.HeaderHook` | type | §9, §13.5 (extension point) | HeaderHook returns application headers to add to one outgoing prompt. |
| `caller.WithRequestHeaders` | func | §9, §13.5 (extension point) | WithRequestHeaders installs the outgoing-header hook of: it adds application headers to every prompt. |
| `caller.WithInactivityTimeout` | func | §6.6 | WithInactivityTimeout sets the default per-stream inactivity timeout, which counts from the latest chunk se... |
| `caller.WithMaxWait` | func | SDK policy | WithMaxWait sets the default ceiling on one stream, [DefaultMaxWait]. |
| `caller.WithStreamBuffer` | func | SDK policy | WithStreamBuffer bounds the unread messages and bytes one stream may hold. |
| `caller.WithResolveTTL` | func | §13.10 | WithResolveTTL sets how long the sender resolver reuses one enumeration (SAP §13.10 suggests a short cache). |
| `caller.DefaultQueryReplyPolicy` | func | §7.1 | DefaultQueryReplyPolicy is the query reply policy a client uses unless [WithQueryReplyPolicy] replaces it. |
| `caller.WithQueryReplyPolicy` | func | §7.1 | WithQueryReplyPolicy sets the policy that vets the agent-supplied reply_subject of every query before [Quer... |
| `caller.WithTracker` | func | §8.5 | WithTracker passes options to the heartbeat tracker the client owns. |
| `caller.ReservedHeader` | func | §9, §13.5 (extension point) | ReservedHeader reports whether name is a header the outgoing-header hook and interceptors may not set. |
| `caller.TrustPolicy` | type | §13.10 | TrustPolicy says which agent identities a program trusts. |
| `caller.WithTrustPolicy` | func | §13.10 | WithTrustPolicy makes verified discovery the default. |
| `caller.Client.SelfID` | method | §13.4 | SelfID returns the connection's own agent ID (SAP §13.4), looking it up once and remembering it. |
| `caller.Client.RefreshSelfID` | method | §13.4, §13.6 | RefreshSelfID forgets the remembered identity and looks it up again. |
| `caller.SignOption` | type | §13.4, §13.6 | SignOption configures [Client.SignSender], [Client.PublishSigned] and [Client.RequestSigned]. |
| `caller.WithSub` | func | §13.6.2 | WithSub sets the subject to sign when it differs from the one published to, which is only right behind a se... |
| `caller.WithNonce` | func | §13.5 | WithNonce sets the nonce instead of a fresh one, for a message whose body carries its own id. |
| `caller.WithHeaders` | func | §13.4, §13.6 | WithHeaders adds application headers to a signed publish or request. |
| `caller.Client.SignSender` | method | §13.5, §13.6 | SignSender returns the Agent-Sender header value that signs payload for subject, to attach to a publish the... |
| `caller.Client.PublishSigned` | method | §13.5, §13.8 | PublishSigned publishes payload to subject with a signed Agent-Sender header and Nats-Msg-Id set to the non... |
| `caller.Client.RequestSigned` | method | §13.4, §13.6 | RequestSigned makes a request signed like [Client.PublishSigned] and returns the first reply, waiting at mo... |
| `caller.Client.ResolveSender` | method | §13.10 | ResolveSender maps a verified sender's agent ID back to the agent instance that registered it, by enumerati... |
| `caller.Client.InvalidateSenders` | method | §13.4, §13.6 | InvalidateSenders drops the cached sender index; the next [Client.ResolveSender] enumerates again. |
| `caller.Stream` | type | §6.5, §6.7 | Stream is the reply stream of one prompt: chunks in publication order, ended by the zero-byte terminator (S... |
| `caller.Stream.Subject` | method | §6.5, §6.7 | Subject returns the subject the prompt was published to. |
| `caller.Stream.Terminated` | method | §6.4 | Terminated reports whether the zero-byte terminator was observed: the agent ended the stream. |
| `caller.Stream.Err` | method | §6.5, §6.7 | Err returns the error that ended the sequence, or nil. |
| `caller.Stream.Close` | method | §6.7 | Close drops the reply registration and ends the sequence without error. |
| `caller.Stream.Events` | method | §9.1, §6.6 | Events returns the stream's items in wire order: acks, responses and queries. |
| `caller.DefaultTrackerMaxInstances` | const | §8.5 | DefaultTrackerMaxInstances is the most instances a tracker remembers. |
| `caller.TrackerOption` | type | §8.5 | TrackerOption configures a [HeartbeatTracker]. |
| `caller.WithTrackerSlack` | func | §8.2 | WithTrackerSlack sets the offline threshold as a multiple of an instance's own interval_s. |
| `caller.WithTrackerLogger` | func | §8.5 | WithTrackerLogger sets the tracker's logger; nil is silent. |
| `caller.WithTrackerMaxInstances` | func | §8.5 | WithTrackerMaxInstances bounds how many instances the tracker remembers. |
| `caller.WithHeartbeatVerification` | func | §13.7, §13.11 | WithHeartbeatVerification makes the tracker verify the Agent-Sender header of every heartbeat as a live mes... |
| `caller.Beat` | type | §8.5 | Beat is one heartbeat as the tracker received it. |
| `caller.Beat.Header` | method | §13.5 | Header returns a copy of the headers the heartbeat arrived with, so a program can read what SAP carries the... |
| `caller.Liveness` | type | §8.2 | Liveness is the tracker's view of one instance. |
| `caller.HeartbeatTracker` | type | §3.4, §8.1 | HeartbeatTracker subscribes to the heartbeat wildcard and keeps each instance's last heartbeat, keyed on th... |
| `caller.NewHeartbeatTracker` | func | §8.5 | NewHeartbeatTracker returns a tracker on nc. |
| `caller.HeartbeatTracker.Subject` | method | §8.1, §8.5 | Subject returns the wildcard the tracker subscribes to, agents.hb.*.*.*. |
| `caller.HeartbeatTracker.Started` | method | §3.4, §8.1 | Started reports whether the tracker is subscribed. |
| `caller.HeartbeatTracker.Dropped` | method | §3.4, §8.1 | Dropped counts heartbeats the tracker discarded: malformed, too large, failing verification, or refused for... |
| `caller.HeartbeatTracker.Start` | method | §8.5 | Start subscribes to the heartbeat wildcard and flushes, so the server has the interest when Start returns:... |
| `caller.HeartbeatTracker.Stop` | method | §3.4, §8.1 | Stop drains the subscription, so pending heartbeats are delivered, and waits for it to close or for ctx; wh... |
| `caller.HeartbeatTracker.Liveness` | method | §8.2 | Liveness returns the tracker's view of one instance. |
| `caller.HeartbeatTracker.Snapshot` | method | §3.4, §8.1 | Snapshot returns the view of every remembered instance, in no order. |
| `caller.HeartbeatTracker.OnHeartbeat` | method | §3.4, §8.1 | OnHeartbeat registers fn for the heartbeats of one instance and returns the function that removes it. |
| `caller.HeartbeatTracker.OnAnyHeartbeat` | method | §3.4, §8.1 | OnAnyHeartbeat registers fn for every heartbeat and returns the function that removes it. |
| `caller.Beat.String` | method | §8.5 |  |

## Package `host`: the host side

Registration as a micro service, prompt and status endpoints, the reply stream, queries, heartbeats and the host half of the sender identity.

| Identifier | Kind | Section | Note |
|---|---|---|---|
| `host.ErrRejected` | var | §9.2 | ErrRejected matches every [*RejectError]. |
| `host.ErrInvalidCode` | var | §9.2 | ErrInvalidCode is returned by [Reject] for a code outside the SAP §9.2 table (400, 401, 403, 404, 409, 429,... |
| `host.ErrStreamClosed` | var | §6.5 | ErrStreamClosed is returned by a [Responder] call made after the terminator was sent, or when the request h... |
| `host.ErrStatusChunk` | var | §6.4 | ErrStatusChunk is returned by [Responder.SendChunk] for a status chunk: the host owns the ack and the keep-... |
| `host.ErrQueryTimeout` | var | §7.3 | ErrQueryTimeout is returned by [Responder.Ask] when the caller does not answer in time. |
| `host.ErrNextCalledTwice` | var | §5, §6 (extension point) | ErrNextCalledTwice is returned by the next function of an [Interceptor] called a second time. |
| `host.ErrNextNotCalled` | var | §5, §6 (extension point) | ErrNextNotCalled is the failure recorded when an [Interceptor] returns nil without calling next; the reques... |
| `host.ErrStarted` | var | §3 (SDK policy: lifecycle) | ErrStarted is returned by [Host.Start] on a host that already started, stopped or is stopping. |
| `host.ErrStopped` | var | §3 (SDK policy: lifecycle) | ErrStopped is returned by [Host.Start] when [Host.Shutdown] or [Host.Close] was requested while Start was r... |
| `host.ErrNotStarted` | var | §3 (SDK policy: lifecycle) | ErrNotStarted is returned when a call needs a started host. |
| `host.ErrEndpointOverlap` | var | §13.7 | ErrEndpointOverlap is wrapped with [ErrConfig] by [New] when an extra endpoint's subject or pattern could m... |
| `host.ErrConfig` | var | §3 (SDK policy: lifecycle) | ErrConfig matches the error [New] returns for an invalid configuration. |
| `host.RejectError` | type | §9.2, §9.1 | RejectError refuses a request with a chosen SAP §9.2 code. |
| `host.RejectError.Error` | method | §9.2, §9.1 | Error implements error. |
| `host.RejectError.Is` | method | §9.2, §9.1 | Is matches ErrRejected. |
| `host.RejectError.Unwrap` | method | §9.2, §9.1 | Unwrap returns the *agents.ServiceError the rejection answers with, so agents.IsCode and errors.Is(err, age... |
| `host.Reject` | func | §9.2 | Reject returns a [*RejectError] with the code and description. |
| `host.RejectWithBody` | func | §9.1 | RejectWithBody is Reject with the optional JSON error body of SAP §9.1. |
| `host.HeartbeatSigner` | type | §13.11 | HeartbeatSigner is the identity a host signs its heartbeats with. |
| `host.SignHeartbeatOptions` | type | §13.11 | SignHeartbeatOptions are the inputs of [SignHeartbeatHeader]. |
| `host.SignHeartbeatHeader` | func | §13.11 | SignHeartbeatHeader builds the Agent-Sender header of a heartbeat: sub is the heartbeat subject as publishe... |
| `host.SignHeartbeat` | func | §13.11 | SignHeartbeat is [SignHeartbeatHeader] returning NATS headers ready to publish with. |
| `host.PromptHandler` | type | §5, §6, §9 | PromptHandler serves one prompt (SAP §5, §6). |
| `host.Stats` | type | SDK policy (§3.3, §5) | Stats is a snapshot of the host's counters. |
| `host.Host` | type | §3, §5, §6, §8 | Host serves one agent instance: its prompt and status endpoints and its heartbeat. |
| `host.New` | func | §3 (SDK policy: lifecycle) | New validates the configuration and returns a host. |
| `host.Host.Start` | method | §3, §8.2, §13.4 | Start registers the micro service, its prompt, status and extra endpoints, flushes, and publishes the first... |
| `host.Host.Shutdown` | method | §8.6 | Shutdown shuts the host down gracefully, as [net/http.Server.Shutdown] does: it stops the heartbeat (a beat... |
| `host.Host.Close` | method | §3, §5, §6, §8 | Close stops the host at once and waits for it to be complete, as [net/http.Server.Close] does, so a Host is... |
| `host.Host.InstanceID` | method | §8.3 | InstanceID returns the micro service id, which is the heartbeat's instance_id (SAP §8.3). |
| `host.Host.Subject` | method | §3, §5, §6, §8 | Subject returns the instance's subject. |
| `host.Host.Identity` | method | §13.10 | Identity returns the agent ID the host registered (user_nkey and account), and false when it registered non... |
| `host.Host.MinSenderTrust` | method | §13.9 | MinSenderTrust returns the min_sender_trust the host advertises and enforces. |
| `host.Host.OperatorAttested` | method | §3, §5, §6, §8 | OperatorAttested reports whether the operator-attested cross-check is on. |
| `host.Host.Gate` | method | §3, §5, §6, §8 | Gate returns the sender gate of the host, for a test or a tool that classifies messages the way the host does. |
| `host.Host.Service` | method | §3, §5, §6, §8 | Service returns the underlying micro service, the escape hatch for endpoints that are only known at runtime... |
| `host.Host.PromptMetadata` | method | §2.1, §13.9 | PromptMetadata returns a copy of the metadata of the prompt endpoint: max_payload, attachments_ok and min_s... |
| `host.Host.Stats` | method | §3, §5, §6, §8 | Stats returns a snapshot of the host's counters. |
| `host.CallNext` | type | §5, §6 (extension point) | CallNext continues a request: it publishes the ack, runs the interceptors after the caller's and then the h... |
| `host.Interceptor` | type | §5, §6 (extension point) | Interceptor wraps the handling of a prompt request. |
| `host.Interceptor.AroundRequest` | method | §5, §6 (extension point) |  |
| `host.InterceptorFunc` | type | §5, §6 (extension point) | InterceptorFunc adapts a function to [Interceptor]. |
| `host.InterceptorFunc.AroundRequest` | method | §5, §6 (extension point) | AroundRequest calls f. |
| `host.DefaultHeartbeatInterval` | const | §8.2 | DefaultHeartbeatInterval is the recommended cadence. |
| `host.DefaultKeepalive` | const | §6.6 | DefaultKeepalive is the cadence of the keep-alive ack, 30 seconds, below the 60 seconds of SAP §6.6. |
| `host.DefaultHeartbeatSignTimeout` | const | §8.3, §13.11 (SDK policy: signing bound) | DefaultHeartbeatSignTimeout bounds the signing of one heartbeat (SDK policy). |
| `host.DefaultAttachmentsOK` | const | §2.1 | DefaultAttachmentsOK is the attachments_ok the prompt endpoint advertises. |
| `host.DefaultMaxConcurrentPrompts` | const | SDK policy (§3.3, §5) | DefaultMaxConcurrentPrompts is the number of prompts served at once (SDK policy). |
| `host.DefaultMinSenderTrust` | const | §13.9 | DefaultMinSenderTrust is the min_sender_trust the prompt endpoint advertises. |
| `host.DefaultVersion` | const | §3.1 | DefaultVersion is the service version registered when [WithVersion] is not used. |
| `host.DefaultStartTimeout` | const | §3 (SDK policy: lifecycle) | DefaultStartTimeout bounds the flush of [Host.Start] when its context has no deadline. |
| `host.DefaultCloseTimeout` | const | §3 (SDK policy: lifecycle) | DefaultCloseTimeout bounds how long [Host.Close] waits for the host to be complete. |
| `host.DefaultMaxPayload` | const | §2.1 | DefaultMaxPayload is advertised when the connection reports no limit. |
| `host.BusyPolicy` | type | SDK policy (§3.3, §5) | BusyPolicy says what the host does with a prompt when every concurrency slot is taken. |
| `host.BusyQueue` | const | SDK policy (§3.3, §5) | BusyQueue holds the prompt endpoint's subscription callback until a slot frees, which back-pressures that s... |
| `host.BusyReject429` | const | §9.2 | BusyReject429 answers a prompt that finds no free slot with 429 and the terminator at once (SAP §9.2). |
| `host.IdentityOptions` | type | §13.10 | IdentityOptions configure the host's own identity registration (SAP §13.10). |
| `host.AcceptSenderFunc` | type | §13.7.3 | AcceptSenderFunc is the admission hook (SAP §13.7.3;). |
| `host.Option` | type | §3 (SDK policy: lifecycle) | Option configures a [Host]. |
| `host.WithDescription` | func | §3.1 | WithDescription sets the service description; the default is "<agent> agent <name>". |
| `host.WithVersion` | func | §3.1 | WithVersion sets the service version, a semver of the harness implementation; the default is [DefaultVersion]. |
| `host.WithSession` | func | §3.2 | WithSession sets metadata.session and the heartbeat's session field. |
| `host.WithMaxPayload` | func | §2.1 | WithMaxPayload sets the max_payload the prompt endpoint advertises. |
| `host.WithAttachmentsOK` | func | §2.1 | WithAttachmentsOK sets the attachments_ok the prompt endpoint advertises; the default is [DefaultAttachment... |
| `host.WithInboundChecks` | func | §5.4, §9.2 | WithInboundChecks turns the host's own enforcement of the advertised limits on or off (SDK policy; SAP §5.4... |
| `host.WithHeartbeatInterval` | func | §8.2 | WithHeartbeatInterval sets the heartbeat cadence; the default is [DefaultHeartbeatInterval]. |
| `host.WithSubSecondHeartbeats` | func | §8.2 | WithSubSecondHeartbeats is the explicit opt-in that allows a heartbeat interval under one second. |
| `host.WithKeepalive` | func | §6.4, §6.6 | WithKeepalive sets the cadence of the keep-alive ack: while a handler runs, the host sends an extra ack sta... |
| `host.WithoutKeepalive` | func | §6.4 | WithoutKeepalive disables the keep-alive ack, so a stream carries exactly one ack, the strict reading of SA... |
| `host.WithHeartbeatSignTimeout` | func | §8.3, §13.11 (SDK policy: signing bound) | WithHeartbeatSignTimeout bounds the signing of one heartbeat; the default is [DefaultHeartbeatSignTimeout]... |
| `host.WithExtraMetadata` | func | §3.2 | WithExtraMetadata adds keys to the service metadata (SAP §3.2: additional keys MAY be included). |
| `host.WithEndpoint` | func | §3 (extension point) | WithEndpoint registers an extra micro endpoint beside prompt and status. |
| `host.WithLogger` | func | §3 (SDK policy: lifecycle) | WithLogger sets the logger; nil is silent. |
| `host.WithIdentity` | func | §13.10, §13.11 | WithIdentity enables the sender identity extension's registration side: the host learns its own agent ID, r... |
| `host.WithMinSenderTrust` | func | §13.9 | WithMinSenderTrust sets the min_sender_trust the prompt endpoint advertises and enforces (SAP §13.9). |
| `host.WithReplayWindow` | func | §13.7 | WithReplayWindow sets the timestamp skew and the nonce window of live verification; the default is identity... |
| `host.WithMaxNonces` | func | §13.7 (SDK policy: replay bounds) | WithMaxNonces bounds the replay set; the default is identity.DefaultMaxNonces. |
| `host.WithMaxNoncesPerSender` | func | §13.7 (SDK policy: replay bounds) | WithMaxNoncesPerSender sets the per-sender quota of the replay set; the default is identity.DefaultMaxNonce... |
| `host.WithAdmission` | func | §13.7.3 | WithAdmission sets the admission hook (SAP §13.7.3;). |
| `host.WithResolveTTL` | func | §13.10 | WithResolveTTL sets how long the reverse-lookup index of [Request.ResolveSender] is reused; the default is... |
| `host.WithOperatorAttested` | func | §13.7.2 | WithOperatorAttested is the deployment promise that every request reaching this host crossed a service impo... |
| `host.WithAccountTokenPosition` | func | §13.7.1 | WithAccountTokenPosition sets the 1-based position of the account token an export with account_token_positi... |
| `host.WithInterceptors` | func | §5, §6 (extension point) | WithInterceptors adds host interceptors; the first listed is the outermost. |
| `host.WithHeartbeatExtras` | func | §8.3 | WithHeartbeatExtras adds fields to every heartbeat and status reply, read when each is built. |
| `host.WithMaxConcurrentPrompts` | func | SDK policy (§3.3, §5) | WithMaxConcurrentPrompts sets how many prompts are served at once; the default is [DefaultMaxConcurrentProm... |
| `host.WithBusyPolicy` | func | SDK policy (§3.3, §5) | WithBusyPolicy sets what happens when every slot is taken; the default is [BusyQueue]. |
| `host.Endpoint` | type | §3 (extension point) | Endpoint is an extra micro endpoint the host registers without interpreting it (the TypeS... |
| `host.WithReplySubjectPolicy` | func | §2, §6 | WithReplySubjectPolicy sets the policy that vets the reply subject of every prompt and status request befor... |
| `host.Request` | type | §5 | Request is a prompt request as the handler sees it. |
| `host.Request.ResolveSender` | method | §13.10 | ResolveSender maps a verified sender back to the agent instance that registered its agent ID, with the id_s... |
| `host.RequestInfo` | type | §5 (extension point) | RequestInfo is what an [Interceptor] and the admission hook see of a request, after the envelope decode and... |
| `host.DefaultListenProbeTimeout` | const | §6.7 | DefaultListenProbeTimeout is how long [Responder.CallerListening] waits for a no-responders answer when no... |
| `host.Responder` | type | §6, §6.5 | Responder writes the reply stream of one prompt (SAP §6). |
| `host.Responder.Send` | method | §6.3 | Send publishes a response chunk with the given text, in the bare-string form of SAP §6.3. |
| `host.Responder.SendAttachments` | method | §6.3, §5.2 | SendAttachments publishes a response chunk with text and attachments, in the object form of SAP §6.3. |
| `host.Responder.SendChunk` | method | §6.4 | SendChunk publishes one chunk. |
| `host.AskOption` | type | §7 | AskOption configures [Responder.Ask]. |
| `host.WithAskAttachments` | func | §7.1 | WithAskAttachments adds attachments to the query chunk, merged after those of the envelope. |
| `host.Responder.Ask` | method | §7, §7.2, §7.3 | Ask puts a question to the caller in the middle of the stream and waits for one reply (SAP §7). |
| `host.Responder.AskEnvelope` | method | §6, §6.5 | AskEnvelope is [Responder.Ask] with an envelope: its prompt and attachments form the query, and unknown fie... |
| `host.Responder.CallerListening` | method | §6.7 | CallerListening probes whether the caller still listens on the reply subject, as the TypeScript host does:... |
| `host.ErrReplySubject` | var | §7.1 | ErrReplySubject is the error of the default reply-subject policy and the sentinel a custom [ReplySubjectPol... |
| `host.ReplySubjectPolicy` | type | §7.1 | ReplySubjectPolicy decides whether the host may publish its replies to a subject a caller supplied. |
| `host.DefaultReplySubjectPolicy` | func | §2, §6 | DefaultReplySubjectPolicy is the host-independent part of the policy a host uses unless [WithReplySubjectPo... |

## Package `identity`: sender identity (SAP §13)

The optional sender identity extension: agent IDs, the `Agent-Sender` header, signing, verification, trust classes, admission, replay protection and registration.

| Identifier | Kind | Section | Note |
|---|---|---|---|
| `identity.ErrInvalidAgentID` | var | §13.3 | ErrInvalidAgentID is returned (wrapped) when an agent ID, or one of its two halves, does not meet SAP §13.3. |
| `identity.MaxAccountLen` | const | §13.3.1 | MaxAccountLen is the size allowance for an account token in [MaxHeaderWireBytes], in bytes: 56 for an NKEY,... |
| `identity.AgentID` | type | §13.2, §13.3 | AgentID is the canonical text form "{account}.{user}" of a sender's identity. |
| `identity.NewAgentID` | func | §13.3 | NewAgentID builds an agent ID from its two halves and validates both: user must be a user public NKEY, and... |
| `identity.ParseAgentID` | func | §13.3, §13.3.3 | ParseAgentID parses the canonical text form and nothing else. |
| `identity.AgentID.Account` | method | §13.2, §13.3 | Account returns the account half of the ID, or "" for the zero value. |
| `identity.AgentID.User` | method | §13.2, §13.3 | User returns the user public NKEY half of the ID, or "" for the zero value. |
| `identity.AgentID.String` | method | §13.2, §13.3 | String returns the canonical text form. |
| `identity.ErrMalformedHeader` | var | §13.5.1, §13.7 | ErrMalformedHeader marks a malformed Agent-Sender header, answered with 400. |
| `identity.ErrVerification` | var | §13.7 | ErrVerification marks a signed header that failed verification (bad signature, subject, timestamp or replay... |
| `identity.ErrSignatureRequired` | var | §13.9 | ErrSignatureRequired marks a request that is not verified at an endpoint that requires a verified sender, a... |
| `identity.ErrSenderRefused` | var | §13.7.3 | ErrSenderRefused marks a verified sender that the receiver's admission hook refused, answered with 403. |
| `identity.ErrAdmission` | var | §13.7.3 | ErrAdmission marks a failure of the admission hook itself, answered with 500 (SDK policy: a hook that fails... |
| `identity.ErrNoNonceSet` | var | §13.7 | ErrNoNonceSet is returned when live verification is configured without a replay set. |
| `identity.ErrNonceWindow` | var | §13.7 | ErrNonceWindow is returned when a [NonceSet]'s window is shorter than the replay window of the verifier or... |
| `identity.DescriptionMalformed` | const | §13.7.3 | DescriptionMalformed is the wire description of a 400. |
| `identity.DescriptionRejected` | const | §13.7.3 | DescriptionRejected is the wire description of a 401 for a failed check and of a 403. |
| `identity.DescriptionSignatureRequired` | const | §13.7.3 | DescriptionSignatureRequired is the wire description of a 401 when the endpoint requires a signature the re... |
| `identity.DescriptionServerError` | const | §13.7.3 | DescriptionServerError is the wire description of a 500. |
| `identity.Error` | type | §9.2, §13.7 | Error is a sender-identity refusal. |
| `identity.Error.Error` | method | §9.2, §13.7 | Error implements the error interface. |
| `identity.Error.Unwrap` | method | §9.2, §13.7 | Unwrap returns the sentinel for the outcome, so that errors.Is(err, ErrMalformedHeader) and the like work. |
| `identity.AcceptFunc` | type | §13.7.3, §6.4 | AcceptFunc is the admission hook: it decides whether the receiver accepts a sender. |
| `identity.GateOptions` | type | §13.7 | GateOptions configure a [Gate]. |
| `identity.Gate` | type | §13.7 | Gate classifies the requests of one receiver (SAP §13.7): it parses and verifies the Agent-Sender header (l... |
| `identity.NewGate` | func | §13.7 | NewGate returns a Gate. |
| `identity.Gate.MinSenderTrust` | method | §13.7 | MinSenderTrust returns the requirement the gate enforces on prompt requests. |
| `identity.Gate.Nonces` | method | §13.7 | Nonces returns the replay set. |
| `identity.Admission` | type | §13.7 | Admission is the outcome of [Gate.AdmitPrompt]. |
| `identity.Admission.OK` | method | §13.7 | OK reports whether the request is admitted. |
| `identity.Gate.Classify` | method | §13.7 | Classify parses and verifies the sender of a live request and records its nonce (a verified request classif... |
| `identity.Gate.AdmitPrompt` | method | §13.7 | AdmitPrompt runs the full admission of a prompt request, in this order: header parse (400); ts, sub and the... |
| `identity.StatusResult` | type | §13.7 | StatusResult is the outcome of [Gate.ClassifyStatus]. |
| `identity.StatusResult.Answerable` | method | §13.9, §8.7 | Answerable reports whether the host answers the status request. |
| `identity.Gate.ClassifyStatus` | method | §13.9 | ClassifyStatus classifies the sender of a status request so the agent knows who probed it, records a verifi... |
| `identity.HeaderName` | const | §13.5.1 | HeaderName is the name of the sender header. |
| `identity.HeaderVersion` | const | §13.5 | HeaderVersion is the header format version this package implements: the JSON number 1. |
| `identity.SignedInputTag` | const | §13.6.1 | SignedInputTag is the first line of the signed input of an Agent-Sender signature. |
| `identity.HeaderFramingBytes` | const | §13.5.1 | HeaderFramingBytes is the framing the server counts against max_payload for one Agent-Sender header: "NATS/... |
| `identity.MaxHeaderValueBytes` | const | §13.5, §13.5.1 | MaxHeaderValueBytes is the cap this package applies to a header value before parsing it (SDK policy; a sign... |
| `identity.MaxNameLength` | const | §13.5, §13.5.1 | MaxNameLength is the cap on the display name, in UTF-16 code units (SDK policy, the same rule as the offici... |
| `identity.MaxNonceLength` | const | §13.5 | MaxNonceLength is the longest nonce SAP §13.5 allows. |
| `identity.DefaultReplayWindow` | const | §13.7 | DefaultReplayWindow is the default timestamp skew and nonce window, 30 seconds. |
| `identity.SenderHeader` | type | §13.5 | SenderHeader is a parsed or built Agent-Sender header. |
| `identity.SenderHeader.Signed` | method | §13.5 | Signed reports whether the header carries a signature. |
| `identity.SenderHeader.ID` | method | §13.5 | ID returns the agent ID of the header's account and user. |
| `identity.SenderHeader.String` | method | §13.5 | String returns a form safe for logs: the nonce and signature are redacted. |
| `identity.SenderHeader.LogValue` | method | §13.5 | LogValue implements [slog.LogValuer]; the nonce and signature are redacted. |
| `identity.SenderHeader.Encode` | method | §13.5.1 | Encode returns the header value in its canonical wire form: compact JSON on one line, fields in the order v... |
| `identity.SenderHeader.WireBytes` | method | §13.5.1 | WireBytes returns the size the header adds to a message on the wire: the framing plus the encoded value. |
| `identity.SenderHeader.Set` | method | §13.5 | Set stores the header on a NATS header map as one Agent-Sender value, replacing any earlier value. |
| `identity.FormatTimestamp` | func | §13.5 | FormatTimestamp formats t as the header ts: RFC 3339 UTC with second precision and the Z suffix, as signers... |
| `identity.ParseTimestamp` | func | §13.5 | ParseTimestamp parses a header ts. |
| `identity.ValidNonce` | func | §13.5 | ValidNonce reports whether nonce fits the header grammar, [A-Za-z0-9_-]{1,64} (SAP §13.5). |
| `identity.ValidName` | func | §13.5, §13.5.1 | ValidName reports whether name is acceptable as a display name: at most [MaxNameLength] UTF-16 code units,... |
| `identity.ReadHeaderValue` | func | §13.5, §13.5.1 | ReadHeaderValue reads the raw Agent-Sender value from NATS headers. |
| `identity.ParseHeader` | func | §13.5.1 | ParseHeader parses an Agent-Sender header value. |
| `identity.SignedInput` | func | §13.6.1 | SignedInput returns the exact bytes a caller signs and a receiver rebuilds: AGENT-SENDER-V1\n{account}\n{us... |
| `identity.MaxHeaderWireBytes` | func | §13.5.1 | MaxHeaderWireBytes returns an upper bound on the framed size of any header a signer would send for subject... |
| `identity.ExpectedHeaderWireBytes` | func | §13.5.1 | ExpectedHeaderWireBytes returns the exact framed size of the header this package sends for a known identity. |
| `identity.AgentIDSignedInputTag` | const | §13.10 | AgentIDSignedInputTag is the first line of the signed input of an id_sig. |
| `identity.AgentIDSignedInput` | func | §13.10 | AgentIDSignedInput returns the bytes an id_sig covers. |
| `identity.SignAgentID` | func | §13.10 | SignAgentID returns the id_sig value for registration metadata: an ed25519 signature by the agent's user se... |
| `identity.RegistrationMetadata` | func | §13.10 | RegistrationMetadata returns the three keys a host with a seed adds to its service metadata (SAP §13.10): u... |
| `identity.VerifyAgentID` | func | §13.10 | VerifyAgentID verifies a service record's id_sig against its own prompt endpoint subject. |
| `identity.DefaultMaxNonces` | const | §13.7 (replay window; bounds are SDK policy) | DefaultMaxNonces is the default maximum size of a [NonceSet]. |
| `identity.DefaultMaxNoncesPerSender` | const | §13.7 (replay window; bounds are SDK policy) | DefaultMaxNoncesPerSender is the default per-sender quota of a [NonceSet]: the most unexpired entries one u... |
| `identity.RecordResult` | type | §13.7 (replay window; bounds are SDK policy) | RecordResult is the outcome of [NonceSet.Record]. |
| `identity.Recorded` | const | §13.7 (replay window; bounds are SDK policy) | Recorded means the pair was new and is now in the set. |
| `identity.Replayed` | const | §13.7 (replay window; bounds are SDK policy) | Replayed means the pair is already in the set and unexpired: a replay. |
| `identity.SetFull` | const | §13.7 (replay window; bounds are SDK policy) | SetFull means the set is at its maximum size. |
| `identity.SenderQuotaExceeded` | const | §13.7 (replay window; bounds are SDK policy) | SenderQuotaExceeded means the sender (user public key) already holds its quota of unexpired entries. |
| `identity.Expired` | const | §13.7 (replay window) | Expired means the pair's window had already passed when it was offered (ts plus the set's window is before... |
| `identity.NonceSetOptions` | type | §13.7 (replay window; bounds are SDK policy) | NonceSetOptions configure a [NonceSet]. |
| `identity.NonceSet` | type | §13.7, §13.7.2 | NonceSet is the live-verification replay set of SAP §13.7, keyed by (user, nonce). |
| `identity.NewNonceSet` | func | §13.7 (replay window; bounds are SDK policy) | NewNonceSet returns an empty set. |
| `identity.NonceSet.Window` | method | §13.7, §13.7.2 | Window returns the replay window of the set: how long past its timestamp an entry is kept. |
| `identity.NonceSet.Len` | method | §13.7, §13.7.2 | Len returns the number of unexpired entries. |
| `identity.NonceSet.Seen` | method | §13.7, §13.7.2 | Seen reports whether (user, nonce) is in the set and unexpired. |
| `identity.NonceSet.Record` | method | §13.7, §13.7.2 | Record is an atomic check-and-set: it records (user, nonce) until ts plus the window, unless the pair is al... |
| `identity.RequestInfoHeader` | const | §13.1 | RequestInfoHeader is the header the server stamps on a request that crosses an account boundary through a s... |
| `identity.ErrBadRequestInfo` | var | §13.4, §13.7.2 | ErrBadRequestInfo is returned (wrapped) when a Nats-Request-Info header is not what the server writes: not... |
| `identity.RequestInfo` | type | §13.7.2 | RequestInfo holds the two identity fields of a Nats-Request-Info stamp. |
| `identity.ParseRequestInfo` | func | §13.4, §13.7.2 | ParseRequestInfo parses a Nats-Request-Info value. |
| `identity.ReadRequestInfo` | func | §13.4, §13.7.2 | ReadRequestInfo reads the stamp from message headers. |
| `identity.DefaultResolveTTL` | const | §13.10 | DefaultResolveTTL is how long one enumeration is reused (suggested by SAP §13.10, not required). |
| `identity.DefaultListMaxWait` | const | §13.8, §13.10 | DefaultListMaxWait caps one $SRV.INFO enumeration. |
| `identity.DefaultListStall` | const | §13.8, §13.10 | DefaultListStall ends an enumeration once no reply has arrived for this long after the first. |
| `identity.Registration` | type | §13.8, §13.10 | Registration is one agent instance as $SRV.INFO reports it. |
| `identity.RegistrationLister` | type | §13.8, §13.10 | RegistrationLister enumerates the agent instances the connection's account can reach, as $SRV.INFO.agents d... |
| `identity.ListOptions` | type | §13.8, §13.10 | ListOptions configure [ConnLister]. |
| `identity.ConnLister` | func | §13.10 | ConnLister returns a [RegistrationLister] that scatter-gathers $SRV.INFO.agents over nc. |
| `identity.ResolverOptions` | type | §13.8, §13.10 | ResolverOptions configure a [SenderResolver]. |
| `identity.SenderResolver` | type | §13.10 | SenderResolver maps a verified sender back to a protocol address (SAP §13.10, "Reverse lookup"). |
| `identity.NewSenderResolver` | func | §13.8, §13.10 | NewSenderResolver returns a resolver over list. |
| `identity.SenderResolver.Invalidate` | method | §13.10 | Invalidate drops the cached index; the next Resolve enumerates again. |
| `identity.SenderResolver.Resolve` | method | §13.10 | Resolve returns the verified instance registered under id. |
| `identity.UserInfoSubject` | const | §13.4 | UserInfoSubject is the subject that returns a connection's own user and account to any connected user. |
| `identity.DefaultUserInfoTimeout` | const | §13.4 | DefaultUserInfoTimeout is the default timeout of the $SYS.REQ.USER.INFO request, 2 seconds. |
| `identity.DefaultSelfIDRetryInterval` | const | §13.4 | DefaultSelfIDRetryInterval is the default interval after which a failed lookup is retried (SDK policy; SAP... |
| `identity.ErrNoIdentity` | var | §13.4 | ErrNoIdentity means the server answered and the connection has no NKEY user (no authentication, a password... |
| `identity.ErrIdentityUnknown` | var | §13.4 | ErrIdentityUnknown means the implementation does not know the connection's identity: the server did not ans... |
| `identity.Requester` | type | §13.4 | Requester is the part of a NATS connection the identity lookup needs. |
| `identity.Requester.RequestWithContext` | method | §13.4 |  |
| `identity.SelfIDOptions` | type | §13.4 | SelfIDOptions configure a [SelfIDResolver]. |
| `identity.SelfIDResolver` | type | §13.4 | SelfIDResolver learns a connection's own agent ID once per connection (SAP §13.4) and remembers it. |
| `identity.NewSelfIDResolver` | func | §13.4 | NewSelfIDResolver returns a resolver over conn. |
| `identity.SelfIDResolver.Peek` | method | §13.4 | Peek returns the remembered ID without asking the server, and false when there is none: never looked up, in... |
| `identity.SelfIDResolver.Reset` | method | §13.4 | Reset forgets the remembered ID and any remembered failure, so the next call asks again. |
| `identity.SelfIDResolver.ID` | method | §13.4 | ID returns the connection's agent ID. |
| `identity.SelfIDResolver.ObserveError` | method | §13.4 | ObserveError feeds an asynchronous NATS error to the resolver. |
| `identity.IdentityFromUserInfoReply` | func | §13.4 | IdentityFromUserInfoReply derives the agent ID from a $SYS.REQ.USER.INFO reply ({"data":{"user":...,"accoun... |
| `identity.SignOptions` | type | §13.6 | SignOptions are the inputs of [SignHeader]. |
| `identity.SignHeader` | func | §13.6.1, §13.5, §13.11 | SignHeader builds and signs an Agent-Sender header for one request. |
| `identity.ClaimHeader` | func | §13.5, §13.1 | ClaimHeader builds an unsigned claim: exactly v, account, user and an optional name. |
| `identity.NewNonce` | func | §13.5 | NewNonce returns a fresh nonce of 22 characters from [A-Za-z0-9], the shape of a NUID, which SAP §13.5 RECO... |
| `identity.ErrInvalidSeed` | var | §13.6 | ErrInvalidSeed is returned (wrapped) when key material is not a usable user seed or credentials file. |
| `identity.ErrIdentityMismatch` | var | §13.4 | ErrIdentityMismatch is returned (wrapped) when two sources of a connection's identity disagree, or when a s... |
| `identity.Signer` | type | §13.6 | Signer signs with a connection's user NKEY seed. |
| `identity.Signer.PublicKey` | method | §13.6 | PublicKey returns the user public NKEY ("U...") this signer signs for. |
| `identity.Signer.Sign` | method | §13.6 | Sign returns an ed25519 signature over data (the NKEY Sign operation). |
| `identity.JWTSigner` | type | §13.4 | JWTSigner is a [Signer] that also carries the user JWT of a credentials file. |
| `identity.JWTSigner.JWT` | method | §13.4 | JWT returns the user JWT. |
| `identity.SignerWiper` | type | §13.6 | SignerWiper is implemented by signers that can clear their key material. |
| `identity.SignerWiper.Wipe` | method | §13.6 | Wipe clears the key material. |
| `identity.SignerFromSeed` | func | §13.2.1 | SignerFromSeed builds a [Signer] from a user seed ("SU..."). |
| `identity.SignerFromCreds` | func | §13.4 | SignerFromCreds builds a [JWTSigner] from the text of a NATS credentials file: the user JWT and the user seed. |
| `identity.SignerFromSeedAndJWT` | func | §13.6 | SignerFromSeedAndJWT is [SignerFromSeed] with a user JWT, for a program that holds the seed and the JWT apart. |
| `identity.IdentityFromJWT` | func | §13.4 | IdentityFromJWT returns the agent ID a user JWT carries: sub is the user public NKEY, and the account is na... |
| `identity.MetadataKeyMinSenderTrust` | const | §13.9 | MetadataKeyMinSenderTrust is the key of the prompt endpoint metadata that declares what the endpoint requir... |
| `identity.MetadataKeyUserNKey` | const | §13.10 | Registration metadata keys of the agent ID, next to agent, owner and protocol_version. |
| `identity.MetadataKeyAccount` | const | §13.10 | Registration metadata keys of the agent ID, next to agent, owner and protocol_version. |
| `identity.MetadataKeyIDSig` | const | §13.10 | Registration metadata keys of the agent ID, next to agent, owner and protocol_version. |
| `identity.MinSenderTrust` | type | §13.9 | MinSenderTrust is what a prompt endpoint requires of the sender. |
| `identity.TrustAny` | const | §13.1, §13.7 | TrustAny serves every request. |
| `identity.TrustSigned` | const | §13.7 | TrustSigned serves a request only when it is verified (SAP §13.7). |
| `identity.ParseMinSenderTrust` | func | §13.9 | ParseMinSenderTrust interprets a declared min_sender_trust. |
| `identity.ErrCannotSign` | var | §13.4, §13.9 | ErrCannotSign is the local refusal of SAP §13.4 and §13.9: the target endpoint requires a signed sender and... |
| `identity.CheckCanSend` | func | §13.9 | CheckCanSend returns nil when a caller may send to an endpoint that requires required. |
| `identity.PromptMetadata` | func | §13.9 | PromptMetadata returns the identity key a host adds to its prompt endpoint metadata: min_sender_trust. |
| `identity.Trust` | type | §13.1 | Trust is the trust class of a sender. |
| `identity.TrustVerified` | const | §13.1, §13.7 | TrustVerified means the Agent-Sender header carries a valid signature: the sender holds the seed for the user. |
| `identity.TrustClaimed` | const | §13.1, §13.7 | TrustClaimed means the header has no signature. |
| `identity.TrustAbsent` | const | §13.1, §13.7 | TrustAbsent means no Agent-Sender header, or one with an unknown v. |
| `identity.Message` | type | §13.1, §13.7 | Message is the part of a message verification needs: the subject it arrived on (the stored subject, for a s... |
| `identity.MessageFromNATS` | func | §13.1, §13.7 | MessageFromNATS converts a NATS message. |
| `identity.Sender` | type | §13.1, §13.7 | Sender is a classified sender. |
| `identity.ClaimedIdentity` | type | §13.1, §13.7 | ClaimedIdentity is the account and user in a header without a valid signature. |
| `identity.TrustOf` | func | §13.1, §13.7 | TrustOf returns the trust class of s: [TrustAbsent] for nil. |
| `identity.Sender.Describe` | method | §13.12 | Describe returns a short text for logs that always carries the trust class, as SAP §13.12 asks of whatever... |
| `identity.SubjectRules` | type | §13.7.1 | SubjectRules configure which sub values are acceptable for an arrival subject (SAP §13.7.1). |
| `identity.LiveOptions` | type | §13.1, §13.7 | LiveOptions configure [VerifyLive]. |
| `identity.StoredOptions` | type | §13.8 | StoredOptions configure [VerifyStored]. |
| `identity.VerifyLive` | func | §13.7 | VerifyLive classifies a live request and verifies its Agent-Sender header (SAP §13.7). |
| `identity.VerifyStored` | func | §13.8 | VerifyStored verifies the authorship of a stored message (SAP §13.8): the signature and sub, from the store... |

## Package `connect`: connection helpers

Reads NATS CLI contexts and credential files into connection options and, optionally, a signer. SAP does not cover connecting, so these are outside the protocol; the official SDKs have equivalents.

| Identifier | Kind | Section | Note |
|---|---|---|---|
| `connect.Source` | type | outside SAP: connection helper | Source selects where a [Bundle] gets its connection credentials: either a NATS CLI context by name, or a UR... |
| `connect.Source.String` | method | outside SAP: connection helper | String shows the source with URL userinfo removed (a malformed URL prints as a placeholder). |
| `connect.Source.GoString` | method | outside SAP: connection helper | GoString is String, so %#v cannot print URL userinfo. |
| `connect.Source.MarshalJSON` | method | outside SAP: connection helper | MarshalJSON redacts URL userinfo; the field names are the Go field names. |
| `connect.Source.Validate` | method | outside SAP: connection helper | Validate reports why the source is ambiguous or empty. |
| `connect.BundleOption` | type | outside SAP: connection helper | BundleOption configures [ResolveBundle]. |
| `connect.WithSignedIdentity` | func | §13.4, §13.6 | WithSignedIdentity makes [ResolveBundle] return a Signer derived from the same credential read as the conne... |
| `connect.WithBundleContextOptions` | func | outside SAP: connection helper | WithBundleContextOptions passes context-location options (such as [WithConfigDir]) to the context read of [... |
| `connect.Bundle` | type | §13.4 | Bundle is a connection and, when requested, a signer, both made from one read of the credential files, so a... |
| `connect.Bundle.Wipe` | method | §13.4 | Wipe clears the retained key material and drops the options. |
| `connect.Bundle.String` | method | §13.4 | String reports only the identity mode. |
| `connect.Bundle.GoString` | method | §13.4 | GoString is String, so %#v cannot print option values. |
| `connect.Bundle.MarshalJSON` | method | §13.4 | MarshalJSON reports only the identity mode. |
| `connect.ResolveBundle` | func | outside SAP: connection helper | ResolveBundle resolves src into a [Bundle]: the URL and options, and with [WithSignedIdentity] the signer b... |
| `connect.CurrentContext` | const | outside SAP: connection helper | CurrentContext is the selector that resolves through $NATS_CONTEXT and the context.txt selection file. |
| `connect.ContextFile` | type | outside SAP: connection helper | ContextFile is a NATS CLI context file that has been read and parsed but not turned into options. |
| `connect.ContextFile.String` | method | outside SAP: connection helper | String prints the name and path only, never the fields. |
| `connect.ContextFile.GoString` | method | outside SAP: connection helper | GoString is String, so %#v cannot print the fields. |
| `connect.ContextFile.MarshalJSON` | method | outside SAP: connection helper | MarshalJSON redacts the fields that hold secrets: any field whose name contains "seed", "token", "password"... |
| `connect.ContextOption` | type | outside SAP: connection helper | ContextOption customizes how a context is located. |
| `connect.WithInsecureKeyFilePermissions` | func | outside SAP: connection helper | WithInsecureKeyFilePermissions turns off the key file permission check. |
| `connect.WithConfigDir` | func | outside SAP: connection helper | WithConfigDir reads contexts from dir/context/NAME.json (and the selection from dir/context.txt) instead of... |
| `connect.ReadContext` | func | outside SAP: connection helper | ReadContext reads the NATS CLI context named selector, or the current one when selector is "current" ([Curr... |
| `connect.LoadContext` | func | outside SAP: connection helper | LoadContext reads the named context (see [ReadContext]) and returns the server URL and the options for it:... |
| `connect.SignerFromContext` | func | §13.6 | SignerFromContext returns the sender-identity signer for the user seed a context holds: its creds file, els... |
| `connect.ErrContext` | var | outside SAP: connection helper | ErrContext is matched by every error this package returns for a context, URL, source or credential file it... |
| `connect.ErrNoUserSeed` | var | §13.6 | ErrNoUserSeed is matched (together with [ErrContext]) when a signer is requested but the credential source... |
| `connect.ContextError` | type | outside SAP: connection helper | ContextError reports why a context, URL or source could not be used. |
| `connect.ContextError.Error` | method | outside SAP: connection helper | Error implements error. |
| `connect.ContextError.Unwrap` | method | outside SAP: connection helper | Unwrap returns the underlying cause. |
| `connect.ContextError.Is` | method | outside SAP: connection helper | Is reports whether target is [ErrContext]. |
| `connect.DefaultReconnectWait` | const | outside SAP: connection helper | DefaultReconnectWait is the wait between reconnect attempts. |
| `connect.DefaultReconnectJitter` | const | outside SAP: connection helper | DefaultReconnectJitter is the random addition to that wait. |
| `connect.DefaultReconnectJitterTLS` | const | outside SAP: connection helper | DefaultReconnectJitterTLS is the jitter for TLS connections (the nats.go default). |
| `connect.ReconnectDefaults` | func | outside SAP: connection helper | ReconnectDefaults returns options that keep an agent reachable: never give up reconnecting (MaxReconnects -... |
| `connect.WithAgentReconnectDefaults` | func | outside SAP: connection helper | WithAgentReconnectDefaults returns [ReconnectDefaults] followed by user. |
| `connect.Connection` | type | outside SAP: connection helper | Connection is a server URL and the options that authenticate to it. |
| `connect.ParseURL` | func | outside SAP: connection helper | ParseURL parses one NATS URL, or several separated by commas, and moves credentials in the userinfo into op... |

## Package `agentstest`: test support

A reference agent for tests of programs that talk to an agent. It is not part of the protocol.

| Identifier | Kind | Section | Note |
|---|---|---|---|
| `agentstest.DefaultHeartbeatInterval` | const | §8.2 | DefaultHeartbeatInterval is the reference agent's heartbeat cadence, the shortest SAP recommends (SAP §8.2). |
| `agentstest.RawHandler` | type | outside SAP: test support | RawHandler serves a prompt with the raw micro request and the classified sender (nil for the absent class),... |
| `agentstest.ReferenceOption` | type | outside SAP: test support | ReferenceOption configures a [ReferenceAgent]. |
| `agentstest.WithHandler` | func | outside SAP: test support | WithHandler replaces the echo with a handler served through the full host pipeline (decode, admission, ack,... |
| `agentstest.WithRawHandler` | func | outside SAP: test support | WithRawHandler serves prompts with h and no help from the host. |
| `agentstest.WithHost` | func | outside SAP: test support | WithHost passes options to the underlying [host.Host], after the reference agent's own defaults, so they win. |
| `agentstest.ReferenceAgent` | type | outside SAP: test support | ReferenceAgent is a spec-compliant agent for tests. |
| `agentstest.NewReferenceAgent` | func | outside SAP: test support | NewReferenceAgent builds a reference agent for subject on nc. |
| `agentstest.ReferenceAgent.Start` | method | outside SAP: test support | Start registers the agent and publishes its first heartbeat. |
| `agentstest.ReferenceAgent.Stop` | method | outside SAP: test support | Stop stops the agent without closing the connection. |
| `agentstest.ReferenceAgent.InstanceID` | method | outside SAP: test support | InstanceID returns the service id, the heartbeat's instance_id; empty before Start. |
| `agentstest.ReferenceAgent.PromptSubject` | method | outside SAP: test support | PromptSubject returns the prompt endpoint's subject. |
| `agentstest.ReferenceAgent.HeartbeatSubject` | method | outside SAP: test support | HeartbeatSubject returns the heartbeat subject. |
| `agentstest.ReferenceAgent.StatusSubject` | method | outside SAP: test support | StatusSubject returns the status endpoint's subject. |
| `agentstest.ReferenceAgent.SenderGate` | method | outside SAP: test support | SenderGate returns the gate that classifies the agent's requests. |
| `agentstest.ReferenceAgent.Identity` | method | outside SAP: test support | Identity returns the agent ID the agent registered, if any. |
| `agentstest.ReferenceAgent.MinSenderTrust` | method | outside SAP: test support | MinSenderTrust returns the min_sender_trust the agent advertises. |
| `agentstest.ReferenceAgent.Host` | method | outside SAP: test support | Host returns the underlying host, for the options and accessors this package does not repeat. |

## Package `tools`: agent tool layer

The SDK counterpart of the official SDKs' agent-tool helpers: tool definitions and an executor over `caller`. SAP defines no tools, so every row is outside the protocol.

| Identifier | Kind | Section | Note |
|---|---|---|---|
| `tools.ToolDiscoverAgents` | const | outside SAP: agent tool layer | ToolDiscoverAgents lists the agents that can be prompted, one entry per address. |
| `tools.ToolPromptAgent` | const | outside SAP: agent tool layer | ToolPromptAgent sends one prompt and waits for the reply or a question, or returns at once. |
| `tools.ToolWaitAgent` | const | outside SAP: agent tool layer | ToolWaitAgent waits for the first of some calls that finished or asked a question. |
| `tools.ToolAnswerAgent` | const | outside SAP: agent tool layer | ToolAnswerAgent answers the open question of a call. |
| `tools.ToolCancelAgent` | const | outside SAP: agent tool layer | ToolCancelAgent stops calls: it drops their streams and refuses their questions. |
| `tools.ToolListAgentCalls` | const | outside SAP: agent tool layer | ToolListAgentCalls lists the calls tracked in the current scope. |
| `tools.ToolNames` | func | outside SAP: agent tool layer | ToolNames returns the six tool names in the contract's order, as a fresh slice (AGENT_TOOL_NAMES in the off... |
| `tools.BlockingTools` | func | outside SAP: agent tool layer | BlockingTools returns the three tools an agent offers when it runs no calls at once, in the contract's orde... |
| `tools.Definition` | type | outside SAP: agent tool layer | Definition is one tool as a model is shown it: a name, the description it reads and its parameters as a JSO... |
| `tools.ToolDefinitions` | func | outside SAP: agent tool layer | ToolDefinitions returns the six definitions in the contract's order, as a fresh copy on every call: a progr... |
| `tools.DepthHeader` | const | §5.6 | DepthHeader is the application header that carries a prompt's delegation depth: how many tool prompts led t... |
| `tools.ContextWithDepth` | func | outside SAP: agent tool layer | ContextWithDepth returns ctx marked as serving a prompt that arrived at delegation depth n. |
| `tools.DepthFromHeader` | func | outside SAP: agent tool layer | DepthFromHeader reads the delegation depth from the headers of a received prompt: 0 when the header is miss... |
| `tools.DepthInterceptor` | func | outside SAP: agent tool layer | DepthInterceptor returns the caller interceptor that stamps [DepthHeader] on the prompts these tools make w... |
| `tools.Extension` | type | outside SAP: agent tool layer | Extension adds to the tools without changing them (docs/agent-tools.md section 6.3). |
| `tools.Extension.Name` | method | outside SAP: agent tool layer | Name identifies the extension in logs and refusals. |
| `tools.DiscoveryFields` | type | §4 | DiscoveryFields adds fields to a discover_agents entry, given the agent's live handle. |
| `tools.DiscoveryFields.DiscoveryFields` | method | §4 |  |
| `tools.PromptRewriter` | type | outside SAP: agent tool layer | PromptRewriter rewrites a prompt before prompt_agent sends it, after the guards. |
| `tools.PromptRewriter.RewritePrompt` | method | outside SAP: agent tool layer |  |
| `tools.ReplyLooker` | type | §6.3, §7.1 | ReplyLooker looks at a completed reply and at an arriving question, and may add fields to that result. |
| `tools.ReplyLooker.LookReply` | method | §6.3, §7.1 |  |
| `tools.PromptContext` | type | outside SAP: agent tool layer | PromptContext is what a [PromptRewriter] sees. |
| `tools.PromptRewrite` | type | outside SAP: agent tool layer | PromptRewrite is what a [PromptRewriter] may return; every field is optional. |
| `tools.ReplyContext` | type | outside SAP: agent tool layer | ReplyContext is what a [ReplyLooker] sees. |
| `tools.DefaultMaxCalls` | const | outside SAP: agent tool layer | DefaultMaxCalls is the number of calls tracked per scope (256, the value both official SDKs use). |
| `tools.DefaultMaxSavedBytesPerCall` | const | outside SAP: agent tool layer | DefaultMaxSavedBytesPerCall is the total of returned files saved per call, 64 MiB, the SDK's default for [a... |
| `tools.QuestionRefusal` | const | §7.2 | QuestionRefusal is what an open question is answered with when its call is canceled (by cancel_agent, a hos... |
| `tools.DefaultCloseTimeout` | const | outside SAP: agent tool layer | DefaultCloseTimeout bounds how long [Tools.Close] waits for the calls to end. |
| `tools.DefaultMaxReplyBytes` | const | outside SAP: agent tool layer | DefaultMaxReplyBytes is the reply text kept per call, 4 MiB. |
| `tools.DefaultMaxQuestions` | const | outside SAP: agent tool layer | DefaultMaxQuestions is the questions one call may ask over its life, 64. |
| `tools.DefaultMaxReturnedFiles` | const | outside SAP: agent tool layer | DefaultMaxReturnedFiles is the returned-file records one call may collect, 64. |
| `tools.DefaultMaxResultBytes` | const | outside SAP: agent tool layer | DefaultMaxResultBytes is the total a call may hold for its result, 8 MiB: reply text, question texts and re... |
| `tools.DefaultMaxDepth` | const | outside SAP: agent tool layer | DefaultMaxDepth is how many delegation hops deep a prompt may go, 4. |
| `tools.DefaultMaxActiveCalls` | const | outside SAP: agent tool layer | DefaultMaxActiveCalls is the calls open at once across every scope of one [Tools], 1024. |
| `tools.DefaultMaxAttachments` | const | outside SAP: agent tool layer | DefaultMaxAttachments is the attachments one prompt_agent call may carry, 32 (SDK policy). |
| `tools.Option` | type | outside SAP: agent tool layer | Option configures [New]. |
| `tools.WithTools` | func | outside SAP: agent tool layer | WithTools offers only the named tools, of the six; the default is all six. |
| `tools.WithSelfAddress` | func | outside SAP: agent tool layer | WithSelfAddress sets the agent's own prompt subject: it is left out of discovery and prompt_agent refuses i... |
| `tools.WithDiscoverTimeout` | func | §4.1 | WithDiscoverTimeout sets how long one discovery waits (the caller's WithDiscoverTimeout strategy). |
| `tools.WithMaxWait` | func | outside SAP: agent tool layer | WithMaxWait sets the runtime limit per call; past it the call is expired. |
| `tools.WithMaxWaitAgent` | func | outside SAP: agent tool layer | WithMaxWaitAgent sets the cap on wait_agent's timeout_ms, which is also its default. |
| `tools.WithMaxCalls` | func | outside SAP: agent tool layer | WithMaxCalls sets how many calls are tracked per scope; the default is [DefaultMaxCalls]. |
| `tools.WithAttachmentRoots` | func | outside SAP: agent tool layer | WithAttachmentRoots names directories files may be sent from, besides the staging directory, which always i... |
| `tools.WithStagingDir` | func | outside SAP: agent tool layer | WithStagingDir sets where returned files are saved, one directory per call. |
| `tools.WithMaxSavedBytesPerCall` | func | outside SAP: agent tool layer | WithMaxSavedBytesPerCall sets the total of returned files saved per call; the default is [DefaultMaxSavedBy... |
| `tools.WithOnSettled` | func | outside SAP: agent tool layer | WithOnSettled sets a function called for every call that finishes outside a served prompt, with its result... |
| `tools.WithExtensions` | func | outside SAP: agent tool layer | WithExtensions adds extensions, run in the order given. |
| `tools.WithLogger` | func | outside SAP: agent tool layer | WithLogger sets the logger. |
| `tools.WithMaxReplyBytes` | func | §6 | WithMaxReplyBytes sets the reply text kept per call; the default is [DefaultMaxReplyBytes]. |
| `tools.WithMaxQuestions` | func | outside SAP: agent tool layer | WithMaxQuestions sets how many questions one call may ask in all; the default is [DefaultMaxQuestions]. |
| `tools.WithMaxReturnedFiles` | func | outside SAP: agent tool layer | WithMaxReturnedFiles sets how many returned-file records one call may collect; the default is [DefaultMaxRe... |
| `tools.WithMaxResultBytes` | func | outside SAP: agent tool layer | WithMaxResultBytes sets the total one call may hold for its result: reply text, question texts and returned... |
| `tools.WithMaxDepth` | func | outside SAP: agent tool layer | WithMaxDepth sets how many delegation hops a prompt may go: a prompt the program serves at depth d may star... |
| `tools.WithMaxAttachments` | func | §2.1, §5.2 | WithMaxAttachments sets how many attachments one prompt_agent call may list; the default is [DefaultMaxAtta... |
| `tools.WithMaxActiveCalls` | func | outside SAP: agent tool layer | WithMaxActiveCalls sets the calls open at once across every scope of the helper; the default is [DefaultMax... |
| `tools.WithAnswerReplyPrefixes` | func | §7.1 | WithAnswerReplyPrefixes sets the subject prefixes an answer (and the refusal of a question) may be publishe... |
| `tools.ErrClosed` | var | outside SAP: agent tool layer | ErrClosed is returned by [Tools.Execute] and [Tools.RunInPromptScope] after [Tools.Close]. |
| `tools.CallState` | type | outside SAP: agent tool layer | CallState is a call's state (docs/agent-tools.md section 1.1). |
| `tools.StateRunning` | const | outside SAP: agent tool layer | StateRunning: the agent works on it. |
| `tools.StateInputRequired` | const | §7 | StateInputRequired: the agent asked a question (SAP §7) that waits for answer_agent. |
| `tools.StateCompleted` | const | §6.5 | StateCompleted: the stream ended with its terminator (SAP §6.5). |
| `tools.StateFailed` | const | §9, §6.6 | StateFailed: no reply came: the prompt was refused (SAP §9), the stream stalled (SAP §6.6), or the transpor... |
| `tools.StateCancelled` | const | outside SAP: agent tool layer | StateCanceled: cancel_agent stopped it, or the prompt it belonged to ended. |
| `tools.StateExpired` | const | outside SAP: agent tool layer | StateExpired: it ran past its runtime limit. |
| `tools.Result` | type | outside SAP: agent tool layer | Result is a tool's result: a JSON-serializable object the program hands its model as JSON text. |
| `tools.CallResult` | type | outside SAP: agent tool layer | CallResult is one call's result as prompt_agent, answer_agent, wait_agent and the OnSettled function give i... |
| `tools.CallResult.CallID` | method | outside SAP: agent tool layer | CallID returns the call's id, "" if the result has none. |
| `tools.CallResult.State` | method | outside SAP: agent tool layer | State returns the call's state, "" if the result has none. |
| `tools.ReturnedFile` | type | §6.3, §7.1 | ReturnedFile is a file the prompted agent sent, as a result lists it. |
| `tools.SettledInfo` | type | outside SAP: agent tool layer | SettledInfo is what [WithOnSettled] is told besides the result. |
| `tools.ExecuteOption` | type | outside SAP: agent tool layer | ExecuteOption configures one [Tools.Execute]. |
| `tools.WithToolCallID` | func | outside SAP: agent tool layer | WithToolCallID passes the model's id for this tool call. |
| `tools.ToolCallIDFromContext` | func | §5.6 | ToolCallIDFromContext returns the model's tool-call id that prompt_agent put in the context it passes to th... |
| `tools.PromptContextFromContext` | func | outside SAP: agent tool layer | PromptContextFromContext returns the values a [PromptRewrite] added with its Context field, as seen by a pr... |
| `tools.Tools` | type | §5 | Tools is the AgentTools helper: the definitions a model is shown and the execution of a tool call by name,... |
| `tools.New` | func | outside SAP: agent tool layer | New returns the helper over client. |
| `tools.Tools.Definitions` | method | §5 | Definitions returns the definitions of the tools offered, in the contract's order, as a fresh copy. |
| `tools.Tools.Execute` | method | §5 | Execute runs one tool call from the model and returns its result, which the program hands back as JSON text... |
| `tools.Tools.Interceptor` | method | §13.7 | Interceptor returns the host interceptor that opens a scope around each served prompt: tool calls made from... |
| `tools.Tools.RunInPromptScope` | method | §13.3 | RunInPromptScope runs fn as the handler of a served prompt, for hosts that do not use [Tools.Interceptor]:... |
| `tools.Tools.Close` | method | §5 | Close cancels every open call in every scope, waits for the calls' readers to end, and removes the staging... |
| `tools.Tools.Shutdown` | method | §5 | Shutdown is [Tools.Close] with a deadline the caller controls, as [net/http.Server.Shutdown] is to Close. |

## Package `conformance`: the conformance catalog

The machine-readable catalog of SAP 0.3 requirements and the check that ties each to its tests.

| Identifier | Kind | Section | Note |
|---|---|---|---|
| `conformance.LevelMust` | const | outside SAP: conformance catalog | Levels an entry may carry. |
| `conformance.LevelMustNot` | const | outside SAP: conformance catalog | Levels an entry may carry. |
| `conformance.LevelShould` | const | outside SAP: conformance catalog | Levels an entry may carry. |
| `conformance.LevelShouldNot` | const | outside SAP: conformance catalog | Levels an entry may carry. |
| `conformance.LevelMay` | const | outside SAP: conformance catalog | Levels an entry may carry. |
| `conformance.LevelRecommend` | const | outside SAP: conformance catalog | Levels an entry may carry. |
| `conformance.LevelWire` | const | outside SAP: conformance catalog | Levels an entry may carry. |
| `conformance.RoleCaller` | const | outside SAP: conformance catalog | Roles an entry may carry: who must behave as the entry says. |
| `conformance.RoleHost` | const | outside SAP: conformance catalog | Roles an entry may carry: who must behave as the entry says. |
| `conformance.RoleBoth` | const | outside SAP: conformance catalog | Roles an entry may carry: who must behave as the entry says. |
| `conformance.RoleOperator` | const | outside SAP: conformance catalog | Roles an entry may carry: who must behave as the entry says. |
| `conformance.CapBlackBox` | const | outside SAP: conformance catalog | Capabilities an entry may carry. |
| `conformance.CapNeedsCfg` | const | outside SAP: conformance catalog | Capabilities an entry may carry. |
| `conformance.CapSDK` | const | outside SAP: conformance catalog | Capabilities an entry may carry. |
| `conformance.CapNA` | const | outside SAP: conformance catalog | Capabilities an entry may carry. |
| `conformance.StatusPlanned` | const | outside SAP: conformance catalog | Statuses an entry may carry. |
| `conformance.StatusCovered` | const | outside SAP: conformance catalog | Statuses an entry may carry. |
| `conformance.StatusNA` | const | outside SAP: conformance catalog | Statuses an entry may carry. |
| `conformance.StatusDeferred` | const | outside SAP: conformance catalog | StatusDeferred marks an obligation that no test covers yet and that the catalog says so openly: it needs a... |
| `conformance.Spec` | type | outside SAP: conformance catalog | Spec identifies the pinned revision of the protocol document. |
| `conformance.Entry` | type | outside SAP: conformance catalog | Entry is one atomic obligation. |
| `conformance.Entry.IsStrict` | method | outside SAP: conformance catalog | IsStrict reports whether the entry is a MUST or MUST NOT, the strengths that need a fault check. |
| `conformance.Catalog` | type | outside SAP: conformance catalog | Catalog is the parsed catalog file. |
| `conformance.Parse` | func | outside SAP: conformance catalog | Parse decodes catalog JSON. |
| `conformance.Default` | func | outside SAP: conformance catalog | Default returns the catalog embedded in this package. |
| `conformance.Load` | func | outside SAP: conformance catalog | Load reads a catalog from a file. |
| `conformance.Catalog.ByID` | method | outside SAP: conformance catalog | ByID returns the entry with the given id. |
| `conformance.Catalog.AnyPlanned` | method | outside SAP: conformance catalog | AnyPlanned reports whether any entry still has status "planned". |
| `conformance.Summary` | type | outside SAP: conformance catalog | Summary counts the catalog's entries. |
| `conformance.Catalog.Summarize` | method | outside SAP: conformance catalog | Summarize counts entries by level, role, capability and status. |
| `conformance.MaxSummaryLen` | const | outside SAP: conformance catalog | MaxSummaryLen caps a summary, which must be a paraphrase and not a quotation. |
| `conformance.Validate` | func | outside SAP: conformance catalog | Validate checks the catalog's structure and returns every problem found. |
| `conformance.ValidateSpec` | func | outside SAP: conformance catalog | ValidateSpec checks the catalog against the spec file it pins. |
| `conformance.MaxClaims` | const | outside SAP: conformance catalog | MaxClaims is the number of obligations a single test may name before the checker flags it for review. |
| `conformance.TestFunc` | type | outside SAP: conformance catalog | TestFunc is a Go test function found in a *_test.go file. |
| `conformance.ScanTests` | func | outside SAP: conformance catalog | ScanTests parses every *_test.go file under root and returns the test functions found, which are functions... |
| `conformance.CheckCoverage` | func | outside SAP: conformance catalog | CheckCoverage verifies the catalog's links to tests. |
| `conformance.CatalogJSON` | func | outside SAP: conformance catalog | CatalogJSON returns a copy of the catalog.json this build embeds. |

## Package `runner`: the conformance runner

The black-box checks behind the `sapconform` command.

| Identifier | Kind | Section | Note |
|---|---|---|---|
| `runner.Endpoint` | type | outside SAP: conformance runner | Endpoint is one endpoint of a $SRV.INFO record as the runner saw it. |
| `runner.Instance` | type | outside SAP: conformance runner | Instance is one $SRV.INFO record, kept loosely typed so a check can judge the shape of what the host actual... |
| `runner.Instance.Endpoint` | method | outside SAP: conformance runner | Endpoint returns the endpoint with the given name. |
| `runner.Instance.MetaString` | method | outside SAP: conformance runner | MetaString returns a metadata value that is a string. |
| `runner.HB` | type | outside SAP: conformance runner | HB is one heartbeat message as observed. |
| `runner.T` | type | outside SAP: conformance runner | T is the session a check runs in: one target and its cached observations. |
| `runner.T.ExpectedHeartbeatSubject` | method | outside SAP: conformance runner | ExpectedHeartbeatSubject is agents.hb.{agent}.{owner}.{name} built from the prompt subject's tokens, or ""... |
| `runner.Reply` | type | outside SAP: conformance runner | Reply is one request/reply answer. |
| `runner.Msg` | type | outside SAP: conformance runner | Msg is one message received on a reply subject. |
| `runner.Msg.IsTerminator` | method | §6.5 | IsTerminator reports a zero-byte message with no headers (SAP §6.5). |
| `runner.Msg.ErrorCode` | method | outside SAP: conformance runner | ErrorCode returns the Nats-Service-Error-Code header value. |
| `runner.Stream` | type | outside SAP: conformance runner | Stream is everything one request received on its reply subject. |
| `runner.Stream.FirstLatency` | method | outside SAP: conformance runner | FirstLatency is the time from publishing to the first message. |
| `runner.Stream.ErrorMsg` | method | outside SAP: conformance runner | ErrorMsg returns the first error-headered message. |
| `runner.Stream.Code` | method | outside SAP: conformance runner | Code returns the error code of the stream's error message, or "". |
| `runner.Stream.Served` | method | outside SAP: conformance runner | Served reports a stream that ended with the terminator and no error message. |
| `runner.Stream.Describe` | method | outside SAP: conformance runner | Describe says in a few words how the stream went, for a detail line. |
| `runner.Chunk` | type | outside SAP: conformance runner | Chunk is a decoded non-terminating message body. |
| `runner.Version` | const | outside SAP: conformance runner | Version is the version of the runner, reported in every report. |
| `runner.Scope` | const | outside SAP: conformance runner | Scope is the statement every report carries about what it covers. |
| `runner.Result` | type | outside SAP: conformance runner | Result is the outcome of one check in one run. |
| `runner.Pass` | const | outside SAP: conformance runner | Pass means the obligation was observed to hold. |
| `runner.Fail` | const | outside SAP: conformance runner | Fail means the host was observed to break the obligation. |
| `runner.ConfigMissing` | const | outside SAP: conformance runner | ConfigMissing means the entry needs configuration the operator did not supply. |
| `runner.Policy` | const | outside SAP: conformance runner | Policy means the host made a choice the spec leaves to it (a host-optional 400, for example); it is reporte... |
| `runner.NotApplicable` | const | outside SAP: conformance runner | NotApplicable means this host or this run does not exercise the behavior (the agent never emits a query, th... |
| `runner.NotRun` | const | outside SAP: conformance runner | NotRun means the runner does not check the entry: it is not a host obligation, it is observable only inside... |
| `runner.Outcome` | type | outside SAP: conformance runner | Outcome is what a check returns. |
| `runner.CheckFunc` | type | outside SAP: conformance runner | CheckFunc checks one catalog entry against one target. |
| `runner.Config` | type | outside SAP: conformance runner | Config configures a run. |
| `runner.Timing` | type | outside SAP: conformance runner | Timing is every fixed wait in the runner that is not already a Config field (DiscoverWait, HeartbeatWindow,... |
| `runner.Timing.Defaults` | method | outside SAP: conformance runner | Defaults returns the production timing. |
| `runner.Config.Supplied` | method | outside SAP: conformance runner | Supplied returns the configuration keys the operator supplied. |
| `runner.KeyTrust` | const | outside SAP: conformance runner | Configuration keys. |
| `runner.KeySeed` | const | outside SAP: conformance runner | Configuration keys. |
| `runner.KeyRefused` | const | outside SAP: conformance runner | Configuration keys. |
| `runner.KeySession` | const | outside SAP: conformance runner | Configuration keys. |
| `runner.KeyInst` | const | outside SAP: conformance runner | Configuration keys. |
| `runner.KeyOversize` | const | outside SAP: conformance runner | Configuration keys. |
| `runner.KeyQuery` | const | outside SAP: conformance runner | Configuration keys. |
| `runner.KeySlow` | const | outside SAP: conformance runner | Configuration keys. |
| `runner.KeyAnswer` | const | outside SAP: conformance runner | Configuration keys. |
| `runner.EntryResult` | type | outside SAP: conformance runner | EntryResult is one catalog entry's line in a report. |
| `runner.EntryResult.IsFailure` | method | outside SAP: conformance runner | IsFailure reports whether this result makes the run fail: a fail of a MUST or MUST NOT entry. |
| `runner.EntryResult.IsStrictFailure` | method | outside SAP: conformance runner | IsStrictFailure reports whether this result makes the strict verdict fail: a fail of any level, a MUST with... |
| `runner.TargetInfo` | type | outside SAP: conformance runner | TargetInfo identifies the agent a TargetReport is about. |
| `runner.TargetReport` | type | outside SAP: conformance runner | TargetReport is the result list for one target. |
| `runner.Report` | type | outside SAP: conformance runner | Report is the one report of a run. |
| `runner.Verdict` | type | outside SAP: conformance runner | Verdict is the outcome of a run, in two strengths. |
| `runner.Selector` | type | outside SAP: conformance runner | Selector echoes the target selection of a run. |
| `runner.Report.OK` | method | outside SAP: conformance runner | OK reports the default verdict: no MUST or MUST NOT entry failed on any target. |
| `runner.Report.StrictOK` | method | outside SAP: conformance runner | StrictOK reports the strict verdict: no entry of any level failed and no entry the operator supplied config... |
| `runner.Report.Failures` | method | outside SAP: conformance runner | Failures returns the entries that make the run fail, with their target. |
| `runner.ErrNoTarget` | var | outside SAP: conformance runner | ErrNoTarget is returned when no agent matched the selection. |
| `runner.Runner` | type | outside SAP: conformance runner | Runner runs the catalog's checks against targets. |
| `runner.New` | func | outside SAP: conformance runner | New builds a runner over the embedded catalog. |
| `runner.NewWithCatalog` | func | outside SAP: conformance runner | NewWithCatalog builds a runner over a given catalog. |
| `runner.Runner.Catalog` | method | outside SAP: conformance runner | Catalog returns the catalog the runner uses. |
| `runner.Registered` | func | outside SAP: conformance runner | Registered returns the ids that have a registered check, sorted. |
| `runner.HostEntries` | func | outside SAP: conformance runner | HostEntries returns the catalog's host and both-role entries of a capability. |
| `runner.Runner.Run` | method | outside SAP: conformance runner | Run discovers the targets and checks each. |
