package pypi

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/manifest"
)

// writeRequirements creates a requirements.txt file in a fresh
// temp dir, returns the absolute path. Keeps each test's filesystem
// scope isolated from -r recursion safety checks.
func writeRequirements(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "requirements.txt")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

// --- Line-shape parsing (table-driven) -------------------------------------

func TestParseRequirements_LineShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want manifest.Dep
	}{
		{
			name: "bare name",
			line: "requests",
			want: manifest.Dep{
				Name:         "requests",
				CanonicalURI: "pkg:pypi/requests",
				Version:      "",
				Direct:       true,
				Ecosystem:    "pypi",
			},
		},
		{
			name: "pinned with ==",
			line: "requests==2.31.0",
			want: manifest.Dep{
				Name:         "requests",
				CanonicalURI: "pkg:pypi/requests",
				Version:      "==2.31.0",
				Direct:       true,
				Ecosystem:    "pypi",
			},
		},
		{
			name: "lower-bound with >=",
			line: "requests>=2.30.0",
			want: manifest.Dep{
				Name:         "requests",
				CanonicalURI: "pkg:pypi/requests",
				Version:      ">=2.30.0",
				Direct:       true,
				Ecosystem:    "pypi",
			},
		},
		{
			name: "compatible with ~=",
			line: "requests~=2.30",
			want: manifest.Dep{
				Name:         "requests",
				CanonicalURI: "pkg:pypi/requests",
				Version:      "~=2.30",
				Direct:       true,
				Ecosystem:    "pypi",
			},
		},
		{
			name: "exclusion with !=",
			line: "requests!=2.30.0",
			want: manifest.Dep{
				Name:         "requests",
				CanonicalURI: "pkg:pypi/requests",
				Version:      "!=2.30.0",
				Direct:       true,
				Ecosystem:    "pypi",
			},
		},
		{
			name: "multiple constraints",
			line: "requests>=2.30.0,<3.0.0",
			want: manifest.Dep{
				Name:         "requests",
				CanonicalURI: "pkg:pypi/requests",
				Version:      ">=2.30.0,<3.0.0",
				Direct:       true,
				Ecosystem:    "pypi",
			},
		},
		{
			name: "pre-release version",
			line: "requests==2.31.0a1",
			want: manifest.Dep{
				Name:         "requests",
				CanonicalURI: "pkg:pypi/requests",
				Version:      "==2.31.0a1",
				Direct:       true,
				Ecosystem:    "pypi",
			},
		},
		{
			name: "with extras — extras kept in name, stripped from canonical URI",
			line: "requests[security]==2.31.0",
			want: manifest.Dep{
				Name:         "requests[security]",
				CanonicalURI: "pkg:pypi/requests",
				Version:      "==2.31.0",
				Direct:       true,
				Ecosystem:    "pypi",
			},
		},
		{
			name: "with multiple extras",
			line: "requests[security,socks]==2.31.0",
			want: manifest.Dep{
				Name:         "requests[security,socks]",
				CanonicalURI: "pkg:pypi/requests",
				Version:      "==2.31.0",
				Direct:       true,
				Ecosystem:    "pypi",
			},
		},
		{
			name: "PEP 503 normalization — mixed case",
			line: "Python-Dotenv==1.0.0",
			want: manifest.Dep{
				Name:         "Python-Dotenv",
				CanonicalURI: "pkg:pypi/python-dotenv",
				Version:      "==1.0.0",
				Direct:       true,
				Ecosystem:    "pypi",
			},
		},
		{
			name: "PEP 503 normalization — underscores collapse to hyphens",
			line: "Python_Dotenv==1.0.0",
			want: manifest.Dep{
				Name:         "Python_Dotenv",
				CanonicalURI: "pkg:pypi/python-dotenv",
				Version:      "==1.0.0",
				Direct:       true,
				Ecosystem:    "pypi",
			},
		},
		{
			name: "PEP 503 normalization — repeated separators collapse",
			line: "python__dot..env==1.0.0",
			want: manifest.Dep{
				Name:         "python__dot..env",
				CanonicalURI: "pkg:pypi/python-dot-env",
				Version:      "==1.0.0",
				Direct:       true,
				Ecosystem:    "pypi",
			},
		},
		{
			name: "environment marker stripped",
			line: `requests==2.31.0 ; python_version >= "3.10"`,
			want: manifest.Dep{
				Name:         "requests",
				CanonicalURI: "pkg:pypi/requests",
				Version:      "==2.31.0",
				Direct:       true,
				Ecosystem:    "pypi",
			},
		},
		{
			name: "hash directive stripped",
			line: "requests==2.31.0 --hash=sha256:abcdef0123456789",
			want: manifest.Dep{
				Name:         "requests",
				CanonicalURI: "pkg:pypi/requests",
				Version:      "==2.31.0",
				Direct:       true,
				Ecosystem:    "pypi",
			},
		},
		{
			name: "extras + version + marker + hash all together",
			line: `requests[security]==2.31.0 ; python_version >= "3.10" --hash=sha256:abc`,
			want: manifest.Dep{
				Name:         "requests[security]",
				CanonicalURI: "pkg:pypi/requests",
				Version:      "==2.31.0",
				Direct:       true,
				Ecosystem:    "pypi",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := writeRequirements(t, tt.line+"\n")
			deps, err := ParseRequirements(path)
			require.NoError(t, err)
			require.Len(t, deps, 1, "expected exactly one dep from %q", tt.line)
			assert.Equal(t, tt.want, deps[0])
		})
	}
}

