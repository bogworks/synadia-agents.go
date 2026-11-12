// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller

import (
	"context"
	"errors"
	"slices"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
)

// Filter selects agents on the caller's side: every non-empty field must equal
// the record's. A caller with an identity tuple but no instance id discovers
// broadly and narrows the result itself (SAP §4.2).
type Filter struct {
	// Agent matches metadata.agent.
	Agent string
	// Owner matches metadata.owner.
	Owner string
	// Name matches the instance name, the fifth token of the prompt subject.
	Name string
	// Session matches metadata.session.
	Session string
	// ProtocolVersion matches metadata.protocol_version verbatim.
	ProtocolVersion string
}

func (f Filter) match(i *agents.AgentInfo) bool {
	return (f.Agent == "" || f.Agent == i.Agent) &&
		(f.Owner == "" || f.Owner == i.Owner) &&
		(f.Name == "" || f.Name == i.Name) &&
		(f.Session == "" || f.Session == i.Session) &&
		(f.ProtocolVersion == "" || f.ProtocolVersion == i.ProtocolVersion)
}

type discoverConfig struct {
	timeout        time.Duration
	stall          time.Duration
	maxWait        time.Duration
	filter         Filter
	compatibleOnly bool
	verifyID       func(identity.AgentID) bool
	maxRecords     int
}

// DiscoverOption configures [Client.Discover], [Client.ListAgents] and
// [Client.PingAll].
type DiscoverOption func(*discoverConfig)

// WithDiscoverTimeout waits exactly d and returns every responder seen in that
// window (the timer strategy). Without it, discovery returns
// [DefaultDiscoverStall] after the latest reply, or at [DefaultDiscoverMaxWait].
func WithDiscoverTimeout(d time.Duration) DiscoverOption {
	return func(c *discoverConfig) { c.timeout = d }
}

// WithDiscoverStall sets the idle window after the latest reply that ends a
// discovery.
func WithDiscoverStall(d time.Duration) DiscoverOption {
	return func(c *discoverConfig) { c.stall = d }
}

// WithDiscoverMaxWait sets the absolute ceiling of a discovery that has no
// fixed timeout.
func WithDiscoverMaxWait(d time.Duration) DiscoverOption {
	return func(c *discoverConfig) { c.maxWait = d }
}

// WithDiscoverMaxRecords bounds the records one [Client.Discover],
// [Client.Find], [Client.ListAgents] or [Client.PingAll] call accepts; the
// gathering ends, with what was kept, when the bound is reached. It guards the
// caller's memory against a responder that answers with endless distinct
// instances. A value of zero or less keeps [DefaultDiscoverMaxRecords]. SDK
// policy.
func WithDiscoverMaxRecords(n int) DiscoverOption {
	return func(c *discoverConfig) {
		if n > 0 {
			c.maxRecords = n
		}
	}
}

// WithFilter keeps only the agents the filter matches.
func WithFilter(f Filter) DiscoverOption { return func(c *discoverConfig) { c.filter = f } }

// WithCompatibleOnly drops agents whose protocol version the client's policy
// refuses. Without it they are returned and refuse to be prompted
// ([Agent.CheckVersion]).
func WithCompatibleOnly() DiscoverOption { return func(c *discoverConfig) { c.compatibleOnly = true } }

// WithRequireVerifiedIdentity keeps only the agents whose discovery record
// carries a valid id_sig (SAP §13.10) made by the NATS user in expected. With
// the zero AgentID it keeps any agent with a valid id_sig. Records that fail
// are dropped from the result, counted in [Stats.RejectedRecords] and logged at
// debug. It applies to [Client.Discover], [Client.Find] and [Client.ListAgents].
//
// What a valid id_sig proves: the holder of that user's NKEY seed signed this
// record's agent, owner and advertised prompt subject, so that user, and no
// one without the seed, vouches for the binding between the identity and the
// subject the program will publish to. What it does not prove: that the
// discovery reply came from that holder (a signed record can be copied and
// replayed by anyone who sees it, SAP §13.10), that the endpoint later behind
// the subject is the registrant, or that the user is trustworthy. A prompt
// still travels over a subject any permitted NATS client may subscribe to
// unless the account's permissions say otherwise (SAP §13.9). Treat the result
// as identification, not authorization.
func WithRequireVerifiedIdentity(expected identity.AgentID) DiscoverOption {
	return WithRequireVerifiedIdentityFunc(func(id identity.AgentID) bool {
		return expected == "" || id == expected
	})
}

// WithRequireVerifiedIdentityFunc is [WithRequireVerifiedIdentity] with a
// predicate: an agent is kept only when its id_sig verifies and accept returns
// true for the agent ID the signature binds. accept must not block. A nil
// accept keeps any verified agent.
func WithRequireVerifiedIdentityFunc(accept func(identity.AgentID) bool) DiscoverOption {
	if accept == nil {
		accept = func(identity.AgentID) bool { return true }
	}
	return func(c *discoverConfig) { c.verifyID = accept }
}

