// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
)

// PromptHandler serves one prompt (SAP §5, §6). Returning nil ends the stream
// with the terminator. Returning an error answers with a SAP §9 error frame
// and then the terminator: a [*RejectError] uses its code, an error wrapping
// agents.ErrProtocol is 400, and any other error is 500 "handler error"; its
// text never reaches the caller. ctx ends when the host stops. A handler that
// ignores ctx can outlive [Host.Shutdown].
type PromptHandler func(ctx context.Context, req *Request, w *Responder) error

const (
	stateNew = iota
	stateRunning
	stateStopping
	stateStopped
)

// Stats is a snapshot of the host's counters. The statistics of the micro
// service ($SRV.STATS) count only the dispatch of a prompt, because the host
// serves it on its own goroutine after the micro callback has returned: micro's
// NumErrors stays zero and its processing time is the time to hand the prompt
// over, not to serve it. These fields, also published in the custom data of the
// prompt endpoint's micro stats, are the accurate numbers.
type Stats struct {
	// InFlight is the number of prompts being served or waiting for a slot.
	InFlight int
	// Prompts counts prompts handed to the handler chain.
	Prompts uint64
	// RejectedBusy counts prompts answered 429 under [BusyReject429].
	RejectedBusy uint64
	// RefusedReplies counts prompt and status requests dropped because the
	// reply-subject policy refused their reply subject (see
	// [WithReplySubjectPolicy]).
	RefusedReplies uint64
	// HeartbeatSignFailures counts heartbeats skipped because the signer
	// failed; the host never publishes an unsigned beat in their place.
	HeartbeatSignFailures uint64
	// HeartbeatSignTimeouts counts heartbeats skipped because the signer did
	// not return within [WithHeartbeatSignTimeout] (or a signing was still
	// outstanding); each is also counted in HeartbeatSignFailures.
	HeartbeatSignTimeouts uint64
	// PromptErrors counts the error frames the host sent on prompt streams
	// (handler errors, rejections, 400 to 500 refusals, busy and stopping
	// answers).
	PromptErrors uint64
	// LastPromptError is the code and wire description of the latest such
	// error frame, empty before the first. It never carries a handler's text.
	LastPromptError string
	// PromptsCompleted counts prompts whose serving has finished.
	PromptsCompleted uint64
	// PromptProcessingTime is the total time spent serving the completed
	// prompts, from the moment a slot was taken to the end of the stream.
	PromptProcessingTime time.Duration
}

// Host serves one agent instance: its prompt and status endpoints and its
// heartbeat. It is safe for concurrent use. See SAP §3, §5, §6, §8.
type Host struct {
	nc      *nats.Conn
	subject agents.AgentSubject
	handler PromptHandler
	cfg     config
	log     *slog.Logger

	gate     *identity.Gate
	resolver *identity.SenderResolver
	limits   agents.PromptLimits

	promptMeta map[string]string

	base   context.Context //nolint:containedctx // constructor-owned context for the host's background work, canceled by Close and Shutdown (rule 20)
	cancel context.CancelFunc

	sem      chan struct{}
	inflight sync.WaitGroup
	stopCh   chan struct{}
	done     chan struct{}

	prompts      atomic.Uint64
	rejectedBusy atomic.Uint64
	refusedReply atomic.Uint64
	hbSignFail   atomic.Uint64
	hbSignTO     atomic.Uint64
	active       atomic.Int64
	promptErrs   atomic.Uint64
	completed    atomic.Uint64
	procNS       atomic.Int64
	signing      atomic.Bool // a signer call is outstanding

	errMu   sync.Mutex
	lastErr string

	// lc serializes Start and Stop through registration: whoever holds it
	// owns the lifecycle transition. It is a channel so that Start can give up
	// when its context ends; Stop never gives up.
	lc       chan struct{}
	stopOnce sync.Once

	// stopReq and stopRequested record a Shutdown or Close request at once,
	// before the lifecycle lock is touched. A Start in progress checks them
	// at its checkpoints and tears itself down; exactly one party performs
	// the stop, decided by the state under mu.
	stopReq       atomic.Bool
	stopRequested chan struct{}
	reqOnce       sync.Once
	doneOnce      sync.Once

	// cbs counts the micro callbacks in progress. closed, set under mu once
	// the workers have exited, makes later callbacks do nothing; after cbs
	// drains, done is closed, so nothing is answered once the host is
	// complete.
	cbs    sync.WaitGroup
	closed bool

	mu      sync.Mutex
	state   int
	svc     micro.Service
	id      string
	agentID identity.AgentID
	hasID   bool
	signer  identity.Signer
	hbStop  context.CancelFunc
	hbWG    sync.WaitGroup
}