// --- Comments and whitespace ----------------------------------------------

func TestParseRequirements_SkipsCommentLines(t *testing.T) {
	t.Parallel()
	path := writeRequirements(t, "# top-level comment\nrequests==2.31.0\n# trailing comment\n")
	deps, err := ParseRequirements(path)
	require.NoError(t, err)
	require.Len(t, deps, 1)
	assert.Equal(t, "requests", deps[0].Name)
}

func TestParseRequirements_SkipsBlankLines(t *testing.T) {
	t.Parallel()
	path := writeRequirements(t, "\n\nrequests==2.31.0\n\n\nclick==8.0.0\n\n")
	deps, err := ParseRequirements(path)
	require.NoError(t, err)
	require.Len(t, deps, 2)
}

func TestParseRequirements_StripsInlineComments(t *testing.T) {
	t.Parallel()
	path := writeRequirements(t, "requests==2.31.0  # this is the latest\n")
	deps, err := ParseRequirements(path)
	require.NoError(t, err)
	require.Len(t, deps, 1)
	assert.Equal(t, "==2.31.0", deps[0].Version)
}

func TestParseRequirements_HandlesContinuationLines(t *testing.T) {
	t.Parallel()
	// Backslash-newline joins lines per pip's grammar. Common with hash spreads.
	path := writeRequirements(t, "requests==2.31.0 \\\n    --hash=sha256:abc \\\n    --hash=sha256:def\n")
	deps, err := ParseRequirements(path)
	require.NoError(t, err)
	require.Len(t, deps, 1)
	assert.Equal(t, "requests", deps[0].Name)
	assert.Equal(t, "==2.31.0", deps[0].Version)
}

func TestParseRequirements_EmptyFile(t *testing.T) {
	t.Parallel()
	path := writeRequirements(t, "")
	deps, err := ParseRequirements(path)
	require.NoError(t, err)
	assert.Empty(t, deps)
}

func TestParseRequirements_OnlyCommentsAndBlankLines(t *testing.T) {
	t.Parallel()
	path := writeRequirements(t, "# comment 1\n\n# comment 2\n\n")
	deps, err := ParseRequirements(path)
	require.NoError(t, err)
	assert.Empty(t, deps)
}

// --- Non-registry specs → pypi-local --------------------------------------

