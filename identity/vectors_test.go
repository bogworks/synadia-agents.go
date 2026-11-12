// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func readJSON(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // the test reads a fixture or temp path it built itself
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

type senderVector struct {
	ID    string `json:"id"`
	Note  string `json:"note"`
	Input struct {
		User       string `json:"user"`
		Seed       string `json:"seed"`
		Account    string `json:"account"`
		Subject    string `json:"subject"`
		Sub        string `json:"sub"`
		PayloadB64 string `json:"payload_b64"`
		TS         string `json:"ts"`
		Nonce      string `json:"nonce"`
		Name       string `json:"name"`
		Signed     bool   `json:"signed"`
	} `json:"input"`
	Expected struct {
		SignedInput     string `json:"signed_input"`
		Sig             string `json:"sig"`
		Header          string `json:"header"`
		HeaderBytes     int    `json:"header_bytes"`
		HeaderWireBytes int    `json:"header_wire_bytes"`
	} `json:"expected"`
}

// TestVectors_SenderVectors runs every vector of sender-vectors.json: the
// signer reproduces the exact header bytes, signed input, signature and framed
// length, and the parsed header verifies both live and stored (REQ-012).
//
// catalog: SAP-13.6.1-02 SAP-13.6.1-03 SAP-13.6.1-04 SAP-13.6.1-05 SAP-B.13-01
func TestVectors_SenderVectors(t *testing.T) {
	var file struct {
		FramingBytes int            `json:"header_framing_bytes"`
		Vectors      []senderVector `json:"vectors"`
	}
	readJSON(t, "sender-vectors.json", &file)
	if len(file.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	if file.FramingBytes != HeaderFramingBytes {
		t.Fatalf("framing bytes: file %d, constant %d", file.FramingBytes, HeaderFramingBytes)
	}
	for _, v := range file.Vectors {
		t.Run(v.ID, func(t *testing.T) {
			payload, err := base64.StdEncoding.DecodeString(v.Input.PayloadB64)
			if err != nil {
				t.Fatal(err)
			}
			id, err := NewAgentID(v.Input.Account, v.Input.User)
			if err != nil {
				t.Fatal(err)
			}
			var h SenderHeader
			if v.Input.Signed {
				signer, err := SignerFromSeed([]byte(v.Input.Seed))
				if err != nil {
					t.Fatal(err)
				}
				if signer.PublicKey() != v.Input.User {
					t.Fatalf("seed derives %s, vector says %s", signer.PublicKey(), v.Input.User)
				}
				sub := v.Input.Subject
				if v.Input.Sub != "" {
					sub = v.Input.Sub // an import renamed by the caller's own account (SAP §13.6.2)
				}
				h, err = SignHeader(SignOptions{Signer: signer, ID: id, Name: v.Input.Name, Subject: sub,
					Payload: payload, TS: v.Input.TS, Nonce: v.Input.Nonce})
				if err != nil {
					t.Fatal(err)
				}
				if got := string(SignedInput(h.Account, h.User, h.Sub, h.TS, h.Nonce, payload)); got != v.Expected.SignedInput {
					t.Errorf("signed input\n got %q\nwant %q", got, v.Expected.SignedInput)
				}
				if h.Sig != v.Expected.Sig {
					t.Errorf("sig\n got %s\nwant %s", h.Sig, v.Expected.Sig)
				}
			} else {
				h, err = ClaimHeader(id, v.Input.Name)
				if err != nil {
					t.Fatal(err)
				}
			}
			if got := h.Encode(); got != v.Expected.Header {
				t.Errorf("header\n got %s\nwant %s", got, v.Expected.Header)
			}
			if got := len(h.Encode()); got != v.Expected.HeaderBytes {
				t.Errorf("header_bytes: got %d want %d", got, v.Expected.HeaderBytes)
			}
			if got := h.WireBytes(); got != v.Expected.HeaderWireBytes {
				t.Errorf("header_wire_bytes: got %d want %d", got, v.Expected.HeaderWireBytes)
			}

			// The expected header, parsed from the wire form, verifies.
			parsed, err := ParseHeader(v.Expected.Header)
			if err != nil || parsed == nil {
				t.Fatalf("ParseHeader: %v", err)
			}
			if parsed.Encode() != v.Expected.Header {
				t.Errorf("parse then encode is not the identity")
			}
			arrival := v.Input.Subject
			if v.Input.Sub != "" {
				arrival = v.Input.Sub
			}
			msg := Message{Subject: arrival, Data: payload, Header: nats.Header{HeaderName: {v.Expected.Header}}}
			now, _ := time.Parse(time.RFC3339, v.Input.TS)
			live, err := VerifyLive(msg, LiveOptions{Now: func() time.Time { return now },
				Nonces: NewNonceSet(NonceSetOptions{Now: func() time.Time { return now }})})
			stored, serr := VerifyStored(msg, StoredOptions{})
			if v.Input.Signed {
				if err != nil || live == nil || live.Trust != TrustVerified || live.ID != id {
					t.Errorf("live verification: %+v %v", live, err)
				}
				if serr != nil || stored == nil || stored.Trust != TrustVerified {
					t.Errorf("stored verification: %+v %v", stored, serr)
				}
			} else {
				if err != nil || live == nil || live.Trust != TrustClaimed || live.ID != "" {
					t.Errorf("claim, live: %+v %v", live, err)
				}
				if serr != nil || stored == nil || stored.Trust != TrustClaimed {
					t.Errorf("claim, stored: %+v %v", stored, serr)
				}
			}
		})
	}
	t.Logf("sender-vectors.json: %d vectors", len(file.Vectors))
}

// TestVectors_IDSigVectors runs every vector of id-sig-vectors.json.
func TestVectors_IDSigVectors(t *testing.T) {
	var file struct {
		Vectors []struct {
			ID    string `json:"id"`
			Input struct {
				User          string `json:"user"`
				Seed          string `json:"seed"`
				Account       string `json:"account"`
				Agent         string `json:"agent"`
				Owner         string `json:"owner"`
				PromptSubject string `json:"prompt_subject"`
			} `json:"input"`
			Expected struct {
				SignedInput string            `json:"signed_input"`
				IDSig       string            `json:"id_sig"`
				Metadata    map[string]string `json:"metadata"`
			} `json:"expected"`
		} `json:"vectors"`
	}
	readJSON(t, "id-sig-vectors.json", &file)
	if len(file.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, v := range file.Vectors {
		t.Run(v.ID, func(t *testing.T) {
			in := v.Input
			signer, err := SignerFromSeed([]byte(in.Seed))
			if err != nil {
				t.Fatal(err)
			}
			id, err := NewAgentID(in.Account, in.User)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(AgentIDSignedInput(in.User, in.Account, in.Agent, in.Owner, in.PromptSubject)); got != v.Expected.SignedInput {
				t.Errorf("signed input\n got %q\nwant %q", got, v.Expected.SignedInput)
			}
			sig, err := SignAgentID(signer, id, in.Agent, in.Owner, in.PromptSubject)
			if err != nil {
				t.Fatal(err)
			}
			if sig != v.Expected.IDSig {
				t.Errorf("id_sig\n got %s\nwant %s", sig, v.Expected.IDSig)
			}
			if !VerifyAgentID(v.Expected.Metadata, in.PromptSubject) {
				t.Error("the vector's metadata does not verify")
			}
			meta, err := RegistrationMetadata(signer, id, in.Agent, in.Owner, in.PromptSubject)
			if err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{MetadataKeyUserNKey, MetadataKeyAccount, MetadataKeyIDSig} {
				if meta[k] != v.Expected.Metadata[k] {
					t.Errorf("registration metadata %s: got %q want %q", k, meta[k], v.Expected.Metadata[k])
				}
			}
			if VerifyAgentID(v.Expected.Metadata, in.PromptSubject+"x") {
				t.Error("id_sig verified over a different prompt subject")
			}
		})
	}
	t.Logf("id-sig-vectors.json: %d vectors", len(file.Vectors))
}

// TestVectors_AgentIDFixtures runs every parse row and every new() row of
// agent-id-fixtures.json.
//
// catalog: SAP-13.3.3-01
func TestVectors_AgentIDFixtures(t *testing.T) {
	var file struct {
		Parse []struct {
			Input   string `json:"input"`
			Valid   bool   `json:"valid"`
			Account string `json:"account"`
			User    string `json:"user"`
			Length  int    `json:"length"`
			Note    string `json:"note"`
		} `json:"parse"`
		New []struct {
			Account string `json:"account"`
			User    string `json:"user"`
			Valid   bool   `json:"valid"`
			String  string `json:"string"`
			Note    string `json:"note"`
		} `json:"new"`
	}
	readJSON(t, "agent-id-fixtures.json", &file)
	if len(file.Parse) == 0 || len(file.New) == 0 {
		t.Fatal("no fixtures")
	}
	for i, p := range file.Parse {
		t.Run("parse/"+strconv.Itoa(i), func(t *testing.T) {
			id, err := ParseAgentID(p.Input)
			if p.Valid {
				if err != nil {
					t.Fatalf("%s: %v", p.Note, err)
				}
				if id.Account() != p.Account || id.User() != p.User || string(id) != p.Input {
					t.Errorf("got %q/%q", id.Account(), id.User())
				}
				if p.Length != 0 && len(id) != p.Length {
					t.Errorf("length %d, want %d", len(id), p.Length)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted %q (%s)", p.Input, p.Note)
			}
			if !errors.Is(err, ErrInvalidAgentID) {
				t.Errorf("error does not match ErrInvalidAgentID: %v", err)
			}
		})
	}
	for i, n := range file.New {
		t.Run("new/"+strconv.Itoa(i), func(t *testing.T) {
			id, err := NewAgentID(n.Account, n.User)
			if n.Valid {
				if err != nil {
					t.Fatalf("%s: %v", n.Note, err)
				}
				if string(id) != n.String {
					t.Errorf("got %q want %q", id, n.String)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted (%s)", n.Note)
			}
		})
	}
	t.Logf("agent-id-fixtures.json: %d parse rows, %d new rows", len(file.Parse), len(file.New))
}

// TestVectors_KeysDeriveTheirPublicKeys pins keys.json: every seed derives its
// public key, so the committed configs and vectors stay consistent.
func TestVectors_KeysDeriveTheirPublicKeys(t *testing.T) {
	var file struct {
		Users map[string]struct {
			Public string `json:"public"`
			Seed   string `json:"seed"`
		} `json:"users"`
	}
	readJSON(t, "keys.json", &file)
	if len(file.Users) != 5 {
		t.Fatalf("got %d users, want 5", len(file.Users))
	}
	for name, u := range file.Users {
		s, err := SignerFromSeed([]byte(u.Seed))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if s.PublicKey() != u.Public {
			t.Errorf("%s: seed derives %s, file says %s", name, s.PublicKey(), u.Public)
		}
	}
}
