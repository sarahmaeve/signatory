package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// Integration tests for the Plan-A posture canonicalization
// contract: a posture for `pkg:npm/X@V` lives at the unversioned
// `pkg:npm/X` entity with the posture row's `version` column = "V".
// Every user-facing command (get, set, unset, accept) normalizes
// both write and read URIs through profile.SplitURIVersion so the
// two URI forms (pkg:npm/X@V and pkg:npm/X with --version V)
// resolve to the same storage row.
//
// See design/m6-synthesis-contract.md and the 2026-04-21 dogfood
// session where a version-suffix `posture get` missed a posture the
// `posture accept` had just written. The scenarios below each
// encode one user-visible behavior the canonicalization guarantees.

// writeUnversionedPostureWithVersionCol is the fixture setup:
// write a posture to the unversioned entity with version column
// populated — mirrors what `posture accept` produces in the common
// dogfood flow. The test that follows verifies the corresponding
// version-suffix read path finds it.
func writeUnversionedPostureWithVersionCol(
	t *testing.T, g *Globals,
	unversionedURI, version, tier, rationale string,
) {
	t.Helper()
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	// Resolve + ensure entity exists for the unversioned URI.
	resolved, err := profile.ResolveTarget(unversionedURI)
	require.NoError(t, err)
	entity, err := s.FindEntityByURI(ctx, resolved.CanonicalURI)
	if err != nil {
		// Create.
		entity = &profile.Entity{
			ID:           profile.NewEntityID(),
			CanonicalURI: resolved.CanonicalURI,
			Type:         profile.EntityPackage,
			ShortName:    resolved.ShortName,
			CreatedAt:    time.Now().UTC(),
			UpdatedAt:    time.Now().UTC(),
		}
		require.NoError(t, s.PutEntity(ctx, entity))
	}

	require.NoError(t, s.SetPosture(ctx, &profile.Posture{
		EntityID:  entity.ID,
		Tier:      profile.PostureTier(tier),
		Version:   version,
		Rationale: rationale,
		SetBy:     "team:test",
		SetAt:     time.Now().UTC(),
	}))
}

// TestPostureGet_VersionedURI_FindsCanonicalPosture is the direct
// regression test for the dogfood bug: a posture at (unversioned,
// version="V") must be findable via `posture get <unversioned>@V`.
func TestPostureGet_VersionedURI_FindsCanonicalPosture(t *testing.T) {
	g := newTestGlobals(t)
	writeUnversionedPostureWithVersionCol(t, g,
		"pkg:npm/planA-get-example", "1.2.3",
		"trusted-for-now", "rationale for plan-A get test")

	cmd := &PostureGetCmd{Target: "pkg:npm/planA-get-example@1.2.3"}
	stdout := captureStdout(t, func() {
		require.NoError(t, cmd.Run(g))
	})

	assert.Contains(t, stdout, "trusted-for-now",
		"versioned-URI posture get must find the posture stored at (unversioned entity, version='1.2.3')")
	assert.Contains(t, stdout, "1.2.3",
		"the version column must surface in the output")
	assert.NotContains(t, stdout, "No posture recorded",
		"the canonicalized query must not report absence")
}

// TestPostureSet_VersionedURI_WritesToUnversionedEntity: when the
// user types `posture set pkg:npm/X@V --tier ...`, Plan A routes
// the write to the UNVERSIONED entity with the posture row's
// version column populated. Pre-Plan-A, this write landed at the
// versioned entity — which is why the dogfood bug happened: a
// posture accept wrote to unversioned (via synthesis target), a
// hypothetical posture set @V would write to versioned, and the
// two rows were invisible to each other.
func TestPostureSet_VersionedURI_WritesToUnversionedEntity(t *testing.T) {
	g := newTestGlobals(t)

	cmd := &PostureSetCmd{
		Target:    "pkg:npm/planA-set-example@2.0.0",
		Tier:      "trusted-for-now",
		Rationale: "rationale for plan-A set test",
	}
	require.NoError(t, cmd.Run(g))

	// Verify the posture row landed at the UNVERSIONED entity with
	// version="2.0.0". The versioned entity may or may not exist;
	// what matters is that `posture get unversioned --version 2.0.0`
	// finds it.
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	entity, err := s.FindEntityByURI(ctx, "pkg:npm/planA-set-example")
	require.NoError(t, err,
		"Plan A: `posture set X@V` must materialize an UNVERSIONED entity for X, even though the user typed @V")

	posture, err := s.GetPosture(ctx, entity.ID, "2.0.0")
	require.NoError(t, err,
		"posture must be retrievable at (unversioned entity, version='2.0.0')")
	assert.Equal(t, profile.PostureTier("trusted-for-now"), posture.Tier)
	assert.Equal(t, "2.0.0", posture.Version)
}

