package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/sarahmaeve/signatory/internal/artifact/stream"
)

// vcsInfoIntentName is the CaptureIntent.Name the cargo-side capture
// intent registers. Used as the lookup key in Manifest.Captured by
// extractCargoVCSInfoSHA. Stable string — changing it would silently
// break the post-fetch SHA recovery.
const vcsInfoIntentName = "cargo-vcs-info"

// vcsInfoMaxBytes caps how many bytes the walker is willing to copy
// for the vcs_info entry. Real .cargo_vcs_info.json files are well
// under 1 KiB (a single sha1 + path object); 64 KiB is generous and
// matches the size we cap other small named-metadata files at.
const vcsInfoMaxBytes int64 = 64 * 1024

// vcsInfoFileName is the basename cargo's `cargo publish` writes
// into the .crate tarball at the wrapping-directory root.
const vcsInfoFileName = ".cargo_vcs_info.json"

// cargoVCSInfoIntent is the CaptureIntent the artifact collector
// registers when the entity ecosystem is cargo. The matcher accepts
// the file at depth 0 (no wrapper) OR depth 1 under any wrapping
// directory (the typical .crate layout: "<name>-<version>/...").
//
// Depth-bounded matching avoids the squatting attack: a file at
// "src/.cargo_vcs_info.json" or "vendor/x/.cargo_vcs_info.json" is
// suspicious-looking but NOT the canonical cargo manifest. The
// walker would otherwise capture the deepest such match (or first,
// depending on iteration order) and feed an attacker-controlled
// path into the pair resolver.
//
// First-match-wins is enforced by stream.Walk; subsequent matches
// land in Manifest.SkippedIntents with reason "duplicate match",
// surfacing the squatting attempt as evidence.
var cargoVCSInfoIntent = stream.CaptureIntent{
	Name: vcsInfoIntentName,
	Match: func(e stream.Entry) bool {
		if e.Type != stream.EntryFile {
			return false
		}
		// Depth 0: file at archive root, no wrapping directory.
		// Depth 1: file directly under the wrapping directory
		// (e.g. "mycrate-0.1.0/.cargo_vcs_info.json").
		// Anything deeper is squatting and gets filtered here.
		switch strings.Count(e.Path, "/") {
		case 0:
			return e.Path == vcsInfoFileName
		case 1:
			slash := strings.IndexByte(e.Path, '/')
			return e.Path[slash+1:] == vcsInfoFileName
		default:
			return false
		}
	},
	MaxSize: vcsInfoMaxBytes,
}

// vcsInfoPayload is the unmarshal target for .cargo_vcs_info.json.
// The file shape (per cargo's source — src/cargo/ops/cargo_package.rs)
// is `{"git":{"sha1":"<40-hex>"},"path_in_vcs":"<rel-path>"}`. We
// only consume the SHA; path_in_vcs and other future fields are
// ignored.
type vcsInfoPayload struct {
	Git struct {
		SHA1 string `json:"sha1"`
	} `json:"git"`
}

// parseVCSInfoSHA extracts the publisher-stamped commit SHA from a
// .cargo_vcs_info.json byte payload. Returns ("", false) on:
//
//   - Malformed JSON (parse error, truncated, garbage bytes).
//   - Missing or empty git.sha1 field.
//   - SHA value that's not exactly 40 lowercase hexadecimal
//     characters — the strict shape `git rev-parse` produces. A
//     looser check would let an attacker who controls the tarball
//     supply a non-SHA string that some downstream tool might
//     interpret differently.
//
// All failure modes return cleanly rather than erroring — the
// caller falls back to tag-match resolution, which is the
// well-defined "no exact pair available" path.
func parseVCSInfoSHA(payload []byte) (string, bool) {
	var p vcsInfoPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", false
	}
	sha := p.Git.SHA1
	if !is40HexLower(sha) {
		return "", false
	}
	return sha, true
}

// is40HexLower reports whether s is exactly 40 lowercase hex chars
// — git's canonical full-SHA shape. Mixed-case (uppercase A-F) is
// rejected: cargo writes lowercase and accepting uppercase would
// admit looser-validating attacker variants.
func is40HexLower(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isDigit := c >= '0' && c <= '9'
		isLowerHex := c >= 'a' && c <= 'f'
		if !isDigit && !isLowerHex {
			return false
		}
	}
	return true
}

