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

// TestCollector_EmitsOneCompoundSignal verifies the end-to-end shape:
// one repo_files signal emitted, grouped under hygiene, value
// structured as a map from family-name to Result.
func TestCollector_EmitsOneCompoundSignal(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"),
		[]byte("readme body"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "SECURITY.md"),
		[]byte("security body"), 0o644))

	c := NewCollector(root)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "repo:github/test/repo"})
	require.NoError(t, err)
	require.NotNil(t, result)

	// Exactly one signal emitted — it's a compound signal, not
	// one-signal-per-family.
	require.Len(t, result.Collected, 1,
		"collector must emit exactly one compound signal")

	soa := result.Collected[0]
	require.False(t, soa.IsAbsence(), "compound signal must not be an absence")

	sig := soa.ToSignal()
	assert.Equal(t, "repo_files", sig.Type)
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

// TestCollector_EmptyClone_AllFamiliesAbsent verifies the empty-repo
// shape: one signal still emitted, but every family reports absent.
// This is distinct from "no signal at all" and is important for the
// handoff — the agent sees "we checked and nothing was there" rather
// than "signatory didn't look."
func TestCollector_EmptyClone_AllFamiliesAbsent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))

	c := NewCollector(root)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "e1"})
	require.NoError(t, err)
	require.Len(t, result.Collected, 1)

	sig := result.Collected[0].ToSignal()
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

	sig := result.Collected[0].ToSignal()

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

	sig := result.Collected[0].ToSignal()
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
