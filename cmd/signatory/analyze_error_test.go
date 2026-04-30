package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	npmregistry "github.com/sarahmaeve/signatory/internal/signal/registry/npm"
	"github.com/sarahmaeve/signatory/internal/store"
)

// failingNpmSrv returns an httptest server that always replies 500 for
// registry calls, simulating a transient npm registry failure.
func failingNpmSrv() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"registry unavailable"}`)
	}))
}

// TestFunctional_AnalyzeRefresh_NpmFailurePropagates_Error (Test A)
// verifies that when the npm registry call fails during --refresh, Run
// returns a non-nil error whose message names the npm failure.
// Prior to the fix, the error was demoted to a stderr warning and Run
// returned nil — silently degrading the analysis.
func TestFunctional_AnalyzeRefresh_NpmFailurePropagates_Error(t *testing.T) {
	npmSrv := failingNpmSrv()
	defer npmSrv.Close()

	npmCollector := npmregistry.NewCollectorWithClient(
		npmregistry.NewClientWithBaseURL(npmSrv.URL))

	dir := t.TempDir()
	globals := &Globals{
		DBPath:         filepath.Join(dir, "test.db"),
		Collectors:     []signal.Collector{npmCollector},
		AuditFilePath:  filepath.Join(dir, "audit.log"),
		NpmRegistryURL: npmSrv.URL,
	}

	cmd := &AnalyzeCmd{Target: "pkg:npm/express", Refresh: true}
	err := cmd.Run(globals)

	// Test A: --refresh + npm failure must return an error, not nil.
	require.Error(t, err, "npm registry failure during --refresh must propagate as an error")
	assert.Contains(t, err.Error(), "npm",
		"error message must name the npm failure")
}

// TestFunctional_AnalyzeRefresh_NpmFailurePropagates_AbsenceSignal (Test B)
// verifies that when the npm registry call fails, an absence:repo_declaration
// signal with retryable=true is written to the store BEFORE the error return.
// This gives the profile a machine-readable marker distinguishing
// "tried and registry failed" from "tried and got no declared repo."
func TestFunctional_AnalyzeRefresh_NpmFailurePropagates_AbsenceSignal(t *testing.T) {
	npmSrv := failingNpmSrv()
	defer npmSrv.Close()

	npmCollector := npmregistry.NewCollectorWithClient(
		npmregistry.NewClientWithBaseURL(npmSrv.URL))

	dir := t.TempDir()
	globals := &Globals{
		DBPath:         filepath.Join(dir, "test.db"),
		Collectors:     []signal.Collector{npmCollector},
		AuditFilePath:  filepath.Join(dir, "audit.log"),
		NpmRegistryURL: npmSrv.URL,
	}

	cmd := &AnalyzeCmd{Target: "pkg:npm/express", Refresh: true}
	err := cmd.Run(globals)

	// Test B: error is expected (covered by Test A); now check the store.
	require.Error(t, err, "expected error from npm failure")

	// Open the store and look for the absence signal.
	s, openErr := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, openErr)
	defer s.Close()

	entity, findErr := s.FindEntityByURI(context.Background(), "pkg:npm/express")
	require.NoError(t, findErr, "entity must exist even when npm resolution fails")

	signals, sigErr := s.GetSignals(context.Background(), entity.ID)
	require.NoError(t, sigErr)

	var absenceSig *profile.Signal
	for i := range signals {
		if signals[i].Type == "absence:repo_declaration" {
			absenceSig = &signals[i]
			break
		}
	}
	require.NotNil(t, absenceSig,
		"absence:repo_declaration signal must be written before the error return; got signals: %v",
		func() []string {
			var types []string
			for _, s := range signals {
				types = append(types, s.Type)
			}
			return types
		}())

	// Decode the value and assert retryable=true.
	var val map[string]any
	require.NoError(t, json.Unmarshal(absenceSig.Value, &val))
	retryable, ok := val["retryable"].(bool)
	assert.True(t, ok && retryable,
		"absence:repo_declaration signal must carry retryable=true in its metadata; got val=%v", val)
}

// TestAnalyze_CorruptedEntityErrorNotSwallowed verifies that a
// non-ErrNotFound error from FindEntityByURI is surfaced to the user,
// not silently ignored. (Review 3, C1)
func TestAnalyze_CorruptedEntityErrorNotSwallowed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)

	// Insert an entity with a corrupted timestamp so the entity read
	// will return a parse error (not ErrNotFound).
	_, err = s.DB().ExecContext(context.Background(),
		`INSERT INTO entities (id, canonical_uri, type, short_name, description, ecosystem, url, created_at, updated_at)
		 VALUES ('corrupt-id', 'repo:github/corrupt/corrupt', 'project', 'corrupt/corrupt', '', '', 'https://github.com/corrupt/corrupt', 'INVALID', '2026-01-01T00:00:00Z')`)
	require.NoError(t, err)
	s.Close()

	// Now try to analyze — the error should propagate, not be swallowed.
	mock := &mockCollector{
		name: "mock",
		signals: []profile.Signal{{
			ID:                "mock-sig",
			Type:              "stars",
			Group:             profile.SignalGroupCriticality,
			Source:            "mock",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{"count":1}`),
			CollectedAt:       time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		}},
	}
	globals := &Globals{
		DBPath:        dbPath,
		Collectors:    []signal.Collector{mock},
		AuditFilePath: filepath.Join(t.TempDir(), "audit.log"),
	}

	cmd := &AnalyzeCmd{Target: "corrupt/corrupt", Refresh: true}
	err = cmd.Run(globals)

	// The error should NOT be nil — the corrupted timestamp should
	// be surfaced, not silently ignored.
	assert.Error(t, err, "corrupted entity read should produce an error, not be silently swallowed")
}