type infoKey struct{}

// New validates the configuration and returns a host. Nothing is registered
// until [Host.Start]. subject names the instance (agents.NewAgentSubject) and
// handler serves its prompts. The host does not close nc.
func New(nc *nats.Conn, subject agents.AgentSubject, handler PromptHandler, opts ...Option) (*Host, error) {
	if nc == nil {
		return nil, fmt.Errorf("%w: nil connection", ErrConfig)
	}
	if subject.IsZero() {
		return nil, fmt.Errorf("%w: zero subject (use agents.NewAgentSubject)", ErrConfig)
	}
	cfg := defaultConfig()
	for _, o := range opts {
		if o == nil {
			continue
		}
		if err := o(&cfg); err != nil {
			return nil, err
		}
	}
	if handler == nil && cfg.hooks.Raw == nil {
		return nil, fmt.Errorf("%w: nil handler", ErrConfig)
	}
	if err := agents.ValidateInterval(cfg.hbInterval, cfg.subSecond); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfig, err)
	}
	log := cfg.logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err := checkEndpoints(cfg.endpoints, subject); err != nil {
		return nil, err
	}

	h := &Host{
		nc: nc, subject: subject, handler: handler, cfg: cfg, log: log,
		sem:    make(chan struct{}, cfg.maxConc),
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),

		stopRequested: make(chan struct{}),
		lc:            make(chan struct{}, 1),
	}
	h.base, h.cancel = context.WithCancel(context.Background()) //nolint:forbidigo // New takes no context; Close cancels this one (standard rule 20)

	// The payload limit advertised: the connection's own unless lowered.
	mp := nc.MaxPayload()
	switch {
	case cfg.maxPayload == 0 && mp > 0:
		cfg.maxPayload = mp
	case cfg.maxPayload == 0:
		cfg.maxPayload = DefaultMaxPayload
	case mp > 0 && cfg.maxPayload > mp:
		log.Warn("max payload exceeds the server limit; clamping the advertised value",
			"requested", cfg.maxPayload, "server", mp)
		cfg.maxPayload = mp
	}
	if cfg.replyPolicy == nil {
		cfg.replyPolicy = defaultPolicyFor(subject, cfg.endpoints)
	}
	h.cfg = cfg
	pm := agents.PromptEndpointMetadata{MaxPayload: cfg.maxPayload, AttachmentsOK: cfg.attachmentsOK}
	h.promptMeta = pm.Map()
	maps.Copy(h.promptMeta, identity.PromptMetadata(cfg.minTrust))
	h.limits = pm.Limits()

	var accept identity.AcceptFunc
	if cfg.accept != nil {
		hook := cfg.accept
		accept = func(ctx context.Context, s *identity.Sender) (ok bool, err error) {
			defer func() {
				if r := recover(); r != nil {
					ok, err = false, errors.New("host: admission hook panicked")
				}
			}()
			var ri RequestInfo
			if info, _ := ctx.Value(infoKey{}).(*RequestInfo); info != nil {
				ri = *info
			}
			ri.Sender = s
			return hook(ctx, s, &ri)
		}
	}
	gate, err := identity.NewGate(identity.GateOptions{
		MinSenderTrust:     cfg.minTrust,
		Subject:            identity.SubjectRules{AccountTokenPosition: cfg.accountTokenPos},
		ReplayWindow:       cfg.replayWindow,
		MaxNonces:          cfg.maxNonces,
		MaxNoncesPerSender: cfg.maxPerSender,
		OperatorAttested:   cfg.operatorAttested,
		Accept:             accept,
		Logger:             log,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfig, err)
	}
	h.gate = gate
	h.resolver = identity.NewSenderResolver(identity.ConnLister(nc, identity.ListOptions{}),
		identity.ResolverOptions{TTL: cfg.resolveTTL})
	return h, nil
}

