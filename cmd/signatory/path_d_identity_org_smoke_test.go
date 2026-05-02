package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// Path D smoke tests: exercise the identity:/org: URI surface
// end-to-end through the CLI command Run() entry points to find
// rough edges before automating identity/org entity creation
// (Path A in the entity-burn roadmap).
//
// These replace the ad-hoc shell probing recorded in the design
// notes — same coverage, but reproducible, isolated to t.TempDir
// databases, and runnable as part of the standard `go test` pass.
//
// Each test pins one observed CLI behaviour. Where the behaviour
// is desirable, the assertion serves as a regression guard. Where
// the behaviour is surprising but accepted (silent-skip on
// analyze --refresh against a non-collectable entity type, etc.),
// the test documents the current shape with a t.Logf finding so
// the gap is visible in test output and traceable to design
// follow-ups.
//
// Tests in this file are intentionally NOT t.Parallel(). The
// captureStdout helper (handoff_test.go) swaps the global
// os.Stdout pointer; running capture-using tests concurrently
// would let them fight over it and leak each other's output.
// Existing convention across cmd/signatory matches: see the
// SummaryCmd / PostureGetCmd tests in posture_canonical_test.go.

// TestPathD_BurnAdd_IdentityURI_MintsIdentityEntity covers the
// identity:<platform>/<login> path through `signatory burn add`.
// Asserts the post-PR0 invariant that the entity row carries
// Type=EntityIdentity (not the legacy hardcoded EntityProject)
// and that the burn row attaches correctly.
func TestPathD_BurnAdd_IdentityURI_MintsIdentityEntity(t *testing.T) {
	g := newTestGlobals(t)

	cmd := &BurnAddCmd{
		Target: "identity:github/bufferzonecorp",
		Reason: "test: campaign-shaped account, 11 days old, 17 throwaway repos",
	}
	captureStdout(t, func() {
		require.NoError(t, cmd.Run(g))
	})

	s, err := g.OpenStore(t.Context())
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	entity, err := s.FindEntityByURI(t.Context(), "identity:github/bufferzonecorp")
	require.NoError(t, err, "burn add must mint the entity even when the URI is brand-new to the store")
	assert.Equal(t, profile.EntityIdentity, entity.Type,
		"identity:<platform>/<login> URIs must produce Type=EntityIdentity rows — that's the post-PR0 contract")
	assert.Equal(t, "identity:github/bufferzonecorp", entity.CanonicalURI)
	assert.Equal(t, "bufferzonecorp", entity.ShortName,
		"ShortName should be the bare login, not the full URI")

	burn, err := s.GetBurn(t.Context(), entity.ID)
	require.NoError(t, err, "the burn row must attach to the freshly-minted identity entity")
	assert.Contains(t, burn.Reason, "campaign-shaped",
		"the burn reason must round-trip through the store")
}

// TestPathD_BurnAdd_OrgURI_MintsOrgEntity is the org: parallel.
// org:<platform>/<name> for a publisher-org account (the
// Organization-typed analog of identity:<login>).
func TestPathD_BurnAdd_OrgURI_MintsOrgEntity(t *testing.T) {
	g := newTestGlobals(t)

	cmd := &BurnAddCmd{
		Target: "org:github/some-malicious-org",
		Reason: "test: hypothetical org-as-publisher campaign",
	}
	captureStdout(t, func() {
		require.NoError(t, cmd.Run(g))
	})

	s, err := g.OpenStore(t.Context())
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	entity, err := s.FindEntityByURI(t.Context(), "org:github/some-malicious-org")
	require.NoError(t, err)
	assert.Equal(t, profile.EntityOrg, entity.Type,
		"org:<platform>/<name> URIs must produce Type=EntityOrg rows")
	assert.Equal(t, "some-malicious-org", entity.ShortName)

	burn, err := s.GetBurn(t.Context(), entity.ID)
	require.NoError(t, err)
	assert.Contains(t, burn.Reason, "org-as-publisher")
}

