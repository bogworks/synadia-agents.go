// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// errDuplicateKey is returned by checkNoDuplicateKeys for an object that
// repeats a member name.
var errDuplicateKey = errors.New("identity: JSON object repeats a member name")

// checkNoDuplicateKeys reports errDuplicateKey when any object in the JSON text,
// at any depth, repeats a member name. Names are compared after decoding, so
// "account" and "account" are the same name. encoding/json keeps the last
// value of a repeated name while a first-value parser keeps the first, so a
// repeated name lets two components read different identities from one value.
// Text that is not valid JSON returns the decoder's error.
func checkNoDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := scanValue(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("identity: trailing data after JSON value")
	}
	return nil
}

func scanValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := kt.(string) // an object key is always a string token
			if _, dup := seen[key]; dup {
				return errDuplicateKey
			}
			seen[key] = struct{}{}
			if err := scanValue(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := scanValue(dec); err != nil {
				return err
			}
		}
	}
	_, err = dec.Token() // the closing delimiter
	return err
}
