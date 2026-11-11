// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package sapconform_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"

	"github.com/bogworks/synadia-agents.go/identity"
)

// A fake host written with raw NATS: the minimal conforming agent, plus switches
// that break it in one chosen way. It stands in for "a system under test" and
// uses nothing of the SDK's host package.

type fakeOpts struct {
	Agent, Owner, Name string
	Version            string // protocol_version, default "0.3"
	HBInterval         time.Duration

	// Trust is the declared min_sender_trust: "" declares nothing.
	Trust string
	// AdmitUnsigned makes a host that declares Trust "signed" serve every request.
	AdmitUnsigned bool
	// RefuseUsers are verified senders the host refuses with 403.
	RefuseUsers map[string]bool

	// SignStatus puts an Agent-Sender header on every status reply, which SAP 13.9
	// forbids.
	SignStatus bool

	// Faults.
	NoTerminator      bool
	AfterTerminator   bool
	NoAck             bool
	WrongStatusShape  bool
	NoQueueGroup      bool
	NoHeartbeat       bool
	AttachmentsOK     *bool // default true
	StatusStale       bool
	SessionAware      bool
	SlowAckDelay      time.Duration
	QueryPrompt       string // a prompt that makes the host ask a query
	QueryNoReplyError bool   // when the query goes unanswered, end with 503-free error 409
	// QueryReplySubject, when set, is the reply_subject the host puts in its
	// query chunk instead of its own inbox: a hostile host aiming the runner's
	// answer at a subject of its choosing.
	QueryReplySubject string
	// MalformedInfo adds a second responder on $SRV.INFO.agents that answers
	// with garbage next to the host's real record.
	MalformedInfo bool
	// NoOwnerMeta leaves metadata.owner out of the service record, an
	// instance-level fault.
	NoOwnerMeta bool
	// ServiceName is the service name in the INFO and PING records (default
	// "agents").
	ServiceName string
	// DupInfo answers every $SRV.INFO.agents request twice with the same record;
	// DupDirectInfo does so for the direct INFO subject; DupStatus adds a second
	// responder on the status subject, outside the queue group.
	DupInfo, DupDirectInfo, DupStatus bool

	// WorkAfterAck is slow work the host does after the ack and before its
	// first response chunk. The fake records when each happened.
	WorkAfterAck time.Duration
	// ContentBeforeAck sends a response chunk ahead of the ack.
	ContentBeforeAck bool
	// OversizeCode, when non-zero, refuses a request over the declared
	// max_payload (512KB) with that error code.
	OversizeCode int

	// UnknownChunk publishes a well-formed chunk of a type the spec does not
	// define; NumericTypeChunk publishes one whose type is not a string.
	UnknownChunk     bool
	NumericTypeChunk bool

	// Floods, for the resource-limit tests. FloodMsgs response chunks of
	// FloodMsgBytes bytes follow the ack and the stream never terminates.
	// HBFlood extra heartbeats naming fresh instance ids go out with each beat.
	FloodMsgs     int
	FloodMsgBytes int
	HBFlood       int
}

type fakeHost struct {
	t    *testing.T
	nc   *nats.Conn
	o    fakeOpts
	id   string
	gate *identity.Gate

	mu   sync.Mutex
	subs []*nats.Subscription
	stop chan struct{}

	gotAnswer atomic.Bool // a query answer reached the host
	ackAt     atomic.Int64
	workAt    atomic.Int64
}

func startServer(t *testing.T) *server.Server {
	t.Helper()
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats-server not ready")
	}
	t.Cleanup(s.Shutdown)
	return s
}

