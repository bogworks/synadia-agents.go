// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package caller_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/caller"
	"github.com/nats-io/nats.go"
)

// refused asserts that a prompt failed locally with want, matched by errors.Is,
// as a local refusal, and that nothing reached the host.
func refused(t *testing.T, f *fixture, env agents.Envelope, want error, opts ...caller.PromptOption) error {
	t.Helper()
	st, err := f.a.Prompt(ctxT(t), env, opts...)
	if err == nil {
		_ = st.Close()
		t.Fatalf("prompt was accepted, want %v", want)
	}
	if want != nil && !errors.Is(err, want) {
		t.Fatalf("error %v does not match %v", err, want)
	}
	if !errors.Is(err, agents.ErrLocal) {
		t.Fatalf("a local refusal must match agents.ErrLocal: %v", err)
	}
	var se *agents.ServiceError
	if errors.As(err, &se) {
		t.Fatalf("a local refusal is not a remote error: %v", err)
	}
	f.h.barrier(t, f.cnc)
	if n := f.h.promptsSeen(); n != 0 {
		t.Fatalf("%d prompts reached the host despite the refusal", n)
	}
	return err
}

// TestPrompt_LocalRefusalsPublishNothing asserts the caller's local MUSTs: an
// empty prompt, attachments to an endpoint that does not accept them, an
// attachment with URL-safe or whitespace-bearing base64, and a payload over
// max_payload all fail before anything is published, with typed errors a
// program can tell from remote ones.
//
// catalog: SAP-5.1-03
// catalog: SAP-5.2-03
// catalog: SAP-5.2-04
// catalog: SAP-5.4-02
// catalog: SAP-5.4-03
// catalog: SAP-2.1-05
func TestPrompt_LocalRefusalsPublishNothing(t *testing.T) {
	t.Run("empty prompt", func(t *testing.T) {
		f := setup(t, nil)
		refused(t, f, agents.Envelope{}, agents.ErrPromptEmpty)
	})
	t.Run("attachments the endpoint does not accept", func(t *testing.T) {
		f := setup(t, mod(func(h *host) { h.attachmentsOK = false }))
		refused(t, f, agents.Envelope{Prompt: "x", Attachments: []agents.Attachment{agents.NewAttachment("a", []byte("b"))}}, agents.ErrAttachmentsNotSupported)
	})
	t.Run("url-safe base64", func(t *testing.T) {
		f := setup(t, nil)
		refused(t, f, agents.Envelope{Prompt: "x", Attachments: []agents.Attachment{{Filename: "a", Content: "-_-_"}}}, agents.ErrInvalidAttachment)
	})
	t.Run("whitespace in base64", func(t *testing.T) {
		f := setup(t, nil)
		refused(t, f, agents.Envelope{Prompt: "x", Attachments: []agents.Attachment{{Filename: "a", Content: "aGk=\n"}}}, agents.ErrInvalidAttachment)
	})
	t.Run("unpadded base64", func(t *testing.T) {
		f := setup(t, nil)
		refused(t, f, agents.Envelope{Prompt: "x", Attachments: []agents.Attachment{{Filename: "a", Content: "aGk"}}}, agents.ErrInvalidAttachment)
	})
	t.Run("over max_payload", func(t *testing.T) {
		f := setup(t, mod(func(h *host) { h.maxPayload = "2KB" }))
		err := refused(t, f, agents.Envelope{Prompt: strings.Repeat("x", 3000)}, agents.ErrPayloadTooLarge)
		var pl *agents.PayloadTooLargeError
		if !errors.As(err, &pl) || pl.Limit != 2048 || pl.Connection {
			t.Fatalf("%+v", pl)
		}
		// The limit is on the final encoded size, attachments included.
		refused(t, f, agents.Envelope{Prompt: "x", Attachments: []agents.Attachment{agents.NewAttachment("a", make([]byte, 2000))}}, agents.ErrPayloadTooLarge)
	})
	t.Run("capability metadata missing or malformed", func(t *testing.T) {
		f := setup(t, mod(func(h *host) { h.maxPayload = "lots" }))
		refused(t, f, agents.Envelope{Prompt: "x"}, agents.ErrInvalidCapability)
	})
	t.Run("connection limit", func(t *testing.T) {
		f := setup(t, mod(func(h *host) { h.maxPayload = "8GB" }))
		err := refused(t, f, agents.Envelope{Prompt: strings.Repeat("x", int(f.cnc.MaxPayload())+10)}, agents.ErrPayloadTooLarge)
		var pl *agents.PayloadTooLargeError
		if !errors.As(err, &pl) || !pl.Connection {
			t.Fatalf("%+v", pl)
		}
	})
}

