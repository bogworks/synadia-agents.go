// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "attachments", name)) //nolint:gosec // the test reads a fixture or temp path it built itself
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// REQ-006, SAP:L272 (§5.2): the official strict-base64 vectors (testdata/attachments/SOURCE.md).
func TestAttachment_Base64Fixtures(t *testing.T) {
	var fx struct {
		Cases []struct {
			Content string `json:"content"`
			Valid   bool   `json:"valid"`
			Hex     string `json:"hex"`
			Note    string `json:"note"`
		} `json:"cases"`
	}
	readFixture(t, "base64-content.json", &fx)
	if len(fx.Cases) < 10 {
		t.Fatalf("fixture too small: %d", len(fx.Cases))
	}
	for _, c := range fx.Cases {
		size, ok := Base64DecodedSize(c.Content)
		if ok != c.Valid {
			t.Errorf("%q (%s): valid=%v want %v", c.Content, c.Note, ok, c.Valid)
			continue
		}
		a := Attachment{Filename: "f", Content: c.Content}
		got, err := a.Bytes()
		if !c.Valid {
			if err == nil {
				t.Errorf("%q (%s): Bytes accepted it", c.Content, c.Note)
			}
			continue
		}
		if err != nil || hex.EncodeToString(got) != c.Hex || size != len(got) {
			t.Errorf("%q (%s): %x %v size=%d want hex %s", c.Content, c.Note, got, err, size, c.Hex)
		}
	}
}

// REQ-006: what the SDK encodes, the strict decoder reads back, for every length.
func TestAttachment_EncodeIsStrictBase64(t *testing.T) {
	for n := 0; n < 64; n++ {
		data := make([]byte, n)
		for i := range data {
			data[i] = byte((i*37 + 250) % 256)
		}
		a := NewAttachment("f", data)
		if strings.ContainsAny(a.Content, "-_ \r\n") {
			t.Fatalf("n=%d: not the standard alphabet: %q", n, a.Content)
		}
		if got, err := a.Bytes(); err != nil || string(got) != string(data) {
			t.Fatalf("n=%d: %v", n, err)
		}
		if a.Content != base64.StdEncoding.EncodeToString(data) {
			t.Fatal("not padded standard base64")
		}
	}
}

// SAP:L272 (§5.2): the vectors, directly, as table cases the catalog can name.
func TestAttachment_StrictBase64Table(t *testing.T) {
	for in, ok := range map[string]bool{
		"": true, "aGk=": true, "aGVsbA==": true, "/+//": true,
		"aGk": false, "aGk\n": false, "a-k=": false, "a_k=": false, "a===": false, "====": false, "aG=k": false, "aGk=aGk=": false,
		"éééé": false,
	} {
		if _, got := Base64DecodedSize(in); got != ok {
			t.Errorf("%q: %v", in, got)
		}
	}
}

// REQ-006: the official safe-name vectors (testdata/attachments/SOURCE.md).
func TestAttachment_SafeFileNameFixtures(t *testing.T) {
	var fx struct {
		MaxNameBytes     int `json:"max_name_bytes"`
		MaxExtensionChar int `json:"max_extension_chars"`
		Names            []struct {
			Input    string `json:"input"`
			Position int    `json:"position"`
			Expected string `json:"expected"`
			Note     string `json:"note"`
		} `json:"names"`
	}
	readFixture(t, "save-names.json", &fx)
	if fx.MaxNameBytes != maxSaveNameBytes || fx.MaxExtensionChar != maxSaveExtensionRune {
		t.Fatalf("limits drifted from the fixture: %d %d", fx.MaxNameBytes, fx.MaxExtensionChar)
	}
	if len(fx.Names) < 50 {
		t.Fatalf("fixture too small: %d", len(fx.Names))
	}
	for _, c := range fx.Names {
		pos := c.Position
		if pos == 0 {
			pos = 1
		}
		if got := SafeFileName(c.Input, pos); got != c.Expected {
			t.Errorf("%s: SafeFileName(%q) = %q want %q", c.Note, c.Input, got, c.Expected)
		}
	}
}