func checkEndpoints(eps []Endpoint, subject agents.AgentSubject) error {
	seen := map[string]bool{agents.EndpointPrompt: true, agents.EndpointStatus: true}
	reserved := []string{subject.Prompt(), subject.Status(), subject.Heartbeat(), subject.Attachments()}
	for i, e := range eps {
		switch {
		case e.Name == "":
			return fmt.Errorf("%w: endpoint %d has no name", ErrConfig, i)
		case agents.Verb(e.Name).Valid():
			return fmt.Errorf("%w: endpoint name %q is a reserved verb (SAP §2)", ErrConfig, e.Name)
		case seen[e.Name]:
			return fmt.Errorf("%w: endpoint name %q is reserved or repeated", ErrConfig, e.Name)
		case e.Subject == "":
			return fmt.Errorf("%w: endpoint %q has no subject", ErrConfig, e.Name)
		case e.Handler == nil:
			return fmt.Errorf("%w: endpoint %q has no handler", ErrConfig, e.Name)
		}
		for _, r := range reserved {
			if subjectsOverlap(e.Subject, r) {
				return fmt.Errorf("%w: %w: endpoint %q subject %q could match %q", ErrConfig, ErrEndpointOverlap, e.Name, e.Subject, r)
			}
		}
		seen[e.Name] = true
	}
	return nil
}

// Start registers the micro service, its prompt, status and extra endpoints,
// flushes, and publishes the first heartbeat; "started" means registered at
// the server (SAP §3, §8.2). It fails when called twice. With an identity
// configured it first learns the connection's agent ID (SAP §13.4); with a
// signer a failure is an error, without one the host starts without identity
// metadata.
//
// A [Host.Shutdown] or [Host.Close] that is requested while Start is running
// is honored at once: Start checks for it between its registration steps and
// before it starts the heartbeat, tears down what it registered (service
// unregistered, no heartbeat started, flushed) and returns [ErrStopped]; the
// host is then stopped. Start also returns [ErrStopped] when the stop was
// requested, by a Shutdown whose context ended first, before it took the
// lifecycle lock.
func (h *Host) Start(ctx context.Context) error {
	// The lifecycle lock is held through registration, so a concurrent Stop
	// waits for Start to finish and then removes what Start registered.
	select {
	case h.lc <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	err := h.startLocked(ctx)
	<-h.lc
	// A stop requested after the last checkpoint, or by a Shutdown whose
	// context ended while it waited for the lock, is performed here: the
	// request is never lost. If a Shutdown took the lock first, the state
	// says it already stopped the host.
	if h.stopReq.Load() {
		h.stopRequestedTeardown(ctx)
	}
	return err
}

// startLocked runs with the lifecycle lock held.
func (h *Host) startLocked(ctx context.Context) error {
	h.mu.Lock()
	if h.state != stateNew {
		// Already started, or stopped by a Shutdown that ran before this call.
		h.mu.Unlock()
		return ErrStarted
	}
	if err := h.checkStop(); err != nil {
		// A stop was requested but nobody has performed it yet.
		h.mu.Unlock()
		return err
	}
	h.state = stateRunning
	h.mu.Unlock()
	if err := h.start(ctx); err != nil {
		if errors.Is(err, ErrStopped) {
			// The stop is performed by stopRequestedTeardown, once the lock
			// is released.
			return err
		}
		h.abort()
		return err
	}
	return nil
}

// checkStop reports ErrStopped when a stop was requested.
func (h *Host) checkStop() error {
	if h.stopReq.Load() {
		return ErrStopped
	}
	return nil
}

// requestStop records the stop request: immediately, atomically and without
// the lifecycle lock.
func (h *Host) requestStop() {
	h.stopReq.Store(true)
	h.reqOnce.Do(func() { close(h.stopRequested) })
}

// stopRequestedTeardown takes the lifecycle lock and, if nobody has stopped
// the host yet, stops it. Whoever finds the state new or running performs the
// stop; the other finds it already under way and does nothing.
func (h *Host) stopRequestedTeardown(ctx context.Context) {
	h.lc <- struct{}{}
	performed := h.stopLocked(ctx)
	<-h.lc
	if performed {
		go h.complete()
	}
}

// stopLocked stops the host with the lifecycle lock held: it moves the state
// to stopping, stops the heartbeat and unregisters the service. It reports
// whether it performed the stop; the caller releases the lock and then starts
// complete.
func (h *Host) stopLocked(ctx context.Context) bool {
	h.mu.Lock()
	if h.state != stateNew && h.state != stateRunning {
		h.mu.Unlock()
		return false
	}
	h.state = stateStopping
	h.closeStop()
	svc, hbStop := h.svc, h.hbStop
	h.mu.Unlock()

	if hbStop != nil {
		hbStop()
	}
	if svc != nil {
		if err := svc.Stop(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
			h.log.Warn("service stop failed", "error", err.Error())
		}
		// The unsubscribes are only buffered until flushed: without this, a
		// request another connection sends right after Stop returns can still
		// reach the dead subscription and go unanswered instead of failing at
		// once with no responders.
		fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		if err := h.nc.FlushWithContext(fctx); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
			h.log.Debug("flush after service stop failed", "error", err.Error())
		}
		fcancel()
	}
	return true
}