// extractCargoVCSInfoSHA pulls the SHA out of a manifest's captured
// vcs_info bytes, if any. Returns ("", false) when the manifest has
// no captured vcs_info, or when the captured bytes don't parse to a
// valid SHA. Does not log or error — silent fallthrough is the
// caller's recovery contract (revert to tag-match).
func extractCargoVCSInfoSHA(manifest *stream.Manifest) (string, bool) {
	if manifest == nil {
		return "", false
	}
	bytes_, ok := manifest.Captured[vcsInfoIntentName]
	if !ok {
		return "", false
	}
	return parseVCSInfoSHA(bytes_)
}

// captureIntentsForEcosystem returns the per-ecosystem named-file
// capture intents the artifact collector registers against
// stream.Walk. Returns nil for ecosystems with no post-fetch
// metadata recovery (npm — gitHead comes from registry; future
// pypi sdists similarly).
//
// The returned slice is safe to pass directly to stream.Walk —
// nil yields zero intent matches, so there's no need for the
// collector to special-case nil.
func captureIntentsForEcosystem(ecosystem string) []stream.CaptureIntent {
	switch ecosystem {
	case "cargo":
		return []stream.CaptureIntent{cargoVCSInfoIntent}
	default:
		return nil
	}
}

// gemDataIntentName is the CaptureIntent.Name the gem-side outer
// walker registers. Used as the lookup key in Manifest.Captured by
// the artifact collector to retrieve the inner data.tar.gz bytes
// for the second-pass walk.
const gemDataIntentName = "gem-data"

// gemDataFileName is the basename `gem build` writes into the .gem
// outer tar at the archive root — the actual file payload of a gem.
// Sibling to metadata.gz and checksums.yaml.gz.
const gemDataFileName = "data.tar.gz"

// gemDataMaxBytes caps how many bytes the outer walker will copy
// for the data.tar.gz capture. Matched to the default total cap so
// any gem whose outer walk would have succeeded under the standard
// resource ceiling can also have its inner data tarball captured
// for the second-pass walk.
const gemDataMaxBytes int64 = 256 * 1024 * 1024

// gemDataIntent captures the inner data.tar.gz from a `.gem`
// outer tar. The matcher is strict: the file must be at the archive
// root (no wrapping directory — gems don't use one) and named
// exactly "data.tar.gz". Anything else is suspicious for a gem and
// should not feed the second-pass walk.
//
// First-match-wins is enforced by stream.Walk; a malformed gem with
// two data.tar.gz entries would record the duplicate in
// SkippedIntents, surfacing the anomaly as evidence rather than
// silently choosing one.
var gemDataIntent = stream.CaptureIntent{
	Name: gemDataIntentName,
	Match: func(e stream.Entry) bool {
		if e.Type != stream.EntryFile {
			return false
		}
		// Gems have no wrapping directory; data.tar.gz is at the
		// outer root. Reject any path with a slash to refuse e.g.
		// "subdir/data.tar.gz" planted by an attacker.
		return e.Path == gemDataFileName
	},
	MaxSize: gemDataMaxBytes,
}

// walkGemArchive performs the two-pass walk a `.gem` requires:
//
//  1. Walk the outer (plain, uncompressed tar) capturing the inner
//     data.tar.gz bytes via gemDataIntent.
//  2. Re-walk those captured bytes as FormatTarGzip to produce the
//     manifest used for the diff.
//
// The diff machinery upstream operates on the inner manifest — so
// the comparison surface is the actual file payload, not the gem's
// outer envelope (which would otherwise compare {data.tar.gz,
// metadata.gz, checksums.yaml.gz} against source paths and produce
// no useful signal).
//
// Both walks honour lim. The intent's MaxSize bounds how much of
// the inner data.tar.gz the outer walk is willing to copy into
// memory; the inner walk applies lim's MaxTotalBytes against the
// gunzipped second-pass stream.
//
// Returns an error wrapped with context ("outer gem walk", "no
// data.tar.gz", "inner data walk") so the caller's absence message
// names which step failed.
func walkGemArchive(ctx context.Context, src io.Reader,
	lim stream.Limits) (*stream.Manifest, error) {

	outer, err := stream.Walk(ctx, src, stream.FormatTar,
		[]stream.CaptureIntent{gemDataIntent}, lim)
	if err != nil {
		return nil, fmt.Errorf("outer gem walk: %w", err)
	}
	innerBytes, ok := outer.Captured[gemDataIntentName]
	if !ok {
		// The outer didn't contain a data.tar.gz at the root. Either
		// the file is absent or the matcher rejected its position
		// (squatter at "subdir/data.tar.gz"). Either way, no inner
		// payload to compare.
		if reason, skipped := outer.SkippedIntents[gemDataIntentName]; skipped {
			return nil, fmt.Errorf("no data.tar.gz captured: %s", reason)
		}
		return nil, fmt.Errorf("no data.tar.gz at gem outer root")
	}
	inner, err := stream.Walk(ctx, bytes.NewReader(innerBytes),
		stream.FormatTarGzip, nil, lim)
	if err != nil {
		return nil, fmt.Errorf("inner data walk: %w", err)
	}
	return inner, nil
}

