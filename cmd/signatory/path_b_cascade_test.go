package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// Path B end-to-end: with Path A populating owner entities and the
// cascade resolver in place, burning an operator's identity must
// surface as a "burned via owner" rendering on every repo that
// owner publishes — without per-repo manual burn add.
//
// This is the BufferZone case from countercampaign.md, finally
// closed end-to-end:
//
//   1. analyze --refresh repo:github/bufferzonecorp/grpc-client
//      → minted identity:github/bufferzonecorp + owner_profile signal
//   2. burn add identity:github/bufferzonecorp
//      → burn row on the identity entity
//   3. summary repo:github/bufferzonecorp/grpc-client
//      → "BURNED ... via owner identity:github/bufferzonecorp"
//
// Driven through the actual CLI surface via runCLI (same helper
// path_d_cli_test.go uses) so exit codes, stdout/stderr placement,
// and the kong parsing layer are all exercised.
//
// burn list (the audit surface) MUST keep showing the literal burn
// on the identity, NOT the cascaded one on the repo. That's pinned
// here too — countercampaign.md §7.7 split: display callers move to
// EffectiveBurn, audit callers stay on GetBurn/ListBurns.

// seedCascadeScenario sets up the standard "operator burned, repo
// they published is queried" state in a fresh DB:
//
//   - identity:github/bufferzonecorp entity exists and is burned
//   - repo:github/bufferzonecorp/grpc-client entity exists
//   - owner_profile signal on the repo points at bufferzonecorp
//
// Returns the DB path so tests can drive runCLI against it.
func seedCascadeScenario(t *testing.T) string {
	t.Helper()

	dbPath := newCLITestDB(t)

	// Mint the identity entity via burn add.
	add := runCLI(t, dbPath,
		"burn", "add", "identity:github/bufferzonecorp",
		"--reason", "campaign-shaped account, 17 throwaway repos",
	)
	require.Equal(t, 0, add.exitCode, "burn add must succeed; stderr=%q", add.stderr)

	// Open the store directly to seed the repo entity + signal.
	// AnalyzeCmd would be the production path but it requires a
	// mocked github API; this test focuses on the cascade side, so
	// we synthesize the owner_profile signal directly.
	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	// The Path-A integration tests already cover the "analyze run
	// mints + emits the signal" case end-to-end. Here we want the
	// post-Path-A cascade behaviour, so we put the row + signal in
	// directly via the public store API.
	now := time.Now().UTC().Truncate(time.Second)
	repo := &profile.Entity{
		ID:           profile.NewEntityID(),
		CanonicalURI: "repo:github/bufferzonecorp/grpc-client",
		Type:         profile.EntityProject,
		ShortName:    "bufferzonecorp/grpc-client",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	require.NoError(t, s.PutEntity(t.Context(), repo))

	ownerProfile, err := json.Marshal(map[string]any{
		"login": "bufferzonecorp",
		"type":  "User",
	})
	require.NoError(t, err)
	sig := profile.Signal{
		ID:                profile.NewEntityID(),
		EntityID:          repo.ID,
		Type:              "owner_profile",
		Group:             profile.SignalGroupGovernance,
		Source:            "github",
		ForgeryResistance: profile.ForgeryVeryHigh,
		Value:             ownerProfile,
		CollectedAt:       now,
		ExpiresAt:         now.Add(time.Hour),
	}
	require.NoError(t, s.AppendSignals(t.Context(), []profile.Signal{sig}))

	return dbPath
}

// TestPathB_CLI_Summary_Repo_ShowsCascadeFromBurnedOwner is the
// load-bearing end-to-end test for Path B. With the operator burned
// and Path A's owner_profile signal in place, summary on a repo the
// operator publishes must show:
//
//   - the BURNED marker (the cascade fired),
//   - the cascaded reason in the rendered output,
//   - some indication the burn cascaded from the owner (so the
//     user knows which ledger entry caused the degradation).
func TestPathB_CLI_Summary_Repo_ShowsCascadeFromBurnedOwner(t *testing.T) {
	dbPath := seedCascadeScenario(t)

	r := runCLI(t, dbPath,
		"summary", "repo:github/bufferzonecorp/grpc-client",
	)

	require.Equal(t, 0, r.exitCode, "summary must exit 0; stderr=%q", r.stderr)
	assert.Contains(t, r.stdout, "BURNED",
		"summary must surface the burn marker even though the burn is on the owner, not the repo itself")
	assert.Contains(t, r.stdout, "campaign-shaped",
		"the cascaded burn reason must surface in the rendered output")
	assert.Contains(t, r.stdout, "identity:github/bufferzonecorp",
		"the rendering must name the cascade source so users can trace which ledger entry caused the degradation")
}

// TestPathB_CLI_Summary_Identity_ShowsDirectBurn is the parallel
// pin: summary on the operator's identity entity itself shows the
// direct burn (Direct=true path), not via-owner phrasing. Same
// machinery, different code path through EffectiveBurn — this
// guards against a renderer that mistakenly applies the cascade-
// rendering branch to direct burns.
func TestPathB_CLI_Summary_Identity_ShowsDirectBurn(t *testing.T) {
	dbPath := seedCascadeScenario(t)

	r := runCLI(t, dbPath,
		"summary", "identity:github/bufferzonecorp",
	)

	require.Equal(t, 0, r.exitCode, "summary must exit 0; stderr=%q", r.stderr)
	assert.Contains(t, r.stdout, "BURNED")
	assert.Contains(t, r.stdout, "campaign-shaped",
		"the direct-burn reason surfaces verbatim")
	// The "via owner" cascade phrase must NOT appear when the burn
	// is direct on the queried entity. That's how the rendering
	// distinguishes the two cases.
	assert.NotContains(t, r.stdout, "via owner",
		"direct burn rendering must NOT include the cascade phrase — Direct=true is its own case")
}

// TestPathB_CLI_AnalyzeRefresh_BurnedOwner_GateAborts pins the
// pre-collection gate: when the operator is burned, analyze
// --refresh on a brand-new repo by that operator must refuse to
// run collectors before any network or filesystem work happens.
// Exits non-zero with a clear message naming the cascade source.
//
// This is the answer to "burned vendor = not safe, period" — the
// /analyze pipeline's primary use case is "is this safe?" and a
// burned operator is the strongest possible negative answer.
//
// Uses a brand-new repo URI (no entity row, no signals) to
// exercise the URI-derived branch of EffectiveBurnByURI — proves
// the gate works on first contact with a burned operator's tree,
// not just on cached entities.
func TestPathB_CLI_AnalyzeRefresh_BurnedOwner_GateAborts(t *testing.T) {
	dbPath := newCLITestDB(t)

	// Burn the operator. The repo we'll target doesn't exist in
	// the store yet — only the operator burn + the URI structure
	// tell the gate this is unsafe.
	add := runCLI(t, dbPath,
		"burn", "add", "identity:github/bufferzonecorp",
		"--reason", "campaign-shaped account, 17 throwaway repos",
	)
	require.Equal(t, 0, add.exitCode, "burn add must succeed; stderr=%q", add.stderr)

	// Now refresh a brand-new repo by that operator. The gate must
	// fire and exit non-zero before any collector runs.
	r := runCLI(t, dbPath,
		"analyze", "--refresh", "repo:github/bufferzonecorp/never-seen-repo",
	)

	assert.NotEqual(t, 0, r.exitCode,
		"analyze --refresh against a burned-operator's repo must exit non-zero; stdout=%q stderr=%q", r.stdout, r.stderr)
	assert.Contains(t, r.stderr, "refusing to collect",
		"the abort message must be unambiguous about the refusal")
	assert.Contains(t, r.stderr, "identity:github/bufferzonecorp",
		"the abort message must name the cascade source so the user can trace it")
	assert.Contains(t, r.stderr, "campaign-shaped",
		"the burn reason must surface so the user knows WHY it's burned")
	assert.Contains(t, r.stderr, "--ignore-burn",
		"the abort message must surface the override flag so the user knows the escape hatch")
	assert.Contains(t, r.stderr, "burn remove",
		"the abort message must surface the unburn path for cases where the burn was premature")
}

// TestPathB_CLI_AnalyzeRefresh_BurnedOwner_IgnoreBurn_Proceeds pins
// the override contract: --ignore-burn skips the gate. The user
// has explicitly opted in to running collectors against a known-
// burned target (forensic / verification case). The collectors
// may still fail or produce no signals in the test environment
// (no real github API), but the gate must not block the attempt.
func TestPathB_CLI_AnalyzeRefresh_BurnedOwner_IgnoreBurn_Proceeds(t *testing.T) {
	dbPath := newCLITestDB(t)

	add := runCLI(t, dbPath,
		"burn", "add", "identity:github/bufferzonecorp",
		"--reason", "campaign-shaped account",
	)
	require.Equal(t, 0, add.exitCode)

	// With --ignore-burn, the gate does NOT abort. The collectors
	// then try to hit the real github API, which we can't redirect
	// from runCLI (it parses --db only, not --github-base-url).
	// Concrete assertion: stderr must NOT contain the gate-refusal
	// message, even if collection eventually fails for other
	// reasons (network, rate limit). We're pinning the gate, not
	// the collection happy-path.
	r := runCLI(t, dbPath,
		"analyze", "--refresh", "--ignore-burn",
		"repo:github/bufferzonecorp/never-seen-repo",
	)
	assert.NotContains(t, r.stderr, "refusing to collect",
		"--ignore-burn must skip the gate; stderr=%q", r.stderr)
}

// TestPathB_CLI_AnalyzeRefresh_HealthyTarget_NoGate confirms the
// gate doesn't fire on healthy targets — a normal analyze run
// against an un-burned operator's repo proceeds as before.
// Catches a future regression where the gate becomes overly broad.
func TestPathB_CLI_AnalyzeRefresh_HealthyTarget_NoGate(t *testing.T) {
	dbPath := newCLITestDB(t)

	// No burns seeded. analyze --refresh on a healthy target must
	// reach the collector dispatch (which then fails for network
	// reasons in the test env, but that's collector-side, not
	// gate-side). Concrete assertion: stderr must NOT contain the
	// gate-refusal phrase.
	r := runCLI(t, dbPath,
		"analyze", "--refresh", "repo:github/healthy-org/healthy-repo",
	)
	assert.NotContains(t, r.stderr, "refusing to collect",
		"healthy-target analyze must NOT trip the gate; stderr=%q", r.stderr)
}

// TestPathB_CLI_AnalyzeNoRefresh_BurnedOwner_DisplayOnly: without
// --refresh, the cached-display path runs and the gate should NOT
// fire — there's no collection to abort. The display still surfaces
// the cascade via Path B's renderer (signatory analyze without
// --refresh on a target that has cached signals shows the cached
// state with the cascade phrase, not "refusing to collect").
//
// For a target with no cached signals at all, the existing "no
// cached data" path is unchanged; the gate doesn't apply.
func TestPathB_CLI_AnalyzeNoRefresh_BurnedOwner_DisplayOnly(t *testing.T) {
	dbPath := newCLITestDB(t)

	add := runCLI(t, dbPath,
		"burn", "add", "identity:github/bufferzonecorp",
		"--reason", "campaign-shaped account",
	)
	require.Equal(t, 0, add.exitCode)

	r := runCLI(t, dbPath,
		"analyze", "repo:github/bufferzonecorp/never-seen-repo",
	)
	// No cached data + no refresh = the existing "no cached data"
	// soft-skip. The gate does NOT fire — that would be a UX
	// regression (the user typed a read-only verb).
	assert.NotContains(t, r.stderr, "refusing to collect",
		"non-refresh analyze must NOT trip the gate — there's no collection to refuse")
}

// TestPathB_CLI_ShowAnalyses_BurnedTarget_BannerFirst pins the
// surfacing contract for `signatory show-analyses` (the verb the
// /analyze skill calls at its Step 0). A target whose owner is
// burned must surface the BURNED banner BEFORE the existing
// listing/absence message — so a human running the command sees
// it as the lede and an LLM consumer reading the captured output
// linearly hits BURNED before deciding what to do next.
//
// Uses seedCascadeScenario (signal-derived cascade): the repo
// entity exists with an owner_profile signal pointing at the
// burned identity. Listing produces "No analyses for X" because
// no analyst outputs are seeded; the test asserts the banner
// appears in stdout above the "No analyses" line.
func TestPathB_CLI_ShowAnalyses_BurnedTarget_BannerFirst(t *testing.T) {
	dbPath := seedCascadeScenario(t)

	r := runCLI(t, dbPath,
		"show-analyses", "repo:github/bufferzonecorp/grpc-client",
	)
	require.Equal(t, 0, r.exitCode,
		"show-analyses must stay exit 0 even with a burn — read-only command, surfacing not refusing; stderr=%q", r.stderr)

	assert.Contains(t, r.stdout, "BURNED",
		"the BURNED banner must appear in stdout when the queried target has a cascade-applicable burn")
	assert.Contains(t, r.stdout, "via publisher identity:github/bufferzonecorp",
		"the cascade form must name the role and owner URI so the user can trace the source")
	assert.Contains(t, r.stdout, "campaign-shaped",
		"the burn reason must surface in the banner")

	// The banner appears BEFORE the existing absence/listing
	// message — that's the lede-first contract.
	burnedIdx := strings.Index(r.stdout, "BURNED")
	noAnalysesIdx := strings.Index(r.stdout, "No analyses")
	require.GreaterOrEqual(t, burnedIdx, 0)
	require.GreaterOrEqual(t, noAnalysesIdx, 0)
	assert.Less(t, burnedIdx, noAnalysesIdx,
		"BURNED banner must precede the analyses listing/absence message in stdout — output ordering is the contract")
}

// TestPathB_CLI_ShowAnalyses_DirectBurn_BannerNoViaOwner pins the
// direct-burn render branch: when the queried target itself has
// the burn (not a cascade), the banner uses the direct phrasing
// — no "via owner ..." clause. Mirrors the equivalent split in
// summary's renderer.
func TestPathB_CLI_ShowAnalyses_DirectBurn_BannerNoViaOwner(t *testing.T) {
	dbPath := newCLITestDB(t)

	// Direct burn on the identity itself; show-analyses on that
	// identity should render direct-form banner.
	add := runCLI(t, dbPath,
		"burn", "add", "identity:github/operator-x",
		"--reason", "test: direct burn render check",
	)
	require.Equal(t, 0, add.exitCode)

	r := runCLI(t, dbPath, "show-analyses", "identity:github/operator-x")
	require.Equal(t, 0, r.exitCode)
	assert.Contains(t, r.stdout, "BURNED",
		"direct burn must produce a banner")
	assert.NotContains(t, r.stdout, "via owner",
		"direct burn render must NOT include the cascade phrase — that branch is reserved for cascaded burns")
	assert.NotContains(t, r.stdout, "via publisher",
		"same — direct burns don't carry a cascade role")
	assert.Contains(t, r.stdout, "direct burn render check",
		"the direct burn reason must surface")
}

// TestPathB_CLI_ShowAnalyses_BrandNewURIByBurnedOperator_Banner
// is the URI-derived cascade case: a target whose canonical URI
// names a burned operator but whose entity row doesn't exist in
// the store yet. The banner must still fire — it's the load-
// bearing path for /analyze Step 0 against a brand-new repo by
// a known-burned operator.
func TestPathB_CLI_ShowAnalyses_BrandNewURIByBurnedOperator_Banner(t *testing.T) {
	dbPath := newCLITestDB(t)

	add := runCLI(t, dbPath,
		"burn", "add", "identity:github/bufferzonecorp",
		"--reason", "campaign-shaped account",
	)
	require.Equal(t, 0, add.exitCode)

	// Note: this repo URI has NO entity row — only the URI structure
	// + the operator burn tell show-analyses it's unsafe.
	r := runCLI(t, dbPath,
		"show-analyses", "repo:github/bufferzonecorp/never-seen-repo",
	)
	require.Equal(t, 0, r.exitCode,
		"show-analyses on a never-ingested target must stay exit 0 — soft absence is the contract")

	assert.Contains(t, r.stdout, "BURNED",
		"URI-derived cascade must fire the banner even when no entity row exists for the queried target")
	assert.Contains(t, r.stdout, "identity:github/bufferzonecorp",
		"the cascade source must be named")
	assert.Contains(t, r.stdout, "No entity matches",
		"the existing absence message must still appear — the banner is additive, not replacing")

	// Banner before the absence message, same lede-first contract.
	burnedIdx := strings.Index(r.stdout, "BURNED")
	noEntityIdx := strings.Index(r.stdout, "No entity matches")
	assert.Less(t, burnedIdx, noEntityIdx,
		"BURNED banner must precede the absence message")
}

// TestPathB_CLI_ShowAnalyses_HealthyTarget_NoBanner is the
// regression guard: a healthy target with no burn anywhere must
// produce no banner. Catches a future change that overzealously
// adds a banner unconditionally.
func TestPathB_CLI_ShowAnalyses_HealthyTarget_NoBanner(t *testing.T) {
	dbPath := newCLITestDB(t)

	r := runCLI(t, dbPath, "show-analyses", "repo:github/healthy-org/healthy-repo")
	require.Equal(t, 0, r.exitCode)
	assert.NotContains(t, r.stdout, "BURNED",
		"healthy target must NOT produce a BURNED banner")
	assert.Contains(t, r.stdout, "No entity matches",
		"the existing absence message stays intact")
}

// TestPathB_CLI_ShowAnalyses_NoTarget_NoBanner pins the
// list-all behaviour: without a target, show-analyses can't
// burn-check a specific entity and must NOT print a banner.
// Per-row burn tagging on the list-all path is deferred (would
// be N+1 store calls for marginal value).
func TestPathB_CLI_ShowAnalyses_NoTarget_NoBanner(t *testing.T) {
	dbPath := newCLITestDB(t)

	// Seed a burn so the store DOES contain a burned entity.
	// The list-all command must still not surface it as a banner —
	// the contract is "banner only when querying a specific target."
	add := runCLI(t, dbPath,
		"burn", "add", "identity:github/some-burned-op",
		"--reason", "test",
	)
	require.Equal(t, 0, add.exitCode)

	r := runCLI(t, dbPath, "show-analyses")
	require.Equal(t, 0, r.exitCode)
	assert.NotContains(t, r.stdout, "BURNED",
		"list-all show-analyses must NOT print a banner — there's no specific target to attribute it to")
	assert.Contains(t, r.stdout, "No analyses in store",
		"list-all behaviour stays unchanged")
}

// TestPathB_CLI_BurnList_ShowsLiteralRowsNotCascaded pins the
// audit-surface contract: burn list must keep showing what's
// LITERALLY in the burns table — one row, on the identity. The
// repo gets its cascade through EffectiveBurn at display time
// elsewhere (summary, analyze), but the audit surface stays
// faithful to the rows. countercampaign.md §7.7.
func TestPathB_CLI_BurnList_ShowsLiteralRowsNotCascaded(t *testing.T) {
	dbPath := seedCascadeScenario(t)

	r := runCLI(t, dbPath, "burn", "list")
	require.Equal(t, 0, r.exitCode)

	assert.Contains(t, r.stdout, "identity:github/bufferzonecorp",
		"burn list must include the literal burn row on the operator")

	// The repo URI must NOT appear in burn list, even though
	// summary on the repo shows it as cascaded-burned. burn list
	// is the audit surface; cascade lives at the display layer.
	assert.NotContains(t, r.stdout, "repo:github/bufferzonecorp/grpc-client",
		"burn list must NOT include the cascaded repo — the audit surface stays faithful to literal table rows; the cascade lives at the display layer")
}
