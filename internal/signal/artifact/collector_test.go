package artifact

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// TestCollector_Collect_AbsenceWhenNoArtifactURL is the smallest
// integration: when the in-run accumulator carries no artifact_url
// signal for the entity (the upstream registry collector either
// didn't run or had no dist.tarball to emit), the artifact collector
// records a positive_absence for artifact_repo_divergence carrying
// AbsenceReasonNoArtifactURL.
//
// This is the "graceful no-op" branch that lets the dispatch path
// schedule the collector unconditionally on (registry-shaped, git-
// hosted) entities without having to pre-check upstream output. The
// absence row is itself meaningful — it says "we tried, the registry
// side wasn't there" — and lands in the entity's profile so the
// synthesist can distinguish it from "this collector didn't run."
func TestCollector_Collect_AbsenceWhenNoArtifactURL(t *testing.T) {
	t.Parallel()

	// Empty in-run result: no upstream artifact_url emission.
	// Construction mirrors what the orchestrator passes today.
	inRun := &signal.CollectionResult{}

	// Clone path is irrelevant when there's no URL — but supply
	// one to verify the absence path doesn't depend on it.
	collector := NewCollector(CollectorConfig{
		InRun:     inRun,
		ClonePath: "/tmp/some/clone/path",
	})

	entity := &profile.Entity{
		ID:           "e-test",
		CanonicalURI: "pkg:npm/test-pkg",
		Type:         profile.EntityPackage,
		Ecosystem:    "npm",
	}

	result, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err,
		"missing upstream input is not a collection failure — the "+
			"absence row IS the result")
	require.NotNil(t, result)

	// Exactly one absence, no real signals. The absence carries the
	// documented wire-contract reason so the synthesist can
	// distinguish "no URL available" from "couldn't pair tarball."
	assert.Equal(t, 0, result.SignalCount(),
		"no real signals when there's no URL to fetch")
	assert.Equal(t, 1, result.AbsenceCount(),
		"exactly one absence row: no_artifact_url")

	// Absence is recorded against the divergence signal type, not
	// against artifact_url itself — the npm collector owns
	// artifact_url's absence; we own divergence's absence.
	var found bool
	var reason string
	for _, s := range result.Signals() {
		if s.Type == "absence:artifact_repo_divergence" {
			found = true
			// Absence value carries reason in a "reason" field by
			// signal.RecordAbsence's convention.
			reason = string(s.Value)
		}
	}
	require.True(t, found,
		"absence row must be on artifact_repo_divergence — that's the "+
			"signal whose absence the synthesist looks up to know whether "+
			"the comparison was attempted")
	assert.Contains(t, reason, AbsenceReasonNoArtifactURL,
		"absence reason payload must include the documented "+
			"AbsenceReasonNoArtifactURL string verbatim")
}

// TestCollector_Collect_HappyPathEmitsDivergenceSignal is the
// canonical end-to-end test: in-run carries an artifact_url, the
// stub fetcher returns an xz-shaped tarball, the stub git inspector
// returns a tag list and a path list missing the malicious m4 file,
// and the collector emits artifact_repo_divergence with the right
// payload shape.
//
// Stubs let this run as a unit test (no httptest, no real git
// fixture). The HTTP-fetcher and gitenv-inspector implementations
// have their own tests (8c / 8d).
func TestCollector_Collect_HappyPathEmitsDivergenceSignal(t *testing.T) {
	t.Parallel()

	const (
		entityID = "e-xz"
		version  = "5.6.1"
		tag      = "v5.6.1"
		commit   = "deadbeefcafebabe1234567890abcdef12345678"
	)

	// Build the xz-shaped tarball: three benign files plus the
	// malicious m4. Same fixture shape as TestCompare_XZShapedFixture.
	tarball := buildTarGz(t, []tarEntry{
		{path: "src/lzma_decoder.c", body: []byte("// real source")},
		{path: "configure.ac", body: []byte("AC_INIT([xz], [5.6.1])")},
		{path: "m4/build-to-host.m4", body: []byte("# attacker payload")},
	})
	gitPaths := []string{
		"src/lzma_decoder.c",
		"configure.ac",
	}

	inRun := &signal.CollectionResult{}
	inRun.RecordSignal(entityID, "artifact_url", "npm-registry",
		mustParseTime(t, "2026-04-01T00:00:00Z"), 24*time.Hour,
		map[string]any{
			"url":       "https://example.invalid/xz-5.6.1.tar.gz",
			"version":   version,
			"git_head":  "", // force tag-match path
			"integrity": "sha512-AAAA",
		})

	collector := NewCollector(CollectorConfig{
		InRun:     inRun,
		ClonePath: "/fake/clone",
		Fetcher:   &stubFetcher{body: tarball},
		Git: &stubGit{
			tags:        []string{"v5.6.0", tag},
			pathsByRef:  map[string][]string{tag: gitPaths},
			commitByRef: map[string]string{tag: commit},
		},
	})

	entity := &profile.Entity{
		ID:           entityID,
		CanonicalURI: "pkg:npm/xz-utils",
		Type:         profile.EntityPackage,
		Ecosystem:    "npm",
	}

	result, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err)

	assert.Equal(t, 1, result.SignalCount(),
		"happy path emits exactly one artifact_repo_divergence signal")
	assert.Equal(t, 0, result.AbsenceCount(),
		"no absences on happy path")

	// Pull the signal out and verify the payload shape.
	var sig profile.Signal
	for _, s := range result.Signals() {
		if s.Type == "artifact_repo_divergence" {
			sig = s
			break
		}
	}
	require.NotEmpty(t, sig.Type,
		"artifact_repo_divergence signal must be present")

	var cmp Comparison
	require.NoError(t, json.Unmarshal(sig.Value, &cmp))

	assert.Equal(t, "https://example.invalid/xz-5.6.1.tar.gz", cmp.ArtifactURL)
	assert.Equal(t, tag, cmp.GitRef)
	assert.Equal(t, commit, cmp.GitCommit,
		"commit must be resolved via stubGit.CommitForRef when ResolvePair "+
			"returned a tag-match (no commit) — exercises the lookup branch")
	assert.Equal(t, PairConfidenceTagMatch, cmp.PairConfidence)
	assert.Equal(t, 1, cmp.ExtrasInTarballCount,
		"the m4 payload is the one tarball-only file in this fixture")

	// And the headline fact: the malicious file shows up in the
	// sample with the right category. Same assertion shape as
	// TestCompare_XZShapedFixture but flowing through the full
	// collector path.
	var found bool
	for _, e := range cmp.ExtrasInTarballSample {
		if e.Path == "m4/build-to-host.m4" {
			found = true
			assert.Equal(t, CategoryBuildGlue, e.Category)
		}
	}
	assert.True(t, found,
		"m4/build-to-host.m4 must surface through the full collector path "+
			"with category=build_glue — the load-bearing CVE-2024-3094 fact")
}