// publisherMetadataPaths returns the post-strip filenames the
// publisher's packaging tool injects into the release tarball but
// that are NOT present in the upstream git tree at any commit.
// They are not divergence; they're expected output of the publish
// pipeline.
//
// Returning these from the collector and appending them to the
// gitPaths input to ComputeDiff makes the diff treat them as
// already-present-in-git, suppressing the false positive without
// the divergence diff needing per-ecosystem awareness. The diff
// stays generic; the per-ecosystem knowledge stays here.
//
// cargo (`cargo publish` writes all three):
//
//   - .cargo_vcs_info.json — git provenance JSON (the same file
//     parseVCSInfoSHA reads).
//   - Cargo.toml.orig — verbatim copy of the source Cargo.toml,
//     written alongside the normalized Cargo.toml.
//   - Cargo.lock — the lockfile. Always injected by `cargo publish`
//     even for library crates, which by Rust convention gitignore
//     Cargo.lock (binaries commit it; libraries don't). Without
//     suppression every library crate's divergence falsely flags
//     Cargo.lock. Surfaced by the blake3 dogfood run on this
//     branch — the file showed up as the sole extra-in-tarball,
//     dominating an otherwise-clean comparison.
//
// Trade-off worth naming: for binary crates that DO commit
// Cargo.lock, suppressing it here means lockfile drift between
// the published .crate and the source repo will not surface as
// divergence. That class of drift is interesting (stale lockfile,
// dependency-pin discrepancy) but fits better as its own signal
// than as an extras-in-tarball entry — it's a fact about the
// lockfile pair, not about which files are present.
//
// Other ecosystems: nil. npm's tarball contents are governed by
// .npmignore / "files" and are user-controlled; PyPI sdist /
// gem / wheel similarly. Add filters here as dogfood traces show
// false positives from per-ecosystem packaging conventions.
func publisherMetadataPaths(ecosystem string) []string {
	switch ecosystem {
	case "cargo":
		return []string{
			".cargo_vcs_info.json",
			"Cargo.toml.orig",
			"Cargo.lock",
		}
	case "pypi":
		// PKG-INFO is the PEP 241 / core-metadata file every sdist
		// carries at the wrapper root. Setuptools, hatch, flit, pdm,
		// poetry — every modern build backend writes it. Never
		// committed to git: it's a build output, regenerated each
		// publish from pyproject.toml / setup.cfg.
		//
		// <name>.egg-info/* is also publisher-injected but the
		// directory name embeds the package name and so can't be
		// expressed as a literal here. eggInfoPaths derives the
		// concrete paths from the walked manifest; the collector
		// appends both to gitPaths.
		return []string{"PKG-INFO"}
	default:
		return nil
	}
}

// eggInfoPaths walks manifest for entries whose first POST-STRIP
// path component ends with ".egg-info" and returns the post-strip
// paths. The return is the merge-into-gitPaths input the collector
// uses to suppress the egg-info subtree as expected publisher
// output rather than surfacing it as divergence.
//
// Manifest entry paths are verbatim — pre-strip, with the wrapping
// <name>-<version>/ prefix still attached. ComputeDiff applies
// manifest.StrippedTopDir to compute the post-strip view it
// compares against gitPaths. We mirror that same strip here so the
// returned paths land in gitPaths in the form ComputeDiff will look
// up.
//
// Returns nil for a nil manifest or one with no egg-info entries.
// The collector's append-nil pattern handles both cleanly.
func eggInfoPaths(manifest *stream.Manifest) []string {
	if manifest == nil {
		return nil
	}
	stripPrefix := manifest.StrippedTopDir
	var out []string
	for _, e := range manifest.Entries {
		if e.Type != stream.EntryFile {
			continue
		}
		path := strings.TrimPrefix(e.Path, stripPrefix)
		if path == "" {
			continue
		}
		// Check the first POST-STRIP path component for the .egg-info
		// suffix. Covers "<name>.egg-info/PKG-INFO" and any nested
		// entry under the egg-info directory.
		first, _, ok := strings.Cut(path, "/")
		if !ok {
			continue
		}
		if strings.HasSuffix(first, ".egg-info") {
			out = append(out, path)
		}
	}
	return out
}
