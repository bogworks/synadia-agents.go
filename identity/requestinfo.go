// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"encoding/json"
	"errors"

	"github.com/nats-io/nats.go"
)

// RequestInfoHeader is the header the server stamps on a request that crosses
// an account boundary through a service import with share: true. It is always
// a claim: within one account the server forwards a client-written value
// unchanged. See SAP §13.1.
const RequestInfoHeader = "Nats-Request-Info"

// ErrBadRequestInfo is returned (wrapped) when a Nats-Request-Info header is not
// what the server writes: not a JSON object, an object that repeats a member name, acc or user not a string, or more
// than one value.
var ErrBadRequestInfo = errors.New("identity: malformed Nats-Request-Info header")

// RequestInfo holds the two identity fields of a Nats-Request-Info stamp.
// Everything else in the stamp is ignored. See SAP §13.7.2.
type RequestInfo struct {
	// Account is acc: the caller's account as the server names it. Empty when
	// the field is absent.
	Account string
	// User is user: the caller's user public key, present only behind a
	// share: true import. Empty when absent.
	User string
}

// ParseRequestInfo parses a Nats-Request-Info value. Unknown fields (rtt,
// server, jwt, issuer_key and others) are ignored. There is deliberately no size
// cap: behind a share: true import the stamp embeds the caller's whole user JWT,
// and the server's max_payload bounds it.
func ParseRequestInfo(value string) (RequestInfo, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &obj); err != nil || obj == nil {
		return RequestInfo{}, ErrBadRequestInfo
	}
	if checkNoDuplicateKeys([]byte(value)) != nil {
		return RequestInfo{}, ErrBadRequestInfo
	}
	var info RequestInfo
	for key, dst := range map[string]*string{"acc": &info.Account, "user": &info.User} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, dst) != nil {
			return RequestInfo{}, ErrBadRequestInfo
		}
	}
	return info, nil
}

// ReadRequestInfo reads the stamp from message headers. It returns (nil, nil)
// when the header is absent, and [ErrBadRequestInfo] when it is present but
// malformed or present more than once, which the server never produces.
func ReadRequestInfo(hdr nats.Header) (*RequestInfo, error) {
	vals := hdr[RequestInfoHeader]
	switch len(vals) {
	case 0:
		return nil, nil
	case 1:
		info, err := ParseRequestInfo(vals[0])
		if err != nil {
			return nil, err
		}
		return &info, nil
	default:
		return nil, ErrBadRequestInfo
	}
}