// SafeFileName output is always a plain, bounded, valid-UTF-8 single name.
func TestAttachment_SafeFileNameInvariants(t *testing.T) {
	inputs := []string{"", "..", "a/b", `a\b`, "x\x00y", "\xff\xfe", strings.Repeat("日", 500) + ".md", "CON", " . ", string(rune(0x202e)) + "x"}
	for _, in := range inputs {
		got := SafeFileName(in, 2)
		if got == "" || len(got) > maxSaveNameBytes || strings.ContainsAny(got, `/\`+"\x00") || got == "." || got == ".." {
			t.Errorf("%q -> %q", in, got)
		}
		if strings.ToValidUTF8(got, "?") != got {
			t.Errorf("%q -> invalid UTF-8 %q", in, got)
		}
	}
}

func TestAttachment_Save(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub", "dir")
	atts := []Attachment{
		NewAttachment("report.txt", []byte("one")),
		NewAttachment("report.txt", []byte("two")),
		NewAttachment("../../escape.txt", []byte("three")),
		{Filename: "bad.bin", Content: "not-base64"},
		NewAttachment("", []byte("four")),
	}
	res, err := SaveAttachments(atts, dir, SaveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != len(atts) {
		t.Fatal(len(res))
	}
	wantBase := []string{"report.txt", "report (2).txt", "escape.txt", "", "attachment-5"}
	for i, r := range res {
		if r.Filename != atts[i].Filename {
			t.Errorf("%d: filename %q", i, r.Filename)
		}
		if wantBase[i] == "" {
			if r.Path != "" || r.Skipped != SkipInvalidContent || r.Size != 0 {
				t.Errorf("%d: %+v", i, r)
			}
			continue
		}
		if filepath.Base(r.Path) != wantBase[i] || filepath.Dir(r.Path) != dir || r.Skipped != "" {
			t.Errorf("%d: %+v", i, r)
		}
	}
	if b, _ := os.ReadFile(res[1].Path); string(b) != "two" {
		t.Fatal("content")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.txt")); err == nil {
		t.Fatal("escaped the directory")
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(res[0].Path)
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("file mode %v", fi.Mode())
		}
		di, _ := os.Stat(dir)
		if di.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode %v", di.Mode())
		}
	}
}

func TestAttachment_SaveNeverOverwritesOrFollowsLinks(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "b.txt")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	res, err := SaveAttachments([]Attachment{NewAttachment("a.txt", []byte("new")), NewAttachment("b.txt", []byte("new"))}, dir, SaveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(res[0].Path) != "a (2).txt" || filepath.Base(res[1].Path) != "b (2).txt" {
		t.Fatalf("%+v", res)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" { //nolint:gosec // the test reads a fixture or temp path it built itself
		t.Fatal("followed a symlink")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "old" { //nolint:gosec // the test reads a fixture or temp path it built itself
		t.Fatal("overwrote")
	}
}

func TestAttachment_SaveLimit(t *testing.T) {
	dir := t.TempDir()
	atts := []Attachment{NewAttachment("a", make([]byte, 6)), NewAttachment("b", make([]byte, 6)), NewAttachment("c", make([]byte, 4))}
	res, err := SaveAttachments(atts, dir, SaveOptions{MaxTotalBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Path == "" || res[1].Skipped != SkipOverLimit || res[1].Size != 6 || res[2].Path == "" {
		t.Fatalf("%+v", res)
	}
	res, _ = SaveAttachments(atts[:1], t.TempDir(), SaveOptions{MaxTotalBytes: -1})
	if res[0].Path == "" {
		t.Fatal("unlimited refused")
	}
	if DefaultSaveMaxTotalBytes != 64<<20 {
		t.Fatal("default cap")
	}
}
