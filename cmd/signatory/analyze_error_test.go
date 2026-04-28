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