func TestParseRequirements_NonRegistrySpecs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		line     string
		wantName string // verbatim spec
	}{
		{"editable local current dir", "-e .", "-e ."},
		{"editable local subdir", "-e ./subdir", "-e ./subdir"},
		{"git+https VCS", "git+https://github.com/foo/bar.git@v1.0", "git+https://github.com/foo/bar.git@v1.0"},
		{"git+ssh VCS", "git+ssh://git@github.com/foo/bar.git", "git+ssh://git@github.com/foo/bar.git"},
		{"URL-to-wheel", "https://example.com/foo-1.0.whl", "https://example.com/foo-1.0.whl"},
		{"PEP 508 URL form", "requests @ git+https://github.com/psf/requests.git", "requests @ git+https://github.com/psf/requests.git"},
		{"file URL", "file:///tmp/local-pkg", "file:///tmp/local-pkg"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := writeRequirements(t, tt.line+"\n")
			deps, err := ParseRequirements(path)
			require.NoError(t, err)
			require.Len(t, deps, 1)
			assert.Equal(t, "pypi-local", deps[0].Ecosystem,
				"non-registry specs must be classified as pypi-local")
			assert.Empty(t, deps[0].CanonicalURI,
				"pypi-local deps must have empty CanonicalURI to avoid stamping bad URIs into the store")
			assert.Equal(t, tt.wantName, deps[0].Name,
				"verbatim spec preserved in Name so the operator sees what was declared")
			assert.True(t, deps[0].Direct)
		})
	}
}

// --- Recursive includes (-r) ----------------------------------------------

func TestParseRequirements_FollowsSiblingInclude(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "base.txt"), []byte("requests==2.31.0\n"), 0o600))
	main := filepath.Join(dir, "requirements.txt")
	require.NoError(t, os.WriteFile(main, []byte("-r base.txt\nclick==8.0.0\n"), 0o600))

	deps, err := ParseRequirements(main)
	require.NoError(t, err)
	require.Len(t, deps, 2)

	names := []string{deps[0].Name, deps[1].Name}
	assert.Contains(t, names, "requests")
	assert.Contains(t, names, "click")
}

func TestParseRequirements_FollowsSubdirectoryInclude(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	subdir := filepath.Join(dir, "reqs")
	require.NoError(t, os.MkdirAll(subdir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(subdir, "dev.txt"), []byte("pytest==8.0.0\n"), 0o600))
	main := filepath.Join(dir, "requirements.txt")
	require.NoError(t, os.WriteFile(main, []byte("-r reqs/dev.txt\n"), 0o600))

	deps, err := ParseRequirements(main)
	require.NoError(t, err)
	require.Len(t, deps, 1)
	assert.Equal(t, "pytest", deps[0].Name)
}

func TestParseRequirements_RejectsAbsolutePathInclude(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	main := filepath.Join(dir, "requirements.txt")
	require.NoError(t, os.WriteFile(main, []byte("-r /etc/passwd\n"), 0o600))

	_, err := ParseRequirements(main)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrIncludeOutOfScope,
		"absolute paths in -r must be rejected to prevent reading files outside the project")
}

func TestParseRequirements_RejectsParentTraversalInclude(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	subdir := filepath.Join(dir, "project")
	require.NoError(t, os.MkdirAll(subdir, 0o755))
	main := filepath.Join(subdir, "requirements.txt")
	require.NoError(t, os.WriteFile(main, []byte("-r ../shared.txt\n"), 0o600))

	_, err := ParseRequirements(main)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrIncludeOutOfScope,
		"-r references that escape the original file's directory must be rejected")
}

func TestParseRequirements_RejectsNestedTraversalInclude(t *testing.T) {
	// A two-hop traversal: top-level -r points into a subdir, then
	// the subdir file -r points back up beyond scope. The check
	// must anchor to the ORIGINAL file's directory, not each hop's.
	t.Parallel()
	dir := t.TempDir()
	subdir := filepath.Join(dir, "deep")
	require.NoError(t, os.MkdirAll(subdir, 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(subdir, "inner.txt"), []byte("-r ../../etc/passwd\n"), 0o600))
	main := filepath.Join(dir, "requirements.txt")
	require.NoError(t, os.WriteFile(main, []byte("-r deep/inner.txt\n"), 0o600))

	_, err := ParseRequirements(main)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrIncludeOutOfScope)
}

func TestParseRequirements_IncludeFileNotFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	main := filepath.Join(dir, "requirements.txt")
	require.NoError(t, os.WriteFile(main, []byte("-r missing.txt\n"), 0o600))

	_, err := ParseRequirements(main)
	require.Error(t, err)
	// File-not-found is distinct from out-of-scope: the user asked
	// for a file we'd allow but it isn't there.
	assert.NotErrorIs(t, err, ErrIncludeOutOfScope)
}

