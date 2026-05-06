package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// setConfirmPrompt swaps the package-level confirmPrompt with the
// supplied stub for the duration of the test. Restores the original
// in t.Cleanup so other tests in the package don't see the override.
func setConfirmPrompt(t *testing.T, fn func(scopeLabel string) bool) {
	t.Helper()
	prev := confirmPrompt
	confirmPrompt = fn
	t.Cleanup(func() { confirmPrompt = prev })
}

// setSelectOpPrompt swaps the package-level selectOpPrompt with a
// stub for the duration of the test. Used by `prune duplicates`
// per-op tests to deterministically drive the y/n/a/q flow without
// touching real stdin.
func setSelectOpPrompt(t *testing.T, fn func(op store.ConsolidationOp) Decision) {
	t.Helper()
	prev := selectOpPrompt
	selectOpPrompt = fn
	t.Cleanup(func() { selectOpPrompt = prev })
}

// newTestOp builds a minimal ConsolidationOp suitable for prompt-
// rendering tests. Uses a merge action by default (the action and
// class strings just flow through to the rendered prompt; tests
// that care about the dispatch use store-layer fixtures, not these).
func newTestOp(sourceURI, canonicalURI string) store.ConsolidationOp {
	return store.ConsolidationOp{
		Action:       store.ConsolidationActionMerge,
		Class:        store.ConsolidationClassCaseFold,
		Source:       store.ConsolidationEntity{ID: "test-id", CanonicalURI: sourceURI},
		CanonicalURI: canonicalURI,
		CanonicalSibling: &store.ConsolidationEntity{
			ID: "sibling-id", CanonicalURI: canonicalURI,
		},
	}
}

// TestPromptSelectOp pins the per-op selection contract for
// `prune duplicates`. Four decisions, mapped from short answers in
// the git-add-p tradition:
//
//	y / yes  → Apply this op, advance
//	n / no   → Skip this op, advance
//	a / all  → Apply this op AND all remaining without further prompting
//	q / quit → Do not apply this op; stop the walk (earlier applied stay applied)
//
// Anything else — including empty input and EOF — collapses to
// DecisionSkip. Same fail-safe principle as promptConfirmation: the
// only way to authorize an op is to type something unambiguously
// affirmative.
func TestPromptSelectOp(t *testing.T) {
	t.Parallel()

	op := newTestOp("repo:github/BurntSushi/toml", "repo:github/burntsushi/toml")

	cases := []struct {
		name  string
		input string
		want  Decision
	}{
		// Apply variants.
		{"lowercase y", "y\n", DecisionApply},
		{"uppercase Y", "Y\n", DecisionApply},
		{"yes spelled out", "yes\n", DecisionApply},
		{"YES uppercase", "YES\n", DecisionApply},
		{"trimmed yes", "  yes  \n", DecisionApply},

		// Skip variants.
		{"lowercase n", "n\n", DecisionSkip},
		{"uppercase N", "N\n", DecisionSkip},
		{"no spelled out", "no\n", DecisionSkip},

		// Apply-all variants.
		{"lowercase a", "a\n", DecisionApplyAllRemaining},
		{"uppercase A", "A\n", DecisionApplyAllRemaining},
		{"all spelled out", "all\n", DecisionApplyAllRemaining},

		// Quit variants.
		{"lowercase q", "q\n", DecisionQuit},
		{"uppercase Q", "Q\n", DecisionQuit},
		{"quit spelled out", "quit\n", DecisionQuit},

		// Anything else collapses to Skip — fail-safe default.
		{"empty (just enter)", "\n", DecisionSkip},
		{"unrelated text", "maybe\n", DecisionSkip},
		{"y-prefix not y/yes", "yolo\n", DecisionSkip},
		{"EOF (no input)", "", DecisionSkip},
		{"whitespace only", "   \n", DecisionSkip},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			got := promptSelectOp(&out, strings.NewReader(tc.input), op)
			assert.Equal(t, tc.want, got,
				"input %q must produce %v", tc.input, tc.want)
			// Prompt always renders the op summary so the operator
			// has the context — they're answering about THIS op,
			// not "the next op" or "the whole batch."
			assert.Contains(t, out.String(), op.Source.CanonicalURI,
				"prompt must echo the op's source URI so the operator can verify what they're answering about")
			assert.Contains(t, out.String(), op.CanonicalURI,
				"prompt must echo the op's canonical URI (the destination) for the same reason")
		})
	}
}

