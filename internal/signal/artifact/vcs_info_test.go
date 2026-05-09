package artifact

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/sarahmaeve/signatory/internal/artifact/stream"
)

// TestParseVCSInfoSHA_Strict drives the parser against the full
// matrix of well-formed and adversarial inputs. The function is
// load-bearing for trust: a SHA accepted here flows into
// ResolvePair as the publisher-stamped commit, which then drives
// PathsAtRef and the divergence diff. Loose validation would let
// an attacker who controls the tarball supply a non-SHA string
// that some downstream tool might interpret differently.
//
// Strictness contract:
//
//   - Exactly 40 characters.
//   - Lowercase hex only (0-9, a-f).
//   - JSON must be well-formed; git.sha1 must be a string field.
//
// Every other shape returns ("", false) with no panic, no log,
// no error — silent fallthrough is the caller's recovery contract
// (revert to tag-match resolution).
func TestParseVCSInfoSHA_Strict(t *testing.T) {
	t.Parallel()

	const validSHA = "abcdef0123456789abcdef0123456789abcdef01"

	cases := []struct {
		name    string
		payload string
		wantSHA string
		wantOK  bool
	}{
		{
			name:    "well-formed cargo vcs_info",
			payload: `{"git":{"sha1":"` + validSHA + `"},"path_in_vcs":""}`,
			wantSHA: validSHA,
			wantOK:  true,
		},
		{
			name:    "well-formed without path_in_vcs (older cargo)",
			payload: `{"git":{"sha1":"` + validSHA + `"}}`,
			wantSHA: validSHA,
			wantOK:  true,
		},
		{
			name:    "uppercase hex rejected",
			payload: `{"git":{"sha1":"` + strings.ToUpper(validSHA) + `"}}`,
			wantOK:  false,
		},
		{
			name:    "39 chars (one too short)",
			payload: `{"git":{"sha1":"` + validSHA[:39] + `"}}`,
			wantOK:  false,
		},
		{
			name:    "41 chars (one too long)",
			payload: `{"git":{"sha1":"` + validSHA + "a" + `"}}`,
			wantOK:  false,
		},
		{
			name:    "non-hex character mid-string",
			payload: `{"git":{"sha1":"abcdef0123456789abcdefxx23456789abcdef01"}}`,
			wantOK:  false,
		},
		{
			name:    "empty SHA",
			payload: `{"git":{"sha1":""}}`,
			wantOK:  false,
		},
		{
			name:    "missing git.sha1 entirely",
			payload: `{"git":{}}`,
			wantOK:  false,
		},
		{
			name:    "missing git object",
			payload: `{}`,
			wantOK:  false,
		},
		{
			name:    "malformed JSON (truncated)",
			payload: `{"git":{"sha1":"`,
			wantOK:  false,
		},
		{
			name:    "non-JSON garbage",
			payload: `not json at all`,
			wantOK:  false,
		},
		{
			name:    "empty payload",
			payload: ``,
			wantOK:  false,
		},
		{
			name: "SHA is an array (type confusion attempt)",
			// JSON spec: "sha1":[...] should fail json.Unmarshal
			// against our struct (string-typed field).
			payload: `{"git":{"sha1":["` + validSHA + `"]}}`,
			wantOK:  false,
		},
		{
			name:    "extra surrounding whitespace (still valid JSON)",
			payload: `   {"git":{"sha1":"` + validSHA + `"}}   `,
			wantSHA: validSHA,
			wantOK:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotSHA, gotOK := parseVCSInfoSHA([]byte(tc.payload))
			assert.Equal(t, tc.wantOK, gotOK,
				"parseVCSInfoSHA returned ok=%v for %q (payload=%q)",
				gotOK, tc.name, tc.payload)
			if tc.wantOK {
				assert.Equal(t, tc.wantSHA, gotSHA)
			} else {
				assert.Empty(t, gotSHA,
					"failed parse must return empty SHA, never partial; got %q", gotSHA)
			}
		})
	}
}