// abort tears down a failed Start. It runs with the lifecycle lock held, so no
// Stop can be closing the same channels; the Once guards make the completion
// single-owner regardless.
func (h *Host) abort() {
	h.mu.Lock()
	svc := h.svc
	h.svc = nil
	h.state = stateStopped
	h.closed = true
	h.mu.Unlock()
	if svc != nil {
		_ = svc.Stop()
	}
	h.cancel()
	h.closeStop()
	h.cbs.Wait()
	// A prompt may have arrived between registration and the failure; its
	// worker is in the in-flight set and the host is not complete while it
	// runs. The gate is closed, so none can enter now. Like Stop, completion
	// is signaled in the background so that Start returns its error even when
	// a handler ignores its context; a later Stop or Close waits on done.
	go func() {
		h.inflight.Wait()
		h.closeDone()
	}()
}

func (h *Host) closeStop() { h.stopOnce.Do(func() { close(h.stopCh) }) }
func (h *Host) closeDone() { h.doneOnce.Do(func() { close(h.done) }) }

func (h *Host) start(ctx context.Context) error {
	meta := maps.Clone(h.cfg.extraMeta)
	if meta == nil {
		meta = map[string]string{}
	}
	delete(meta, identity.MetadataKeyUserNKey)
	delete(meta, identity.MetadataKeyAccount)
	delete(meta, identity.MetadataKeyIDSig)

	var id identity.AgentID
	var hasID bool
	var signer identity.Signer
	if o := h.cfg.identity; o != nil {
		signer = o.Signer
		r := identity.NewSelfIDResolver(h.nc, identity.SelfIDOptions{Signer: signer, Logger: h.log})
		got, err := r.ID(ctx)
		if serr := h.checkStop(); serr != nil {
			return serr
		}
		switch {
		case err == nil:
			if signer != nil && got.User() != signer.PublicKey() {
				return fmt.Errorf("host: the signer's user does not match the connection's identity: %w", identity.ErrIdentityMismatch)
			}
			id, hasID = got, true
		case signer != nil:
			return fmt.Errorf("host: own identity: %w", err)
		default:
			h.log.Warn("starting without identity metadata", "reason", err.Error())
		}
	}
	if hasID {
		if signer != nil {
			reg, err := identity.RegistrationMetadata(signer, id, h.subject.Agent(), h.subject.Owner(), h.subject.Prompt())
			if err != nil {
				return fmt.Errorf("host: id_sig: %w", err)
			}
			maps.Copy(meta, reg)
		} else {
			meta[identity.MetadataKeyUserNKey] = id.User()
			meta[identity.MetadataKeyAccount] = id.Account()
		}
	}
	// Required keys win over extras.
	maps.Copy(meta, agents.ServiceMetadata{
		Agent: h.subject.Agent(), Owner: h.subject.Owner(), Session: h.cfg.session,
	}.Map())

	desc := h.cfg.description
	if desc == "" {
		desc = h.subject.Agent() + " agent " + h.subject.Name()
	}
	svc, err := micro.AddService(h.nc, micro.Config{
		Name:         agents.ServiceName,
		Version:      h.cfg.version,
		Description:  desc,
		Metadata:     meta,
		StatsHandler: h.endpointStats,
	})
	if err != nil {
		return fmt.Errorf("host: register service: %w", err)
	}
	h.mu.Lock()
	h.svc = svc
	h.id = svc.Info().ID
	h.agentID, h.hasID, h.signer = id, hasID, signer
	h.mu.Unlock()
	if err := h.checkStop(); err != nil {
		return err
	}

	if err := svc.AddEndpoint(agents.EndpointPrompt, micro.HandlerFunc(h.onPrompt),
		micro.WithEndpointSubject(h.subject.Prompt()),
		micro.WithEndpointQueueGroup(agents.QueueGroup),
		micro.WithEndpointMetadata(cloneMap(h.promptMeta))); err != nil {
		return fmt.Errorf("host: register prompt endpoint: %w", err)
	}
	if err := svc.AddEndpoint(agents.EndpointStatus, micro.HandlerFunc(h.onStatus),
		micro.WithEndpointSubject(h.subject.Status()),
		micro.WithEndpointQueueGroup(agents.QueueGroup)); err != nil {
		return fmt.Errorf("host: register status endpoint: %w", err)
	}
	if err := h.checkStop(); err != nil {
		return err
	}
	for _, e := range h.cfg.endpoints {
		eo := []micro.EndpointOpt{micro.WithEndpointSubject(e.Subject)}
		if e.QueueGroup != "" {
			eo = append(eo, micro.WithEndpointQueueGroup(e.QueueGroup))
		}
		if e.Metadata != nil {
			eo = append(eo, micro.WithEndpointMetadata(cloneMap(e.Metadata)))
		}
		if err := svc.AddEndpoint(e.Name, e.Handler, eo...); err != nil {
			return fmt.Errorf("host: register endpoint %q: %w", e.Name, err)
		}
	}
	if err := h.checkStop(); err != nil {
		return err
	}
	fctx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		fctx, cancel = context.WithTimeout(ctx, DefaultStartTimeout)
		defer cancel()
	}
	if err := h.nc.FlushWithContext(fctx); err != nil {
		return fmt.Errorf("host: flush: %w", err)
	}

	// First beat out before Start returns (SAP §8.2: after registration).
	// A beat skipped for a signing failure is already counted and logged; the
	// host still starts and beats again at the next tick. A stop requested
	// while the beat was being signed stops the beat before it is published
	// (publishHeartbeat checks) and the heartbeat loop is never started.
	if err := h.publishHeartbeat(ctx); err != nil && !errors.Is(err, errHeartbeatSign) {
		return err
	}
	hctx, hstop := context.WithCancel(h.base)
	h.mu.Lock()
	if h.stopReq.Load() {
		h.mu.Unlock()
		hstop()
		return ErrStopped
	}
	h.hbStop = hstop
	h.mu.Unlock()
	h.hbWG.Add(1)
	go h.heartbeatLoop(hctx, &h.hbWG)
	return nil
}

