package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
)

// Canonicalization tests covering the v10 Plan-A contract: ingest
// strips `@V` from the caller's target before creating/looking up
// the entity, and preserves the original versioned form on the
// analyst_outputs.target column. Regression targets:
//
//   - 2026-04-22 testify dogfood: `pkg:golang/github.com/stretchr/
//     testify@v1.11.1` created a parallel entity row, fragmenting
//     analysis history across versions and breaking posture accept.
//   - Posture-set vs ingest URI-model asymmetry: `posture set X@V`
//     strips @V via normalizeTargetForPosture, but ingest did not —
//     producing two entity rows for "the same package."
//
// The tests are written to the post-v10 behavior and assert both
// the new invariants (entity at unversioned URI) AND the preserved
// caller fidelity (analyst_outputs.target carries @V verbatim).
// Structured so re-reading doesn't require cross-referencing the
// ensureEntityForTarget implementation — the tests are the spec.

// minimalAnalystOutput builds a minimally valid AnalystOutput with
// the supplied target. One conclusion, no supplement. Terse on
// purpose so the canonicalization-shape assertions aren't buried.
func minimalAnalystOutput(target string) *exchange.AnalystOutput {
	lineStart := 10
	return &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "external-sec-v1",
			Model:     "test-model",
			InvokedAt: "2026-04-22T12:00:00Z",
		},
		Target: target,
		Conclusions: []exchange.Conclusion{
			{
				ID:        "F001",
				Verdict:   "test",
				Rationale: "test rationale",
				Severity:  exchange.Severity{Default: exchange.SeverityLow},
				Category:  "test",
				Citations: []exchange.Citation{
					{Path: "src/x.go", LineStart: &lineStart},
				},
			},
		},
	}
}

// TestIngest_PkgVersionedTarget_StripsForEntity covers the primary
// Plan-A promise for pkg: URIs. Inputs carrying `@V` must route to
// the unversioned entity row; the versioned form stays on the
// analyst_outputs row.
func TestIngest_PkgVersionedTarget_StripsForEntity(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	out := minimalAnalystOutput("pkg:npm/example-pkg@1.2.3")
	result, err := s.IngestAnalystOutput(ctx, out, "test-source")
	require.NoError(t, err)

	// Entity lookup must succeed at the UNVERSIONED URI.
	entity, err := s.FindEntityByURI(ctx, "pkg:npm/example-pkg")
	require.NoError(t, err, "entity must be keyed at the unversioned URI (Plan A)")
	assert.Equal(t, result.EntityID, entity.ID,
		"IngestResult.EntityID must match the unversioned entity's id")

	// No stray versioned entity created.
	_, err = s.FindEntityByURI(ctx, "pkg:npm/example-pkg@1.2.3")
	assert.ErrorIs(t, err, ErrNotFound,
		"no versioned entity row may be created — that's the dogfood bug this closes")

	// Short name derives from the unversioned form — "example-pkg",
	// not "example-pkg@1.2.3" (the latter is the ugly-render
	// symptom we saw on testify@v1.11.1).
	assert.Equal(t, "example-pkg", entity.ShortName,
		"entity.short_name must derive from the unversioned URI so CLI rendering stays clean")

	// The row preserves the caller-supplied target verbatim.
	loaded, err := s.GetAnalystOutput(ctx, result.OutputID)
	require.NoError(t, err)
	assert.Equal(t, "pkg:npm/example-pkg@1.2.3", loaded.Target,
		"GetAnalystOutput must reconstruct the original target including @V — the row carries it on the new target column")
}

