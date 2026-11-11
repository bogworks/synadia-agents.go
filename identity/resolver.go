// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
)

// Discovery defaults for [ConnLister] and [SenderResolver].
const (
	// DefaultResolveTTL is how long one enumeration is reused (suggested by SAP
	// §13.10, not required).
	DefaultResolveTTL = 10 * time.Second
	// DefaultListMaxWait caps one $SRV.INFO enumeration.
	DefaultListMaxWait = 2 * time.Second
	// DefaultListStall ends an enumeration once no reply has arrived for this
	// long after the first.
	DefaultListStall = 300 * time.Millisecond

	srvInfoAgentsSubject = "$SRV.INFO.agents"
	promptEndpointName   = "prompt"
)

// Registration is one agent instance as $SRV.INFO reports it.
type Registration struct {
	// Name and ID are the service name and instance id.
	Name string
	ID   string
	// Metadata is the service metadata, including user_nkey, account and
	// id_sig.
	Metadata map[string]string
	// PromptSubject is the subject of the instance's prompt endpoint.
	PromptSubject string
}

// RegistrationLister enumerates the agent instances the connection's account
// can reach, as $SRV.INFO.agents does.
type RegistrationLister func(ctx context.Context) ([]Registration, error)

// ListOptions configure [ConnLister].
type ListOptions struct {
	// MaxWait caps an enumeration. Zero means [DefaultListMaxWait].
	MaxWait time.Duration
	// Stall ends an enumeration after this long without a reply, once one has
	// arrived. Zero means [DefaultListStall].
	Stall time.Duration
}

// ConnLister returns a [RegistrationLister] that scatter-gathers
// $SRV.INFO.agents over nc. Discovery is account-local: it sees only agents
// whose $SRV subjects the connection's account can reach (SAP §13.10).
func ConnLister(nc *nats.Conn, opts ListOptions) RegistrationLister {
	if opts.MaxWait == 0 {
		opts.MaxWait = DefaultListMaxWait
	}
	if opts.Stall == 0 {
		opts.Stall = DefaultListStall
	}
	return func(ctx context.Context) ([]Registration, error) {
		inbox := nc.NewRespInbox()
		ch := make(chan *nats.Msg, 256)
		sub, err := nc.ChanSubscribe(inbox, ch)
		if err != nil {
			return nil, err
		}
		defer func() { _ = sub.Unsubscribe() }()
		if err := nc.PublishRequest(srvInfoAgentsSubject, inbox, nil); err != nil {
			return nil, err
		}
		deadline := time.NewTimer(opts.MaxWait)
		defer deadline.Stop()
		var stall *time.Timer
		var stallC <-chan time.Time
		defer func() {
			if stall != nil {
				stall.Stop()
			}
		}()
		var out []Registration
		for {
			select {
			case m := <-ch:
				if reg, ok := parseInfo(m.Data); ok {
					out = append(out, reg)
				}
				if stall == nil {
					stall = time.NewTimer(opts.Stall)
				} else {
					if !stall.Stop() {
						select {
						case <-stall.C:
						default:
						}
					}
					stall.Reset(opts.Stall)
				}
				stallC = stall.C
			case <-stallC:
				return out, nil
			case <-deadline.C:
				return out, nil
			case <-ctx.Done():
				return out, ctx.Err()
			}
		}
	}
}

func parseInfo(data []byte) (Registration, bool) {
	if checkNoDuplicateKeys(data) != nil {
		return Registration{}, false
	}
	var info struct {
		Name      string            `json:"name"`
		ID        string            `json:"id"`
		Metadata  map[string]string `json:"metadata"`
		Endpoints []struct {
			Name    string `json:"name"`
			Subject string `json:"subject"`
		} `json:"endpoints"`
	}
	if json.Unmarshal(data, &info) != nil {
		return Registration{}, false
	}
	reg := Registration{Name: info.Name, ID: info.ID, Metadata: info.Metadata}
	for _, e := range info.Endpoints {
		if e.Name == promptEndpointName {
			reg.PromptSubject = e.Subject
			break
		}
	}
	return reg, true
}

// ResolverOptions configure a [SenderResolver].
type ResolverOptions struct {
	// TTL is how long one enumeration is reused. Zero means
	// [DefaultResolveTTL]; a negative value enumerates on every call.
	TTL time.Duration
	// Now returns the current time. Nil means [time.Now]. Test hook.
	Now func() time.Time
}

