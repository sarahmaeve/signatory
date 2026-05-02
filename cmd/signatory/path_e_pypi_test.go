package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	pypicollector "github.com/sarahmaeve/signatory/internal/signal/registry/pypi"
	"github.com/sarahmaeve/signatory/internal/store"
)

// Path E end-to-end: a `signatory analyze --refresh pkg:pypi/X` run
// against a pypi-ecosystem target must mint identity:pypi/<login>
// rows for each login-shaped value extracted from the project's
// publisher metadata (info.maintainer, info.author, the PEP 639
// info.maintainers list). And once those rows exist + a burn lands
// on one of them, summary on a package they maintain must surface
// the BURNED banner via cascade.
//
// Path E extends Paths A (github owner entities) and C (npm
// publisher entities) to the third major ecosystem; entity-burn1.md
// "Pending work #1". The test patterns mirror path_a / path_c /
// path_b — same EntityStore wiring shape, same httptest fixture
// pattern, same runCLI cascade probe.
//
// Tests inject the pypi collector via globals.Collectors so the
// httptest server's URL is reachable. The collectorsFor wiring
// (CollectOpts.EntityStore → pypicollector.WithEntityStore) is
// covered by the unit-level TestCollectorsFor_PypiPackage_NoURL_
// GetsPypiCollector; these tests pin the higher-level invariant
// that an analyze run produces the publisher-entity side effect
// AND that the cascade resolver picks up the resulting signals.

// pathERegistryBody is the minimal pypi /pypi/<name>/json payload
// Path E needs: a login-shaped info.maintainer plus a PEP 639
// maintainers list with a second login. Together they exercise
// both extraction branches and the dedupe across them.
//
// Note: no "releases" / "urls" — the v0.1 collector reads only
// info.{maintainer,author,maintainers}, so the rest of the legacy
// PyPI envelope is intentionally absent.
const pathERegistryBody = `{
  "info": {
    "name": "pypath-e-test",
    "maintainer": "ofek",
    "maintainers": [
      {"name": "ofek", "email": "ofek@example.com"},
      {"name": "konstin", "email": "konstin@example.com"}
    ]
  }
}`

// pathEPypiServer returns an httptest server that responds to the
// /pypi/<name>/json endpoint with pathERegistryBody. Mirrors the
// pypi package's projectInfoServer test helper but accessible from
// cmd/. Unlike npm's multi-endpoint shape, pypi has a single
// endpoint at this stage (no per-package downloads/CI).
func pathEPypiServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(pathERegistryBody))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runAnalyzeWithPyPIRegistry sets up a fresh test DB, builds a
// pypi collector pointed at the supplied httptest server with
// WithEntityStore wired to the same DB, runs AnalyzeCmd, and
// returns the DB path so the caller can inspect post-run state.
// Mirrors path_c_publisher_entity_test.go's runAnalyzeWithNpmRegistry.
func runAnalyzeWithPyPIRegistry(t *testing.T, srv *httptest.Server, target string) string {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Pre-create the DB by opening + closing once so the schema
	// migrations run before the collector tries to attach.
	{
		s, err := store.OpenSQLite(t.Context(), dbPath)
		require.NoError(t, err)
		require.NoError(t, s.Close())
	}

	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	pypiClient := pypicollector.NewClientWithBaseURL(srv.URL)
	pypiColl := pypicollector.NewCollectorWithClient(pypiClient).WithEntityStore(s)

	globals := &Globals{
		DBPath:        dbPath,
		Collectors:    []signal.Collector{pypiColl},
		AuditFilePath: filepath.Join(dir, "audit.log"),
		Context:       context.Background(),
		// Redirect the analyze flow's separate resolvePyPIRepo step
		// at the orchestrator (analyze.go:565) to the same httptest
		// server. Without this it hits the real pypi registry, which
		// will 404 on the test-only package name and abort the run
		// before the collector loop fires.
		PypiRegistryURL: srv.URL,
	}

	cmd := &AnalyzeCmd{Target: target, Refresh: true}
	require.NoError(t, cmd.Run(globals),
		"analyze --refresh against the mocked pypi registry must succeed")

	return dbPath
}

