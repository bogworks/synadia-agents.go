// Copyright 2026 The synadia-agents.go Authors
// SPDX-License-Identifier: Apache-2.0

package agents

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Attachment is a file carried in a request, a response chunk or a query
// chunk: {"filename", "content"} with the content in standard-alphabet,
// padded base64 without whitespace. No MIME type is carried. See SAP §5.2.
//
// Content stays the base64 text as it was on the wire, so a received
// attachment that is not strict base64 does not spoil the chunk it came in;
// [Attachment.Bytes] reports that when the program decodes it. Unknown
// fields are kept in Extra and written back when the attachment is relayed.
// See SAP §5.6.
type Attachment struct {
	// Filename is the authoritative file name. It is untrusted when received;
	// use [SafeFileName] or [SaveAttachments] before it touches a disk.
	Filename string
	// Content is standard-alphabet padded base64 (RFC 4648 §4).
	Content string
	// Extra holds unknown fields of the attachment object, verbatim.
	Extra map[string]json.RawMessage
}

// NewAttachment encodes data as an attachment named filename. See SAP §5.2.
func NewAttachment(filename string, data []byte) Attachment {
	return Attachment{Filename: filename, Content: base64.StdEncoding.EncodeToString(data)}
}

// Bytes decodes the content. It fails with an [*AttachmentError] unless the
// content is strict RFC 4648 §4 base64: standard alphabet, padded, no
// whitespace. See SAP §5.2.
func (a Attachment) Bytes() ([]byte, error) {
	if _, ok := Base64DecodedSize(a.Content); !ok {
		return nil, &AttachmentError{Index: -1, Reason: "content is not standard-alphabet, padded base64 without whitespace (SAP §5.2)"}
	}
	b, err := base64.StdEncoding.DecodeString(a.Content)
	if err != nil {
		return nil, &AttachmentError{Index: -1, Reason: "content is not valid base64 (SAP §5.2)"}
	}
	return b, nil
}

// Validate checks the attachment as a sender: the filename is not empty and
// the content is strict base64. The error is a [*AttachmentError]. See SAP
// §5.2.
func (a Attachment) Validate() error { return a.validateAt(-1) }

func (a Attachment) validateAt(i int) error {
	if a.Filename == "" {
		return &AttachmentError{Index: i, Reason: "empty filename"}
	}
	if _, ok := Base64DecodedSize(a.Content); !ok {
		return &AttachmentError{Index: i, Reason: "content is not standard-alphabet, padded base64 without whitespace (SAP §5.2)"}
	}
	return nil
}

// MarshalJSON writes {"filename", "content"} followed by the extra fields. An
// extra named "filename" or "content" is not written; an extra that is not
// exactly one valid JSON value fails with [ErrInvalidExtra].
func (a Attachment) MarshalJSON() ([]byte, error) {
	return marshalObject([]field{
		{"filename", a.Filename, true},
		{"content", a.Content, true},
	}, a.Extra, "filename", "content")
}

// UnmarshalJSON reads an attachment object. Both fields are required strings;
// every other field lands in Extra. Base64 is not checked here (see
// [Attachment.Bytes]).
func (a *Attachment) UnmarshalJSON(data []byte) error {
	obj, err := splitObject(data)
	if err != nil {
		return errors.New("attachment must be an object")
	}
	fn, ok := stringField(obj, "filename")
	if !ok {
		return errors.New("attachment missing string `filename`")
	}
	ct, ok := stringField(obj, "content")
	if !ok {
		return errors.New("attachment missing string `content`")
	}
	*a = Attachment{Filename: fn, Content: ct, Extra: withoutKeys(obj, "filename", "content")}
	return nil
}

