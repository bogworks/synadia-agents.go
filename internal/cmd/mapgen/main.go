// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

// Command mapgen regenerates docs/protocol-mapping.md from the Go source of
// the module: it parses every public package with go/parser and writes one row
// per exported identifier. The section of a row is taken from the "SAP §n"
// citations in the godoc of the identifier (or of its type); an identifier
// whose godoc cites none is looked up in the rules table below.
//
// Usage, from the repository root:
//
//	go run ./internal/cmd/mapgen
//
// or "go generate ./internal/cmd/mapgen". Packages under internal/ are not
// part of the public API and are not mapped.
package main

//go:generate go run . -root ../../..

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// item is one exported identifier.
type item struct {
	pkg, dir, name, kind, doc, recv string
}

// rule gives the section of the identifiers (qualified as package.Name) that
// match pattern, when their godoc cites no SAP section.
type rule struct{ pattern, section string }

// outside names the packages that SAP does not cover at all.
var outside = map[string]string{
	"connect":     "outside SAP: connection helper",
	"tools":       "outside SAP: agent tool layer",
	"runner":      "outside SAP: conformance runner",
	"conformance": "outside SAP: conformance catalog",
	"agentstest":  "outside SAP: test support",
}

// rules is the table for identifiers whose godoc cites no section. Keep it
// small: prefer a "SAP §n" citation in the godoc itself.
var rules = []rule{
	{`^caller\.DefaultDiscoverMaxRecords$`, "SDK policy (§4.1, §4.2)"},
	{`^host\.(DefaultHeartbeatSignTimeout|WithHeartbeatSignTimeout)$`, "§8.3, §13.11 (SDK policy: signing bound)"},
	{`^identity\.Expired$`, "§13.7 (replay window)"},
	{`^caller\.(ErrUntrustedAgent|WithTrustPolicy)$`, "§13.10"},
	{`^agents\.(DefaultSaveMaxTotalBytes|SkipReason|Skip.*|SavedAttachment|SaveOptions)$`, "§5.2 (saving is SDK policy)"},
	{`^agents\.Trust(Any|Signed)$`, "§13.9"},
	{`^agents\.DecodeServiceInfo$`, "§3, §4.1"},
	{`^agents\.DecodeServicePing$`, "§8.4"},
	{`^agents\.ErrRecordInconsistent$`, "§2, §4.3"},
	{`^agents\.ErrInvalidExtra$`, "§5.6"},
	{`^agents\.(ErrProtocol|ProtocolError.*)$`, "§6.6, §9"},
	{`^agents\.CodeOf$`, "§9.1"},
	{`^agents\.Header$`, "§9, §13.5"},
	{`^agents\.(DefaultHeartbeatInterval|MinRecommendedHeartbeatInterval|OfflineThresholdFactor)$`, "§8.2"},
	{`^agents\.SubjectOption$`, "§2.2"},
	{`^agents\.ParseAgentSubject$`, "§2"},
	{`^agents\.InstanceIDFromInfoSubject$`, "§4.2"},
	{`^agents\.SpecCommit$`, "§1.2"},
	{`^agents\.(Compatible|MinorDrift|Incompatible|VersionExact|VersionSameMajor)$`, "§11.2"},
	{`^caller\.(PromptOption|WithPrompt.*)$`, "§5, §6.6"},
	{`^caller\.(StatusOption|WithStatus.*)$`, "§8.7"},
	{`^caller\.(Client|Client\.(Connection|IsClosed|Done|Close|Drain|Stats)|Stats)$`, "§5, §6 (SDK policy: lifecycle and counters)"},
	{`^caller\.Default(InboxPrefix|MaxWait|StreamBuffer.*|CloseTimeout|FlushTimeout)$`, "SDK policy (§5.1, §6.6)"},
	{`^caller\.Default(StatusTimeout)$`, "SDK policy (§8.7)"},
	{`^caller\.Default(LookupTimeout|DiscoverStall|DiscoverMaxWait|DiscoverBuffer)$`, "SDK policy (§4.1, §4.2)"},
	{`^caller\.(DiscoverOption|WithDiscover.*|WithFilter|WithCompatibleOnly)$`, "§4.1, §4.2"},
	{`^caller\.WithRequireVerifiedIdentityFunc$`, "§13.10"},
	{`^caller\.(ErrClosed|ErrStreamConsumed)$`, "SDK policy (§6)"},
	{`^caller\.(ErrSlowConsumer|SlowConsumerError.*)$`, "SDK policy (§6.6)"},
	{`^caller\.MaxWaitError.*$`, "SDK policy (§6.6)"},
	{`^caller\.(ErrReservedHeader|ErrInvalidHeader|ReservedHeader|HeaderHook|WithRequestHeaders)$`, "§9, §13.5 (extension point)"},
	{`^caller\.ErrNoSigner$`, "§13.5, §13.6"},
	{`^caller\.QueryReplyRefusedError.*$`, "§7.1"},
	{`^caller\.(PromptInfo|Extras|Interceptor.*|PublishInterceptor.*|InterceptorFunc.*|WithInterceptors)$`, "§5.6 (extension point)"},
	{`^caller\.(Option|WithLogger|WithMaxWait|WithStreamBuffer)$`, "SDK policy"},
	{`^caller\.WithInboxPrefix$`, "§5 (reply subject; SDK policy)"},
	{`^caller\.WithTracker$`, "§8.5"},
	{`^caller\.(Client\.RefreshSelfID|Client\.InvalidateSenders|Client\.RequestSigned|SignOption|WithHeaders)$`, "§13.4, §13.6"},
	{`^caller\.(DefaultTrackerMaxInstances|TrackerOption|WithTracker.*|Beat.*|NewHeartbeatTracker)$`, "§8.5"},
	{`^host\.(ErrNextCalledTwice|ErrNextNotCalled|CallNext|Interceptor.*|InterceptorFunc.*|WithInterceptors|RawPromptHandler)$`, "§5, §6 (extension point)"},
	{`^host\.(ErrStarted|ErrNotStarted|ErrConfig|New|DefaultStartTimeout|Option|WithLogger)$`, "§3 (SDK policy: lifecycle)"},
	{`^host\.WithEndpoint$`, "§3 (extension point)"},
	{`^host\.Endpoint$`, "§3 (extension point)"},
	{`^host\.SignHeartbeatOptions$`, "§13.11"},
	{`^host\.(Stats|DefaultMaxConcurrentPrompts|BusyPolicy|BusyQueue|WithMaxConcurrentPrompts|WithBusyPolicy)$`, "SDK policy (§3.3, §5)"},
	{`^host\.DefaultMaxPayload$`, "§2.1"},
	{`^host\.RequestInfo$`, "§5 (extension point)"},
	{`^host\.WithMaxNonces.*$`, "§13.7 (SDK policy: replay bounds)"},
	{`^host\.DefaultListenProbeTimeout$`, "§6.7"},
	{`^host\.(ErrReplySubject|ReplySubjectPolicy)$`, "§7.1"},
	{`^host\.AskOption$`, "§7"},
	{`^identity\.(ErrAdmission|Description.*)$`, "§13.7.3"},
	{`^identity\.(GateOptions|NewGate|Admission.*|StatusResult)$`, "§13.7"},
	{`^identity\.(MaxHeaderValueBytes|MaxNameLength|ValidName|ReadHeaderValue)$`, "§13.5, §13.5.1"},
	{`^identity\.(DefaultMaxNonces|DefaultMaxNoncesPerSender|RecordResult|Recorded|Replayed|SetFull|SenderQuotaExceeded|NonceSetOptions|NewNonceSet)$`, "§13.7 (replay window; bounds are SDK policy)"},
	{`^identity\.(ErrBadRequestInfo|ParseRequestInfo|ReadRequestInfo)$`, "§13.4, §13.7.2"},
	{`^identity\.(DefaultListMaxWait|DefaultListStall|Registration|RegistrationLister|ListOptions|ResolverOptions|NewSenderResolver)$`, "§13.8, §13.10"},
	{`^identity\.(Requester.*|SelfIDOptions|NewSelfIDResolver)$`, "§13.4"},
	{`^identity\.(SignOptions|ErrInvalidSeed|SignerWiper.*|SignerFromSeedAndJWT)$`, "§13.6"},
	{`^identity\.(Trust(Verified|Claimed|Absent|Any)|TrustOf|Message|MessageFromNATS|ClaimedIdentity|LiveOptions)$`, "§13.1, §13.7"},
}