// admit reports whether info passes the identity requirements in force: the
// client's trust policy, when it has one, and the per-call requirement, when
// the call made one. Both must hold; a call can only narrow the policy.
func (c *Client) admit(dc discoverConfig, info *agents.AgentInfo) bool {
	if dc.verifyID == nil && c.cfg.trust == nil {
		return true
	}
	if !identity.VerifyAgentID(info.Metadata, info.Prompt.Subject) {
		return false
	}
	// id_sig covers the agent, owner and prompt subject only. The status
	// subject and the instance id ride unsigned, so a replayed genuine record
	// could carry an attacker's status subject. Bind them to what is signed:
	// the status subject must be the prompt subject with the verb swapped, and
	// the instance id must be a well-formed token. SDK policy; SAP §13.10 signs
	// only the prompt subject, and the official SDKs check neither. A prompt
	// subject that is not in the default form cannot be bound, so under trust
	// such a record is dropped.
	if agents.ValidateToken(agents.RoleInstanceID, info.InstanceID) != nil || !statusBound(info) {
		return false
	}
	id, err := identity.NewAgentID(info.Metadata[identity.MetadataKeyAccount], info.Metadata[identity.MetadataKeyUserNKey])
	if err != nil {
		return false
	}
	if c.cfg.trust != nil && !c.cfg.trust.trusts(id, info) {
		return false
	}
	return dc.verifyID == nil || dc.verifyID(id)
}

// statusBound reports whether the record's status subject is the default-form
// prompt subject with the verb swapped to status.
func statusBound(info *agents.AgentInfo) bool {
	if info.Status == nil {
		return false
	}
	as, err := agents.ParseAgentSubject(info.Prompt.Subject, agents.VerbPrompt)
	if err != nil {
		return false
	}
	return info.Status.Subject == as.Status()
}

func newDiscoverConfig(opts []DiscoverOption) discoverConfig {
	dc := discoverConfig{stall: DefaultDiscoverStall, maxWait: DefaultDiscoverMaxWait, maxRecords: DefaultDiscoverMaxRecords}
	for _, o := range opts {
		o(&dc)
	}
	return dc
}

// StartTracking starts the client's heartbeat tracker. [Client.Discover] does
// it before its first request; call it directly to track liveness without
// discovering. It is idempotent. See SAP §8.5.
func (c *Client) StartTracking(ctx context.Context) error {
	if err := c.checkOpen(); err != nil {
		return err
	}
	if err := c.tracker.Start(ctx); err != nil {
		return err
	}
	// Close sets the closed flag before it stops the tracker, and Start and Stop
	// are serialized, so either Close's Stop saw this subscription or the flag
	// is visible here. In the second case nothing else will stop it.
	if c.closed.Load() {
		_ = c.tracker.Stop(context.WithoutCancel(ctx))
		return ErrClosed
	}
	return nil
}

