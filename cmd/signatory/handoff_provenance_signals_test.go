package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// These tests cover Phase 1 of design/ImproveProvSignals.md — inlining
// Layer-1 signals into the provenance handoff body so the agent cites
// cached values rather than re-deriving them.

// seedProvenanceTestEntity inserts an entity and a representative set
// of signals covering every group the provenance agent would otherwise
// re-derive (the 5 categories from the kong dogfood baseline:
// commit_signing, contributors, go_dependencies, owner_profile,
// last_push + stars). Returns the entity's ID so tests can assert on
// it if needed.
func seedProvenanceTestEntity(t *testing.T, s store.Store, canonicalURI, shortName string) *profile.Entity {
	t.Helper()
	ctx := context.Background()

	entity := &profile.Entity{
		ID:           profile.NewEntityID(),
		CanonicalURI: canonicalURI,
		Type:         profile.EntityProject,
		ShortName:    shortName,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	// Mini-set of signals. Each one covers a group the provenance
	// agent would otherwise burn tokens to re-derive. Values are
	// stylized-but-realistic — enough shape for the test to assert
	// on, not so much that maintenance is a chore.
	seeded := []profile.Signal{
		{
			ID:                entity.ID + "-commit-signing",
			EntityID:          entity.ID,
			Type:              "commit_signing",
			Group:             profile.SignalGroupGovernance,
			Source:            "github",
			ForgeryResistance: profile.ForgeryHigh,
			Value:             json.RawMessage(`{"ratio": 1.0, "signed_count": 10, "total_count": 10}`),
			CollectedAt:       time.Now().UTC(),
			ExpiresAt:         time.Now().Add(24 * time.Hour).UTC(),
		},
		{
			ID:                entity.ID + "-last-push",
			EntityID:          entity.ID,
			Type:              "last_push",
			Group:             profile.SignalGroupVitality,
			Source:            "github",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{"date": "2026-04-17T06:00:30Z"}`),
			CollectedAt:       time.Now().UTC(),
			ExpiresAt:         time.Now().Add(24 * time.Hour).UTC(),
		},
		{
			ID:                entity.ID + "-stars",
			EntityID:          entity.ID,
			Type:              "stars",
			Group:             profile.SignalGroupCriticality,
			Source:            "github",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{"count": 3035}`),
			CollectedAt:       time.Now().UTC(),
			ExpiresAt:         time.Now().Add(24 * time.Hour).UTC(),
		},
		{
			ID:                entity.ID + "-license",
			EntityID:          entity.ID,
			Type:              "license",
			Group:             profile.SignalGroupHygiene,
			Source:            "github",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{"spdx_id": "MIT"}`),
			CollectedAt:       time.Now().UTC(),
			ExpiresAt:         time.Now().Add(24 * time.Hour).UTC(),
		},
		{
			ID:                entity.ID + "-tags",
			EntityID:          entity.ID,
			Type:              "tags",
			Group:             profile.SignalGroupPublication,
			Source:            "github",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{"count": 10, "recent": ["v1.15.0", "v1.14.0"]}`),
			CollectedAt:       time.Now().UTC(),
			ExpiresAt:         time.Now().Add(24 * time.Hour).UTC(),
		},
	}
	require.NoError(t, s.AppendSignals(ctx, seeded))
	return entity
}