// TestPathE_AnalyzeRefresh_MintsPyPIPublisherEntities is the
// end-to-end pin: after analyze --refresh against a pypi-hosted
// target, every login-shaped value in info.maintainer + the PEP 639
// info.maintainers list lands as an identity:pypi/<login> entity
// row. Includes the dedup case (ofek appears in both info.maintainer
// AND info.maintainers; one row should result, not two).
func TestPathE_AnalyzeRefresh_MintsPyPIPublisherEntities(t *testing.T) {
	srv := pathEPypiServer(t)
	dbPath := runAnalyzeWithPyPIRegistry(t, srv, "pkg:pypi/pypath-e-test")

	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	// ofek appears in BOTH info.maintainer AND info.maintainers.
	// EnsureEntityByCanonicalURI's "find OR mint" contract means one
	// row, not two; verify it materialised.
	ofek, err := s.FindEntityByURI(t.Context(), "identity:pypi/ofek")
	require.NoError(t, err,
		"identity:pypi/ofek must exist after analyze --refresh — present in both info.maintainer and info.maintainers")
	assert.Equal(t, profile.EntityIdentity, ofek.Type,
		"pypi-publisher entities must carry Type=EntityIdentity")
	assert.Equal(t, "ofek", ofek.ShortName,
		"the pypi login flows through as the entity's short name")

	// konstin appears ONLY in info.maintainers (the PEP 639 list).
	// The collector must mint identity entities from both extraction
	// branches so a future cascade-burn on konstin has a row to
	// attach to even when info.maintainer named someone else.
	konstin, err := s.FindEntityByURI(t.Context(), "identity:pypi/konstin")
	require.NoError(t, err,
		"identity:pypi/konstin must exist — info.maintainers entries are identity-relevant in their own right")
	assert.Equal(t, profile.EntityIdentity, konstin.Type)
}

// TestPathE_AnalyzeRefresh_PublisherEntityIdempotent verifies that
// running analyze twice against the same pypi target does not
// produce duplicate publisher-entity rows. Parallel to Paths A and
// C's idempotency tests — pin the contract end-to-end so a future
// refactor that re-mints with a fresh UUID against the same URI
// (which the unique index would reject, breaking the second run)
// gets caught here.
func TestPathE_AnalyzeRefresh_PublisherEntityIdempotent(t *testing.T) {
	srv := pathEPypiServer(t)
	dbPath := runAnalyzeWithPyPIRegistry(t, srv, "pkg:pypi/pypath-e-test")

	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	pypiClient := pypicollector.NewClientWithBaseURL(srv.URL)
	pypiColl := pypicollector.NewCollectorWithClient(pypiClient).WithEntityStore(s)

	globals := &Globals{
		DBPath:          dbPath,
		Collectors:      []signal.Collector{pypiColl},
		AuditFilePath:   filepath.Join(t.TempDir(), "audit.log"),
		Context:         context.Background(),
		PypiRegistryURL: srv.URL,
	}
	cmd := &AnalyzeCmd{Target: "pkg:pypi/pypath-e-test", Refresh: true}
	require.NoError(t, cmd.Run(globals),
		"second analyze run must succeed — publisher-entity creation is idempotent")

	// FindEntityByURI returns at most one row (canonical_uri is
	// UNIQUE indexed); the assertion is that it returns successfully,
	// confirming no duplicate-row collision happened on the second mint.
	ofek, err := s.FindEntityByURI(t.Context(), "identity:pypi/ofek")
	require.NoError(t, err)
	assert.Equal(t, "ofek", ofek.ShortName,
		"the existing publisher row must remain intact across re-runs — no overwrite, no duplicate")
}

// TestPathE_CLI_Summary_PyPIPackage_ShowsCascadeFromBurnedMaintainer
// is the end-to-end cascade pin for the pypi flavour of Path B.
// With the analyze run having minted identity:pypi/<login> rows AND
// emitted the maintainer_count signal, burning one of those
// identities must surface as a "BURNED ... via maintainer
// identity:pypi/<login>" rendering on `signatory summary` for the
// package — without per-package manual burn add.
//
// Two-step probe:
//
//  1. Run analyze --refresh against the pypi httptest server (mints
//     entities + emits maintainer_count signal in the same store).
//  2. burn add identity:pypi/ofek via runCLI.
//  3. summary pkg:pypi/pypath-e-test via runCLI.
//
// The third step's stdout must contain BURNED + the ofek URI + the
// burn reason — proves the cascade resolver's pypi-registry source
// dispatch (Phase D of this work) actually fires when invoked
// through the binary, not just in unit tests.
func TestPathE_CLI_Summary_PyPIPackage_ShowsCascadeFromBurnedMaintainer(t *testing.T) {
	srv := pathEPypiServer(t)
	dbPath := runAnalyzeWithPyPIRegistry(t, srv, "pkg:pypi/pypath-e-test")

	// burn add via the actual CLI — exercises kong parsing + the
	// burn add command's entity-finding logic against the row
	// runAnalyzeWithPyPIRegistry just minted.
	add := runCLI(t, dbPath,
		"burn", "add", "identity:pypi/ofek",
		"--reason", "test: pypi maintainer account compromised",
	)
	require.Equal(t, 0, add.exitCode,
		"burn add on a minted pypi identity must succeed; stderr=%q", add.stderr)

	// summary on the package — the cascade fires via the
	// maintainer_count signal that the analyze run just emitted.
	r := runCLI(t, dbPath, "summary", "pkg:pypi/pypath-e-test")
	require.Equal(t, 0, r.exitCode,
		"summary must exit 0; stderr=%q", r.stderr)

	assert.Contains(t, r.stdout, "BURNED",
		"summary must surface the burn marker even though the burn is on the maintainer, not the package itself")
	assert.Contains(t, r.stdout, "compromised",
		"the cascaded burn reason must surface in the rendered output")
	assert.Contains(t, r.stdout, "identity:pypi/ofek",
		"the rendering must name the cascade source so users can trace which ledger entry caused the degradation")
}