// TestCollector_Collect_Cargo_VCSInfoRescuesPair is the canonical
// cargo case: registry metadata exposes no gitHead (crates.io
// doesn't carry one), and the version string is "0.0.0-dev" with
// no matching tag in the repo. Without vcs_info recovery the
// pairing would fail and the collector would emit an absence.
//
// The collector must:
//
//  1. Register a CaptureIntent for .cargo_vcs_info.json when the
//     entity ecosystem is cargo.
//  2. Walk the tarball, capturing the vcs_info bytes under the
//     intent's MaxSize cap.
//  3. Parse git.sha1 from the captured JSON.
//  4. Use that SHA as the effective gitHead for ResolvePair, so
//     the pairing succeeds with PairConfidenceExactGitHead even
//     when registry metadata is silent.
//  5. Surface the same divergence signal shape as the npm path:
//     the extra-file (cargo-equivalent of CVE-2024-3094's
//     build-to-host.m4) appears in extras_in_tarball_sample.
//
// This is the load-bearing test for B1 — closes the same
// signal-gap as A1's Phase 1 work (cargo registry collector emits
// artifact_url) but on the consumption side.
func TestCollector_Collect_Cargo_VCSInfoRescuesPair(t *testing.T) {
	t.Parallel()

	const (
		entityID = "e-cargo"
		version  = "0.0.0-dev" // deliberately doesn't match any tag
		// 40-char hex SHA — must match what the stubGit advertises
		// against vcsInfoSHA so the pairing resolves.
		vcsInfoSHA = "abcdef0123456789abcdef0123456789abcdef01"
	)

	// Build a cargo-shaped tarball: .cargo_vcs_info.json at the
	// archive root (typical for .crate format which uses
	// "<name>-<version>/" as the wrapping directory; the walker's
	// strip detection handles the wrapper, leaving the file at
	// the root for intent matching).
	vcsInfo := `{"git":{"sha1":"` + vcsInfoSHA + `"},"path_in_vcs":""}`
	tarball := buildTarGz(t, []tarEntry{
		{path: "mycrate-0.0.0-dev/Cargo.toml", body: []byte("[package]\nname=\"mycrate\"")},
		{path: "mycrate-0.0.0-dev/src/lib.rs", body: []byte("// real source")},
		{path: "mycrate-0.0.0-dev/.cargo_vcs_info.json", body: []byte(vcsInfo)},
		// The xz-equivalent: a file in the tarball that's NOT in
		// the git tree at vcsInfoSHA. Surface in divergence.
		{path: "mycrate-0.0.0-dev/build/inject.sh", body: []byte("#!/bin/sh\n# attacker payload")},
	})

	// gitPaths reflect what `git ls-tree -r --name-only` at the
	// vcs_info SHA returns: the legitimate source, no inject.sh.
	gitPaths := []string{
		"Cargo.toml",
		"src/lib.rs",
	}

	inRun := &signal.CollectionResult{}
	inRun.RecordSignal(entityID, "artifact_url", "cargo-registry",
		mustParseTime(t, "2026-04-01T00:00:00Z"), 24*time.Hour,
		map[string]any{
			"url":       "https://static.crates.io/crates/mycrate/0.0.0-dev/download",
			"version":   version,
			"git_head":  "", // crates.io never supplies this — vcs_info path must rescue
			"integrity": "abcd",
		})

	collector := NewCollector(CollectorConfig{
		InRun:     inRun,
		ClonePath: "/fake/clone",
		Fetcher:   &stubFetcher{body: tarball},
		Git: &stubGit{
			// No tag matches version "0.0.0-dev" — tag-match path
			// would fail. vcs_info SHA must be the rescue.
			tags:        []string{"v1.0.0", "v0.9.0"},
			pathsByRef:  map[string][]string{vcsInfoSHA: gitPaths},
			commitByRef: map[string]string{vcsInfoSHA: vcsInfoSHA},
		},
	})

	entity := &profile.Entity{
		ID:           entityID,
		CanonicalURI: "pkg:cargo/mycrate",
		Type:         profile.EntityPackage,
		Ecosystem:    "cargo",
	}

	result, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err)

	require.Equal(t, 1, result.SignalCount(),
		"vcs_info-rescued pair must produce one divergence signal — "+
			"fall-through to absence here means the cargo-specific intent "+
			"didn't fire or the SHA wasn't extracted")
	assert.Equal(t, 0, result.AbsenceCount(),
		"successful vcs_info rescue must NOT emit any absence row")

	var sig profile.Signal
	for _, s := range result.Signals() {
		if s.Type == "artifact_repo_divergence" {
			sig = s
			break
		}
	}
	require.NotEmpty(t, sig.Type)

	var cmp Comparison
	require.NoError(t, json.Unmarshal(sig.Value, &cmp))

	assert.Equal(t, vcsInfoSHA, cmp.GitCommit,
		"resolved commit must equal the SHA recovered from .cargo_vcs_info.json")
	assert.Equal(t, PairConfidenceExactGitHead, cmp.PairConfidence,
		"vcs_info-derived pairing carries exact_gitHead confidence — same as "+
			"npm's registry-supplied gitHead, since cargo's vcs_info is also "+
			"publisher-attested (written by `cargo publish`, not user input)")
	assert.Equal(t, 1, cmp.ExtrasInTarballCount,
		"build/inject.sh is the one tarball-only file in this fixture")

	var foundInject bool
	for _, e := range cmp.ExtrasInTarballSample {
		if e.Path == "build/inject.sh" {
			foundInject = true
		}
	}
	assert.True(t, foundInject,
		"build/inject.sh must surface as divergence — the cargo-equivalent "+
			"of CVE-2024-3094's build-to-host.m4: a file shipped only in the "+
			"tarball, not present in the git tree at the publisher-stamped SHA")
}