// TestAnalyze_DisplayProfileErrorNotSwallowed verifies that corrupted
// posture timestamps are surfaced when displaying a profile.
// (Review 3, H4)
func TestAnalyze_DisplayProfileErrorNotSwallowed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)

	ctx := context.Background()

	// Insert a valid entity with v2 schema.
	entity := &profile.Entity{
		ID:           "display-test",
		CanonicalURI: "repo:github/display/test",
		Type:         profile.EntityProject,
		ShortName:    "display/test",
		URL:          "https://github.com/display/test",
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	// Insert a signal so there's cached data.
	require.NoError(t, s.AppendSignals(ctx, []profile.Signal{{
		ID: "sig-1", EntityID: "display-test", Type: "stars",
		Group: profile.SignalGroupCriticality, Source: "test",
		ForgeryResistance: profile.ForgeryHigh,
		Value:             json.RawMessage(`{}`),
		CollectedAt:       time.Now().UTC(),
		ExpiresAt:         time.Now().UTC().Add(time.Hour),
	}}))

	// Insert a posture with corrupted timestamp via raw SQL.
	_, err = s.DB().ExecContext(ctx,
		`INSERT INTO postures (entity_id, version, tier, rationale, set_by, set_at)
		 VALUES ('display-test', '', 'trusted-for-now', 'test', 'team:sarah', 'CORRUPTED')`)
	require.NoError(t, err)
	s.Close()

	// Analyze without refresh — should read from cache and hit the
	// corrupted posture.
	globals := &Globals{
		DBPath:        dbPath,
		AuditFilePath: filepath.Join(t.TempDir(), "audit.log"),
	}
	cmd := &AnalyzeCmd{Target: "display/test", Refresh: false}
	err = cmd.Run(globals)

	assert.Error(t, err, "corrupted posture timestamp should produce an error, not be silently ignored")
}

// ----- PyPI repo resolution -----
//
// Mirrors the npm tests above: --refresh on a pkg:pypi/<name>
// target triggers PyPI project-URL resolution; success stamps
// entity.URL; failure writes an absence:repo_declaration signal
// and (under --refresh) returns an error to the caller.
//
// PyPI's metadata response shape uses info.project_urls (a free-
// form key->URL map) plus a deprecated info.home_page. The
// resolver in internal/signal/registry/pypi/resolve.go walks
// project_urls in priority order then falls back to home_page;
// we don't re-test those rules here (covered in the pypi
// package's own tests). These tests verify the analyze.go wiring.