// TestIngest_RepoVersionedTarget_StripsForEntity is the repo:-scheme
// half. repo: URIs have their own parser in SplitURIVersion; locking
// both schemes in prevents drift.
func TestIngest_RepoVersionedTarget_StripsForEntity(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	out := minimalAnalystOutput("repo:github/owner/example-repo@v2.5.0")
	result, err := s.IngestAnalystOutput(ctx, out, "test-source")
	require.NoError(t, err)

	entity, err := s.FindEntityByURI(ctx, "repo:github/owner/example-repo")
	require.NoError(t, err)
	assert.Equal(t, result.EntityID, entity.ID)

	_, err = s.FindEntityByURI(ctx, "repo:github/owner/example-repo@v2.5.0")
	assert.ErrorIs(t, err, ErrNotFound)

	assert.Equal(t, "example-repo", entity.ShortName,
		"short_name on a repo: URI must not carry the version tag")

	loaded, err := s.GetAnalystOutput(ctx, result.OutputID)
	require.NoError(t, err)
	assert.Equal(t, "repo:github/owner/example-repo@v2.5.0", loaded.Target)
}

// TestIngest_EntityType_MatchesURIScheme is the producer-side regression
// for the long-standing analyst-output ingest bug: ensureEntityForTarget
// hardcoded Type=EntityProject for every newly-created entity, regardless
// of URI scheme. Every pkg: URI ingested through MCP signatory_ingest_
// analysis or manual `signatory ingest` produced a row mistyped as
// EntityProject, and downstream Type-gates (npm/pypi resolver triggers
// in cmd/signatory/analyze.go) silently failed to fire on those rows.
//
// Fix routes through profile.EntityTypeForURI so the stored type is
// always derived from the canonical URI scheme. This test pins the
// invariant for every scheme analyst-output ingest can reach.
func TestIngest_EntityType_MatchesURIScheme(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		target string
		uri    string // expected unversioned URI on the entity row
		want   profile.EntityType
	}{
		{
			name:   "pkg npm",
			target: "pkg:npm/example-pkg",
			uri:    "pkg:npm/example-pkg",
			want:   profile.EntityPackage,
		},
		{
			name:   "pkg npm versioned",
			target: "pkg:npm/example-pkg@1.2.3",
			uri:    "pkg:npm/example-pkg",
			want:   profile.EntityPackage,
		},
		{
			name:   "pkg cargo",
			target: "pkg:cargo/example-crate",
			uri:    "pkg:cargo/example-crate",
			want:   profile.EntityPackage,
		},
		{
			name:   "pkg pypi",
			target: "pkg:pypi/example-py",
			uri:    "pkg:pypi/example-py",
			want:   profile.EntityPackage,
		},
		{
			name:   "pkg golang",
			target: "pkg:golang/github.com/example/mod",
			uri:    "pkg:golang/github.com/example/mod",
			want:   profile.EntityPackage,
		},
		{
			name:   "repo github",
			target: "repo:github/example-org/example-repo",
			uri:    "repo:github/example-org/example-repo",
			want:   profile.EntityProject,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestDB(t)
			ctx := context.Background()

			out := minimalAnalystOutput(tc.target)
			_, err := s.IngestAnalystOutput(ctx, out, "type-regression")
			require.NoError(t, err)

			entity, err := s.FindEntityByURI(ctx, tc.uri)
			require.NoError(t, err, "entity must exist at expected URI")
			assert.Equal(t, tc.want, entity.Type,
				"entity.type must match the URI scheme — pkg: → package, repo: → project; "+
					"hardcoded EntityProject was the producer bug this test pins")
		})
	}
}

