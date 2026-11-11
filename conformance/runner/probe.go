// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	agents "github.com/bogworks/synadia-agents.go"
)

// ---- discovery records ------------------------------------------------------

// Endpoint is one endpoint of a $SRV.INFO record as the runner saw it.
type Endpoint struct {
	Name       string
	Subject    string
	QueueGroup string
	Meta       map[string]any
	HasMeta    bool
}

// Instance is one $SRV.INFO record, kept loosely typed so a check can judge
// the shape of what the host actually sent.
type Instance struct {
	Raw         []byte
	Type        string
	Name        string
	ID          string
	Version     string
	Description string
	// Meta is the service metadata when it is a JSON object; MetaIsObject says
	// whether it was.
	Meta         map[string]any
	MetaIsObject bool
	Endpoints    []Endpoint
	// FieldTypes lists service fields (name, id, version, description) that were
	// present but not strings.
	BadFields []string
	// Problems are structural faults in the record that parsing tolerated: an
	// endpoints entry that is not an object, a field of the wrong type. They fail
	// SAP-4.1-01 for the instance; nothing in a record is dropped unreported.
	Problems []string
	// Dups counts further INFO replies that carried this instance's id, and
	// DupConflict says one of them differed from the first. Either fails SAP-4.1-01.
	Dups        int
	DupConflict bool
}

// Endpoint returns the endpoint with the given name.
func (i *Instance) Endpoint(name string) *Endpoint {
	for k := range i.Endpoints {
		if i.Endpoints[k].Name == name {
			return &i.Endpoints[k]
		}
	}
	return nil
}

// MetaString returns a metadata value that is a string.
func (i *Instance) MetaString(key string) (string, bool) { return metaString(i.Meta, key) }

func metaString(m map[string]any, key string) (string, bool) {
	s, ok := m[key].(string)
	return s, ok
}

func parseInstance(raw []byte) (*Instance, error) {
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	in := &Instance{Raw: append([]byte(nil), raw...)}
	str := func(k string, dst *string) {
		v, present := top[k]
		if !present {
			return
		}
		if s, ok := v.(string); ok {
			*dst = s
		} else {
			in.BadFields = append(in.BadFields, k)
		}
	}
	str("type", &in.Type)
	str("name", &in.Name)
	str("id", &in.ID)
	str("version", &in.Version)
	str("description", &in.Description)
	if m, ok := top["metadata"].(map[string]any); ok {
		in.Meta, in.MetaIsObject = m, true
	}
	if raw, present := top["endpoints"]; present {
		eps, ok := raw.([]any)
		if !ok && raw != nil {
			in.Problems = append(in.Problems, "endpoints is not an array")
		}
		for i, e := range eps {
			em, ok := e.(map[string]any)
			if !ok {
				in.Problems = append(in.Problems, fmt.Sprintf("endpoints[%d] is not an object", i))
				continue
			}
			ep := Endpoint{}
			for _, f := range []struct {
				key string
				dst *string
			}{{"name", &ep.Name}, {"subject", &ep.Subject}, {"queue_group", &ep.QueueGroup}} {
				v, present := em[f.key]
				if !present {
					continue
				}
				if str, ok := v.(string); ok {
					*f.dst = str
				} else {
					in.Problems = append(in.Problems, fmt.Sprintf("endpoints[%d].%s is not a string", i, f.key))
				}
			}
			if v, present := em["metadata"]; present {
				if md, ok := v.(map[string]any); ok {
					ep.Meta, ep.HasMeta = md, len(md) > 0
				} else if v != nil {
					in.Problems = append(in.Problems, fmt.Sprintf("endpoints[%d].metadata is not an object", i))
				}
			}
			in.Endpoints = append(in.Endpoints, ep)
		}
	}
	if in.ID == "" {
		return nil, errors.New("service record has no id")
	}
	return in, nil
}

// collectMany publishes a request and gathers every reply until none has
// arrived for stall after the first, or wait has passed.
func collectMany(ctx context.Context, nc *nats.Conn, subject string, wait time.Duration, limit int, stallMin time.Duration) ([]*nats.Msg, error) {
	inbox := nc.NewInbox()
	sub, err := nc.SubscribeSync(inbox)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := nc.PublishRequest(subject, inbox, nil); err != nil {
		return nil, err
	}
	if err := nc.FlushTimeout(5 * time.Second); err != nil {
		return nil, err
	}
	stall := wait / 4
	if stall < stallMin {
		stall = stallMin
	}
	deadline := time.Now().Add(wait)
	var out []*nats.Msg
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			return out, nil
		}
		w := remain
		if len(out) > 0 && stall < w {
			w = stall
		}
		m, err := sub.NextMsg(w)
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) {
				if len(out) > 0 || time.Until(deadline) <= 0 {
					return out, nil
				}
				continue
			}
			if errors.Is(err, nats.ErrNoResponders) {
				return out, nil // nobody is listening: an empty round, not a failure
			}
			return out, err
		}
		out = append(out, m)
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if limit > 0 && len(out) >= limit {
			return out, nil // replies beyond the cap are not read; see Config.MaxDiscovery
		}
	}
}

