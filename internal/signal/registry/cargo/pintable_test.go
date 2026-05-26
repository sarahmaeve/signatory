package cargo

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// buildCrateTarball builds a gzipped tar with a single
// `<name>-<version>/.cargo_vcs_info.json` entry whose `git.sha1`
// field is set to vcsSHA. Mirrors `cargo publish`'s output layout
// at the canonical depth (depth-1 under the wrapping directory) so
// the artifact CaptureIntent's matcher accepts it.
//
// payloadOverride, when non-empty, replaces the canonical JSON with
// the supplied bytes — used by the negative tests to inject
// malformed JSON or off-shape values.
//
// entryPathOverride, when non-empty, replaces the canonical
// `<name>-<version>/.cargo_vcs_info.json` path — used by the
// negative tests to verify the depth-squat case (file at depth ≥ 2)
// is filtered.
func buildCrateTarball(t *testing.T, name, version, vcsSHA, payloadOverride, entryPathOverride string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	payload := payloadOverride
	if payload == "" {
		payload = fmt.Sprintf(`{"git":{"sha1":%q},"path_in_vcs":""}`, vcsSHA)
	}
	path := entryPathOverride
	if path == "" {
		path = fmt.Sprintf("%s-%s/.cargo_vcs_info.json", name, version)
	}
	hdr := &tar.Header{
		Name: path,
		Mode: 0o644,
		Size: int64(len(payload)),
	}
	require.NoError(t, tw.WriteHeader(hdr))
	_, err := tw.Write([]byte(payload))
	require.NoError(t, err)

	// Add a decoy file so the walker sees more than one entry — proves
	// the discard-non-matching-bytes path doesn't break the capture.
	decoy := []byte("// not a real Cargo.toml\n")
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: fmt.Sprintf("%s-%s/Cargo.toml.orig", name, version),
		Mode: 0o644,
		Size: int64(len(decoy)),
	}))
	_, err = tw.Write(decoy)
	require.NoError(t, err)

	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// initRepoWithTags builds a git repo at a fresh tempdir with one
// commit per tag. Returns the clone path and a tag→SHA map. Minimal
// version of internal/signal/source's initRepoWithVersionedProgression —
// scoped to what the pin-table tests need (tag refs that
// `git rev-parse --verify ^{commit}` will resolve).
func initRepoWithTags(t *testing.T, tags []string) (clonePath string, shaByTag map[string]string) {
	t.Helper()
	clonePath = t.TempDir()
	runGit := func(args ...string) {
		full := append([]string{"-C", clonePath}, args...)
		cmd := exec.Command("git", full...) //nolint:gosec // G204: test helper, args are constants/tags
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %v failed: %s", args, out)
	}
	runGit("init", "-b", "main", "-q")
	runGit("config", "user.email", "test@example.invalid")
	runGit("config", "user.name", "Test")
	runGit("config", "commit.gpgsign", "false")
	runGit("config", "tag.gpgSign", "false")

	shaByTag = make(map[string]string, len(tags))
	for i, tag := range tags {
		require.NoError(t, os.WriteFile(
			filepath.Join(clonePath, "VERSION"),
			[]byte(tag+"\n"),
			0o600))
		runGit("add", "-A")
		runGit("commit", "-m", "commit "+tag, "-q")
		runGit("tag", tag)
		out, err := exec.Command("git", "-C", clonePath, "rev-parse", "HEAD").Output() //nolint:gosec // G204: test helper
		require.NoError(t, err)
		shaByTag[tag] = strings.TrimSpace(string(out))
		_ = i
	}
	return clonePath, shaByTag
}

// findEmittedPinTable extracts the version_pin_table signal value
// from a CollectionResult, failing the test if the signal isn't
// present. Returns the parsed wire value plus the raw signal for
// metadata assertions.
func findEmittedPinTable(t *testing.T, result *signal.CollectionResult) (versionPinTableValue, profile.Signal) {
	t.Helper()
	for _, s := range result.Signals() {
		if s.Type == "version_pin_table" {
			var v versionPinTableValue
			require.NoError(t, json.Unmarshal(s.Value, &v))
			return v, s
		}
	}
	t.Fatalf("version_pin_table not found in result; got %d signals", len(result.Signals()))
	return versionPinTableValue{}, profile.Signal{}
}