// TestCollector_Collect_Cargo_VCSInfoSquatterIgnored verifies the
// security boundary: an attacker who can write a non-canonical
// .cargo_vcs_info.json into the tarball (at depth ≥ 2) must NOT
// have that file's SHA used as the publisher-attested commit.
//
// The intent's depth-bounded matcher (TestCargoVCSInfoIntent_
// DepthBoundedMatch covers the matcher in isolation) feeds the
// collector's effective-gitHead resolution. Without the depth
// bound, an attacker could publish a crate whose tarball ships
// `mycrate-X/src/.cargo_vcs_info.json` pointing at a clean
// commit while the actual tarball contents diverge from that
// commit's tree — pairing succeeds, divergence vanishes, attack
// hidden.
//
// In this fixture: only a squatted vcs_info exists. Pairing must
// fall through to tag-match (which succeeds, exercising the
// fallback path) and the resulting confidence must be tag_match,
// NOT exact_gitHead — confirming the squatter's SHA was ignored.
func TestCollector_Collect_Cargo_VCSInfoSquatterIgnored(t *testing.T) {
	t.Parallel()

	const (
		entityID = "e-cargo-squatter"
		version  = "1.0.0"
		tag      = "v1.0.0"
		// The legitimate commit the tag points to. Distinct from
		// the SHA the squatter advertises, so we can confirm
		// pairing used the tag, not the squatter.
		legitCommit = "1111111111111111111111111111111111111111"
		// The SHA the squatted vcs_info advertises. If this ever
		// shows up as cmp.GitCommit, the attack succeeded.
		squatterSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	)

	squatterPayload := `{"git":{"sha1":"` + squatterSHA + `"}}`
	tarball := buildTarGz(t, []tarEntry{
		{path: "mycrate-1.0.0/Cargo.toml", body: []byte("[package]")},
		{path: "mycrate-1.0.0/src/lib.rs", body: []byte("// real")},
		// The squatter — at depth 2, must be ignored by the intent.
		{path: "mycrate-1.0.0/src/.cargo_vcs_info.json", body: []byte(squatterPayload)},
	})

	inRun := &signal.CollectionResult{}
	inRun.RecordSignal(entityID, "artifact_url", "cargo-registry",
		mustParseTime(t, "2026-04-01T00:00:00Z"), 24*time.Hour,
		map[string]any{
			"url":      "https://static.crates.io/crates/mycrate/1.0.0/download",
			"version":  version,
			"git_head": "",
		})

	collector := NewCollector(CollectorConfig{
		InRun:     inRun,
		ClonePath: "/fake/clone",
		Fetcher:   &stubFetcher{body: tarball},
		Git: &stubGit{
			tags:        []string{tag},
			pathsByRef:  map[string][]string{tag: {"Cargo.toml", "src/lib.rs"}},
			commitByRef: map[string]string{tag: legitCommit},
		},
	})

	entity := &profile.Entity{
		ID:           entityID,
		CanonicalURI: "pkg:cargo/mycrate",
		Type:         profile.EntityPackage,
		Ecosystem:    "cargo",
	}

	result, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err)
	require.Equal(t, 1, result.SignalCount(),
		"tag-match fallback must produce a divergence signal — "+
			"exact_gitHead via the squatter would also reach this point, "+
			"so the next assertion is what actually catches the attack")

	var sig profile.Signal
	for _, s := range result.Signals() {
		if s.Type == "artifact_repo_divergence" {
			sig = s
			break
		}
	}
	var cmp Comparison
	require.NoError(t, json.Unmarshal(sig.Value, &cmp))

	assert.Equal(t, PairConfidenceTagMatch, cmp.PairConfidence,
		"pairing must come from the tag, NOT from the squatted vcs_info — "+
			"if this is exact_gitHead the depth-bounded match was bypassed "+
			"and the attacker's SHA reached ResolvePair")
	assert.Equal(t, legitCommit, cmp.GitCommit,
		"resolved commit must be the tag's, not the squatter's; "+
			"%q would mean the attack succeeded", squatterSHA)
	assert.NotEqual(t, squatterSHA, cmp.GitCommit,
		"squatter's SHA MUST NOT appear as the resolved commit — "+
			"the depth-bounded intent matcher is the load-bearing defense")
}

