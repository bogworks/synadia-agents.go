// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package host_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/host"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
)

// TestEndpoints_ExtraEndpointsServedAndAdvertised: extra micro endpoints the
// SDK does not interpret register after prompt and status, are served by their
// own handler, and appear in the $SRV.INFO record verbatim (REQ-004, REQ-016).
func TestEndpoints_ExtraEndpointsServedAndAdvertised(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, echo,
		host.WithEndpoint(host.Endpoint{
			Name: "tools", Subject: "svc.tools.list", Metadata: map[string]string{"kind": "catalog"},
			Handler: micro.HandlerFunc(func(r micro.Request) { _ = r.Respond([]byte("tool list")) }),
		}),
		host.WithEndpoint(host.Endpoint{
			Name: "ctl", Subject: "svc.ctl", QueueGroup: "ops",
			Handler: micro.HandlerFunc(func(r micro.Request) { _ = r.Respond([]byte("ctl ok")) }),
		}))
	si := info(t, nc, h.InstanceID())
	var names []string
	for _, e := range si.Endpoints {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "prompt,status,tools,ctl" {
		t.Fatalf("endpoint order: %v", names)
	}
	tools, ctl := endpointOf(t, si, "tools"), endpointOf(t, si, "ctl")
	if tools.Subject != "svc.tools.list" || tools.QueueGroup != "q" || tools.Metadata["kind"] != "catalog" {
		t.Errorf("tools: %+v", tools)
	}
	if ctl.Subject != "svc.ctl" || ctl.QueueGroup != "ops" {
		t.Errorf("ctl: %+v", ctl)
	}
	for subj, want := range map[string]string{"svc.tools.list": "tool list", "svc.ctl": "ctl ok"} {
		m, err := nc.Request(subj, nil, 2*time.Second)
		if err != nil || string(m.Data) != want {
			t.Errorf("%s: %v %v", subj, m, err)
		}
	}
	// The protocol endpoints are unaffected, and the record is still an agent's.
	if _, err := agents.NewAgentInfo(si); err != nil {
		t.Error(err)
	}
	if got := prompt(t, nc, h, "x").texts(t); len(got) != 1 {
		t.Error("prompt broken by extra endpoints")
	}
}

// TestEndpoints_ReservedVerbsRefused: the four reserved verbs are never used
// for any other purpose (SAP §2).
//
// catalog: SAP-2-01
func TestEndpoints_ReservedVerbsRefused(t *testing.T) {
	_, nc := setup(t)
	for _, verb := range []string{"prompt", "hb", "status", "attachments"} {
		_, err := host.New(nc, subjectOf(t, testName), echo, host.WithEndpoint(host.Endpoint{
			Name: verb, Subject: "x." + verb, Handler: nopHandler()}))
		if !errors.Is(err, host.ErrConfig) {
			t.Errorf("verb %q: %v", verb, err)
		}
	}
}

// TestHost_ServiceEscapeHatch: Service exposes the micro service for endpoints
// only known at runtime.
func TestHost_ServiceEscapeHatch(t *testing.T) {
	_, nc := setup(t)
	h := newHost(t, nc, echo)
	err := h.Service().AddEndpoint("late", micro.HandlerFunc(func(r micro.Request) { _ = r.Respond([]byte("late")) }),
		micro.WithEndpointSubject("svc.late"))
	if err != nil {
		t.Fatal(err)
	}
	if m, err := nc.Request("svc.late", nil, 2*time.Second); err != nil || string(m.Data) != "late" {
		t.Fatalf("late endpoint: %v %v", m, err)
	}
	endpointOf(t, info(t, nc, h.InstanceID()), "late")
}

// tagInterceptor records its position around the handler.
type tagInterceptor struct {
	tag string
	log *[]string
	mu  *sync.Mutex
}

func (i tagInterceptor) AroundRequest(ctx context.Context, info *host.RequestInfo, next host.CallNext) error {
	i.mu.Lock()
	*i.log = append(*i.log, "in:"+i.tag)
	i.mu.Unlock()
	err := next(context.WithValue(ctx, ctxKey("tag"), i.tag))
	i.mu.Lock()
	*i.log = append(*i.log, "out:"+i.tag)
	i.mu.Unlock()
	return err
}