// TestCargoVCSInfoIntent_DepthBoundedMatch verifies the intent's
// match function rejects squatted paths at depths > 1 — the
// security-critical filter that prevents an attacker who can write
// `src/.cargo_vcs_info.json` (or any deeper nesting) into the
// tarball from having that file's SHA used as the publisher's
// attested commit.
//
// Real cargo .crate tarballs wrap content in "<name>-<version>/",
// so the canonical vcs_info path is at depth 1. A squatter at
// depth 2 ("mycrate-1.0/src/.cargo_vcs_info.json") would still be
// IN the canonical wrapping directory but at a non-root location
// inside the source tree — a clearly suspicious shape.
func TestCargoVCSInfoIntent_DepthBoundedMatch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		path      string
		entryType stream.EntryType
		wantMatch bool
	}{
		{
			name:      "depth 0 (no wrapper, file at archive root)",
			path:      ".cargo_vcs_info.json",
			entryType: stream.EntryFile,
			wantMatch: true,
		},
		{
			name:      "depth 1 (canonical .crate layout)",
			path:      "mycrate-1.0.0/.cargo_vcs_info.json",
			entryType: stream.EntryFile,
			wantMatch: true,
		},
		{
			name:      "depth 2 (squatted under src/) MUST NOT match",
			path:      "mycrate-1.0.0/src/.cargo_vcs_info.json",
			entryType: stream.EntryFile,
			wantMatch: false,
		},
		{
			name:      "depth 3 (deeply nested squatter) MUST NOT match",
			path:      "mycrate-1.0.0/vendor/sub/.cargo_vcs_info.json",
			entryType: stream.EntryFile,
			wantMatch: false,
		},
		{
			name:      "wrong basename at root MUST NOT match",
			path:      "vcs_info.json",
			entryType: stream.EntryFile,
			wantMatch: false,
		},
		{
			name:      "wrong basename at depth 1 MUST NOT match",
			path:      "mycrate-1.0.0/vcs_info.json",
			entryType: stream.EntryFile,
			wantMatch: false,
		},
		{
			name:      "directory entry with matching name MUST NOT match",
			path:      ".cargo_vcs_info.json",
			entryType: stream.EntryDir,
			wantMatch: false,
		},
		{
			name:      "symlink with matching name MUST NOT match",
			path:      ".cargo_vcs_info.json",
			entryType: stream.EntrySymlink,
			wantMatch: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := cargoVCSInfoIntent.Match(stream.Entry{
				Path: tc.path,
				Type: tc.entryType,
			})
			assert.Equal(t, tc.wantMatch, got,
				"intent matcher misjudged %q (type=%v)", tc.path, tc.entryType)
		})
	}
}

// TestPublisherMetadataPaths verifies the per-ecosystem expected-
// tarball-only filename list. Pinning these explicitly catches a
// future "I added a metadata path to vcs_info.go but forgot to
// document the rationale" regression — every entry here should
// have a comment in vcs_info.go explaining why.
func TestPublisherMetadataPaths(t *testing.T) {
	t.Parallel()

	cargo := publisherMetadataPaths("cargo")
	assert.Contains(t, cargo, ".cargo_vcs_info.json",
		"cargo metadata list must include the vcs_info file — without "+
			"it, every cargo crate's divergence falsely flags vcs_info as extra")
	assert.Contains(t, cargo, "Cargo.toml.orig",
		"cargo metadata list must include Cargo.toml.orig — `cargo "+
			"package` writes both alongside the normalized Cargo.toml")
	assert.Contains(t, cargo, "Cargo.lock",
		"cargo metadata list must include Cargo.lock — `cargo publish` "+
			"injects the lockfile into the .crate tarball regardless of "+
			"whether the source repo commits one. Library crates (per Rust "+
			"convention) gitignore Cargo.lock, so without this filter every "+
			"library crate's divergence falsely flags Cargo.lock as extra. "+
			"Surfaced by the blake3 dogfood run on this branch.")

	assert.Empty(t, publisherMetadataPaths("npm"),
		"npm has no equivalent fixed metadata; tarball contents are "+
			"governed by .npmignore / files field and are user-controlled")

	assert.Empty(t, publisherMetadataPaths("unknown-ecosystem"),
		"unknown ecosystem must return nil — silently empty rather "+
			"than panic, since the value flows into a slice append")
}