// TestSummary_UnknownTarget_SoftAbsence asserts that `summary` on a
// target with no entity is a soft absence (no error, exit 0, human-
// readable "no record" message), matching `posture get`'s behavior
// for the same condition. Pre-fix, summary returned a usage error
// (exit 64) — asymmetric with the sibling read verbs and confusing
// for scripters pipelining queries. 2026-04-22 read-surface pass.
func TestSummary_UnknownTarget_SoftAbsence(t *testing.T) {
	g := newTestGlobals(t)

	cmd := &SummaryCmd{Target: "pkg:npm/never-ingested-target"}
	stdout := captureStdout(t, func() {
		require.NoError(t, cmd.Run(g),
			"unknown target must NOT produce an error — it's a soft absence, "+
				"matching posture get's contract")
	})

	assert.Contains(t, stdout, "never-ingested-target",
		"output should name the queried target")
	assert.Contains(t, stdout, "No signatory record",
		"output should say there's no record rather than error out")
}

// TestSummary_ZeroConclusions_OmitsEmptyBracket: the severity
// breakdown `[-]` placeholder is visual noise on synthesis rows
// (which always have 0 conclusions by Plan-A design). Drop the
// bracket entirely when the count is 0. 2026-04-22 read-surface
// pass.
func TestSummary_ZeroConclusions_OmitsEmptyBracket(t *testing.T) {
	g := newTestGlobals(t)

	// Ingest a synthesis output (0 conclusions) to get the zero-
	// conclusion rendering path.
	outputID := ingestSynthesisForAccept(t, g, synthesisForAccept())
	_ = outputID

	cmd := &SummaryCmd{Target: "pkg:npm/accept-example"}
	stdout := captureStdout(t, func() {
		require.NoError(t, cmd.Run(g))
	})

	assert.Contains(t, stdout, "0 conclusion(s)",
		"the conclusion count still renders")
	assert.NotContains(t, stdout, "0 conclusion(s) [-]",
		"the empty-severity-bracket placeholder must be suppressed when there are 0 conclusions")
	assert.NotContains(t, stdout, "[-]",
		"no '[-]' placeholder should appear anywhere in the render")
}

// TestSummary_VersionedURI_SurfacesCanonicalPosture asserts that
// `signatory summary pkg:npm/X@V` surfaces the posture stored at
// (unversioned entity, version="V"). This was the SECOND part of
// the dogfood bug: not only did `posture get X@V` miss, but
// `summary X@V` also reported "(none recorded)" for a posture that
// was right there at the sibling URI.
func TestSummary_VersionedURI_SurfacesCanonicalPosture(t *testing.T) {
	g := newTestGlobals(t)
	writeUnversionedPostureWithVersionCol(t, g,
		"pkg:npm/planA-summary-example", "4.5.6",
		"vetted-frozen", "plan-A summary fixture")

	cmd := &SummaryCmd{Target: "pkg:npm/planA-summary-example@4.5.6"}
	stdout := captureStdout(t, func() {
		require.NoError(t, cmd.Run(g))
	})

	assert.Contains(t, stdout, "vetted-frozen",
		"versioned-URI summary must surface the canonical posture")
	assert.NotContains(t, stdout, "Posture:   (none recorded)",
		"Plan A: summary at @V must not report absence when the unversioned entity has a posture with matching version column")
}