// Base64DecodedSize returns the number of bytes strict base64 content decodes
// to, and false when s is not strict RFC 4648 §4 base64: standard alphabet
// only (no URL-safe "-" "_"), length a multiple of 4, at most two "=" and only
// at the end, no whitespace. Empty content is valid and zero bytes. Non-zero
// trailing bits are tolerated, as the official SDKs do. See SAP §5.2.
func Base64DecodedSize(s string) (int, bool) {
	if len(s)%4 != 0 {
		return 0, false
	}
	pad := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
			if pad > 0 {
				return 0, false
			}
		case c == '=':
			pad++
			if pad > 2 {
				return 0, false
			}
		default:
			return 0, false
		}
	}
	return len(s)/4*3 - pad, true
}

// Save-name limits, shared with the official SDKs' fixtures.
const (
	maxSaveNameBytes     = 200
	maxSaveExtensionRune = 16
	maxSaveNameAttempts  = 1000

	// DefaultSaveMaxTotalBytes is the default cap on decoded bytes one
	// [SaveAttachments] call writes: 64 MiB.
	DefaultSaveMaxTotalBytes int64 = 64 << 20
)

// edgeChars are stripped from both ends of a name: dots, and the characters
// the official SDKs treat as whitespace.
const edgeChars = " .\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"

var windowsDeviceName = regexp.MustCompile(`(?i)^(?:CON|PRN|AUX|NUL|CONIN\$|CONOUT\$|(?:COM|LPT)[1-9\x{b9}\x{b2}\x{b3}]) *(?:\.|$)`)

// SafeFileName turns an untrusted attachment name into a name that is safe to
// create in a directory, the same on every OS. position is the attachment's
// 1-based place in its list, used by the fallback "attachment-<position>".
//
// Only the part after the last "/" or "\" survives. Control characters (C0,
// DEL, C1) and the characters that change text direction (U+200E, U+200F,
// U+202A-U+202E, U+2066-U+2069) are removed, the characters Windows forbids
// (< > : " | ? *) become "_", leading and trailing dots and whitespace are
// stripped, the fallback is used when nothing is left, a name longer than 200
// UTF-8 bytes is shortened keeping an extension of up to 16 characters, and a
// Windows device name (CON, nul.txt, COM1.log) gets a leading "_". It agrees
// with the official SDKs on their shared fixtures. SAP §5.2 makes the
// filename authoritative but says nothing of saving it; this is SDK policy for
// the receiving side.
func SafeFileName(name string, position int) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name { // an invalid byte arrives as U+FFFD
		switch {
		case r <= 0x1f, r >= 0x7f && r <= 0x9f:
			continue
		case r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			continue
		case strings.ContainsRune(`<>:"|?*`, r):
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), edgeChars)
	if out == "" {
		return fmt.Sprintf("attachment-%d", position)
	}
	out = shortenName(out)
	if windowsDeviceName.MatchString(out) {
		return shortenName("_" + out)
	}
	return out
}

func shortenName(name string) string {
	if len(name) <= maxSaveNameBytes {
		return name
	}
	stem, ext := splitExtension(name)
	if utf8.RuneCountInString(ext) > maxSaveExtensionRune {
		stem, ext = name, ""
	}
	budget := maxSaveNameBytes - len(ext)
	// name is valid UTF-8 here: SafeFileName wrote it rune by rune.
	used, end := 0, 0
	for i, r := range stem {
		n := utf8.RuneLen(r)
		if used+n > budget {
			break
		}
		used += n
		end = i + n
	}
	return strings.Trim(stem[:end], edgeChars) + ext
}

// splitExtension returns ("archive.tar", ".gz"); no extension when there is
// no dot past the first character.
func splitExtension(name string) (string, string) {
	if dot := strings.LastIndexByte(name, '.'); dot > 0 {
		return name[:dot], name[dot:]
	}
	return name, ""
}

// SkipReason says why [SaveAttachments] did not save an attachment.
type SkipReason string

const (
	// SkipOverLimit means saving the attachment would pass MaxTotalBytes.
	SkipOverLimit SkipReason = "over_limit"
	// SkipInvalidContent means the content is not strict base64.
	SkipInvalidContent SkipReason = "invalid_content"
)

