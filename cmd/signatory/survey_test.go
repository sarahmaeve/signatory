package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/manifest"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
	"github.com/sarahmaeve/signatory/internal/survey"
)

// writeTestManifest creates a temp dir containing a go.mod with
// the given content and returns the directory path. Callers use
// it as either the CWD or the parent of --manifest.
func writeTestManifest(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0o600))
	return dir
}

// seedSurveyEntity inserts an entity into the store.
func seedSurveyEntity(t *testing.T, s store.Store, uri string) *profile.Entity {
	t.Helper()
	e := &profile.Entity{
		ID:           profile.NewEntityID(),
		CanonicalURI: uri,
		Type:         profile.EntityProject,
		ShortName:    filepath.Base(uri),
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	require.NoError(t, s.PutEntity(context.Background(), e))
	return e
}

func seedSurveyPosture(t *testing.T, s store.Store, entityID, version string, tier profile.PostureTier, rationale string) {
	t.Helper()
	require.NoError(t, s.SetPosture(context.Background(), &profile.Posture{
		EntityID:  entityID,
		Tier:      tier,
		Version:   version,
		Rationale: rationale,
		SetBy:     "test",
		SetAt:     time.Now().UTC(),
	}))
}

// seedSurveyPostureAt is the explicit-timestamp variant of
// seedSurveyPosture. Needed for tests that assert on most-recent-
// wins semantics — time.Now() collapses back-to-back calls to the
// same timestamp at sub-second resolution, which makes the
// tiebreak non-deterministic.
func seedSurveyPostureAt(t *testing.T, s store.Store, entityID, version string, tier profile.PostureTier, rationale string, setAt time.Time) {
	t.Helper()
	require.NoError(t, s.SetPosture(context.Background(), &profile.Posture{
		EntityID:  entityID,
		Tier:      tier,
		Version:   version,
		Rationale: rationale,
		SetBy:     "test",
		SetAt:     setAt,
	}))
}

func seedSurveyBurn(t *testing.T, s store.Store, entityID, reason string) {
	t.Helper()
	require.NoError(t, s.SetBurn(context.Background(), &profile.Burn{
		EntityID: entityID,
		Reason:   reason,
		Source:   profile.BurnSourceLocal,
		BurnedAt: time.Now().UTC(),
		BurnedBy: "test",
	}))
}

// runSurvey executes SurveyCmd.Run with Stdout/Stderr pointed at
// test-local buffers. Returns the captured stdout + stderr
// alongside any error Run returned. No global os.Stdout
// redirection — each test owns its own buffers, so parallel
// tests are race-free.
func runSurvey(t *testing.T, cmd *SurveyCmd, globals *Globals) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err = cmd.Run(globals)
	return out.String(), errBuf.String(), err
}