// pypiSrvSucceeding returns an httptest server that serves a
// minimal PyPI project metadata response with a github
// Repository URL. The URL the client will hit is
// /pypi/<name>/json — we pin the package name to "idna" to
// match the actual target the test uses.
func pypiSrvSucceeding(repoURL string) *httptest.Server {
	body := fmt.Sprintf(`{
		"info": {
			"name": "idna",
			"home_page": "",
			"project_urls": {
				"Repository": %q,
				"Documentation": "https://idna.readthedocs.io/"
			}
		}
	}`, repoURL)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
}

// failingPypiSrv always returns 500. Used for the
// transient-failure path where --refresh must propagate the error.
func failingPypiSrv() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"registry unavailable"}`)
	}))
}

// TestFunctional_AnalyzeRefresh_PyPIRepoResolution_Success
// verifies the happy path: --refresh on a pkg:pypi/ target
// hits the PyPI registry, resolves project_urls.Repository,
// and stamps the entity's URL with the github clone URL.
//
// Without this wiring, the entity's URL stays empty, the
// downstream github + git collectors are skipped, and the
// analysts see an empty signal store (the gap the ms dogfood
// audit surfaced).
func TestFunctional_AnalyzeRefresh_PyPIRepoResolution_Success(t *testing.T) {
	pypiSrv := pypiSrvSucceeding("https://github.com/kjd/idna")
	defer pypiSrv.Close()

	dir := t.TempDir()
	// Inject a mock collector so collectorsFor is bypassed — without
	// it, the resolved entity.URL would trigger the clone-required
	// check (entity is now github-hosted) and the test would fail
	// for an unrelated reason. The mock just returns a single signal
	// so the loop runs to completion.
	globals := &Globals{
		DBPath:          filepath.Join(dir, "test.db"),
		Collectors:      []signal.Collector{newMockCollector()},
		AuditFilePath:   filepath.Join(dir, "audit.log"),
		PypiRegistryURL: pypiSrv.URL,
	}

	cmd := &AnalyzeCmd{Target: "pkg:pypi/idna", Refresh: true}
	err := cmd.Run(globals)
	require.NoError(t, err, "happy-path PyPI resolution should not error: %v", err)

	// Verify the entity's URL got stamped.
	s, openErr := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, openErr)
	defer s.Close() //nolint:errcheck

	entity, findErr := s.FindEntityByURI(context.Background(), "pkg:pypi/idna")
	require.NoError(t, findErr)
	assert.Equal(t, "https://github.com/kjd/idna", entity.URL,
		"PyPI repo resolution must stamp the github URL onto entity.URL so downstream github+git collectors see it")
}

// TestFunctional_AnalyzeRefresh_PyPIFailurePropagates_Error
// verifies that a PyPI 5xx during --refresh returns an error
// (parallel to the npm contract). Pre-fix, no PyPI resolution
// happened at all so this code path didn't exist.
func TestFunctional_AnalyzeRefresh_PyPIFailurePropagates_Error(t *testing.T) {
	pypiSrv := failingPypiSrv()
	defer pypiSrv.Close()

	dir := t.TempDir()
	globals := &Globals{
		DBPath:          filepath.Join(dir, "test.db"),
		Collectors:      []signal.Collector{newMockCollector()},
		AuditFilePath:   filepath.Join(dir, "audit.log"),
		PypiRegistryURL: pypiSrv.URL,
	}

	cmd := &AnalyzeCmd{Target: "pkg:pypi/idna", Refresh: true}
	err := cmd.Run(globals)
	require.Error(t, err, "PyPI registry failure during --refresh must propagate as an error")
	assert.Contains(t, err.Error(), "pypi",
		"error message must name the pypi failure")
}

// ----- ensureEntity (posture.go) sets Ecosystem on creation -----
//
// Pre-fix bug discovered 2026-04-28 (idna refresh meltdown):
// `ensureEntity` constructed entities without setting the
// Ecosystem field, so a `signatory analysis begin pkg:pypi/idna`
// (which calls ensureEntity to create the stub row) produced an
// entity with ecosystem=''. Subsequent `signatory analyze
// --refresh` saw the existing entity, skipped its create-block
// (which DOES set Ecosystem), and the resolver guards
// `entity.Ecosystem == "pypi"` failed → resolvePyPIRepo never
// ran → 0 signals collected.
//
// The same bug affected pkg:npm/ targets identically. The fix
// is one line in ensureEntity, but the absence of these tests
// is what let the regression land.