// TestPostureSet_EquivalentURIForms: the two user-facing ways of
// expressing "posture set X at version V" — `X@V` and
// `X --version V` — produce functionally equivalent storage rows.
// Without this, a user who types one form and then queries the
// other is back to the dogfood bug.
func TestPostureSet_EquivalentURIForms(t *testing.T) {
	g := newTestGlobals(t)

	cmdA := &PostureSetCmd{
		Target:    "pkg:npm/planA-equiv@3.0.0",
		Tier:      "trusted-for-now",
		Rationale: "via URI suffix",
	}
	require.NoError(t, cmdA.Run(g))

	cmdB := &PostureSetCmd{
		Target:    "pkg:npm/planA-equiv",
		Version:   "3.0.0",
		Tier:      "rejected",
		Rationale: "via --version flag",
	}
	require.NoError(t, cmdB.Run(g))

	// Both wrote to (unversioned entity, version="3.0.0"). The
	// append-only posture table keeps both rows; the latest wins on
	// reads.
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	entity, err := s.FindEntityByURI(ctx, "pkg:npm/planA-equiv")
	require.NoError(t, err)
	latest, err := s.GetPosture(ctx, entity.ID, "3.0.0")
	require.NoError(t, err)
	assert.Equal(t, profile.PostureTier("rejected"), latest.Tier,
		"latest posture should reflect cmdB (via --version flag)")
	assert.Equal(t, "via --version flag", latest.Rationale)
}

// TestPostureSet_MalformedVersion_Rejected covers the manual
// "paste the whole URI into --version" mistake that produced the
// google/uuid malformed posture row in dogfood (see the
// accept_posture audit entry from 2026-04-22T11:35:59Z). The
// shape-check runs BEFORE ensureEntity / SetPosture, so the
// rejection is a pure usage error with no side effect on the
// store.
//
// Each row exercises a different reject-category in the
// exchange.ValidateVersionScopeShape helper; if any category
// silently degrades to accept, a real malformed posture slips
// through the manual-input door.
func TestPostureSet_MalformedVersion_Rejected(t *testing.T) {
	cases := []struct {
		name      string
		version   string
		errSubstr string
	}{
		{
			name:      "canonical pkg URI",
			version:   "pkg:golang/github.com/google/uuid@v1.6.0",
			errSubstr: "canonical URI",
		},
		{
			name:      "canonical repo URI",
			version:   "repo:github/google/uuid",
			errSubstr: "canonical URI",
		},
		{
			name:      "https URL",
			version:   "https://example.com/v1.2.3",
			errSubstr: "URL",
		},
		{
			name:      "multiline paste",
			version:   "v1.6.0\n(extracted from the synthesis notes)",
			errSubstr: "single-line",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newTestGlobals(t)
			cmd := &PostureSetCmd{
				Target:    "pkg:npm/manual-bad-version-test",
				Version:   tc.version,
				Tier:      "trusted-for-now",
				Rationale: "should never land — version should be rejected",
			}
			err := cmd.Run(g)
			require.Error(t, err,
				"malformed --version must be rejected before any store write")
			assert.Contains(t, err.Error(), "--version",
				"error should name the offending flag so the caller can fix their command")
			assert.Contains(t, err.Error(), tc.errSubstr,
				"error should describe the specific shape problem")

			// Prove no side effect: the entity we named should not have
			// been created (ensureEntity runs after the check).
			s, serr := g.OpenStore(t.Context())
			require.NoError(t, serr)
			defer s.Close() //nolint:errcheck // test cleanup
			_, ferr := s.FindEntityByURI(t.Context(), "pkg:npm/manual-bad-version-test")
			assert.Error(t, ferr,
				"rejected posture set must not have created any entity")
		})
	}
}

// TestPostureSet_CleanVersion_Accepted is the positive-companion:
// confirms the reject tests above aren't simply "posture set is
// broken" — legitimate version strings still pass through and
// write postures as expected.
func TestPostureSet_CleanVersion_Accepted(t *testing.T) {
	g := newTestGlobals(t)
	cmd := &PostureSetCmd{
		Target:    "pkg:npm/manual-good-version-test",
		Version:   "v1.6.0",
		Tier:      "trusted-for-now",
		Rationale: "clean version string must work",
	}
	require.NoError(t, cmd.Run(g),
		"a legitimate version must not be rejected by the shape check")

	s, err := g.OpenStore(t.Context())
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	entity, err := s.FindEntityByURI(t.Context(), "pkg:npm/manual-good-version-test")
	require.NoError(t, err)
	posture, err := s.GetPosture(t.Context(), entity.ID, "v1.6.0")
	require.NoError(t, err)
	assert.Equal(t, "v1.6.0", posture.Version)
}