// TestPromptConfirmation pins the answer-parsing contract: only
// case-insensitive "y" or "yes" (with optional surrounding whitespace)
// counts as confirmation. Anything else — empty input, unrelated
// strings, EOF, "no" — falls through to "do not proceed."
//
// The intent is "fail safe": the only way to authorize a destructive
// prune is to type something unambiguously affirmative. A user who
// hits Enter accidentally, or a script feeding empty stdin, gets
// cancellation by default.
func TestPromptConfirmation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"lowercase y", "y\n", true},
		{"uppercase Y", "Y\n", true},
		{"lowercase yes", "yes\n", true},
		{"uppercase YES", "YES\n", true},
		{"mixed case Yes", "Yes\n", true},
		{"trimmed whitespace", "  yes  \n", true},

		// Anything else cancels.
		{"empty (just enter)", "\n", false},
		{"explicit no", "n\n", false},
		{"explicit NO", "NO\n", false},
		{"unrelated text", "maybe\n", false},
		{"starts with y but isn't y/yes", "yolo\n", false},
		{"EOF (no input at all)", "", false},
		{"whitespace only", "   \n", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			got := promptConfirmation(&out, strings.NewReader(tc.input), "test scope")
			assert.Equal(t, tc.want, got,
				"input %q must produce confirmed=%v", tc.input, tc.want)
			assert.Contains(t, out.String(), "test scope",
				"prompt must mention the scope label so the operator can verify the action's target before answering")
		})
	}
}

// pruneAnalystOutput builds a minimally-valid analyst output for
// prune test fixtures. Distinct from ingestSynthesisForAccept's
// helper so the prune tests don't share state with posture accept.
//
// The invokedAt parameter was previously used to differentiate
// content hashes across calls. With the new server-stamped contract
// (validator rejects caller-supplied invoked_at), it's ignored:
// callers should differentiate via the target argument or via other
// fields that participate in the content hash. The parameter is
// kept in the signature so existing call sites compile unchanged.
func pruneAnalystOutput(target, analystID, _ string) *exchange.AnalystOutput {
	lineStart := 1
	return &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: analystID,
			// Model and InvokedAt server-stamped at ingest.
		},
		Target: target,
		Conclusions: []exchange.Conclusion{
			{
				ID: "F001", Verdict: "v", Rationale: "r",
				Severity: exchange.Severity{Default: exchange.SeverityLow},
				Category: "c",
				Citations: []exchange.Citation{
					{Path: "src/x.go", LineStart: &lineStart},
				},
			},
		},
	}
}

// ingestForPrune is a one-liner that commits an analyst_output and
// returns the entity ID for the test to operate against.
func ingestForPrune(t *testing.T, g *Globals, target, analystID, invokedAt string) string {
	t.Helper()
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	res, err := s.IngestAnalystOutput(ctx, pruneAnalystOutput(target, analystID, invokedAt), "test")
	require.NoError(t, err)
	return res.EntityID
}

// TestPruneEntity_DryRunNoWrites: without --yes, the command must
// NOT mutate the store. The plan is printed; the data stays.
// Regression: operator runs `signatory prune entity X` to inspect
// the plan; if that command deletes, they lose data without
// confirming.
func TestPruneEntity_DryRunNoWrites(t *testing.T) {
	g := newTestGlobals(t)
	entityID := ingestForPrune(t, g, "pkg:npm/dry-run-test", "external-sec-v1", "2026-04-23T09:00:00Z")

	cmd := &PruneEntityCmd{
		Target: "pkg:npm/dry-run-test",
		// Yes deliberately omitted.
	}
	require.NoError(t, cmd.Run(g), "dry-run must exit cleanly")

	// Entity must still exist.
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	entity, err := s.FindEntityByURI(ctx, "pkg:npm/dry-run-test")
	require.NoError(t, err, "dry-run must NOT delete the entity")
	assert.Equal(t, entityID, entity.ID)
}