func TestEnsureEntity_SetsEcosystem_PkgPypi(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	entity, err := ensureEntity(t.Context(), s, "pkg:pypi/idna")
	require.NoError(t, err)
	require.NotNil(t, entity)
	assert.Equal(t, "pypi", entity.Ecosystem,
		"ensureEntity must stamp Ecosystem on pkg:pypi/ creation; without it, downstream resolvePyPIRepo's guard fails and the entity stays unresolvable")

	// Re-read to confirm the value persisted, not just lived in
	// the returned struct.
	reread, err := s.FindEntityByURI(t.Context(), "pkg:pypi/idna")
	require.NoError(t, err)
	assert.Equal(t, "pypi", reread.Ecosystem,
		"persisted Ecosystem must match — store row, not just in-memory struct")
}

func TestEnsureEntity_SetsEcosystem_PkgNpm(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	entity, err := ensureEntity(t.Context(), s, "pkg:npm/ms")
	require.NoError(t, err)
	assert.Equal(t, "npm", entity.Ecosystem,
		"ensureEntity must stamp Ecosystem on pkg:npm/ creation; same bug shape as pypi above")
}

func TestEnsureEntity_RepoScheme_EcosystemEmpty(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	entity, err := ensureEntity(t.Context(), s, "github.com/dustin/go-humanize")
	require.NoError(t, err)
	// repo: scheme entities have no ecosystem in the v0.1 model;
	// the field stays empty. This test pins that expectation so a
	// future "fill ecosystem for repos too" change is a deliberate
	// decision, not an accident.
	assert.Equal(t, "", entity.Ecosystem,
		"repo: scheme entities have no ecosystem — empty is correct")
}

// TestFunctional_AnalyzeRefresh_BackfillsEcosystemOnStaleEntity:
// the defensive companion to the ensureEntity fix. Entities
// created before the fix have Ecosystem=”. When `signatory
// analyze --refresh` finds such an entity, it backfills
// Ecosystem from the canonical URI before the resolver guards
// run, so resolvePyPIRepo can fire on the existing row instead
// of staying silent. Persists the backfilled value so subsequent
// reads see it.
//
// This test would FAIL without both fixes: the entity stub is
// pre-created with empty Ecosystem (mimicking the post-bug
// state), and only the backfill makes the PyPI resolver match
// its guard. Verifies the migration path for stale data without
// requiring users to prune-and-rerun.
func TestFunctional_AnalyzeRefresh_BackfillsEcosystemOnStaleEntity(t *testing.T) {
	pypiSrv := pypiSrvSucceeding("https://github.com/kjd/idna")
	defer pypiSrv.Close()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Pre-create the entity with empty Ecosystem to mimic the
	// pre-fix state: entity exists in store from an earlier
	// `signatory analysis begin` run that omitted the field.
	{
		s, err := store.OpenSQLite(t.Context(), dbPath)
		require.NoError(t, err)
		stale := &profile.Entity{
			ID:           profile.NewEntityID(),
			CanonicalURI: "pkg:pypi/idna",
			Type:         profile.EntityPackage,
			ShortName:    "idna",
			Ecosystem:    "", // ← THE BUG STATE
			URL:          "",
			CreatedAt:    time.Now().UTC(),
			UpdatedAt:    time.Now().UTC(),
		}
		require.NoError(t, s.PutEntity(t.Context(), stale))
		require.NoError(t, s.Close())
	}

	globals := &Globals{
		DBPath:          dbPath,
		Collectors:      []signal.Collector{newMockCollector()},
		AuditFilePath:   filepath.Join(dir, "audit.log"),
		PypiRegistryURL: pypiSrv.URL,
	}
	cmd := &AnalyzeCmd{Target: "pkg:pypi/idna", Refresh: true}
	err := cmd.Run(globals)
	require.NoError(t, err, "analyze --refresh against a stale entity should backfill Ecosystem and proceed: %v", err)

	// Re-read and verify both fields are now correct.
	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	entity, err := s.FindEntityByURI(t.Context(), "pkg:pypi/idna")
	require.NoError(t, err)
	assert.Equal(t, "pypi", entity.Ecosystem,
		"empty Ecosystem on a stale pkg:pypi/ entity must be backfilled by analyze --refresh so the next run's resolver guards match")
	assert.Equal(t, "https://github.com/kjd/idna", entity.URL,
		"after Ecosystem backfill, resolvePyPIRepo must fire and stamp the github URL — proves the chain works end-to-end")
}