type ctxKey string

// TestInterceptors_OrderContextAndInfo: the first interceptor is the
// outermost, they see the decoded envelope with unknown fields, the headers and
// the subject, and a context passed to next reaches the handler.
func TestInterceptors_OrderContextAndInfo(t *testing.T) {
	_, nc := setup(t)
	var mu sync.Mutex
	var log []string
	var seen atomic.Value
	h := newHost(t, nc, func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		mu.Lock()
		log = append(log, "handler")
		mu.Unlock()
		return w.Send(ctx, ctx.Value(ctxKey("tag")).(string))
	}, host.WithInterceptors(
		tagInterceptor{"a", &log, &mu},
		host.InterceptorFunc(func(ctx context.Context, info *host.RequestInfo, next host.CallNext) error {
			seen.Store(info)
			return next(ctx)
		}),
		tagInterceptor{"b", &log, &mu},
	))
	hdr := nats.Header{}
	hdr.Set("X-Trace", "t1")
	s := rawPrompt(t, nc, h.Subject().Prompt(), []byte(`{"prompt":"p","extra":1}`), hdr, 5*time.Second)
	if got := s.texts(t); len(got) != 1 || got[0] != "b" {
		t.Fatalf("handler context: %v", got)
	}
	if got := strings.Join(log, ","); got != "in:a,in:b,handler,out:b,out:a" {
		t.Errorf("order: %s", got)
	}
	info := seen.Load().(*host.RequestInfo)
	if info.Envelope.Prompt != "p" || string(info.Envelope.Extra["extra"]) != "1" || info.Subject != h.Subject().Prompt() || info.Header.Get("X-Trace") != "t1" || info.Sender != nil {
		t.Errorf("info: %+v", info)
	}
}

// TestInterceptors_RefusalMisuseAndFailure: an interceptor that refuses before
// next leaves an error frame and the terminator and no ack; returning without
// next is 500; a second next is an error; a failure after the handler is
// logged and the reply stays.
func TestInterceptors_RefusalMisuseAndFailure(t *testing.T) {
	_, nc := setup(t)
	var handlerCalls atomic.Int32
	handler := func(ctx context.Context, _ *host.Request, w *host.Responder) error {
		handlerCalls.Add(1)
		return w.Send(ctx, "served")
	}
	var secondErr atomic.Value
	cases := []struct {
		name     string
		ic       host.InterceptorFunc
		wantCode int
		wantDesc string
		acks     int
		served   bool
	}{
		{"refuses before next", func(ctx context.Context, _ *host.RequestInfo, _ host.CallNext) error {
			return host.Reject(429, "rate limited here")
		}, 429, "rate limited here", 0, false},
		{"plain error before next", func(ctx context.Context, _ *host.RequestInfo, _ host.CallNext) error {
			return errors.New("secret detail")
		}, 500, "handler error", 0, false},
		{"returns without next", func(ctx context.Context, _ *host.RequestInfo, _ host.CallNext) error {
			return nil
		}, 500, "handler error", 0, false},
		{"panics", func(ctx context.Context, _ *host.RequestInfo, _ host.CallNext) error {
			panic("interceptor bug")
		}, 500, "handler error", 0, false},
		{"calls next twice", func(ctx context.Context, _ *host.RequestInfo, next host.CallNext) error {
			err := next(ctx)
			secondErr.Store(next(ctx))
			return err
		}, 0, "", 1, true},
		{"fails after the handler", func(ctx context.Context, _ *host.RequestInfo, next host.CallNext) error {
			if err := next(ctx); err != nil {
				return err
			}
			return errors.New("audit sink down")
		}, 0, "", 1, true},
		{"next error is passed back", func(ctx context.Context, _ *host.RequestInfo, next host.CallNext) error {
			return next(ctx)
		}, 0, "", 1, true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := handlerCalls.Load()
			h := newHostNamed(t, nc, "i"+string(rune('a'+i)), handler, host.WithInterceptors(tc.ic))
			s := prompt(t, nc, h, "x")
			se := s.errorFrame()
			if tc.served {
				if se != nil || len(s.texts(t)) != 1 || handlerCalls.Load() != before+1 {
					t.Fatalf("not served: %+v", se)
				}
			} else {
				if se == nil || se.Code != tc.wantCode || se.Description != tc.wantDesc {
					t.Fatalf("got %+v", se)
				}
				if handlerCalls.Load() != before {
					t.Error("handler ran")
				}
				if strings.Contains(se.Description, "secret") {
					t.Error("leaked")
				}
			}
			if s.acks() != tc.acks || !s.last().terminator() {
				t.Errorf("acks %d, terminator %v", s.acks(), s.last().terminator())
			}
		})
	}
	if err, _ := secondErr.Load().(error); !errors.Is(err, host.ErrNextCalledTwice) {
		t.Errorf("second next: %v", err)
	}
}