// Shutdown shuts the host down gracefully, as [net/http.Server.Shutdown] does: it stops the heartbeat (a beat being
// signed is abandoned), stops accepting prompts (a prompt that arrives
// afterwards is answered 500 "service stopping" and the terminator, and a
// status request is not answered), and waits until ctx is done for the handlers
// in flight and for the micro callbacks that were already running. Then it
// cancels the handlers' contexts and returns ctx.Err(); a canceled handler's
// stream still gets an error frame and the terminator.
//
// The stop is requested at once, before anything can block: Shutdown records
// the request first, and ctx bounds only the waits (for a [Host.Start] in
// progress to release the lifecycle lock, and for completion). If ctx ends
// first the wrapped ctx error is returned and the stop still happens: a Start
// in progress sees the request, unregisters what it registered, starts no
// heartbeat and returns [ErrStopped]. Exactly one of Shutdown and Start
// performs the stop. A later Shutdown or [Host.Close] waits for the real
// completion.
//
// The host is complete when the heartbeat and prompt workers have exited and
// the callbacks have finished; a Shutdown that returned ctx.Err() leaves that
// shutdown running, and a later Shutdown or [Host.Close] waits for it. Once the host
// is complete, a request that was still queued on a subscription is dropped
// unanswered. Shutdown does not close the connection, publishes no going-away
// signal (SAP §8.6) and is idempotent. The shutdown starts whatever ctx is, even
// when ctx is already done. A handler that ignores its context
// delays completion, not the return of a Shutdown whose ctx has ended.
func (h *Host) Shutdown(ctx context.Context) error {
	h.requestStop()
	// A free lock is taken even when ctx is done, so the stop is performed
	// here whenever it can be; otherwise wait for Start, bounded by ctx.
	select {
	case h.lc <- struct{}{}:
	default:
		select {
		case h.lc <- struct{}{}:
		case <-ctx.Done():
			h.cancel()
			return fmt.Errorf("host: shutdown: stop requested, Start still in progress: %w", ctx.Err())
		}
	}
	performed := h.stopLocked(ctx)
	<-h.lc
	if performed {
		go h.complete()
	}
	return h.waitDone(ctx)
}