// TestPrompt_AttachmentsNotAcceptedStillTakesText asserts that when
// attachments_ok is false, plain text and a JSON envelope without attachments
// remain valid requests.
//
// catalog: SAP-2.1-06
func TestPrompt_AttachmentsNotAcceptedStillTakesText(t *testing.T) {
	f := setup(t, mod(func(h *host) { h.attachmentsOK = false }))
	for _, env := range []agents.Envelope{{Prompt: "plain"}, {Prompt: "with extra", Extra: map[string]json.RawMessage{"k": json.RawMessage(`1`)}}} {
		st, err := f.a.Prompt(ctxT(t), env)
		if err != nil {
			t.Fatal(err)
		}
		evs, err := collect(t, st)
		if err != nil || !strings.Contains(texts(evs), env.Prompt) {
			t.Fatalf("%q %v", texts(evs), err)
		}
	}
}

// TestPrompt_EnvelopeOnTheWire asserts what the host receives: a JSON envelope
// whose attachments are {filename, content} with standard padded base64, and
// additional top-level fields from an interceptor, while the envelope's own
// fields cannot be set as extras.
//
// catalog: SAP-5.1-05
// catalog: SAP-5.1-06
// catalog: SAP-5.2-01
// catalog: SAP-5.2-02
func TestPrompt_EnvelopeOnTheWire(t *testing.T) {
	data := []byte{0xfb, 0xff, 0xfe, 0x01} // standard and URL-safe alphabets differ here
	f := setup(t, nil, caller.WithInterceptors(caller.InterceptorFunc(func(ctx context.Context, p *caller.PromptInfo) (*caller.Extras, error) {
		return &caller.Extras{Fields: map[string]any{"trace_id": "abc-123", "n": 7}}, nil
	})))
	st, err := f.a.Prompt(ctxT(t), agents.Envelope{Prompt: "see file", Attachments: []agents.Attachment{agents.NewAttachment("data.bin", data)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collect(t, st); err != nil {
		t.Fatal(err)
	}
	m := f.h.nextPrompt(t)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(m.Data, &raw); err != nil {
		t.Fatalf("%q: %v", m.Data, err)
	}
	var atts []map[string]string
	if err := json.Unmarshal(raw["attachments"], &atts); err != nil || len(atts) != 1 {
		t.Fatalf("attachments %s: %v", raw["attachments"], err)
	}
	if atts[0]["filename"] != "data.bin" || atts[0]["content"] != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("attachment %v", atts[0])
	}
	if strings.ContainsAny(atts[0]["content"], "-_ \r\n") {
		t.Fatalf("content %q is not standard-alphabet base64", atts[0]["content"])
	}
	if string(raw["trace_id"]) != `"abc-123"` || string(raw["n"]) != `7` || string(raw["prompt"]) != `"see file"` {
		t.Fatalf("envelope %s", m.Data)
	}

	// The envelope's own fields are not extras.
	g := setup(t, nil, caller.WithInterceptors(caller.InterceptorFunc(func(ctx context.Context, p *caller.PromptInfo) (*caller.Extras, error) {
		return &caller.Extras{Fields: map[string]any{"prompt": "hijack"}}, nil
	})))
	if _, err := g.a.PromptText(ctxT(t), "x"); !errors.Is(err, caller.ErrReservedField) || !errors.Is(err, agents.ErrLocal) {
		t.Fatalf("error %v", err)
	}
	g.h.barrier(t, g.cnc)
	if g.h.promptsSeen() != 0 {
		t.Fatal("a prompt with a refused extra was published")
	}
}

// TestPrompt_VersionPolicy asserts the protocol version rules: only MAJOR.MINOR
// is compared, the default pins the exact minor while the protocol is 0.x, and
// the program can relax it to the same major.
//
// catalog: SAP-11.1-02
// catalog: SAP-11.2-01
// catalog: SAP-11.2-03
// catalog: SAP-12-02
func TestPrompt_VersionPolicy(t *testing.T) {
	cases := []struct {
		name     string
		version  string
		policy   agents.VersionPolicy
		accepted bool
	}{
		{"exact match, default", "0.3", agents.VersionDefault, true},
		{"patch and qualifier are ignored", "0.3.7-rc1", agents.VersionDefault, true},
		{"other minor refused by default", "0.4", agents.VersionDefault, false},
		{"older minor refused by default", "0.2", agents.VersionDefault, false},
		{"other major refused", "1.3", agents.VersionDefault, false},
		{"other minor accepted for the same major", "0.4", agents.VersionSameMajor, true},
		{"other major refused even then", "1.0", agents.VersionSameMajor, false},
		{"exact policy refuses other minors", "0.4", agents.VersionExact, false},
		{"unparseable version refused", "banana", agents.VersionSameMajor, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, mod(func(h *host) { h.protocol = tc.version }), caller.WithVersionPolicy(tc.policy))
			if tc.accepted {
				st, err := f.a.PromptText(ctxT(t), "hi")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := collect(t, st); err != nil {
					t.Fatal(err)
				}
				return
			}
			var ve *agents.VersionError
			err := refused(t, f, agents.Envelope{Prompt: "hi"}, agents.ErrVersionMismatch)
			if !errors.As(err, &ve) || ve.Agent != tc.version {
				t.Fatalf("%v", err)
			}
		})
	}
	t.Run("compatible only drops refused agents from discovery", func(t *testing.T) {
		srv := startServer(t)
		hnc, cnc := connect(t, srv), connect(t, srv)
		newHost(t, hnc).start()
		newHost(t, hnc, func(h *host) { h.protocol = "0.9" }).start()
		c := newClient(t, cnc)
		all, err := c.Discover(ctxT(t), caller.WithDiscoverTimeout(300*time.Millisecond))
		if err != nil || len(all) != 2 {
			t.Fatal(len(all), err)
		}
		ok, err := c.Discover(ctxT(t), caller.WithDiscoverTimeout(300*time.Millisecond), caller.WithCompatibleOnly())
		if err != nil || len(ok) != 1 || ok[0].CheckVersion() != nil {
			t.Fatal(len(ok), err)
		}
	})
}