// TestPruneEntity_DestructiveAppliesDelete: with --destructive AND
// the operator confirming via the [y/N] prompt, the entity and
// every child row goes away.
func TestPruneEntity_DestructiveAppliesDelete(t *testing.T) {
	g := newTestGlobals(t)
	_ = ingestForPrune(t, g, "pkg:npm/apply-test", "external-sec-v1", "2026-04-23T10:00:00Z")

	setConfirmPrompt(t, func(_ string) bool { return true })
	cmd := &PruneEntityCmd{
		Target:      "pkg:npm/apply-test",
		Destructive: true,
	}
	require.NoError(t, cmd.Run(g))

	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	_, err = s.FindEntityByURI(ctx, "pkg:npm/apply-test")
	assert.ErrorIs(t, err, store.ErrNotFound,
		"--destructive + confirmed prompt must delete the entity")
}

// TestPruneEntity_DestructiveCancelledByPrompt: --destructive is
// passed but the operator types "n" (or anything non-affirmative)
// at the [y/N] prompt. The entity must remain.
//
// This is the second-lock guarantee — passing --destructive on the
// command line is necessary but NOT sufficient; the interactive
// confirmation is the final gate.
func TestPruneEntity_DestructiveCancelledByPrompt(t *testing.T) {
	g := newTestGlobals(t)
	entityID := ingestForPrune(t, g, "pkg:npm/cancel-test", "external-sec-v1", "2026-04-23T10:30:00Z")

	setConfirmPrompt(t, func(_ string) bool { return false })
	cmd := &PruneEntityCmd{
		Target:      "pkg:npm/cancel-test",
		Destructive: true,
	}
	require.NoError(t, cmd.Run(g),
		"cancel-at-prompt must exit cleanly, not error — the operator chose to bail out")

	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	entity, err := s.FindEntityByURI(ctx, "pkg:npm/cancel-test")
	require.NoError(t, err, "cancelled prune must NOT delete the entity")
	assert.Equal(t, entityID, entity.ID,
		"the entity must be the same row that was seeded — confirmation gate is the second lock past --destructive")
}

// TestPruneEntity_UnknownTarget errors cleanly with a message that
// names the target and hints at recovery. Important for the skill's
// error-surfacing UX — "no entity matches" is a user-fixable state,
// not a crash.
func TestPruneEntity_UnknownTarget(t *testing.T) {
	g := newTestGlobals(t)

	setConfirmPrompt(t, func(_ string) bool { return true })
	cmd := &PruneEntityCmd{Target: "pkg:npm/does-not-exist", Destructive: true}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no entity matches")
	assert.Contains(t, err.Error(), "pkg:npm/does-not-exist",
		"error must name the target so the operator sees which argument failed")
}

// TestPruneEntity_ByUUID: operator can pass an entity UUID
// directly, bypassing the URI parser. Useful for cleanup of rows
// with malformed canonical URIs (if any exist post-v10).
func TestPruneEntity_ByUUID(t *testing.T) {
	g := newTestGlobals(t)
	entityID := ingestForPrune(t, g, "pkg:npm/by-uuid-test", "external-sec-v1", "2026-04-23T11:00:00Z")

	setConfirmPrompt(t, func(_ string) bool { return true })
	cmd := &PruneEntityCmd{Target: entityID, Destructive: true}
	require.NoError(t, cmd.Run(g))

	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	_, err = s.GetEntity(ctx, entityID)
	assert.ErrorIs(t, err, store.ErrNotFound,
		"UUID-targeted prune must delete the entity")
}

// TestPruneVersioned_NoVersionedRows prints a helpful message and
// exits cleanly when the store is already clean. Important: no
// error — "nothing to do" is a valid state, not a failure.
func TestPruneVersioned_NoVersionedRows(t *testing.T) {
	g := newTestGlobals(t)
	// Ingest an unversioned target so the entity table isn't empty.
	_ = ingestForPrune(t, g, "pkg:npm/already-clean", "external-sec-v1", "2026-04-23T12:00:00Z")

	setConfirmPrompt(t, func(_ string) bool { return true })
	cmd := &PruneVersionedCmd{Destructive: true}
	require.NoError(t, cmd.Run(g), "clean store must produce no-op success, not an error")
}

