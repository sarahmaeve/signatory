package repofiles

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeClone creates a throwaway directory with a minimal .git marker
// (stat-only check — we don't need a real git repo for scanner
// tests, just the presence of a .git entry). Populates the returned
// root with the files listed in entries, each keyed by its path
// relative to the root and valued by its content.
//
// This avoids the git subprocess overhead of the sibling git-package
// tests. For real git-backed integration the collector test at the
// bottom of this file uses t.TempDir + init-style setup.
func makeClone(t *testing.T, entries map[string]string) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))

	for rel, content := range entries {
		abs := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
	}
	return root
}

// TestScan_MissingClone_ReturnsErrNoClone covers the two shapes of
// "no usable clone": empty string and a path with no .git entry. Both
// must surface the same sentinel so the orchestrator reports one
// clean error to the operator.
func TestScan_MissingClone_ReturnsErrNoClone(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"empty path":       "",
		"nonexistent path": "/does/not/exist/at/all",
		"dir without .git": t.TempDir(),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Scan(path, Families())
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrNoClone), "want ErrNoClone, got %v", err)
		})
	}
}

// TestScan_FullyPopulated verifies every declared family is detected
// when the canonical filename exists. This is the happy-path coverage
// test — a regression here means the collector would report false
// absences for well-maintained repos.
func TestScan_FullyPopulated(t *testing.T) {
	t.Parallel()

	root := makeClone(t, map[string]string{
		"README.md":          "readme body",
		"SECURITY.md":        "security body",
		".github/CODEOWNERS": "* @owner",
		".mailmap":           "A <a@x.com> B <b@x.com>",
		"CHANGELOG.md":       "changelog body",
		"CONTRIBUTING.md":    "contributing body",
		"AUTHORS":            "Author A",
		"MAINTAINERS.md":     "Maintainer A",
		"GOVERNANCE.md":      "governance body",
	})

	matches, err := Scan(root, Families())
	require.NoError(t, err)

	gotFamilies := make(map[string]string, len(matches))
	for _, m := range matches {
		gotFamilies[m.Family] = m.Path
	}
	assert.Equal(t, map[string]string{
		"readme":       "README.md",
		"security":     "SECURITY.md",
		"codeowners":   ".github/CODEOWNERS",
		"mailmap":      ".mailmap",
		"changelog":    "CHANGELOG.md",
		"contributing": "CONTRIBUTING.md",
		"authors":      "AUTHORS",
		"maintainers":  "MAINTAINERS.md",
		"governance":   "GOVERNANCE.md",
	}, gotFamilies)
}

// TestScan_CaseVariants_StillDetected verifies the case-insensitive
// detection rule. Host-FS case sensitivity (Linux sensitive, macOS
// default insensitive) must not shift whether a signal is emitted.
func TestScan_CaseVariants_StillDetected(t *testing.T) {
	t.Parallel()

	root := makeClone(t, map[string]string{
		"Readme.md":       "readme body",
		"security.md":     "security body", // lowercase stem
		"Contributing.md": "contributing body",
	})

	matches, err := Scan(root, Families())
	require.NoError(t, err)

	byFamily := make(map[string]string)
	for _, m := range matches {
		byFamily[m.Family] = m.Path
	}
	assert.Equal(t, "Readme.md", byFamily["readme"])
	assert.Equal(t, "security.md", byFamily["security"])
	assert.Equal(t, "Contributing.md", byFamily["contributing"])
}

// TestScan_ZeroByteFile_ReportedAbsent enforces the placeholder-guard
// rule. Empty stubs are the lowest-effort form of fake hygiene and
// must not count.
func TestScan_ZeroByteFile_ReportedAbsent(t *testing.T) {
	t.Parallel()

	root := makeClone(t, map[string]string{
		"SECURITY.md": "", // explicit zero bytes
		"README.md":   "real content",
	})

	matches, err := Scan(root, Families())
	require.NoError(t, err)

	names := familyNames(matches)
	assert.Contains(t, names, "readme", "non-empty README should be present")
	assert.NotContains(t, names, "security", "zero-byte SECURITY.md must be reported absent")
}

// TestScan_Directory_Ignored verifies that an entry matching a family
// regex but shaped as a directory doesn't satisfy the family. A repo
// with a SECURITY/ directory (some multi-doc projects do this) should
// not have SECURITY reported present — we report files, not dirs.
func TestScan_Directory_Ignored(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(root, "SECURITY"), 0o755))

	matches, err := Scan(root, Families())
	require.NoError(t, err)

	assert.NotContains(t, familyNames(matches), "security",
		"directory named SECURITY/ must not register as the security family")
}