// TestIngest_MultipleVersions_SameEntity is the defragmentation
// invariant: two analyses of the same package at different versions
// land on ONE entity row with two output rows. Pre-v10 this produced
// two entity rows (pkg:X@V1 and pkg:X@V2), invisible to each other's
// show-analyses queries — which was the dogfood fragmentation pain.
func TestIngest_MultipleVersions_SameEntity(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	out1 := minimalAnalystOutput("pkg:npm/multi-version@1.0.0")
	r1, err := s.IngestAnalystOutput(ctx, out1, "first")
	require.NoError(t, err)

	out2 := minimalAnalystOutput("pkg:npm/multi-version@2.0.0")
	// Different content-hash via a distinct invoked_at so the
	// idempotency short-circuit doesn't mask the test.
	out2.Attribution.InvokedAt = "2026-04-22T13:00:00Z"
	r2, err := s.IngestAnalystOutput(ctx, out2, "second")
	require.NoError(t, err)

	assert.Equal(t, r1.EntityID, r2.EntityID,
		"both version analyses must share one entity row — that's the whole point of Plan A")
	assert.NotEqual(t, r1.OutputID, r2.OutputID,
		"distinct invocations must produce distinct output ids")

	entity, err := s.FindEntityByURI(ctx, "pkg:npm/multi-version")
	require.NoError(t, err)
	assert.Equal(t, r1.EntityID, entity.ID)

	// The two rows carry different target_version columns; callers
	// who want version-scoped queries (show-analyses --version X)
	// can filter without re-parsing target.
	loaded1, err := s.GetAnalystOutput(ctx, r1.OutputID)
	require.NoError(t, err)
	loaded2, err := s.GetAnalystOutput(ctx, r2.OutputID)
	require.NoError(t, err)
	assert.Equal(t, "pkg:npm/multi-version@1.0.0", loaded1.Target)
	assert.Equal(t, "pkg:npm/multi-version@2.0.0", loaded2.Target)
}

// TestIngest_UnversionedTarget_Unchanged is the no-op case: a
// target without @V must behave exactly as it did pre-v10 — entity
// at the given URI, analyst_outputs.target matching. Guards against
// my canonicalization accidentally injecting synthetic suffixes on
// unversioned inputs.
func TestIngest_UnversionedTarget_Unchanged(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	out := minimalAnalystOutput("pkg:npm/unversioned-only")
	result, err := s.IngestAnalystOutput(ctx, out, "test-source")
	require.NoError(t, err)

	entity, err := s.FindEntityByURI(ctx, "pkg:npm/unversioned-only")
	require.NoError(t, err)
	assert.Equal(t, result.EntityID, entity.ID)

	loaded, err := s.GetAnalystOutput(ctx, result.OutputID)
	require.NoError(t, err)
	assert.Equal(t, "pkg:npm/unversioned-only", loaded.Target)
}

// TestIngest_ScopedNpmWithVersion_CorrectlySplit exercises a subtle
// parser case: a scoped npm package like `pkg:npm/@types/node@20.0.0`
// has TWO `@` characters — one is the scope marker (non-version),
// one is the version separator. SplitURIVersion anchors to the LAST
// path segment, so the scope `@` in the FIRST segment must not be
// mistaken for a version split.
func TestIngest_ScopedNpmWithVersion_CorrectlySplit(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	out := minimalAnalystOutput("pkg:npm/@types/node@20.0.0")
	result, err := s.IngestAnalystOutput(ctx, out, "test-source")
	require.NoError(t, err)

	// Entity is at the unversioned SCOPED URI — the `@types` scope
	// stays in the name, only the `@20.0.0` is stripped.
	entity, err := s.FindEntityByURI(ctx, "pkg:npm/@types/node")
	require.NoError(t, err, "scope marker must not be mistaken for a version separator")
	assert.Equal(t, result.EntityID, entity.ID)

	// No entity at the wrong-split URI.
	_, err = s.FindEntityByURI(ctx, "pkg:npm/@types")
	assert.ErrorIs(t, err, ErrNotFound,
		"a naive `last @` split would have created a bogus entity at `pkg:npm/@types`")

	loaded, err := s.GetAnalystOutput(ctx, result.OutputID)
	require.NoError(t, err)
	assert.Equal(t, "pkg:npm/@types/node@20.0.0", loaded.Target,
		"target preserves both @-containing segments")
}