// TestCollector_Collect_Cargo_MalformedVCSInfoFallthrough verifies
// graceful handling of a captured-but-unparseable vcs_info file.
// An attacker (or a buggy publisher) could ship a .cargo_vcs_info.json
// that's syntactically broken or has a non-SHA value in the sha1
// field. parseVCSInfoSHA returns ("", false) silently; the
// collector must fall through to tag-match without panicking and
// without surfacing the parse failure as a hard error.
func TestCollector_Collect_Cargo_MalformedVCSInfoFallthrough(t *testing.T) {
	t.Parallel()

	const (
		entityID    = "e-cargo-malformed"
		version     = "2.0.0"
		tag         = "v2.0.0"
		legitCommit = "2222222222222222222222222222222222222222"
	)

	// Garbage JSON in the canonical location (depth 1, root after
	// strip): the intent matches and captures, but parsing fails.
	tarball := buildTarGz(t, []tarEntry{
		{path: "mycrate-2.0.0/Cargo.toml", body: []byte("[package]")},
		{path: "mycrate-2.0.0/src/lib.rs", body: []byte("// real")},
		{path: "mycrate-2.0.0/.cargo_vcs_info.json", body: []byte(`{this is not valid json`)},
	})

	inRun := &signal.CollectionResult{}
	inRun.RecordSignal(entityID, "artifact_url", "cargo-registry",
		mustParseTime(t, "2026-04-01T00:00:00Z"), 24*time.Hour,
		map[string]any{
			"url":      "https://static.crates.io/crates/mycrate/2.0.0/download",
			"version":  version,
			"git_head": "",
		})

	collector := NewCollector(CollectorConfig{
		InRun:     inRun,
		ClonePath: "/fake/clone",
		Fetcher:   &stubFetcher{body: tarball},
		Git: &stubGit{
			tags:        []string{tag},
			pathsByRef:  map[string][]string{tag: {"Cargo.toml", "src/lib.rs"}},
			commitByRef: map[string]string{tag: legitCommit},
		},
	})

	entity := &profile.Entity{
		ID:           entityID,
		CanonicalURI: "pkg:cargo/mycrate",
		Type:         profile.EntityPackage,
		Ecosystem:    "cargo",
	}

	result, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err,
		"malformed vcs_info must NOT propagate as a collector error — "+
			"silent fallthrough to tag-match is the documented contract")

	require.Equal(t, 1, result.SignalCount(),
		"tag-match fallback must succeed; absence here means the parse "+
			"failure surfaced as a hard collector failure rather than fallthrough")

	var sig profile.Signal
	for _, s := range result.Signals() {
		if s.Type == "artifact_repo_divergence" {
			sig = s
			break
		}
	}
	var cmp Comparison
	require.NoError(t, json.Unmarshal(sig.Value, &cmp))
	assert.Equal(t, PairConfidenceTagMatch, cmp.PairConfidence,
		"with vcs_info unparseable, pairing must come from the tag — "+
			"exact_gitHead here would mean some garbage SHA leaked through")
}

// TestCollector_Collect_Cargo_VCSInfoSHAMissingFromClone verifies
// that when the publisher-attested vcs_info SHA isn't present in
// the local clone (e.g. the publisher pushed a commit they later
// rewrote out of history, or the clone is stale), the collector
// records a clean PathsAtRef-failure absence rather than crashing
// or pairing incorrectly.
//
// This exercises the failure path between "vcs_info SHA parsed
// OK" and "PathsAtRef returns error" — it's the realistic shape
// when a registry is fresher than a local mirror.
func TestCollector_Collect_Cargo_VCSInfoSHAMissingFromClone(t *testing.T) {
	t.Parallel()

	const (
		entityID = "e-cargo-staleclone"
		version  = "3.0.0"
		// SHA the vcs_info advertises but that the stub clone
		// doesn't know about.
		unknownSHA = "3333333333333333333333333333333333333333"
	)

	vcsInfo := `{"git":{"sha1":"` + unknownSHA + `"}}`
	tarball := buildTarGz(t, []tarEntry{
		{path: "mycrate-3.0.0/Cargo.toml", body: []byte("[package]")},
		{path: "mycrate-3.0.0/.cargo_vcs_info.json", body: []byte(vcsInfo)},
	})

	inRun := &signal.CollectionResult{}
	inRun.RecordSignal(entityID, "artifact_url", "cargo-registry",
		mustParseTime(t, "2026-04-01T00:00:00Z"), 24*time.Hour,
		map[string]any{
			"url":      "https://static.crates.io/crates/mycrate/3.0.0/download",
			"version":  version,
			"git_head": "",
		})

	collector := NewCollector(CollectorConfig{
		InRun:     inRun,
		ClonePath: "/fake/clone",
		Fetcher:   &stubFetcher{body: tarball},
		Git: &stubGit{
			// No tags AND no entry for unknownSHA in pathsByRef
			// → PathsAtRef returns error for the SHA.
			tags:        []string{},
			pathsByRef:  map[string][]string{},
			commitByRef: map[string]string{},
		},
	})

	entity := &profile.Entity{
		ID:           entityID,
		CanonicalURI: "pkg:cargo/mycrate",
		Type:         profile.EntityPackage,
		Ecosystem:    "cargo",
	}

	result, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err,
		"PathsAtRef failure must surface as absence, NOT as a hard "+
			"collector error — the signal model treats this as a hygiene "+
			"observation about the clone's freshness, not a collection bug")

	assert.Equal(t, 0, result.SignalCount(),
		"no divergence signal when we couldn't read the git tree to "+
			"diff against — emitting a signal with empty diff would be a lie")
	require.Equal(t, 1, result.AbsenceCount(),
		"exactly one absence row recording the read-failure")

	var reason string
	for _, s := range result.Signals() {
		if s.Type == "absence:artifact_repo_divergence" {
			reason = string(s.Value)
		}
	}
	assert.Contains(t, reason, "read git tree",
		"absence reason must point at the PathsAtRef failure for "+
			"operator diagnostics; got: %q", reason)
	assert.Contains(t, reason, unknownSHA,
		"absence reason should include the SHA we tried to read, so "+
			"operators can correlate against their clone state")
}