// parseInfoReply decodes one INFO reply. A reply that is not a JSON object or
// carries no id is malformed; the error says why in a few words, never with the
// payload.
func parseInfoReply(raw []byte) (*Instance, error) {
	in, err := parseInstance(raw)
	if err != nil {
		return nil, fmt.Errorf("not a JSON object (%d bytes)", len(raw))
	}
	if in.ID == "" {
		return nil, fmt.Errorf("no id (%d bytes)", len(raw))
	}
	return in, nil
}

// discovery is what the discovery rounds found. Nothing a responder sent is
// dropped: every parseable record is an instance (whatever its service name),
// a repeated id is recorded on the first record, a reply that cannot be parsed
// is listed in Malformed, and the reply caps are reported in Truncated.
type discovery struct {
	Insts []*Instance
	// Truncated says a reply cap cut a round short.
	Truncated bool
	// Malformed has one note per reply that cannot be attributed to an instance
	// (not a JSON object, or no id); the run reports itself incomplete.
	Malformed []string
}

// discover runs the $SRV.INFO.agents round and then a $SRV.PING.agents round.
// An instance that answers ping but not INFO becomes an instance too, with a
// record that says so, so that the instance-level checks run on it and fail.
func discover(ctx context.Context, nc *nats.Conn, wait time.Duration, limit int, stallMin time.Duration) (*discovery, error) {
	d := &discovery{}
	msgs, err := collectMany(ctx, nc, agents.SubjectInfoAll, wait, limit, stallMin)
	if err != nil {
		return nil, err
	}
	byID := map[string]*Instance{}
	for _, m := range msgs {
		in, err := parseInfoReply(m.Data)
		if err != nil {
			d.Malformed = append(d.Malformed, fmt.Sprintf("malformed reply on %s: %v", agents.SubjectInfoAll, err))
			continue
		}
		if first := byID[in.ID]; first != nil {
			first.Dups++
			if !bytes.Equal(first.Raw, in.Raw) {
				first.DupConflict = true
			}
			continue
		}
		byID[in.ID] = in
		d.Insts = append(d.Insts, in)
	}
	d.Truncated = limit > 0 && len(msgs) >= limit
	pings, err := collectMany(ctx, nc, agents.SubjectPingAll, wait, limit, stallMin)
	if err != nil {
		return nil, err
	}
	pingSeen := map[string]bool{}
	for _, m := range pings {
		in, err := parseInstance(m.Data)
		if err != nil {
			d.Malformed = append(d.Malformed, fmt.Sprintf("malformed reply on %s: not a JSON object with an id (%d bytes)", agents.SubjectPingAll, len(m.Data)))
			continue
		}
		if pingSeen[in.ID] || byID[in.ID] != nil {
			pingSeen[in.ID] = true // a repeated ping answer is judged on the instance by SAP-4-01
			continue
		}
		pingSeen[in.ID] = true
		in.Problems = append(in.Problems, "answered $SRV.PING.agents but did not answer $SRV.INFO.agents")
		byID[in.ID] = in
		d.Insts = append(d.Insts, in)
	}
	if limit > 0 && len(pings) >= limit {
		d.Truncated = true
	}
	return d, nil
}

// ---- targets ----------------------------------------------------------------

// target is one logical agent: the instances that share a prompt subject.
type target struct {
	key   string
	insts []*Instance
}

func promptSubject(in *Instance) string {
	if ep := in.Endpoint(agents.EndpointPrompt); ep != nil {
		return ep.Subject
	}
	return ""
}