// hasAbsenceForPinTable reports whether the result holds an absence
// record for version_pin_table.
func hasAbsenceForPinTable(result *signal.CollectionResult) bool {
	for _, s := range result.Signals() {
		if s.Type == "absence:version_pin_table" {
			return true
		}
	}
	return false
}

// ============================================================
// Skip / absence cases
// ============================================================

func TestPinTableCollector_Name(t *testing.T) {
	t.Parallel()
	c := NewPinTableCollector(NewClient(), "")
	assert.Equal(t, "cargo-registry", c.Name(),
		"pin-table collector shares the cargo-registry source label "+
			"so the analyst-facing source string is uniform across "+
			"cargo signals")
}

func TestPinTableCollector_NonCargoEntity_EmptyResult(t *testing.T) {
	t.Parallel()
	c := NewPinTableCollector(NewClient(), "/tmp/some-clone")
	entity := &profile.Entity{
		ID: "ent-npm", CanonicalURI: "pkg:npm/express", Ecosystem: "npm",
	}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	assert.Equal(t, 0, result.SignalCount())
	assert.Equal(t, 0, result.AbsenceCount(),
		"non-cargo entity must self-gate silently — no absence either")
}

func TestPinTableCollector_NilEntity_EmptyResult(t *testing.T) {
	t.Parallel()
	c := NewPinTableCollector(NewClient(), "/tmp/some-clone")
	result, err := c.Collect(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 0, result.SignalCount())
}

func TestPinTableCollector_NoClonePath_RecordsAbsence(t *testing.T) {
	t.Parallel()
	c := NewPinTableCollector(NewClient(), "")
	entity := &profile.Entity{
		ID: "ent-serde", CanonicalURI: "pkg:cargo/serde", Ecosystem: "cargo",
	}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	assert.True(t, hasAbsenceForPinTable(result),
		"empty clone path must emit a clear absence so the operator "+
			"knows --clone is required")
	assert.Equal(t, 0, result.SignalCount())
}

func TestPinTableCollector_CratesIOFails_RecordsRetryableFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	clonePath, _ := initRepoWithTags(t, []string{"v0.1.0"})
	client := NewClientWithBaseURL(srv.URL)
	c := NewPinTableCollector(client, clonePath)

	entity := &profile.Entity{
		ID: "ent-serde", CanonicalURI: "pkg:cargo/serde", Ecosystem: "cargo",
	}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	assert.NotEmpty(t, result.Failures,
		"5xx from crates.io must surface as a retryable failure record, "+
			"not a silent absence")
}

func TestPinTableCollector_CratesIONotFound_RecordsNonRetryableFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	clonePath, _ := initRepoWithTags(t, []string{"v0.1.0"})
	client := NewClientWithBaseURL(srv.URL)
	c := NewPinTableCollector(client, clonePath)

	entity := &profile.Entity{
		ID: "ent-ghost", CanonicalURI: "pkg:cargo/ghost", Ecosystem: "cargo",
	}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	// 404 → ErrNotFound → non-retryable failure (operator's
	// retry won't bring a non-existent crate back).
	require.NotEmpty(t, result.Failures)
	for _, f := range result.Failures {
		assert.False(t, f.Retryable,
			"ErrNotFound must be non-retryable")
	}
}

// ============================================================
// Happy path: tag-match resolution
// ============================================================