// SavedAttachment is the outcome for one input attachment, in input order.
type SavedAttachment struct {
	// Filename is the name as the sender gave it; the file on disk may differ.
	Filename string
	// Size is the decoded size in bytes, 0 when the content was invalid.
	Size int64
	// Path is the absolute path written, empty when the attachment was not saved.
	Path string
	// Skipped is set iff Path is empty.
	Skipped SkipReason
}

// SaveOptions tunes [SaveAttachments].
type SaveOptions struct {
	// MaxTotalBytes caps the decoded bytes one call writes. An attachment that
	// would pass it is skipped (a later, smaller one may still fit). Zero means
	// [DefaultSaveMaxTotalBytes]; a negative value disables the cap.
	MaxTotalBytes int64
}

// SaveAttachments decodes received attachments and writes each into dir
// (created, mode 0700, if missing), returning one [SavedAttachment] per input
// in the same order. This is SDK policy for the receiving side; SAP §5.2
// defines the shape only.
//
// dir is resolved once, at call time: it is made, opened as an [os.Root], and
// every file is then created through that handle. A symlink in dir or in one
// of its ancestors is followed when dir is opened, as the caller's own choice
// of path, but after that the directory cannot be changed under the call: a
// rename, or a symlink put in its place, does not redirect a write, and no
// name can reach outside the opened directory. The returned Path values are
// built from the absolute form of dir at call time.
//
// Content must be strict base64 (SAP §5.2); anything else is skipped as
// [SkipInvalidContent], never written and never an error. The file name comes
// from [SafeFileName]. A file is only ever created: it is opened exclusively,
// so an existing file or symlink in the directory is never overwritten or
// followed, and on a clash the next free "<stem> (2)<ext>", "(3)", ... is
// used. Files are created with mode 0600. A real I/O error is returned, after
// removing a file this call could not finish writing.
func SaveAttachments(atts []Attachment, dir string, opts SaveOptions) ([]SavedAttachment, error) {
	return saveAttachments(atts, dir, opts, nil)
}

// saveAttachments is SaveAttachments with a hook that runs once the directory
// is open and before the first file is written, so a test can swap the path.
func saveAttachments(atts []Attachment, dir string, opts SaveOptions, afterOpen func()) ([]SavedAttachment, error) {
	limit := opts.MaxTotalBytes
	if limit == 0 {
		limit = DefaultSaveMaxTotalBytes
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if afterOpen != nil {
		afterOpen()
	}
	out := make([]SavedAttachment, 0, len(atts))
	var total int64
	for i, a := range atts {
		size, ok := Base64DecodedSize(a.Content)
		if !ok {
			out = append(out, SavedAttachment{Filename: a.Filename, Skipped: SkipInvalidContent})
			continue
		}
		if limit > 0 && total+int64(size) > limit {
			out = append(out, SavedAttachment{Filename: a.Filename, Size: int64(size), Skipped: SkipOverLimit})
			continue
		}
		data, err := a.Bytes()
		if err != nil {
			out = append(out, SavedAttachment{Filename: a.Filename, Skipped: SkipInvalidContent})
			continue
		}
		path, err := createExclusive(root, abs, SafeFileName(a.Filename, i+1), data)
		if err != nil {
			return out, err
		}
		total += int64(size)
		out = append(out, SavedAttachment{Filename: a.Filename, Size: int64(size), Path: path})
	}
	return out, nil
}

func createExclusive(root *os.Root, absDir, name string, data []byte) (string, error) {
	stem, ext := splitExtension(name)
	for attempt := 1; attempt <= maxSaveNameAttempts; attempt++ {
		candidate := name
		if attempt > 1 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, attempt, ext)
		}
		f, err := root.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return "", err
		}
		if _, err := f.Write(data); err != nil {
			_ = f.Close()              // the write error is the one returned
			_ = root.Remove(candidate) // best-effort cleanup of a partial file
			return "", err
		}
		if err := f.Close(); err != nil {
			_ = root.Remove(candidate) // best-effort cleanup of a partial file
			return "", err
		}
		return filepath.Join(absDir, candidate), nil
	}
	return "", fmt.Errorf("no free name for %q in %s after %d attempts", name, absDir, maxSaveNameAttempts)
}
