// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	agents "github.com/bogworks/synadia-agents.go"
	"github.com/bogworks/synadia-agents.go/identity"
)

// ---- senders ----------------------------------------------------------------

type signerID struct {
	signer identity.Signer
	id     identity.AgentID
	err    error
}

type senderSet struct {
	eph, real, refused signerID
}

// accountOf asks the server who this connection is ($SYS.REQ.USER.INFO); a
// server request, not a probe of the agent.
func (t *T) accountOf(ctx context.Context, s identity.Signer) (string, error) {
	if t.cfg.SenderAccount != "" {
		return t.cfg.SenderAccount, nil
	}
	rctx, cancel := context.WithTimeout(ctx, identity.DefaultUserInfoTimeout)
	defer cancel()
	m, err := t.nc.RequestWithContext(rctx, identity.UserInfoSubject, nil)
	if err != nil {
		return "", fmt.Errorf("could not learn the sender's account (%w); supply --sender-account", err)
	}
	id, err := identity.IdentityFromUserInfoReply(m.Data)
	if err != nil {
		return "", fmt.Errorf("the connection has no usable identity (%w); supply --sender-account", err)
	}
	if id.User() != s.PublicKey() {
		return "", errors.New("the connection's user is not the sender's; supply --sender-account")
	}
	return id.Account(), nil
}

func (t *T) mk(ctx context.Context, s identity.Signer) signerID {
	acct, err := t.accountOf(ctx, s)
	if err != nil {
		return signerID{err: err}
	}
	id, err := identity.NewAgentID(acct, s.PublicKey())
	return signerID{signer: s, id: id, err: err}
}

func (t *T) senderSet(ctx context.Context) *senderSet {
	if t.senders != nil {
		return t.senders
	}
	set := &senderSet{}
	t.senders = set
	if t.cfg.SenderSigner != nil {
		set.real = t.mk(ctx, t.cfg.SenderSigner)
	}
	if t.cfg.RefusedSigner != nil {
		set.refused = t.mk(ctx, t.cfg.RefusedSigner)
	}
	kp, err := nkeys.CreateUser()
	if err != nil {
		set.eph.err = err
		return set
	}
	seed, err := kp.Seed()
	if err != nil {
		set.eph.err = err
		return set
	}
	signer, err := identity.SignerFromSeed(seed)
	if err != nil {
		set.eph.err = err
		return set
	}
	acct := "$G"
	if set.real.err == nil && set.real.signer != nil {
		acct = set.real.id.Account()
	} else if t.cfg.SenderAccount != "" {
		acct = t.cfg.SenderAccount
	}
	id, err := identity.NewAgentID(acct, signer.PublicKey())
	set.eph = signerID{signer: signer, id: id, err: err}
	return set
}

// ---- request builders -------------------------------------------------------

// reqSpec describes one identity probe.
type reqSpec struct {
	// who signs; nil sends no signed header.
	who *signerID
	// header builds the Agent-Sender (or other) headers from the payload.
	header func(payload []byte) (nats.Header, error)
}

// probeIdentity sends the sender-identity probes. Most are unsigned, malformed,
// claimed-only or badly signed on purpose, to test how the host classifies and
// rejects them (SAP §13.7), so they do not go through probeAuth; the ones that
// must be verified carry a signature built by signedSpec.
func (t *T) probeIdentity(ctx context.Context, name string, spec reqSpec) *Stream {
	if s, ok := t.streams[name]; ok {
		return t.reuse(s)
	}
	payload := []byte(t.cfg.ProbePrompt)
	var s *Stream
	if t.Prompt == nil {
		s = &Stream{Name: name, Term: -1, End: "error", Err: errors.New("no prompt endpoint registered")}
	} else {
		var hdr nats.Header
		var err error
		if spec.header != nil {
			hdr, err = spec.header(payload)
		}
		if err != nil {
			s = &Stream{Name: name, Term: -1, End: "error", Err: err}
		} else {
			s = t.collect(ctx, name, t.Prompt.Subject, payload, hdr, collectOpts{})
		}
	}
	t.streams[name] = s
	t.idStreams = append(t.idStreams, s)
	return s
}

func (t *T) identityErrorStreams() []*Stream {
	var out []*Stream
	for _, s := range t.idStreams {
		if _, _, ok := s.ErrorMsg(); ok {
			out = append(out, s)
		}
	}
	return out
}

func rawHeader(value string) nats.Header {
	h := nats.Header{}
	h.Set(agents.HeaderAgentSender, value)
	return h
}

