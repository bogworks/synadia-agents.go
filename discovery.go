// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Service and endpoint names the protocol fixes. See SAP §3.1, §3.3, §8.7.
const (
	// ServiceName is the micro service name every agent registers under. It is
	// the discovery filter that separates agents from other micro services.
	// See SAP §3.1.
	ServiceName = "agents"

	// QueueGroup is the queue group the prompt and status endpoints MUST be
	// registered with, set explicitly: the framework default differs between
	// implementations. See SAP §3.3, §8.7.
	QueueGroup = "agents"

	// EndpointPrompt is the required prompt endpoint's name, equal to its
	// verb. See SAP §2.
	EndpointPrompt = string(VerbPrompt)

	// EndpointStatus is the required status endpoint's name. See SAP §8.7.
	EndpointStatus = string(VerbStatus)

	// EndpointAttachments is the name the future attachments endpoint MUST
	// carry. See SAP §2, §5.5.
	EndpointAttachments = string(VerbAttachments)
)

// Metadata keys. See SAP §2.1, §3.2, §13.9.
const (
	MetaAgent           = "agent"
	MetaOwner           = "owner"
	MetaSession         = "session"
	MetaProtocolVersion = "protocol_version"
	MetaMaxPayload      = "max_payload"
	MetaAttachmentsOK   = "attachments_ok"
	// MetaMinSenderTrust is the prompt endpoint metadata key whose presence
	// advertises the sender-identity extension (§13.9).
	MetaMinSenderTrust = "min_sender_trust"
)

// ServiceMetadata is the required service metadata of an agent. See SAP §3.2.
type ServiceMetadata struct {
	// Agent is the canonical harness identifier (metadata.agent).
	Agent string
	// Owner is the operator or account (metadata.owner).
	Owner string
	// Session is the harness session label. It MUST be set for session-aware
	// harnesses and MAY be omitted for session-less ones; empty omits it.
	Session string
	// ProtocolVersion defaults to [ProtocolVersion] when empty.
	ProtocolVersion string
}

// Map returns the metadata as the micro service registration carries it. Keys
// the SDK does not define can be added to the result: additional keys MAY be
// included. See SAP §3.2.
func (m ServiceMetadata) Map() map[string]string {
	pv := m.ProtocolVersion
	if pv == "" {
		pv = ProtocolVersion
	}
	out := map[string]string{MetaAgent: m.Agent, MetaOwner: m.Owner, MetaProtocolVersion: pv}
	if m.Session != "" {
		out[MetaSession] = m.Session
	}
	return out
}

// PromptEndpointMetadata is the endpoint metadata the prompt endpoint MUST
// declare: max_payload and attachments_ok. See SAP §2.1.
type PromptEndpointMetadata struct {
	// MaxPayload is the largest single request payload in bytes; must be positive.
	MaxPayload int64
	// AttachmentsOK says whether JSON envelopes with attachments are accepted.
	AttachmentsOK bool
}

// Map returns the metadata as the registration carries it: max_payload in the
// "1MB" grammar and attachments_ok as "true" or "false". Other keys, such as
// min_sender_trust, can be added to the result. See SAP §2.1.
func (m PromptEndpointMetadata) Map() map[string]string {
	return map[string]string{
		MetaMaxPayload:    FormatSize(m.MaxPayload),
		MetaAttachmentsOK: strconv.FormatBool(m.AttachmentsOK),
	}
}

// Limits returns the [PromptLimits] a caller derives from this metadata. A
// non-positive MaxPayload makes them invalid ([PromptLimits.Validate]).
func (m PromptEndpointMetadata) Limits() PromptLimits {
	ok := m.AttachmentsOK
	return PromptLimits{MaxPayload: m.MaxPayload, AttachmentsOK: &ok}
}

// SenderTrust is the level a prompt endpoint requires of senders (optional
// extension). See SAP §13.9.
type SenderTrust string

const (
	// TrustAny accepts any sender.
	TrustAny SenderTrust = "any"
	// TrustSigned requires a verified sender.
	TrustSigned SenderTrust = "signed"
)

// ParseSenderTrust reads a min_sender_trust value: "any" is TrustAny, and
// "signed" and every unknown value are TrustSigned, the strictest level a
// caller can satisfy on its own. ok is false when the key is absent. See SAP
// §13.9.
func ParseSenderTrust(raw string, present bool) (t SenderTrust, ok bool) {
	if !present {
		return "", false
	}
	if raw == string(TrustAny) {
		return TrustAny, true
	}
	return TrustSigned, true
}