// TestFunctional_AnalyzeRefresh_BackfillsURLOnStaleGoEntity is the
// defensive companion to the pkg-case CloneURL wiring (target.go's
// derivedGoCloneURL + analyze.go's `entity.URL = resolved.CloneURL`
// in the create branch). Entities created before that wiring have
// empty URL even when the canonical URI encodes an algorithmic github
// source (pkg:golang/github.com/X, golang.org/x/Y). On --refresh,
// the orchestrator must backfill URL from the resolved CloneURL —
// without it, isGitHostedEntity stays false on the stale row and the
// github + git + repofiles + openssf collectors silently skip the
// next run too. Reproduces the dogfood symptom on
// `signatory analyze --clone --refresh pkg:golang/github.com/alecthomas/kong`
// where a stale entity from a 7-day-old security analysis prevented
// the clone-side dispatch from firing.
//
// Mirrors TestFunctional_AnalyzeRefresh_BackfillsEcosystemOnStaleEntity:
// pre-create a stale row, run --refresh, assert URL is populated.
// Mock collectors keep the test offline.
func TestFunctional_AnalyzeRefresh_BackfillsURLOnStaleGoEntity(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Pre-create the entity with empty URL to mimic the pre-fix
	// state: entity exists from an earlier analysis that ran before
	// CloneURL wiring landed. The Ecosystem is set (this isn't the
	// Ecosystem-backfill scenario); only URL is missing.
	{
		s, err := store.OpenSQLite(t.Context(), dbPath)
		require.NoError(t, err)
		stale := &profile.Entity{
			ID:           profile.NewEntityID(),
			CanonicalURI: "pkg:golang/github.com/alecthomas/kong",
			Type:         profile.EntityPackage,
			ShortName:    "github.com/alecthomas/kong",
			Ecosystem:    "golang",
			URL:          "", // ← THE BUG STATE
			CreatedAt:    time.Now().UTC(),
			UpdatedAt:    time.Now().UTC(),
		}
		require.NoError(t, s.PutEntity(t.Context(), stale))
		require.NoError(t, s.Close())
	}

	globals := &Globals{
		DBPath:        dbPath,
		Collectors:    []signal.Collector{newMockCollector()},
		AuditFilePath: filepath.Join(dir, "audit.log"),
	}
	cmd := &AnalyzeCmd{Target: "pkg:golang/github.com/alecthomas/kong", Refresh: true}
	require.NoError(t, cmd.Run(globals),
		"analyze --refresh against a stale Go entity must backfill URL and proceed")

	// Re-read and verify URL is now populated.
	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	entity, err := s.FindEntityByURI(t.Context(), "pkg:golang/github.com/alecthomas/kong")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/alecthomas/kong", entity.URL,
		"empty URL on a stale pkg:golang/github.com/* entity must be backfilled by analyze --refresh so isGitHostedEntity returns true on the next dispatch")
}

// ----- Gap 6c: stderr hint for Go modules analyzed via repo: form -----
//
// The dogfood audit identified a coverage gap: when a Go module
// is analyzed via its github URL form (repo:github/X/Y), the
// gopublish collector's filter (Ecosystem in {"golang","go"})
// doesn't match — repo: entities have no Ecosystem field set.
// Result: the user gets github+git signals but not Go-publish
// provenance (proxy.golang.org / sum.golang.org).
//
// The full fix is URI canonicalization (rewrite repo:github/X/Y
// → pkg:golang/<modpath> at resolve time when the repo is a Go
// module), but that's network-dependent resolver work. For now,
// a stderr hint nudges users toward the canonical form when we
// detect the situation post-clone.