// TestInterceptors_HostStopReachesHandlerThroughOwnContext: an interceptor that
// passes a context of its own to next does not hide the host's cancellation
// from the handler.
func TestInterceptors_HostStopReachesHandlerThroughOwnContext(t *testing.T) {
	_, nc := setup(t)
	g := newGated()
	h := newHost(t, nc, g.handle, host.WithInterceptors(host.InterceptorFunc(
		func(_ context.Context, _ *host.RequestInfo, next host.CallNext) error {
			return next(context.Background())
		})))
	p := fire(t, nc, h, "x")
	g.waitStarted(t, 1)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if se := collect(t, p.sub, 5*time.Second).errorFrame(); se == nil || se.Description != "service stopping" {
		t.Fatalf("got %+v", se)
	}
}

// TestInterceptors_RunAfterAdmission: interceptors do not run for a request the
// host refused earlier (malformed envelope).
func TestInterceptors_RunAfterAdmission(t *testing.T) {
	_, nc := setup(t)
	var ran atomic.Int32
	h := newHost(t, nc, echo, host.WithInterceptors(host.InterceptorFunc(
		func(ctx context.Context, _ *host.RequestInfo, next host.CallNext) error {
			ran.Add(1)
			return next(ctx)
		})))
	rawPrompt(t, nc, h.Subject().Prompt(), nil, nil, 5*time.Second)
	if ran.Load() != 0 {
		t.Error("an interceptor saw a malformed request")
	}
	prompt(t, nc, h, "ok")
	if ran.Load() != 1 {
		t.Error("the interceptor did not run")
	}
}

// TestReplySubject_DefaultRefusesExtraEndpointSubjects: the default policy
// refuses a reply subject an extra endpoint's subject (wildcards included)
// could match, so a prompt cannot make the host publish into its own handler.
func TestReplySubject_DefaultRefusesExtraEndpointSubjects(t *testing.T) {
	_, nc := setup(t)
	var hits atomic.Int32
	count := micro.HandlerFunc(func(r micro.Request) { hits.Add(1); _ = r.Respond(nil) })
	h := newHost(t, nc, echo,
		host.WithEndpoint(host.Endpoint{Name: "ctl", Subject: "svc.ctl", Handler: count}),
		host.WithEndpoint(host.Endpoint{Name: "tools", Subject: "svc.tools.*", Handler: count}))
	before := h.Stats().RefusedReplies
	replies := []string{"svc.ctl", "svc.tools.list"}
	for _, reply := range replies {
		if err := nc.PublishMsg(&nats.Msg{Subject: h.Subject().Prompt(), Reply: reply, Data: []byte("x")}); err != nil {
			t.Fatal(err)
		}
	}
	settle(t, 3*time.Second, "refusals counted", func() bool {
		return h.Stats().RefusedReplies == before+uint64(len(replies))
	})
	time.Sleep(150 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Errorf("%d replies were delivered to extra endpoints", n)
	}
}