// TestPruneDuplicates_FreshStoreReportsNothingToDo: a canonical-only
// store produces a friendly "store is canonical" message and exits
// without prompting. Critical for the no-op case — operators should
// be able to run `prune duplicates` defensively without it being
// noisy.
func TestPruneDuplicates_FreshStoreReportsNothingToDo(t *testing.T) {
	g := newTestGlobals(t)
	// Seed only canonical entities — no fragmentation.
	_ = ingestForPrune(t, g, "pkg:npm/canonical-only", "external-sec-v1", "2026-04-28T10:00:00Z")

	cmd := &PruneDuplicatesCmd{Destructive: true}
	// confirmPrompt deliberately not stubbed — must not be called
	// when there are zero ops, otherwise the operator sees a prompt
	// they can't usefully answer.
	require.NoError(t, cmd.Run(g),
		"canonical-only store must produce no-op success without prompting")
}

// TestPruneDuplicates_DryRunNoWrites: with no flag (and no confirm
// stub), the command must NOT mutate the store. Mirrors the
// dry-run guarantee of the other prune subcommands.
func TestPruneDuplicates_DryRunNoWrites(t *testing.T) {
	g := newTestGlobals(t)
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)

	// Seed a case-fold fragmentation: stub at mixed-case + canonical
	// at lowercase.
	require.NoError(t, s.PutEntity(ctx, &profile.Entity{
		ID: "stub", CanonicalURI: "repo:github/MixedCase/Repo",
		Type: profile.EntityProject, ShortName: "Repo",
	}))
	require.NoError(t, s.PutEntity(ctx, &profile.Entity{
		ID: "canonical", CanonicalURI: "repo:github/mixedcase/repo",
		Type: profile.EntityProject, ShortName: "repo",
	}))
	s.Close() //nolint:errcheck // test cleanup

	cmd := &PruneDuplicatesCmd{} // Destructive deliberately false.
	require.NoError(t, cmd.Run(g))

	// Both rows must still exist.
	s, err = g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	_, err = s.GetEntity(ctx, "stub")
	require.NoError(t, err, "dry-run must NOT delete the stub entity")
	_, err = s.GetEntity(ctx, "canonical")
	require.NoError(t, err, "dry-run must NOT touch the canonical sibling either")
}

// seedDuplicatesFixture creates a multi-op fragmentation state for
// the per-op interactive tests: three case-fold fragmentations,
// each with its own canonical sibling. Returns the entity IDs in
// op order (matching the canonical_uri sort order
// ListDuplicateFragmentations uses) so tests can assert which
// specific entities did or didn't get merged.
//
// The three pairs are:
//
//	stub-a / canonical-a:  repo:github/Alpha/A    → repo:github/alpha/a
//	stub-b / canonical-b:  repo:github/Bravo/B    → repo:github/bravo/b
//	stub-c / canonical-c:  repo:github/Charlie/C  → repo:github/charlie/c
//
// Source URIs sort alphabetically: Alpha, Bravo, Charlie. So the
// op walk order is deterministic: stub-a first, stub-b second,
// stub-c third.
func seedDuplicatesFixture(t *testing.T, g *Globals) {
	t.Helper()
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	pairs := []struct {
		stubID, canonID, stubURI, canonURI string
	}{
		{"stub-a", "canonical-a", "repo:github/Alpha/A", "repo:github/alpha/a"},
		{"stub-b", "canonical-b", "repo:github/Bravo/B", "repo:github/bravo/b"},
		{"stub-c", "canonical-c", "repo:github/Charlie/C", "repo:github/charlie/c"},
	}
	for _, p := range pairs {
		require.NoError(t, s.PutEntity(ctx, &profile.Entity{
			ID: p.stubID, CanonicalURI: p.stubURI,
			Type: profile.EntityProject, ShortName: "stub",
		}))
		require.NoError(t, s.PutEntity(ctx, &profile.Entity{
			ID: p.canonID, CanonicalURI: p.canonURI,
			Type: profile.EntityProject, ShortName: "canonical",
		}))
	}
}

// queueDecisions returns a selectOpPrompt stub that hands out
// decisions in order — one per call. Tests use it to drive the
// interactive walk deterministically: queueDecisions(Apply, Skip,
// Apply) means "y on op 1, n on op 2, y on op 3."
//
// Calls beyond the queue length return DecisionSkip, mirroring the
// fail-safe default for unknown / EOF input. Tests that assert
// "the walk shouldn't reach op N" use that to catch over-runs.
func queueDecisions(decisions ...Decision) func(store.ConsolidationOp) Decision {
	idx := 0
	return func(_ store.ConsolidationOp) Decision {
		if idx >= len(decisions) {
			return DecisionSkip
		}
		d := decisions[idx]
		idx++
		return d
	}
}