type section struct {
	dir, title, intro string
}

var sections = []section{
	{".", "Package `agents` (root): the shared protocol types", "Everything both sides share: subjects, headers, envelope, chunks, errors, heartbeats, attachments, discovery records and versions. It needs no connection."},
	{"caller", "Package `caller`: the caller side", "Discovery, prompting, the reply stream, queries, status, heartbeat tracking and the caller half of the sender identity."},
	{"host", "Package `host`: the host side", "Registration as a micro service, prompt and status endpoints, the reply stream, queries, heartbeats and the host half of the sender identity."},
	{"identity", "Package `identity`: sender identity (SAP §13)", "The optional sender identity extension: agent IDs, the `Agent-Sender` header, signing, verification, trust classes, admission, replay protection and registration."},
	{"connect", "Package `connect`: connection helpers", "Reads NATS CLI contexts and credential files into connection options and, optionally, a signer. SAP does not cover connecting, so these are outside the protocol; the official SDKs have equivalents."},
	{"agentstest", "Package `agentstest`: test support", "A reference agent for tests of programs that talk to an agent. It is not part of the protocol."},
	{"tools", "Package `tools`: agent tool layer", "The SDK counterpart of the official SDKs' agent-tool helpers: tool definitions and an executor over `caller`. SAP defines no tools, so every row is outside the protocol."},
	{"conformance", "Package `conformance`: the conformance catalog", "The machine-readable catalog of SAP 0.3 requirements and the check that ties each to its tests."},
	{"conformance/runner", "Package `runner`: the conformance runner", "The black-box checks behind the `sapconform` command."},
}