// TestPathE_CLI_BurnList_ShowsLiteralPyPIRow pins the audit-surface
// contract for pypi: burn list shows the literal burn row on the
// identity, NOT the cascaded one on the package. Same split as
// path_b_cascade_test.go's TestPathB_CLI_BurnList_ShowsLiteralRowsNotCascaded
// but pypi-flavoured. countercampaign.md §7.7.
func TestPathE_CLI_BurnList_ShowsLiteralPyPIRow(t *testing.T) {
	srv := pathEPypiServer(t)
	dbPath := runAnalyzeWithPyPIRegistry(t, srv, "pkg:pypi/pypath-e-test")

	add := runCLI(t, dbPath,
		"burn", "add", "identity:pypi/ofek",
		"--reason", "test: pypi maintainer compromised",
	)
	require.Equal(t, 0, add.exitCode)

	r := runCLI(t, dbPath, "burn", "list")
	require.Equal(t, 0, r.exitCode)

	assert.Contains(t, r.stdout, "identity:pypi/ofek",
		"burn list must include the literal burn row on the maintainer identity")
	assert.NotContains(t, r.stdout, "pkg:pypi/pypath-e-test",
		"burn list must NOT include the cascaded package — audit surface stays faithful to literal table rows")
}

// pathEDisplayNameOnlyBody is the python-dotenv-shape: legacy
// metadata with info.author set to a free-text display name and
// no info.maintainer. The collector's conservative login-shape
// filter must reject this and emit absence:maintainer_count rather
// than mint identity:pypi/<display name> rows.
//
// This is a load-bearing invariant — without it, the store would
// pollute with non-existent identities scraped from publisher
// display names, creating false-positive cascades when those names
// happen to collide with real identities elsewhere.
const pathEDisplayNameOnlyBody = `{
  "info": {
    "name": "display-name-only",
    "author": "Saurabh Kumar"
  }
}`

// TestPathE_AnalyzeRefresh_DisplayNameOnly_NoMintsNoCascade is the
// end-to-end equivalent of the unit-level
// TestCollector_PublisherEntities_DisplayNameDoesNotMint. Drives
// the same case through AnalyzeCmd to confirm the conservative
// filter holds across the orchestrator boundary.
func TestPathE_AnalyzeRefresh_DisplayNameOnly_NoMintsNoCascade(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(pathEDisplayNameOnlyBody))
	}))
	defer srv.Close()

	dbPath := runAnalyzeWithPyPIRegistry(t, srv, "pkg:pypi/display-name-only")

	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	// No identity:pypi/* row should exist — "Saurabh Kumar" has a
	// space and fails the login-shape filter; minting it would
	// pollute the store with a fabricated identity.
	_, err = s.FindEntityByURI(t.Context(), "identity:pypi/saurabh kumar")
	require.Error(t, err,
		"identity:pypi/saurabh kumar must NOT exist — the login-shape filter must reject display names with spaces")
	_, err = s.FindEntityByURI(t.Context(), "identity:pypi/saurabh")
	require.Error(t, err,
		"identity:pypi/saurabh must NOT exist either — the extractor doesn't speculate on tokenizing display names")

	// And summary on the package must NOT show a BURNED banner —
	// no entities were minted, no burns exist, no cascade can fire.
	// The summary verb renders analyses (not the signal panel —
	// that's analyze's job), so the assertion stays focused on
	// "burn-cascade absence" rather than signal-row presence.
	r := runCLI(t, dbPath, "summary", "pkg:pypi/display-name-only")
	require.Equal(t, 0, r.exitCode, "summary must exit 0; stderr=%q", r.stderr)
	assert.NotContains(t, r.stdout, "BURNED",
		"display-name-only target with no burns must NOT show a BURNED banner — the conservative login filter prevented entity minting, so cascade has nothing to walk through")

	// Cross-check via the analyze verb (which DOES show absences):
	// the maintainer_count signal must record as absence with the
	// reason from extractPyPILogins, so the entity profile reflects
	// 'we tried, no logins extractable'.
	a := runCLI(t, dbPath, "analyze", "pkg:pypi/display-name-only")
	require.Equal(t, 0, a.exitCode, "analyze must exit 0; stderr=%q", a.stderr)
	assert.Contains(t, a.stdout, "maintainer_count",
		"analyze must list maintainer_count under absences so the user knows we tried")
	assert.Contains(t, a.stdout, "no login-shaped value",
		"the absence reason must surface verbatim — it's the contract that explains why no entity was minted")
}