func groupTargets(insts []*Instance, agent, owner, name string) []*target {
	byKey := map[string]*target{}
	var order []string
	for _, in := range insts {
		// A record whose service name is not "agents" is still a responder: it
		// becomes a target so SAP-3.1-01 judges it. It is never filtered out.
		key := promptSubject(in)
		if key == "" {
			key = "instance:" + in.ID
		}
		if !matches(in, key, agent, owner, name) {
			// Kept filter: the operator selected one agent and this record is another
			// agent's unsolicited discovery answer. Out of scope, not a discarded
			// responder; a discover-all run (no selector) never reaches this line.
			continue
		}
		tg := byKey[key]
		if tg == nil {
			tg = &target{key: key}
			byKey[key] = tg
			order = append(order, key)
		}
		tg.insts = append(tg.insts, in)
	}
	sort.Strings(order)
	out := make([]*target, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out
}

func matches(in *Instance, subject, agent, owner, name string) bool {
	var tok []string
	if _, as, err := agents.ParseSubject(subject); err == nil {
		tok = []string{as.Token(), as.Owner(), as.Name()}
	} else {
		tok = []string{"", "", ""}
	}
	ma, _ := in.MetaString(agents.MetaAgent)
	mo, _ := in.MetaString(agents.MetaOwner)
	if agent != "" && agent != tok[0] && agent != ma {
		return false
	}
	if owner != "" && owner != tok[1] && owner != mo {
		return false
	}
	if name != "" && name != tok[2] {
		return false
	}
	return true
}

// ---- heartbeats -------------------------------------------------------------

// HB is one heartbeat message as observed.
type HB struct {
	Subject string
	Reply   string
	Data    []byte
	Header  nats.Header
	At      time.Time
	// InfoAnswered is set when $SRV.INFO.agents.{instance_id} answered at the
	// moment the instance's first heartbeat arrived (see SAP-8.2-05).
	InstanceID string
}

type hbCollector struct {
	sub   *nats.Subscription
	nc    *nats.Conn
	start time.Time
	// maxBeats and maxProbes are the run's caps on retained heartbeats and on
	// INFO probes started because a heartbeat named a new instance.
	maxBeats, maxProbes int
	// infoTimeout bounds one INFO probe (Timing.InfoProbeTimeout).
	infoTimeout time.Duration
	// dropped counts heartbeats not retained, and skipped counts INFO probes not
	// started, because of those caps.
	dropped, skipped int

	mu    sync.Mutex
	beats []HB
	// firstInfo records, per instance id, whether INFO answered at its first beat.
	firstInfo map[string]bool
}

func startHeartbeats(nc *nats.Conn, maxBeats, maxProbes int, infoTimeout time.Duration) (*hbCollector, error) {
	c := &hbCollector{nc: nc, start: time.Now(), firstInfo: map[string]bool{}, maxBeats: maxBeats, maxProbes: maxProbes, infoTimeout: infoTimeout}
	sub, err := nc.Subscribe(agents.SubjectHeartbeatAll, func(m *nats.Msg) {
		b := HB{Subject: m.Subject, Reply: m.Reply, Data: append([]byte(nil), m.Data...), Header: m.Header, At: time.Now()}
		var p struct {
			ID string `json:"instance_id"`
		}
		if json.Unmarshal(m.Data, &p) == nil {
			b.InstanceID = p.ID
		}
		c.mu.Lock()
		if len(c.beats) < c.maxBeats {
			c.beats = append(c.beats, b)
		} else {
			c.dropped++
		}
		first := false
		if b.InstanceID != "" {
			if _, ok := c.firstInfo[b.InstanceID]; !ok {
				if len(c.firstInfo) < c.maxProbes {
					c.firstInfo[b.InstanceID] = false
					first = true
				} else {
					c.skipped++
				}
			}
		}
		c.mu.Unlock()
		if first {
			go c.probeInfo(b.InstanceID)
		}
	})
	if err != nil {
		return nil, err
	}
	if err := nc.Flush(); err != nil {
		_ = sub.Unsubscribe()
		return nil, err
	}
	c.sub = sub
	c.start = time.Now()
	return c, nil
}

// probeInfo asks $SRV.INFO.agents.{id}: service discovery, not a prompt or
// status request, so there is no sender to sign.
func (c *hbCollector) probeInfo(id string) {
	subj, err := agents.InfoSubject(id)
	if err != nil {
		// An id that cannot form a subject cannot be probed; firstInfo[id] stays
		// false, so SAP-8.2-05 fails for the instance. Not a discarded responder.
		return
	}
	msg, err := c.nc.Request(subj, nil, c.infoTimeout)
	ok := err == nil && msg != nil && bytes.Contains(msg.Data, []byte(id))
	c.mu.Lock()
	c.firstInfo[id] = ok
	c.mu.Unlock()
}

func (c *hbCollector) stop() { _ = c.sub.Unsubscribe() }

// strangers describes the retained heartbeats whose instance id is not in known
// (or that carry no parseable id): a heartbeat from an instance that never
// answered discovery. It returns "" when there are none.
func (c *hbCollector) strangers(known map[string]bool) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := map[string]bool{}
	noID := 0
	for _, b := range c.beats {
		switch {
		case b.InstanceID == "":
			noID++
		case !known[b.InstanceID]:
			ids[b.InstanceID] = true
		}
	}
	var parts []string
	if len(ids) > 0 {
		list := make([]string, 0, len(ids))
		for id := range ids {
			list = append(list, id)
		}
		sort.Strings(list)
		if len(list) > 3 {
			list = append(list[:3], fmt.Sprintf("and %d more", len(list)-3))
		}
		parts = append(parts, fmt.Sprintf("%d instance(s) published heartbeats but never answered discovery (%s)", len(ids), strings.Join(list, ", ")))
	}
	if noID > 0 {
		parts = append(parts, fmt.Sprintf("%d heartbeat(s) carried no parseable instance_id", noID))
	}
	return strings.Join(parts, "; ")
}

func (c *hbCollector) snapshot() []HB {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]HB(nil), c.beats...)
}

