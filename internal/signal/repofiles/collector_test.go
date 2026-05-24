package repofiles

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

func TestCollector_NoClone_ReturnsErrNoClone(t *testing.T) {
	t.Parallel()

	c := NewCollector("/does/not/exist")
	_, err := c.Collect(context.Background(), &profile.Entity{ID: "e1"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNoClone), "want ErrNoClone, got %v", err)
}

func TestCollector_EmptyPath_ReturnsErrNoClone(t *testing.T) {
	t.Parallel()

	c := NewCollector("")
	_, err := c.Collect(context.Background(), &profile.Entity{ID: "e1"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNoClone), "want ErrNoClone, got %v", err)
}

// TestCollector_Name locks in the source identifier. Changes here
// are a breaking change for any downstream query that filters signals
// by source — treat accordingly.
func TestCollector_Name(t *testing.T) {
	t.Parallel()

	c := NewCollector("")
	assert.Equal(t, "repofiles", c.Name())
}

// TestCollector_EmitsRepoFilesCompoundSignal verifies the end-to-end
// shape for the repo_files signal specifically. The collector emits
// multiple signals (repo_files + agent_config_files +
// agent_config_content_injection always; proc_macro_crate when
// Cargo.toml is present); this test locks in the repo_files entry's
// shape independent of the others.
func TestCollector_EmitsRepoFilesCompoundSignal(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"),
		[]byte("readme body"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "SECURITY.md"),
		[]byte("security body"), 0o644))

	c := NewCollector(root)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "repo:github/test/repo"})
	require.NoError(t, err)
	require.NotNil(t, result)

	sig := findSignal(t, result, "repo_files")
	require.NotNil(t, sig, "repo_files signal must be emitted")
	assert.Equal(t, "repofiles", sig.Source)
	assert.Equal(t, profile.SignalGroupHygiene, sig.Group)
	assert.Equal(t, profile.ForgeryLowDeclining, sig.ForgeryResistance)
	assert.Equal(t, "repo:github/test/repo", sig.EntityID)

	// Value is a map[family]Result. Every declared family must
	// appear — absent families as {present: false}.
	var value map[string]Result
	require.NoError(t, json.Unmarshal(sig.Value, &value))

	for _, fam := range Families() {
		entry, ok := value[fam.Name]
		require.True(t, ok, "family %q missing from signal value", fam.Name)
		_ = entry
	}

	assert.True(t, value["readme"].Present)
	assert.Equal(t, "README.md", value["readme"].Path)
	assert.True(t, value["security"].Present)
	assert.Equal(t, "SECURITY.md", value["security"].Path)
	assert.False(t, value["contributing"].Present)
	assert.False(t, value["codeowners"].Present)
}

// findSignal returns the first emitted signal with the given type,
// or nil if not present. Walks the collected slice rather than
// asserting position because the collector emits multiple signals
// and the relative order is not part of the contract.
func findSignal(t *testing.T, result *signal.CollectionResult, sigType string) *profile.Signal {
	t.Helper()
	if result == nil {
		return nil
	}
	for i := range result.Collected {
		s := result.Collected[i]
		if s.IsAbsence() {
			continue
		}
		sig := s.ToSignal()
		if sig.Type == sigType {
			return &sig
		}
	}
	return nil
}

// TestCollector_EmptyClone_AllFamiliesAbsent verifies the empty-repo
// shape: the repo_files signal still emits, every hygiene family
// reports absent. Distinct from "no signal at all" — the handoff
// reads "we checked and nothing was there" rather than "signatory
// didn't look."
func TestCollector_EmptyClone_AllFamiliesAbsent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o750))

	c := NewCollector(root)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "e1"})
	require.NoError(t, err)

	sig := findSignal(t, result, "repo_files")
	require.NotNil(t, sig)
	var value map[string]Result
	require.NoError(t, json.Unmarshal(sig.Value, &value))

	for _, fam := range Families() {
		entry := value[fam.Name]
		assert.False(t, entry.Present, "family %q should be absent in empty clone", fam.Name)
		assert.Empty(t, entry.Path, "family %q: path must be empty when absent", fam.Name)
	}
}

// TestCollector_SignalValueOmitsFamilyField is the JSON-shape
// regression test for Result.Family's json:"-" tag. If drift removed
// the tag, the encoded value would double-encode family names (once
// as map key, once as "Family" field). Lock it in at the collector's
// public boundary.
func TestCollector_SignalValueOmitsFamilyField(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"),
		[]byte("readme"), 0o644))

	c := NewCollector(root)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "e1"})
	require.NoError(t, err)

	sig := findSignal(t, result, "repo_files")
	require.NotNil(t, sig)

	// Parse generically and inspect the readme entry's keys.
	var generic map[string]map[string]any
	require.NoError(t, json.Unmarshal(sig.Value, &generic))

	readme := generic["readme"]
	require.NotNil(t, readme)

	_, hasFamily := readme["Family"]
	assert.False(t, hasFamily, "Family field must be omitted from encoded output")

	// Positive shape assertions.
	assert.Equal(t, true, readme["present"])
	assert.Equal(t, "README.md", readme["path"])
}

