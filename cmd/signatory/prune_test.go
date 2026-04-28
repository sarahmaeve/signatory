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
func pruneAnalystOutput(target, analystID, invokedAt string) *exchange.AnalystOutput {
	lineStart := 1
	return &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: analystID,
			Model:     "test-model",
			InvokedAt: invokedAt,
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

// TestPruneDuplicates_DestructiveAppliesMerge: with --destructive
// and a confirmed prompt, the case-fold fragmentation collapses
// into the canonical row. End state: one entity at the canonical
// URI; the stub is gone.
func TestPruneDuplicates_DestructiveAppliesMerge(t *testing.T) {
	g := newTestGlobals(t)
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)

	require.NoError(t, s.PutEntity(ctx, &profile.Entity{
		ID: "stub", CanonicalURI: "repo:github/MixedCase/Repo",
		Type: profile.EntityProject, ShortName: "Repo",
	}))
	require.NoError(t, s.PutEntity(ctx, &profile.Entity{
		ID: "canonical", CanonicalURI: "repo:github/mixedcase/repo",
		Type: profile.EntityProject, ShortName: "repo",
	}))
	s.Close() //nolint:errcheck // test cleanup

	setConfirmPrompt(t, func(_ string) bool { return true })
	cmd := &PruneDuplicatesCmd{Destructive: true}
	require.NoError(t, cmd.Run(g))

	s, err = g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	_, err = s.GetEntity(ctx, "stub")
	assert.ErrorIs(t, err, store.ErrNotFound, "merge applied — stub must be gone")
	canonical, err := s.GetEntity(ctx, "canonical")
	require.NoError(t, err)
	assert.Equal(t, "repo:github/mixedcase/repo", canonical.CanonicalURI)
}

// TestPruneDuplicates_DestructiveCancelledByPrompt: the second-lock
// guarantee — even with --destructive, "n" at the prompt leaves the
// store untouched. Same shape as the case for `prune entity`.
func TestPruneDuplicates_DestructiveCancelledByPrompt(t *testing.T) {
	g := newTestGlobals(t)
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)

	require.NoError(t, s.PutEntity(ctx, &profile.Entity{
		ID: "stub", CanonicalURI: "repo:github/MixedCase/Repo",
		Type: profile.EntityProject, ShortName: "Repo",
	}))
	require.NoError(t, s.PutEntity(ctx, &profile.Entity{
		ID: "canonical", CanonicalURI: "repo:github/mixedcase/repo",
		Type: profile.EntityProject, ShortName: "repo",
	}))
	s.Close() //nolint:errcheck // test cleanup

	setConfirmPrompt(t, func(_ string) bool { return false })
	cmd := &PruneDuplicatesCmd{Destructive: true}
	require.NoError(t, cmd.Run(g),
		"cancel-at-prompt must exit cleanly, not error")

	s, err = g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	_, err = s.GetEntity(ctx, "stub")
	require.NoError(t, err,
		"cancelled consolidation must NOT delete the stub")
	_, err = s.GetEntity(ctx, "canonical")
	require.NoError(t, err)
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
