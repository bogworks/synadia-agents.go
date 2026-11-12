// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package tools_test

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// This file is a copy of tools/schema_test.go (the test module cannot import another
// package's test files); keep the two in step.
//
// A small JSON Schema 2020-12 checker, enough for the agent-tools result
// fixtures (the root module may use only the standard library). It supports
// the keywords those schemas use: type, enum, const, required, properties,
// additionalProperties, items, minItems, minLength, minimum, pattern, format
// (date-time), allOf, anyOf, oneOf, not, if/then/else, dependentRequired,
// dependentSchemas and $ref to a sibling file. Another keyword is an error,
// so a fixture that grows one cannot pass unchecked.
type schemaSet struct {
	mu   sync.Mutex
	dir  string
	docs map[string]any
}

func newSchemaSet(dir string) *schemaSet { return &schemaSet{dir: dir, docs: map[string]any{}} }

func (s *schemaSet) load(name string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.docs[name]; ok {
		return d, nil
	}
	raw, err := os.ReadFile(filepath.Join(s.dir, name)) //nolint:gosec // the test reads a fixture or temp path it built itself
	if err != nil {
		return nil, err
	}
	var d any
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	s.docs[name] = d
	return d, nil
}

var knownKeywords = map[string]bool{
	"$schema": true, "title": true, "description": true, "type": true, "enum": true, "const": true,
	"required": true, "properties": true, "additionalProperties": true, "items": true,
	"minItems": true, "minLength": true, "minimum": true, "pattern": true, "format": true,
	"allOf": true, "anyOf": true, "oneOf": true, "not": true, "if": true, "then": true,
	"else": true, "dependentRequired": true, "dependentSchemas": true, "$ref": true,
}

// validate returns the violations of value v against the schema file name.
func (s *schemaSet) validate(name string, v any) ([]string, error) {
	doc, err := s.load(name)
	if err != nil {
		return nil, err
	}
	var errs []string
	var walkErr error
	s.check(doc, v, "$", &errs, &walkErr)
	return errs, walkErr
}

func (s *schemaSet) valid(schema, v any, walkErr *error) bool {
	var errs []string
	s.check(schema, v, "$", &errs, walkErr)
	return len(errs) == 0
}

func (s *schemaSet) check(schema, v any, path string, errs *[]string, walkErr *error) {
	bad := func(format string, a ...any) { *errs = append(*errs, path+": "+fmt.Sprintf(format, a...)) }
	switch sc := schema.(type) {
	case bool:
		if !sc {
			bad("schema is false")
		}
		return
	case map[string]any:
		s.checkObject(sc, v, path, errs, walkErr, bad)
	default:
		*walkErr = fmt.Errorf("unsupported schema %T", schema)
	}
}