// EndpointInfo is one endpoint of a service record, as the micro framework
// reports it in $SRV.INFO. See SAP §4.3.
type EndpointInfo struct {
	Name string `json:"name"`
	// Subject is the subject to publish to, verbatim. Callers MUST use it, not
	// a subject built from identity (SAP §2, §4).
	Subject string `json:"subject"`
	// QueueGroup is the registered queue group; empty means none. The prompt
	// endpoint MUST use "agents" (SAP §3.3).
	QueueGroup string            `json:"queue_group,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// MaxPayload returns max_payload in bytes. ok is false when it is missing or
// not in the SAP §2.1 grammar; [EndpointInfo.PromptLimits] then fails.
func (e EndpointInfo) MaxPayload() (n int64, ok bool) {
	raw, present := e.Metadata[MetaMaxPayload]
	if !present {
		return 0, false
	}
	n, err := ParseSize(raw)
	return n, err == nil
}

// AttachmentsOK returns attachments_ok. ok is false unless the value is
// "true" or "false". See SAP §2.1.
func (e EndpointInfo) AttachmentsOK() (v, ok bool) {
	switch e.Metadata[MetaAttachmentsOK] {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// MinSenderTrust returns the endpoint's min_sender_trust. See [ParseSenderTrust].
func (e EndpointInfo) MinSenderTrust() (SenderTrust, bool) {
	raw, present := e.Metadata[MetaMinSenderTrust]
	return ParseSenderTrust(raw, present)
}

// PromptLimits returns what a caller enforces locally before publishing to
// this prompt endpoint. max_payload and attachments_ok are mandatory (SAP
// §2.1); when either is missing or malformed the endpoint is unusable and the
// error is a [*CapabilityError], which matches [ErrInvalidCapability]. See SAP
// §2.1, §5.4.
func (e EndpointInfo) PromptLimits() (PromptLimits, error) {
	n, ok := e.MaxPayload()
	if !ok {
		return PromptLimits{}, &CapabilityError{Key: MetaMaxPayload, Reason: "missing or not in the SAP §2.1 size grammar"}
	}
	v, ok := e.AttachmentsOK()
	if !ok {
		return PromptLimits{}, &CapabilityError{Key: MetaAttachmentsOK, Reason: "missing or not \"true\" or \"false\""}
	}
	return PromptLimits{MaxPayload: n, AttachmentsOK: &v}, nil
}

// ServiceInfo is the micro $SRV.INFO record of a service instance. See SAP
// §4.
type ServiceInfo struct {
	Name        string            `json:"name"`
	ID          string            `json:"id"`
	Version     string            `json:"version"`
	Description string            `json:"description,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Endpoints   []EndpointInfo    `json:"endpoints"`
}

// DecodeServiceInfo parses an $SRV.INFO response. Unknown fields are ignored.
// A body that is not a service info object is a [*ProtocolError].
func DecodeServiceInfo(data []byte) (ServiceInfo, error) {
	var si ServiceInfo
	if err := json.Unmarshal(data, &si); err != nil {
		return ServiceInfo{}, &ProtocolError{Reason: "service info must be a JSON object", Err: err}
	}
	return si, nil
}

