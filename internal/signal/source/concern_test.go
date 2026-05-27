package source

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/signal/source/astfeature"
)

// trapdoorShape returns Counts approximating a Trapdoor-style cargo
// build.rs payload: 5 rare-on-benign fields populated on a single
// version. Used as the canonical "born-malicious" shape across the
// concern-detector tests.
func trapdoorShape() astfeature.Counts {
	return astfeature.Counts{
		EnvCredentialReads:  2,
		SensitivePathReads:  2,
		SensitivePathWrites: 1,
		ExecCalls:           1,
		XORAssignments:      1,
		CloudMetadataCalls:  1,
		// Plus the excluded "naturally non-zero" fields — these must
		// not be the reason the detector fires. Each is independently
		// populated so a regression that re-included one as a
		// concerning feature would still need the 9-field subset to
		// trip.
		ImportTimeCallSites: 17,
		NetworkCallSites:    1,
		Base64DecodeCalls:   1,
	}
}

// (row helper is in anomaly_test.go — same package, same shape; we
// reuse it directly rather than re-declare.)

// TestDetectConcern_BornMaliciousFires is the load-bearing test for
// the in-situ detector: a single weaponized version with no clean
// prior must independently fire the concern signal. The Trapdoor
// shape — 5+ rare-on-benign fields populated on v0.1.0 — is the
// dominant born-malicious typo-squat pattern (the 2026-05-24
// cargo crates were exactly this shape).
func TestDetectConcern_BornMaliciousFires(t *testing.T) {
	t.Parallel()

	rows := []MatrixRow{
		row("0.1.0", trapdoorShape()),
	}
	got := DetectConcern(rows)

	assert.True(t, got.ConcernPresent,
		"a single Trapdoor-shape row must independently fire concern — "+
			"the differential anomaly detector can't catch born-malicious crates")
	assert.Equal(t, "0.1.0", got.FirstConcernVersion)
	assert.GreaterOrEqual(t, len(got.ConcerningFeatures), MinConcernFeatures,
		"firing requires >= MinConcernFeatures (%d); got %d", MinConcernFeatures, len(got.ConcerningFeatures))
	for _, f := range []string{
		"sensitive_path_reads", "exec_calls", "xor_assignments",
		"env_credential_reads", "sensitive_path_writes", "cloud_metadata_calls",
	} {
		assert.Contains(t, got.ConcerningFeatures, f,
			"feature %q must be named for the analyst", f)
	}
	// The three excluded fields must NOT appear even though their
	// counts in trapdoorShape() are non-zero.
	for _, excluded := range []string{
		"network_call_sites", "base64_decode_calls", "import_time_call_sites",
	} {
		assert.NotContainsf(t, got.ConcerningFeatures, excluded,
			"%q is in the deliberately-excluded set (naturally non-zero on benign code) "+
				"and must NEVER appear in concerning_features", excluded)
	}
}

// TestDetectConcern_BenignNoFire pins the no-false-positive baseline.
// An all-zero AST (the dogfood shape for healthy crates like anyhow,
// kong, ms across every selected row) must NOT fire the detector.
func TestDetectConcern_BenignNoFire(t *testing.T) {
	t.Parallel()

	rows := []MatrixRow{
		row("0.3.0", astfeature.Counts{}),
		row("0.2.0", astfeature.Counts{}),
		row("0.1.0", astfeature.Counts{}),
	}
	got := DetectConcern(rows)
	assert.False(t, got.ConcernPresent,
		"benign code (every catalog field 0) must NOT fire concern — "+
			"the no-false-positive baseline is the load-bearing AST.md §3 property")
	assert.Empty(t, got.FirstConcernVersion)
	assert.Empty(t, got.ConcerningFeatures)
}

// TestDetectConcern_OneFieldOnly_NoFire confirms the threshold isn't
// 1: a single rare-on-benign field firing alone (e.g. a legitimate
// CLI tool that reads ~/.ssh/config for ssh agent management, or a
// build script that calls exec for env probing in cases where the
// FirstArg DID resolve to a literal — both rare but possible) must
// not trip the boolean. The threshold-of-2 is the joint-evidence
// discipline mirroring MinSpikedFeatures.
func TestDetectConcern_OneFieldOnly_NoFire(t *testing.T) {
	t.Parallel()

	rows := []MatrixRow{
		row("1.0.0", astfeature.Counts{SensitivePathReads: 1}),
	}
	got := DetectConcern(rows)
	assert.False(t, got.ConcernPresent,
		"a single rare-on-benign field firing alone is below the joint-threshold; must NOT fire")
}