// TestCollector_Collect_Cargo_VCSInfoBeatsTagMatch is the priority
// test: when a tarball has BOTH a parseable vcs_info AND a tag
// matching the version, the resolved pairing must use the
// vcs_info SHA (exact_gitHead confidence), not the tag (tag_match).
//
// This pins the priority order documented in collector.go:
// registry-supplied gitHead > vcs_info-derived SHA > tag-match.
// Both SHA-based pairings are equally trustworthy in provenance
// terms (publisher-stamped at publish time); both should produce
// exact_gitHead confidence rather than dropping to tag-match.
func TestCollector_Collect_Cargo_VCSInfoBeatsTagMatch(t *testing.T) {
	t.Parallel()

	const (
		entityID   = "e-cargo-priority"
		version    = "4.0.0"
		tag        = "v4.0.0"
		vcsInfoSHA = "4444444444444444444444444444444444444444"
		tagCommit  = "5555555555555555555555555555555555555555"
	)

	vcsInfo := `{"git":{"sha1":"` + vcsInfoSHA + `"}}`
	tarball := buildTarGz(t, []tarEntry{
		{path: "mycrate-4.0.0/Cargo.toml", body: []byte("[package]")},
		{path: "mycrate-4.0.0/.cargo_vcs_info.json", body: []byte(vcsInfo)},
	})

	inRun := &signal.CollectionResult{}
	inRun.RecordSignal(entityID, "artifact_url", "cargo-registry",
		mustParseTime(t, "2026-04-01T00:00:00Z"), 24*time.Hour,
		map[string]any{
			"url":      "https://static.crates.io/crates/mycrate/4.0.0/download",
			"version":  version,
			"git_head": "",
		})

	collector := NewCollector(CollectorConfig{
		InRun:     inRun,
		ClonePath: "/fake/clone",
		Fetcher:   &stubFetcher{body: tarball},
		Git: &stubGit{
			tags: []string{tag},
			// Both routes resolve, but to different commits — so
			// the test can tell which one the collector picked.
			pathsByRef: map[string][]string{
				vcsInfoSHA: {"Cargo.toml"},
				tag:        {"Cargo.toml"},
			},
			commitByRef: map[string]string{
				vcsInfoSHA: vcsInfoSHA,
				tag:        tagCommit,
			},
		},
	})

	entity := &profile.Entity{
		ID:           entityID,
		CanonicalURI: "pkg:cargo/mycrate",
		Type:         profile.EntityPackage,
		Ecosystem:    "cargo",
	}

	result, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err)
	require.Equal(t, 1, result.SignalCount())

	var sig profile.Signal
	for _, s := range result.Signals() {
		if s.Type == "artifact_repo_divergence" {
			sig = s
			break
		}
	}
	var cmp Comparison
	require.NoError(t, json.Unmarshal(sig.Value, &cmp))

	assert.Equal(t, PairConfidenceExactGitHead, cmp.PairConfidence,
		"vcs_info SHA must beat tag-match — both are publisher-stamped "+
			"and the exact-SHA form is documented as higher priority")
	assert.Equal(t, vcsInfoSHA, cmp.GitCommit,
		"resolved commit must be the vcs_info SHA, not the tag's; "+
			"%q here would mean tag-match won when vcs_info should have", tagCommit)
}

// stubFetcher returns the same body for every URL — sufficient
// for tests that exercise one Compare flow.
type stubFetcher struct{ body []byte }

func (s *stubFetcher) Fetch(_ context.Context, _ string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.body)), nil
}

// stubGit serves a tag list, a per-ref path list, and a per-ref
// commit. Missing-key lookups return errors so tests can exercise
// the not-found branches by omitting an entry.
type stubGit struct {
	tags        []string
	pathsByRef  map[string][]string
	commitByRef map[string]string
}

func (s *stubGit) Tags(context.Context) ([]string, error) {
	return s.tags, nil
}

func (s *stubGit) PathsAtRef(_ context.Context, ref string) ([]string, error) {
	paths, ok := s.pathsByRef[ref]
	if !ok {
		return nil, fmt.Errorf("stubGit: no paths for ref %q", ref)
	}
	return paths, nil
}