// TestPinTableCollector_TagMatchHappyPath is the load-bearing test
// for the cargo pin-table emitter. It builds a real git repo with
// three v-prefixed tags, stands up a crates.io httptest returning
// matching versions, and asserts:
//
//   - version_pin_table lands as a signal (not an absence)
//   - module_path equals the trust-boundary-validated entity name
//   - VersionCountTotal counts every version (incl. yanked)
//   - VersionCountProcessed counts only orderable, non-yanked
//   - Pins[].SHA matches the actual rev-parsed SHA from the clone
//   - Pins[].Source == "cargo-tag-match"
//   - Pins[].PublishedAt is RFC3339 UTC
func TestPinTableCollector_TagMatchHappyPath(t *testing.T) {
	t.Parallel()

	clonePath, shaByTag := initRepoWithTags(t, []string{
		"v0.1.0", "v0.2.0", "v0.3.0",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/crates/synth" {
			cr := CrateResponse{
				Crate: Crate{Name: "synth", Repository: "https://github.com/example/synth"},
				Versions: []Version{
					{Num: "0.3.0", CreatedAt: "2026-03-01T10:00:00Z"},
					{Num: "0.2.0", CreatedAt: "2026-02-01T10:00:00Z"},
					{Num: "0.1.0", CreatedAt: "2026-01-01T10:00:00Z"},
				},
			}
			json.NewEncoder(w).Encode(cr) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewPinTableCollector(client, clonePath)

	entity := &profile.Entity{
		ID: "ent-synth", CanonicalURI: "pkg:cargo/synth", Ecosystem: "cargo",
	}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	assert.Empty(t, result.Failures, "no failures on the happy path")

	pt, sig := findEmittedPinTable(t, result)
	assert.Equal(t, profile.SignalGroupPublication, sig.Group)
	assert.Equal(t, profile.ForgeryVeryHigh, sig.ForgeryResistance)
	assert.Equal(t, "cargo-registry", sig.Source)

	assert.Equal(t, "synth", pt.ModulePath,
		"module_path must be the trust-boundary-validated entity name, "+
			"not the registry-supplied Crate.Name (which is publisher-controlled)")
	assert.Equal(t, 3, pt.VersionCountTotal)
	assert.Equal(t, 3, pt.VersionCountProcessed)
	require.Len(t, pt.Pins, 3)
	assert.Empty(t, pt.MissingOriginVersions)

	byVersion := map[string]versionPin{}
	for _, p := range pt.Pins {
		byVersion[p.Version] = p
	}
	for _, ver := range []string{"0.1.0", "0.2.0", "0.3.0"} {
		pin, ok := byVersion[ver]
		require.True(t, ok, "pin for %s missing", ver)
		assert.Equal(t, shaByTag["v"+ver], pin.SHA,
			"pin SHA for %s must match rev-parse on the v-prefixed tag", ver)
		assert.Equal(t, "cargo-tag-match", pin.Source,
			"every tag-resolved pin must be labeled cargo-tag-match")
		_, err := time.Parse(time.RFC3339, pin.PublishedAt)
		assert.NoError(t, err,
			"PublishedAt must be RFC3339 (smoke driver asserts the same)")
	}
}

// TestPinTableCollector_BareVersionTags_AlsoResolve covers older
// crates whose release tags don't use the v-prefix. Both forms must
// resolve; the artifact pair-resolver's "v-prefix first" precedence
// is preserved.
func TestPinTableCollector_BareVersionTags_AlsoResolve(t *testing.T) {
	t.Parallel()

	clonePath, shaByTag := initRepoWithTags(t, []string{
		"0.1.0", "0.2.0",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/crates/synth" {
			cr := CrateResponse{
				Crate: Crate{Name: "synth"},
				Versions: []Version{
					{Num: "0.2.0", CreatedAt: "2026-02-01T10:00:00Z"},
					{Num: "0.1.0", CreatedAt: "2026-01-01T10:00:00Z"},
				},
			}
			json.NewEncoder(w).Encode(cr) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewPinTableCollector(NewClientWithBaseURL(srv.URL), clonePath)
	entity := &profile.Entity{
		ID: "ent-synth", CanonicalURI: "pkg:cargo/synth", Ecosystem: "cargo",
	}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	pt, _ := findEmittedPinTable(t, result)
	require.Len(t, pt.Pins, 2, "both bare-version tags must resolve")
	for _, p := range pt.Pins {
		assert.Equal(t, shaByTag[p.Version], p.SHA,
			"bare-version tag %s must resolve to its own commit", p.Version)
	}
}

// TestPinTableCollector_UnmatchedVersion_GoesToMissingOrigin covers
// the version-not-tagged-locally case. The crate publishes 0.3.0
// but no v0.3.0 or 0.3.0 tag exists in the clone (force-pushed
// history, sparse-tag publisher, or a freshly-published version
// not yet pulled). The pin table must include the resolved
// versions as pins AND the unresolved one in missing_origin, not
// drop or fail.
func TestPinTableCollector_UnmatchedVersion_GoesToMissingOrigin(t *testing.T) {
	t.Parallel()

	clonePath, _ := initRepoWithTags(t, []string{
		"v0.1.0", "v0.2.0", // No v0.3.0 in the clone.
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/crates/synth" {
			cr := CrateResponse{
				Crate: Crate{Name: "synth"},
				Versions: []Version{
					{Num: "0.3.0", CreatedAt: "2026-03-01T10:00:00Z"},
					{Num: "0.2.0", CreatedAt: "2026-02-01T10:00:00Z"},
					{Num: "0.1.0", CreatedAt: "2026-01-01T10:00:00Z"},
				},
			}
			json.NewEncoder(w).Encode(cr) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewPinTableCollector(NewClientWithBaseURL(srv.URL), clonePath)
	entity := &profile.Entity{
		ID: "ent-synth", CanonicalURI: "pkg:cargo/synth", Ecosystem: "cargo",
	}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	pt, _ := findEmittedPinTable(t, result)
	assert.Equal(t, 3, pt.VersionCountTotal)
	assert.Equal(t, 3, pt.VersionCountProcessed,
		"every non-yanked, time-orderable version counts as processed, "+
			"whether or not the tag resolves")
	assert.Len(t, pt.Pins, 2, "0.1.0 and 0.2.0 must resolve")
	assert.True(t, slices.Contains(pt.MissingOriginVersions, "0.3.0"),
		"0.3.0 must land in missing_origin since no tag exists for it")
}

// TestPinTableCollector_YankedVersion_Excluded confirms yanked
// versions are filtered before pin emission — same discipline as
// the cargo registry collector's recentVersionsByPublishTime
// helper. A yanked version is neither pinned nor counted as
// processed.
func TestPinTableCollector_YankedVersion_Excluded(t *testing.T) {
	t.Parallel()

	clonePath, _ := initRepoWithTags(t, []string{"v0.1.0", "v0.2.0"})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/crates/synth" {
			cr := CrateResponse{
				Crate: Crate{Name: "synth"},
				Versions: []Version{
					{Num: "0.2.0", CreatedAt: "2026-02-01T10:00:00Z", Yanked: true},
					{Num: "0.1.0", CreatedAt: "2026-01-01T10:00:00Z"},
				},
			}
			json.NewEncoder(w).Encode(cr) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewPinTableCollector(NewClientWithBaseURL(srv.URL), clonePath)
	entity := &profile.Entity{
		ID: "ent-synth", CanonicalURI: "pkg:cargo/synth", Ecosystem: "cargo",
	}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	pt, _ := findEmittedPinTable(t, result)
	assert.Equal(t, 2, pt.VersionCountTotal,
		"VersionCountTotal counts ALL versions including yanked — "+
			"matches gopublish semantics so the analyst can see "+
			"how many releases the registry knows about")
	assert.Equal(t, 1, pt.VersionCountProcessed,
		"only the non-yanked version was processed")
	require.Len(t, pt.Pins, 1)
	assert.Equal(t, "0.1.0", pt.Pins[0].Version,
		"only 0.1.0 should be pinned; 0.2.0 was yanked")
}

// TestPinTableCollector_UnparseableCreatedAt_Skipped covers the
// version-with-bad-timestamp case. The chronological axis is
// load-bearing (source-evolution's matrix orders rows by
// published_at), so a version we can't place in time is one we
// can't anchor a row on — same discipline npm applies (skip on
// missing time entry). The version isn't counted as processed and
// doesn't appear in pins or missing_origin.
func TestPinTableCollector_UnparseableCreatedAt_Skipped(t *testing.T) {
	t.Parallel()

	clonePath, _ := initRepoWithTags(t, []string{"v0.1.0"})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/crates/synth" {
			cr := CrateResponse{
				Crate: Crate{Name: "synth"},
				Versions: []Version{
					{Num: "0.1.0", CreatedAt: "2026-01-01T10:00:00Z"},
					{Num: "0.0.1-corrupt", CreatedAt: "not-a-timestamp"},
				},
			}
			json.NewEncoder(w).Encode(cr) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewPinTableCollector(NewClientWithBaseURL(srv.URL), clonePath)
	entity := &profile.Entity{
		ID: "ent-synth", CanonicalURI: "pkg:cargo/synth", Ecosystem: "cargo",
	}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	pt, _ := findEmittedPinTable(t, result)
	assert.Equal(t, 2, pt.VersionCountTotal)
	assert.Equal(t, 1, pt.VersionCountProcessed,
		"the unparseable-timestamp version is silently dropped, "+
			"not promoted to missing_origin")
	require.Len(t, pt.Pins, 1)
	assert.Equal(t, "0.1.0", pt.Pins[0].Version)
	assert.NotContains(t, pt.MissingOriginVersions, "0.0.1-corrupt",
		"a version we can't place in time is one we can't anchor — "+
			"not pinned, not missing-origin, just dropped")
}

// ============================================================
// vcs_info tarball upgrade pass
// ============================================================

// TestPinTableCollector_VCSInfoUpgrade_OverridesTagMatch is the
// load-bearing test for the second pin-source tier. Builds three
// version tags (all resolving via tag-match to one SHA each), an
// httptest server that serves both the crate JSON AND per-version
// .crate tarballs whose `.cargo_vcs_info.json` carries a DIFFERENT
// SHA, and asserts:
//
//   - Each pin's SHA is upgraded to the vcs_info SHA (publisher-
//     stamped, stronger provenance) instead of the tag-match SHA
//   - Each pin's Source field is restamped from cargo-tag-match to
//     cargo-vcs-info
//
// The upgrade is "silent override" per the user-confirmed Option A —
// the tag-match SHA is replaced without emitting a divergence signal.
func TestPinTableCollector_VCSInfoUpgrade_OverridesTagMatch(t *testing.T) {
	t.Parallel()

	clonePath, _ := initRepoWithTags(t, []string{
		"v0.1.0", "v0.2.0", "v0.3.0",
	})

	// Hand-picked vcs_info SHAs distinct from the local clone's tag
	// SHAs — so the test PROVES the upgrade replaces the values, not
	// just stamps a different label on the same bytes.
	const (
		vcsSHA010 = "1111111111111111111111111111111111111111"
		vcsSHA020 = "2222222222222222222222222222222222222222"
		vcsSHA030 = "3333333333333333333333333333333333333333"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/crates/synth":
			w.Header().Set("Content-Type", "application/json")
			cr := CrateResponse{
				Crate: Crate{Name: "synth"},
				Versions: []Version{
					{Num: "0.3.0", CreatedAt: "2026-03-01T10:00:00Z"},
					{Num: "0.2.0", CreatedAt: "2026-02-01T10:00:00Z"},
					{Num: "0.1.0", CreatedAt: "2026-01-01T10:00:00Z"},
				},
			}
			json.NewEncoder(w).Encode(cr) //nolint:errcheck
		case r.URL.Path == "/crates/synth/0.3.0/download":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(buildCrateTarball(t, "synth", "0.3.0", vcsSHA030, "", ""))
		case r.URL.Path == "/crates/synth/0.2.0/download":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(buildCrateTarball(t, "synth", "0.2.0", vcsSHA020, "", ""))
		case r.URL.Path == "/crates/synth/0.1.0/download":
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(buildCrateTarball(t, "synth", "0.1.0", vcsSHA010, "", ""))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewPinTableCollector(NewClientWithBaseURL(srv.URL), clonePath).
		WithTarballBaseURL(srv.URL)

	entity := &profile.Entity{
		ID: "ent-synth", CanonicalURI: "pkg:cargo/synth", Ecosystem: "cargo",
	}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	pt, _ := findEmittedPinTable(t, result)
	require.Len(t, pt.Pins, 3)

	want := map[string]string{
		"0.1.0": vcsSHA010,
		"0.2.0": vcsSHA020,
		"0.3.0": vcsSHA030,
	}
	for _, pin := range pt.Pins {
		assert.Equal(t, "cargo-vcs-info", pin.Source,
			"version %s: SHA must be re-stamped cargo-vcs-info after the upgrade", pin.Version)
		assert.Equal(t, want[pin.Version], pin.SHA,
			"version %s: SHA must be the publisher-stamped vcs_info value, not the tag SHA", pin.Version)
	}
}

// TestPinTableCollector_VCSInfoUpgrade_SilentFallback covers the
// four failure modes for the tarball fetch / parse:
//
//   - HTTP 404 (tarball missing from the CDN)
//   - Tarball decompresses but vcs_info file is absent
//   - vcs_info file present but malformed JSON
//   - vcs_info file present, JSON valid, sha1 not 40-hex-lowercase
//   - vcs_info file at depth > 1 (squat attempt) — capture intent
//     filters it
//
// Each must leave the corresponding pin on the tag-match SHA with
// Source==cargo-tag-match. The upgrade STRICTLY upgrades; it never
// demotes or removes an existing pin.
func TestPinTableCollector_VCSInfoUpgrade_SilentFallback(t *testing.T) {
	t.Parallel()

	clonePath, shaByTag := initRepoWithTags(t, []string{
		"v0.1.0", "v0.2.0", "v0.3.0", "v0.4.0", "v0.5.0",
	})

	const goodSHA = "4444444444444444444444444444444444444444"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/crates/synth":
			w.Header().Set("Content-Type", "application/json")
			cr := CrateResponse{
				Crate: Crate{Name: "synth"},
				Versions: []Version{
					{Num: "0.5.0", CreatedAt: "2026-05-01T10:00:00Z"},
					{Num: "0.4.0", CreatedAt: "2026-04-01T10:00:00Z"},
					{Num: "0.3.0", CreatedAt: "2026-03-01T10:00:00Z"},
					{Num: "0.2.0", CreatedAt: "2026-02-01T10:00:00Z"},
					{Num: "0.1.0", CreatedAt: "2026-01-01T10:00:00Z"},
				},
			}
			json.NewEncoder(w).Encode(cr) //nolint:errcheck
		case r.URL.Path == "/crates/synth/0.5.0/download":
			// HTTP 404 — tarball missing
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/crates/synth/0.4.0/download":
			// Tarball decompresses but contains no vcs_info — only a
			// decoy file at the depth-1 location.
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(buildTarballWithoutVCSInfo(t, "synth", "0.4.0"))
		case r.URL.Path == "/crates/synth/0.3.0/download":
			// vcs_info present but malformed JSON.
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(buildCrateTarball(t, "synth", "0.3.0", "", `not-valid-json`, ""))
		case r.URL.Path == "/crates/synth/0.2.0/download":
			// vcs_info JSON shape valid but sha1 is not 40-hex-lower.
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(buildCrateTarball(t, "synth", "0.2.0", "",
				`{"git":{"sha1":"NOT-A-REAL-SHA"}}`, ""))
		case r.URL.Path == "/crates/synth/0.1.0/download":
			// vcs_info at depth 2 (squat attempt) — intent filter rejects.
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(buildCrateTarball(t, "synth", "0.1.0", goodSHA, "",
				"synth-0.1.0/src/.cargo_vcs_info.json"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewPinTableCollector(NewClientWithBaseURL(srv.URL), clonePath).
		WithTarballBaseURL(srv.URL)

	entity := &profile.Entity{
		ID: "ent-synth", CanonicalURI: "pkg:cargo/synth", Ecosystem: "cargo",
	}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	pt, _ := findEmittedPinTable(t, result)
	require.Len(t, pt.Pins, 5, "every version must stay pinned — no demotion to missing_origin")

	// Every pin must remain on tag-match with the LOCAL clone's SHA;
	// none of the failure modes above should have promoted to
	// cargo-vcs-info or replaced the SHA.
	for _, pin := range pt.Pins {
		assert.Equalf(t, "cargo-tag-match", pin.Source,
			"version %s: must stay cargo-tag-match after a fallback case", pin.Version)
		wantSHA := shaByTag["v"+pin.Version]
		assert.Equalf(t, wantSHA, pin.SHA,
			"version %s: SHA must be the tag-resolved clone SHA, not the failed-fetch vcs_info value",
			pin.Version)
	}
}

// buildTarballWithoutVCSInfo builds a .crate-shape tarball whose
// depth-1 root has files OTHER than .cargo_vcs_info.json. Used by
// the silent-fallback test to verify the "no vcs_info captured"
// path — Manifest.Captured lookup misses cleanly.
func buildTarballWithoutVCSInfo(t *testing.T, name, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, entry := range []struct {
		path, body string
	}{
		{path: name + "-" + version + "/Cargo.toml.orig", body: "// no vcs_info here\n"},
		{path: name + "-" + version + "/src/lib.rs", body: "pub fn hello() {}\n"},
	} {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: entry.path,
			Mode: 0o644,
			Size: int64(len(entry.body)),
		}))
		_, err := tw.Write([]byte(entry.body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// TestPinTableCollector_VCSInfoUpgrade_BoundedByWindow confirms the
// upgrade pass only fetches tarballs for the recent
// crossVersionWindow (10) — older versions stay on tag-match
// unconditionally, even if a tarball would have been available.
// Guards the bound; a regression that walked ALL versions on a
// large crate (serde has ~315) would balloon analyze time and
// trigger crates.io rate limits.
func TestPinTableCollector_VCSInfoUpgrade_BoundedByWindow(t *testing.T) {
	t.Parallel()

	// 12 versions — 2 beyond the crossVersionWindow=10 bound.
	// Month numbers 1..12 map to versions 0.1.0..0.12.0 so the
	// CreatedAt parseable-date check never trips on the test data
	// (a month-13 bug here would silently drop the newest version).
	tags := make([]string, 12)
	versions := make([]Version, 12)
	for i := range 12 {
		month := 12 - i // i=0 → 12 (newest), i=11 → 1 (oldest)
		ver := fmt.Sprintf("0.%d.0", month)
		tags[i] = "v" + ver
		versions[i] = Version{
			Num:       ver,
			CreatedAt: fmt.Sprintf("2026-%02d-01T10:00:00Z", month),
		}
	}
	clonePath, _ := initRepoWithTags(t, tags)

	// Track which tarball paths the server received — used to assert
	// the upgrade pass ONLY fetched the recent N.
	var fetched []string
	const vcsSHA = "5555555555555555555555555555555555555555"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/crates/synth":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CrateResponse{ //nolint:errcheck
				Crate: Crate{Name: "synth"}, Versions: versions,
			})
		case strings.HasPrefix(r.URL.Path, "/crates/synth/") && strings.HasSuffix(r.URL.Path, "/download"):
			fetched = append(fetched, r.URL.Path)
			ver := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/crates/synth/"), "/download")
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(buildCrateTarball(t, "synth", ver, vcsSHA, "", ""))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewPinTableCollector(NewClientWithBaseURL(srv.URL), clonePath).
		WithTarballBaseURL(srv.URL)
	entity := &profile.Entity{
		ID: "ent-synth", CanonicalURI: "pkg:cargo/synth", Ecosystem: "cargo",
	}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	pt, _ := findEmittedPinTable(t, result)
	require.Len(t, pt.Pins, 12)

	// The crossVersionWindow constant is 10 (defined in collector.go);
	// pin to the precise count so a regression that walked all 12
	// versions or only 5 surfaces here as a counter mismatch.
	assert.Lenf(t, fetched, 10,
		"upgrade pass must fetch exactly crossVersionWindow=10 tarballs, "+
			"got %d (paths: %v)", len(fetched), fetched)

	// Newest-first sort means pins[0:10] are the upgraded window,
	// pins[10:] stay on tag-match.
	for i := range 10 {
		assert.Equalf(t, "cargo-vcs-info", pt.Pins[i].Source,
			"pin[%d] (version %s) is inside the upgrade window — must be cargo-vcs-info",
			i, pt.Pins[i].Version)
	}
	for i := 10; i < 12; i++ {
		assert.Equalf(t, "cargo-tag-match", pt.Pins[i].Source,
			"pin[%d] (version %s) is OUTSIDE the upgrade window — must stay cargo-tag-match",
			i, pt.Pins[i].Version)
	}
}