// TestScan_MultiDotRejected verifies that backup-style names don't
// register. The regex uses [^.]+, not .+, specifically for this case.
func TestScan_MultiDotRejected(t *testing.T) {
	t.Parallel()

	root := makeClone(t, map[string]string{
		"README.md.bak": "stale backup",
	})

	matches, err := Scan(root, Families())
	require.NoError(t, err)

	assert.Empty(t, matches, "README.md.bak is a backup artifact, not the readme")
}

// TestScan_MissingSubdir_Silent verifies that repos lacking the
// .github/ or docs/ sub-dir don't cause scan failures. This is the
// common case — most repos don't have docs/.
func TestScan_MissingSubdir_Silent(t *testing.T) {
	t.Parallel()

	root := makeClone(t, map[string]string{
		"CODEOWNERS": "* @owner",
	})

	matches, err := Scan(root, Families())
	require.NoError(t, err, "missing .github/ and docs/ must not fail the scan")

	// CODEOWNERS at root is still detected when the sub-dirs don't exist.
	byFamily := make(map[string]string)
	for _, m := range matches {
		byFamily[m.Family] = m.Path
	}
	assert.Equal(t, "CODEOWNERS", byFamily["codeowners"])
}

// TestScan_Symlink_RecordsResolvedTarget verifies the symlink policy:
// the emitted path is the resolved target, not the link. Skipped on
// Windows where os.Symlink often needs elevated privileges.
func TestScan_Symlink_RecordsResolvedTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
	t.Parallel()

	root := makeClone(t, map[string]string{
		"HISTORY.md": "actual changelog content here",
	})
	// Create CHANGELOG.md -> HISTORY.md (in same dir).
	require.NoError(t, os.Symlink("HISTORY.md", filepath.Join(root, "CHANGELOG.md")))

	matches, err := Scan(root, Families())
	require.NoError(t, err)

	// The symlink CHANGELOG.md triggered detection; its resolved path
	// is HISTORY.md. HISTORY.md by itself doesn't match any detector.
	byFamily := make(map[string]string)
	for _, m := range matches {
		byFamily[m.Family] = m.Path
	}
	assert.Equal(t, "HISTORY.md", byFamily["changelog"],
		"symlink must be recorded as its resolved target path")
}

// TestScan_BrokenSymlink_Absent verifies that a dangling symlink
// doesn't satisfy its family. Rare but must not emit a false-positive
// path that points nowhere.
func TestScan_BrokenSymlink_Absent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.Symlink("does-not-exist.md",
		filepath.Join(root, "README.md")))

	matches, err := Scan(root, Families())
	require.NoError(t, err)

	assert.NotContains(t, familyNames(matches), "readme",
		"broken symlink must not count as readme presence")
}

// TestScan_EscapeSymlink_Absent verifies that a symlink escaping the
// clone root is rejected. We refuse to emit absolute host paths or
// paths outside the repo into the signal store.
func TestScan_EscapeSymlink_Absent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
	t.Parallel()

	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "sneaky.md")
	require.NoError(t, os.WriteFile(outsideFile, []byte("outside content"), 0o644))

	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.Symlink(outsideFile, filepath.Join(root, "README.md")))

	matches, err := Scan(root, Families())
	require.NoError(t, err)

	assert.NotContains(t, familyNames(matches), "readme",
		"symlink target outside the clone root must not register as presence")
}

// TestScan_SortedOutput verifies deterministic iteration order for
// downstream ranking and tests. Matches sort by (Family, Path).
func TestScan_SortedOutput(t *testing.T) {
	t.Parallel()

	root := makeClone(t, map[string]string{
		// Two readme variants — both should surface, sorted by Path.
		"README.md":  "a",
		"README.rst": "b",
	})

	matches, err := Scan(root, Families())
	require.NoError(t, err)
	require.Len(t, matches, 2)

	// Sorted lex by path: README.md < README.rst.
	assert.Equal(t, "README.md", matches[0].Path)
	assert.Equal(t, "README.rst", matches[1].Path)
}

// familyNames extracts family names from a match slice, for coverage
// assertions that don't care about paths.
func familyNames(matches []Match) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.Family)
	}
	return out
}