func connect(t *testing.T, s *server.Server) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func newFake(t *testing.T, nc *nats.Conn, o fakeOpts) *fakeHost {
	t.Helper()
	if o.Agent == "" {
		o.Agent, o.Owner, o.Name = "fake", "tester", "main"
	}
	if o.Version == "" {
		o.Version = "0.3"
	}
	if o.HBInterval == 0 {
		o.HBInterval = 300 * time.Millisecond
	}
	f := &fakeHost{t: t, nc: nc, o: o, id: nuid.Next(), stop: make(chan struct{})}
	if o.Trust != "" {
		trust := identity.TrustAny
		if o.Trust == "signed" && !o.AdmitUnsigned {
			trust = identity.TrustSigned
		}
		g, err := identity.NewGate(identity.GateOptions{MinSenderTrust: trust, Accept: func(_ context.Context, s *identity.Sender) (bool, error) {
			if s != nil && o.RefuseUsers[s.ID.User()] {
				return false, nil
			}
			return true, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		f.gate = g
	}
	f.start()
	t.Cleanup(f.Close)
	return f
}

func (f *fakeHost) promptSubject() string {
	return fmt.Sprintf("agents.prompt.%s.%s.%s", f.o.Agent, f.o.Owner, f.o.Name)
}
func (f *fakeHost) statusSubject() string {
	return fmt.Sprintf("agents.status.%s.%s.%s", f.o.Agent, f.o.Owner, f.o.Name)
}
func (f *fakeHost) hbSubject() string {
	return fmt.Sprintf("agents.hb.%s.%s.%s", f.o.Agent, f.o.Owner, f.o.Name)
}

func (f *fakeHost) attachmentsOK() bool { return f.o.AttachmentsOK == nil || *f.o.AttachmentsOK }

func (f *fakeHost) promptMeta() map[string]string {
	m := map[string]string{"max_payload": "512KB", "attachments_ok": strconv.FormatBool(f.attachmentsOK())}
	if f.o.Trust != "" {
		m["min_sender_trust"] = f.o.Trust
	}
	return m
}

func (f *fakeHost) serviceMeta() map[string]string {
	m := map[string]string{"agent": f.o.Agent, "owner": f.o.Owner, "protocol_version": f.o.Version}
	if f.o.SessionAware {
		m["session"] = "s1"
	}
	if f.o.NoOwnerMeta {
		delete(m, "owner")
	}
	return m
}

func (f *fakeHost) info() []byte {
	queue := "agents"
	if f.o.NoQueueGroup {
		queue = "q"
	}
	b, _ := json.Marshal(map[string]any{
		"type": "io.nats.micro.v1.info_response", "name": f.serviceName(), "id": f.id, "version": "1.2.3",
		"description": "a minimal conforming fake agent", "metadata": f.serviceMeta(),
		"endpoints": []any{
			map[string]any{"name": "prompt", "subject": f.promptSubject(), "queue_group": queue, "metadata": f.promptMeta()},
			map[string]any{"name": "status", "subject": f.statusSubject(), "queue_group": queue},
		},
	})
	return b
}

func (f *fakeHost) serviceName() string {
	if f.o.ServiceName != "" {
		return f.o.ServiceName
	}
	return "agents"
}

func (f *fakeHost) ping() []byte {
	b, _ := json.Marshal(map[string]any{"type": "io.nats.micro.v1.ping_response", "name": f.serviceName(), "id": f.id, "version": "1.2.3", "metadata": f.serviceMeta()})
	return b
}

func (f *fakeHost) beat() []byte { return f.beatFrom(f.id) }

func (f *fakeHost) beatFrom(id string) []byte {
	m := map[string]any{
		"agent": f.o.Agent, "owner": f.o.Owner, "instance_id": id,
		"ts": time.Now().UTC().Format(time.RFC3339), "interval_s": f.o.HBInterval.Seconds(),
		"protocol_version": f.o.Version,
		"endpoints":        map[string]any{"prompt": map[string]any{"subject": f.promptSubject(), "metadata": f.promptMeta()}},
	}
	if f.o.SessionAware {
		m["session"] = "s1"
	}
	b, _ := json.Marshal(m)
	return b
}

func (f *fakeHost) sub(subject, queue string, h nats.MsgHandler) {
	var s *nats.Subscription
	var err error
	if queue != "" {
		s, err = f.nc.QueueSubscribe(subject, queue, h)
	} else {
		s, err = f.nc.Subscribe(subject, h)
	}
	if err != nil {
		f.t.Fatal(err)
	}
	f.subs = append(f.subs, s)
}

func (f *fakeHost) start() {
	f.sub("$SRV.PING.agents", "", func(m *nats.Msg) { _ = m.Respond(f.ping()) })
	f.sub("$SRV.INFO.agents", "", func(m *nats.Msg) {
		_ = m.Respond(f.info())
		if f.o.DupInfo {
			_ = m.Respond(f.info())
		}
	})
	if f.o.MalformedInfo {
		f.sub("$SRV.INFO.agents", "", func(m *nats.Msg) { _ = m.Respond([]byte("<<not json>>")) })
	}
	f.sub("$SRV.INFO.agents."+f.id, "", func(m *nats.Msg) {
		_ = m.Respond(f.info())
		if f.o.DupDirectInfo {
			_ = m.Respond(f.info())
		}
	})
	f.sub(f.statusSubject(), "agents", f.status)
	if f.o.DupStatus {
		f.sub(f.statusSubject(), "", f.status)
	}
	f.sub(f.promptSubject(), "agents", func(m *nats.Msg) { go f.prompt(m) })
	if err := f.nc.Flush(); err != nil {
		f.t.Fatal(err)
	}
	if !f.o.NoHeartbeat {
		go func() {
			tick := time.NewTicker(f.o.HBInterval)
			defer tick.Stop()
			for {
				select {
				case <-f.stop:
					return
				case <-tick.C:
					_ = f.nc.Publish(f.hbSubject(), f.beat())
					for i := 0; i < f.o.HBFlood; i++ {
						_ = f.nc.Publish(f.hbSubject(), f.beatFrom(nuid.Next()))
					}
				}
			}
		}()
	}
}

// Close stops the fake.
func (f *fakeHost) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.stop:
		return
	default:
		close(f.stop)
	}
	for _, s := range f.subs {
		_ = s.Unsubscribe()
	}
}

func (f *fakeHost) status(m *nats.Msg) {
	b := f.beat()
	if f.o.WrongStatusShape {
		b, _ = json.Marshal(map[string]any{"agent": f.o.Agent, "status": "ok"})
	}
	if f.o.StatusStale {
		b = []byte(`{"agent":"` + f.o.Agent + `","owner":"` + f.o.Owner + `","instance_id":"` + f.id + `","ts":"2020-01-01T00:00:00Z","interval_s":1}`)
	}
	if f.o.SignStatus {
		h := nats.Header{}
		h.Set(identity.HeaderName, "forbidden")
		_ = m.RespondMsg(&nats.Msg{Data: b, Header: h})
		return
	}
	_ = m.Respond(b)
}

func (f *fakeHost) chunk(typ string, data any) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "data": data})
	return b
}