// TestPathD_BurnList_RendersIdentityAndOrgRows asserts that
// `signatory burn list` surfaces both identity: and org: entity
// burns alongside the project/package burns the list verb was
// originally designed for. Exercises the rendering path for the
// less-common entity types.
func TestPathD_BurnList_RendersIdentityAndOrgRows(t *testing.T) {
	g := newTestGlobals(t)

	// Seed three burns of different entity types so the list has
	// to render heterogeneous rows.
	for _, target := range []string{
		"identity:github/operator-x",
		"org:github/operator-org",
		"pkg:npm/some-pkg",
	} {
		captureStdout(t, func() {
			require.NoError(t, (&BurnAddCmd{Target: target, Reason: "test seed"}).Run(g))
		})
	}

	stdout := captureStdout(t, func() {
		require.NoError(t, (&BurnListCmd{}).Run(g))
	})

	assert.Contains(t, stdout, "identity:github/operator-x",
		"burn list must include identity: rows in its rendering")
	assert.Contains(t, stdout, "org:github/operator-org",
		"burn list must include org: rows in its rendering")
	assert.Contains(t, stdout, "pkg:npm/some-pkg",
		"sanity: pkg: rows must still render alongside the new types")
}

// TestPathD_Summary_IdentityURI_RendersBurnAndType pins the
// human-readable summary output for an identity entity. The
// rendering path was originally designed around project/package
// shapes; identity/org shapes lack URL/ecosystem and have no
// analyses-rollup data — the test confirms the renderer handles
// the absence cleanly rather than crashing or printing empty
// labels.
func TestPathD_Summary_IdentityURI_RendersBurnAndType(t *testing.T) {
	g := newTestGlobals(t)

	// Seed: burn the identity entity so the summary has burn data
	// to render.
	captureStdout(t, func() {
		require.NoError(t, (&BurnAddCmd{
			Target: "identity:github/operator-x",
			Reason: "test: render check",
		}).Run(g))
	})

	cmd := &SummaryCmd{Target: "identity:github/operator-x"}
	stdout := captureStdout(t, func() {
		require.NoError(t, cmd.Run(g))
	})

	assert.Contains(t, stdout, "URI:       identity:github/operator-x",
		"summary must echo the canonical URI verbatim")
	assert.Contains(t, stdout, "Type:      identity",
		"summary must render the entity type so the operator knows what kind of row this is")
	assert.Contains(t, stdout, "BURNED",
		"the burn marker must appear when an active burn exists")
	assert.Contains(t, stdout, "render check",
		"the burn reason must surface in the summary output")

	// URL field is project-shaped; identity rows have no URL.
	// Renderer correctly omits the line entirely (per summary.go:81-83
	// `if s.URL != ""`).
	assert.NotContains(t, stdout, "URL:       \n",
		"the URL: line must not render with an empty value for identity entities")
}

// TestPathD_Summary_OrgURI_RendersBurnAndType is the org: parallel.
func TestPathD_Summary_OrgURI_RendersBurnAndType(t *testing.T) {
	g := newTestGlobals(t)

	captureStdout(t, func() {
		require.NoError(t, (&BurnAddCmd{
			Target: "org:github/operator-org",
			Reason: "test: org render check",
		}).Run(g))
	})

	cmd := &SummaryCmd{Target: "org:github/operator-org"}
	stdout := captureStdout(t, func() {
		require.NoError(t, cmd.Run(g))
	})

	assert.Contains(t, stdout, "URI:       org:github/operator-org")
	assert.Contains(t, stdout, "Type:      org")
	assert.Contains(t, stdout, "BURNED")
	assert.Contains(t, stdout, "org render check")
}

// TestPathD_Summary_NeverSeenIdentity_SoftAbsence asserts that
// summary on an identity URI with no row in the store returns a
// soft absence (no error, exit 0, "no record" message) — matching
// the contract pinned for pkg:/repo: in
// TestSummary_UnknownTarget_SoftAbsence (posture_canonical_test.go:134).
//
// This is the parity check: identity/org URIs must not regress the
// soft-absence contract.
func TestPathD_Summary_NeverSeenIdentity_SoftAbsence(t *testing.T) {
	g := newTestGlobals(t)

	cmd := &SummaryCmd{Target: "identity:github/never-seen-account"}
	stdout := captureStdout(t, func() {
		require.NoError(t, cmd.Run(g),
			"summary on a never-ingested identity must NOT error — soft absence, like pkg: and repo:")
	})

	assert.Contains(t, stdout, "never-seen-account",
		"the soft-absence message must name the queried target")
	assert.Contains(t, stdout, "No signatory record",
		"the soft-absence wording must match the contract used for the other URI schemes")
}