// TestIngest_SplitURIVersion_Consistency sanity-checks that the
// canonicalization path uses profile.SplitURIVersion — the same
// primitive that posture set/get/unset/accept use. If either side
// drifts to a local implementation, pairs of operations (ingest
// then posture set) will stop sharing an entity row, reintroducing
// the dogfood fragmentation.
//
// We exercise this by covering the grammar tables that SplitURIVersion
// already tests — a thin wrapper assertion, not re-deriving the logic.
func TestIngest_SplitURIVersion_Consistency(t *testing.T) {
	cases := []struct {
		name             string
		target           string
		wantEntityURI    string
		wantTargetColumn string
		wantVersion      string
	}{
		{
			name:             "pkg versioned",
			target:           "pkg:npm/lodash@4.17.21",
			wantEntityURI:    "pkg:npm/lodash",
			wantTargetColumn: "pkg:npm/lodash@4.17.21",
			wantVersion:      "4.17.21",
		},
		{
			name:             "pkg unversioned",
			target:           "pkg:npm/lodash",
			wantEntityURI:    "pkg:npm/lodash",
			wantTargetColumn: "pkg:npm/lodash",
			wantVersion:      "",
		},
		{
			name:             "repo versioned",
			target:           "repo:github/foo/bar@v1.0.0",
			wantEntityURI:    "repo:github/foo/bar",
			wantTargetColumn: "repo:github/foo/bar@v1.0.0",
			wantVersion:      "v1.0.0",
		},
		{
			name:             "pkg scoped versioned",
			target:           "pkg:npm/@scope/pkg@5.6.7",
			wantEntityURI:    "pkg:npm/@scope/pkg",
			wantTargetColumn: "pkg:npm/@scope/pkg@5.6.7",
			wantVersion:      "5.6.7",
		},
		{
			name:             "pkg golang module",
			target:           "pkg:golang/github.com/stretchr/testify@v1.11.1",
			wantEntityURI:    "pkg:golang/github.com/stretchr/testify",
			wantTargetColumn: "pkg:golang/github.com/stretchr/testify@v1.11.1",
			wantVersion:      "v1.11.1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Cross-check: resolveIngestTarget's output must match
			// profile.SplitURIVersion's decomposition, so ingest and
			// posture paths see the same splits.
			resolved, err := resolveIngestTarget(tc.target)
			require.NoError(t, err)
			assert.Equal(t, tc.wantEntityURI, resolved.EntityURI,
				"EntityURI must match SplitURIVersion's base")
			assert.Equal(t, tc.wantTargetColumn, resolved.FullURI,
				"FullURI is the pre-split canonical form — what goes on analyst_outputs.target")
			assert.Equal(t, tc.wantVersion, resolved.Version,
				"Version must match SplitURIVersion's version half")

			// And re-verify directly against SplitURIVersion to catch
			// drift between resolveIngestTarget and the grammar truth.
			base, version := profile.SplitURIVersion(tc.wantTargetColumn)
			assert.Equal(t, tc.wantEntityURI, base)
			assert.Equal(t, tc.wantVersion, version)
		})
	}
}

// TestIngest_IdempotencyUnderCanonicalization: re-ingesting the
// same JSON must still hit the content-hash short-circuit after the
// v10 refactor, not create a new row or redirect to a different
// entity. Content hash is computed over the AnalystOutput struct,
// not the canonicalized URI, so the idempotency check is unaffected
// by Plan-A. This test guards against future refactors accidentally
// coupling them.
func TestIngest_IdempotencyUnderCanonicalization(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	out := minimalAnalystOutput("pkg:npm/idempotency-test@1.0.0")
	r1, err := s.IngestAnalystOutput(ctx, out, "source")
	require.NoError(t, err)
	assert.False(t, r1.Idempotent, "first ingest is a new row")

	r2, err := s.IngestAnalystOutput(ctx, out, "source")
	require.NoError(t, err)
	assert.True(t, r2.Idempotent, "second ingest of same content must hit the hash short-circuit")
	assert.Equal(t, r1.OutputID, r2.OutputID,
		"idempotency must return the same output id")
	assert.Equal(t, r1.EntityID, r2.EntityID,
		"idempotency must return the same entity id — which is the unversioned row under v10")
}