func (f *fakeHost) reject(m *nats.Msg, code int, desc string) {
	h := nats.Header{}
	h.Set("Nats-Service-Error-Code", strconv.Itoa(code))
	h.Set("Nats-Service-Error", desc)
	body, _ := json.Marshal(map[string]string{"error": "rejected", "message": desc})
	_ = f.nc.PublishMsg(&nats.Msg{Subject: m.Reply, Header: h, Data: body})
	_ = f.nc.Publish(m.Reply, nil)
}

// parse applies SAP 5.3 and 5.2.
func (f *fakeHost) parse(data []byte) (prompt string, err string) {
	if len(data) == 0 {
		return "", "empty payload"
	}
	trimmed := bytes.TrimLeft(data, "\t\n\r ")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return string(data), ""
	}
	var env struct {
		Prompt      *string `json:"prompt"`
		Attachments []struct {
			Filename *string `json:"filename"`
			Content  *string `json:"content"`
		} `json:"attachments"`
	}
	if json.Unmarshal(trimmed, &env) != nil || env.Prompt == nil {
		return "", "invalid envelope"
	}
	if len(env.Attachments) > 0 && !f.attachmentsOK() {
		return "", "attachments not accepted"
	}
	for _, a := range env.Attachments {
		if a.Filename == nil || a.Content == nil {
			return "", "attachment needs filename and content"
		}
		if _, e := base64.StdEncoding.Strict().DecodeString(*a.Content); e != nil || strings.ContainsAny(*a.Content, "\r\n \t") {
			return "", "invalid base64"
		}
	}
	if *env.Prompt == "" {
		return "", "empty prompt"
	}
	return *env.Prompt, ""
}

