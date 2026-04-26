package pypi

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/sarahmaeve/signatory/internal/manifest"
)

// These tests exercise parsePEP508Requirement directly, locking
// in the contract independently of the ParseRequirements wrapper.
// The same shapes are also covered transitively via the
// requirements.txt integration tests, but a future change to the
// helper that breaks pyproject.toml use should fail tests at
// THIS layer first — not only at the integration layer for one
// of two callers.
//
// Inputs here are already pre-stripped of pip directives (-e,
// --hash, -r, comments, continuations) — that's the contract.
// The caller is responsible for any pip-specific preprocessing
// before reaching this helper.

func TestParsePEP508Requirement_Shapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		spec   string
		want   manifest.Dep
		wantOK bool
	}{
		{
			name:   "bare name",
			spec:   "requests",
			want:   manifest.Dep{Name: "requests", CanonicalURI: "pkg:pypi/requests", Direct: true, Ecosystem: "pypi"},
			wantOK: true,
		},
		{
			name:   "pinned with ==",
			spec:   "requests==2.31.0",
			want:   manifest.Dep{Name: "requests", Version: "==2.31.0", CanonicalURI: "pkg:pypi/requests", Direct: true, Ecosystem: "pypi"},
			wantOK: true,
		},
		{
			name:   "lower-bound with >=",
			spec:   "requests>=2.30",
			want:   manifest.Dep{Name: "requests", Version: ">=2.30", CanonicalURI: "pkg:pypi/requests", Direct: true, Ecosystem: "pypi"},
			wantOK: true,
		},
		{
			name:   "multiple constraints",
			spec:   "requests>=2.30,<3.0",
			want:   manifest.Dep{Name: "requests", Version: ">=2.30,<3.0", CanonicalURI: "pkg:pypi/requests", Direct: true, Ecosystem: "pypi"},
			wantOK: true,
		},
		{
			name:   "extras preserved in name, stripped from canonical URI",
			spec:   "requests[security]==2.31.0",
			want:   manifest.Dep{Name: "requests[security]", Version: "==2.31.0", CanonicalURI: "pkg:pypi/requests", Direct: true, Ecosystem: "pypi"},
			wantOK: true,
		},
		{
			name:   "PEP 503 normalization in canonical URI",
			spec:   "Python_Dotenv==1.0.0",
			want:   manifest.Dep{Name: "Python_Dotenv", Version: "==1.0.0", CanonicalURI: "pkg:pypi/python-dotenv", Direct: true, Ecosystem: "pypi"},
			wantOK: true,
		},
		{
			name:   "environment marker stripped",
			spec:   `requests==2.31.0 ; python_version >= "3.10"`,
			want:   manifest.Dep{Name: "requests", Version: "==2.31.0", CanonicalURI: "pkg:pypi/requests", Direct: true, Ecosystem: "pypi"},
			wantOK: true,
		},
		{
			name:   "trailing pip options stripped — caller may leave --hash on the spec",
			spec:   "requests==2.31.0 --hash=sha256:abcdef",
			want:   manifest.Dep{Name: "requests", Version: "==2.31.0", CanonicalURI: "pkg:pypi/requests", Direct: true, Ecosystem: "pypi"},
			wantOK: true,
		},
		{
			name:   "PEP 508 URL form — name @ url",
			spec:   "requests @ git+https://github.com/psf/requests.git",
			want:   manifest.Dep{Name: "requests @ git+https://github.com/psf/requests.git", Ecosystem: "pypi-local", Direct: true},
			wantOK: true,
		},
		{
			name:   "pip-flavored VCS — git+https",
			spec:   "git+https://github.com/foo/bar.git@v1.0",
			want:   manifest.Dep{Name: "git+https://github.com/foo/bar.git@v1.0", Ecosystem: "pypi-local", Direct: true},
			wantOK: true,
		},
		{
			name:   "pip-flavored URL — direct wheel download",
			spec:   "https://example.com/foo-1.0.whl",
			want:   manifest.Dep{Name: "https://example.com/foo-1.0.whl", Ecosystem: "pypi-local", Direct: true},
			wantOK: true,
		},
		{
			name:   "pip-flavored local file URL",
			spec:   "file:///tmp/local-pkg",
			want:   manifest.Dep{Name: "file:///tmp/local-pkg", Ecosystem: "pypi-local", Direct: true},
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parsePEP508Requirement(tt.spec)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestParsePEP508Requirement_RejectsEmptyInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		spec string
	}{
		{"empty string", ""},
		{"whitespace only", "   \t  "},
		{"only environment marker — strips to empty", `; python_version >= "3.10"`},
		{"only whitespace and marker", `   ; sys_platform == "win32"   `},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, ok := parsePEP508Requirement(tt.spec)
			assert.False(t, ok, "empty spec must produce ok=false")
		})
	}
}

// TestParsePEP508Requirement_DefensiveOnMalformedName documents
// that an invalid PEP 508 name (failing the name grammar) still
// produces a Dep — but with empty CanonicalURI. The Dep surfaces
// the raw input to the operator without stamping a malformed
// pkg:pypi/<garbage> URI into the store. Same pattern as npm's
// malformed-name handling.
func TestParsePEP508Requirement_DefensiveOnMalformedName(t *testing.T) {
	t.Parallel()
	got, ok := parsePEP508Requirement("--malformed==1.0")
	assert.True(t, ok, "non-empty spec produces a Dep even when name is invalid")
	assert.Empty(t, got.CanonicalURI, "invalid name must not produce a canonical URI")
	assert.Equal(t, "pypi", got.Ecosystem, "ecosystem slug still set so the Dep is recognizable as PyPI-shaped")
}