// TestWarnGoModuleViaRepoForm_EmitsHint: the cloned repo carries
// a go.mod, the entity is repo:github/... — the hint MUST land.
// Module path from go.mod appears in the suggested
// pkg:golang/<modpath> form so the user can copy-paste.
func TestWarnGoModuleViaRepoForm_EmitsHint(t *testing.T) {
	clone := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(clone, "go.mod"),
		[]byte("module github.com/dustin/go-humanize\n\ngo 1.21\n"),
		0o600,
	))

	entity := &profile.Entity{
		CanonicalURI: "repo:github/dustin/go-humanize",
		Type:         profile.EntityProject,
	}

	var buf bytes.Buffer
	maybeWarnGoModuleViaRepoForm(&buf, entity, clone)

	got := buf.String()
	assert.Contains(t, got, "github.com/dustin/go-humanize",
		"hint must name the module path so the user can copy it")
	assert.Contains(t, got, "pkg:golang/github.com/dustin/go-humanize",
		"hint must give the canonical pkg:golang/<modpath> form to switch to")
}

// TestWarnGoModuleViaRepoForm_NoGoMod_NoHint: clone has no go.mod
// (Python project, Rust project, plain repo). Hint must NOT
// fire — we only nudge when the situation actually applies.
func TestWarnGoModuleViaRepoForm_NoGoMod_NoHint(t *testing.T) {
	clone := t.TempDir() // empty directory, no go.mod
	entity := &profile.Entity{
		CanonicalURI: "repo:github/foo/bar",
		Type:         profile.EntityProject,
	}

	var buf bytes.Buffer
	maybeWarnGoModuleViaRepoForm(&buf, entity, clone)

	assert.Empty(t, buf.String(),
		"non-Go projects must not emit the hint — no go.mod, no nudge")
}

// TestWarnGoModuleViaRepoForm_PkgGolang_NoHint: entity already
// uses the canonical pkg:golang/ form. Hint must NOT fire — the
// user is already doing the right thing.
func TestWarnGoModuleViaRepoForm_PkgGolang_NoHint(t *testing.T) {
	clone := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(clone, "go.mod"),
		[]byte("module github.com/dustin/go-humanize\n"),
		0o600,
	))

	entity := &profile.Entity{
		CanonicalURI: "pkg:golang/github.com/dustin/go-humanize",
		Type:         profile.EntityPackage,
		Ecosystem:    "golang",
	}

	var buf bytes.Buffer
	maybeWarnGoModuleViaRepoForm(&buf, entity, clone)

	assert.Empty(t, buf.String(),
		"canonical form must not get the hint — user is already on the right path")
}

// TestWarnGoModuleViaRepoForm_NoClonePath_NoHint: --refresh
// without --path (e.g., for non-git-hosted entities) leaves
// clonePath empty. The detection can't run; the hint must NOT
// fire (would emit a false positive otherwise).
func TestWarnGoModuleViaRepoForm_NoClonePath_NoHint(t *testing.T) {
	entity := &profile.Entity{
		CanonicalURI: "repo:github/foo/bar",
		Type:         profile.EntityProject,
	}

	var buf bytes.Buffer
	maybeWarnGoModuleViaRepoForm(&buf, entity, "")

	assert.Empty(t, buf.String(),
		"empty clonePath means we can't detect; stay silent rather than guess")
}

// TestFunctional_AnalyzeRefresh_PyPIFailurePropagates_AbsenceSignal:
// on PyPI registry failure, an absence:repo_declaration signal
// with retryable=true must land BEFORE the error return —
// gives the profile a stored marker distinguishing "tried and
// registry failed" from "never tried."
func TestFunctional_AnalyzeRefresh_PyPIFailurePropagates_AbsenceSignal(t *testing.T) {
	pypiSrv := failingPypiSrv()
	defer pypiSrv.Close()

	dir := t.TempDir()
	globals := &Globals{
		DBPath:          filepath.Join(dir, "test.db"),
		Collectors:      []signal.Collector{newMockCollector()},
		AuditFilePath:   filepath.Join(dir, "audit.log"),
		PypiRegistryURL: pypiSrv.URL,
	}

	cmd := &AnalyzeCmd{Target: "pkg:pypi/idna", Refresh: true}
	_ = cmd.Run(globals) // error expected; covered above

	s, openErr := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, openErr)
	defer s.Close() //nolint:errcheck

	entity, findErr := s.FindEntityByURI(context.Background(), "pkg:pypi/idna")
	require.NoError(t, findErr, "entity must exist even when pypi resolution fails")

	signals, sigErr := s.GetSignals(context.Background(), entity.ID)
	require.NoError(t, sigErr)

	var absenceSig *profile.Signal
	for i := range signals {
		if signals[i].Type == "absence:repo_declaration" && signals[i].Source == "pypi-registry" {
			absenceSig = &signals[i]
			break
		}
	}
	require.NotNil(t, absenceSig,
		"absence:repo_declaration signal (source=pypi-registry) must be written before the error return")

	var val map[string]any
	require.NoError(t, json.Unmarshal(absenceSig.Value, &val))
	retryable, ok := val["retryable"].(bool)
	assert.True(t, ok && retryable,
		"absence signal must carry retryable=true; got val=%v", val)
}

