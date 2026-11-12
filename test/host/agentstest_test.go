// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host_test

import (
	"context"
	"errors"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/agentstest"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/bogworks/synadia-agents.go/identity"
	"github.com/bogworks/synadia-agents.go/internal/hosttest"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
)

func startRef(t *testing.T, nc *nats.Conn, opts ...agentstest.ReferenceOption) *agentstest.ReferenceAgent {
	t.Helper()
	ra, err := agentstest.NewReferenceAgent(nc, subjectOf(t, testName), opts...)
	if err != nil {
		t.Fatal(err)
	}
	if err := ra.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ra.Stop(context.Background()) })
	return ra
}

// TestReferenceAgent_EchoesThroughTheFullPipeline: the default reference agent
// is a compliant counterparty: ack, an echoed chunk, the terminator, a status
// reply and a one-second heartbeat.
func TestReferenceAgent_EchoesThroughTheFullPipeline(t *testing.T) {
	_, nc := setup(t)
	hb := subscribeHeartbeats(t, nc)
	ra := startRef(t, nc)
	if ra.InstanceID() == "" || ra.PromptSubject() != "agents.prompt."+testAgent+"."+testOwner+"."+testName ||
		ra.StatusSubject() != "agents.status."+testAgent+"."+testOwner+"."+testName ||
		ra.HeartbeatSubject() != "agents.hb."+testAgent+"."+testOwner+"."+testName {
		t.Errorf("accessors: %q %q %q", ra.InstanceID(), ra.PromptSubject(), ra.HeartbeatSubject())
	}
	s := rawPrompt(t, nc, ra.PromptSubject(), []byte("ping"), nil, 5*time.Second)
	if got := s.texts(t); len(got) != 1 || got[0] != "ping" || s.acks() != 1 || !s.last().terminator() {
		t.Fatalf("stream: %v acks=%d", got, s.acks())
	}
	m, err := nc.Request(ra.StatusSubject(), nil, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agents.DecodeHeartbeat(m.Data); err != nil {
		t.Error(err)
	}
	var beats []time.Time
	for len(beats) < 2 {
		msg, err := hb.NextMsg(3 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		d, err := agents.DecodeHeartbeat(msg.Data)
		if err != nil || d.IntervalS != 1 {
			t.Fatalf("heartbeat %+v %v", d, err)
		}
		beats = append(beats, time.Now())
	}
	if gap := beats[1].Sub(beats[0]); gap < 800*time.Millisecond || gap > 1500*time.Millisecond {
		t.Errorf("heartbeat gap %v", gap)
	}
	if ra.SenderGate() == nil || ra.MinSenderTrust() != identity.TrustAny || ra.Host() == nil {
		t.Error("accessors")
	}
}

// TestReferenceAgent_Options: a custom handler runs through the pipeline, and
// WithHost options override the reference agent's defaults.
func TestReferenceAgent_Options(t *testing.T) {
	_, nc := setup(t)
	ra := startRef(t, nc,
		agentstest.WithHandler(func(ctx context.Context, _ *host.Request, w *host.Responder) error {
			return host.Reject(409, "custom")
		}),
		agentstest.WithHost(host.WithMinSenderTrust(identity.TrustSigned), host.WithHeartbeatInterval(2*time.Second)))
	s := rawPrompt(t, nc, ra.PromptSubject(), []byte("x"), nil, 5*time.Second)
	if se := s.errorFrame(); se == nil || se.Code != 401 {
		t.Fatalf("an unsigned request to a signed reference agent: %+v", se)
	}
	m, err := nc.Request(ra.StatusSubject(), nil, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := agents.DecodeHeartbeat(m.Data); d.IntervalS != 2 {
		t.Errorf("interval %v", d.IntervalS)
	}
	if got := endpointOf(t, info(t, nc, ra.InstanceID()), "prompt").Metadata["min_sender_trust"]; got != "signed" {
		t.Errorf("trust %q", got)
	}
	if d := info(t, nc, ra.InstanceID()).Description; d != testAgent+" reference agent" {
		t.Errorf("description %q", d)
	}
}

// TestReferenceAgent_RawHandlerOwnsTheStream: with a raw handler the host adds
// no ack, no terminator and no error mapping, so a test can produce a stream a
// compliant host never would; an error is answered 500 and the terminator.
func TestReferenceAgent_RawHandlerOwnsTheStream(t *testing.T) {
	_, nc := setup(t)
	mode := make(chan string, 1)
	senders := make(chan *identity.Sender, 1)
	ra := startRef(t, nc, agentstest.WithRawHandler(func(req micro.Request, sender *identity.Sender) error {
		senders <- sender
		switch <-mode {
		case "silent":
			return nil
		case "fail":
			return errors.New("fixture failure")
		}
		_ = req.Respond([]byte(`{"type":"response","data":"raw"}`))
		return req.Respond(nil)
	}))

	// A handler that sends nothing leaves the caller with silence: no ack, no terminator.
	sub, reply := openStream(t, nc)
	mode <- "silent"
	if err := nc.PublishMsg(&nats.Msg{Subject: ra.PromptSubject(), Reply: reply, Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if s := <-senders; s != nil {
		t.Errorf("sender %+v", s)
	}
	if m, err := sub.NextMsg(300 * time.Millisecond); err == nil {
		t.Fatalf("the host sent %q for a raw handler", m.Data)
	}

	mode <- "raw"
	s := rawPrompt(t, nc, ra.PromptSubject(), []byte("x"), nil, 5*time.Second)
	<-senders
	if len(s.Frames) != 2 || s.acks() != 0 {
		t.Fatalf("raw stream: %d frames, %d acks", len(s.Frames), s.acks())
	}

	mode <- "fail"
	s = rawPrompt(t, nc, ra.PromptSubject(), []byte("x"), nil, 5*time.Second)
	<-senders
	if se := s.errorFrame(); se == nil || se.Code != 500 || se.Description != "handler error" || !s.last().terminator() {
		t.Fatalf("failing raw handler: %+v", se)
	}
}

// TestReferenceAgent_RawHandlerStillGated: sender admission applies before the
// raw handler.
func TestReferenceAgent_RawHandlerStillGated(t *testing.T) {
	_, nc := setup(t)
	called := make(chan struct{}, 1)
	ra := startRef(t, nc,
		agentstest.WithRawHandler(func(micro.Request, *identity.Sender) error { called <- struct{}{}; return nil }),
		agentstest.WithHost(host.WithMinSenderTrust(identity.TrustSigned)))
	s := rawPrompt(t, nc, ra.PromptSubject(), []byte("x"), nil, 5*time.Second)
	if se := s.errorFrame(); se == nil || se.Code != 401 {
		t.Fatalf("got %+v", se)
	}
	select {
	case <-called:
		t.Error("the raw handler ran for a refused sender")
	default:
	}
}

// TestHost_NilHandlerNeedsRawHandler: a host needs one of the two.
func TestHost_NilHandlerNeedsRawHandler(t *testing.T) {
	_, nc := setup(t)
	if _, err := host.New(nc, subjectOf(t, testName), nil); err == nil {
		t.Error("nil handler accepted")
	}
	if _, err := host.New(nc, subjectOf(t, testName), nil, host.WithRawPromptHandler(hosttest.Raw{})); err == nil {
		t.Error("nil raw handler accepted")
	}
	if _, err := host.New(nc, subjectOf(t, testName), nil,
		host.WithRawPromptHandler(hosttest.NewRaw(func(context.Context, micro.Request, *identity.Sender) error { return nil }))); err != nil {
		t.Errorf("raw handler refused: %v", err)
	}
}