// SenderResolver maps a verified sender back to a protocol address (SAP §13.10,
// "Reverse lookup"). It indexes the enumerated records by (account, user_nkey)
// and keeps only instances whose id_sig verifies against their own prompt
// subject. The lookup identifies; it never authorizes. The index is cached for
// the TTL, and concurrent callers share one enumeration. It is safe for
// concurrent use.
type SenderResolver struct {
	list RegistrationLister
	ttl  time.Duration
	now  func() time.Time

	mu       sync.Mutex
	index    map[AgentID]Registration
	builtAt  time.Time
	built    bool
	inflight chan struct{}
	lastErr  error
	// abandoned counts enumerations that failed because the enumerating
	// caller's own context ended. Waiters of such a flight must not inherit
	// its error: they enumerate again.
	abandoned uint64
	gen       uint64 // bumped by Invalidate; an enumeration commits only if it started in the current generation

	// waiters counts callers blocked on the enumeration in flight. Tests read
	// it to prove that concurrent callers share one enumeration.
	waiters atomic.Int32
}

// NewSenderResolver returns a resolver over list.
func NewSenderResolver(list RegistrationLister, opts ResolverOptions) *SenderResolver {
	r := &SenderResolver{list: list, ttl: opts.TTL, now: opts.Now}
	if r.ttl == 0 {
		r.ttl = DefaultResolveTTL
	}
	if r.now == nil {
		r.now = time.Now
	}
	return r
}

// Invalidate drops the cached index; the next Resolve enumerates again. An
// enumeration already in flight describes the registry as it was before the
// call: its result is never cached, and the callers waiting on it enumerate
// again.
func (r *SenderResolver) Invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gen++
	r.built, r.index = false, nil
}

// Resolve returns the verified instance registered under id. It reports
// ok=false when no verified instance claims the key: the sender is then not a
// reachable agent (a human, a plain service, or an agent that is offline).
// Several instances of one logical agent share one agent ID; the first one
// enumerated is returned. An enumeration failure is returned and the previous
// index, if any, is kept.
func (r *SenderResolver) Resolve(ctx context.Context, id AgentID) (Registration, bool, error) {
	if _, perr := ParseAgentID(string(id)); perr != nil {
		return Registration{}, false, perr
	}
	for {
		r.mu.Lock()
		if r.built && r.ttl > 0 && r.now().Sub(r.builtAt) < r.ttl {
			e, found := r.index[id]
			r.mu.Unlock()
			return e, found, nil
		}
		if r.inflight != nil {
			ch := r.inflight
			gen := r.gen
			aband := r.abandoned
			r.mu.Unlock()
			r.waiters.Add(1)
			select {
			case <-ch:
				r.waiters.Add(-1)
				r.mu.Lock()
				if r.gen != gen || r.abandoned != aband {
					// Invalidate ran during the enumeration, or its caller gave
					// up: its result was discarded. Ask again.
					r.mu.Unlock()
					continue
				}
				e, found := r.index[id]
				lerr := r.lastErr
				r.mu.Unlock()
				if lerr != nil {
					return Registration{}, false, lerr
				}
				return e, found, nil
			case <-ctx.Done():
				r.waiters.Add(-1)
				return Registration{}, false, ctx.Err()
			}
		}
		ch := make(chan struct{})
		r.inflight = ch
		gen := r.gen
		r.mu.Unlock()

		regs, lerr := r.list(ctx)

		r.mu.Lock()
		r.inflight = nil
		if r.gen != gen {
			// Invalidate ran during the enumeration: the result describes the
			// registry before it and must not be cached. Unless the caller is
			// gone, enumerate again.
			close(ch)
			r.mu.Unlock()
			if cerr := ctx.Err(); cerr != nil {
				return Registration{}, false, cerr
			}
			continue
		}
		if lerr != nil && ctx.Err() != nil {
			// The enumerating caller's context ended: that is its own
			// failure, not the registry's. Waiters with a live context
			// enumerate again.
			r.abandoned++
			close(ch)
			r.mu.Unlock()
			return Registration{}, false, lerr
		}
		r.lastErr = lerr
		if lerr == nil {
			idx := make(map[AgentID]Registration)
			for _, rg := range regs {
				if !VerifyAgentID(rg.Metadata, rg.PromptSubject) {
					continue
				}
				aid, aerr := NewAgentID(rg.Metadata[MetadataKeyAccount], rg.Metadata[MetadataKeyUserNKey])
				if aerr != nil {
					continue
				}
				if _, dup := idx[aid]; !dup {
					idx[aid] = rg
				}
			}
			r.index, r.built, r.builtAt = idx, true, r.now()
		}
		e, found := r.index[id]
		close(ch)
		r.mu.Unlock()
		if lerr != nil {
			return Registration{}, false, lerr
		}
		return e, found, nil
	}
}
