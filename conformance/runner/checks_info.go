// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	agents "github.com/bogworks/synadia-agents.go"
)

var semverRE = regexp.MustCompile(`^v?\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// knownAbbreviations is SAP Appendix C (informative).
var knownAbbreviations = map[string]string{"claude-code": "cc", "openclaw": "oc", "pi": "pi", "hermes": "hermes", "dspy": "dspy"}

func registerInfo(add adder) {
	// Every check here judges one instance's own record: run it for each.
	add = perEach(add)
	add(chkAgentToken, "SAP-2-02")
	add(chkOwnerToken, "SAP-2-03", "SAP-3.2-05")
	add(chkEndpointNames, "SAP-2-04")
	add(func(_ context.Context, t *T) Outcome { return subjectForm(t, t.Prompt, "prompt") }, "SAP-2-05")
	add(func(_ context.Context, t *T) Outcome { return subjectForm(t, t.Status, "status") }, "SAP-2-07", "SAP-8.7-02")
	add(func(_ context.Context, t *T) Outcome { return endpointNamed(t, t.Prompt, "prompt") }, "SAP-2-09")
	add(func(_ context.Context, t *T) Outcome { return endpointNamed(t, t.Status, "status") }, "SAP-2-10")
	add(chkPromptMetadata, "SAP-2.1-01")
	add(chkMaxPayload, "SAP-2.1-02")
	add(chkAttachmentsOK, "SAP-2.1-04")
	add(chkTokensMUST, "SAP-2.2-01")
	add(chkTokensDollar, "SAP-2.2-02")
	add(chkTokensRecommended, "SAP-2.2-03", "SAP-2.2-04")
	add(chkSubjectLength, "SAP-2.2-05")
	add(chkMicroService, "SAP-3-01")
	add(chkServiceName, "SAP-3.1-01")
	add(chkServiceVersion, "SAP-3.1-02")
	add(chkServiceDescription, "SAP-3.1-03")
	add(chkMetadataObject, "SAP-3.1-04")
	add(func(_ context.Context, t *T) Outcome { return metaStringKey(t, agents.MetaAgent) }, "SAP-3.2-01")
	add(func(_ context.Context, t *T) Outcome { return metaStringKey(t, agents.MetaOwner) }, "SAP-3.2-02")
	add(func(_ context.Context, t *T) Outcome { return metaStringKey(t, agents.MetaProtocolVersion) }, "SAP-3.2-03")
	add(chkAbbreviationUsed, "SAP-3.2-04")
	add(chkProtocolVersionKnown, "SAP-3.2-08")
	add(chkExtraMetadata, "SAP-3.2-10")
	add(func(_ context.Context, t *T) Outcome { return queueGroup(t, t.Prompt, "prompt") }, "SAP-3.3-01", "SAP-3.3-02")
	add(func(_ context.Context, t *T) Outcome { return queueGroup(t, t.Status, "status") }, "SAP-8.7-03")
	add(chkHeartbeatNoQueue, "SAP-3.3-03")
	add(chkPing, "SAP-4-01")
	add(chkDirectInfo, "SAP-4-02")
	add(chkInfoAll, "SAP-4.1-01")
	add(chkVersionQualifier, "SAP-11.1-01")
}

func chkAgentToken(_ context.Context, t *T) Outcome {
	if !t.DefaultForm {
		return policy("the prompt subject %q is not the default form, so there is no agent token to compare", t.tg.key)
	}
	agent, ok := t.Inst.MetaString(agents.MetaAgent)
	if !ok {
		return fail("metadata.agent is missing, so the subject token cannot be compared")
	}
	switch {
	case t.AgentTok == agent:
		return pass("token %q is the harness identifier", agent)
	case knownAbbreviations[agent] == t.AgentTok:
		return pass("token %q is the Appendix C abbreviation of %q", t.AgentTok, agent)
	case isAbbreviation(agent, t.AgentTok):
		return pass("token %q reads as an abbreviation of %q", t.AgentTok, agent)
	}
	return policy("token %q differs from metadata.agent %q and is not a known abbreviation; the runner cannot judge it", t.AgentTok, agent)
}

func isAbbreviation(full, short string) bool {
	if short == "" || len(short) >= len(full) {
		return false
	}
	if strings.HasPrefix(full, short) {
		return true
	}
	var initials strings.Builder
	for _, p := range strings.FieldsFunc(full, func(r rune) bool { return r == '-' || r == '_' }) {
		initials.WriteByte(p[0])
	}
	return initials.String() == short
}

func chkOwnerToken(_ context.Context, t *T) Outcome {
	owner, ok := t.Inst.MetaString(agents.MetaOwner)
	if !ok {
		return fail("metadata.owner is missing")
	}
	if !t.DefaultForm {
		return policy("the prompt subject %q is not the default form, so there is no owner token to compare", t.tg.key)
	}
	if t.OwnerTok != owner {
		return fail("owner token %q differs from metadata.owner %q", t.OwnerTok, owner)
	}
	return pass("owner token equals metadata.owner (%q)", owner)
}

func chkEndpointNames(_ context.Context, t *T) Outcome {
	var problems []string
	checked := 0
	for _, ep := range t.Inst.Endpoints {
		verb, _, err := agents.ParseSubject(ep.Subject)
		if err != nil {
			continue // a custom subject carries no verb token to compare
		}
		checked++
		if string(verb) != ep.Name {
			problems = append(problems, fmt.Sprintf("endpoint %q is on a %s subject", ep.Name, verb))
		}
	}
	if checked == 0 {
		return policy("no endpoint uses the default subject form, so no verb token can be compared")
	}
	return verdict(problems, fmt.Sprintf("%d endpoint name(s) equal their verb token", checked))
}

func subjectForm(t *T, ep *Endpoint, verb string) Outcome {
	if ep == nil {
		return fail("no %s endpoint is registered", verb)
	}
	v, as, err := agents.ParseSubject(ep.Subject)
	if err != nil || string(v) != verb {
		return policy("subject %q is not the default agents.%s.{agent}.{owner}.{name}; the spec lets an agent override it", ep.Subject, verb)
	}
	if ep.Subject != fmt.Sprintf("agents.%s.%s.%s.%s", verb, as.Token(), as.Owner(), as.Name()) {
		return fail("subject %q does not round-trip", ep.Subject)
	}
	return pass("%s", ep.Subject)
}

func endpointNamed(_ *T, ep *Endpoint, name string) Outcome {
	if ep == nil {
		return fail("the service record has no endpoint named %q", name)
	}
	return pass("endpoint %q is registered on %s", name, ep.Subject)
}

func chkPromptMetadata(_ context.Context, t *T) Outcome {
	if t.Prompt == nil {
		return fail("no prompt endpoint")
	}
	if !t.Prompt.HasMeta {
		return fail("the prompt endpoint declares no metadata")
	}
	return pass("%d metadata key(s)", len(t.Prompt.Meta))
}

func chkMaxPayload(_ context.Context, t *T) Outcome {
	if t.Prompt == nil {
		return fail("no prompt endpoint")
	}
	v, ok := t.Prompt.Meta[agents.MetaMaxPayload]
	if !ok {
		return fail("max_payload is missing")
	}
	s, ok := v.(string)
	if !ok {
		return fail("max_payload is %T, not a string", v)
	}
	if _, err := agents.ParseSize(s); err != nil {
		return fail("max_payload %q is not a positive integer with a B, KB, MB or GB suffix", s)
	}
	return pass("max_payload %q", s)
}

func chkAttachmentsOK(_ context.Context, t *T) Outcome {
	if t.Prompt == nil {
		return fail("no prompt endpoint")
	}
	if _, ok := t.Prompt.Meta[agents.MetaAttachmentsOK]; !ok {
		return fail("attachments_ok is missing")
	}
	v, ok := t.attachmentsOK()
	if !ok {
		return fail("attachments_ok is %v, not true or false", t.Prompt.Meta[agents.MetaAttachmentsOK])
	}
	return pass("attachments_ok %v", v)
}

// protocolSubjects are the endpoint subjects the runner holds the token rules to.
func (t *T) protocolSubjects() []string {
	var out []string
	for _, ep := range t.Inst.Endpoints {
		out = append(out, ep.Subject)
	}
	if hb := t.ExpectedHeartbeatSubject(); hb != "" {
		out = append(out, hb)
	}
	return out
}

func chkTokensMUST(_ context.Context, t *T) Outcome {
	var problems []string
	for _, s := range t.protocolSubjects() {
		for _, tok := range strings.Split(s, ".") {
			if err := agents.ValidateToken(agents.RoleSubjectToken, tok); err != nil {
				problems = append(problems, fmt.Sprintf("%q in %q: %v", tok, s, err))
			}
		}
	}
	return verdict(problems, "every token of every endpoint subject is a valid NATS token")
}

func chkTokensDollar(_ context.Context, t *T) Outcome {
	var problems []string
	for _, s := range t.protocolSubjects() {
		for _, tok := range strings.Split(s, ".") {
			if strings.HasPrefix(tok, "$") {
				problems = append(problems, fmt.Sprintf("token %q of %q begins with $", tok, s))
			}
		}
	}
	return verdict(problems, "no token begins with $")
}

func chkTokensRecommended(_ context.Context, t *T) Outcome {
	var problems []string
	for _, s := range t.protocolSubjects() {
		for _, tok := range strings.Split(s, ".") {
			if tok == agents.SubjectRoot {
				continue // the root token "agents" is exempt from the recommended-form rule
			}
			if !agents.IsRecommendedToken(tok) && !isVerb(tok) {
				problems = append(problems, fmt.Sprintf("token %q of %q is outside a-z 0-9 - _ or 1-63 characters", tok, s))
			}
		}
	}
	return verdict(problems, "every token is within the recommended form")
}

func isVerb(tok string) bool {
	switch agents.Verb(tok) {
	case agents.VerbPrompt, agents.VerbHeartbeat, agents.VerbStatus, agents.VerbAttachments:
		return true
	}
	return false
}

func chkSubjectLength(_ context.Context, t *T) Outcome {
	var problems []string
	for _, s := range t.protocolSubjects() {
		problems = append(problems, agents.SubjectWarnings(s)...)
	}
	return verdict(problems, fmt.Sprintf("every subject is under %d characters", agents.MaxRecommendedSubjectLen))
}

func chkMicroService(ctx context.Context, t *T) Outcome {
	var problems []string
	if t.Inst.Type != "io.nats.micro.v1.info_response" {
		problems = append(problems, fmt.Sprintf("the INFO record type is %q, not the micro info type", t.Inst.Type))
	}
	p := t.pingObs(ctx)
	found := false
	for _, m := range p.Msgs {
		if strings.Contains(string(m.Data), t.Inst.ID) {
			found = true
		}
	}
	if !found {
		problems = append(problems, "the instance did not answer $SRV.PING.agents")
	}
	return verdict(problems, "the record is a micro info response and the instance answers $SRV.PING")
}

func chkServiceName(_ context.Context, t *T) Outcome {
	if t.Inst.Name != agents.ServiceName {
		return fail("service name is %q, not %q (instance %s)", t.Inst.Name, agents.ServiceName, t.Inst.ID)
	}
	return pass("service name %q", t.Inst.Name)
}

func chkServiceVersion(_ context.Context, t *T) Outcome {
	if !semverRE.MatchString(t.Inst.Version) {
		return fail("service version %q is not a semver", t.Inst.Version)
	}
	return pass("version %s", t.Inst.Version)
}

func chkServiceDescription(_ context.Context, t *T) Outcome {
	if strings.TrimSpace(t.Inst.Description) == "" {
		return fail("the service description is empty or missing")
	}
	return pass("%q", trunc(t.Inst.Description, 60))
}

func chkMetadataObject(_ context.Context, t *T) Outcome {
	if !t.Inst.MetaIsObject {
		return fail("service metadata is not a JSON object")
	}
	return pass("%d key(s)", len(t.Inst.Meta))
}

func metaStringKey(t *T, key string) Outcome {
	v, present := t.Inst.Meta[key]
	if !present {
		return fail("metadata.%s is missing", key)
	}
	s, ok := v.(string)
	if !ok {
		return fail("metadata.%s is %T, not a string", key, v)
	}
	if s == "" {
		return fail("metadata.%s is empty", key)
	}
	return pass("%s = %q", key, s)
}

func chkAbbreviationUsed(_ context.Context, t *T) Outcome {
	agent, _ := t.Inst.MetaString(agents.MetaAgent)
	if !t.DefaultForm || t.AgentTok == agent {
		return na("the subject uses the full agent identifier")
	}
	return pass("subject token %q abbreviates %q", t.AgentTok, agent)
}

func chkProtocolVersionKnown(_ context.Context, t *T) Outcome {
	s, ok := t.Inst.MetaString(agents.MetaProtocolVersion)
	if !ok {
		return fail("metadata.protocol_version is missing")
	}
	v, err := agents.ParseVersion(s)
	if err != nil {
		return fail("protocol_version %q is not MAJOR.MINOR: %v", s, err)
	}
	switch fmt.Sprintf("%d.%d", v.Major, v.Minor) {
	case "0.1", "0.2", "0.3":
		if v.Major == agents.ProtocolMajor && v.Minor == agents.ProtocolMinor {
			return pass("protocol_version %q", s)
		}
		return pass("protocol_version %q is in the known-versions table, but this runner checks 0.3", s)
	}
	return fail("protocol_version %q is not in the known-versions table (0.1, 0.2, 0.3)", s)
}

func chkExtraMetadata(_ context.Context, t *T) Outcome {
	var extra []string
	for k := range t.Inst.Meta {
		switch k {
		case agents.MetaAgent, agents.MetaOwner, agents.MetaSession, agents.MetaProtocolVersion:
		default:
			extra = append(extra, k)
		}
	}
	if len(extra) == 0 {
		return na("the agent adds no metadata keys beyond the required ones")
	}
	return pass("additional keys: %s", strings.Join(sortedStrings(extra), ", "))
}

func queueGroup(_ *T, ep *Endpoint, name string) Outcome {
	if ep == nil {
		return fail("no %s endpoint is registered", name)
	}
	if ep.QueueGroup != agents.QueueGroup {
		return fail("the %s endpoint's queue group is %q, not %q", name, ep.QueueGroup, agents.QueueGroup)
	}
	return pass("%s queue group %q", name, ep.QueueGroup)
}

func chkHeartbeatNoQueue(_ context.Context, t *T) Outcome {
	for _, ep := range t.Inst.Endpoints {
		if agents.IsHeartbeatSubject(ep.Subject) {
			return fail("endpoint %q is registered on the heartbeat subject %s (queue group %q)", ep.Name, ep.Subject, ep.QueueGroup)
		}
	}
	return pass("no endpoint, and so no queue group, is registered on a heartbeat subject")
}

func chkPing(ctx context.Context, t *T) Outcome {
	p := t.pingObs(ctx)
	if p.Err != nil {
		return fail("$SRV.PING.agents: %v", p.Err)
	}
	n, others, unreadable := 0, 0, 0
	for _, m := range p.Msgs {
		var r struct {
			Type string `json:"type"`
			Name string `json:"name"`
			ID   string `json:"id"`
		}
		if err := json.Unmarshal(m.Data, &r); err != nil || r.ID == "" {
			unreadable++
			continue
		}
		if r.ID != t.Inst.ID {
			// Another instance's answer: it was discovered (discover adds every
			// pinged id) and is judged as its own target, so it is not discarded.
			others++
			continue
		}
		n++
		if r.Name != agents.ServiceName {
			return fail("the ping answer names the service %q", r.Name)
		}
	}
	if unreadable > 0 {
		return fail("%d unparseable $SRV.PING.agents answer(s), with no instance id to attribute them to (instance %s answered %d time(s))", unreadable, t.Inst.ID, n)
	}
	if t.cfg.MaxDiscovery > 0 && len(p.Msgs) >= t.cfg.MaxDiscovery {
		return fail("$SRV.PING.agents reached the reply cap (%d, --max-discovery): later answers were not read", t.cfg.MaxDiscovery)
	}
	switch n {
	case 1:
		return pass("instance %s answered $SRV.PING.agents once", t.Inst.ID)
	case 0:
		return fail("instance %s did not answer $SRV.PING.agents (%d answers from others)", t.Inst.ID, others)
	}
	return fail("instance %s answered $SRV.PING.agents %d times", t.Inst.ID, n)
}

func chkDirectInfo(ctx context.Context, t *T) Outcome {
	p := t.pingObs(ctx)
	if p.Info == nil {
		if p.InfoErr != nil {
			return fail("$SRV.INFO.agents.%s: %v", t.Inst.ID, p.InfoErr)
		}
		return fail("$SRV.INFO.agents.%s was not answered", t.Inst.ID)
	}
	var first *Instance
	// Every answer to the direct request is judged: a second responder, or one
	// that claims another id or service, is a finding, not an extra to skip.
	for i, m := range p.InfoDirect {
		in, err := parseInstance(m.Data)
		if err != nil {
			return fail("the direct INFO answer %d is not a service record: %v", i, err)
		}
		if in.ID != t.Inst.ID || in.Name != agents.ServiceName || in.Endpoint(agents.EndpointPrompt) == nil {
			return fail("the direct INFO answer %d is not the full record of instance %s (it claims id %q, service %q)", i, t.Inst.ID, in.ID, in.Name)
		}
		if first == nil {
			first = in
		}
	}
	if len(p.InfoDirect) > 1 {
		return fail("%d responders answered $SRV.INFO.agents.%s; one instance id is one responder", len(p.InfoDirect), t.Inst.ID)
	}
	return pass("full record with %d endpoint(s)", len(first.Endpoints))
}

func chkInfoAll(ctx context.Context, t *T) Outcome {
	p := t.pingObs(ctx)
	if p.InfoAllErr != nil {
		return fail("$SRV.INFO.agents: %v", p.InfoAllErr)
	}
	var bad []string
	var found *Instance
	copies := 0
	for _, m := range p.InfoAll {
		in, err := parseInfoReply(m.Data)
		if err != nil {
			bad = append(bad, err.Error())
			continue
		}
		if in.ID == t.Inst.ID {
			copies++
			if found == nil {
				found = in
			}
		}
	}
	if len(bad) > 0 {
		return fail("%d malformed reply(ies) on $SRV.INFO.agents (instance %s): %s", len(bad), t.Inst.ID, strings.Join(bad, "; "))
	}
	if t.cfg.MaxDiscovery > 0 && len(p.InfoAll) >= t.cfg.MaxDiscovery {
		return fail("$SRV.INFO.agents reached the reply cap (%d, --max-discovery): later records were not read", t.cfg.MaxDiscovery)
	}
	if found == nil {
		return fail("instance %s is absent from $SRV.INFO.agents", t.Inst.ID)
	}
	if copies > 1 || t.Inst.Dups > 0 {
		return fail("instance %s answered $SRV.INFO.agents %d times (%d repeated in discovery%s); a record per instance means one", t.Inst.ID, max(copies, t.Inst.Dups+1), t.Inst.Dups, conflictNote(t.Inst))
	}
	if problems := append(append([]string(nil), found.Problems...), t.Inst.Problems...); len(problems) > 0 {
		return fail("the $SRV.INFO.agents record of instance %s is malformed: %s", t.Inst.ID, strings.Join(uniqueStrings(problems), "; "))
	}
	ep := found.Endpoint(agents.EndpointPrompt)
	if ep == nil || !ep.HasMeta {
		return fail("the $SRV.INFO.agents record lacks the prompt endpoint's capability metadata")
	}
	return pass("full record with endpoint metadata")
}

func conflictNote(in *Instance) string {
	if in.DupConflict {
		return ", and the copies differ"
	}
	return ""
}

func chkVersionQualifier(_ context.Context, t *T) Outcome {
	s, _ := t.Inst.MetaString(agents.MetaProtocolVersion)
	if strings.Count(s, ".") > 1 || strings.ContainsAny(s, "-+") {
		return pass("protocol_version %q carries a qualifier", s)
	}
	return na("protocol_version %q carries no qualifier", s)
}