func (f *fakeHost) prompt(m *nats.Msg) {
	if f.gate != nil {
		adm := f.gate.AdmitPrompt(context.Background(), identity.MessageFromNATS(m))
		if adm.Err != nil {
			f.reject(m, adm.Err.Code, adm.Err.Description)
			return
		}
	}
	if f.o.OversizeCode != 0 && len(m.Data) > 512<<10 {
		f.reject(m, f.o.OversizeCode, "request too large")
		return
	}
	prompt, perr := f.parse(m.Data)
	if perr != "" {
		f.reject(m, 400, perr)
		return
	}
	pub := func(b []byte) { _ = f.nc.Publish(m.Reply, b) }
	if f.o.ContentBeforeAck {
		pub(f.chunk("response", "early"))
	}
	if !f.o.NoAck {
		if f.o.SlowAckDelay > 0 {
			time.Sleep(f.o.SlowAckDelay)
		}
		pub(f.chunk("status", "ack"))
		f.ackAt.Store(time.Now().UnixNano())
	}
	if f.o.UnknownChunk {
		pub([]byte(`{"type":"future-type","data":{"x":1}}`))
	}
	if f.o.NumericTypeChunk {
		pub([]byte(`{"type":5,"data":"x"}`))
	}
	if f.o.WorkAfterAck > 0 {
		f.workAt.Store(time.Now().UnixNano())
		time.Sleep(f.o.WorkAfterAck)
	}
	if f.o.FloodMsgs > 0 {
		body := f.chunk("response", strings.Repeat("x", f.o.FloodMsgBytes))
		for i := 0; i < f.o.FloodMsgs; i++ {
			select {
			case <-f.stop:
				return
			default:
			}
			pub(body)
		}
		return
	}
	if f.o.QueryPrompt != "" && prompt == f.o.QueryPrompt {
		f.askQuery(m, pub)
		return
	}
	pub(f.chunk("response", "echo: "+prompt))
	pub(f.chunk("response", map[string]any{"text": "done"}))
	if f.o.NoTerminator {
		return
	}
	pub(nil)
	if f.o.AfterTerminator {
		pub(f.chunk("response", "late"))
	}
}

func (f *fakeHost) askQuery(m *nats.Msg, pub func([]byte)) {
	reply := f.nc.NewInbox()
	sub, err := f.nc.SubscribeSync(reply)
	if err != nil {
		return
	}
	defer func() { _ = sub.Unsubscribe() }()
	replySubject := reply
	if f.o.QueryReplySubject != "" {
		replySubject = f.o.QueryReplySubject
	}
	pub(f.chunk("query", map[string]any{"id": "a8f1c2e4-9b63-4d7e-aaaa-112233445566", "reply_subject": replySubject, "prompt": "proceed?"}))
	// The fake waits half of fastConfig().QueryUnansweredWait for an answer, so
	// the runner always outwaits the host's own query timeout.
	ans, err := sub.NextMsg(time.Second)
	if err == nil {
		f.gotAnswer.Store(true)
	}
	if err != nil {
		if f.o.QueryNoReplyError {
			f.reject(m, 409, "query not answered")
			return
		}
		pub(f.chunk("response", "no answer, continuing"))
		pub(nil)
		return
	}
	pub(f.chunk("response", "answer: "+string(ans.Data)))
	pub(nil)
}