// TestPrompt_RecordBeatsHeartbeat asserts that when the caller has both a
// discovery record and a heartbeat that declares different capabilities, it
// enforces the record.
//
// catalog: SAP-8.3-10
func TestPrompt_RecordBeatsHeartbeat(t *testing.T) {
	f := setup(t, mod(func(h *host) { h.maxPayload = "1KB" }))
	// A heartbeat that declares a far larger max_payload.
	hb := f.h.heartbeat()
	hb.Endpoints = map[string]agents.HeartbeatEndpoint{"prompt": {Subject: f.h.promptSubject, Metadata: map[string]string{"max_payload": "8MB", "attachments_ok": "true"}}}
	b, err := agents.EncodeHeartbeat(hb)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.c.StartTracking(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	got := make(chan struct{}, 1)
	cancel := f.c.Tracker().OnAnyHeartbeat(func(caller.Beat) { got <- struct{}{} })
	defer cancel()
	_ = f.h.nc.Publish("agents.hb."+f.h.agent+"."+f.h.owner+"."+f.h.name, b)
	<-got
	refused(t, f, agents.Envelope{Prompt: strings.Repeat("x", 4096)}, agents.ErrPayloadTooLarge)
}

// TestInterceptors_AndHeaderHook asserts the extension points of REQ-016 on the
// public API: the two interceptor phases run in order and phase two sees what
// phase one returned, the outgoing-header hook adds application headers, and
// none of them can set a header SAP defines or relies on: setting one fails the
// prompt before anything is published.
func TestInterceptors_AndHeaderHook(t *testing.T) {
	type rec struct{ phases []string }
	r := &rec{}
	ic := &phased{r: &r.phases}
	f := setup(t, nil,
		caller.WithInterceptors(ic),
		caller.WithRequestHeaders(func(ctx context.Context, p *caller.PromptInfo) (map[string]string, error) {
			return map[string]string{"X-App": "hook-" + p.Prompt}, nil
		}))
	st, err := f.a.PromptText(ctxT(t), "go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collect(t, st); err != nil {
		t.Fatal(err)
	}
	m := f.h.nextPrompt(t)
	if m.Header.Get("X-App") != "hook-go" || m.Header.Get("X-Interceptor") != "yes" {
		t.Fatalf("headers %v", m.Header)
	}
	if strings.Join(r.phases, ",") != "before,publish:state-1" {
		t.Fatalf("phases %v", r.phases)
	}

	t.Run("reserved headers are refused case-insensitively", func(t *testing.T) {
		for _, name := range []string{"Agent-Sender", "agent-sender", "Nats-Service-Error", "Nats-Service-Error-Code", "NATS-REQUEST-INFO", "Nats-Msg-Id"} {
			if !caller.ReservedHeader(name) {
				t.Fatalf("%s is not reserved", name)
			}
			for _, via := range []string{"hook", "interceptor"} {
				var opt caller.Option
				if via == "hook" {
					opt = caller.WithRequestHeaders(func(context.Context, *caller.PromptInfo) (map[string]string, error) {
						return map[string]string{name: "x"}, nil
					})
				} else {
					opt = caller.WithInterceptors(caller.InterceptorFunc(func(context.Context, *caller.PromptInfo) (*caller.Extras, error) {
						return &caller.Extras{Headers: map[string]string{name: "x"}}, nil
					}))
				}
				g := setup(t, nil, opt)
				_, err := g.a.PromptText(ctxT(t), "go")
				if !errors.Is(err, caller.ErrReservedHeader) || !errors.Is(err, agents.ErrLocal) {
					t.Fatalf("%s via %s: %v", name, via, err)
				}
				g.h.barrier(t, g.cnc)
				if g.h.promptsSeen() != 0 {
					t.Fatalf("%s via %s: the prompt was published", name, via)
				}
			}
		}
	})
	t.Run("a failing or panicking phase one stops the prompt; a panicking phase two does not", func(t *testing.T) {
		g := setup(t, nil, caller.WithInterceptors(caller.InterceptorFunc(func(context.Context, *caller.PromptInfo) (*caller.Extras, error) {
			return nil, errors.New("no")
		})))
		if _, err := g.a.PromptText(ctxT(t), "go"); err == nil {
			t.Fatal("a failing interceptor did not stop the prompt")
		}
		g.h.barrier(t, g.cnc)
		if g.h.promptsSeen() != 0 {
			t.Fatal("published")
		}
		g = setup(t, nil, caller.WithInterceptors(caller.InterceptorFunc(func(context.Context, *caller.PromptInfo) (*caller.Extras, error) {
			panic("boom")
		})))
		if _, err := g.a.PromptText(ctxT(t), "go"); err == nil {
			t.Fatal("a panicking interceptor did not stop the prompt")
		}
		g.h.barrier(t, g.cnc)
		if g.h.promptsSeen() != 0 {
			t.Fatal("published")
		}
		g = setup(t, nil, caller.WithInterceptors(&panicsInPhaseTwo{}))
		st, err := g.a.PromptText(ctxT(t), "go")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := collect(t, st); err != nil || g.h.promptsSeen() != 1 {
			t.Fatalf("error %v", err)
		}
	})
	t.Run("a header with a line break is refused", func(t *testing.T) {
		g := setup(t, nil, caller.WithRequestHeaders(func(context.Context, *caller.PromptInfo) (map[string]string, error) {
			return map[string]string{"X-App": "a\r\nNats-Msg-Id: 1"}, nil
		}))
		if _, err := g.a.PromptText(ctxT(t), "go"); !errors.Is(err, caller.ErrInvalidHeader) {
			t.Fatal(err)
		}
	})
}

type phased struct{ r *[]string }

func (p *phased) BeforePrompt(ctx context.Context, i *caller.PromptInfo) (*caller.Extras, error) {
	*p.r = append(*p.r, "before")
	return &caller.Extras{Headers: map[string]string{"X-Interceptor": "yes"}, State: "state-1"}, nil
}

func (p *phased) BeforePublish(ctx context.Context, i *caller.PromptInfo, state any) {
	*p.r = append(*p.r, "publish:"+state.(string))
}

type panicsInPhaseTwo struct{}

func (panicsInPhaseTwo) BeforePrompt(context.Context, *caller.PromptInfo) (*caller.Extras, error) {
	return nil, nil
}
func (panicsInPhaseTwo) BeforePublish(context.Context, *caller.PromptInfo, any) { panic("late boom") }

// TestPrompt_DefaultsToTheClientTimeouts asserts that the client's inactivity
// timeout is the default of a prompt that sets none.
func TestPrompt_DefaultsToTheClientTimeouts(t *testing.T) {
	f := setup(t, mod(func(h *host) { h.script = func(h *host, m *nats.Msg) { h.ack(m) } }), caller.WithInactivityTimeout(150*time.Millisecond))
	st, err := f.a.PromptText(ctxT(t), "go")
	if err != nil {
		t.Fatal(err)
	}
	_, err = collect(t, st)
	var s *agents.StreamStalledError
	if !errors.As(err, &s) || s.Timeout != 150*time.Millisecond {
		t.Fatal(err)
	}
}