// TestHandoff_Provenance_InlinesCachedSignals is the happy-path
// anchor for Phase 1. When the store has cached signals for the
// target, the rendered provenance handoff must contain them —
// grouped under the SignalsSummary keys — so the agent can cite
// directly rather than re-derive.
func TestHandoff_Provenance_InlinesCachedSignals(t *testing.T) {
	g := newTestGlobals(t)
	s, err := g.OpenStore(context.Background())
	require.NoError(t, err)
	seedProvenanceTestEntity(t, s, "repo:github/alecthomas/kong", "alecthomas/kong")
	require.NoError(t, s.Close())

	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:      "provenance",
		Target:    "https://github.com/alecthomas/kong",
		Path:      "/tmp/kong",
		Ecosystem: "go",
		Output:    outPath,
		Quiet:     true,
	}
	require.NoError(t, cmd.Run(g))

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	rendered := string(body)

	// The pre-collected-signals prose section must be present —
	// that's what tells the agent to trust the block.
	assert.Contains(t, rendered, "Pre-collected signals (trust as ground truth)",
		"rendered provenance handoff must carry the signal-block prose")
	assert.Contains(t, rendered, "Do NOT use WebFetch to re-derive",
		"rendered prose must explicitly forbid re-derivation")

	// The envelope shape: collected_for (URI) + signals (per-group).
	assert.Contains(t, rendered, `"collected_for": "repo:github/alecthomas/kong"`,
		"signals envelope must name the URI the cache was looked up under")
	assert.Contains(t, rendered, `"signals":`,
		"signals envelope must include the grouped signal map")

	// Per-group keys populated from the seeded signals.
	assert.Contains(t, rendered, `"governance":`, "governance group present")
	assert.Contains(t, rendered, `"commit_signing"`, "governance.commit_signing present")
	assert.Contains(t, rendered, `"vitality":`, "vitality group present")
	assert.Contains(t, rendered, `"last_push"`, "vitality.last_push present")
	assert.Contains(t, rendered, `"criticality":`, "criticality group present")
	assert.Contains(t, rendered, `"hygiene":`, "hygiene group present")
	assert.Contains(t, rendered, `"publication":`, "publication group present")

	// Actual values must round-trip through the template (not just
	// the group keys). This is the "agent can cite the value" test.
	assert.Contains(t, rendered, `"signed_count": 10`,
		"seeded commit_signing.signed_count must appear in rendered block")
	assert.Contains(t, rendered, `"spdx_id": "MIT"`,
		"seeded hygiene.license must appear in rendered block")

	// The fallback marker must NOT be present when real signals
	// are available.
	assert.NotContains(t, rendered, provenanceSignalsFallback,
		"fallback marker must not appear when signals are present")
}

// TestHandoff_Provenance_FallbackWhenNoCache covers the soft-case
// path: the target has no cached signals. The agent still receives a
// well-formed handoff with a clear fallback marker in place of the
// JSON block, telling it to collect from scratch.
func TestHandoff_Provenance_FallbackWhenNoCache(t *testing.T) {
	g := newTestGlobals(t)

	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:      "provenance",
		Target:    "https://github.com/nonexistent/target",
		Path:      "/tmp/nonexistent",
		Ecosystem: "go",
		Output:    outPath,
		Quiet:     true,
	}
	require.NoError(t, cmd.Run(g))

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	rendered := string(body)

	// Prose still present — that's a property of the template, not
	// the block content.
	assert.Contains(t, rendered, "Pre-collected signals (trust as ground truth)",
		"prose section must render even when the cache is empty")

	// The fallback marker stands in for the JSON block.
	assert.Contains(t, rendered, provenanceSignalsFallback,
		"fallback marker must render when no cached signals exist")

	// No JSON envelope keys leaked — the fallback path shouldn't
	// emit a half-built structure.
	assert.NotContains(t, rendered, `"collected_for":`,
		"no JSON envelope keys when the fallback is in play")
}

