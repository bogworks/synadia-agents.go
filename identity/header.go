// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nats-io/nats.go"
)

const (
	// HeaderName is the name of the sender header. It is matched
	// case-sensitively by this package. See SAP §13.5.1.
	HeaderName = "Agent-Sender"

	// HeaderVersion is the header format version this package implements: the
	// JSON number 1. See SAP §13.5.
	HeaderVersion = 1

	// SignedInputTag is the first line of the signed input of an Agent-Sender
	// signature. See SAP §13.6.1.
	SignedInputTag = "AGENT-SENDER-V1"

	// HeaderFramingBytes is the framing the server counts against max_payload
	// for one Agent-Sender header: "NATS/1.0\r\nAgent-Sender: " plus
	// "\r\n\r\n". See SAP §13.5.1.
	HeaderFramingBytes = 28

	// MaxHeaderValueBytes is the cap this package applies to a header value
	// before parsing it (SDK policy; a signed header is about 400 bytes).
	MaxHeaderValueBytes = 2048

	// MaxNameLength is the cap on the display name, in UTF-16 code units (SDK
	// policy, the same rule as the official SDKs; the name is never identity).
	MaxNameLength = 64

	// MaxNonceLength is the longest nonce SAP §13.5 allows.
	MaxNonceLength = 64

	// DefaultReplayWindow is the default timestamp skew and nonce window, 30
	// seconds. See SAP §13.7.
	DefaultReplayWindow = 30 * time.Second

	nuidLength      = 22
	tsLength        = 20 // 2006-01-02T15:04:05Z
	sigBase64Length = 86
	userKeyLength   = 56
	timestampLayout = "2006-01-02T15:04:05Z"
)

