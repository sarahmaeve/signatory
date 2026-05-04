package ecosystem

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSource is an in-package stub for the Source interface. It lets
// each test pre-load filenames and language strings keyed by
// owner/name, plus optional canned errors for either call. Keeping it
// minimal — two maps per call type — avoids accreting test-only
// abstraction.
type fakeSource struct {
	files   map[string][]string
	langs   map[string]string
	listErr error
	langErr error
}

func key(owner, name string) string { return owner + "/" + name }

func (f *fakeSource) ListRootFilenames(_ context.Context, owner, name string) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.files[key(owner, name)], nil
}

func (f *fakeSource) GetRepoLanguage(_ context.Context, owner, name string) (string, error) {
	if f.langErr != nil {
		return "", f.langErr
	}
	return f.langs[key(owner, name)], nil
}

// --- classifyRootFiles: rule-set tests ---------------------------------------

func TestClassifyRootFiles_Go(t *testing.T) {
	assert.Equal(t, []Ecosystem{EcosystemGo}, classifyRootFiles([]string{"go.mod"}))
}

func TestClassifyRootFiles_Crates(t *testing.T) {
	assert.Equal(t, []Ecosystem{EcosystemCrates}, classifyRootFiles([]string{"Cargo.toml"}))
}

func TestClassifyRootFiles_PyPI_Modern(t *testing.T) {
	assert.Equal(t, []Ecosystem{EcosystemPyPI}, classifyRootFiles([]string{"pyproject.toml"}))
}

func TestClassifyRootFiles_PyPI_Legacy(t *testing.T) {
	assert.Equal(t, []Ecosystem{EcosystemPyPI}, classifyRootFiles([]string{"setup.py"}))
}

func TestClassifyRootFiles_PyPI_Both(t *testing.T) {
	// Both modern and legacy Python manifests collapse to a single
	// EcosystemPyPI entry — we don't double-count an ecosystem when
	// multiple of its signal files coexist.
	assert.Equal(t,
		[]Ecosystem{EcosystemPyPI},
		classifyRootFiles([]string{"pyproject.toml", "setup.py"}),
	)
}

func TestClassifyRootFiles_Maven(t *testing.T) {
	assert.Equal(t, []Ecosystem{EcosystemMaven}, classifyRootFiles([]string{"pom.xml"}))
}

func TestClassifyRootFiles_NPM(t *testing.T) {
	assert.Equal(t, []Ecosystem{EcosystemNPM}, classifyRootFiles([]string{"package.json"}))
}

func TestClassifyRootFiles_Empty(t *testing.T) {
	got := classifyRootFiles(nil)
	assert.Empty(t, got)
	assert.NotNil(t, got, "empty result must be a zero-length slice, not nil — callers range over it")
}

func TestClassifyRootFiles_PriorityGoBeatsNPM(t *testing.T) {
	// Real-world case: a Go backend repo that also ships JS release
	// scripts via package.json. Go is the publishing ecosystem; npm
	// is incidental. Both should appear, Go first.
	got := classifyRootFiles([]string{"go.mod", "package.json"})
	assert.Equal(t, []Ecosystem{EcosystemGo, EcosystemNPM}, got)
}

func TestClassifyRootFiles_PriorityOrder(t *testing.T) {
	// All manifests present at once — verifies the documented
	// priority order Go > Crates > Gem > Maven > PyPI > NPM.
	got := classifyRootFiles([]string{
		"package.json", "setup.py", "Cargo.toml", "go.mod", "Gemfile", "pom.xml",
	})
	assert.Equal(t,
		[]Ecosystem{EcosystemGo, EcosystemCrates, EcosystemGem, EcosystemMaven, EcosystemPyPI, EcosystemNPM},
		got,
	)
}

func TestClassifyRootFiles_IgnoresUnrelatedFiles(t *testing.T) {
	// Common repo-root noise must not affect the result.
	got := classifyRootFiles([]string{"LICENSE", "README.md", ".github", "Makefile", "docs"})
	assert.Empty(t, got)
}

// --- Detect: integration with Source ----------------------------------------

func TestDetect_HappyPath_Go(t *testing.T) {
	src := &fakeSource{
		files: map[string][]string{key("o", "r"): {"go.mod", "README.md"}},
		langs: map[string]string{key("o", "r"): "Go"},
	}
	got, err := NewDetector(src).Detect(context.Background(), "o", "r")
	require.NoError(t, err)
	assert.Equal(t, EcosystemGo, got.Primary)
	assert.Equal(t, []Ecosystem{EcosystemGo}, got.Candidates)
	assert.Equal(t, "Go", got.Language)
	assert.Equal(t, []string{"go.mod", "README.md"}, got.RootFiles)
}

