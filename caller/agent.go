// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
)

// maxAttachmentFile is the largest file [AttachmentFromFile] reads. A larger
// file cannot fit any NATS payload (SDK policy).
const maxAttachmentFile = 64 << 20

// Agent is a discovered agent instance: an immutable handle over the service
// record it was built from. It is safe for concurrent use. A caller addresses
// the prompt endpoint by the subject in the record and never builds one from
// identity (SAP §2, §4.3).
type Agent struct {
	c    *Client
	info agents.AgentInfo
	// admitted is true when the client's discovery admitted the record: with a
	// trust policy, its id_sig verified and its signer satisfied the policy.
	admitted bool
}

func newAgent(c *Client, info *agents.AgentInfo, admitted bool) *Agent {
	return &Agent{c: c, info: cloneInfo(info), admitted: admitted}
}

// checkTrusted refuses an agent that discovery did not admit when the client
// has a trust policy (SDK policy).
func (a *Agent) checkTrusted() error {
	if a.c.cfg.trust != nil && !a.admitted {
		return ErrUntrustedAgent
	}
	return nil
}

// Info returns a copy of the service record the handle was built from.
func (a *Agent) Info() agents.AgentInfo { return cloneInfo(&a.info) }

// InstanceID returns the service id of the instance, the heartbeat's
// instance_id.
func (a *Agent) InstanceID() string { return a.info.InstanceID }

// PromptSubject returns the prompt endpoint's subject, verbatim from the
// discovery record. See SAP §4.3.
func (a *Agent) PromptSubject() string { return a.info.Prompt.Subject }

// Client returns the client the handle belongs to.
func (a *Agent) Client() *Client { return a.c }

// MinSenderTrust returns what the prompt endpoint requires of senders (SAP
// §13.9). An endpoint without the key implies "any"; an unknown value is read
// as "signed", the strictest level a caller can satisfy on its own.
func (a *Agent) MinSenderTrust() identity.MinSenderTrust {
	v, ok := a.info.Prompt.Metadata[identity.MetadataKeyMinSenderTrust]
	return identity.ParseMinSenderTrust(v, ok)
}

// CheckVersion applies the client's version policy to the agent's declared
// protocol_version. It returns a *[agents.VersionError] when the policy refuses
// it. See SAP §11.2.
func (a *Agent) CheckVersion() error {
	return a.info.CheckVersion(a.c.versionPolicy())
}

// PromptOption configures one [Agent.Prompt].
type PromptOption func(*promptConfig) error

type promptConfig struct {
	inactivity time.Duration
	maxWait    time.Duration
	subject    string
	signSub    string
	buffer     int
	bufferB    int64
	queryPol   func(string) error
}

// WithPromptInactivityTimeout overrides the client's inactivity timeout for one
// prompt. See SAP §6.6.
func WithPromptInactivityTimeout(d time.Duration) PromptOption {
	return func(c *promptConfig) error {
		if d <= 0 {
			return fmt.Errorf("caller: inactivity timeout must be positive")
		}
		c.inactivity = d
		return nil
	}
}

// WithPromptMaxWait overrides the client's stream ceiling for one prompt.
func WithPromptMaxWait(d time.Duration) PromptOption {
	return func(c *promptConfig) error {
		if d <= 0 {
			return fmt.Errorf("caller: max wait must be positive")
		}
		c.maxWait = d
		return nil
	}
}

// WithPromptSubject publishes to subject instead of the one in the discovery
// record. It is for a caller behind a service import that renames the subject
// (SAP §13.6.2); without it the record's subject is used verbatim.
func WithPromptSubject(subject string) PromptOption {
	return func(c *promptConfig) error {
		if err := agents.ValidatePublishSubject(subject); err != nil {
			return err
		}
		c.subject = subject
		return nil
	}
}

// WithSignedSubject signs sub instead of the subject published to: the
// exporter's subject behind an import the caller's own account renamed (SAP
// §13.6.2). Only correct in that one case.
func WithSignedSubject(sub string) PromptOption {
	return func(c *promptConfig) error {
		if err := agents.ValidatePublishSubject(sub); err != nil {
			return err
		}
		c.signSub = sub
		return nil
	}
}