const header = `# Protocol mapping

This file maps every exported identifier of the module to the section of the Synadia Agents Protocol
(SAP) 0.3 that it implements. The SAP text is at spec commit ` + "`c48cea5f07965cc8fd4054e120459f1833b3ed2b`" + `;
section numbers (` + "`§5.2`" + `) refer to its ` + "`core-protocol.md`" + `.

This file is generated. Regenerate it, from the repository root, with:

    go run ./internal/cmd/mapgen

(or ` + "`go generate ./internal/cmd/mapgen`" + `). The section of a row comes from the SAP citations in the godoc
of the identifier; identifiers whose godoc cites none are classified by the small rules table in
` + "`internal/cmd/mapgen/main.go`" + `. Packages under ` + "`internal/`" + ` are not part of the API and are not mapped.

How to read it:

- **Section** is the part of SAP the identifier implements or is bounded by. "SDK policy" marks a choice the
  SDK makes where the spec leaves it open (a default, a bound, a timeout); the spec is never made stricter
  than its MUSTs by it, and a SHOULD or MAY is never turned into a MUST. "Outside SAP" marks code that SAP
  does not cover at all.
- Methods are listed as ` + "`package.Type.Method`" + `. Fields of a struct and the enumerated values of a type are
  documented on the type; the row of the type stands for its fields.
- The godoc of each identifier (` + "`go doc -all`" + `) cites the same sections and is the authority for behavior.
  The completeness of this file is checked by ` + "`test/docs`" + `: it fails when an exported identifier of a package
  of this module has no row here.

The protocol reading itself, in order, is: subjects (§2), registration (§3), discovery (§4), the request (§5),
the response stream (§6), mid-stream queries (§7), heartbeat and status (§8), errors (§9), security (§10),
versioning (§11) and the sender identity extension (§13).
`