func randomSig() string {
	b := make([]byte, 64)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

type signMod struct {
	subject string
	tsDelta time.Duration
	ts      string
	tamper  func(*identity.SenderHeader)
}

func signedSpec(who *signerID, subject func() string, m signMod) reqSpec {
	return reqSpec{who: who, header: func(payload []byte) (nats.Header, error) {
		if who.err != nil {
			return nil, who.err
		}
		sub := subject()
		if m.subject != "" {
			sub = m.subject
		}
		opts := identity.SignOptions{Signer: who.signer, ID: who.id, Subject: sub, Payload: payload, Name: "sapconform"}
		switch {
		case m.ts != "":
			opts.TS = m.ts
		case m.tsDelta != 0:
			opts.TS = identity.FormatTimestamp(time.Now().Add(m.tsDelta))
		}
		sh, err := identity.SignHeader(opts)
		if err != nil {
			return nil, err
		}
		if m.tamper != nil {
			m.tamper(&sh)
		}
		h := nats.Header{}
		sh.Set(h)
		return h, nil
	}}
}

func (t *T) promptSubj() string {
	if t.Prompt == nil {
		return ""
	}
	return t.Prompt.Subject
}

// The named identity probes. Each returns a cached stream.
func (t *T) pAbsent(ctx context.Context) *Stream {
	return t.probeIdentity(ctx, "id-absent", reqSpec{})
}

// probeAuth returns the Agent-Sender headers the well-formed probes carry. With
// a seed and an endpoint that implements the extension, every probe is signed,
// so a signed endpoint serves it. need is non-empty when the endpoint requires
// a signed sender and the runner cannot sign.
func (t *T) probeAuth(ctx context.Context, payload []byte) (hdr nats.Header, need string) {
	declared, _ := t.Prompt.Meta[identity.MetadataKeyMinSenderTrust].(string)
	_, has := t.Prompt.Meta[identity.MetadataKeyMinSenderTrust]
	ext := has || t.trust() == "any" || t.trust() == "signed"
	if t.cfg.SenderSigner != nil && ext {
		set := t.senderSet(ctx)
		if set.real.err != nil {
			return nil, "the sender seed cannot be used: " + set.real.err.Error()
		}
		h, err := signedSpec(&set.real, t.promptSubj, signMod{}).header(payload)
		if err != nil {
			return nil, "signing failed: " + err.Error()
		}
		return h, ""
	}
	if has && declared != "any" || t.trust() == "signed" {
		return nil, "the endpoint requires a signed sender; supply --sender-seed so the runner can send requests it will serve"
	}
	return nil, ""
}

func (t *T) pClaimed(ctx context.Context) *Stream {
	set := t.senderSet(ctx)
	return t.probeIdentity(ctx, "id-claimed", reqSpec{header: func([]byte) (nats.Header, error) {
		if set.eph.err != nil {
			return nil, set.eph.err
		}
		ch, err := identity.ClaimHeader(set.eph.id, "sapconform")
		if err != nil {
			return nil, err
		}
		h := nats.Header{}
		ch.Set(h)
		return h, nil
	}})
}

func (t *T) pUnknownV(ctx context.Context) *Stream {
	set := t.senderSet(ctx)
	return t.probeIdentity(ctx, "id-unknown-v", reqSpec{header: func([]byte) (nats.Header, error) {
		return rawHeader(fmt.Sprintf(`{"v":2,"account":%s,"user":%s,"future_field":true}`,
			jsonString(set.eph.id.Account()), jsonString(set.eph.id.User()))), nil
	}})
}

func (t *T) pUnknownField(ctx context.Context) *Stream {
	set := t.senderSet(ctx)
	return t.probeIdentity(ctx, "id-unknown-field", reqSpec{header: func([]byte) (nats.Header, error) {
		return rawHeader(fmt.Sprintf(`{"v":1,"account":%s,"user":%s,"sapconform_unknown":{"a":1}}`,
			jsonString(set.eph.id.Account()), jsonString(set.eph.id.User()))), nil
	}})
}

func (t *T) pRequestInfo(ctx context.Context) *Stream {
	set := t.senderSet(ctx)
	return t.probeIdentity(ctx, "id-request-info", reqSpec{header: func([]byte) (nats.Header, error) {
		h := nats.Header{}
		h.Set(agents.HeaderRequestInfo, fmt.Sprintf(`{"acc":%s,"user":%s}`, jsonString(set.eph.id.Account()), jsonString(set.eph.id.User())))
		return h, nil
	}})
}

var malformedCases = []struct{ name, group, value string }{
	{"id-mal-json", "form", `{this is not json`},
	{"id-mal-missing", "form", `{"v":1,"account":"A"}`},
	{"id-mal-type", "form", `{"v":1,"account":5,"user":"U"}`},
	{"id-mal-sigalone", "form", `{"v":1,"account":"A","user":"UABC","sig":"AAAA"}`},
	{"id-mal-vstring", "vstring", `{"v":"1","account":"A","user":"UABC"}`},
}

func (t *T) pMalformed(ctx context.Context, group string) []*Stream {
	var out []*Stream
	for _, c := range malformedCases {
		if group != "" && c.group != group {
			continue // selects the probe group; every case in the group still runs
		}
		v := c.value
		out = append(out, t.probeIdentity(ctx, c.name, reqSpec{header: func([]byte) (nats.Header, error) { return rawHeader(v), nil }}))
	}
	return out
}

func (t *T) pBadSig(ctx context.Context) *Stream {
	set := t.senderSet(ctx)
	return t.probeIdentity(ctx, "id-badsig", signedSpec(&set.eph, t.promptSubj, signMod{tamper: func(h *identity.SenderHeader) { h.Sig = randomSig() }}))
}

func (t *T) pSubMismatch(ctx context.Context, who *signerID, name string) *Stream {
	other := "agents.hb.sapconform.other.instance"
	return t.probeIdentity(ctx, name, signedSpec(who, t.promptSubj, signMod{subject: other}))
}

func (t *T) pStale(ctx context.Context, who *signerID, name string, delta time.Duration) *Stream {
	return t.probeIdentity(ctx, name, signedSpec(who, t.promptSubj, signMod{tsDelta: delta}))
}

func (t *T) pValid(ctx context.Context, who *signerID, name string) *Stream {
	return t.probeIdentity(ctx, name, signedSpec(who, t.promptSubj, signMod{}))
}

func (t *T) pReplay(ctx context.Context, who *signerID, name string) (first, second *Stream) {
	if s, ok := t.streams[name+"-2"]; ok {
		return t.reuse(t.streams[name+"-1"]), t.reuse(s)
	}
	var hdr nats.Header
	var herr error
	frozen := reqSpec{who: who, header: func(payload []byte) (nats.Header, error) {
		h, err := signedSpec(who, t.promptSubj, signMod{}).header(payload)
		hdr, herr = h, err
		return h, err
	}}
	first = t.probeIdentity(ctx, name+"-1", frozen)
	if herr != nil || hdr == nil {
		second = &Stream{Name: name + "-2", Term: -1, End: "error", Err: herr}
		t.streams[name+"-2"] = second
		return first, second
	}
	same := reqSpec{header: func([]byte) (nats.Header, error) { return hdr, nil }}
	second = t.probeIdentity(ctx, name+"-2", same)
	return first, second
}

func (t *T) pFractional(ctx context.Context, who *signerID, name string) *Stream {
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	return t.probeIdentity(ctx, name, signedSpec(who, t.promptSubj, signMod{ts: ts}))
}

// statusWith sends a status request with the header the caller chose: claimed,
// malformed or badly signed on purpose, to show status is never refused on
// identity grounds (SAP §13.9); intentionally not through probeAuth. It reads
// the first reply only: a second responder on the status subject answers the
// unsigned probes of statusObs too, and SAP-8.7-07 reports it there.
func (t *T) statusWith(ctx context.Context, hdr nats.Header) (*nats.Msg, error) {
	if t.Status == nil {
		return nil, errors.New("no status endpoint registered")
	}
	rctx, cancel := context.WithTimeout(ctx, t.cfg.Timing.StatusTimeout)
	defer cancel()
	return t.nc.RequestMsgWithContext(rctx, &nats.Msg{Subject: t.Status.Subject, Header: hdr})
}

// ---- judging ------------------------------------------------------------------

// verified reports a stream whose request got past verification: served, or
// refused with 403 (verified but not accepted).
func verified(s *Stream) bool { return s.Served() || s.Code() == "403" }

func (t *T) trust() string { return t.cfg.ExpectMinSenderTrust }

func ext(f CheckFunc) CheckFunc {
	return func(ctx context.Context, t *T) Outcome {
		if t.trust() == "none" {
			return na("the operator says the agent does not implement the sender-identity extension")
		}
		return f(ctx, t)
	}
}

func signedOnly(f CheckFunc) CheckFunc {
	return ext(func(ctx context.Context, t *T) Outcome {
		if t.trust() != "signed" {
			return na("the endpoint declares min_sender_trust %q; this behavior is that of a signed endpoint", t.trust())
		}
		return f(ctx, t)
	})
}

// wantUnverified is what an endpoint does with a request that is not verified.
func (t *T) wantUnverified(s *Stream) (bool, string) {
	if t.trust() == "signed" {
		if s.Code() == "401" {
			return true, "401"
		}
		return false, "expected 401 from a signed endpoint, got " + s.Describe()
	}
	if s.Served() {
		return true, "served"
	}
	return false, "expected the request served by an any endpoint, got " + s.Describe()
}

func needSeed(t *T, who signerID, what string) (Outcome, bool) {
	if who.err != nil {
		return Outcome{ConfigMissing, what + ": " + who.err.Error()}, false
	}
	return Outcome{}, true
}

func registerIdentity(add adder) {
	each := perEach(add)
	// -- declaration and classification, no seed needed --
	each(ext(func(_ context.Context, t *T) Outcome {
		v, present := t.Prompt.Meta[identity.MetadataKeyMinSenderTrust]
		if !present {
			return fail("the prompt endpoint declares no min_sender_trust, although the operator says the agent implements the extension (%s)", t.trust())
		}
		if v != t.trust() {
			return fail("min_sender_trust is %v, the operator expects %q", v, t.trust())
		}
		return pass("min_sender_trust %q is declared in the prompt endpoint metadata", v)
	}), "SAP-13.9-01")

	each(ext(func(_ context.Context, t *T) Outcome {
		v, present := t.Prompt.Meta[identity.MetadataKeyMinSenderTrust]
		if !present {
			if t.trust() == "any" {
				return pass("min_sender_trust is not declared, which implies any")
			}
			return fail("min_sender_trust is not declared but the operator expects %q", t.trust())
		}
		if v != "any" && v != "signed" {
			return fail("min_sender_trust %v is neither any nor signed", v)
		}
		return pass("min_sender_trust %q", v)
	}), "SAP-13.9-02")

	each(func(_ context.Context, t *T) Outcome {
		_, present := t.Prompt.Meta[identity.MetadataKeyMinSenderTrust]
		switch {
		case t.trust() == "none" && present:
			return fail("the prompt endpoint declares min_sender_trust although the operator says the agent does not implement the extension")
		case t.trust() == "none":
			return pass("no min_sender_trust is declared, so the agent does not implement the extension")
		case present:
			return pass("min_sender_trust is declared, so the agent implements the extension")
		}
		return fail("the operator expects the extension (%s) but min_sender_trust is not declared", t.trust())
	}, "SAP-13.13-03")

	add(signedOnly(func(ctx context.Context, t *T) Outcome {
		var problems []string
		for _, s := range []*Stream{t.pAbsent(ctx), t.pClaimed(ctx), t.pUnknownV(ctx)} {
			if s.Code() != "401" {
				problems = append(problems, fmt.Sprintf("%s: %s, want 401", s.Name, s.Describe()))
			}
		}
		return verdict(problems, "an absent, a claimed and an unknown-version sender were each refused with 401")
	}), "SAP-13.9-03")

	add(signedOnly(func(ctx context.Context, t *T) Outcome {
		s := t.pClaimed(ctx)
		if s.Code() != "401" {
			return fail("a claimed sender was %s on a signed endpoint, want 401: the claim must grant nothing", s.Describe())
		}
		return pass("a claimed identity was refused with 401 on a signed endpoint")
	}), "SAP-13.1-01")

	add(signedOnly(func(ctx context.Context, t *T) Outcome {
		s := t.pRequestInfo(ctx)
		if s.Code() != "401" {
			return fail("a request with a client-written Nats-Request-Info and no signature was %s, want 401", s.Describe())
		}
		return pass("a client-written Nats-Request-Info was treated as a claim and refused with 401")
	}), "SAP-13.1-02")

	add(signedOnly(func(ctx context.Context, t *T) Outcome {
		var problems []string
		for _, s := range []*Stream{t.pAbsent(ctx), t.pClaimed(ctx)} {
			if s.Code() != "401" {
				problems = append(problems, fmt.Sprintf("%s: %s, want 401", s.Name, s.Describe()))
			}
		}
		return verdict(problems, "refusing a claimed or absent sender is 401")
	}), "SAP-13.7.3-02")

	add(ext(func(ctx context.Context, t *T) Outcome {
		var problems []string
		for _, s := range []*Stream{t.pAbsent(ctx), t.pUnknownV(ctx)} {
			if ok, why := t.wantUnverified(s); !ok {
				problems = append(problems, s.Name+": "+why)
			}
		}
		return verdict(problems, "an absent header and an unknown v both classify the sender as absent")
	}), "SAP-13.7-01")

	add(ext(func(ctx context.Context, t *T) Outcome {
		s := t.pClaimed(ctx)
		if ok, why := t.wantUnverified(s); !ok {
			return fail("a well-formed header without sig: %s", why)
		}
		return pass("a well-formed header without sig is treated as claimed (%s)", t.trust())
	}), "SAP-13.7-10")

	add(ext(func(ctx context.Context, t *T) Outcome {
		var problems []string
		for _, s := range []*Stream{t.pUnknownField(ctx), t.pUnknownV(ctx)} {
			if s.Code() == "400" {
				problems = append(problems, s.Name+": refused with 400, but unknown fields and versions are ignored")
			} else if ok, why := t.wantUnverified(s); !ok {
				problems = append(problems, s.Name+": "+why)
			}
		}
		return verdict(problems, "an unknown header field is ignored and an unknown v is treated as absent")
	}), "SAP-13.5.1-03")

	mal := func(group, ok string) CheckFunc {
		return ext(func(ctx context.Context, t *T) Outcome {
			var problems []string
			for _, s := range t.pMalformed(ctx, group) {
				if s.Code() != "400" {
					problems = append(problems, fmt.Sprintf("%s: %s, want 400", s.Name, s.Describe()))
				}
			}
			return verdict(problems, ok)
		})
	}
	add(mal("form", "each malformed header (bad JSON, missing field, mistyped field, sig alone) was refused with 400"), "SAP-13.5.1-04")
	add(mal("vstring", `v sent as the string "1" was refused with 400`), "SAP-13.5.1-05")
	add(mal("", "every malformed header was refused with 400"), "SAP-13.5.1-06", "SAP-13.7-02")

	// -- failing signatures, an ephemeral key stands in for a forger --
	add(ext(func(ctx context.Context, t *T) Outcome {
		var problems []string
		set := t.senderSet(ctx)
		if o, ok := needSeed(t, set.eph, "ephemeral key"); !ok {
			return o
		}
		for _, s := range []*Stream{t.pBadSig(ctx), t.pSubMismatch(ctx, &set.eph, "id-eph-sub"), t.pStale(ctx, &set.eph, "id-eph-stale", -10*time.Minute)} {
			if s.Code() != "401" {
				problems = append(problems, fmt.Sprintf("%s: %s, want 401", s.Name, s.Describe()))
			}
		}
		first, second := t.pReplay(ctx, &set.eph, "id-eph-replay")
		switch {
		case !verified(first):
			// A replay can only be judged when the first request passed verification.
		case second.Code() != "401":
			problems = append(problems, fmt.Sprintf("a replayed nonce: %s, want 401", second.Describe()))
		}
		return verdict(problems, "a bad signature, a wrong sub, a stale ts and a replayed nonce were each refused with 401")
	}), "SAP-13.7-07")

	add(ext(func(ctx context.Context, t *T) Outcome {
		s := t.pBadSig(ctx)
		if s.Code() != "401" {
			return fail("a failing signature on a %s endpoint: %s, want 401", t.trust(), s.Describe())
		}
		return pass("a failing signature was refused with 401 on a %s endpoint", t.trust())
	}), "SAP-13.7-08")

	add(ext(func(ctx context.Context, t *T) Outcome {
		s := t.pBadSig(ctx)
		if s.Code() != "401" {
			return fail("a failing signature was %s, want 401", s.Describe())
		}
		if t.trust() == "signed" {
			for _, u := range []*Stream{t.pAbsent(ctx), t.pClaimed(ctx)} {
				if u.Code() != "401" {
					return fail("%s on a signed endpoint was %s, want 401", u.Name, u.Describe())
				}
			}
		}
		return pass("401 for a failing signature%s", map[bool]string{true: ", and for an absent or claimed sender on a signed endpoint"}[t.trust() == "signed"])
	}), "SAP-13.7-12")

	add(ext(func(ctx context.Context, t *T) Outcome {
		var problems []string
		for _, s := range []*Stream{t.pBadSig(ctx), t.pClaimed(ctx), t.pAbsent(ctx)} {
			if s.Code() == "401" {
				if s.Term < 1 || s.Term != len(s.Msgs)-1-s.After {
					problems = append(problems, s.Name+": the 401 is not followed by the terminator")
				} else if s.Term != 1 {
					problems = append(problems, fmt.Sprintf("%s: %d message(s) before the terminator, want only the error-headered message", s.Name, s.Term))
				}
			}
		}
		if len(t.identityErrorStreams()) == 0 {
			return na("no probe drew a rejection to inspect")
		}
		return verdict(problems, "a rejection is an error-headered message followed by the terminator")
	}), "SAP-13.7-11")

	// -- status is always answerable --
	each(ext(func(_ context.Context, t *T) Outcome {
		if t.Status == nil {
			return fail("no status endpoint is registered")
		}
		if _, present := t.Status.Meta[identity.MetadataKeyMinSenderTrust]; present {
			return fail("the status endpoint's metadata carries min_sender_trust")
		}
		return pass("the status endpoint declares no min_sender_trust")
	}), "SAP-13.9-05")

	statusAnswerable := func(ctx context.Context, t *T) Outcome {
		set := t.senderSet(ctx)
		cases := []struct {
			name string
			hdr  nats.Header
		}{
			{"a claimed sender", func() nats.Header {
				if ch, err := identity.ClaimHeader(set.eph.id, "sapconform"); err == nil {
					h := nats.Header{}
					ch.Set(h)
					return h
				}
				return nil
			}()},
			{"a malformed header", rawHeader(`{not json`)},
			{"a failing signature", func() nats.Header {
				h, err := signedSpec(&set.eph, func() string { return t.Status.Subject }, signMod{tamper: func(h *identity.SenderHeader) { h.Sig = randomSig() }}).header(nil)
				if err != nil {
					return nil
				}
				return h
			}()},
		}
		var problems []string
		for _, c := range cases {
			m, err := t.statusWith(ctx, c.hdr)
			if err != nil {
				problems = append(problems, fmt.Sprintf("status request with %s: %v", c.name, err))
				continue // the failure was recorded as a problem on the line above
			}
			if _, isErr := m.Header[agents.HeaderServiceErrorCode]; isErr {
				problems = append(problems, fmt.Sprintf("status request with %s was refused", c.name))
			} else if p := t.hbProblems(m.Data, time.Now()); len(p) > 0 {
				problems = append(problems, fmt.Sprintf("status request with %s: %s", c.name, joinProblems(p)))
			}
		}
		return verdict(problems, "status requests from a claimed sender, with a malformed header and with a failing signature were all answered")
	}
	add(ext(statusAnswerable), "SAP-13.9-06", "SAP-13.9-07")

	add(func(ctx context.Context, t *T) Outcome {
		if _, present := t.Prompt.Meta[identity.MetadataKeyMinSenderTrust]; present {
			return na("the agent declares min_sender_trust, so it has identity support")
		}
		var problems []string
		for _, s := range []*Stream{t.pClaimed(ctx), t.pUnknownV(ctx), t.pMalformed(ctx, "form")[0]} {
			if !s.Served() {
				problems = append(problems, fmt.Sprintf("%s: %s", s.Name, s.Describe()))
			}
		}
		return verdict(problems, "an unknown Agent-Sender header (claim, unknown version, unparseable) was ignored and the request served")
	}, "SAP-13.13-01")

	add(func(ctx context.Context, t *T) Outcome {
		if t.trust() != "any" {
			return na("the operator does not say the endpoint declares any")
		}
		s := t.pAbsent(ctx)
		if !s.Served() {
			return fail("a request without identity was not served by an any endpoint: %s", s.Describe())
		}
		return pass("a request without identity was served as an absent sender")
	}, "SAP-13.13-02")

	// -- registration and signed heartbeats --
	each(ext(func(_ context.Context, t *T) Outcome {
		have := 0
		for _, k := range []string{identity.MetadataKeyUserNKey, identity.MetadataKeyAccount, identity.MetadataKeyIDSig} {
			if _, ok := t.Inst.MetaString(k); ok {
				have++
			}
		}
		switch have {
		case 3:
			return pass("user_nkey, account and id_sig are in the service metadata")
		case 0:
			return policy("the agent registers no agent ID, so it runs without a seed (SAP-13.11-06 describes that case)")
		}
		return fail("only %d of user_nkey, account and id_sig are in the service metadata", have)
	}), "SAP-13.10-01")

	idsig := func(_ context.Context, t *T) Outcome {
		meta := map[string]string{}
		for k, v := range t.Inst.Meta {
			if s, ok := v.(string); ok {
				meta[k] = s
			}
		}
		if meta[identity.MetadataKeyIDSig] == "" {
			return policy("the agent registers no id_sig, so it runs without a seed")
		}
		if !identity.VerifyAgentID(meta, t.promptSubj()) {
			return fail("id_sig does not verify over the agent ID fields and the advertised prompt subject %q", t.promptSubj())
		}
		derived := ""
		if t.DefaultForm {
			derived = fmt.Sprintf("agents.prompt.%s.%s.%s", t.AgentTok, t.OwnerTok, t.NameTok)
		}
		if derived == t.promptSubj() {
			return pass("id_sig verifies against the advertised prompt subject (the same as the one derived from the instance name here)")
		}
		return pass("id_sig verifies against the advertised prompt subject %q", t.promptSubj())
	}
	each(ext(idsig), "SAP-13.10-02", "SAP-13.10-03")

	holdsSeed := func(t *T) bool {
		for _, in := range t.Insts {
			if _, ok := in.MetaString(identity.MetadataKeyUserNKey); ok {
				return true
			}
		}
		return false
	}
	signedBeats := func(what string, check func(t *T, b HB, h *identity.SenderHeader) string) CheckFunc {
		return ext(func(ctx context.Context, t *T) Outcome {
			if !holdsSeed(t) {
				return na("the agent registers no user_nkey, so it holds no seed")
			}
			beats, bad := t.beatsOrFail(ctx)
			if bad != nil {
				return *bad
			}
			var problems []string
			for i, b := range beats {
				v, ok, _ := identity.ReadHeaderValue(b.Header)
				if !ok {
					problems = append(problems, fmt.Sprintf("heartbeat %d has no Agent-Sender header", i))
					continue // recorded as a problem on the line above
				}
				h, err := identity.ParseHeader(v)
				if err != nil || h == nil {
					problems = append(problems, fmt.Sprintf("heartbeat %d: unusable Agent-Sender header", i))
					continue // recorded as a problem on the line above
				}
				if p := check(t, b, h); p != "" {
					problems = append(problems, fmt.Sprintf("heartbeat %d: %s", i, p))
				}
			}
			return verdict(uniqueStrings(problems), fmt.Sprintf("%s on all %d heartbeat(s)", what, len(beats)))
		})
	}
	add(signedBeats("every heartbeat carries Agent-Sender", func(*T, HB, *identity.SenderHeader) string { return "" }), "SAP-13.11-01")
	add(signedBeats("sub is the heartbeat subject as published", func(_ *T, b HB, h *identity.SenderHeader) string {
		if h.Sub != b.Subject {
			return fmt.Sprintf("sub %q, published on %q", h.Sub, b.Subject)
		}
		return ""
	}), "SAP-13.11-02")
	add(signedBeats("ts is the payload's own ts", func(_ *T, b HB, h *identity.SenderHeader) string {
		obj := mustObj(b.Data)
		if h.TS != fmt.Sprint(obj["ts"]) {
			return fmt.Sprintf("header ts %q differs from payload ts %v", h.TS, obj["ts"])
		}
		return ""
	}), "SAP-13.11-03")
	add(ext(func(ctx context.Context, t *T) Outcome {
		if !holdsSeed(t) {
			return na("the agent registers no user_nkey, so it holds no seed")
		}
		beats, bad := t.beatsOrFail(ctx)
		if bad != nil {
			return *bad
		}
		seen := map[string]bool{}
		for i, b := range beats {
			v, ok, _ := identity.ReadHeaderValue(b.Header)
			if !ok {
				return fail("heartbeat %d has no Agent-Sender header", i)
			}
			h, err := identity.ParseHeader(v)
			if err != nil || h == nil {
				return fail("heartbeat %d: unusable Agent-Sender header", i)
			}
			if seen[h.Nonce] {
				return fail("heartbeat %d repeats the nonce %q", i, h.Nonce)
			}
			seen[h.Nonce] = true
		}
		return pass("%d heartbeat(s), %d distinct nonce(s)", len(beats), len(seen))
	}), "SAP-13.11-04")
	add(signedBeats("sig verifies over the payload bytes, by the key in user_nkey", func(t *T, b HB, h *identity.SenderHeader) string {
		want, _ := t.instFor(b.InstanceID).MetaString(identity.MetadataKeyUserNKey)
		if h.User != want {
			return fmt.Sprintf("signed by %q, not by the registered user_nkey", h.User)
		}
		_, err := identity.VerifyStored(identity.Message{Subject: b.Subject, Data: b.Data, Header: b.Header}, identity.StoredOptions{})
		if err != nil {
			return "the signature does not verify: " + err.Error()
		}
		return ""
	}), "SAP-13.11-05")

	add(ext(func(ctx context.Context, t *T) Outcome {
		if holdsSeed(t) {
			return na("the agent registers a user_nkey, so it holds a seed")
		}
		beats, bad := t.beatsOrFail(ctx)
		if bad != nil {
			return *bad
		}
		for i, b := range beats {
			if _, ok := b.Header[identity.HeaderName]; ok {
				return fail("heartbeat %d carries Agent-Sender although the agent registers no seed", i)
			}
		}
		return pass("an agent without a seed publishes heartbeats without the header")
	}), "SAP-13.11-06")

	add(ext(func(ctx context.Context, t *T) Outcome {
		o := t.statusObs(ctx)
		var problems []string
		for i, r := range o.Replies {
			if _, ok := r.Header[identity.HeaderName]; ok {
				problems = append(problems, fmt.Sprintf("status reply %d is signed", i))
			}
		}
		served := t.servedStreams2(ctx)
		for _, s := range served {
			for i, m := range s.Msgs {
				if _, ok := m.Header[identity.HeaderName]; ok {
					problems = append(problems, fmt.Sprintf("%s: reply message %d is signed", s.Name, i))
				}
			}
		}
		if len(o.Replies) == 0 && o.Err != nil {
			problems = append(problems, "the status endpoint did not answer")
		}
		if len(problems) > 0 {
			// Everything above was seen on the wire. The prompt probes may have
			// needed a seed the runner lacks; that must not erase it (see worse).
			t.observe(verdict(problems, ""))
		}
		// The prompt probes were never sent without a seed: say so here, in the
		// check, after the evidence above was recorded. The merge keeps a fail.
		if o, gap := seedGate(served...); gap {
			return o
		}
		return verdict(problems, "no status reply or prompt reply carries Agent-Sender")
	}), "SAP-13.11-08")

	registerSeeded(add)
}

func (t *T) servedStreams2(ctx context.Context) []*Stream {
	all, _ := t.servedStreams(ctx)
	return all
}

// ---- needs a seed the host admits ---------------------------------------------

func registerSeeded(add adder) {
	seeded := func(f func(ctx context.Context, t *T, set *senderSet) Outcome) CheckFunc {
		return ext(func(ctx context.Context, t *T) Outcome {
			set := t.senderSet(ctx)
			if o, ok := needSeed(t, set.real, "--sender-seed"); !ok {
				return o
			}
			control := t.pValid(ctx, &set.real, "id-valid")
			if !verified(control) {
				return fail("the control request, correctly signed by the supplied seed, was %s: either the host does not verify this sender (SAP-13.7-03) or --sender-account is wrong", control.Describe())
			}
			return f(ctx, t, set)
		})
	}
	note := func(s *Stream) string {
		if s.Code() == "403" {
			return " (the host verified the sender and then refused it with 403, which also shows verification passed)"
		}
		return ""
	}

	add(seeded(func(ctx context.Context, t *T, set *senderSet) Outcome {
		bad := t.probeIdentity(ctx, "id-real-badsig", signedSpec(&set.real, t.promptSubj, signMod{tamper: func(h *identity.SenderHeader) { h.Sig = randomSig() }}))
		if bad.Code() != "401" {
			return fail("a valid signature passed and a failing one was %s, want 401", bad.Describe())
		}
		return pass("a valid signature is accepted and a failing one refused with 401")
	}), "SAP-13.7-03")

	add(seeded(func(ctx context.Context, t *T, set *senderSet) Outcome {
		s := t.pFractional(ctx, &set.real, "id-fractional")
		if !verified(s) {
			return fail("a ts with fractional seconds was %s, but a whole-second ts was verified", s.Describe())
		}
		return pass("a ts with fractional seconds was accepted%s", note(s))
	}), "SAP-13.5-08")

	add(seeded(func(ctx context.Context, t *T, set *senderSet) Outcome {
		s := t.pSubMismatch(ctx, &set.real, "id-real-sub")
		if s.Code() != "401" {
			return fail("a valid signature over a different sub was %s, want 401", s.Describe())
		}
		return pass("a sub that is not the arrival subject was refused with 401")
	}), "SAP-13.7-04", "SAP-13.7.1-05")

	add(seeded(func(ctx context.Context, t *T, set *senderSet) Outcome {
		var problems []string
		for _, d := range []struct {
			name  string
			delta time.Duration
		}{{"id-real-old", -5 * time.Minute}, {"id-real-future", 5 * time.Minute}} {
			if s := t.pStale(ctx, &set.real, d.name, d.delta); s.Code() != "401" {
				problems = append(problems, fmt.Sprintf("%s (ts %+v from now): %s, want 401", d.name, d.delta, s.Describe()))
			}
		}
		return verdict(problems, "a ts five minutes in the past and in the future were refused with 401")
	}), "SAP-13.7-05")

	add(seeded(func(ctx context.Context, t *T, set *senderSet) Outcome {
		first, second := t.pReplay(ctx, &set.real, "id-real-replay")
		if !verified(first) {
			return fail("the first request of the replay pair was %s", first.Describe())
		}
		if second.Code() != "401" {
			return fail("the same header and payload replayed: %s, want 401", second.Describe())
		}
		return pass("a replayed nonce was refused with 401")
	}), "SAP-13.7-06")

	add(seeded(func(ctx context.Context, t *T, set *senderSet) Outcome {
		s := t.probeIdentity(ctx, "id-pattern", signedSpec(&set.real, t.promptSubj, signMod{subject: "agents.prompt.>"}))
		if s.Code() != "401" {
			return fail("sub set to the pattern agents.prompt.> was %s, want 401: sub is compared, not matched", s.Describe())
		}
		return pass("a sub that is a pattern matching the arrival subject was refused with 401")
	}), "SAP-13.7.1-01")

	add(seeded(func(_ context.Context, t *T, set *senderSet) Outcome {
		return pass("sub equal to the arrival subject was verified%s", note(t.streams["id-valid"]))
	}), "SAP-13.7.1-02")

	refused := func(ctx context.Context, t *T) Outcome {
		set := t.senderSet(ctx)
		if o, ok := needSeed(t, set.refused, "--refused-seed"); !ok {
			return o
		}
		s := t.pValid(ctx, &set.refused, "id-refused")
		if s.Code() != "403" {
			return fail("a verified sender the host should refuse was %s, want 403", s.Describe())
		}
		return pass("a verified but unaccepted sender was refused with 403")
	}
	add(ext(refused), "SAP-13.7-13", "SAP-13.7.3-01")
}
