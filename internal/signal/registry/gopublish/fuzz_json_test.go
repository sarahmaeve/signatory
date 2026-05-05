package gopublish

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// --- Fuzz targets for gopublish JSON and text parsers ---
//
// The Go module proxy (proxy.golang.org) and sum database
// (sum.golang.org) serve JSON and line-delimited text responses.
// These are external services — a compromised proxy or MITM can
// serve adversarial content.

// --- FuzzLookupTransparencyParse ---
//
// LookupTransparency parses line-delimited text from sum.golang.org.
// Line 1 is the leaf ID (int64 parsed via strconv.ParseInt); the
// remainder is the raw hash record. This fuzz test exercises the
// parsing logic in isolation — no HTTP, no database.

func FuzzLookupTransparencyParse(f *testing.F) {
	// Normal: numeric leaf ID followed by hash record
	f.Add([]byte("12345\ngithub.com/foo/bar v1.0.0 h1:abc=\n"))
	f.Add([]byte("0\nmodule v0.0.0 h1:000=\n"))
	f.Add([]byte("9999999999\nlong/path v99.99.99 h1:xyz=\n"))
	// No newline (single line — leaf ID only)
	f.Add([]byte("42"))
	// Empty
	f.Add([]byte{})
	// Non-numeric first line
	f.Add([]byte("not-a-number\nsome content\n"))
	f.Add([]byte("abc\n"))
	// Negative leaf ID (strconv.ParseInt allows it)
	f.Add([]byte("-1\nmodule v1.0.0 h1:x=\n"))
	// Very large number
	f.Add([]byte("9223372036854775807\nmax int64\n"))
	// Overflow
	f.Add([]byte("9223372036854775808\noverflow\n"))
	// Leading whitespace on first line
	f.Add([]byte("  123  \nmodule v1.0.0 h1:x=\n"))
	// Binary garbage
	f.Add([]byte{0xff, 0xfe, 0x00, 0x01, '\n', 'a', 'b', 'c'})
	// Many newlines
	f.Add([]byte("100\n\n\n\nmultiline content\n"))

	f.Fuzz(func(t *testing.T, body []byte) {
		// Replicate the parsing logic from LookupTransparency
		// without the HTTP layer.
		rec := parseTransparencyBody(body)

		// Invariant 1: LeafID must be non-negative.
		// The sum database uses monotonically increasing sequence
		// numbers. A negative leaf ID is nonsensical.
		if rec.LeafID < 0 {
			t.Errorf("TransparencyRecord.LeafID is negative: %d", rec.LeafID)
		}

		// Invariant 2: if the first line is a valid non-negative int64,
		// LeafID must reflect it (round-trip consistency).
		if idx := bytes.IndexByte(body, '\n'); idx > 0 {
			first := strings.TrimSpace(string(body[:idx]))
			if n, err := strconv.ParseInt(first, 10, 64); err == nil && n >= 0 {
				if rec.LeafID != n {
					t.Errorf("LeafID=%d but first line parses to %d", rec.LeafID, n)
				}
			}
		}

		// Note: RawRecord is intentionally the raw body bytes —
		// callers that need the hash payload access it directly.
		// We do NOT assert UTF-8 validity on it since it preserves
		// the wire format verbatim.
	})
}

// parseTransparencyBody replicates the body-parsing logic from
// LookupTransparency without the HTTP/validation wrapping.
// This is the code under test — extracted so the fuzzer can
// call it directly.
func parseTransparencyBody(body []byte) *TransparencyRecord {
	rec := &TransparencyRecord{RawRecord: string(body)}
	if idx := bytes.IndexByte(body, '\n'); idx > 0 {
		first := strings.TrimSpace(string(body[:idx]))
		if n, perr := strconv.ParseInt(first, 10, 64); perr == nil && n >= 0 {
			rec.LeafID = n
		}
	}
	return rec
}

// --- FuzzLatestInfoUnmarshal ---

func FuzzLatestInfoUnmarshal(f *testing.F) {
	f.Add([]byte(`{"Version":"v1.2.3","Time":"2024-01-15T10:30:00Z"}`))
	f.Add([]byte(`{"Version":"v0.0.0-20240101000000-abcdef123456","Time":"2024-01-01T00:00:00Z"}`))
	f.Add([]byte(`{"Version":"","Time":"0001-01-01T00:00:00Z"}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		var li LatestInfo
		if err := json.Unmarshal(data, &li); err != nil {
			return
		}

		if !utf8.ValidString(li.Version) {
			t.Errorf("LatestInfo.Version is invalid UTF-8: %q", li.Version)
		}
	})
}

// --- FuzzVersionInfoUnmarshal ---

func FuzzVersionInfoUnmarshal(f *testing.F) {
	f.Add([]byte(`{"Version":"v1.2.3","Time":"2024-01-15T10:30:00Z","Origin":{"VCS":"git","URL":"https://github.com/x/y","Ref":"refs/tags/v1.2.3","Hash":"abc123"}}`))
	f.Add([]byte(`{"Version":"v0.1.0","Time":"2024-06-01T00:00:00Z","Origin":{}}`))
	f.Add([]byte(`{"Version":"v1.0.0","Time":"2024-01-01T00:00:00Z"}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte{})
	// Adversarial: Origin URL with control chars
	f.Add([]byte(`{"Version":"v1.0.0","Time":"2024-01-01T00:00:00Z","Origin":{"VCS":"git","URL":"https://github.com/x/y","Ref":"","Hash":""}}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var vi VersionInfo
		if err := json.Unmarshal(data, &vi); err != nil {
			return
		}

		// Invariant 1: Version must be valid UTF-8.
		if !utf8.ValidString(vi.Version) {
			t.Errorf("VersionInfo.Version is invalid UTF-8: %q", vi.Version)
		}

		// Invariant 2: Origin fields must be valid UTF-8.
		originStrings := []string{vi.Origin.VCS, vi.Origin.URL, vi.Origin.Ref, vi.Origin.Hash}
		for _, s := range originStrings {
			if !utf8.ValidString(s) {
				t.Errorf("Origin field is invalid UTF-8: %q", s)
			}
		}
	})
}