// ---- the target session -----------------------------------------------------

// T is the session a check runs in: one target and its cached observations.
type T struct {
	cfg *Config
	nc  *nats.Conn
	tg  *target
	hb  *hbCollector

	Insts  []*Instance
	Inst   *Instance
	Prompt *Endpoint
	Status *Endpoint
	// Subject tokens of the prompt subject; empty when it is not the default form.
	AgentTok, OwnerTok, NameTok string
	DefaultForm                 bool

	streams map[string]*Stream
	hbs     []HB
	hbDone  bool
	hbOver  string
	status  *statusObs
	ping    *pingObs
	senders *senderSet

	idStreams []*Stream
	queryObs  map[string]*queryObs
	dead      bool

	// curOver lists the resource caps hit by streams the running check used.
	curOver []string

	// rec is the running entry's one result. Everything merges into it with
	// worse; see merge.go.
	rec recorder
}

// observe records something the running check has already seen for sure (a
// forbidden header on a status reply, say) before it goes on to probes that may
// turn out to need configuration. The runner merges it with the check's own
// outcome by worse, so what was observed cannot be erased by a later gap.
func (t *T) observe(o Outcome) { t.rec.add(o) }

func newT(cfg *Config, tg *target, hb *hbCollector) *T {
	t := &T{cfg: cfg, nc: cfg.Conn, tg: tg, hb: hb, Insts: tg.insts, Inst: tg.insts[0], streams: map[string]*Stream{}, queryObs: map[string]*queryObs{}}
	t.Prompt = t.Inst.Endpoint(agents.EndpointPrompt)
	t.Status = t.Inst.Endpoint(agents.EndpointStatus)
	if t.Prompt != nil {
		if as, err := agents.ParseAgentSubject(t.Prompt.Subject, agents.VerbPrompt); err == nil {
			t.AgentTok, t.OwnerTok, t.NameTok, t.DefaultForm = as.Token(), as.Owner(), as.Name(), true
		}
	}
	return t
}

func (t *T) info() TargetInfo {
	ti := TargetInfo{PromptSubject: t.tg.key}
	if t.Prompt == nil {
		ti.PromptSubject = ""
	}
	ti.Agent, _ = t.Inst.MetaString(agents.MetaAgent)
	ti.Owner, _ = t.Inst.MetaString(agents.MetaOwner)
	ti.Name = t.NameTok
	for _, in := range t.Insts {
		ti.InstanceIDs = append(ti.InstanceIDs, in.ID)
	}
	return ti
}

func (t *T) instanceIDs() map[string]bool {
	m := map[string]bool{}
	for _, in := range t.Insts {
		m[in.ID] = true
	}
	return m
}

// ExpectedHeartbeatSubject is agents.hb.{agent}.{owner}.{name} built from the
// prompt subject's tokens, or "" when the prompt subject is not the default form.
func (t *T) ExpectedHeartbeatSubject() string {
	if !t.DefaultForm {
		return ""
	}
	return fmt.Sprintf("agents.hb.%s.%s.%s", t.AgentTok, t.OwnerTok, t.NameTok)
}

// heartbeats waits for the observation window and returns the target's beats.
func (t *T) heartbeats(ctx context.Context) []HB {
	if t.hbDone {
		t.noteHBOver()
		return t.hbs
	}
	window := t.cfg.HeartbeatWindow
	auto := window <= 0
	if auto {
		window = t.cfg.Timing.AutoHeartbeatWindow
		if st := t.statusObs(ctx); st.Err == nil && len(st.Replies) > 0 {
			if iv := declaredInterval(st.Replies[0].Data); iv > 0 {
				window = time.Duration(2.2 * iv * float64(time.Second))
				if window < t.cfg.Timing.AutoHeartbeatMin {
					window = t.cfg.Timing.AutoHeartbeatMin
				}
				if window > t.cfg.Timing.AutoHeartbeatMax {
					window = t.cfg.Timing.AutoHeartbeatMax
				}
			}
		}
	}
	deadline := t.hb.start.Add(window)
	tick := time.NewTicker(t.cfg.Timing.PollInterval)
	defer tick.Stop()
wait:
	for time.Now().Before(deadline) {
		if auto && t.enoughBeats() {
			break
		}
		select {
		case <-ctx.Done():
			break wait
		case <-tick.C:
		}
	}
	t.hbs = t.filterBeats(t.hb.snapshot())
	t.hbDone = true
	t.hbOver = t.hb.overReason()
	t.noteHBOver()
	return t.hbs
}

// capNotes describes the heartbeat and INFO-probe caps the run reached.
func (c *hbCollector) capNotes() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var parts []string
	if c.dropped > 0 {
		parts = append(parts, fmt.Sprintf("%d heartbeats were not retained after the first %d (--max-heartbeats)", c.dropped, c.maxBeats))
	}
	if c.skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d heartbeats naming new instances started no INFO probe after the first %d (--max-info-probes)", c.skipped, c.maxProbes))
	}
	return strings.Join(parts, "; ")
}