// WithPromptBuffer overrides the client's stream buffer bounds for one prompt;
// zero keeps the client's value for that bound. See [WithStreamBuffer].
func WithPromptBuffer(messages int, bytes int64) PromptOption {
	return func(c *promptConfig) error {
		if messages < 0 || bytes < 0 {
			return fmt.Errorf("caller: stream buffer bounds must not be negative")
		}
		if messages > 0 {
			c.buffer = messages
		}
		if bytes > 0 {
			c.bufferB = bytes
		}
		return nil
	}
}

// WithPromptQueryReplyPolicy overrides the client's query reply policy for the
// queries of one prompt. See [WithQueryReplyPolicy].
func WithPromptQueryReplyPolicy(fn func(subject string) error) PromptOption {
	return func(c *promptConfig) error {
		if fn == nil {
			return fmt.Errorf("caller: nil query reply policy")
		}
		c.queryPol = fn
		return nil
	}
}

// PromptText prompts the agent with text. It is Prompt with an envelope that
// carries only the prompt.
func (a *Agent) PromptText(ctx context.Context, text string, opts ...PromptOption) (*Stream, error) {
	return a.Prompt(ctx, agents.Envelope{Prompt: text}, opts...)
}

// Prompt sends the envelope to the agent and returns its reply stream.
//
// Every local check runs, and fails before anything is published (SAP §5.4;
// errors match [agents.ErrLocal]): the agent's protocol version against the
// client's policy, the envelope (a non-empty UTF-8 prompt, well-formed
// attachments), attachments against attachments_ok, the encoded size against
// max_payload (the Agent-Sender header's bytes included, SAP §13.5.1) and the
// connection's own limit, and an endpoint that requires a signed sender against
// the client's ability to sign (SAP §13.9). With a trust policy, an agent
// discovery did not admit is refused with [ErrUntrustedAgent]. Interceptors and the header hook run
// before the size check; a header SAP defines cannot be set by them.
//
// The request is signed and published during this call, so its timestamp and
// nonce are fresh and every error is returned here. ctx governs the life of the
// stream: canceling it drops the reply registration. Prompt publishes to the
// subject in the discovery record verbatim (SAP §4.3).
func (a *Agent) Prompt(ctx context.Context, env agents.Envelope, opts ...PromptOption) (*Stream, error) {
	c := a.c
	if c.closed.Load() || c.draining.Load() {
		return nil, ErrClosed
	}
	pc := promptConfig{inactivity: c.cfg.inactivity, maxWait: c.cfg.maxWait, buffer: c.cfg.buffer, bufferB: c.cfg.bufferBytes}
	for _, o := range opts {
		if err := o(&pc); err != nil {
			return nil, err
		}
	}
	if err := a.checkTrusted(); err != nil {
		return nil, err
	}
	if err := a.CheckVersion(); err != nil {
		return nil, err
	}
	limits, err := a.info.Prompt.PromptLimits()
	if err != nil {
		return nil, err
	}
	if err := env.Validate(); err != nil {
		return nil, err
	}
	if err := limits.CheckAttachments(len(env.Attachments)); err != nil {
		return nil, err
	}
	subject := a.info.Prompt.Subject
	if pc.subject != "" {
		subject = pc.subject
	}
	sub := subject
	if pc.signSub != "" {
		sub = pc.signSub
	}
	required := a.MinSenderTrust() == identity.TrustSigned

	// Fail locally, before any side effect, when the endpoint needs a signature
	// and this client cannot give one (SAP §13.4, §13.9).
	if required {
		if err := identity.CheckCanSend(identity.TrustSigned, c.signer() != nil); err != nil {
			return nil, local(err)
		}
	}

	info := &PromptInfo{Agent: a, Prompt: env.Prompt, Subject: subject, Attachments: len(env.Attachments), Extra: cloneRaw(env.Extra)}
	col, err := c.runBeforePrompt(ctx, info)
	if err != nil {
		return nil, err
	}
	if len(col.fields) > 0 {
		merged := cloneRaw(env.Extra)
		if merged == nil {
			merged = map[string]json.RawMessage{}
		}
		for k, v := range col.fields {
			merged[k] = v
		}
		env.Extra = merged
	}

	plan, err := c.planSender(ctx, sub, required)
	if err != nil {
		return nil, err
	}
	// Size the message as it will be framed: the headers the interceptors and
	// the hook added, and the sender header whose size is known before it is
	// signed (SAP §5.4, §13.5.1).
	var hdr nats.Header
	if len(col.headers) > 0 || plan != nil {
		hdr = nats.Header{}
		for k, v := range col.headers {
			hdr.Set(k, v)
		}
	}
	hdrBytes := framedHeaderBytes(hdr)
	if plan != nil {
		if hdrBytes == 0 {
			hdrBytes = plan.wire
		} else {
			hdrBytes += plan.wire - headerBlockBytes
		}
	}
	payload, err := limits.Prepare(env, hdrBytes, c.nc.MaxPayload())
	if err != nil {
		return nil, err
	}
	if plan != nil {
		sh, err := c.senderHeader(plan, payload)
		if err != nil {
			return nil, err
		}
		sh.Set(hdr)
		// The completed message is what the servers count: check it once more,
		// whatever the estimate said.
		if err := limits.CheckSize(len(payload), framedHeaderBytes(hdr), c.nc.MaxPayload()); err != nil {
			return nil, err
		}
	}
	c.runBeforePublish(ctx, info, col)

	if err := c.mux.ensure(ctx); err != nil {
		return nil, errf("reply subscription", err)
	}
	s := &Stream{
		c: c, ctx: ctx, subject: subject,
		inactivity: pc.inactivity, maxWait: pc.maxWait,
		closed: make(chan struct{}), expired: make(chan struct{}), queryPol: pc.queryPol,
	}
	r, err := c.mux.register(pc.buffer, pc.bufferB, s.arrived, s.stopTimers)
	if err != nil {
		return nil, err
	}
	s.r = r
	// The deadlines run from here, from the publication, and not from the first
	// iteration: a stream nobody consumes still releases its route (SAP §6.6).
	s.startTimers()
	if err := c.publish(&nats.Msg{Subject: subject, Reply: r.subject, Data: payload, Header: hdr}); err != nil {
		s.stopTimers()
		r.close()
		return nil, err
	}
	// The context drops the registration even when nobody ranges over the stream.
	s.stopCtx = context.AfterFunc(ctx, func() { s.stopTimers(); r.close() })
	return s, nil
}