// TestFunctional_AnalyzeRefresh_ResolvesGoModuleViaProxyOrigin pins
// the canonical "post-Go-1.20 vanity host" path: the proxy returns
// a version-info with an Origin block pointing at github, the
// orchestrator stamps entity.URL with that URL, and downstream
// dispatch can fire the github + git + repofiles + openssf
// collectors. Mirrors the npm/pypi integration tests in shape.
//
// Pre-creates a stale entity with empty URL (the
// pkg:golang/gopkg.in/yaml.v3-style dogfood scenario).
// resolveGoRepo walks proxy → finds Origin → stamps entity.URL.
func TestFunctional_AnalyzeRefresh_ResolvesGoModuleViaProxyOrigin(t *testing.T) {
	t.Parallel()

	// Proxy returns @latest + .info with Origin pointing at github.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/example.org/foo/@latest":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}`)
		case "/example.org/foo/@v/v1.0.0.info":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{
                "Version":"v1.0.0",
                "Time":"2026-01-01T00:00:00Z",
                "Origin":{
                    "VCS":"git",
                    "URL":"https://github.com/example-org/foo"
                }
            }`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer proxy.Close()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Pre-create the stale entity. Empty URL is the dogfood state:
	// pkg:golang/<vanity>/<name> entity exists from a prior run that
	// didn't have resolveGoRepo wired.
	{
		s, err := store.OpenSQLite(t.Context(), dbPath)
		require.NoError(t, err)
		stale := &profile.Entity{
			ID:           profile.NewEntityID(),
			CanonicalURI: "pkg:golang/example.org/foo",
			Type:         profile.EntityPackage,
			ShortName:    "example.org/foo",
			Ecosystem:    "golang",
			URL:          "", // ← THE BUG STATE: vanity host, no URL
			CreatedAt:    time.Now().UTC(),
			UpdatedAt:    time.Now().UTC(),
		}
		require.NoError(t, s.PutEntity(t.Context(), stale))
		require.NoError(t, s.Close())
	}

	globals := &Globals{
		DBPath:        dbPath,
		Collectors:    []signal.Collector{newMockCollector()},
		AuditFilePath: filepath.Join(dir, "audit.log"),
		GoProxyURL:    proxy.URL,
	}
	cmd := &AnalyzeCmd{Target: "pkg:golang/example.org/foo", Refresh: true}
	require.NoError(t, cmd.Run(globals),
		"analyze --refresh against a stale Go vanity entity must resolve via proxy and proceed")

	// Re-read and verify URL is now populated with the github source
	// the proxy declared.
	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	entity, err := s.FindEntityByURI(t.Context(), "pkg:golang/example.org/foo")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/example-org/foo", entity.URL,
		"empty URL on a stale pkg:golang/vanity entity must be backfilled by analyze --refresh via proxy Origin")
}

// TestFunctional_AnalyzeRefresh_ResolvesGoModuleViaMetaTag pins the
// pre-Go-1.20 path: proxy has the module but no Origin block (the
// gopkg.in/yaml.v3 dogfood symptom), the resolver falls back to the
// vanity host's go-import meta tag, finds the github URL, and
// stamps entity.URL with it.
func TestFunctional_AnalyzeRefresh_ResolvesGoModuleViaMetaTag(t *testing.T) {
	t.Parallel()

	// Proxy: module exists but no Origin block.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gopkg.in/yaml.v3/@latest":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"Version":"v3.0.1","Time":"2022-05-27T08:35:30Z"}`)
		case "/gopkg.in/yaml.v3/@v/v3.0.1.info":
			w.Header().Set("Content-Type", "application/json")
			// Pre-1.20 publish: no Origin block.
			fmt.Fprint(w, `{"Version":"v3.0.1","Time":"2022-05-27T08:35:30Z"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer proxy.Close()

	// Vanity host: returns go-import meta tag pointing at github.
	vanity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head>
            <meta name="go-import" content="gopkg.in/yaml.v3 git https://github.com/go-yaml/yaml">
        </head></html>`)
	}))
	defer vanity.Close()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	{
		s, err := store.OpenSQLite(t.Context(), dbPath)
		require.NoError(t, err)
		stale := &profile.Entity{
			ID:           profile.NewEntityID(),
			CanonicalURI: "pkg:golang/gopkg.in/yaml.v3",
			Type:         profile.EntityPackage,
			ShortName:    "gopkg.in/yaml.v3",
			Ecosystem:    "golang",
			URL:          "",
			CreatedAt:    time.Now().UTC(),
			UpdatedAt:    time.Now().UTC(),
		}
		require.NoError(t, s.PutEntity(t.Context(), stale))
		require.NoError(t, s.Close())
	}

	globals := &Globals{
		DBPath:        dbPath,
		Collectors:    []signal.Collector{newMockCollector()},
		AuditFilePath: filepath.Join(dir, "audit.log"),
		GoProxyURL:    proxy.URL,
		GoVanityURL:   vanity.URL,
	}
	cmd := &AnalyzeCmd{Target: "pkg:golang/gopkg.in/yaml.v3", Refresh: true}
	require.NoError(t, cmd.Run(globals),
		"analyze --refresh against gopkg.in vanity must fall back to meta-tag and resolve")

	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	entity, err := s.FindEntityByURI(t.Context(), "pkg:golang/gopkg.in/yaml.v3")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/go-yaml/yaml", entity.URL,
		"meta-tag fallback must resolve gopkg.in/yaml.v3 to its github source")
}