// ServicePing is the micro $SRV.PING response of a service instance. See SAP
// §4, §8.4.
type ServicePing struct {
	Name     string            `json:"name"`
	ID       string            `json:"id"`
	Version  string            `json:"version"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// DecodeServicePing parses an $SRV.PING response.
func DecodeServicePing(data []byte) (ServicePing, error) {
	var p ServicePing
	if err := json.Unmarshal(data, &p); err != nil {
		return ServicePing{}, &ProtocolError{Reason: "service ping must be a JSON object", Err: err}
	}
	return p, nil
}

// AgentInfo is the view of an agent that a caller assembles from a service
// record. See SAP §4.3.
type AgentInfo struct {
	// InstanceID is the service id, unique per running instance; the heartbeat's
	// instance_id.
	InstanceID string
	// Agent is metadata.agent, the canonical identifier.
	Agent string
	// Owner is metadata.owner.
	Owner string
	// Session is metadata.session; empty when absent.
	Session string
	// Name is the instance name, the 5th token of the prompt subject when it
	// has the default form; empty when the subject is custom.
	Name string
	// ProtocolVersion is metadata.protocol_version verbatim; compare it with
	// [CheckProtocolVersion].
	ProtocolVersion string
	// Description and Version are the service's own fields (Version is the
	// harness's, not the protocol's).
	Description string
	Version     string
	// Metadata is the whole service metadata; unknown keys are preserved
	// (SAP §3.2), including the optional identity keys of §13.10.
	Metadata map[string]string
	// Endpoints are all registered endpoints.
	Endpoints []EndpointInfo
	// Prompt is the prompt endpoint, present on every valid record.
	Prompt EndpointInfo
	// Status is the status endpoint, present on every valid record.
	Status *EndpointInfo
}

// NewAgentInfo reads a service record as an agent. It returns a
// [*NotAgentError] (matching [ErrNotAgent]) when the record is not one: the
// service name is not "agents" (SAP §3.1), agent, owner or protocol_version is
// missing from the metadata (SAP §3.2), or no endpoint is named "prompt" or
// none is named "status" (SAP §4.3, §8.7: every agent exposes both). A caller drops such records silently so unrelated micro services do
// not pollute discovery. It does not judge the protocol version; see
// [AgentInfo.CheckVersion].
func NewAgentInfo(si ServiceInfo) (*AgentInfo, error) {
	if si.Name != ServiceName {
		return nil, &NotAgentError{Reason: "service name is not " + ServiceName}
	}
	agent, okA := si.Metadata[MetaAgent]
	owner, okO := si.Metadata[MetaOwner]
	pv, okP := si.Metadata[MetaProtocolVersion]
	if !okA || !okO || !okP {
		return nil, &NotAgentError{Reason: "metadata lacks agent, owner or protocol_version"}
	}
	info := &AgentInfo{
		InstanceID:      si.ID,
		Agent:           agent,
		Owner:           owner,
		Session:         si.Metadata[MetaSession],
		ProtocolVersion: pv,
		Description:     si.Description,
		Version:         si.Version,
		Metadata:        copyStringMap(si.Metadata),
		Endpoints:       append([]EndpointInfo(nil), si.Endpoints...),
	}
	havePrompt := false
	for i := range info.Endpoints {
		e := info.Endpoints[i]
		e.Metadata = copyStringMap(e.Metadata)
		info.Endpoints[i] = e
		switch e.Name {
		case EndpointPrompt:
			if !havePrompt {
				info.Prompt, havePrompt = e, true
			}
		case EndpointStatus:
			if info.Status == nil {
				c := e
				info.Status = &c
			}
		}
	}
	if !havePrompt {
		return nil, &NotAgentError{Reason: "no endpoint named " + EndpointPrompt}
	}
	if info.Status == nil {
		return nil, &NotAgentError{Reason: "no endpoint named " + EndpointStatus}
	}
	info.Name = InstanceNameFromSubject(info.Prompt.Subject)
	return info, nil
}

// CheckVersion applies a [VersionPolicy] to the agent's protocol version. See
// [CheckProtocolVersion].
func (a *AgentInfo) CheckVersion(policy VersionPolicy) error {
	return CheckProtocolVersion(a.ProtocolVersion, policy)
}

// PromptLimits returns the limits of the prompt endpoint; see
// [EndpointInfo.PromptLimits]. A record whose prompt endpoint lacks valid
// capability metadata is unusable and fails with a [*CapabilityError].
func (a *AgentInfo) PromptLimits() (PromptLimits, error) { return a.Prompt.PromptLimits() }

// Endpoint returns the endpoint with the given name, if registered.
func (a *AgentInfo) Endpoint(name string) (EndpointInfo, bool) {
	for _, e := range a.Endpoints {
		if e.Name == name {
			return e, true
		}
	}
	return EndpointInfo{}, false
}

// SupportsSenderIdentity reports whether the prompt endpoint declares
// min_sender_trust, the feature-detection rule of the sender-identity
// extension. See SAP §13.9.
func (a *AgentInfo) SupportsSenderIdentity() bool {
	_, ok := a.Prompt.MinSenderTrust()
	return ok
}

// PromptEndpoint returns the declared prompt endpoint of a heartbeat as an
// [EndpointInfo], and false when the heartbeat declares none. The registration
// stays authoritative; prefer a discovery record when both are at hand. See
// SAP §8.3.
func (h Heartbeat) PromptEndpoint() (EndpointInfo, bool) {
	ep, ok := h.Endpoints[EndpointPrompt]
	if !ok {
		return EndpointInfo{}, false
	}
	return EndpointInfo{Name: EndpointPrompt, Subject: ep.Subject, Metadata: copyStringMap(ep.Metadata)}, true
}

func copyStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ErrRecordInconsistent matches the error of [AgentInfo.CheckSubjectConsistency]:
// a discovery record whose default-form endpoint subject names a different
// owner than the record's own metadata.
var ErrRecordInconsistent = errors.New("agents: discovery record disagrees with its own subject")

// ErrAgentTokenUnusual matches the error of [AgentInfo.CheckAgentToken]: the
// third token of a default-form endpoint subject is neither metadata.agent nor
// an abbreviation of it. SAP says an abbreviation SHOULD be used (§2), not that
// a record without one is unusable, so this is a diagnostic and never a reason
// to drop a record.
var ErrAgentTokenUnusual = errors.New("agents: subject agent token is not metadata.agent or an abbreviation of it")

// CheckAgentToken reports whether the third token of the record's default-form
// prompt and status subjects is metadata.agent or an abbreviation of it. An
// abbreviation is, as SDK policy, a non-empty subsequence of the canonical
// identifier that starts with the same letter ("cc" for "claude-code"). A
// subject of any other form is custom and is not checked. The error matches
// [ErrAgentTokenUnusual]. SAP states the abbreviation rule as a SHOULD (§2,
// §3.2) and takes the endpoint subject in the record as authoritative (§4.3),
// so a caller uses this for diagnostics only.
func (a *AgentInfo) CheckAgentToken() error {
	for _, e := range a.defaultFormEndpoints() {
		if !agentTokenMatches(e.parts[2], a.Agent) {
			return fmt.Errorf("%w: %s subject", ErrAgentTokenUnusual, e.role)
		}
	}
	return nil
}

type defaultEndpoint struct {
	role  string
	parts []string
}

// defaultFormEndpoints returns the prompt and status endpoints whose subject
// has the default form agents.{verb}.{agent}.{owner}.{name}.
func (a *AgentInfo) defaultFormEndpoints() []defaultEndpoint {
	type ep struct{ role, subject string }
	eps := []ep{{EndpointPrompt, a.Prompt.Subject}}
	if a.Status != nil {
		eps = append(eps, ep{EndpointStatus, a.Status.Subject})
	}
	var out []defaultEndpoint
	for _, e := range eps {
		parts := strings.Split(e.subject, ".")
		if len(parts) != 5 || parts[0] != SubjectRoot || !Verb(parts[1]).Valid() {
			continue
		}
		out = append(out, defaultEndpoint{e.role, parts})
	}
	return out
}

// CheckSubjectConsistency reports whether the record's endpoint subjects agree
// with its owner. For the prompt and status endpoints whose subject has the
// default form agents.{verb}.{agent}.{owner}.{name}, the owner token must equal
// metadata.owner (SAP §3.2, "matches the 4th subject token"). The agent token
// is not checked: SAP says an abbreviation SHOULD be used, not that a record
// without one is unusable (§2); see [AgentInfo.CheckAgentToken] for that
// diagnostic. The instance name is not echoed in metadata, so it is not
// compared. A subject of any other form is custom and is not checked. The error
// matches [ErrRecordInconsistent].
//
// The check catches a careless or hostile record that advertises another
// owner's metadata over a subject of its own default form, which would
// redirect that owner's prompts. It does not authenticate the record: only a
// verifying id_sig does that (SAP §13.10).
func (a *AgentInfo) CheckSubjectConsistency() error {
	for _, e := range a.defaultFormEndpoints() {
		if e.parts[3] != a.Owner {
			return fmt.Errorf("%w: %s subject owner token does not match metadata.owner", ErrRecordInconsistent, e.role)
		}
	}
	return nil
}

// agentTokenMatches reports whether token is agent or an abbreviation of it.
func agentTokenMatches(token, agent string) bool {
	if token == agent {
		return true
	}
	if token == "" || agent == "" {
		return false
	}
	t, g := []rune(token), []rune(agent)
	if t[0] != g[0] {
		return false
	}
	i := 0
	for _, r := range g {
		if i < len(t) && r == t[i] {
			i++
		}
	}
	return i == len(t)
}