// TestSurvey_Human_AllNotInStore — baseline: empty store, every
// dep reports not-in-store, direct deps surface as action items,
// no indirect-deps list by default.
func TestSurvey_Human_AllNotInStore(t *testing.T) {
	t.Parallel()

	dir := writeTestManifest(t, `module github.com/example/survey-functional

go 1.25.1

require (
	github.com/alecthomas/kong v1.15.0
	gopkg.in/yaml.v3 v3.0.1
)
`)

	globals := testGlobals(t)
	cmd := &SurveyCmd{Manifest: filepath.Join(dir, "go.mod")}

	out, _, err := runSurvey(t, cmd, globals)
	require.NoError(t, err)

	// Project header.
	assert.Contains(t, out, "Surveying go.mod")
	assert.Contains(t, out, "github.com/example/survey-functional")
	assert.Contains(t, out, "go 1.25.1")

	// Summary: 2 direct, 0 indirect, all not-in-store.
	assert.Contains(t, out, "2 dependencies")
	assert.Contains(t, out, "2 direct")
	assert.Contains(t, out, "not-in-store")

	// Direct-deps table.
	assert.Contains(t, out, "github.com/alecthomas/kong")
	assert.Contains(t, out, "gopkg.in/yaml.v3")

	// not-in-store annotation — clarifies that [·] rows are "no
	// data collected yet," distinct from [?] which is "data but
	// no verdict." See tierSummaryAnnotation for why these two
	// states need to be disambiguated.
	assert.Contains(t, out, "(no data collected yet)",
		"not-in-store tier must carry the 'no data collected yet' clarifier in the summary block")

	// Vet-path footer — names the three categories of next move.
	// Guarded on NeedsReview; these deps are all not-in-store
	// (direct) so the footer fires.
	assert.Contains(t, out, "To vet direct dependencies",
		"footer must steer the user toward the actual vet-path categories")
	assert.Contains(t, out, "/analyze <target>",
		"footer must name /analyze (the Claude skill) as the LLM-backed review path")
	assert.Contains(t, out, "signatory posture set",
		"footer must name posture set as the known-verdict path")
	assert.Contains(t, out, "signatory burn",
		"footer must name burn as the known-bad path")

	// No "Action items" / suggested-commands section. The CLI
	// verbs survey previously pointed at (signatory analyze) only
	// collect signals — they cannot produce the trust verdict
	// that flips a [?] row. See the package-level doc on
	// printSurveyHuman for the dropped-section rationale.
	assert.NotContains(t, out, "Action items",
		"survey output must not steer users to commands that don't deliver postures")
	assert.NotContains(t, out, "signatory analyze ",
		"survey output must not embed `signatory analyze` invocations — they collect signals but don't produce verdicts")
}

// TestSurvey_Human_MixedTiers covers the rich output with each
// tier represented. Every rendering branch fires at least once.
func TestSurvey_Human_MixedTiers(t *testing.T) {
	t.Parallel()

	dir := writeTestManifest(t, `module github.com/example/mixed-tiers

go 1.25.1

require (
	github.com/vetted/lib v1.0.0
	github.com/rejected/lib v1.0.0
	github.com/burned/lib v1.0.0
	github.com/unexamined/lib v1.0.0
)
`)

	globals := testGlobals(t)

	// Open the store to seed it. Close explicitly before running
	// survey so survey sees committed state.
	s, err := store.OpenSQLite(context.Background(), globals.DBPath)
	require.NoError(t, err)

	vettedEnt := seedSurveyEntity(t, s, "repo:github/vetted/lib")
	seedSurveyPosture(t, s, vettedEnt.ID, "v1.0.0", profile.PostureVettedFrozen, "strong signals")

	rejectedEnt := seedSurveyEntity(t, s, "repo:github/rejected/lib")
	seedSurveyPosture(t, s, rejectedEnt.ID, "v1.0.0", profile.PostureRejected, "abandoned project")

	burnedEnt := seedSurveyEntity(t, s, "repo:github/burned/lib")
	seedSurveyBurn(t, s, burnedEnt.ID, "supply-chain compromise 2026-04-15")

	// unexamined/lib: entity present, no posture → TierUnexamined.
	seedSurveyEntity(t, s, "repo:github/unexamined/lib")

	require.NoError(t, s.Close())

	cmd := &SurveyCmd{Manifest: filepath.Join(dir, "go.mod")}
	out, _, err := runSurvey(t, cmd, globals)
	require.NoError(t, err)

	// Each tier appears in the summary block.
	for _, tierName := range []string{
		"vetted-frozen",
		"rejected",
		"burned",
		"unexamined",
	} {
		assert.Contains(t, out, tierName, "summary should list %s", tierName)
	}

	// Burn context rendered.
	assert.Contains(t, out, "supply-chain compromise 2026-04-15")

	// Posture rationales rendered.
	assert.Contains(t, out, "strong signals")
	assert.Contains(t, out, "abandoned project")

	// unexamined annotation — this test's fixture includes
	// unexamined/lib as a direct dep (entity seeded, no posture),
	// so the summary block must carry the "signal data in store;
	// no posture verdict yet" clarifier. Companion to the
	// not-in-store annotation test in AllNotInStore; together
	// they cover both non-resolved tiers.
	assert.Contains(t, out, "(signal data in store; no posture verdict yet)",
		"unexamined tier must carry the 'signal data in store' clarifier — distinguishes it from not-in-store")

	// "Only unexamined deps end up in NeedsReview" — that intent
	// is now covered structurally by internal/survey/survey_test.go
	// (TestSurvey_NeedsReview_*). The Action-items rendering that
	// formerly proved it via stdout was removed, so we no longer
	// re-assert it here through a presentation-layer proxy.
}

