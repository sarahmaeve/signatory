package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestParseCommitSigningLog covers the byte-level parser with
// hand-constructed inputs. Byte literals use explicit 0x1F / 0x1E
// escapes so the test spec is readable and matches exactly what
// the git log format emits in practice.
func TestParseCommitSigningLog(t *testing.T) {
	t.Parallel()

	const us = "\x1f"
	const rs = "\x1e"

	cases := []struct {
		name string
		data string
		want []commitSigningRow
	}{
		{
			name: "empty input produces empty slice not nil",
			data: "",
			want: []commitSigningRow{},
		},
		{
			name: "single unsigned commit",
			data: "abc123" + us + "Alice" + us + "alice@example.com" + us + "N" + us + "" + us + "" + rs,
			want: []commitSigningRow{
				{Hash: "abc123", AuthorName: "Alice", AuthorEmail: "alice@example.com", SignatureStatus: "N"},
			},
		},
		{
			name: "single per-developer signed commit",
			data: "def456" + us + "Bob" + us + "bob@example.com" + us + "G" + us + "Bob Signer" + us + "DEADBEEFCAFEBABE" + rs,
			want: []commitSigningRow{
				{
					Hash: "def456", AuthorName: "Bob", AuthorEmail: "bob@example.com",
					SignatureStatus: "G", SignerName: "Bob Signer", KeyID: "DEADBEEFCAFEBABE",
				},
			},
		},
		{
			name: "web-flow signed commit",
			data: "789abc" + us + "GitHub" + us + "noreply@github.com" + us + "G" + us + "GitHub" + us + "B5690EEEBB952194" + rs,
			want: []commitSigningRow{
				{
					Hash: "789abc", AuthorName: "GitHub", AuthorEmail: "noreply@github.com",
					SignatureStatus: "G", SignerName: "GitHub", KeyID: "B5690EEEBB952194",
				},
			},
		},
		{
			name: "three commits separated by record terminators plus newline",
			data: "a" + us + "A" + us + "a@x" + us + "N" + us + "" + us + "" + rs +
				"\nb" + us + "B" + us + "b@x" + us + "G" + us + "B Signer" + us + "0123456789ABCDEF" + rs +
				"\nc" + us + "C" + us + "c@x" + us + "N" + us + "" + us + "" + rs,
			want: []commitSigningRow{
				{Hash: "a", AuthorName: "A", AuthorEmail: "a@x", SignatureStatus: "N"},
				{Hash: "b", AuthorName: "B", AuthorEmail: "b@x", SignatureStatus: "G", SignerName: "B Signer", KeyID: "0123456789ABCDEF"},
				{Hash: "c", AuthorName: "C", AuthorEmail: "c@x", SignatureStatus: "N"},
			},
		},
		{
			name: "pipe characters in author name do not confuse parser",
			data: "h" + us + "A|uthor | name" + us + "a@x" + us + "N" + us + "" + us + "" + rs,
			want: []commitSigningRow{
				{Hash: "h", AuthorName: "A|uthor | name", AuthorEmail: "a@x", SignatureStatus: "N"},
			},
		},
		{
			name: "truncated record (fewer than 6 fields) is skipped silently",
			data: "short" + us + "only" + us + "three" + rs +
				"good" + us + "A" + us + "a@x" + us + "N" + us + "" + us + "" + rs,
			want: []commitSigningRow{
				{Hash: "good", AuthorName: "A", AuthorEmail: "a@x", SignatureStatus: "N"},
			},
		},
		{
			name: "trailing record-terminator only input produces empty slice",
			data: rs,
			want: []commitSigningRow{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseCommitSigningLog([]byte(tc.data))
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestClassifySigning walks every %G? flag value and both web-flow
// key IDs plus a novel per-developer key, locking in the
// classification table specified in the signing.go doc comment.
func TestClassifySigning(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		row  commitSigningRow
		want signingClass
	}{
		// Unsigned equivalents.
		{"N (no signature)", commitSigningRow{SignatureStatus: "N"}, classUnsigned},
		{"empty status", commitSigningRow{SignatureStatus: ""}, classUnsigned},
		{"B (bad signature)", commitSigningRow{SignatureStatus: "B", KeyID: "DEADBEEFCAFEBABE"}, classUnsigned},
		{"E (cannot check)", commitSigningRow{SignatureStatus: "E", KeyID: "DEADBEEFCAFEBABE"}, classUnsigned},
		{"R (revoked key)", commitSigningRow{SignatureStatus: "R", KeyID: "DEADBEEFCAFEBABE"}, classUnsigned},
		{"future unknown flag Z", commitSigningRow{SignatureStatus: "Z", KeyID: "DEADBEEFCAFEBABE"}, classUnsigned},

		// Valid-signature equivalents, per-developer key.
		{"G (good) per-dev", commitSigningRow{SignatureStatus: "G", KeyID: "DEADBEEFCAFEBABE"}, classPerDeveloper},
		{"U (unknown validity) per-dev", commitSigningRow{SignatureStatus: "U", KeyID: "DEADBEEFCAFEBABE"}, classPerDeveloper},
		{"X (sig expired) per-dev", commitSigningRow{SignatureStatus: "X", KeyID: "DEADBEEFCAFEBABE"}, classPerDeveloper},
		{"Y (key expired) per-dev", commitSigningRow{SignatureStatus: "Y", KeyID: "DEADBEEFCAFEBABE"}, classPerDeveloper},

		// Web-flow keys (both listed IDs, both cases).
		{"G with current web-flow key uppercase", commitSigningRow{SignatureStatus: "G", KeyID: "B5690EEEBB952194"}, classWebFlow},
		{"G with current web-flow key lowercase", commitSigningRow{SignatureStatus: "G", KeyID: "b5690eeebb952194"}, classWebFlow},
		{"G with older web-flow key", commitSigningRow{SignatureStatus: "G", KeyID: "4AEE18F83AFDEB23"}, classWebFlow},
		{"G with web-flow key surrounded by whitespace", commitSigningRow{SignatureStatus: "G", KeyID: "  B5690EEEBB952194 "}, classWebFlow},

		// Empty key ID with good status falls through to per-developer.
		// This is an oddity of git output (some signature types report
		// status G without filling GK) and the conservative call is to
		// NOT classify it as web-flow.
		{"G with empty key falls to per-dev", commitSigningRow{SignatureStatus: "G", KeyID: ""}, classPerDeveloper},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifySigning(tc.row)
			assert.Equal(t, tc.want, got)
		})
	}
}