// TestPruneDuplicates_AllApply: y on every op applies every op.
// Three stubs go away, three canonicals survive.
func TestPruneDuplicates_AllApply(t *testing.T) {
	g := newTestGlobals(t)
	seedDuplicatesFixture(t, g)
	setSelectOpPrompt(t, queueDecisions(DecisionApply, DecisionApply, DecisionApply))

	cmd := &PruneDuplicatesCmd{Destructive: true}
	require.NoError(t, cmd.Run(g))

	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	for _, stub := range []string{"stub-a", "stub-b", "stub-c"} {
		_, err := s.GetEntity(ctx, stub)
		assert.ErrorIs(t, err, store.ErrNotFound, "%s must be merged away", stub)
	}
	for _, canonical := range []string{"canonical-a", "canonical-b", "canonical-c"} {
		_, err := s.GetEntity(ctx, canonical)
		require.NoError(t, err, "%s must survive the merge", canonical)
	}
}

// TestPruneDuplicates_AllSkip: n on every op applies nothing. All
// six entities (3 stubs, 3 canonicals) survive untouched.
func TestPruneDuplicates_AllSkip(t *testing.T) {
	g := newTestGlobals(t)
	seedDuplicatesFixture(t, g)
	setSelectOpPrompt(t, queueDecisions(DecisionSkip, DecisionSkip, DecisionSkip))

	cmd := &PruneDuplicatesCmd{Destructive: true}
	require.NoError(t, cmd.Run(g))

	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	// Every entity (stub and canonical) must still exist.
	for _, id := range []string{"stub-a", "canonical-a", "stub-b", "canonical-b", "stub-c", "canonical-c"} {
		_, err := s.GetEntity(ctx, id)
		require.NoError(t, err, "%s must survive — operator skipped every op", id)
	}
}

// TestPruneDuplicates_MixedYesNo: y on op 1, n on op 2, y on op 3.
// Stubs A and C merge away; stub B survives.
func TestPruneDuplicates_MixedYesNo(t *testing.T) {
	g := newTestGlobals(t)
	seedDuplicatesFixture(t, g)
	setSelectOpPrompt(t, queueDecisions(DecisionApply, DecisionSkip, DecisionApply))

	cmd := &PruneDuplicatesCmd{Destructive: true}
	require.NoError(t, cmd.Run(g))

	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	_, err = s.GetEntity(ctx, "stub-a")
	assert.ErrorIs(t, err, store.ErrNotFound, "y'd op 1 must merge stub-a away")
	_, err = s.GetEntity(ctx, "stub-b")
	require.NoError(t, err, "n'd op 2 must leave stub-b untouched")
	_, err = s.GetEntity(ctx, "stub-c")
	assert.ErrorIs(t, err, store.ErrNotFound, "y'd op 3 must merge stub-c away")
}

// TestPruneDuplicates_ApplyAllRemaining_ShortCircuitsPrompt: "a" on
// op 1 applies op 1 AND every remaining op without further prompts.
// The selectOpPrompt stub records calls — assert it was called
// exactly once (only for op 1; ops 2 and 3 short-circuit).
func TestPruneDuplicates_ApplyAllRemaining_ShortCircuitsPrompt(t *testing.T) {
	g := newTestGlobals(t)
	seedDuplicatesFixture(t, g)

	calls := 0
	setSelectOpPrompt(t, func(_ store.ConsolidationOp) Decision {
		calls++
		if calls == 1 {
			return DecisionApplyAllRemaining
		}
		// We should never reach this — "a" on op 1 means ops 2/3
		// auto-apply. If the test gets here, the short-circuit
		// is broken.
		t.Errorf("selectOpPrompt called %d times — must short-circuit after DecisionApplyAllRemaining on op 1", calls)
		return DecisionSkip
	})

	cmd := &PruneDuplicatesCmd{Destructive: true}
	require.NoError(t, cmd.Run(g))

	assert.Equal(t, 1, calls, "selectOpPrompt must be called only for op 1; ops 2/3 short-circuit under DecisionApplyAllRemaining")

	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	for _, stub := range []string{"stub-a", "stub-b", "stub-c"} {
		_, err := s.GetEntity(ctx, stub)
		assert.ErrorIs(t, err, store.ErrNotFound, "%s must be merged — DecisionApplyAllRemaining propagates", stub)
	}
}