func (s *stubGit) CommitForRef(_ context.Context, ref string) (string, error) {
	commit, ok := s.commitByRef[ref]
	if !ok {
		return "", fmt.Errorf("stubGit: no commit for ref %q", ref)
	}
	return commit, nil
}

// mustParseTime is a small RFC3339 helper for the inline signal
// fixtures above. Keeps test bodies readable.
func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return parsed
}

// TestCollector_Name pins the collector's name string. Cheap, but
// the name lands in profile.Signal.Source for every absence/signal
// the collector emits — keeping it stable across refactors matters
// for store-side queries.
func TestCollector_Name(t *testing.T) {
	t.Parallel()
	c := NewCollector(CollectorConfig{})
	assert.Equal(t, "artifact-vs-repo", c.Name())
}

// TestCollector_Collect_Gem_DescendsIntoInnerDataTarball is the
// canonical gem case: a `.gem` file is a plain (uncompressed) tar
// holding `data.tar.gz` + `metadata.gz` + `checksums.yaml.gz` as
// siblings. The artifact-vs-repo check only cares about
// data.tar.gz — the actual file payload corresponding to the source
// repo. The collector must:
//
//  1. Walk the outer `.gem` with FormatTar (no gunzip).
//  2. Capture data.tar.gz bytes via a CaptureIntent (the same
//     mechanism cargo uses for .cargo_vcs_info.json).
//  3. Re-walk the captured bytes with FormatTarGzip to produce the
//     inner manifest.
//  4. Diff that INNER manifest against the git tree.
//
// Without that descent, the diff would compare {data.tar.gz,
// metadata.gz, checksums.yaml.gz} against the repo's source files
// and report every real source file as missing-from-tarball noise
// — i.e. produce no useful signal at all.
//
// This is the load-bearing test for the gem extension: closes the
// signal-gap by making the collector aware of the nested-tarball
// shape.
func TestCollector_Collect_Gem_DescendsIntoInnerDataTarball(t *testing.T) {
	t.Parallel()

	const (
		entityID = "e-gem"
		version  = "1.2.3"
		tag      = "v1.2.3"
		commit   = "abcdef0123456789abcdef0123456789abcdef01"
	)

	// Build the inner data.tar.gz: real source plus one xz-shaped
	// extra (file in tarball, not in git).
	innerTarball := buildTarGz(t, []tarEntry{
		{path: "lib/mygem.rb", body: []byte("# real source")},
		{path: "lib/mygem/version.rb", body: []byte("VERSION = '1.2.3'")},
		// xz-equivalent: shipped only in the tarball.
		{path: "ext/inject.rb", body: []byte("# attacker payload")},
	})

	// Build the outer `.gem` (plain tar, no gzip) holding
	// data.tar.gz + metadata.gz + checksums.yaml.gz as siblings.
	outerGem := buildPlainTar(t, []tarEntry{
		{path: "data.tar.gz", body: innerTarball},
		{path: "metadata.gz", body: []byte("\x1f\x8b\x08\x00fake-metadata")},
		{path: "checksums.yaml.gz", body: []byte("\x1f\x8b\x08\x00fake-checksums")},
	})

	// gitPaths reflect what `git ls-tree -r --name-only v1.2.3`
	// returns: legitimate source, no inject.rb.
	gitPaths := []string{
		"lib/mygem.rb",
		"lib/mygem/version.rb",
	}

	inRun := &signal.CollectionResult{}
	inRun.RecordSignal(entityID, "artifact_url", "gem-registry",
		mustParseTime(t, "2026-04-01T00:00:00Z"), 24*time.Hour,
		map[string]any{
			"url":       "https://rubygems.org/downloads/mygem-1.2.3.gem",
			"version":   version,
			"git_head":  "", // rubygems.org doesn't expose a publisher-stamped SHA
			"integrity": "sha256-placeholder",
		})

	collector := NewCollector(CollectorConfig{
		InRun:     inRun,
		ClonePath: "/fake/clone",
		Fetcher:   &stubFetcher{body: outerGem},
		Git: &stubGit{
			tags:        []string{tag},
			pathsByRef:  map[string][]string{tag: gitPaths},
			commitByRef: map[string]string{tag: commit},
		},
	})

	entity := &profile.Entity{
		ID:           entityID,
		CanonicalURI: "pkg:gem/mygem",
		Type:         profile.EntityPackage,
		Ecosystem:    "gem",
	}

	result, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err)

	require.Equal(t, 1, result.SignalCount(),
		"gem divergence must produce one signal — fall-through to absence "+
			"means the nested-tarball descent didn't happen")
	assert.Equal(t, 0, result.AbsenceCount())

	var sig profile.Signal
	for _, s := range result.Signals() {
		if s.Type == "artifact_repo_divergence" {
			sig = s
			break
		}
	}
	require.NotEmpty(t, sig.Type)

	var cmp Comparison
	require.NoError(t, json.Unmarshal(sig.Value, &cmp))

	extras := make([]string, 0, len(cmp.ExtrasInTarballSample))
	for _, e := range cmp.ExtrasInTarballSample {
		extras = append(extras, e.Path)
	}

	assert.Equal(t, []string{"ext/inject.rb"}, extras,
		"only the malicious inner file should surface — if data.tar.gz, "+
			"metadata.gz, or checksums.yaml.gz appear here, the collector "+
			"failed to descend into the inner data.tar.gz and is comparing "+
			"the OUTER gem manifest against the source tree instead")
	assert.Equal(t, 1, cmp.ExtrasInTarballCount)

	assert.Equal(t, PairConfidenceTagMatch, cmp.PairConfidence,
		"rubygems.org exposes no publisher-stamped commit SHA; "+
			"the resolver falls through to tag-match")
}