// overReason says whether a heartbeat cap was hit during the run.
func (c *hbCollector) overReason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped > 0 {
		return fmt.Sprintf("more than %d heartbeats were seen, so %d were not retained (--max-heartbeats)", c.maxBeats, c.dropped)
	}
	return ""
}

// noteHBOver makes the running check fail when heartbeats were dropped: its
// cadence and content judgement would rest on a partial set.
func (t *T) noteHBOver() {
	if t.hbOver != "" {
		t.curOver = append(t.curOver, "heartbeats: "+t.hbOver)
	}
}

func (t *T) filterBeats(all []HB) []HB {
	ids := t.instanceIDs()
	want := t.ExpectedHeartbeatSubject()
	var out []HB
	for _, b := range all {
		if ids[b.InstanceID] || (want != "" && b.Subject == want) {
			out = append(out, b)
		}
	}
	return out
}

func (t *T) enoughBeats() bool {
	beats := t.filterBeats(t.hb.snapshot())
	count := map[string]int{}
	for _, b := range beats {
		count[b.InstanceID]++
	}
	for id := range t.instanceIDs() {
		if count[id] < 3 {
			return false
		}
	}
	return true
}

func declaredInterval(data []byte) float64 {
	var p struct {
		I float64 `json:"interval_s"`
	}
	if json.Unmarshal(data, &p) != nil {
		return 0
	}
	return p.I
}

// infoAtFirstBeat reports, for an instance id, whether INFO answered when its
// first heartbeat arrived.
func (t *T) infoAtFirstBeat(id string) (answered, known bool) {
	t.hb.mu.Lock()
	defer t.hb.mu.Unlock()
	v, ok := t.hb.firstInfo[id]
	return v, ok
}

// ---- status and ping --------------------------------------------------------

// Reply is one request/reply answer.
type Reply struct {
	Data   []byte
	Header nats.Header
	At     time.Time
	Sent   []byte
}

type statusObs struct {
	Replies []Reply
	// Extras are answers beyond the first to the same status request.
	Extras []Reply
	Err    error
}

// requestAll sends msg and returns the first reply within timeout, then every
// further reply that arrives within grace, so a second responder is seen and
// not left unread.
func requestAll(ctx context.Context, nc *nats.Conn, msg *nats.Msg, timeout, grace time.Duration) (*nats.Msg, []*nats.Msg, error) {
	inbox := nc.NewInbox()
	sub, err := nc.SubscribeSync(inbox)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = sub.Unsubscribe() }()
	msg.Reply = inbox
	if err := nc.PublishMsg(msg); err != nil {
		return nil, nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	first, err := sub.NextMsgWithContext(rctx)
	if err != nil {
		return nil, nil, err
	}
	var extra []*nats.Msg
	for len(extra) < 16 {
		m, err := sub.NextMsg(grace)
		if err != nil {
			break // grace elapsed: no further responder
		}
		extra = append(extra, m)
	}
	return first, extra, nil
}

// statusObs sends the status probes with no Agent-Sender header, on purpose:
// status is always answerable and a liveness probe must not depend on the
// prober's credentials (SAP §13.9), so the unsigned request is the strictest
// test of that and is served by a signed-only endpoint too.
func (t *T) statusObs(ctx context.Context) *statusObs {
	if t.status != nil {
		return t.status
	}
	obs := &statusObs{}
	t.status = obs
	if t.Status == nil {
		obs.Err = errors.New("no status endpoint registered")
		return obs
	}
	bodies := [][]byte{nil, []byte(`{"sapconform":"this body must be ignored"}`)}
	for i, body := range bodies {
		if i > 0 {
			// ts has one-second precision; two answers in different seconds show the
			// reply is built fresh for each request (SAP §8.7).
			select {
			case <-ctx.Done():
				obs.Err = ctx.Err()
				return obs
			case <-time.After(t.cfg.Timing.FreshTSGap):
			}
		}
		first, extra, err := requestAll(ctx, t.nc, &nats.Msg{Subject: t.Status.Subject, Data: body}, t.cfg.Timing.StatusTimeout, t.cfg.PostTerminatorGrace)
		if err != nil {
			obs.Err = err
			return obs
		}
		obs.Replies = append(obs.Replies, Reply{Data: first.Data, Header: first.Header, At: time.Now(), Sent: body})
		// A second answer to one status request is a second responder; keep it so
		// SAP-8.7-07 judges it and reports it.
		for _, m := range extra {
			obs.Extras = append(obs.Extras, Reply{Data: m.Data, Header: m.Header, At: time.Now(), Sent: body})
		}
	}
	return obs
}