func TestParseRequirements_CapsRecursionDepth(t *testing.T) {
	// Build a chain longer than maxIncludeDepth. Each file -r's the
	// next, ending in a dep. The parser must stop with a clear error
	// rather than overflow the stack on a malicious cycle.
	t.Parallel()
	dir := t.TempDir()

	// Build a chain of (maxIncludeDepth + 2) files — req-0 -r's req-1,
	// which -r's req-2, ... ending in a leaf with a real dep. The
	// chain length guarantees the depth check fires before reaching
	// the leaf.
	chainLen := maxIncludeDepth + 2
	for i := 0; i < chainLen; i++ {
		var content string
		if i < chainLen-1 {
			content = formatChainLink(i + 1)
		} else {
			content = "requests==2.31.0\n"
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, chainName(i)), []byte(content), 0o600))
	}

	_, err := ParseRequirements(filepath.Join(dir, chainName(0)))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrIncludeDepthExceeded)
}

// chainName returns the filename for the i-th link in a recursion chain.
func chainName(i int) string {
	return "req-" + strconv.Itoa(i) + ".txt"
}

// formatChainLink returns the content for a chain file that -r's the next link.
func formatChainLink(nextIdx int) string {
	return "-r " + chainName(nextIdx) + "\n"
}

// --- File-level errors ----------------------------------------------------

func TestParseRequirements_FileNotFound(t *testing.T) {
	t.Parallel()
	_, err := ParseRequirements("/does/not/exist/requirements.txt")
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrNotExist) || isPathErr(err),
		"file-not-found should surface a recognizable filesystem error, got %v", err)
}

// isPathErr returns true for an error that wraps an *os.PathError
// — covers the case where the underlying filesystem returns a
// path-shaped error that doesn't unwrap to os.ErrNotExist on every
// platform.
func isPathErr(err error) bool {
	var pe *os.PathError
	return errors.As(err, &pe)
}

// --- Boto3-shaped happy path -----------------------------------------------

func TestParseRequirements_Boto3Shaped(t *testing.T) {
	// A requirements.txt shaped like boto3's actual dev requirements:
	// pinned versions, comments, hashes, env markers, a -r reference.
	// This is the dogfood target for v0.1.
	t.Parallel()
	dir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "core.txt"), []byte(
		"# core dependencies, pinned for reproducibility\n"+
			"botocore==1.35.0\n"+
			"jmespath==1.0.1\n"+
			"s3transfer==0.10.0\n",
	), 0o600))

	main := filepath.Join(dir, "requirements.txt")
	require.NoError(t, os.WriteFile(main, []byte(
		"# boto3 development requirements\n"+
			"\n"+
			"-r core.txt\n"+
			"\n"+
			"# test deps\n"+
			"pytest>=8.0.0\n"+
			"pytest-cov==4.1.0  # coverage reporter\n"+
			"requests[security]==2.31.0 \\\n"+
			"    --hash=sha256:abcdef\n"+
			"# windows-only helpers, kept regardless of platform\n"+
			`pywin32==306 ; sys_platform == "win32"`+"\n",
	), 0o600))

	deps, err := ParseRequirements(main)
	require.NoError(t, err)
	require.Len(t, deps, 7)

	// Spot-check key shapes: extras preserved in Name, env marker
	// stripped, included file's deps present, all marked Direct.
	byName := make(map[string]manifest.Dep, len(deps))
	for _, d := range deps {
		byName[d.Name] = d
		assert.True(t, d.Direct, "every requirements.txt dep is direct")
	}

	require.Contains(t, byName, "botocore")
	require.Contains(t, byName, "requests[security]")
	require.Contains(t, byName, "pywin32")

	assert.Equal(t, "==1.35.0", byName["botocore"].Version)
	assert.Equal(t, "pkg:pypi/requests", byName["requests[security]"].CanonicalURI)
	assert.Equal(t, "==306", byName["pywin32"].Version,
		"environment marker on pywin32 must be stripped, version preserved")
}