// TestPathD_Analyze_IdentityURI_FreshDB_RejectsScheme pins the
// create-path error: when AnalyzeCmd.Run hits an identity:/org:
// URI that has no entity row yet, the per-scheme switch in
// analyze.go falls through to the default branch and returns
//
//	"analyze does not yet support %q-scheme targets"
//
// This is the correct behaviour for v0.1 — analyze is for
// repo:/pkg: targets that have collectable signals; identity/org
// rows are populated through other paths (burn add, ingest,
// future Path A producer wiring).
//
// Pinning this in a test prevents a future refactor from silently
// extending analyze to identity/org without thinking through what
// "analyze an identity" should mean.
func TestPathD_Analyze_IdentityURI_FreshDB_RejectsScheme(t *testing.T) {
	g := newTestGlobals(t)
	// No collectors registered — the create-path scheme guard fires
	// before any collector runs, so collector setup is irrelevant
	// here.

	cmd := &AnalyzeCmd{Target: "identity:github/never-seen-account", Refresh: true}
	err := cmd.Run(g)
	require.Error(t, err,
		"analyze --refresh on a fresh-DB identity: target must error — the analyze flow doesn't know what to collect for an identity")
	assert.True(t, strings.Contains(err.Error(), "identity") &&
		strings.Contains(err.Error(), "scheme"),
		"the error must name the scheme so the user knows analyze isn't the right verb here, got: %v", err)
	assert.Contains(t, err.Error(), "summary",
		"the error must point users at the right verb (`signatory summary`) for viewing cached state, got: %v", err)
}

// TestPathD_Analyze_OrgURI_FreshDB_RejectsScheme is the org:
// parallel. Same shape, same expected failure.
func TestPathD_Analyze_OrgURI_FreshDB_RejectsScheme(t *testing.T) {
	g := newTestGlobals(t)
	// No collectors needed — same reason as the identity: parallel above.

	cmd := &AnalyzeCmd{Target: "org:github/never-seen-org", Refresh: true}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "org") &&
		strings.Contains(err.Error(), "scheme"),
		"the error must name the scheme, got: %v", err)
	assert.Contains(t, err.Error(), "summary",
		"the error must point users at the right verb (`signatory summary`) for viewing cached state, got: %v", err)
}

// TestPathD_Analyze_IdentityURI_AlreadyMinted_RejectsScheme is the
// load-path companion to the FreshDB rejection test: when an
// identity entity ALREADY exists in the store (e.g. minted by a
// prior `burn add`), `signatory analyze --refresh identity:...`
// must still reject the operation rather than silently producing
// zero signals.
//
// Why this matters: the create-path scheme guard in analyze.go's
// per-scheme switch only fires when the entity has to be created.
// Without a top-level guard, an existing identity entity loaded
// from the store skipped the rejection path, then collectorsFor
// returned an empty slice (no Ecosystem registry match, not
// git-hosted), and the loop terminated with `Stored 0 signals`
// and exit 0 — misleading because the user-facing message read
// "Collecting signals for: identity:..." while doing nothing.
//
// The fix lifts the scheme guard to fire right after target
// resolution, covering both the create-path and the load-path
// in one place. The error message names `signatory summary` as
// the right verb for viewing the cached state of identity/org
// entities, so users don't have to guess.
func TestPathD_Analyze_IdentityURI_AlreadyMinted_RejectsScheme(t *testing.T) {
	g := newTestGlobals(t)

	// Pre-mint the identity entity via burn add so the analyze
	// flow finds an existing row when it does its lookup.
	captureStdout(t, func() {
		require.NoError(t, (&BurnAddCmd{
			Target: "identity:github/already-minted",
			Reason: "test: pre-mint for analyze probe",
		}).Run(g))
	})

	cmd := &AnalyzeCmd{Target: "identity:github/already-minted", Refresh: true}
	err := cmd.Run(g)
	require.Error(t, err,
		"analyze --refresh on an existing identity entity must reject the unsupported scheme rather than silently exit 0")
	assert.Contains(t, err.Error(), "identity",
		"the error must name the rejected scheme so the user knows what's unsupported, got: %v", err)
	assert.Contains(t, err.Error(), "summary",
		"the error must point users at the right verb (`signatory summary`) for viewing cached state, got: %v", err)
}