// TestDetectConcern_FirstChronologicalWins covers the case where
// multiple rows are concerning (born-malicious patterns where the
// payload was present from v0.1.0 onwards). The detector reports the
// FIRST (oldest) such row — the entry-point — not the latest.
// Mirrors DetectAnomaly's first-chronological-pair convention.
func TestDetectConcern_FirstChronologicalWins(t *testing.T) {
	t.Parallel()

	// Three weaponized versions in a row, no clean prior.
	// rows[0] (newest) … rows[2] (oldest, the entry point).
	rows := []MatrixRow{
		row("0.3.0", trapdoorShape()),
		row("0.2.0", trapdoorShape()),
		row("0.1.0", trapdoorShape()),
	}
	got := DetectConcern(rows)
	assert.True(t, got.ConcernPresent)
	assert.Equal(t, "0.1.0", got.FirstConcernVersion,
		"first chronological concerning row is the oldest one, not the newest — "+
			"analyst asks 'when did this start', the answer is the entry-point version")
}

// TestDetectConcern_ExcludedFieldsDoNotCount is the load-bearing
// guard for the field-set choice: a row whose non-zero fields are
// ALL drawn from the deliberately-excluded set (Network /
// Base64Decode / ImportTime) must NOT fire concern, regardless of
// magnitude. These three fields are designed to be naturally
// non-zero on legitimate code, so an absolute threshold on them
// would false-positive across most of the ecosystem.
//
// This is the AST.md §4 "their value is the spike, never the
// absolute" property applied to the in-situ detector.
func TestDetectConcern_ExcludedFieldsDoNotCount(t *testing.T) {
	t.Parallel()

	rows := []MatrixRow{
		// All three excluded fields populated in high numbers; the
		// remaining 9 (the rare-on-benign subset) zero. Models the
		// sigstore live-dogfood shape: Base64DecodeCalls=18,
		// NetworkCallSites elsewhere ~0, ImportTime non-zero from
		// module-scope getLogger() etc.
		row("1.0.0", astfeature.Counts{
			NetworkCallSites:    50,
			Base64DecodeCalls:   100,
			ImportTimeCallSites: 200,
		}),
	}
	got := DetectConcern(rows)
	assert.False(t, got.ConcernPresent,
		"only the 3 excluded fields are non-zero; concern must NOT fire — "+
			"AST.md §4 'spike not absolute' discipline applied to in-situ detection")
}

// TestDetectConcern_NilASTRowsSkipped covers the missing-from-clone /
// missing-origin / fetch-failed row classes: their AST is nil
// because the analyzer couldn't run. The detector must skip them
// silently and not panic on the dereference, and a born-malicious
// pattern in an ANALYZABLE row should still fire even when an older
// row has nil AST blocking the way.
func TestDetectConcern_NilASTRowsSkipped(t *testing.T) {
	t.Parallel()

	rows := []MatrixRow{
		row("0.3.0", trapdoorShape()),
		// Missing-from-clone middle row — AST nil.
		{Version: "0.2.0", TagSHALocalStatus: TagSHALocalMissingFromClone, AST: nil},
		{Version: "0.1.0", TagSHALocalStatus: TagSHALocalMissingOrigin, AST: nil},
	}
	got := DetectConcern(rows)
	assert.True(t, got.ConcernPresent,
		"the analyzable row's concern must still surface even when older rows are nil-AST")
	assert.Equal(t, "0.3.0", got.FirstConcernVersion,
		"with no analyzable older row, the only concerning row IS the first chronological — "+
			"nil rows are silently skipped, not treated as 'oldest concern'")
}

// TestDetectConcern_EmptyRows is the trivial leniency case: no rows
// at all (empty matrix from a brand-new package or an unbuildable
// repo) yields the zero ConcernValue.
func TestDetectConcern_EmptyRows(t *testing.T) {
	t.Parallel()
	got := DetectConcern(nil)
	assert.False(t, got.ConcernPresent)
	assert.Equal(t, ConcernValue{}, got)
}