// TestPruneDuplicates_QuitMidWalk_KeepsEarlierApplied: y on op 1,
// q on op 2. Op 1 (stub-a) is committed; op 2 (stub-b) is NOT
// applied. Op 3 (stub-c) is also NOT applied because q stops the
// walk. The earlier-applied op stays applied — per-op transaction
// commit was the whole reason for this design.
func TestPruneDuplicates_QuitMidWalk_KeepsEarlierApplied(t *testing.T) {
	g := newTestGlobals(t)
	seedDuplicatesFixture(t, g)
	setSelectOpPrompt(t, queueDecisions(DecisionApply, DecisionQuit))

	cmd := &PruneDuplicatesCmd{Destructive: true}
	require.NoError(t, cmd.Run(g),
		"quit mid-walk must exit cleanly without rolling back already-applied ops")

	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	_, err = s.GetEntity(ctx, "stub-a")
	assert.ErrorIs(t, err, store.ErrNotFound,
		"op 1 was y'd — its merge must be committed and persist past the quit")

	_, err = s.GetEntity(ctx, "stub-b")
	require.NoError(t, err,
		"op 2 was q'd — stub-b must NOT be touched (quit doesn't apply the current op)")
	_, err = s.GetEntity(ctx, "stub-c")
	require.NoError(t, err,
		"op 3 must NOT be touched — q'd before reaching it")
}

// TestPruneDuplicates_QuitFirst_NothingApplied: q on op 1 stops
// the walk before applying anything. Whole store untouched.
func TestPruneDuplicates_QuitFirst_NothingApplied(t *testing.T) {
	g := newTestGlobals(t)
	seedDuplicatesFixture(t, g)
	setSelectOpPrompt(t, queueDecisions(DecisionQuit))

	cmd := &PruneDuplicatesCmd{Destructive: true}
	require.NoError(t, cmd.Run(g))

	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	for _, id := range []string{"stub-a", "canonical-a", "stub-b", "canonical-b", "stub-c", "canonical-c"} {
		_, err := s.GetEntity(ctx, id)
		require.NoError(t, err, "%s must survive — q on op 1 means no ops applied", id)
	}
}

// TestPruneVersioned_DeletesLegacyRows: a pre-v10-shaped versioned
// entity (created manually to simulate dogfood data) gets removed
// by `prune versioned`. Scoped npm packages in the same store are
// preserved.
func TestPruneVersioned_DeletesLegacyRows(t *testing.T) {
	g := newTestGlobals(t)
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)

	// Simulate a pre-v10 versioned entity by injecting one at the
	// store level — v10 ingest would strip @V, so we can't produce
	// this shape via IngestAnalystOutput anymore.
	require.NoError(t, s.PutEntity(ctx, &profile.Entity{
		ID:           "legacy-pkg-1",
		CanonicalURI: "pkg:npm/legacy-versioned@9.9.9",
		Type:         profile.EntityPackage,
		ShortName:    "legacy-versioned@9.9.9",
	}))

	// A scoped npm package co-exists — must NOT be touched.
	require.NoError(t, s.PutEntity(ctx, &profile.Entity{
		ID:           "scoped-1",
		CanonicalURI: "pkg:npm/@types/node",
		Type:         profile.EntityPackage,
		ShortName:    "node",
	}))
	s.Close() //nolint:errcheck // test cleanup

	setConfirmPrompt(t, func(_ string) bool { return true })
	cmd := &PruneVersionedCmd{Destructive: true}
	require.NoError(t, cmd.Run(g))

	s, err = g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	_, err = s.FindEntityByURI(ctx, "pkg:npm/legacy-versioned@9.9.9")
	assert.ErrorIs(t, err, store.ErrNotFound,
		"versioned entity must be pruned")
	_, err = s.FindEntityByURI(ctx, "pkg:npm/@types/node")
	require.NoError(t, err,
		"scoped npm package (non-versioned, scope `@`) must survive `prune versioned`")
}