var (
	tsRegexp    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$`)
	nonceRegexp = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// SenderHeader is a parsed or built Agent-Sender header. Only the known fields
// are kept; unknown fields are ignored on parse. See SAP §13.5.
//
// A header with an empty Sig is a claim. A header with a Sig always carries Sub,
// TS and Nonce.
type SenderHeader struct {
	// V is the header format version, always [HeaderVersion].
	V int
	// Account is the caller's account (SAP §13.3).
	Account string
	// User is the caller's user public NKEY.
	User string
	// Name is a display name. It is never identity and is outside the signature
	// by design. Empty means no name.
	Name string
	// Sub is the subject the caller published to, byte for byte, with the
	// exception of SAP §13.6.2. Empty on a claim.
	Sub string
	// TS is the request time, RFC 3339 UTC. Empty on a claim.
	TS string
	// Nonce is unique per request from this user, 1 to 64 characters from
	// [A-Za-z0-9_-]. Empty on a claim.
	Nonce string
	// Sig is the base64url signature without padding. Empty on a claim.
	Sig string
}

// Signed reports whether the header carries a signature.
func (h SenderHeader) Signed() bool { return h.Sig != "" }

// ID returns the agent ID of the header's account and user. It returns an
// error when either half is invalid. Note that the ID of an unsigned header is
// a claim.
func (h SenderHeader) ID() (AgentID, error) { return NewAgentID(h.Account, h.User) }

// String returns a form safe for logs: the nonce and signature are redacted.
func (h SenderHeader) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "AgentSenderHeader{account:%s user:%s", h.Account, h.User)
	if h.Name != "" {
		fmt.Fprintf(&b, " name:%q", h.Name)
	}
	if h.Sub != "" {
		fmt.Fprintf(&b, " sub:%s", h.Sub)
	}
	if h.TS != "" {
		fmt.Fprintf(&b, " ts:%s", h.TS)
	}
	if h.Nonce != "" {
		b.WriteString(" nonce:[redacted]")
	}
	if h.Sig != "" {
		b.WriteString(" sig:[redacted]")
	}
	b.WriteString("}")
	return b.String()
}

// LogValue implements [slog.LogValuer]; the nonce and signature are redacted.
func (h SenderHeader) LogValue() slog.Value { return slog.StringValue(h.String()) }

// wireHeader fixes the field order of the canonical serialization.
type wireHeader struct {
	V       int    `json:"v"`
	Account string `json:"account"`
	User    string `json:"user"`
	Name    string `json:"name,omitempty"`
	Sub     string `json:"sub,omitempty"`
	TS      string `json:"ts,omitempty"`
	Nonce   string `json:"nonce,omitempty"`
	Sig     string `json:"sig,omitempty"`
}

// Encode returns the header value in its canonical wire form: compact JSON on
// one line, fields in the order v, account, user, name, sub, ts, nonce, sig,
// absent fields omitted, non-ASCII written raw. See SAP §13.5.1.
func (h SenderHeader) Encode() string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Marshaling a struct of strings and one int cannot fail.
	_ = enc.Encode(wireHeader{
		V: HeaderVersion, Account: h.Account, User: h.User, Name: h.Name,
		Sub: h.Sub, TS: h.TS, Nonce: h.Nonce, Sig: h.Sig,
	})
	return strings.TrimSuffix(buf.String(), "\n")
}

// WireBytes returns the size the header adds to a message on the wire: the
// framing plus the encoded value. Callers include it in the local max_payload
// check. See SAP §13.5.1.
func (h SenderHeader) WireBytes() int { return HeaderFramingBytes + len(h.Encode()) }

// Set stores the header on a NATS header map as one Agent-Sender value,
// replacing any earlier value.
func (h SenderHeader) Set(hdr nats.Header) { hdr[HeaderName] = []string{h.Encode()} }

// FormatTimestamp formats t as the header ts: RFC 3339 UTC with second
// precision and the Z suffix, as signers SHOULD emit (SAP §13.5).
func FormatTimestamp(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(timestampLayout)
}

// ParseTimestamp parses a header ts. Fractional seconds are accepted, as SAP
// §13.5 requires of verifiers.
func ParseTimestamp(ts string) (time.Time, error) {
	if !tsRegexp.MatchString(ts) {
		return time.Time{}, fmt.Errorf("identity: ts %q is not an RFC 3339 UTC timestamp", ts)
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, fmt.Errorf("identity: ts is not an RFC 3339 UTC timestamp: %w", err)
	}
	return t, nil
}

// ValidNonce reports whether nonce fits the header grammar, [A-Za-z0-9_-]{1,64}
// (SAP §13.5).
func ValidNonce(nonce string) bool { return nonceRegexp.MatchString(nonce) }

// ValidName reports whether name is acceptable as a display name: at most
// [MaxNameLength] UTF-16 code units, valid UTF-8, and no C0 or C1 control
// character, DEL, U+2028 or U+2029 (SDK policy).
func ValidName(name string) bool {
	if !utf8.ValidString(name) {
		return false
	}
	units := 0
	for _, r := range name {
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029 {
			return false
		}
		if r > 0xffff {
			units += 2
		} else {
			units++
		}
	}
	return units <= MaxNameLength
}

// validSubject reports whether s has the shape of a NATS subject: non-empty
// tokens separated by '.', with no whitespace or control character. U+FEFF is
// refused too: JavaScript's and Python's \s (the upstream SUBJECT_REGEX,
// client-sdk/typescript/src/identity/sender-header.ts:60 and
// client-sdk/python/.../identity/sender_header.py:74) match it, unicode.IsSpace
// does not.
func validSubject(s string) bool {
	if s == "" {
		return false
	}
	for _, tok := range strings.Split(s, ".") {
		if tok == "" {
			return false
		}
		for _, r := range tok {
			if unicode.IsSpace(r) || r == 0xFEFF || r < 0x20 || r == 0x7f {
				return false
			}
		}
	}
	return utf8.ValidString(s)
}

// ReadHeaderValue reads the raw Agent-Sender value from NATS headers. The
// match is on the exact name "Agent-Sender". It reports ok=false when the
// header is absent. More than one value is malformed (400).
//
// NATS clients canonicalise header names when they decode a message, so a
// differently cased name on the wire arrives here as "Agent-Sender". That is
// a property of the client library, not of this package.
func ReadHeaderValue(hdr nats.Header) (value string, ok bool, err error) {
	vals := hdr[HeaderName]
	switch len(vals) {
	case 0:
		return "", false, nil
	case 1:
		return vals[0], true, nil
	default:
		return "", false, malformed("more than one Agent-Sender value")
	}
}

// ParseHeader parses an Agent-Sender header value. It returns (nil, nil) for a
// well-formed header with an unknown v: the receiver treats that as absent
// (SAP §13.5.1). A malformed header returns an *[Error] with code 400 that
// matches [ErrMalformedHeader]. Malformed means: not valid JSON; an object that repeats a member name (also
// under an escaped spelling); lacks v,
// account or user; a field of the wrong shape; or sig without all of sub, ts
// and nonce. v is the number 1, not the string "1". See SAP §13.5.1.
//
// The signature must be base64url without padding and decode to 64 bytes.
func ParseHeader(value string) (*SenderHeader, error) {
	if len(value) > MaxHeaderValueBytes {
		return nil, malformed(fmt.Sprintf("value exceeds %d bytes", MaxHeaderValueBytes))
	}
	if !utf8.ValidString(value) {
		return nil, malformed("value is not valid UTF-8")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &obj); err != nil || obj == nil {
		return nil, malformed("not a JSON object")
	}
	if err := checkNoDuplicateKeys([]byte(value)); err != nil {
		return nil, malformed("a member name is repeated")
	}

	rawV, ok := obj["v"]
	if !ok {
		return nil, malformed("`v` is missing")
	}
	var v json.Number
	if len(rawV) == 0 || (rawV[0] != '-' && (rawV[0] < '0' || rawV[0] > '9')) {
		return nil, malformed("`v` must be a JSON number")
	}
	if err := json.Unmarshal(rawV, &v); err != nil {
		return nil, malformed("`v` must be a JSON number")
	}
	if f, err := v.Float64(); err != nil || f != HeaderVersion {
		if err != nil {
			return nil, malformed("`v` must be a JSON number")
		}
		return nil, nil // unknown version: absent
	}

	str := func(key string, required bool) (string, bool, error) {
		raw, ok := obj[key]
		if !ok {
			if required {
				return "", false, malformed("`" + key + "` is missing")
			}
			return "", false, nil
		}
		var s string
		if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
			return "", false, malformed("`" + key + "` must be a string")
		}
		return s, true, nil
	}

	h := &SenderHeader{V: HeaderVersion}
	var err error
	if h.Account, _, err = str("account", true); err != nil {
		return nil, err
	}
	if h.User, _, err = str("user", true); err != nil {
		return nil, err
	}
	if _, err = NewAgentID(h.Account, h.User); err != nil {
		return nil, malformed(err.Error())
	}

	// encoding/json turns a lone-surrogate escape into U+FFFD silently. The
	// official SDKs reject a lone surrogate in `name` only (TS
	// sender-header.ts:152-171 assertValidSenderName, Python
	// sender_header.py:189-203); SAP names no such rule. So the check is
	// scoped to `name` and no stricter than they are.
	if raw, ok := obj["name"]; ok && hasLoneSurrogateEscape(string(raw)) {
		return nil, malformed("`name` contains a lone surrogate escape")
	}
	var present bool
	if h.Name, present, err = str("name", false); err != nil {
		return nil, err
	} else if present && !ValidName(h.Name) {
		return nil, malformed("`name` is not a valid display name")
	}
	if h.Sub, present, err = str("sub", false); err != nil {
		return nil, err
	} else if present && !validSubject(h.Sub) {
		return nil, malformed("`sub` is not a NATS subject")
	}
	if h.TS, present, err = str("ts", false); err != nil {
		return nil, err
	} else if present {
		if _, perr := ParseTimestamp(h.TS); perr != nil {
			return nil, malformed("`ts` is not an RFC 3339 UTC timestamp")
		}
	}
	if h.Nonce, present, err = str("nonce", false); err != nil {
		return nil, err
	} else if present && !ValidNonce(h.Nonce) {
		return nil, malformed("`nonce` must match [A-Za-z0-9_-]{1,64}")
	}
	if h.Sig, present, err = str("sig", false); err != nil {
		return nil, err
	} else if present {
		if _, derr := decodeSignature(h.Sig); derr != nil {
			return nil, malformed("`sig` is not base64url without padding that decodes to 64 bytes")
		}
		if h.Sub == "" || h.TS == "" || h.Nonce == "" {
			return nil, malformed("`sig` requires `sub`, `ts` and `nonce`")
		}
	}
	return h, nil
}

// SignedInput returns the exact bytes a caller signs and a receiver rebuilds:
//
//	AGENT-SENDER-V1\n{account}\n{user}\n{subject}\n{ts}\n{nonce}\n{sha256(payload) hex}\n
//
// Every line, the last included, ends with one LF byte. The input is never
// sent. See SAP §13.6.1.
func SignedInput(account, user, subject, ts, nonce string, payload []byte) []byte {
	var b bytes.Buffer
	b.WriteString(SignedInputTag)
	for _, f := range []string{account, user, subject, ts, nonce, sha256Hex(payload)} {
		b.WriteByte('\n')
		b.WriteString(f)
	}
	b.WriteByte('\n')
	return b.Bytes()
}

// MaxHeaderWireBytes returns an upper bound on the framed size of any header a
// signer would send for subject and name: the JSON-encoded length of the actual
// subject and name (escapes for quotes, backslashes and control characters
// counted), the fixed widths of user (56), ts (20), nonce (up to 64) and sig
// (86), a [MaxAccountLen] allowance for account, and the framing. It is sound
// for the local max_payload check when the identity is not yet known and its
// account is an NKEY or a name of at most [MaxAccountLen] bytes; a caller with a
// longer account name uses [ExpectedHeaderWireBytes]. See SAP §13.5.1.
func MaxHeaderWireBytes(subject, name string) int {
	return SenderHeader{
		V: HeaderVersion, Account: strings.Repeat("A", MaxAccountLen), User: strings.Repeat("U", userKeyLength),
		Name: name, Sub: subject, TS: strings.Repeat("T", tsLength),
		Nonce: strings.Repeat("N", MaxNonceLength), Sig: strings.Repeat("S", sigBase64Length),
	}.WireBytes()
}

// ExpectedHeaderWireBytes returns the exact framed size of the header this
// package sends for a known identity. signed selects a signed header (with a
// 22-character nonce) over a claim. See SAP §13.5.1.
func ExpectedHeaderWireBytes(id AgentID, name, subject string, signed bool) int {
	h := SenderHeader{V: HeaderVersion, Account: id.Account(), User: id.User(), Name: name}
	if signed {
		h.Sub = subject
		h.TS = strings.Repeat("T", tsLength)
		h.Nonce = strings.Repeat("N", nuidLength)
		h.Sig = strings.Repeat("S", sigBase64Length)
	}
	return h.WireBytes()
}

// hasLoneSurrogateEscape reports whether a JSON string in value contains a \uD800-\uDFFF
// escape that is not half of a valid high-then-low pair. Escapes outside
// strings do not exist in valid JSON, and an invalid value fails later parsing.
func hasLoneSurrogateEscape(value string) bool {
	hex4 := func(i int) (rune, bool) {
		if i+4 > len(value) {
			return 0, false
		}
		var r rune
		for _, c := range value[i : i+4] {
			switch {
			case c >= '0' && c <= '9':
				r = r<<4 | (c - '0')
			case c >= 'a' && c <= 'f':
				r = r<<4 | (c - 'a' + 10)
			case c >= 'A' && c <= 'F':
				r = r<<4 | (c - 'A' + 10)
			default:
				return 0, false
			}
		}
		return r, true
	}
	inString := false
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !inString {
			inString = c == '"'
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			i++
			if i >= len(value) || value[i] != 'u' {
				continue
			}
			r, ok := hex4(i + 1)
			if !ok {
				continue
			}
			i += 4
			switch {
			case r >= 0xDC00 && r <= 0xDFFF:
				return true
			case r >= 0xD800 && r <= 0xDBFF:
				if i+2 < len(value) && value[i+1] == '\\' && value[i+2] == 'u' {
					if lo, ok := hex4(i + 3); ok && lo >= 0xDC00 && lo <= 0xDFFF {
						i += 6
						continue
					}
				}
				return true
			}
		}
	}
	return false
}