// TestDetectConcern_FieldOrderingStable confirms the
// concerning_features slice comes out in canonical Counts-field
// declaration order — not insertion order, not alphabetical, not
// random. Stable output is load-bearing for the deltas pipeline
// (a string-slice change order-only would surface as a false
// transition).
func TestDetectConcern_FieldOrderingStable(t *testing.T) {
	t.Parallel()

	// Populate the rare-on-benign fields in a deliberately scrambled
	// pattern — the OUTPUT must still be in Counts-declaration order.
	rows := []MatrixRow{
		row("1.0.0", astfeature.Counts{
			CloudMetadataCalls:   1, // last in Counts declaration
			InitCount:            1, // first
			ExecCalls:            1, // third (after SensitivePathReads)
			EnvCredentialReads:   1,
			SensitivePathReads:   1,
			SensitivePathWrites:  1,
			XORAssignments:       1,
			DynamicEvalCalls:     1,
			InstallHookOverrides: 1,
		}),
	}
	got := DetectConcern(rows)
	require := assert.New(t)
	require.True(got.ConcernPresent)
	// Canonical order matches Counts struct field declaration
	// (with the three excluded entries omitted).
	want := []string{
		"init_count",
		"sensitive_path_reads",
		"exec_calls",
		"xor_assignments",
		"dynamic_eval_calls",
		"install_hook_overrides",
		"env_credential_reads",
		"sensitive_path_writes",
		"cloud_metadata_calls",
	}
	require.Equal(want, got.ConcerningFeatures,
		"output must be in canonical Counts field-declaration order, "+
			"NOT alphabetical or input-order")
}

// TestConcerningFeatures_FieldExhaustiveness reflects over every
// astfeature.Counts field and asserts each one is either:
//   - in the documented exclusion set (and so must NOT fire concern), OR
//   - wired into concerningFeatures() with its json-tag name (and so
//     MUST fire concern when populated in isolation).
//
// Adding a new Counts field without either listing it in
// rareOnBenignExclusions below OR wiring it into concerningFeatures
// will fail this test. That's the load-bearing invariant — the prior
// suite hand-listed the 9 included fields, so a 13th-field oversight
// in concern.go would have slipped through silently.
func TestConcerningFeatures_FieldExhaustiveness(t *testing.T) {
	t.Parallel()

	// The documented exclusions (see concerningFeatures's doc
	// comment in concern.go). Keys are the json-tag names; values
	// are short rationales mirrored from the production-file comment
	// so a drift in either side fails review.
	rareOnBenignExclusions := map[string]string{
		"network_call_sites":     "any HTTP-client / web framework legitimately populates",
		"base64_decode_calls":    "crypto crates routinely decode base64 (purpose-blind)",
		"import_time_call_sites": "naturally non-zero on every real package",
	}

	countsType := reflect.TypeFor[astfeature.Counts]()
	for i := range countsType.NumField() {
		field := countsType.Field(i)
		tag := field.Tag.Get("json")
		if comma := strings.IndexByte(tag, ','); comma >= 0 {
			tag = tag[:comma]
		}
		require.NotEmptyf(t, tag,
			"astfeature.Counts.%s must carry a json tag — concerningFeatures "+
				"identifies fields by tag name", field.Name)

		t.Run(field.Name, func(t *testing.T) {
			t.Parallel()
			var c astfeature.Counts
			reflect.ValueOf(&c).Elem().Field(i).SetInt(1)

			fired := concerningFeatures(c)

			if rationale, excluded := rareOnBenignExclusions[tag]; excluded {
				assert.Emptyf(t, fired,
					"%s is in the documented exclusion set (%s) — populating "+
						"it must NOT fire concern. If you intentionally moved this "+
						"field INTO the rare-on-benign subset, also remove it from "+
						"rareOnBenignExclusions in the test.",
					field.Name, rationale)
				return
			}
			assert.Equalf(t, []string{tag}, fired,
				"%s (json:%q) must fire concern when populated in isolation. "+
					"Either wire it into concerningFeatures() or add it to "+
					"rareOnBenignExclusions with a rationale comment.",
				field.Name, tag)
		})
	}
}