// TestCollector_Collect_Go_ZipFormatAndExactGitHead is the canonical
// Go-module case. proxy.golang.org serves modules as zip files
// wrapped under "<module-path>@<version>/" — a multi-segment prefix
// when the module path itself contains slashes. The collector must:
//
//  1. Dispatch FormatZip (not FormatTarGzip) for go-ecosystem
//     entities — module zips are PKZIP.
//  2. Strip the multi-segment wrapping prefix correctly so post-
//     strip paths are comparable to git ls-tree output.
//  3. Use the registry-supplied git_head (Origin.Hash) as the
//     pair-resolver input, yielding PairConfidenceExactGitHead.
//
// Without (1) the walker would attempt to gunzip a zip and bail.
// Without (2) every path would carry the wrapper prefix and the
// diff would surface every legitimate source file as missing-
// from-repo noise. Without (3) we'd fall through to tag-match —
// usable but lower-confidence than the publisher-stamped SHA the
// proxy already gives us.
func TestCollector_Collect_Go_ZipFormatAndExactGitHead(t *testing.T) {
	t.Parallel()

	const (
		entityID = "e-go"
		version  = "v0.20.0"
		// 40-char hex SHA — the proxy-recorded Origin.Hash. stubGit
		// must advertise paths against this exact ref so the pairing
		// resolves at exact_gitHead.
		originHash = "ec11c4a93de22cde2abe2bf74d70791033c2464c"
	)

	// Build a Go-module zip with the spec-required wrapping prefix:
	// "<module-path>@<version>/" where module-path itself has slashes.
	// Real source plus one xz-shaped extra not in the git tree.
	moduleZip := buildZip(t, []zipEntry{
		{path: "golang.org/x/sync@v0.20.0/go.mod", body: []byte("module golang.org/x/sync\n")},
		{path: "golang.org/x/sync@v0.20.0/sync.go", body: []byte("// real source")},
		{path: "golang.org/x/sync@v0.20.0/errgroup/errgroup.go", body: []byte("// real source")},
		// xz-equivalent: extra in zip, never in git.
		{path: "golang.org/x/sync@v0.20.0/inject.go", body: []byte("// attacker payload")},
	})

	// What `git ls-tree -r --name-only <originHash>` returns.
	gitPaths := []string{
		"go.mod",
		"sync.go",
		"errgroup/errgroup.go",
	}

	inRun := &signal.CollectionResult{}
	inRun.RecordSignal(entityID, "artifact_url", "go-publish",
		mustParseTime(t, "2026-04-15T10:00:00Z"), 24*time.Hour,
		map[string]any{
			"url":       "https://proxy.golang.org/golang.org/x/sync/@v/v0.20.0.zip",
			"version":   version,
			"git_head":  originHash, // proxy-supplied — exact_gitHead
			"integrity": "",
		})

	collector := NewCollector(CollectorConfig{
		InRun:     inRun,
		ClonePath: "/fake/clone",
		Fetcher:   &stubFetcher{body: moduleZip},
		Git: &stubGit{
			tags:        []string{"v0.20.0"},
			pathsByRef:  map[string][]string{originHash: gitPaths},
			commitByRef: map[string]string{originHash: originHash},
		},
	})

	entity := &profile.Entity{
		ID:           entityID,
		CanonicalURI: "pkg:golang/golang.org/x/sync",
		Type:         profile.EntityPackage,
		Ecosystem:    "golang",
	}

	result, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err)

	require.Equal(t, 1, result.SignalCount(),
		"go module divergence must produce one signal — fall-through to "+
			"absence means the FormatZip dispatch or multi-segment strip failed")
	assert.Equal(t, 0, result.AbsenceCount())

	var sig profile.Signal
	for _, s := range result.Signals() {
		if s.Type == "artifact_repo_divergence" {
			sig = s
			break
		}
	}
	require.NotEmpty(t, sig.Type)

	var cmp Comparison
	require.NoError(t, json.Unmarshal(sig.Value, &cmp))

	extras := make([]string, 0, len(cmp.ExtrasInTarballSample))
	for _, e := range cmp.ExtrasInTarballSample {
		extras = append(extras, e.Path)
	}

	assert.Equal(t, []string{"inject.go"}, extras,
		"only the malicious file should surface — if go.mod, sync.go, "+
			"or errgroup/errgroup.go appear here, the multi-segment wrapping "+
			"prefix wasn't stripped correctly and every legitimate source "+
			"file looks like an extra")
	assert.Equal(t, 1, cmp.ExtrasInTarballCount)

	assert.Equal(t, PairConfidenceExactGitHead, cmp.PairConfidence,
		"proxy.golang.org records the publisher-stamped commit SHA in "+
			"Origin.Hash; same provenance strength as cargo's vcs_info, "+
			"so pairing resolves at exact_gitHead — not tag-match")
	assert.Equal(t, originHash, cmp.GitCommit)
}

// zipEntry is the test-only constructor type for buildZip.
type zipEntry struct {
	path string
	body []byte
}

