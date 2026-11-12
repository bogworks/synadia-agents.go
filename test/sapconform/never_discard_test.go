// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package sapconform_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nuid"

	"github.com/bogworks/synadia-agents.go/conformance/runner"
)

const (
	idServiceName   = "SAP-3.1-01"
	idMetaAgent     = "SAP-3.2-01"
	idMetaOwner     = "SAP-3.2-02"
	idEndpointNames = "SAP-2-04"
	idMetaObject    = "SAP-3.1-04"
	idStatusSchema  = "SAP-8.7-07"
	idPing          = "SAP-4-01"
	idDirectInfo    = "SAP-4-02"
)

// rawResponder answers a discovery subject with fixed payloads, standing in
// for a hostile or broken responder that is not an agent at all.
func rawResponder(t *testing.T, nc *nats.Conn, subject string, payloads ...[]byte) {
	t.Helper()
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		for _, p := range payloads {
			_ = m.Respond(p)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
}

// strangerBeats publishes heartbeats on a subject nobody registered.
func strangerBeats(t *testing.T, nc *nats.Conn, payload func() []byte) {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				_ = nc.Publish("agents.hb.ghost.nobody.main", payload())
			}
		}
	}()
}

// TestNeverDiscardResponder is the reviewer's ruling as a table: next to one
// good host, a bad responder of each kind must make the strict verdict fail and
// the report must name it, either as a target whose checks fail (id is its
// instance id) or, when the reply cannot be attributed to an instance, in the
// run's incomplete note. The good host's own checks still pass.
func TestNeverDiscardResponder(t *testing.T) {
	t.Parallel()
	type badResponder struct {
		id         string // the bad instance's id, when it is a target
		incomplete string // text the run's incomplete note must carry, when unattributable
	}
	other := fakeOpts{Agent: "other", Owner: "tester", Name: "svc"}
	cases := []struct {
		name  string
		setup func(t *testing.T, s *server.Server) badResponder
		// failID is a check that fails for the bad instance (or, when the reply has
		// no instance, the check it fails on every target); detail is text the
		// failure names.
		failID string
		detail string
	}{
		{"wrong service name", func(t *testing.T, s *server.Server) badResponder {
			t.Helper()
			o := other
			o.ServiceName = "notagents"
			return badResponder{id: newFake(t, connect(t, s), o).id}
		}, idServiceName, "notagents"},
		{"malformed INFO JSON", func(t *testing.T, s *server.Server) badResponder {
			t.Helper()
			rawResponder(t, connect(t, s), "$SRV.INFO.agents", []byte("<<not json>>"))
			return badResponder{incomplete: "malformed"}
		}, idInfoAll, "malformed"},
		{"INFO record without an id", func(t *testing.T, s *server.Server) badResponder {
			t.Helper()
			rawResponder(t, connect(t, s), "$SRV.INFO.agents", []byte(`{"name":"agents"}`))
			return badResponder{incomplete: "malformed"}
		}, idInfoAll, "malformed"},
		{"missing required metadata", func(t *testing.T, s *server.Server) badResponder {
			t.Helper()
			o := other
			o.NoOwnerMeta = true
			return badResponder{id: newFake(t, connect(t, s), o).id}
		}, idMetaOwner, "owner"},
		{"duplicate instance id in INFO", func(t *testing.T, s *server.Server) badResponder {
			t.Helper()
			o := other
			o.DupInfo = true
			return badResponder{id: newFake(t, connect(t, s), o).id}
		}, idInfoAll, "2 times"},
		{"duplicate direct INFO responder", func(t *testing.T, s *server.Server) badResponder {
			t.Helper()
			o := other
			o.DupDirectInfo = true
			return badResponder{id: newFake(t, connect(t, s), o).id}
		}, idDirectInfo, "responders answered"},
		{"unparseable PING reply", func(t *testing.T, s *server.Server) badResponder {
			t.Helper()
			rawResponder(t, connect(t, s), "$SRV.PING.agents", []byte("<<not json>>"))
			return badResponder{incomplete: "$SRV.PING.agents"}
		}, idPing, "unparseable"},
		{"PING answer without INFO", func(t *testing.T, s *server.Server) badResponder {
			t.Helper()
			id := nuid.Next()
			b, _ := json.Marshal(map[string]any{"type": "io.nats.micro.v1.ping_response", "name": "agents", "id": id, "version": "1.0.0"})
			rawResponder(t, connect(t, s), "$SRV.PING.agents", b)
			return badResponder{id: id}
		}, idInfoAll, "absent"},
		{"INFO endpoints entry not an object", func(t *testing.T, s *server.Server) badResponder {
			t.Helper()
			id := nuid.Next()
			b, _ := json.Marshal(map[string]any{"type": "io.nats.micro.v1.info_response", "name": "agents", "id": id, "version": "1.0.0", "endpoints": []any{"prompt"}})
			rawResponder(t, connect(t, s), "$SRV.INFO.agents", b)
			rawResponder(t, connect(t, s), "$SRV.PING.agents", []byte(`{"name":"agents","id":"`+id+`"}`))
			return badResponder{id: id}
		}, idInfoAll, "not an object"},
		{"second status responder", func(t *testing.T, s *server.Server) badResponder {
			t.Helper()
			o := other
			o.DupStatus = true
			return badResponder{id: newFake(t, connect(t, s), o).id}
		}, idStatusSchema, "second responder"},
		{"heartbeats from an unknown instance", func(t *testing.T, s *server.Server) badResponder {
			t.Helper()
			strangerBeats(t, connect(t, s), func() []byte {
				b, _ := json.Marshal(map[string]any{"agent": "ghost", "owner": "nobody", "instance_id": "ghost-instance", "ts": time.Now().UTC().Format(time.RFC3339), "interval_s": 0.2})
				return b
			})
			return badResponder{incomplete: "ghost-instance"}
		}, "", ""},
		{"unparseable heartbeats", func(t *testing.T, s *server.Server) badResponder {
			t.Helper()
			strangerBeats(t, connect(t, s), func() []byte { return []byte("<<not json>>") })
			return badResponder{incomplete: "no parseable instance_id"}
		}, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := startServer(t)
			good := newFake(t, connect(t, s), fakeOpts{})
			bad := c.setup(t, s)

			r, err := runner.New(fastConfig(connect(t, s)))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			rep, err := r.Run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if rep.StrictOK() {
				t.Fatalf("a run with a bad responder (%s) passed strictly: %+v", c.name, rep.Verdict)
			}
			if bad.incomplete != "" && !strings.Contains(rep.Incomplete, bad.incomplete) {
				t.Errorf("incomplete = %q, want a note naming %q", rep.Incomplete, bad.incomplete)
			}
			var goodRes, badRes results
			for _, tr := range rep.Targets {
				m := results{}
				for _, e := range tr.Entries {
					m[e.ID] = e
				}
				for _, id := range tr.Target.InstanceIDs {
					switch id {
					case good.id:
						goodRes = m
					case bad.id:
						badRes = m
					}
				}
			}
			if goodRes == nil {
				t.Fatalf("the good host is not a target of the report")
			}
			// The good host's own checks still pass.
			goodRes.want(t, runner.Pass, idServiceName, idMetaAgent, idMetaOwner, idEndpointNames, idMetaObject, idStatusSchema)
			if bad.id == "" {
				// Unattributable: the reply fails the check on every target.
				if c.failID != "" {
					goodRes.want(t, runner.Fail, c.failID)
					if d := goodRes[c.failID].Detail; !strings.Contains(d, c.detail) {
						t.Errorf("%s detail %q does not mention %q", c.failID, d, c.detail)
					}
				}
				return
			}
			goodRes.want(t, runner.Pass, idInfoAll, idPing, idDirectInfo)
			if badRes == nil {
				t.Fatalf("the bad responder %s is not a target of the report", bad.id)
			}
			badRes.want(t, runner.Fail, c.failID)
			if d := badRes[c.failID].Detail; !strings.Contains(d, c.detail) {
				t.Errorf("%s detail %q does not mention %q", c.failID, d, c.detail)
			}
		})
	}
}
