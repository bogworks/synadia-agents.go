// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"
)

// field is one member of a JSON object under construction.
type field struct {
	name    string
	value   any
	present bool
}

// marshalNoEscape is json.Marshal without HTML escaping, so the encoded size is
// the one a peer that does not escape (the official SDKs) would produce.
func marshalNoEscape(v any) ([]byte, error) {
	if str, ok := v.(string); ok && !utf8.ValidString(str) {
		return nil, ErrInvalidUTF8
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// marshalObject writes the present fields in order, then the extras sorted by
// key. An extra whose key is in owned is never written: the protocol's own
// fields win a clash. Every extra value must be exactly one valid JSON value
// and every key valid UTF-8, or the call fails rather than emit bad JSON; an
// invalid UTF-8 string value fails with [ErrInvalidUTF8].
func marshalObject(fields []field, extra map[string]json.RawMessage, owned ...string) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	write := func(name string, raw []byte) error {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		k, err := marshalNoEscape(name)
		if err != nil {
			return err
		}
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(raw)
		return nil
	}
	own := map[string]bool{}
	for _, f := range fields {
		if !f.present {
			continue
		}
		own[f.name] = true
		raw, err := marshalNoEscape(f.value)
		if err != nil {
			return nil, err
		}
		if err := write(f.name, raw); err != nil {
			return nil, err
		}
	}
	for _, o := range owned {
		own[o] = true
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		if !own[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		raw := extra[k]
		if !utf8.ValidString(k) {
			return nil, ErrInvalidUTF8
		}
		if !json.Valid(raw) {
			return nil, fmt.Errorf("%w: extra field %q is not exactly one valid JSON value", ErrInvalidExtra, k)
		}
		if err := write(k, raw); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// splitObject parses data as exactly one JSON object and returns its members
// undecoded.
func splitObject(data []byte) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("not a JSON object")
	}
	return obj, nil
}

// stringField reads a JSON string member; ok is false when the member is
// absent, null, not a string, or not valid UTF-8.
func stringField(obj map[string]json.RawMessage, key string) (s string, ok bool) {
	raw, present := obj[key]
	if !present {
		return "", false
	}
	return rawString(raw)
}

// rawString decodes raw as a JSON string. Unlike json.Unmarshal into a string
// it rejects null, and it rejects bytes that are not valid UTF-8 rather than
// letting the decoder replace them.
func rawString(raw json.RawMessage) (s string, ok bool) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || t[0] != '"' || !utf8.Valid(t) {
		return "", false
	}
	if err := json.Unmarshal(t, &s); err != nil {
		return "", false
	}
	return s, true
}

// withoutKeys copies obj without the listed keys; nil when nothing remains.
func withoutKeys(obj map[string]json.RawMessage, keys ...string) map[string]json.RawMessage {
	var out map[string]json.RawMessage
	for k, v := range obj {
		skip := false
		for _, o := range keys {
			if k == o {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		if out == nil {
			out = map[string]json.RawMessage{}
		}
		out[k] = v
	}
	return out
}

// isJSONNull reports whether raw is the JSON literal null.
func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}