// StatusOption configures one [Agent.Status].
type StatusOption func(*statusConfig) error

type statusConfig struct {
	timeout time.Duration
	subject string
	signSub string
}

// WithStatusTimeout sets how long Status waits. Zero keeps the default,
// [DefaultStatusTimeout]; a negative duration makes Status fail before it sends
// anything, with a plain validation error (SDK policy).
func WithStatusTimeout(d time.Duration) StatusOption {
	return func(c *statusConfig) error {
		if d < 0 {
			return fmt.Errorf("caller: status timeout must not be negative")
		}
		if d > 0 {
			c.timeout = d
		}
		return nil
	}
}

// WithStatusSubject sends the request to subject instead of the one in the
// discovery record, behind a renaming import.
func WithStatusSubject(subject string) StatusOption {
	return func(c *statusConfig) error { c.subject = subject; return nil }
}

// WithStatusSignedSubject signs sub instead of the subject published to (SAP
// §13.6.2).
func WithStatusSignedSubject(sub string) StatusOption {
	return func(c *statusConfig) error { c.signSub = sub; return nil }
}

// Status asks the agent's status endpoint for a point-in-time heartbeat (SAP
// §8.7). The request body is empty and carries an Agent-Sender header when the
// client has an identity (SAP §13.5); a signed status request is never refused
// for want of one. An error-headered reply is a *[agents.ServiceError] and a
// reply that is not a heartbeat is a *[agents.ProtocolError]. A good reply is
// also fed to the client's heartbeat tracker as a just-arrived heartbeat
// (SAP §8.7, MAY).
func (a *Agent) Status(ctx context.Context, opts ...StatusOption) (agents.Heartbeat, error) {
	c := a.c
	if err := a.checkTrusted(); err != nil {
		return agents.Heartbeat{}, err
	}
	sc := statusConfig{timeout: DefaultStatusTimeout}
	for _, o := range opts {
		if err := o(&sc); err != nil {
			return agents.Heartbeat{}, err
		}
	}
	subject := sc.subject
	if subject == "" {
		if a.info.Status == nil {
			return agents.Heartbeat{}, errf("status", &agents.NotAgentError{Reason: "no status endpoint"})
		}
		subject = a.info.Status.Subject
	}
	if err := agents.ValidatePublishSubject(subject); err != nil {
		return agents.Heartbeat{}, err
	}
	sub := sc.signSub
	if sub == "" {
		sub = subject
	}
	plan, err := c.planSender(ctx, sub, false)
	if err != nil {
		return agents.Heartbeat{}, err
	}
	m := &nats.Msg{Subject: subject}
	if plan != nil {
		sh, err := c.senderHeader(plan, nil)
		if err != nil {
			return agents.Heartbeat{}, err
		}
		m.Header = nats.Header{}
		sh.Set(m.Header)
	}
	reply, err := c.request(ctx, m, sc.timeout)
	if err != nil {
		return agents.Heartbeat{}, err
	}
	if se, ok := agents.ParseServiceError(reply.Header, reply.Data); ok {
		return agents.Heartbeat{}, se
	}
	hb, err := agents.DecodeHeartbeat(reply.Data)
	if err != nil {
		return agents.Heartbeat{}, err
	}
	// Feeding the reply to the tracker is optional (SAP §8.7, MAY), and a status
	// reply is never signed (SAP §13.9), so it must not vouch for liveness in a
	// tracker that requires signed heartbeats. It also must speak for the agent
	// asked, not for another instance.
	// Under a trust policy the status subject must also be the one the signed
	// prompt subject implies, and the request must have gone there, so an
	// unsigned status subject cannot feed liveness.
	boundOK := c.cfg.trust == nil || (statusBound(&a.info) && subject == a.info.Status.Subject)
	if boundOK && !c.tracker.requiresSigned() && (a.info.InstanceID == "" || hb.InstanceID == a.info.InstanceID) {
		c.tracker.observe(hb, subject)
	}
	return hb, nil
}