func TestDetect_HappyPath_PyPI(t *testing.T) {
	src := &fakeSource{
		files: map[string][]string{key("o", "r"): {"setup.py", "README.md"}},
		langs: map[string]string{key("o", "r"): "Python"},
	}
	got, err := NewDetector(src).Detect(context.Background(), "o", "r")
	require.NoError(t, err)
	assert.Equal(t, EcosystemPyPI, got.Primary)
	assert.Equal(t, []Ecosystem{EcosystemPyPI}, got.Candidates)
	assert.Equal(t, "Python", got.Language)
}

func TestDetect_Polyglot(t *testing.T) {
	// Order matters: Go must come before NPM in Candidates per
	// priorityOrder, and Primary must be Go.
	src := &fakeSource{
		files: map[string][]string{key("o", "r"): {"go.mod", "package.json"}},
		langs: map[string]string{key("o", "r"): "Go"},
	}
	got, err := NewDetector(src).Detect(context.Background(), "o", "r")
	require.NoError(t, err)
	assert.Equal(t, EcosystemGo, got.Primary)
	assert.Equal(t, []Ecosystem{EcosystemGo, EcosystemNPM}, got.Candidates)
}

func TestDetect_NoManifest(t *testing.T) {
	// Even with no manifest, Language is still populated from the
	// second API call — the detector always calls GetRepoLanguage.
	src := &fakeSource{
		files: map[string][]string{key("o", "r"): {"README.md"}},
		langs: map[string]string{key("o", "r"): "Markdown"},
	}
	got, err := NewDetector(src).Detect(context.Background(), "o", "r")
	require.NoError(t, err)
	assert.Equal(t, EcosystemUnknown, got.Primary)
	assert.Empty(t, got.Candidates)
	assert.Equal(t, "Markdown", got.Language)
}

func TestDetect_ListError(t *testing.T) {
	src := &fakeSource{listErr: errors.New("boom")}
	_, err := NewDetector(src).Detect(context.Background(), "o", "r")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list root of o/r")
	assert.ErrorContains(t, err, "boom")
}

func TestDetect_LanguageErrorTolerated(t *testing.T) {
	// Per the implementation comment: a language-lookup failure is
	// not fatal when we already have a manifest-based ecosystem
	// guess. The result should still carry the manifest-derived
	// Primary/Candidates, with Language left empty.
	src := &fakeSource{
		files:   map[string][]string{key("o", "r"): {"go.mod"}},
		langErr: errors.New("rate limited"),
	}
	got, err := NewDetector(src).Detect(context.Background(), "o", "r")
	require.NoError(t, err)
	assert.Equal(t, EcosystemGo, got.Primary)
	assert.Equal(t, []Ecosystem{EcosystemGo}, got.Candidates)
	assert.Empty(t, got.Language)
}

func TestDetect_LanguageErrorFatalWhenNoManifest(t *testing.T) {
	// With no manifest signal, we have nothing to fall back to, so a
	// language-lookup error is fatal.
	src := &fakeSource{
		files:   map[string][]string{key("o", "r"): {"README.md"}},
		langErr: errors.New("rate limited"),
	}
	_, err := NewDetector(src).Detect(context.Background(), "o", "r")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get language for o/r")
	assert.ErrorContains(t, err, "rate limited")
}

func TestDetect_NilSourceRejected(t *testing.T) {
	// Constructing a Detector by struct-literal (skipping
	// NewDetector) should produce a clear error rather than a nil
	// dereference.
	_, err := (&Detector{}).Detect(context.Background(), "o", "r")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil Source")
}

func TestDetect_EmptyRepo(t *testing.T) {
	// Empty file list and empty language: no error, just a
	// thoroughly "unknown" result.
	src := &fakeSource{
		files: map[string][]string{key("o", "r"): {}},
		langs: map[string]string{key("o", "r"): ""},
	}
	got, err := NewDetector(src).Detect(context.Background(), "o", "r")
	require.NoError(t, err)
	assert.Equal(t, EcosystemUnknown, got.Primary)
	assert.Empty(t, got.Candidates)
	assert.Empty(t, got.Language)
	assert.Empty(t, got.RootFiles)
}