// TestCollector_ComposerIntegration verifies the compound signal
// flows through profile.Summarize into the Hygiene group. This is
// the coupling that makes Phase 1's handoff-inlining automatically
// surface repo_files — if this breaks, the handoff would silently
// omit the new signal.
func TestCollector_ComposerIntegration(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"),
		[]byte("readme"), 0o644))

	c := NewCollector(root)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "e1"})
	require.NoError(t, err)

	// Convert SignalOrAbsence slice to []profile.Signal for Summarize.
	signals := make([]profile.Signal, 0, len(result.Collected))
	for _, soa := range result.Collected {
		signals = append(signals, soa.ToSignal())
	}

	summary := profile.Summarize(signals)
	require.NotNil(t, summary.Hygiene)
	repoFiles, ok := summary.Hygiene["repo_files"]
	require.True(t, ok, "repo_files must land under Hygiene in SignalsSummary")

	// Sanity: the flattened value is a map with the readme key.
	asMap, ok := repoFiles.(map[string]interface{})
	require.True(t, ok, "repo_files value must be a JSON object")
	_, ok = asMap["readme"]
	assert.True(t, ok, "readme family must appear in composed summary")
}

// TestCollector_DefaultTTL locks in the 24h TTL. A change here is
// a cadence decision — the test failing is the reminder to update
// docs and design notes in the same commit.
func TestCollector_DefaultTTL(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))

	c := NewCollector(root)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "e1"})
	require.NoError(t, err)

	sig := findSignal(t, result, "repo_files")
	require.NotNil(t, sig)
	ttl := sig.ExpiresAt.Sub(sig.CollectedAt)
	assert.Equal(t, defaultTTL, ttl)
}

// TestCollector_ImplementsCollectorInterface is a compile-time guard
// via assignment — if Collector stops satisfying signal.Collector,
// this fails to build.
func TestCollector_ImplementsCollectorInterface(t *testing.T) {
	t.Parallel()
	var _ signal.Collector = (*Collector)(nil)
}

// TestCollector_ProcMacroCrate_Present verifies that a Cargo.toml with
// [lib] proc-macro = true triggers a proc_macro_crate signal emission.
func TestCollector_ProcMacroCrate_Present(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "Cargo.toml"), []byte(`
[package]
name = "serde_derive"
version = "1.0.219"

[lib]
proc-macro = true

[dependencies]
syn = "2"
`), 0o644))

	c := NewCollector(root)
	entity := &profile.Entity{
		ID:           "test-proc-macro",
		CanonicalURI: "pkg:cargo/serde-derive",
		Ecosystem:    "cargo",
	}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	// Should emit repo_files + proc_macro_crate.
	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	assert.Contains(t, signalMap, "proc_macro_crate")
	var val map[string]any
	require.NoError(t, json.Unmarshal(signalMap["proc_macro_crate"], &val))
	assert.Equal(t, true, val["present"])
}

// TestCollector_ProcMacroCrate_Absent verifies that a Cargo.toml
// without proc-macro = true does not emit proc_macro_crate as present.
func TestCollector_ProcMacroCrate_Absent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "Cargo.toml"), []byte(`
[package]
name = "serde"
version = "1.0.219"

[dependencies]
serde_derive = "1.0"
`), 0o644))

	c := NewCollector(root)
	entity := &profile.Entity{
		ID:           "test-non-proc-macro",
		CanonicalURI: "pkg:cargo/serde",
		Ecosystem:    "cargo",
	}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	assert.Contains(t, signalMap, "proc_macro_crate")
	var val map[string]any
	require.NoError(t, json.Unmarshal(signalMap["proc_macro_crate"], &val))
	assert.Equal(t, false, val["present"],
		"non-proc-macro crate should emit present=false")
}

// TestCollector_ProcMacroCrate_NoCargo verifies that repos without
// Cargo.toml don't emit proc_macro_crate at all (signal not applicable).
func TestCollector_ProcMacroCrate_NoCargo(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"),
		[]byte("non-rust project"), 0o644))

	c := NewCollector(root)
	entity := &profile.Entity{
		ID:           "test-go-project",
		CanonicalURI: "repo:github/test/goproject",
		Ecosystem:    "go",
	}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	signals := result.Signals()
	for _, s := range signals {
		assert.NotEqual(t, "proc_macro_crate", s.Type,
			"non-Rust repos must not emit proc_macro_crate")
	}
}
