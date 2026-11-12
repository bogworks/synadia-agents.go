// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/nats-io/nats.go"
)

func hostNames(as []*caller.Agent) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.Info().Name)
	}
	return out
}

// TestDiscover_RejectsRecordWhoseSubjectDisagreesWithMetadata asserts that a
// record whose default-form prompt subject names another owner than its
// metadata (an attempt to receive prompts meant for that owner) is dropped,
// counted, and never returned by Discover or Find. A subject whose agent token
// is not an abbreviation of metadata.agent is only a SHOULD of SAP §2: that
// record is kept (the record's subject stays authoritative, SAP §4.3).

func TestDiscover_RejectsRecordWhoseSubjectDisagreesWithMetadata(t *testing.T) {
	srv := startServer(t)
	hnc, cnc := connect(t, srv), connect(t, srv)
	newHost(t, hnc, func(h *host) { h.agent, h.owner, h.name = "claude-code", "alice", "good" }).start()
	newHost(t, hnc, func(h *host) { // abbreviation and custom subjects stay valid
		h.agent, h.owner, h.name = "claude-code", "alice", "abbr"
		h.promptSubject = "agents.prompt.cc.alice.abbr"
		h.statusSubject = "agents.status.cc.alice.abbr"
	}).start()
	newHost(t, hnc, func(h *host) {
		h.agent, h.owner, h.name = "claude-code", "alice", "custom"
		h.promptSubject, h.statusSubject = "private.prompt", "private.status"
	}).start()
	newHost(t, hnc, func(h *host) { // an unusual agent token is not grounds to drop a record
		h.agent, h.owner, h.name = "claude-code", "alice", "token"
		h.promptSubject = "agents.prompt.hermes.alice.token"
		h.statusSubject = "agents.status.hermes.alice.token"
	}).start()
	evil := newHost(t, hnc, func(h *host) {
		h.agent, h.owner, h.name = "claude-code", "alice", "evil"
		h.promptSubject = "agents.prompt.claude-code.mallory.evil" // metadata says alice
		h.statusSubject = "agents.status.claude-code.alice.evil"
	}).start()
	c := newClient(t, cnc)
	got, err := c.Discover(ctxT(t), caller.WithDiscoverTimeout(400*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	names := hostNames(got)
	if len(got) != 4 {
		t.Fatalf("discovered %v, want the four records with the right owner", names)
	}
	for _, a := range got {
		if a.InstanceID() == evil.id {
			t.Fatal("the inconsistent record was returned")
		}
	}
	if n := c.Stats().RejectedRecords; n != 1 {
		t.Fatalf("RejectedRecords %d, want 1", n)
	}
	if _, err := c.Find(ctxT(t), caller.Filter{Owner: "alice", Name: "evil"}, caller.WithDiscoverTimeout(300*time.Millisecond)); !errors.Is(err, caller.ErrAgentNotFound) {
		t.Fatalf("Find returned the inconsistent record: %v", err)
	}
}

// TestDiscover_RequireVerifiedIdentity asserts the opt-in verified discovery:
// only a record whose id_sig verifies over its own prompt subject, and whose
// signer is the expected identity, is returned; an unsigned record, one signed
// for a different subject, and one signed by another user are dropped and
// counted.
//
// catalog: SAP-13.10-04
func TestDiscover_RequireVerifiedIdentity(t *testing.T) {
	srv := startNkeyServer(t)
	hnc, cnc := connectAlice(t, srv), connectAlice(t, srv)
	signer, id := aliceSigner(t), aliceID(t)

	mk := func(name, signedSubject string) *host {
		h := newHost(t, hnc, func(h *host) { h.agent, h.owner, h.name = "real", "tester", name })
		reg, err := identity.RegistrationMetadata(signer, id, "real", "tester", signedSubject)
		if err != nil {
			t.Fatal(err)
		}
		h.extraMeta = reg
		return h.start()
	}
	good := mk("good", "agents.prompt.real.tester.good")
	mk("redirected", "agents.prompt.real.tester.somewhere-else") // id_sig binds another subject
	newHost(t, hnc, func(h *host) { h.agent, h.owner, h.name = "real", "tester", "plain" }).start()
	newHost(t, hnc, func(h *host) {
		h.agent, h.owner, h.name = "real", "tester", "claim"
		h.extraMeta = map[string]string{"user_nkey": alice.pub, "account": "$G", "id_sig": strings.Repeat("A", 86)}
	}).start()

	c := newClient(t, cnc)
	ctx := ctxT(t)
	win := caller.WithDiscoverTimeout(400 * time.Millisecond)

	all, err := c.Discover(ctx, win)
	if err != nil || len(all) != 4 {
		t.Fatalf("plain discovery: %d %v", len(all), err)
	}
	got, err := c.Discover(ctx, win, caller.WithRequireVerifiedIdentity(id))
	if err != nil || len(got) != 1 || got[0].InstanceID() != good.id {
		t.Fatalf("verified discovery: %v %v", hostNames(got), err)
	}
	if n := c.Stats().RejectedRecords; n != 3 {
		t.Fatalf("RejectedRecords %d, want 3", n)
	}
	other, _ := identity.NewAgentID("$G", "UDLPRVYPNSPIPYH4RVGKEHP2AAG4J2CN4GQ3IBTKC6ZDV6SR7ONG27D4")
	if got, err := c.Discover(ctx, win, caller.WithRequireVerifiedIdentity(other)); err != nil || len(got) != 0 {
		t.Fatalf("another identity matched: %v %v", hostNames(got), err)
	}
	if got, err := c.Discover(ctx, win, caller.WithRequireVerifiedIdentity("")); err != nil || len(got) != 1 {
		t.Fatalf("any verified identity: %v %v", hostNames(got), err)
	}
	pred := func(x identity.AgentID) bool { return x == id }
	if a, err := c.Find(ctx, caller.Filter{Owner: "tester"}, win, caller.WithRequireVerifiedIdentityFunc(pred)); err != nil || a.InstanceID() != good.id {
		t.Fatalf("Find with predicate: %v", err)
	}
	if _, err := c.Find(ctx, caller.Filter{Name: "plain"}, win, caller.WithRequireVerifiedIdentity(id)); !errors.Is(err, caller.ErrAgentNotFound) {
		t.Fatalf("Find returned an unsigned record: %v", err)
	}
}

// TestLookupInstance_RequiresTheRequestedID asserts a direct lookup accepts
// only the reply whose instance id is the one asked for: a forged reply for
// another instance is dropped and counted, the lookup keeps waiting, and when
// the right reply arrives it wins; with only forged replies it ends not found.

func TestLookupInstance_RequiresTheRequestedID(t *testing.T) {
	srv := startServer(t)
	hnc, cnc := connect(t, srv), connect(t, srv)
	real := newHost(t, hnc, func(h *host) { h.agent, h.owner, h.name = "real", "tester", "real" })
	decoy := newHost(t, hnc, func(h *host) { h.agent, h.owner, h.name = "decoy", "tester", "decoy" })
	reply := func(m *nats.Msg, h *host) { b, _ := json.Marshal(h.info()); _ = m.Respond(b) }

	// Forger: answers $SRV.INFO.agents.REAL first with the decoy's record, and
	// the real record only after a delay.
	var forged sync.WaitGroup
	forged.Add(1)
	sub, err := hnc.Subscribe("$SRV.INFO.agents."+real.id, func(m *nats.Msg) {
		reply(m, decoy)
		go func() {
			time.Sleep(150 * time.Millisecond)
			reply(m, real)
			forged.Done()
		}()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := hnc.Flush(); err != nil {
		t.Fatal(err)
	}
	c := newClient(t, cnc)
	a, err := c.LookupInstance(ctxT(t), real.id, 3*time.Second)
	if err != nil || a.InstanceID() != real.id {
		t.Fatalf("lookup: %v %v", a, err)
	}
	forged.Wait()
	if n := c.Stats().RejectedRecords; n != 1 {
		t.Fatalf("RejectedRecords %d, want 1", n)
	}
	_ = sub.Unsubscribe()

	// Only forged replies: not found after the timeout.
	sub2, err := hnc.Subscribe("$SRV.INFO.agents.GHOST", func(m *nats.Msg) { reply(m, decoy) })
	if err != nil {
		t.Fatal(err)
	}
	defer sub2.Unsubscribe()
	_ = hnc.Flush()
	start := time.Now()
	if _, err := c.LookupInstance(ctxT(t), "GHOST", 300*time.Millisecond); !errors.Is(err, caller.ErrAgentNotFound) {
		t.Fatalf("lookup of a forged reply: %v", err)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Fatal("the lookup gave up before its timeout")
	}
	if n := c.Stats().RejectedRecords; n != 2 {
		t.Fatalf("RejectedRecords %d, want 2", n)
	}
}

// TestStatus_DoesNotFeedASignedOnlyTracker asserts that a status reply, which
// is never signed (SAP §13.9), does not make an instance look alive in a
// tracker that requires signed heartbeats, that a tracker without that
// requirement still gets it marked as a status observation, and that a reply
// speaking for a different instance than the one asked is not fed at all.
//
// catalog: SAP-8.7-08
func TestStatus_DoesNotFeedASignedOnlyTracker(t *testing.T) {
	srv := startServer(t)
	hnc, cnc := connect(t, srv), connect(t, srv)
	h := newHost(t, hnc).start()
	strict := newClient(t, cnc, caller.WithTracker(caller.WithHeartbeatVerification(liveOpts(), true)))
	lax := newClient(t, connect(t, srv))

	ag, err := strict.LookupInstance(ctxT(t), h.id)
	if err != nil {
		t.Fatal(err)
	}
	if hb, err := ag.Status(ctxT(t)); err != nil || hb.InstanceID != h.id {
		t.Fatalf("status: %+v %v", hb, err)
	}
	if _, seen := strict.Tracker().Liveness(h.id); seen {
		t.Fatal("an unsigned status reply made the instance look alive in a signed-only tracker")
	}

	var beats []caller.Beat
	var mu sync.Mutex
	lax.Tracker().OnAnyHeartbeat(func(b caller.Beat) { mu.Lock(); beats = append(beats, b); mu.Unlock() })
	ag2, err := lax.LookupInstance(ctxT(t), h.id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ag2.Status(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(beats) != 1 || !beats[0].FromStatus || beats[0].Sender != nil {
		t.Fatalf("beats %+v", beats)
	}
	mu.Unlock()

	// A reply for another instance id is not fed.
	h.statusScript = func(h *host, m *nats.Msg) {
		hb := h.heartbeat()
		hb.InstanceID = "SOMEONEELSE"
		b, _ := agents.EncodeHeartbeat(hb)
		_ = m.Respond(b)
	}
	if _, err := ag2.Status(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if _, seen := lax.Tracker().Liveness("SOMEONEELSE"); seen {
		t.Fatal("a status reply for another instance was fed to the tracker")
	}
}

// TestQuery_ReplySubjectPolicy asserts that Query.Reply refuses, with a typed
// local error and nothing published, a reply subject in the system namespace or
// under agents.*, passes an inbox subject, honors a client-level and a
// per-prompt policy, and that a refusal does not use up the query's one reply.

func TestQuery_ReplySubjectPolicy(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		opts    func() ([]caller.Option, []caller.PromptOption)
		refused bool
	}{
		{"inbox passes", "_INBOX.agents.good.1", nil, false},
		{"system subject", "$SYS.REQ.SERVER.PING", nil, true},
		{"jetstream api", "$JS.API.STREAM.DELETE.X", nil, true},
		{"another agent's prompt", "agents.prompt.cc.alice.work", nil, true},
		{"another agent's heartbeat", "agents.hb.cc.alice.work", nil, true},
		{"client policy refuses an inbox", "_INBOX.agents.good.2", func() ([]caller.Option, []caller.PromptOption) {
			return []caller.Option{caller.WithQueryReplyPolicy(func(s string) error { return errors.New("no") })}, nil
		}, true},
		{"client policy allows what the default refuses", "agents.prompt.cc.alice.work", func() ([]caller.Option, []caller.PromptOption) {
			return []caller.Option{caller.WithQueryReplyPolicy(func(string) error { return nil })}, nil
		}, false},
		{"prompt policy overrides the client's", "agents.prompt.cc.alice.work", func() ([]caller.Option, []caller.PromptOption) {
			return nil, []caller.PromptOption{caller.WithPromptQueryReplyPolicy(func(string) error { return nil })}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var copts []caller.Option
			var popts []caller.PromptOption
			if tc.opts != nil {
				copts, popts = tc.opts()
			}
			f := setup(t, mod(func(h *host) {
				h.script = func(h *host, m *nats.Msg) {
					h.ack(m)
					h.chunk(m, "query", map[string]any{"id": "q", "reply_subject": tc.subject, "prompt": "?"})
					time.Sleep(300 * time.Millisecond)
					h.end(m)
				}
			}), copts...)
			sn := newSniffer(t, f.h.nc)
			st, err := f.a.PromptText(ctxT(t), "go", popts...)
			if err != nil {
				t.Fatal(err)
			}
			var q *caller.Query
			for ev, err := range st.Events() {
				if err != nil {
					t.Fatal(err)
				}
				if qq, ok := ev.(*caller.Query); ok {
					q = qq
					rerr := q.Reply(context.Background(), "yes")
					if tc.refused {
						var re *caller.QueryReplyRefusedError
						if !errors.Is(rerr, caller.ErrQueryReplyRefused) || !errors.Is(rerr, agents.ErrLocal) || !errors.As(rerr, &re) || re.Subject != tc.subject {
							t.Fatalf("reply: %v", rerr)
						}
					} else if rerr != nil {
						t.Fatalf("reply: %v", rerr)
					}
				}
			}
			if q == nil {
				t.Fatal("no query")
			}
			_ = f.h.nc.Flush()
			time.Sleep(50 * time.Millisecond)
			published := len(sn.on(tc.subject))
			if tc.refused && published != 0 {
				t.Fatalf("%d messages were published to the refused subject", published)
			}
			if !tc.refused && published != 1 {
				t.Fatalf("%d messages were published, want 1", published)
			}
			if tc.refused && errors.Is(q.Reply(context.Background(), "again"), caller.ErrQueryAlreadyReplied) {
				t.Fatal("a refusal used up the reply")
			}
		})
	}
}

// TestClose_RacesStartTracking asserts that Close racing StartTracking never
// leaves the heartbeat subscription running: once both return, the tracker is
// stopped and further StartTracking calls are refused.
func TestClose_RacesStartTracking(t *testing.T) {
	srv := startServer(t)
	nc := connect(t, srv)
	for i := 0; i < 400; i++ {
		c, err := caller.New(nc)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = c.StartTracking(context.Background()) }()
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i%40) * 10 * time.Microsecond)
			_ = c.Close(context.Background())
		}()
		wg.Wait()
		if c.Tracker().Started() {
			t.Fatalf("iteration %d: the tracker is running after Close", i)
		}
		if err := c.StartTracking(context.Background()); !errors.Is(err, caller.ErrClosed) {
			t.Fatalf("StartTracking after Close: %v", err)
		}
	}
	if n := nc.NumSubscriptions(); n != 0 {
		t.Fatalf("%d subscriptions leaked", n)
	}
}