// TestHandoff_Provenance_SignalsBlockIsValidJSON rebuilds the
// embedded signal block from the rendered handoff and re-parses it.
// Catches regressions that produce invalid JSON (un-escaped chars,
// truncated output) which would silently corrupt the agent's input.
func TestHandoff_Provenance_SignalsBlockIsValidJSON(t *testing.T) {
	g := newTestGlobals(t)
	s, err := g.OpenStore(context.Background())
	require.NoError(t, err)
	seedProvenanceTestEntity(t, s, "repo:github/foo/bar", "foo/bar")
	require.NoError(t, s.Close())

	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:      "provenance",
		Target:    "https://github.com/foo/bar",
		Path:      "/tmp/bar",
		Ecosystem: "go",
		Output:    outPath,
		Quiet:     true,
	}
	require.NoError(t, cmd.Run(g))

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	rendered := string(body)

	// Locate the fenced JSON block that substituted in for
	// {LAYER_1_SIGNALS}. The template wraps it in ```json … ```.
	start := strings.Index(rendered, "```json\n")
	require.NotEqual(t, -1, start, "rendered handoff must contain a fenced json block")
	start += len("```json\n")
	end := strings.Index(rendered[start:], "\n```")
	require.NotEqual(t, -1, end, "fenced json block must have a closing fence")
	blob := rendered[start : start+end]

	var decoded struct {
		CollectedFor string                 `json:"collected_for"`
		Signals      profile.SignalsSummary `json:"signals"`
	}
	require.NoError(t, json.Unmarshal([]byte(blob), &decoded),
		"fenced block must parse as the envelope type — if this fails, "+
			"the agent would receive a schema-violating handoff")
	assert.Equal(t, "repo:github/foo/bar", decoded.CollectedFor)
	assert.NotNil(t, decoded.Signals.Governance,
		"governance map must survive the round-trip")
}

// TestHandoff_Provenance_UnresolvableTargetFallsBackCleanly covers a
// tertiary failure mode: a target that doesn't resolve to a
// canonical URI at all (bare string, malformed). Handoff should
// emit the fallback text without erroring.
//
// Note: a truly unresolvable target today fails at
// HandoffSubstitutions (TARGET_NAME inference). This test picks a
// target that resolves but has no entity in the store — a cousin of
// the primary fallback test but with a different code path (entity
// lookup-not-found vs. store open-failure vs. target-resolve-failure).
// The "target resolve failure" path is hit when cmd.Target is a
// form ResolveTarget can't parse, and that's tested in combination
// with HandoffSubstitutions elsewhere.
func TestHandoff_Provenance_UnresolvableTargetFallsBackCleanly(t *testing.T) {
	g := newTestGlobals(t)
	// Non-existent entity — same as the primary fallback test but
	// pinned here so future refactors of that test can't drop this
	// shape by accident.
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:      "provenance",
		Target:    "https://github.com/ghost/invented",
		Path:      "/tmp/invented",
		Ecosystem: "npm",
		Output:    outPath,
		Quiet:     true,
	}
	require.NoError(t, cmd.Run(g))

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	assert.Contains(t, string(body), provenanceSignalsFallback)
}

// TestHandoff_Security_NoSignalsBlock confirms that security-role
// handoffs are unaffected by the provenance-signals wiring. The
// security template has no {LAYER_1_SIGNALS} placeholder, so even
// if substitution ran against the security template, nothing
// should appear — but more importantly, the assembleProvenanceSignals
// code path must not fire for security (would open a store the
// security path doesn't need).
func TestHandoff_Security_NoSignalsBlock(t *testing.T) {
	g := newTestGlobals(t)
	// Seed a signal for this target deliberately — if the security
	// path somehow ran the provenance assembly, the signal would
	// show up in the body and the assertion below would fail.
	s, err := g.OpenStore(context.Background())
	require.NoError(t, err)
	seedProvenanceTestEntity(t, s, "repo:github/secure/test", "secure/test")
	require.NoError(t, s.Close())

	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://github.com/secure/test",
		Path:     "/tmp/secure",
		Language: "python",
		Output:   outPath,
		Quiet:    true,
	}
	require.NoError(t, cmd.Run(g))

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	rendered := string(body)

	// None of the provenance-specific signals content should be
	// in the security handoff body.
	assert.NotContains(t, rendered, "Pre-collected signals (trust as ground truth)",
		"security handoff must not carry the provenance signals section")
	assert.NotContains(t, rendered, `"collected_for":`,
		"security handoff must not carry the signals envelope")
}