// TestFunctional_AnalyzeRefresh_GoResolutionUnresolvable verifies the
// "fully exhausted" path: proxy 404 AND vanity host serves no meta
// tag. resolveGoRepo returns nil (not an error — empty is the
// affirmative "no source found"), entity.URL stays empty, dispatch
// silently skips github + git collectors. The user gets gopublish-
// only signals + a non-zero exit only if the orchestrator decides
// the empty result merits one (it does not — same shape as a
// pkg:cargo/* target with no resolver).
func TestFunctional_AnalyzeRefresh_GoResolutionUnresolvable(t *testing.T) {
	t.Parallel()

	// Proxy 404s on everything.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer proxy.Close()

	// Vanity also 404s (no meta tag).
	vanity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer vanity.Close()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	{
		s, err := store.OpenSQLite(t.Context(), dbPath)
		require.NoError(t, err)
		stale := &profile.Entity{
			ID:           profile.NewEntityID(),
			CanonicalURI: "pkg:golang/example.org/missing",
			Type:         profile.EntityPackage,
			ShortName:    "example.org/missing",
			Ecosystem:    "golang",
			URL:          "",
			CreatedAt:    time.Now().UTC(),
			UpdatedAt:    time.Now().UTC(),
		}
		require.NoError(t, s.PutEntity(t.Context(), stale))
		require.NoError(t, s.Close())
	}

	globals := &Globals{
		DBPath:        dbPath,
		Collectors:    []signal.Collector{newMockCollector()},
		AuditFilePath: filepath.Join(dir, "audit.log"),
		GoProxyURL:    proxy.URL,
		GoVanityURL:   vanity.URL,
	}
	cmd := &AnalyzeCmd{Target: "pkg:golang/example.org/missing", Refresh: true}
	require.NoError(t, cmd.Run(globals),
		"unresolvable Go module must NOT fail --refresh — empty resolution is a valid 'no github source' answer")

	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	entity, err := s.FindEntityByURI(t.Context(), "pkg:golang/example.org/missing")
	require.NoError(t, err)
	assert.Equal(t, "", entity.URL,
		"unresolvable module must leave URL empty — downstream isGitHostedEntity gates github+git collectors out cleanly")
}