// AttachmentFromFile reads a regular file into an attachment named by its base
// name only, so no directory of the sender's reaches the agent. A file larger
// than 64 MiB cannot fit a NATS payload and is refused. See SAP §5.2.
func AttachmentFromFile(path string) (agents.Attachment, error) {
	f, err := os.Open(path) //nolint:gosec // the program names the file it attaches
	if err != nil {
		return agents.Attachment{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return agents.Attachment{}, err
	}
	if !st.Mode().IsRegular() {
		return agents.Attachment{}, local(fmt.Errorf("caller: %s is not a regular file", filepath.Base(path)))
	}
	if st.Size() > maxAttachmentFile {
		return agents.Attachment{}, local(fmt.Errorf("caller: %s is larger than %s", filepath.Base(path), agents.FormatSize(maxAttachmentFile)))
	}
	data, err := io.ReadAll(io.LimitReader(f, maxAttachmentFile+1))
	if err != nil {
		return agents.Attachment{}, err
	}
	if len(data) > maxAttachmentFile {
		return agents.Attachment{}, local(fmt.Errorf("caller: %s is larger than %s", filepath.Base(path), agents.FormatSize(maxAttachmentFile)))
	}
	return agents.NewAttachment(filepath.Base(path), data), nil
}

func cloneRaw(m map[string]json.RawMessage) map[string]json.RawMessage {
	if m == nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneEndpoint(e agents.EndpointInfo) agents.EndpointInfo {
	e.Metadata = cloneMap(e.Metadata)
	return e
}

func cloneInfo(in *agents.AgentInfo) agents.AgentInfo {
	out := *in
	out.Metadata = cloneMap(in.Metadata)
	out.Endpoints = make([]agents.EndpointInfo, len(in.Endpoints))
	for i, e := range in.Endpoints {
		out.Endpoints[i] = cloneEndpoint(e)
	}
	out.Prompt = cloneEndpoint(in.Prompt)
	if in.Status != nil {
		s := cloneEndpoint(*in.Status)
		out.Status = &s
	}
	return out
}