// scatter publishes a request with no body to subject and calls handle with
// each reply until the stall window, the ceiling or the timeout ends the
// gather. A server "no responders" status ends it at once with no error.
func (c *Client) scatter(ctx context.Context, subject string, dc discoverConfig, handle func(*nats.Msg) (done bool)) error {
	if err := c.checkOpen(); err != nil {
		return err
	}
	if err := c.mux.ensure(ctx); err != nil {
		return errf("reply subscription", err)
	}
	r, err := c.mux.register(c.cfg.discoverLimit, c.cfg.bufferBytes, nil, nil)
	if err != nil {
		return err
	}
	defer r.close()
	if err := c.publish(&nats.Msg{Subject: subject, Reply: r.subject}); err != nil {
		return err
	}
	wait := dc.maxWait
	stall := dc.stall
	if dc.timeout > 0 {
		wait, stall = dc.timeout, 0
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	var stallT *time.Timer
	var stallC <-chan time.Time
	if stall > 0 {
		stallT = time.NewTimer(stall)
		stallT.Stop()
		stallC = stallT.C
		defer stallT.Stop()
	}
	for {
		select {
		case m := <-r.ch:
			r.take(m)
			if isNoResponders(m) {
				return nil
			}
			if handle(m) {
				return nil
			}
			if stallT != nil {
				stallT.Reset(stall)
			}
		case <-stallC:
			return nil
		case <-deadline.C:
			return nil
		case <-r.over:
			c.log.Warn("discovery buffer overflowed; returning what was gathered", "subject", subject)
			for {
				select {
				case m := <-r.ch:
					r.take(m)
					if handle(m) {
						return nil
					}
				default:
					return nil
				}
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return ErrClosed
		}
	}
}

// ListAgents enumerates $SRV.INFO.agents and returns the service record of
// every SAP agent that answered, once per instance, in arrival order. Services
// that are not agents are dropped silently, and so are unknown endpoints and
// metadata keys, which stay in the record (SAP §4.3, §12). No responders is an
// empty list, not an error. Like [Client.Discover], it starts heartbeat
// tracking first.
//
// Without a trust policy (or [WithRequireVerifiedIdentity]) discovery is
// unauthenticated: any client on the subject can answer $SRV.INFO, and a forged
// reply can redirect prompts, for example by claiming metadata.owner of the
// owner searched for and any agent or name token in the prompt subject, or a
// custom prompt subject. Use [WithTrustPolicy] for any caller that sends
// sensitive prompts.
func (c *Client) ListAgents(ctx context.Context, opts ...DiscoverOption) ([]agents.AgentInfo, error) {
	dc := newDiscoverConfig(opts)
	if err := c.StartTracking(ctx); err != nil {
		return nil, err
	}
	var out []agents.AgentInfo
	seen := map[string]bool{}
	err := c.scatter(ctx, agents.SubjectInfoAll, dc, func(m *nats.Msg) bool {
		info, ok := c.parseInfo(m.Data)
		if !ok || seen[info.InstanceID] || !dc.filter.match(info) {
			return false
		}
		if dc.compatibleOnly && info.CheckVersion(c.versionPolicy()) != nil {
			return false
		}
		if !c.admit(dc, info) {
			c.rejected.Add(1)
			c.log.Debug("dropped a record without the required verified identity")
			return false
		}
		seen[info.InstanceID] = true
		out = append(out, *info)
		return c.recordsFull(len(out), dc, agents.SubjectInfoAll)
	})
	return out, err
}

// recordsFull reports whether a discovery has accepted as many records as it
// may, and logs it when it has.
func (c *Client) recordsFull(n int, dc discoverConfig, subject string) bool {
	if n < dc.maxRecords {
		return false
	}
	c.log.Warn("discovery reached its record limit; returning what was gathered",
		"subject", subject, "limit", dc.maxRecords)
	return true
}

// parseInfo decodes one $SRV.INFO reply and reads it as an agent.
func (c *Client) parseInfo(data []byte) (*agents.AgentInfo, bool) {
	si, err := agents.DecodeServiceInfo(data)
	if err != nil {
		c.log.Debug("dropped an unreadable service info reply")
		return nil, false
	}
	info, err := agents.NewAgentInfo(si)
	if err != nil {
		var na *agents.NotAgentError
		if !errors.As(err, &na) {
			c.log.Debug("dropped a service record")
		}
		return nil, false
	}
	if err := info.CheckSubjectConsistency(); err != nil {
		c.rejected.Add(1)
		c.log.Debug("dropped a record whose subject disagrees with its metadata")
		return nil, false
	}
	if err := info.CheckAgentToken(); err != nil {
		// SAP §2 makes the abbreviation a SHOULD and the record's subject
		// authoritative (§4.3): the record is kept.
		c.log.Debug("kept a record whose subject agent token is not an abbreviation of its agent")
	}
	return info, true
}

// Discover finds the agents reachable on the connection and returns a handle
// for each. It publishes $SRV.INFO.agents (SAP §4.1; both official callers do
// the same, which carries the endpoint capability metadata a caller needs) and,
// before that, starts the heartbeat tracker and flushes it, so no first heartbeat
// is missed (SAP §8.5). No responders is an empty slice, not an error.
//
// With a trust policy ([WithTrustPolicy]) only agents whose id_sig verifies and
// whose signer the policy trusts are returned; the rest are dropped and counted
// in [Stats.RejectedRecords]. Without one, verification is opt-in per call
// ([WithRequireVerifiedIdentity]).
//
// Agents whose protocol version the client's policy refuses are returned
// unless [WithCompatibleOnly] is set; prompting one fails with a
// *[agents.VersionError].
//
// Without a trust policy (or [WithRequireVerifiedIdentity]) discovery is
// unauthenticated: any client on the subject can answer $SRV.INFO, and a forged
// reply can redirect prompts, for example by claiming metadata.owner of the
// owner searched for and any agent or name token in the prompt subject, or a
// custom prompt subject. Use [WithTrustPolicy] for any caller that sends
// sensitive prompts.
func (c *Client) Discover(ctx context.Context, opts ...DiscoverOption) ([]*Agent, error) {
	infos, err := c.ListAgents(ctx, opts...)
	if err != nil {
		return nil, err
	}
	out := make([]*Agent, len(infos))
	for i := range infos {
		out[i] = newAgent(c, &infos[i], true)
	}
	return out, nil
}

// Find returns the first agent the filter matches, discovering broadly and
// narrowing here (SAP §4.2). It returns [ErrAgentNotFound] when none matches.
// Several instances of one logical agent share a filter; use
// [Client.Discover] to see them all.
//
// Without a trust policy (or [WithRequireVerifiedIdentity]) discovery is
// unauthenticated: any client on the subject can answer $SRV.INFO, and a forged
// reply can redirect prompts, for example by claiming metadata.owner of the
// owner searched for and any agent or name token in the prompt subject, or a
// custom prompt subject. Use [WithTrustPolicy] for any caller that sends
// sensitive prompts.
func (c *Client) Find(ctx context.Context, f Filter, opts ...DiscoverOption) (*Agent, error) {
	infos, err := c.ListAgents(ctx, append(slices.Clone(opts), WithFilter(f))...)
	if err != nil {
		return nil, err
	}
	if len(infos) == 0 {
		return nil, ErrAgentNotFound
	}
	return newAgent(c, &infos[0], true), nil
}

// LookupInstance fetches the record of one instance with
// $SRV.INFO.agents.{instance_id} (SAP §4.2). It returns [ErrAgentNotFound] when
// nothing answers or the answer is not an agent. timeout defaults to
// [DefaultLookupTimeout].
//
// Without a trust policy (or [WithRequireVerifiedIdentity]) discovery is
// unauthenticated: any client on the subject can answer $SRV.INFO, and a forged
// reply can redirect prompts, for example by claiming metadata.owner of the
// owner searched for and any agent or name token in the prompt subject, or a
// custom prompt subject. Use [WithTrustPolicy] for any caller that sends
// sensitive prompts.
func (c *Client) LookupInstance(ctx context.Context, instanceID string, timeout ...time.Duration) (*Agent, error) {
	subject, err := agents.InfoSubject(instanceID)
	if err != nil {
		return nil, ErrAgentNotFound
	}
	// Only the record of the instance asked for is accepted: a reply that names
	// another instance is dropped and counted, and the wait goes on until the
	// timeout (SAP §4.2 defines direct lookup for one specific instance).
	var found *agents.AgentInfo
	dc := discoverConfig{timeout: firstTimeout(timeout)}
	err = c.scatter(ctx, subject, dc, func(m *nats.Msg) bool {
		info, ok := c.parseInfo(m.Data)
		if !ok {
			return false
		}
		if info.InstanceID != instanceID {
			c.rejected.Add(1)
			c.log.Debug("dropped a lookup reply for a different instance")
			return false
		}
		if !c.admit(dc, info) {
			c.rejected.Add(1)
			c.log.Debug("dropped a lookup reply without the required verified identity")
			return false
		}
		found = info
		return true
	})
	if err != nil {
		if errors.Is(err, nats.ErrNoResponders) || errors.Is(err, nats.ErrTimeout) {
			return nil, ErrAgentNotFound
		}
		return nil, err
	}
	if found == nil {
		return nil, ErrAgentNotFound
	}
	return newAgent(c, found, true), nil
}

// Ping reports whether one instance answers the micro service ping
// $SRV.PING.agents.{instance_id}, a point-in-time reachability check (SAP §8.4).
// A silent instance is false, not an error. timeout defaults to
// [DefaultLookupTimeout].
func (c *Client) Ping(ctx context.Context, instanceID string, timeout ...time.Duration) (bool, error) {
	subject, err := agents.PingSubject(instanceID)
	if err != nil {
		return false, nil
	}
	_, err = c.request(ctx, &nats.Msg{Subject: subject}, firstTimeout(timeout))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, nats.ErrNoResponders), errors.Is(err, nats.ErrTimeout):
		return false, nil
	default:
		return false, err
	}
}

// PingAll sends the general ping $SRV.PING.agents, to which every agent
// instance answers once (SAP §4), and returns the answers, one per instance,
// in arrival order. Callers correlate them with heartbeats by instance id
// (SAP §8.4).
func (c *Client) PingAll(ctx context.Context, opts ...DiscoverOption) ([]agents.ServicePing, error) {
	dc := newDiscoverConfig(opts)
	if err := c.StartTracking(ctx); err != nil {
		return nil, err
	}
	var out []agents.ServicePing
	seen := map[string]bool{}
	err := c.scatter(ctx, agents.SubjectPingAll, dc, func(m *nats.Msg) bool {
		p, err := agents.DecodeServicePing(m.Data)
		if err != nil || p.Name != agents.ServiceName || p.ID == "" || seen[p.ID] {
			return false
		}
		seen[p.ID] = true
		out = append(out, p)
		return c.recordsFull(len(out), dc, agents.SubjectPingAll)
	})
	return out, err
}

func firstTimeout(d []time.Duration) time.Duration {
	if len(d) > 0 && d[0] > 0 {
		return d[0]
	}
	return DefaultLookupTimeout
}