// waitDone waits for completion. At the deadline it cancels the handlers so
// they wind down, and returns ctx.Err().
func (h *Host) waitDone(ctx context.Context) error {
	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
	}
	h.cancel()
	return ctx.Err()
}

// complete waits for the workers and the callbacks and then signals completion.
// Exactly one Stop starts it.
func (h *Host) complete() {
	h.hbWG.Wait()
	h.inflight.Wait()
	// No worker is left, and the callbacks still queued on the drained
	// subscriptions may answer only until the gate closes: one that enters
	// afterwards does nothing.
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.cbs.Wait()
	h.cancel()
	h.mu.Lock()
	h.state = stateStopped
	h.mu.Unlock()
	h.closeDone()
}

// Close stops the host at once and waits for it to be complete, as
// [net/http.Server.Close] does, so a Host is an [io.Closer]. It cancels the
// handlers' contexts, stops the service and the heartbeat, and waits for the
// heartbeat and prompt workers to exit and for the callbacks to finish, until
// [DefaultCloseTimeout]. Where [Host.Shutdown] lets the handlers in flight
// finish first, Close does not.
//
// When the timeout ends first, Close returns the context error wrapped (test it
// with [errors.Is]); the workers have still been told to stop, and a later Close
// or [Host.Shutdown] waits for the real completion. A handler that ignores its
// context delays completion, not the return of Close. The stop is requested at
// once, as [Host.Shutdown] does; the timeout bounds only the wait. If it ends
// while a [Host.Start] is still in progress, Close returns the error and the
// stop still happens when Start finishes. Close is idempotent and
// does not close the connection. For a deadline of your own, cancel nothing and
// call [Host.Shutdown].
func (h *Host) Close() error {
	h.cancel()
	timeout := DefaultCloseTimeout
	if h.cfg.hooks.CloseTimeout > 0 {
		timeout = h.cfg.hooks.CloseTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout) //nolint:forbidigo // io.Closer has no context; the wait is bounded by DefaultCloseTimeout (rule 20)
	defer cancel()
	if err := h.Shutdown(ctx); err != nil {
		return fmt.Errorf("host: close: %w", err)
	}
	return nil
}

// cbEnter registers a micro callback; it reports false, and the callback must
// do nothing, once the host is complete.
func (h *Host) cbEnter() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.cbs.Add(1)
	return true
}

func (h *Host) cbLeave() { h.cbs.Done() }