func (s *schemaSet) checkObject(sc map[string]any, v any, path string, errs *[]string, walkErr *error, bad func(string, ...any)) {
	for k := range sc {
		if !knownKeywords[k] {
			*walkErr = fmt.Errorf("schema keyword %q is not supported by the test checker", k)
		}
	}
	if ref, ok := sc["$ref"].(string); ok {
		d, err := s.load(ref)
		if err != nil {
			*walkErr = err
		} else {
			s.check(d, v, path, errs, walkErr)
		}
	}
	if t, ok := sc["type"]; ok && !typeMatches(t, v) {
		bad("type %v wanted, got %T", t, v)
		return
	}
	if e, ok := sc["enum"].([]any); ok {
		found := false
		for _, x := range e {
			found = found || reflect.DeepEqual(x, v)
		}
		if !found {
			bad("%v is not one of %v", v, e)
		}
	}
	if c, ok := sc["const"]; ok && !reflect.DeepEqual(c, v) {
		bad("%v is not %v", v, c)
	}
	switch x := v.(type) {
	case string:
		if n, ok := sc["minLength"].(float64); ok && float64(len([]rune(x))) < n {
			bad("shorter than %v", n)
		}
		if p, ok := sc["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(x) {
			bad("%q does not match %s", x, p)
		}
		if f, ok := sc["format"].(string); ok && f == "date-time" {
			if _, err := time.Parse(time.RFC3339, x); err != nil {
				bad("%q is not a date-time", x)
			}
		}
	case float64:
		if m, ok := sc["minimum"].(float64); ok && x < m {
			bad("%v is below %v", x, m)
		}
	case []any:
		if n, ok := sc["minItems"].(float64); ok && float64(len(x)) < n {
			bad("fewer than %v items", n)
		}
		if it, ok := sc["items"]; ok {
			for i, e := range x {
				s.check(it, e, fmt.Sprintf("%s[%d]", path, i), errs, walkErr)
			}
		}
	case map[string]any:
		s.checkMap(sc, x, path, errs, walkErr, bad)
	}
	for _, k := range []string{"allOf", "anyOf", "oneOf"} {
		list, ok := sc[k].([]any)
		if !ok {
			continue
		}
		matches := 0
		for _, sub := range list {
			if s.valid(sub, v, walkErr) {
				matches++
			}
			if k == "allOf" {
				s.check(sub, v, path, errs, walkErr)
			}
		}
		if k == "anyOf" && matches == 0 {
			bad("matches none of anyOf")
		}
		if k == "oneOf" && matches != 1 {
			bad("matches %d of oneOf, want exactly 1", matches)
		}
	}
	if n, ok := sc["not"]; ok && s.valid(n, v, walkErr) {
		bad("matches a schema it must not")
	}
	if cond, ok := sc["if"]; ok {
		if s.valid(cond, v, walkErr) {
			if th, ok := sc["then"]; ok {
				s.check(th, v, path, errs, walkErr)
			}
		} else if el, ok := sc["else"]; ok {
			s.check(el, v, path, errs, walkErr)
		}
	}
}

func (s *schemaSet) checkMap(sc map[string]any, x map[string]any, path string, errs *[]string, walkErr *error, bad func(string, ...any)) {
	if req, ok := sc["required"].([]any); ok {
		for _, r := range req {
			if _, has := x[r.(string)]; !has {
				bad("missing %q", r)
			}
		}
	}
	props, _ := sc["properties"].(map[string]any)
	keys := make([]string, 0, len(x))
	for k := range x {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if p, ok := props[k]; ok {
			s.check(p, x[k], path+"."+k, errs, walkErr)
		} else if ap, ok := sc["additionalProperties"]; ok {
			if b, isBool := ap.(bool); isBool && !b {
				bad("unexpected property %q", k)
			} else if !isBool {
				s.check(ap, x[k], path+"."+k, errs, walkErr)
			}
		}
	}
	if dr, ok := sc["dependentRequired"].(map[string]any); ok {
		for k, need := range dr {
			if _, has := x[k]; !has {
				continue
			}
			for _, n := range need.([]any) {
				if _, has := x[n.(string)]; !has {
					bad("%q needs %q", k, n)
				}
			}
		}
	}
	if ds, ok := sc["dependentSchemas"].(map[string]any); ok {
		for k, sub := range ds {
			if _, has := x[k]; has {
				s.check(sub, x, path, errs, walkErr)
			}
		}
	}
}

func typeMatches(t any, v any) bool {
	var names []string
	switch tt := t.(type) {
	case string:
		names = []string{tt}
	case []any:
		for _, n := range tt {
			names = append(names, n.(string))
		}
	}
	for _, n := range names {
		switch n {
		case "object":
			if _, ok := v.(map[string]any); ok {
				return true
			}
		case "array":
			if _, ok := v.([]any); ok {
				return true
			}
		case "string":
			if _, ok := v.(string); ok {
				return true
			}
		case "boolean":
			if _, ok := v.(bool); ok {
				return true
			}
		case "null":
			if v == nil {
				return true
			}
		case "integer":
			if f, ok := v.(float64); ok && f == math.Trunc(f) {
				return true
			}
		case "number":
			if _, ok := v.(float64); ok {
				return true
			}
		}
	}
	return false
}

// normalize turns a Go result into the generic JSON value a model would see.
func normalize(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func joinErrs(e []string) string { return strings.Join(e, "; ") }