type pingObs struct {
	Msgs []*nats.Msg
	Info *nats.Msg // first direct INFO answer for the primary instance
	// InfoDirect is every direct INFO answer, so a second responder is seen.
	InfoDirect []*nats.Msg
	Err        error
	// InfoErr is the error of the direct INFO request, if any.
	InfoErr error
	// InfoAll are the $SRV.INFO.agents answers re-collected now.
	InfoAll    []*nats.Msg
	InfoAllErr error
}

// pingObs probes $SRV.PING and $SRV.INFO, which are discovery subjects with no
// sender identity (intentionally unsigned).
func (t *T) pingObs(ctx context.Context) *pingObs {
	if t.ping != nil {
		return t.ping
	}
	obs := &pingObs{}
	t.ping = obs
	obs.Msgs, obs.Err = collectMany(ctx, t.nc, agents.SubjectPingAll, t.cfg.DiscoverWait, t.cfg.MaxDiscovery, t.cfg.Timing.DiscoverStallMin)
	if subj, err := agents.InfoSubject(t.Inst.ID); err == nil {
		obs.InfoDirect, obs.InfoErr = collectMany(ctx, t.nc, subj, t.cfg.DiscoverWait, t.cfg.MaxDiscovery, t.cfg.Timing.DiscoverStallMin)
		if len(obs.InfoDirect) > 0 {
			obs.Info = obs.InfoDirect[0]
		}
	}
	obs.InfoAll, obs.InfoAllErr = collectMany(ctx, t.nc, agents.SubjectInfoAll, t.cfg.DiscoverWait, t.cfg.MaxDiscovery, t.cfg.Timing.DiscoverStallMin)
	return obs
}

// ---- prompt streams ---------------------------------------------------------

// Msg is one message received on a reply subject.
type Msg struct {
	Data   []byte
	Header nats.Header
	At     time.Time
}

// IsTerminator reports a zero-byte message with no headers (SAP §6.5).
func (m Msg) IsTerminator() bool { return len(m.Data) == 0 && len(m.Header) == 0 }

// ErrorCode returns the Nats-Service-Error-Code header value.
func (m Msg) ErrorCode() (string, bool) {
	v, ok := m.Header[agents.HeaderServiceErrorCode]
	if !ok || len(v) == 0 {
		return "", false
	}
	return v[0], true
}

// Stream is everything one request received on its reply subject.
type Stream struct {
	Name string
	Msgs []Msg
	// Term is the index of the terminator, or -1.
	Term int
	// After counts messages that arrived after the terminator, within the grace.
	After int
	// End is how the observation ended: terminated, timeout, no-responders,
	// canceled or error.
	End    string
	Err    error
	Sent   time.Time
	Header nats.Header
	// Over says which resource cap the stream hit (see Config.MaxStreamMessages
	// and Config.MaxStreamBytes); empty when it stayed within them. A stream
	// over a cap is closed, and every check that used it fails with this reason.
	Over string
	// NeedSeed is why the probe was not sent: the endpoint requires a signed
	// sender and the runner has no usable seed. The stream is empty then, and a
	// check reports configuration-missing, never a finding about the agent.
	NeedSeed string
}

// FirstLatency is the time from publishing to the first message.
func (s *Stream) FirstLatency() time.Duration {
	if len(s.Msgs) == 0 {
		return 0
	}
	return s.Msgs[0].At.Sub(s.Sent)
}

// ErrorMsg returns the first error-headered message.
func (s *Stream) ErrorMsg() (Msg, int, bool) {
	for i, m := range s.Msgs {
		if _, ok := m.ErrorCode(); ok {
			return m, i, true
		}
	}
	return Msg{}, -1, false
}

// Code returns the error code of the stream's error message, or "".
func (s *Stream) Code() string {
	if m, _, ok := s.ErrorMsg(); ok {
		c, _ := m.ErrorCode()
		return c
	}
	return ""
}

// Served reports a stream that ended with the terminator and no error message.
func (s *Stream) Served() bool {
	_, _, isErr := s.ErrorMsg()
	return s.Term >= 0 && !isErr
}

// Describe says in a few words how the stream went, for a detail line.
func (s *Stream) Describe() string {
	switch {
	case s.Over != "":
		return "the stream was closed: " + s.Over
	case s.End == "no-responders":
		return "no responders on the subject"
	case s.Code() != "":
		return "error " + s.Code()
	case s.Term < 0 && len(s.Msgs) == 0:
		return "no reply (" + s.End + ")"
	case s.Term < 0:
		return fmt.Sprintf("%d message(s) and no terminator (%s)", len(s.Msgs), s.End)
	}
	return fmt.Sprintf("%d message(s) then the terminator", len(s.Msgs)-1)
}

type collectOpts struct {
	timeout time.Duration
	// onMsg sees each message before the next is read; it may publish.
	onMsg func(m *nats.Msg)
}

