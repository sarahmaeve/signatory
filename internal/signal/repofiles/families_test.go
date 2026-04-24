package repofiles

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDetector_StemWithExt covers the shared detector for families
// whose filenames follow a <stem>.<ext> pattern with single-extension
// tolerance. The table locks in both the positive cases (canonical
// filenames, case variants, tolerated extensions) and the negatives
// we care about: multi-dot artifacts like backups and editor swaps.
func TestDetector_StemWithExt(t *testing.T) {
	t.Parallel()

	det := stemWithExt("README")
	cases := []struct {
		in   string
		want bool
	}{
		// Canonical matches.
		{"README.md", true},
		{"README.rst", true},
		{"README.txt", true},
		{"README", true},
		// Case variants — we match case-insensitively so host-FS
		// case-sensitivity doesn't shift the signal between macOS
		// and Linux.
		{"readme.md", true},
		{"Readme.md", true},
		{"README.MD", true},
		// Non-canonical but reasonable — still matches, ranking
		// pushes it to alt_paths.
		{"README.markdown", true},
		{"README.asciidoc", true},
		// Rejected: multi-dot files are backups or editor swaps,
		// not the hygiene file.
		{"README.md.bak", false},
		{".README.md.swp", false},
		// Rejected: unrelated filenames and stems.
		{"READMES", false},
		{"NOT-A-README.md", false},
		{"", false},
		// Rejected: directories would pass the regex if the name
		// matches; we stat them as dirs and filter in the scanner.
		// (Assertion at scanner level; detector alone is permissive.)
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, det.MatchString(tc.in))
		})
	}
}

// TestDetector_Exact verifies the no-extension detector used by the
// mailmap family. Case-insensitive per the uniform rule.
func TestDetector_Exact(t *testing.T) {
	t.Parallel()

	det := exact(".mailmap")
	cases := []struct {
		in   string
		want bool
	}{
		{".mailmap", true},
		{".MAILMAP", true}, // case-insensitive
		{".Mailmap", true},
		{".mailmap.bak", false}, // no extension tolerance
		{"mailmap", false},      // missing leading dot
		{".mailmapx", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, det.MatchString(tc.in))
		})
	}
}

// TestDetector_Changelog covers the alternation regex that handles
// both CHANGELOG.<ext> and the bare CHANGES convention. The shape is
// unique to this family; every other family reuses stemWithExt or
// exact directly.
func TestDetector_Changelog(t *testing.T) {
	t.Parallel()

	det := changelogDetector()
	cases := []struct {
		in   string
		want bool
	}{
		{"CHANGELOG.md", true},
		{"CHANGELOG.rst", true},
		{"CHANGELOG.txt", true},
		{"CHANGELOG", true},
		{"CHANGES", true},
		{"changes", true}, // case-insensitive
		{"Changelog.md", true},
		{"CHANGELOG.md.bak", false}, // multi-dot rejected
		{"CHANGESET", false},        // not our family
		{"HISTORY.md", false},       // different stem entirely
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, det.MatchString(tc.in))
		})
	}
}

// TestFamilies_Declaration locks in the declared family list. Adding
// or removing a family is a deliberate act — the test failing is the
// reminder to update the signal's documented coverage in types.go and
// the ImproveProvSignals design doc in the same commit.
func TestFamilies_Declaration(t *testing.T) {
	t.Parallel()

	fams := Families()
	names := make([]string, 0, len(fams))
	for _, f := range fams {
		names = append(names, f.Name)
	}

	want := []string{
		"readme",
		"security",
		"codeowners",
		"mailmap",
		"changelog",
		"contributing",
		"authors",
		"maintainers",
		"governance",
	}
	assert.Equal(t, want, names,
		"family list and order must stay stable — compound signal value iteration depends on it")
}

// TestFamilies_AllHaveDetectorAndPreferred enforces that each family
// is fully declared. A nil detector or empty Preferred list would
// silently disable that family at runtime; this check surfaces the
// omission at build time.
func TestFamilies_AllHaveDetectorAndPreferred(t *testing.T) {
	t.Parallel()

	for _, f := range Families() {
		t.Run(f.Name, func(t *testing.T) {
			t.Parallel()
			require.NotNil(t, f.Detector, "family %q missing Detector", f.Name)
			require.NotEmpty(t, f.Preferred, "family %q missing Preferred list", f.Name)
			require.NotEmpty(t, f.Dirs, "family %q missing Dirs", f.Name)
		})
	}
}

// TestFamilies_Immutable verifies that Families() returns a fresh copy
// on each call — mutating the returned slice must not leak into the
// package-level declaration, which would corrupt subsequent scans.
func TestFamilies_Immutable(t *testing.T) {
	t.Parallel()

	first := Families()
	first[0].Name = "MUTATED"

	second := Families()
	assert.Equal(t, "readme", second[0].Name,
		"Families() must return a fresh slice on each call")
}

// TestFamilies_CodeownersScansThreeDirs is a sanity check on the one
// family that scans more than the repo root. A regression here would
// silently miss CODEOWNERS files at their documented GitHub locations.
func TestFamilies_CodeownersScansThreeDirs(t *testing.T) {
	t.Parallel()

	for _, f := range Families() {
		if f.Name != "codeowners" {
			continue
		}
		assert.ElementsMatch(t, []string{".", ".github", "docs"}, f.Dirs,
			"codeowners must scan root, .github, and docs per GitHub's parser rules")
		return
	}
	t.Fatalf("codeowners family not declared")
}