var (
	secRE   = regexp.MustCompile(`§\s*([0-9]+(?:\.[0-9]+)*|[A-C])((?:\s*(?:,|and|–|-|/)\s*§?\s*[0-9]+(?:\.[0-9]+)*)*)`)
	numRE   = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)*`)
	firstRE = regexp.MustCompile(`^(.+?[.!?])(\s|$)`)
)

func main() {
	root := flag.String("root", ".", "root of the module")
	flag.Parse()
	items, err := collect(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mapgen:", err)
		os.Exit(1)
	}
	out, misses := render(items)
	if err := os.WriteFile(filepath.Join(*root, "docs", "protocol-mapping.md"), []byte(out), 0o644); err != nil { //nolint:gosec // G306: a published docs file, not a secret, must be world-readable
		fmt.Fprintln(os.Stderr, "mapgen:", err)
		os.Exit(1)
	}
	if len(misses) > 0 {
		fmt.Fprintf(os.Stderr, "mapgen: %d identifiers cite no section and match no rule (listed as SDK policy):\n  %s\n", len(misses), strings.Join(misses, "\n  "))
	}
}

func collect(root string) ([]item, error) {
	var items []item
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			n := d.Name()
			if rel == "test" || rel == "examples" || n == "internal" || n == "testdata" || strings.HasPrefix(n, "_") || (strings.HasPrefix(n, ".") && rel != ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		if f.Name.Name == "main" {
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		add := func(name, kind, doc, recv string) {
			items = append(items, item{f.Name.Name, dir, name, kind, doc, recv})
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				if d.Recv == nil {
					add(d.Name.Name, "func", d.Doc.Text(), "")
					continue
				}
				if r := recvName(d.Recv.List[0].Type); ast.IsExported(r) {
					add(r+"."+d.Name.Name, "method", d.Doc.Text(), r)
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						if !s.Name.IsExported() {
							continue
						}
						doc := s.Doc.Text()
						if doc == "" {
							doc = d.Doc.Text()
						}
						add(s.Name.Name, "type", doc, "")
						if it, ok := s.Type.(*ast.InterfaceType); ok {
							for _, m := range it.Methods.List {
								for _, n := range m.Names {
									if n.IsExported() {
										add(s.Name.Name+"."+n.Name, "method", m.Doc.Text(), s.Name.Name)
									}
								}
							}
						}
					case *ast.ValueSpec:
						doc := s.Doc.Text()
						if doc == "" {
							doc = d.Doc.Text()
						}
						kind := "var"
						if d.Tok == token.CONST {
							kind = "const"
						}
						for _, n := range s.Names {
							if n.IsExported() {
								add(n.Name, kind, doc, "")
							}
						}
					}
				}
			}
		}
		return nil
	})
	return items, err
}

func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvName(t.X)
	case *ast.IndexExpr:
		return recvName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

// sections returns the distinct section numbers cited in doc, in order.
func cited(doc string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, m := range secRE.FindAllStringSubmatch(doc, -1) {
		add("§" + m[1])
		for _, r := range numRE.FindAllString(m[2], -1) {
			add("§" + r)
		}
	}
	return out
}

var reqRE = []*regexp.Regexp{
	regexp.MustCompile(`\s*\(\s*REQ-\d+\s*\)`),
	regexp.MustCompile(`,\s*REQ-\d+`),
	regexp.MustCompile(`\(REQ-\d+,\s*`),
	regexp.MustCompile(`\s*REQ-\d+`),
	regexp.MustCompile(`\(\s*\)`),
}

// note is the first sentence of doc, shortened to fit a table cell, without
// the internal requirement numbers.
func note(doc string) string {
	d := strings.Join(strings.Fields(doc), " ")
	s := d
	if m := firstRE.FindStringSubmatch(d); m != nil {
		s = m[1]
	}
	if r := []rune(s); len(r) > 110 {
		s = strings.TrimRight(string(r[:107]), " ") + "..."
	}
	s = strings.ReplaceAll(s, "|", `\|`)
	for _, re := range reqRE {
		s = re.ReplaceAllString(s, "")
	}
	s = strings.NewReplacer(", )", ")", " .", ".", "(, ", "(", " ;", ";", "(; ", "(").Replace(s)
	return strings.TrimSpace(s)
}

func render(items []item) (string, []string) {
	types := map[[2]string]item{}
	for _, i := range items {
		if i.kind == "type" {
			types[[2]string{i.dir, i.name}] = i
		}
	}
	compiled := make([]*regexp.Regexp, len(rules))
	for i, r := range rules {
		compiled[i] = regexp.MustCompile(r.pattern)
	}
	var misses []string
	rows := map[string][]string{}
	for _, i := range items {
		s := cited(i.doc)
		if len(s) == 0 && i.recv != "" {
			if p, ok := types[[2]string{i.dir, i.recv}]; ok {
				s = cited(p.doc)
			}
		}
		if len(s) > 4 {
			s = s[:4]
		}
		sec := strings.Join(s, ", ")
		q := i.pkg + "." + i.name
		if sec == "" {
			for k, re := range compiled {
				if re.MatchString(q) {
					sec = rules[k].section
					break
				}
			}
		}
		if sec == "" {
			if o, ok := outside[i.pkg]; ok {
				sec = o
			} else {
				sec = "SDK policy (no SAP section)"
				misses = append(misses, q)
			}
		}
		rows[i.dir] = append(rows[i.dir], fmt.Sprintf("| `%s` | %s | %s | %s |", q, i.kind, sec, note(i.doc)))
	}
	var b strings.Builder
	b.WriteString(header)
	for _, s := range sections {
		if len(rows[s.dir]) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n\n| Identifier | Kind | Section | Note |\n|---|---|---|---|\n", s.title, s.intro)
		for _, r := range rows[s.dir] {
			b.WriteString(r + "\n")
		}
	}
	return b.String(), misses
}