// TestSurvey_Human_IndirectBreakdown_AllBuckets exercises the
// indirect-reachability breakdown rendering with a Result
// constructed in-memory — so the test doesn't need a real
// go.mod that resolves through `go mod graph`. Asserts each
// bucket's rendered line shape per Option B wording.
//
// Revert proof: change any of "resolved on their own", "inherit
// coverage from resolved directs", "await an unresolved direct"
// in survey.go's renderIndirectBreakdown; this test fails on the
// missing substring.
func TestSurvey_Human_IndirectBreakdown_AllBuckets(t *testing.T) {
	t.Parallel()
	r := survey.Result{
		Project: manifest.ProjectInfo{Ecosystem: "go", ManifestPath: "/x/go.mod"},
		Deps: []survey.DepResult{
			// One indirect of each kind so filterIndirectDeps
			// returns at least one (the indirect block only
			// renders when len(indirect) > 0).
			{Dep: manifest.Dep{Name: "i1", Direct: false}, Tier: survey.TierVettedFrozen},
			{Dep: manifest.Dep{Name: "i2", Direct: false}, Tier: survey.TierNotInStore},
			{Dep: manifest.Dep{Name: "i3", Direct: false}, Tier: survey.TierNotInStore},
		},
		Summary: survey.Summary{
			Total:    3,
			Indirect: 3,
			ByTier:   map[survey.Tier]int{survey.TierNotInStore: 2, survey.TierVettedFrozen: 1},
			IndirectByReachability: survey.IndirectReachabilityBreakdown{
				OwnResolved:   1,
				ViaResolved:   1,
				ViaUnresolved: 1,
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printSurveyHuman(&buf, r, false))
	out := buf.String()

	assert.Contains(t, out, "1 resolved on their own")
	assert.Contains(t, out, "1 inherit coverage from resolved directs")
	assert.Contains(t, out, "1 await an unresolved direct")
	assert.NotContains(t, out, "drill-down unavailable",
		"breakdown is populated, so the fallback line must NOT appear")
}

// TestSurvey_Human_IndirectBreakdown_FallbackWhenUnavailable
// covers the no-graph-data path: when IndirectByReachability is
// zero-valued (the parser returned ErrGraphUnavailable, or no
// graph implementation exists for this ecosystem), the
// breakdown lines are replaced with a single "(drill-down
// unavailable on this system)" line.
func TestSurvey_Human_IndirectBreakdown_FallbackWhenUnavailable(t *testing.T) {
	t.Parallel()
	r := survey.Result{
		Project: manifest.ProjectInfo{Ecosystem: "go", ManifestPath: "/x/go.mod"},
		Deps: []survey.DepResult{
			{Dep: manifest.Dep{Name: "i1", Direct: false}, Tier: survey.TierNotInStore},
		},
		Summary: survey.Summary{
			Total:    1,
			Indirect: 1,
			ByTier:   map[survey.Tier]int{survey.TierNotInStore: 1},
			// IndirectByReachability deliberately zero-valued.
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printSurveyHuman(&buf, r, false))
	out := buf.String()

	assert.Contains(t, out, "(drill-down unavailable on this system)",
		"zero-valued breakdown must produce the fallback line")
	assert.NotContains(t, out, "inherit coverage",
		"fallback path must not emit any of the bucket lines")
}

// TestShortNameFromURI table-locks the URI-to-shortname helper
// across each canonical URI scheme. Reachability tags in --all
// mode use this for "inherit via <short>" / "awaits <short>"
// — incorrect shortening would surface as ugly tag text but
// nothing structurally breaks, so this is presentation regression
// guard rather than safety guard.
func TestShortNameFromURI(t *testing.T) {
	t.Parallel()
	cases := []struct {
		uri  string
		want string
	}{
		{"repo:github/sarahmaeve/signatory", "signatory"},
		{"pkg:go/gopkg.in/yaml.v3", "yaml.v3"},
		{"pkg:go/modernc.org/sqlite", "sqlite"},
		{"pkg:npm/express", "express"},
		{"pkg:npm/@sindresorhus/is", "is"},
		{"identity:github/alecthomas", "alecthomas"},
		{"plain-string-no-scheme", "plain-string-no-scheme"},
	}
	for _, c := range cases {
		t.Run(c.uri, func(t *testing.T) {
			assert.Equal(t, c.want, shortNameFromURI(c.uri))
		})
	}
}

// TestReachabilityLabel covers the three label-producing
// scenarios (await, inherit, none) plus the precedence rule
// for diamond cases (max-pessimism: any unresolved reaching
// direct yields "awaits", not "inherit").
func TestReachabilityLabel(t *testing.T) {
	t.Parallel()
	directTier := map[string]survey.Tier{
		"repo:github/example/resolved":   survey.TierVettedFrozen,
		"repo:github/example/unresolved": survey.TierUnexamined,
	}

	cases := []struct {
		name string
		dep  survey.DepResult
		want string
	}{
		{
			name: "single resolved parent → inherit via",
			dep: survey.DepResult{
				Tier: survey.TierNotInStore,
				Reachability: &survey.Reachability{
					FromDirects: []string{"repo:github/example/resolved"},
				},
			},
			want: "inherit via resolved",
		},
		{
			name: "single unresolved parent → awaits",
			dep: survey.DepResult{
				Tier: survey.TierNotInStore,
				Reachability: &survey.Reachability{
					FromDirects: []string{"repo:github/example/unresolved"},
				},
			},
			want: "awaits unresolved",
		},
		{
			name: "diamond (one resolved, one unresolved) → awaits the blocker (max-pessimism)",
			dep: survey.DepResult{
				Tier: survey.TierNotInStore,
				Reachability: &survey.Reachability{
					FromDirects: []string{
						"repo:github/example/resolved",
						"repo:github/example/unresolved",
					},
				},
			},
			want: "awaits unresolved",
		},
		{
			name: "own-resolved indirect → no label (rationale wins)",
			dep: survey.DepResult{
				Tier: survey.TierVettedFrozen,
				Reachability: &survey.Reachability{
					FromDirects: []string{"repo:github/example/resolved"},
				},
			},
			want: "",
		},
		{
			name: "no reachability → no label",
			dep: survey.DepResult{
				Tier:         survey.TierNotInStore,
				Reachability: nil,
			},
			want: "",
		},
		{
			name: "reachability with empty FromDirects → no label",
			dep: survey.DepResult{
				Tier:         survey.TierNotInStore,
				Reachability: &survey.Reachability{},
			},
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, reachabilityLabel(c.dep, directTier))
		})
	}
}

// TestSurvey_Human_IndirectRow_AwaitsTagUnderAll asserts the
// per-row "awaits <direct>" tag actually renders under --all.
// Direct fixture: one resolved direct (R), one unresolved direct
// (U), one indirect reached via U. Under --all the indirect row
// must carry "awaits u" (where 'u' is the short name of U).
func TestSurvey_Human_IndirectRow_AwaitsTagUnderAll(t *testing.T) {
	t.Parallel()
	r := survey.Result{
		Project: manifest.ProjectInfo{Ecosystem: "go", ManifestPath: "/x/go.mod"},
		Deps: []survey.DepResult{
			{
				Dep:  manifest.Dep{Name: "u", CanonicalURI: "repo:github/example/u", Direct: true},
				Tier: survey.TierUnexamined,
			},
			{
				Dep:  manifest.Dep{Name: "transitive-i", CanonicalURI: "pkg:go/example.com/transitive-i", Direct: false},
				Tier: survey.TierNotInStore,
				Reachability: &survey.Reachability{
					FromDirects: []string{"repo:github/example/u"},
				},
			},
		},
		Summary: survey.Summary{
			Total: 2, Direct: 1, Indirect: 1,
			ByTier: map[survey.Tier]int{
				survey.TierUnexamined:  1,
				survey.TierNotInStore: 1,
			},
			IndirectByReachability: survey.IndirectReachabilityBreakdown{
				ViaUnresolved: 1,
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printSurveyHuman(&buf, r, true)) // --all
	out := buf.String()

	assert.Contains(t, out, "awaits u",
		"indirect row must carry the awaits tag naming the unresolved direct")
	assert.NotContains(t, out, "inherit via",
		"no resolved-only indirect in this fixture; inherit tag must not appear")
}

// TestSurvey_Human_IndirectRow_InheritTagUnderAll covers the
// other half: one indirect reached only via a resolved direct
// gets "inherit via <short>" tag.
func TestSurvey_Human_IndirectRow_InheritTagUnderAll(t *testing.T) {
	t.Parallel()
	r := survey.Result{
		Project: manifest.ProjectInfo{Ecosystem: "go", ManifestPath: "/x/go.mod"},
		Deps: []survey.DepResult{
			{
				Dep:  manifest.Dep{Name: "r", CanonicalURI: "repo:github/example/r", Direct: true},
				Tier: survey.TierVettedFrozen,
			},
			{
				Dep:  manifest.Dep{Name: "transitive-i", CanonicalURI: "pkg:go/example.com/transitive-i", Direct: false},
				Tier: survey.TierNotInStore,
				Reachability: &survey.Reachability{
					FromDirects: []string{"repo:github/example/r"},
				},
			},
		},
		Summary: survey.Summary{
			Total: 2, Direct: 1, Indirect: 1,
			ByTier: map[survey.Tier]int{
				survey.TierVettedFrozen: 1,
				survey.TierNotInStore:   1,
			},
			IndirectByReachability: survey.IndirectReachabilityBreakdown{
				ViaResolved: 1,
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printSurveyHuman(&buf, r, true)) // --all
	out := buf.String()

	assert.Contains(t, out, "inherit via r",
		"indirect row must carry the inherit-via tag naming the resolved direct")
}

// TestSurvey_Human_IndirectBreakdown_RendersUnderAllToo is the
// regression guard for the bug where --all mode skipped the
// reachability breakdown. The breakdown belongs in BOTH modes:
// --all means "give me the per-row detail too," not "hide the
// summary." Pre-fix this test fails because the breakdown lines
// are absent from --all stdout.
func TestSurvey_Human_IndirectBreakdown_RendersUnderAllToo(t *testing.T) {
	t.Parallel()
	r := survey.Result{
		Project: manifest.ProjectInfo{Ecosystem: "go", ManifestPath: "/x/go.mod"},
		Deps: []survey.DepResult{
			{Dep: manifest.Dep{Name: "i1", Direct: false}, Tier: survey.TierNotInStore},
			{Dep: manifest.Dep{Name: "i2", Direct: false}, Tier: survey.TierNotInStore},
		},
		Summary: survey.Summary{
			Total: 2, Indirect: 2,
			ByTier: map[survey.Tier]int{survey.TierNotInStore: 2},
			IndirectByReachability: survey.IndirectReachabilityBreakdown{
				ViaResolved:   1,
				ViaUnresolved: 1,
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printSurveyHuman(&buf, r, true)) // includeIndirect=true → --all mode
	out := buf.String()

	// Per-row list still renders.
	assert.Contains(t, out, "i1", "per-row detail must appear under --all")
	assert.Contains(t, out, "i2")

	// AND the breakdown renders. This is the bug fix: pre-fix the
	// breakdown was suppressed under --all.
	assert.Contains(t, out, "1 inherit coverage from resolved directs",
		"breakdown must appear under --all (the user expanded; they didn't ask to hide the summary)")
	assert.Contains(t, out, "1 await an unresolved direct")
}

// TestSurvey_Human_IndirectBreakdown_OnlyNonzeroBucketsRender
// confirms zero-count buckets are skipped — a project where
// every indirect is OwnResolved gets a one-line breakdown, not
// three. Keeps the output compact for fully-vetted projects.
func TestSurvey_Human_IndirectBreakdown_OnlyNonzeroBucketsRender(t *testing.T) {
	t.Parallel()
	r := survey.Result{
		Project: manifest.ProjectInfo{Ecosystem: "go", ManifestPath: "/x/go.mod"},
		Deps: []survey.DepResult{
			{Dep: manifest.Dep{Name: "i1", Direct: false}, Tier: survey.TierVettedFrozen},
		},
		Summary: survey.Summary{
			Total:    1,
			Indirect: 1,
			ByTier:   map[survey.Tier]int{survey.TierVettedFrozen: 1},
			IndirectByReachability: survey.IndirectReachabilityBreakdown{
				OwnResolved: 1, // others 0
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printSurveyHuman(&buf, r, false))
	out := buf.String()

	assert.Contains(t, out, "1 resolved on their own")
	assert.NotContains(t, out, "inherit coverage",
		"zero-count ViaResolved bucket must not render")
	assert.NotContains(t, out, "await an unresolved",
		"zero-count ViaUnresolved bucket must not render")
}

// TestSurvey_Human_OtherVersionsSuffix covers the per-row suffix
// rendering when the dep's pinned version has no exact-match
// posture but prior-version postures exist. The suffix should
// surface the most-recent posture's version + tier and the total
// posture count — visibility only, no action recommendation.
//
// Revert proof: change the suffix template in renderDep back to
// "(other versions in store)"; this test fails because the new
// substrings are absent from stdout.
func TestSurvey_Human_OtherVersionsSuffix(t *testing.T) {
	t.Parallel()

	dir := writeTestManifest(t, `module github.com/example/other-versions

go 1.25.1

require github.com/alecthomas/kong v1.15.0
`)

	globals := testGlobals(t)
	s, err := store.OpenSQLite(context.Background(), globals.DBPath)
	require.NoError(t, err)

	// Seed entity + two prior-version postures. v1.14.0 is the
	// most recent (set_at is larger); v1.10.0 is older. The
	// queried version (v1.15.0, from the manifest) has no posture.
	e := seedSurveyEntity(t, s, "repo:github/alecthomas/kong")
	seedSurveyPostureAt(t, s, e.ID, "v1.10.0", profile.PostureUnknownProvenance,
		"early look", time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	seedSurveyPostureAt(t, s, e.ID, "v1.14.0", profile.PostureVettedFrozen,
		"full review", time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC))

	require.NoError(t, s.Close())

	cmd := &SurveyCmd{Manifest: filepath.Join(dir, "go.mod")}
	out, _, err := runSurvey(t, cmd, globals)
	require.NoError(t, err)

	// The dep row's suffix should name the most-recent version
	// and tier, plus the count. We assert on the key substrings
	// rather than the full formatted line to stay resilient to
	// column-width tweaks.
	assert.Contains(t, out, "v1.14.0 vetted-frozen",
		"suffix must name the most-recent prior-version posture's version and tier")
	assert.Contains(t, out, "2 postures on record",
		"suffix must carry the total posture count (plural form for N>1)")

	// The old hedge must not reappear.
	assert.NotContains(t, out, "other versions in store",
		"the vague `(other versions in store)` hedge was replaced with concrete data")
}

// TestSurvey_JSON_OutputShape asserts --json output is a
// parseable Result. Future tooling (web UI, CI gates) relies on
// the schema.
func TestSurvey_JSON_OutputShape(t *testing.T) {
	t.Parallel()

	dir := writeTestManifest(t, `module github.com/example/json-shape

go 1.25.1

require github.com/alecthomas/kong v1.15.0
`)

	globals := testGlobals(t)
	cmd := &SurveyCmd{Manifest: filepath.Join(dir, "go.mod"), JSON: true}

	out, _, err := runSurvey(t, cmd, globals)
	require.NoError(t, err)

	var result survey.Result
	require.NoError(t, json.Unmarshal([]byte(out), &result),
		"--json output must parse as a survey.Result")

	assert.Equal(t, "github.com/example/json-shape", result.Project.Name)
	assert.Equal(t, "go", result.Project.Ecosystem)
	assert.Equal(t, "1.25.1", result.Project.EcoVersion)
	require.Len(t, result.Deps, 1)
	assert.Equal(t, "github.com/alecthomas/kong", result.Deps[0].Dep.Name)
	assert.Equal(t, survey.TierNotInStore, result.Deps[0].Tier)
	assert.Equal(t, 1, result.Summary.Direct)
	assert.Equal(t, 1, result.Summary.ByTier[survey.TierNotInStore])
}

// TestSurvey_AutoDetectCWD exercises the no-flag path: running
// survey without --manifest from a dir containing go.mod. The
// command should detect and parse it.
//
// NOT parallel — this test mutates the process-global CWD.
func TestSurvey_AutoDetectCWD(t *testing.T) {
	dir := writeTestManifest(t, `module github.com/example/auto-detect

go 1.25.1
`)

	origCWD, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() {
		_ = os.Chdir(origCWD)
	})

	globals := testGlobals(t)
	cmd := &SurveyCmd{}

	out, _, err := runSurvey(t, cmd, globals)
	require.NoError(t, err)
	assert.Contains(t, out, "github.com/example/auto-detect")
}

// TestSurvey_NoManifestNoCWDMatch fails cleanly when invoked
// from a directory without a recognized manifest and without
// an explicit --manifest flag.
//
// NOT parallel — mutates the process-global CWD.
func TestSurvey_NoManifestNoCWDMatch(t *testing.T) {
	dir := t.TempDir() // empty

	origCWD, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() {
		_ = os.Chdir(origCWD)
	})

	globals := testGlobals(t)
	cmd := &SurveyCmd{}
	_, _, err = runSurvey(t, cmd, globals)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no recognized manifest")
	assert.Contains(t, err.Error(), "pass --manifest")
}

// TestSurvey_RefreshFlagEmitsWarning — --refresh is accepted but
// emits a stderr note in v0.1. This guards against a silent
// regression if someone wires partial refresh behavior without
// updating the help text.
func TestSurvey_RefreshFlagEmitsWarning(t *testing.T) {
	t.Parallel()

	dir := writeTestManifest(t, `module github.com/example/refresh-warn

go 1.25.1
`)

	globals := testGlobals(t)
	cmd := &SurveyCmd{
		Manifest: filepath.Join(dir, "go.mod"),
		Refresh:  true,
	}

	_, stderr, err := runSurvey(t, cmd, globals)
	require.NoError(t, err)
	assert.Contains(t, stderr, "--refresh is not implemented",
		"should warn when --refresh is passed but ignored")
}

// TestSurvey_All_ListsIndirectDeps covers the --all flag:
// indirect deps get rendered individually instead of collapsed
// into a count.
func TestSurvey_All_ListsIndirectDeps(t *testing.T) {
	t.Parallel()

	dir := writeTestManifest(t, `module github.com/example/all-flag

go 1.25.1

require github.com/alecthomas/kong v1.15.0

require github.com/stretchr/testify v1.11.1 // indirect
`)

	// Default: indirect summarized by count.
	defaultOut, _, err := runSurvey(t, &SurveyCmd{Manifest: filepath.Join(dir, "go.mod")}, testGlobals(t))
	require.NoError(t, err)
	assert.Contains(t, defaultOut, "Indirect dependencies: 1")
	assert.NotContains(t, defaultOut, "github.com/stretchr/testify",
		"default (non-all) output should NOT list indirect deps individually")

	// With --all: indirect listed.
	allOut, _, err := runSurvey(t, &SurveyCmd{Manifest: filepath.Join(dir, "go.mod"), All: true}, testGlobals(t))
	require.NoError(t, err)
	assert.Contains(t, allOut, "github.com/stretchr/testify",
		"--all output should list indirect deps")
}