// send is the one way a well-formed (positive) probe reaches the prompt
// endpoint: it carries the Agent-Sender headers probeAuth builds, so an endpoint
// that requires signed senders serves it. When it requires one and the runner
// has no seed, nothing is sent and the stream says so (Stream.NeedSeed). A new
// positive probe goes through send; TestProbesAreSentThroughProbeAuth fails the
// build of one that calls collect or the connection directly.
func (t *T) send(ctx context.Context, name string, payload []byte, o collectOpts) *Stream {
	hdr, need := t.probeAuth(ctx, payload)
	if need != "" {
		return &Stream{Name: name, Term: -1, End: "needs-seed: " + need, NeedSeed: need}
	}
	return t.collect(ctx, name, t.Prompt.Subject, payload, hdr, o)
}

// collect publishes one request to subject and records what arrives on its
// reply subject: until the terminator and the grace after it, the inactivity
// timeout, or a no-responders status.
func (t *T) collect(ctx context.Context, name, subject string, payload []byte, hdr nats.Header, o collectOpts) *Stream {
	if o.timeout <= 0 {
		o.timeout = t.cfg.StreamTimeout
	}
	s := &Stream{Name: name, Term: -1, Header: hdr}
	if t.dead && t.Prompt != nil && subject == t.Prompt.Subject {
		// The first well-formed prompt drew nothing at all; waiting out the
		// timeout for every later probe would only repeat that.
		s.End = "skipped: the prompt endpoint did not answer the first probe"
		return s
	}
	inbox := t.nc.NewInbox()
	ch := make(chan *nats.Msg, 256)
	sub, err := t.nc.ChanSubscribe(inbox, ch)
	if err != nil {
		s.End, s.Err = "error", err
		return s
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := t.nc.Flush(); err != nil {
		s.End, s.Err = "error", err
		return s
	}
	msg := &nats.Msg{Subject: subject, Reply: inbox, Data: payload, Header: hdr}
	s.Sent = time.Now()
	if err := t.nc.PublishMsg(msg); err != nil {
		s.End, s.Err = "error", err
		return s
	}
	timer := time.NewTimer(o.timeout)
	defer timer.Stop()
	var bytesSeen int
	// over notes a message against the caps; true means the stream is to be
	// closed now. The message that crosses a cap is counted but not kept.
	over := func(m *nats.Msg) bool {
		bytesSeen += len(m.Data) + headerSize(m.Header)
		switch {
		case len(s.Msgs) >= t.cfg.MaxStreamMessages:
			s.Over = fmt.Sprintf("more than %d messages (--max-stream-messages)", t.cfg.MaxStreamMessages)
		case bytesSeen > t.cfg.MaxStreamBytes:
			s.Over = fmt.Sprintf("more than %d bytes (--max-stream-bytes)", t.cfg.MaxStreamBytes)
		default:
			return false
		}
		s.End = "limit"
		t.noteOver(s)
		return true
	}
	for {
		select {
		case <-ctx.Done():
			s.End, s.Err = "canceled", ctx.Err()
			return s
		case <-timer.C:
			s.End = "timeout"
			return s
		case m := <-ch:
			if over(m) {
				return s
			}
			s.Msgs = append(s.Msgs, Msg{Data: m.Data, Header: m.Header, At: time.Now()})
			if len(m.Data) == 0 && m.Header.Get("Status") == "503" {
				s.End = "no-responders"
				return s
			}
			if o.onMsg != nil {
				o.onMsg(m)
			}
			if s.Msgs[len(s.Msgs)-1].IsTerminator() {
				s.Term = len(s.Msgs) - 1
				s.End = "terminated"
				grace := time.NewTimer(t.cfg.PostTerminatorGrace)
				defer grace.Stop()
				for {
					select {
					case <-ctx.Done():
						return s
					case <-grace.C:
						return s
					case m := <-ch:
						if over(m) {
							return s
						}
						s.Msgs = append(s.Msgs, Msg{Data: m.Data, Header: m.Header, At: time.Now()})
						s.After++
					}
				}
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(o.timeout)
		}
	}
}

func headerSize(h nats.Header) int {
	n := 0
	for k, vs := range h {
		for _, v := range vs {
			n += len(k) + len(v)
		}
	}
	return n
}

// noteOver records that the check now running used a stream (or heartbeat set)
// that hit a resource cap, so runTarget can fail the check with the reason.
func (t *T) noteOver(s *Stream) {
	for _, n := range t.curOver {
		if n == s.Name+": "+s.Over {
			return
		}
	}
	t.curOver = append(t.curOver, s.Name+": "+s.Over)
}

// reuse returns a cached stream for the check that is running now. A stream
// that hit a resource cap fails every check that judges it, not only the one
// that first sent it, so a cache hit notes the cap again.
func (t *T) reuse(s *Stream) *Stream {
	if s != nil && s.Over != "" {
		t.noteOver(s)
	}
	return s
}

// ---- the named prompt probes ------------------------------------------------

func (t *T) attachmentsOK() (v, known bool) {
	if t.Prompt == nil {
		return false, false
	}
	switch x := t.Prompt.Meta[agents.MetaAttachmentsOK].(type) {
	case bool:
		return x, true
	case string:
		switch x {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// probePayload returns the payload of a named probe.
func (t *T) probePayload(name string) ([]byte, bool) {
	p := t.cfg.ProbePrompt
	file := func(content string) string {
		return fmt.Sprintf(`{"prompt":%s,"attachments":[{"filename":"probe.txt","content":%s}]}`, jsonString(p), jsonString(content))
	}
	switch name {
	case "plain":
		return []byte(p), true
	case "json":
		return []byte(fmt.Sprintf(`{"prompt":%s,"x_sapconform_unknown":{"nested":[1,2]}}`, jsonString(p))), true
	case "wsjson":
		return []byte("\r\n\t " + fmt.Sprintf(`{"prompt":%s}`, jsonString(p))), true
	case "bracket":
		return []byte("[sapconform] " + p), true
	case "att":
		return []byte(fmt.Sprintf(`{"prompt":%s,"attachments":[{"filename":"probe.txt","content":%s,"x_unknown":true}]}`,
			jsonString(p), jsonString(base64.StdEncoding.EncodeToString([]byte("hello from sapconform"))))), true
	case "e-empty":
		return []byte{}, true
	case "e-badjson":
		return []byte(`{"prompt": `), true
	case "e-noprompt":
		return []byte(`{"not_a_prompt":"x"}`), true
	case "e-emptyprompt":
		return []byte(`{"prompt":""}`), true
	case "e-att":
		return []byte(file(base64.StdEncoding.EncodeToString([]byte("hello")))), true
	case "e-b64":
		return []byte(file("!!!not base64!!!")), true
	case "e-urlsafe":
		// "a-_b" is four URL-safe base64 characters and not standard base64.
		return []byte(file("a-_b")), true
	case "e-ws":
		return []byte(file("aGVs\nbG8=")), true
	case "e-nofilename":
		return []byte(fmt.Sprintf(`{"prompt":%s,"attachments":[{"content":"aGVsbG8="}]}`, jsonString(p))), true
	case "e-attshape":
		return []byte(fmt.Sprintf(`{"prompt":%s,"attachments":"not an array"}`, jsonString(p))), true
	}
	return nil, false
}

// run sends a named probe once and caches the stream.
func (t *T) run(ctx context.Context, name string) *Stream {
	if s, ok := t.streams[name]; ok {
		return t.reuse(s)
	}
	var s *Stream
	switch t.Prompt {
	case nil:
		s = &Stream{Name: name, Term: -1, End: "error", Err: errors.New("no prompt endpoint registered")}
	default:
		payload, ok := t.probePayload(name)
		if !ok {
			s = &Stream{Name: name, Term: -1, End: "error", Err: fmt.Errorf("unknown probe %q", name)}
		} else {
			s = t.send(ctx, name, payload, collectOpts{})
		}
	}
	if name == "plain" && len(s.Msgs) == 0 && (s.End == "timeout" || s.End == "no-responders") {
		t.dead = true
	}
	t.streams[name] = s
	return s
}

// ---- chunk parsing ----------------------------------------------------------

// Chunk is a decoded non-terminating message body.
type Chunk struct {
	Type string
	Data json.RawMessage
}

// parseChunk decodes a stream message body as SAP §6.2 requires.
func parseChunk(b []byte) (Chunk, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return Chunk{}, fmt.Errorf("not a JSON object: %w", err)
	}
	var c Chunk
	rt, ok := top["type"]
	if !ok {
		return c, errors.New("no type field")
	}
	if err := json.Unmarshal(rt, &c.Type); err != nil {
		return c, errors.New("type is not a string")
	}
	d, ok := top["data"]
	if !ok {
		return c, errors.New("no data field")
	}
	c.Data = d
	return c, nil
}

// decodeObject decodes a JSON object keeping numbers distinguishable.
func decodeObject(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("null, not an object")
	}
	if dec.More() {
		return nil, errors.New("trailing data")
	}
	return m, nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// useInstance points the per-instance view of t (Inst, its endpoints and the
// observations that depend on it) at in.
func (t *T) useInstance(in *Instance) {
	t.Inst = in
	t.Prompt = in.Endpoint(agents.EndpointPrompt)
	t.Status = in.Endpoint(agents.EndpointStatus)
	t.ping = nil
}

// saveInstance returns a func that restores the per-instance view of t.
func (t *T) saveInstance() func() {
	inst, prompt, status, ping := t.Inst, t.Prompt, t.Status, t.ping
	return func() { t.Inst, t.Prompt, t.Status, t.ping = inst, prompt, status, ping }
}

// instFor returns the registered instance with the given id, or the target's
// first instance when the id is not registered (SAP-8.3-04 reports that).
func (t *T) instFor(id string) *Instance {
	for _, in := range t.Insts {
		if in.ID == id {
			return in
		}
	}
	return t.Inst
}
