// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package connect

import (
	"net/url"
	"strings"

	"github.com/nats-io/nats.go"
)

// Connection is a server URL and the options that authenticate to it. Pass it
// to nats.Connect: nats.Connect(c.URL, c.Options...). URL never contains
// userinfo. See parity rows 1.4 and 1.5; outside SAP.
type Connection struct {
	// URL is one server URL or several joined by commas, as nats.Connect takes.
	URL string
	// Options carry the credentials, TLS settings and inbox prefix.
	Options []nats.Option
}

// urlAuth is what a URL list yields: bare servers and the userinfo credential.
type urlAuth struct {
	servers     []string
	token       string
	user, pass  string
	hasToken    bool
	hasUserPass bool
}

func (a urlAuth) options() []nats.Option {
	switch {
	case a.hasUserPass:
		return []nats.Option{nats.UserInfo(a.user, a.pass)}
	case a.hasToken:
		return []nats.Option{nats.Token(a.token)}
	}
	return nil
}

// ParseURL parses one NATS URL, or several separated by commas, and moves
// credentials in the userinfo into options, as the NATS CLI does:
//
//   - nats://host:port             no credentials
//   - nats://TOKEN@host:port       a single component is a token
//   - nats://USER:PASS@host:port   user and password (PASS may be empty)
//
// Userinfo is percent-decoded. Accepted schemes are nats, tls, ws and wss; a
// scheme-less entry is read as nats. With several entries the userinfo must be
// identical on all of them. A missing port is left to nats.go, which defaults
// it to 4222. Errors never quote any part of the input, so a malformed
// userinfo cannot leak through them; they name the entry's position instead.
// See parity row 1.5; outside SAP.
func ParseURL(raw string) (Connection, error) {
	a, err := parseURL(raw)
	if err != nil {
		return Connection{}, err
	}
	return Connection{URL: strings.Join(a.servers, ","), Options: a.options()}, nil
}

func parseURL(raw string) (urlAuth, error) {
	var out urlAuth
	var first *url.Userinfo
	n := 0
	for i, part := range strings.Split(raw, ",") {
		pos := i + 1 // 1-based entry position, the only locator an error gives
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		withScheme := part
		if !strings.Contains(part, "://") {
			withScheme = "nats://" + part
		}
		u, err := url.Parse(withScheme)
		if err != nil {
			// url.Error text includes the input, and the input may hold a
			// credential split by a comma: quote none of it.
			return out, ctxErr("", "invalid NATS URL in entry %d (the URL is not shown: it may hold credentials)", pos)
		}
		switch u.Scheme {
		case "nats", "tls", "ws", "wss":
		default:
			return out, ctxErr("", "unsupported scheme in NATS URL entry %d (want nats, tls, ws or wss)", pos)
		}
		if u.Hostname() == "" {
			return out, ctxErr("", "NATS URL entry %d is missing a host", pos)
		}
		if n == 0 {
			first = u.User
		} else if !sameUserinfo(first, u.User) {
			return out, ctxErr("", "NATS URL has mixed credentials across server entries (entry %d differs)", pos)
		}
		n++
		bare := *u
		bare.User = nil
		out.servers = append(out.servers, bare.String())
	}
	if n == 0 {
		return out, ctxErr("", "empty NATS URL")
	}
	if first != nil {
		if pw, set := first.Password(); set {
			out.user, out.pass, out.hasUserPass = first.Username(), pw, true
		} else if first.Username() != "" {
			out.token, out.hasToken = first.Username(), true
		}
	}
	return out, nil
}

func sameUserinfo(a, b *url.Userinfo) bool {
	if a == nil || b == nil {
		return a == b
	}
	pa, sa := a.Password()
	pb, sb := b.Password()
	return a.Username() == b.Username() && pa == pb && sa == sb
}

// displayURL is the form of raw that may be printed: the servers with no
// userinfo, or a placeholder when raw does not parse. It never echoes a
// credential, however malformed.
func displayURL(raw string) string {
	if raw == "" {
		return ""
	}
	a, err := parseURL(raw)
	if err != nil {
		return "[invalid URL]"
	}
	return strings.Join(a.servers, ",")
}
