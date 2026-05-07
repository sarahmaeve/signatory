package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRunMethods_PropagateGlobalsContext pins the contract documented
// in main.go's Globals.Context comment: every Run method that accepts
// *Globals must thread globals.Context through to its store/network
// work, falling back to context.Background() only when the field is
// nil (test path).
//
// Pre-cancelling globals.Context and asserting the call surfaces
// errors.Is(..., context.Canceled) is a sentinel for "the wired SIGINT
// context actually reaches the store layer." Before the 2026-05-02
// fix, these Run methods hardcoded `ctx := context.Background()` and
// silently dropped globals.Context — Ctrl-C at the CLI did not
// propagate to in-flight DB work.
//
// Each subtest is parallel-safe: testGlobals() allocates a fresh
// SQLite path under t.TempDir(), and the cancelled context means we
// never actually open the DB.
func TestRunMethods_PropagateGlobalsContext(t *testing.T) {
	t.Parallel()

	// preCancelledGlobals returns a *Globals with Context already in
	// the cancelled state. The expected behavior across every Run
	// method tested here: the store-opening (or first ctx-using)
	// call observes cancellation and returns errors.Is(..., Canceled).
	preCancelledGlobals := func(t *testing.T) *Globals {
		t.Helper()
		g := testGlobals(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		g.Context = ctx
		return g
	}

	t.Run("ShowAnalysesCmd", func(t *testing.T) {
		t.Parallel()
		err := (&ShowAnalysesCmd{}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	t.Run("ShowConclusionsCmd", func(t *testing.T) {
		t.Parallel()
		err := (&ShowConclusionsCmd{}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	t.Run("ShowMethodologyCmd", func(t *testing.T) {
		t.Parallel()
		err := (&ShowMethodologyCmd{}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	t.Run("SummaryCmd", func(t *testing.T) {
		t.Parallel()
		// Target is required by SummaryCmd.Run before OpenStore;
		// supply a syntactically-valid one so the ctx-using call
		// is reached.
		err := (&SummaryCmd{Target: "pkg:npm/anything"}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	t.Run("PostureGetCmd", func(t *testing.T) {
		t.Parallel()
		err := (&PostureGetCmd{Target: "pkg:npm/anything"}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	t.Run("PostureListCmd", func(t *testing.T) {
		t.Parallel()
		err := (&PostureListCmd{}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	t.Run("PostureSetCmd", func(t *testing.T) {
		t.Parallel()
		err := (&PostureSetCmd{
			Target:    "pkg:npm/anything",
			Tier:      "trusted-for-now",
			Rationale: "ctx propagation test",
			Version:   "1.0.0",
		}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	t.Run("PostureUnsetCmd", func(t *testing.T) {
		t.Parallel()
		err := (&PostureUnsetCmd{
			Target: "pkg:npm/anything",
			Reason: "ctx propagation test",
		}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	t.Run("PostureAcceptCmd", func(t *testing.T) {
		t.Parallel()
		err := (&PostureAcceptCmd{
			OutputID: "00000000-0000-0000-0000-000000000000",
			Yes:      true,
		}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	// HandoffCmd's Run inlines context.Background() at multiple call
	// sites (applyNetworkPrecheck, applyClone, validateAnalysisSession,
	// assembleSynthesisEvidence) plus depositRendered constructs
	// Background internally. Reaching validateAnalysisSession requires
	// AnalysisSessionID set + a resolvable Target + a role with a known
	// template; that path terminates in OpenStore which observes a
	// pre-cancelled ctx.
	t.Run("HandoffCmd_ValidateAnalysisSession", func(t *testing.T) {
		t.Parallel()
		err := (&HandoffCmd{
			Target:            "pkg:npm/express",
			Role:              "security",
			AnalysisSessionID: "00000000-0000-0000-0000-000000000000",
		}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	// --- Prune commands (destructive verbs — highest priority) ---

	t.Run("PruneEntityCmd", func(t *testing.T) {
		t.Parallel()
		err := (&PruneEntityCmd{Target: "pkg:npm/anything"}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	t.Run("PruneVersionedCmd", func(t *testing.T) {
		t.Parallel()
		err := (&PruneVersionedCmd{}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	t.Run("PruneOrphansCmd", func(t *testing.T) {
		t.Parallel()
		err := (&PruneOrphansCmd{}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	t.Run("PruneDuplicatesCmd", func(t *testing.T) {
		t.Parallel()
		err := (&PruneDuplicatesCmd{}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	// --- Burn commands ---

	t.Run("BurnAddCmd", func(t *testing.T) {
		t.Parallel()
		err := (&BurnAddCmd{
			Target: "pkg:npm/anything",
			Reason: "ctx propagation test",
		}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	t.Run("BurnRemoveCmd", func(t *testing.T) {
		t.Parallel()
		err := (&BurnRemoveCmd{
			Target: "pkg:npm/anything",
			Reason: "ctx propagation test",
		}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	t.Run("BurnListCmd", func(t *testing.T) {
		t.Parallel()
		err := (&BurnListCmd{}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	// --- Show commands ---

	t.Run("ShowSynthesisCmd", func(t *testing.T) {
		t.Parallel()
		err := (&ShowSynthesisCmd{
			OutputID: "00000000-0000-0000-0000-000000000000",
		}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	// --- Analysis session commands ---

	t.Run("AnalysisBeginCmd", func(t *testing.T) {
		t.Parallel()
		err := (&AnalysisBeginCmd{
			Target: "pkg:npm/anything",
		}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	t.Run("AnalysisEndCmd", func(t *testing.T) {
		t.Parallel()
		err := (&AnalysisEndCmd{
			SessionID: "00000000-0000-0000-0000-000000000000",
			Status:    "failed",
		}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	t.Run("AnalysisListCmd", func(t *testing.T) {
		t.Parallel()
		err := (&AnalysisListCmd{}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	t.Run("AnalysisShowCmd", func(t *testing.T) {
		t.Parallel()
		err := (&AnalysisShowCmd{
			SessionID: "00000000-0000-0000-0000-000000000000",
		}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	t.Run("AnalysisTimingCmd", func(t *testing.T) {
		t.Parallel()
		err := (&AnalysisTimingCmd{
			SessionID: "00000000-0000-0000-0000-000000000000",
		}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	// --- Ingest (needs a valid file to pass pre-store validation) ---

	t.Run("IngestCmd", func(t *testing.T) {
		t.Parallel()
		path := writeTempFile(t, "valid.json", minimalValidJSON)
		err := (&IngestCmd{File: path, Format: "json", Quiet: true}).Run(preCancelledGlobals(t))
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})

	// --- Survey (needs a manifest path to skip auto-detect) ---

	t.Run("SurveyCmd", func(t *testing.T) {
		t.Parallel()
		g := preCancelledGlobals(t)
		// Write a minimal go.mod so manifest parsing succeeds before
		// the store-open call observes the cancelled context.
		dir := t.TempDir()
		gomod := filepath.Join(dir, "go.mod")
		os.WriteFile(gomod, []byte("module test\n\ngo 1.24\n"), 0o644)
		err := (&SurveyCmd{Manifest: gomod}).Run(g)
		require.Error(t, err)
		require.Truef(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in error chain, got: %v", err)
	})
}