// buildZip writes a PKZIP archive in memory and returns the bytes.
// Used by Go-module-shaped fixtures since the proxy serves modules
// as zip, not tar.gz.
func buildZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.Create(e.path)
		require.NoError(t, err)
		_, err = w.Write(e.body)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// buildPlainTar writes a plain (uncompressed) tar archive in memory
// and returns the bytes. Used by gem-shaped fixtures whose outer
// container is a tar without gzip wrapping.
func buildPlainTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name:     e.path,
			Size:     int64(len(e.body)),
			Mode:     0o644,
			Typeflag: tar.TypeReg,
		}))
		_, err := tw.Write(e.body)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

// TestCollector_Collect_PyPI_SuppressesPublisherInjectedFiles is the
// canonical pypi case: a sdist tarball walked through the full
// collector path (artifact_url handoff → fetch → walk → tag-match
// pair → diff) must surface ONLY real divergence and not the noise
// floor of publisher-injected files (PKG-INFO + <name>.egg-info/*).
//
// Fixture mirrors the cargo VCSInfoRescuesPair shape: real source +
// publisher-injected files + one xz-shaped extra not in the repo.
// Tag-match path resolves the pair (PyPI registry never supplies
// gitHead). Suppression must take the egg-info subtree to zero so
// the only surfaced extra is the malicious file.
//
// This is the load-bearing test for the pypi consumer side: closes
// the same signal-gap as the artifact_url emission on the producer
// side (covered in internal/signal/registry/pypi/collector_test.go).
func TestCollector_Collect_PyPI_SuppressesPublisherInjectedFiles(t *testing.T) {
	t.Parallel()

	const (
		entityID = "e-pypi"
		version  = "1.2.3"
		tag      = "v1.2.3" // tag-match resolver accepts "v"+version
		commit   = "abcdef0123456789abcdef0123456789abcdef01"
	)

	// sdist-shaped tarball: <name>-<version>/ wrapper, real source,
	// PKG-INFO at wrapper root, <name>.egg-info/ subtree, plus one
	// xz-shaped malicious file not in the git tree.
	tarball := buildTarGz(t, []tarEntry{
		{path: "hatch-1.2.3/hatch/__init__.py", body: []byte("# real source")},
		{path: "hatch-1.2.3/hatch/cli.py", body: []byte("# real cli")},
		{path: "hatch-1.2.3/PKG-INFO", body: []byte("Metadata-Version: 2.1\nName: hatch\n")},
		{path: "hatch-1.2.3/hatch.egg-info/PKG-INFO", body: []byte("Metadata-Version: 2.1\n")},
		{path: "hatch-1.2.3/hatch.egg-info/SOURCES.txt", body: []byte("hatch/__init__.py\n")},
		{path: "hatch-1.2.3/hatch.egg-info/top_level.txt", body: []byte("hatch\n")},
		// xz-equivalent: extra in tarball, never in git.
		{path: "hatch-1.2.3/build_inject.py", body: []byte("# attacker payload")},
	})

	gitPaths := []string{
		"hatch/__init__.py",
		"hatch/cli.py",
	}

	inRun := &signal.CollectionResult{}
	inRun.RecordSignal(entityID, "artifact_url", "pypi-registry",
		mustParseTime(t, "2026-04-01T00:00:00Z"), 24*time.Hour,
		map[string]any{
			"url":       "https://files.pythonhosted.org/packages/aa/bb/hatch-1.2.3.tar.gz",
			"version":   version,
			"git_head":  "", // PyPI never supplies this — tag-match path resolves
			"integrity": "sha256-placeholder",
		})

	collector := NewCollector(CollectorConfig{
		InRun:     inRun,
		ClonePath: "/fake/clone",
		Fetcher:   &stubFetcher{body: tarball},
		Git: &stubGit{
			tags:        []string{tag},
			pathsByRef:  map[string][]string{tag: gitPaths},
			commitByRef: map[string]string{tag: commit},
		},
	})

	entity := &profile.Entity{
		ID:           entityID,
		CanonicalURI: "pkg:pypi/hatch",
		Type:         profile.EntityPackage,
		Ecosystem:    "pypi",
	}

	result, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err)

	require.Equal(t, 1, result.SignalCount(),
		"pypi divergence must produce one signal — fall-through to absence "+
			"means tag-match didn't resolve or fetch/walk failed")
	assert.Equal(t, 0, result.AbsenceCount())

	var sig profile.Signal
	for _, s := range result.Signals() {
		if s.Type == "artifact_repo_divergence" {
			sig = s
			break
		}
	}
	require.NotEmpty(t, sig.Type)

	var cmp Comparison
	require.NoError(t, json.Unmarshal(sig.Value, &cmp))

	extras := make([]string, 0, len(cmp.ExtrasInTarballSample))
	for _, e := range cmp.ExtrasInTarballSample {
		extras = append(extras, e.Path)
	}

	assert.Equal(t, []string{"build_inject.py"}, extras,
		"only the malicious file should surface — PKG-INFO and the "+
			"<name>.egg-info/ subtree are publisher-injected sdist outputs "+
			"and must be suppressed (otherwise every healthy pypi package "+
			"surfaces 4+ false-positive extras)")
	assert.Equal(t, 1, cmp.ExtrasInTarballCount)

	assert.Equal(t, PairConfidenceTagMatch, cmp.PairConfidence,
		"PyPI exposes no publisher-stamped commit SHA in registry metadata; "+
			"the resolver falls through to tag-match")
	assert.Equal(t, commit, cmp.GitCommit)
}