// running reports whether the host still answers status requests.
func (h *Host) running() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state == stateRunning
}

// noteError records an error frame sent on a prompt stream.
func (h *Host) noteError(se *agents.ServiceError) {
	h.promptErrs.Add(1)
	h.errMu.Lock()
	h.lastErr = fmt.Sprintf("%d %s", se.Code, se.Description)
	h.errMu.Unlock()
}

// notePrompt records the end of serving one prompt.
func (h *Host) notePrompt(start time.Time) {
	h.procNS.Add(int64(time.Since(start)))
	h.completed.Add(1)
}

// InstanceID returns the micro service id, which is the heartbeat's
// instance_id (SAP §8.3). It is empty before Start.
func (h *Host) InstanceID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.id
}

// Subject returns the instance's subject.
func (h *Host) Subject() agents.AgentSubject { return h.subject }

// Identity returns the agent ID the host registered (user_nkey and account),
// and false when it registered none: before Start, without [WithIdentity], or
// when the lookup failed on an unsigned host. See SAP §13.10.
func (h *Host) Identity() (identity.AgentID, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.agentID, h.hasID
}

// MinSenderTrust returns the min_sender_trust the host advertises and
// enforces. See SAP §13.9.
func (h *Host) MinSenderTrust() identity.MinSenderTrust { return h.gate.MinSenderTrust() }

// OperatorAttested reports whether the operator-attested cross-check is on.
func (h *Host) OperatorAttested() bool { return h.cfg.operatorAttested }

// Gate returns the sender gate of the host, for a test or a tool that
// classifies messages the way the host does.
func (h *Host) Gate() *identity.Gate { return h.gate }

// Service returns the underlying micro service, the escape hatch for
// endpoints that are only known at runtime (the TypeScript service). It is nil
// before Start. Endpoints added through it bypass the duplicate-name guard.
func (h *Host) Service() micro.Service {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.svc
}

// PromptMetadata returns a copy of the metadata of the prompt endpoint:
// max_payload, attachments_ok and min_sender_trust. See SAP §2.1, §13.9.
func (h *Host) PromptMetadata() map[string]string { return cloneMap(h.promptMeta) }

// Stats returns a snapshot of the host's counters.
func (h *Host) Stats() Stats {
	h.errMu.Lock()
	last := h.lastErr
	h.errMu.Unlock()
	return Stats{
		InFlight:     int(h.active.Load()),
		Prompts:      h.prompts.Load(),
		RejectedBusy: h.rejectedBusy.Load(),

		RefusedReplies:        h.refusedReply.Load(),
		HeartbeatSignFailures: h.hbSignFail.Load(),
		HeartbeatSignTimeouts: h.hbSignTO.Load(),
		PromptErrors:          h.promptErrs.Load(),
		LastPromptError:       last,
		PromptsCompleted:      h.completed.Load(),
		PromptProcessingTime:  time.Duration(h.procNS.Load()),
	}
}

// endpointStats is the micro StatsHandler: it adds the host's own numbers to
// the prompt endpoint's stats, because micro measures only the dispatch of a
// prompt (the callback returns once the work is handed to its goroutine), so
// its NumErrors, LastError and processing times exclude the serving itself.
func (h *Host) endpointStats(e *micro.Endpoint) any {
	if e.Name != agents.EndpointPrompt {
		return nil
	}
	s := h.Stats()
	avg := time.Duration(0)
	if s.PromptsCompleted > 0 {
		avg = s.PromptProcessingTime / time.Duration(s.PromptsCompleted) //nolint:gosec // a count far below 2^63
	}
	return map[string]any{
		"in_flight": s.InFlight, "prompts": s.Prompts, "rejected_busy": s.RejectedBusy,
		"errors": s.PromptErrors, "last_error": s.LastPromptError,
		"prompts_completed":          s.PromptsCompleted,
		"processing_time_ns":         int64(s.PromptProcessingTime),
		"average_processing_time_ns": int64(avg),
	}
}

func cloneMap(m map[string]string) map[string]string { return maps.Clone(m) }
