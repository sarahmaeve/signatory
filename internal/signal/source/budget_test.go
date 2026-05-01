package source

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================
// parseSemver
// ============================================================

func TestParseSemver_ValidForms(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in                  string
		major, minor, patch int
	}{
		{"v0.0.0", 0, 0, 0},
		{"v1.0.0", 1, 0, 0},
		{"v1.2.3", 1, 2, 3},
		{"v10.20.30", 10, 20, 30},
		{"v1.2.3-alpha", 1, 2, 3},
		{"v1.2.3-alpha.1", 1, 2, 3},
		{"v1.2.3+build", 1, 2, 3},
		{"v1.2.3-alpha+build", 1, 2, 3},
		{"v0.0.0-20260415120000-abc123def456", 0, 0, 0}, // pseudo-version
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			major, minor, patch, ok := parseSemver(tc.in)
			require.True(t, ok, "expected valid")
			assert.Equal(t, tc.major, major)
			assert.Equal(t, tc.minor, minor)
			assert.Equal(t, tc.patch, patch)
		})
	}
}

func TestParseSemver_InvalidForms(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"v",
		"v1",
		"v1.2",
		"v1.2.3.4",
		"1.2.3",
		"v1.2.x",
		"main",
		"develop",
		"refs/tags/v1.0.0",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			_, _, _, ok := parseSemver(in)
			assert.False(t, ok)
		})
	}
}

// ============================================================
// majorOf
// ============================================================

func TestMajorOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, out string
	}{
		{"v0.1.2", "v0"},
		{"v1.2.3", "v1"},
		{"v10.0.0", "v10"},
		{"v0.0.0-alpha", "v0"},
		{"v1.2.3+build", "v1"},
		{"main", ""},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.out, majorOf(tc.in))
		})
	}
}

// ============================================================
// compareSemver
// ============================================================

func TestCompareSemver_OrderingByComponent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		want int // sign-only: -1, 0, +1
	}{
		{"v1.0.0", "v1.0.0", 0},
		{"v1.0.0", "v2.0.0", -1},
		{"v2.0.0", "v1.0.0", 1},
		{"v1.0.0", "v1.1.0", -1},
		{"v1.1.0", "v1.0.0", 1},
		{"v1.0.0", "v1.0.1", -1},
		{"v1.0.1", "v1.0.0", 1},
		// The "v10 < v2 lexicographically" gotcha — semver wins.
		{"v2.0.0", "v10.0.0", -1},
		{"v10.0.0", "v2.0.0", 1},
		// Pre-release and build are ignored for ordering.
		{"v1.0.0-alpha", "v1.0.0", 0},
		{"v1.0.0+build", "v1.0.0", 0},
		{"v1.0.0-alpha", "v1.0.0+build", 0},
	}
	for _, tc := range cases {
		t.Run(tc.a+"_vs_"+tc.b, func(t *testing.T) {
			t.Parallel()
			got := compareSemver(tc.a, tc.b)
			switch {
			case tc.want > 0:
				assert.Positive(t, got)
			case tc.want < 0:
				assert.Negative(t, got)
			default:
				assert.Zero(t, got)
			}
		})
	}
}

func TestCompareSemver_InvalidSortsToEnd(t *testing.T) {
	t.Parallel()
	// Valid < invalid in ascending order (so invalid bubbles to the
	// end; reverse-sort puts invalid at the front of "most recent").
	assert.Negative(t, compareSemver("v1.0.0", "main"))
	assert.Positive(t, compareSemver("main", "v1.0.0"))
}

func TestCompareSemver_TwoInvalidsCompareLexicographically(t *testing.T) {
	t.Parallel()
	got := compareSemver("alpha", "beta")
	assert.Equal(t, strings.Compare("alpha", "beta"), got)
}

// ============================================================
// sortSemverDesc
// ============================================================

func TestSortSemverDesc_DescendingOrder(t *testing.T) {
	t.Parallel()
	in := []string{"v0.1.0", "v0.10.0", "v0.2.0", "v1.0.0", "v0.9.0"}
	want := []string{"v1.0.0", "v0.10.0", "v0.9.0", "v0.2.0", "v0.1.0"}
	got := sortSemverDesc(in)
	assert.Equal(t, want, got)
}

func TestSortSemverDesc_DoesNotMutateInput(t *testing.T) {
	t.Parallel()
	in := []string{"v0.2.0", "v1.0.0", "v0.1.0"}
	inCopy := slices.Clone(in)
	_ = sortSemverDesc(in)
	assert.Equal(t, inCopy, in, "input slice must not be mutated")
}

func TestSortSemverDesc_InvalidsAtEnd(t *testing.T) {
	t.Parallel()
	in := []string{"main", "v0.1.0", "develop", "v1.0.0"}
	got := sortSemverDesc(in)
	require.Len(t, got, 4)
	// Valid versions sort first (descending).
	assert.Equal(t, "v1.0.0", got[0])
	assert.Equal(t, "v0.1.0", got[1])
	// Invalid versions cluster at the tail.
	assert.ElementsMatch(t, []string{"main", "develop"}, got[2:])
}

func TestSortSemverDesc_EmptyAndSingle(t *testing.T) {
	t.Parallel()
	assert.Empty(t, sortSemverDesc(nil))
	assert.Empty(t, sortSemverDesc([]string{}))
	assert.Equal(t, []string{"v1.0.0"}, sortSemverDesc([]string{"v1.0.0"}))
}

// ============================================================
// Select — happy paths
// ============================================================

func TestSelect_EmptyInput_EmptySelection(t *testing.T) {
	t.Parallel()
	sel := Select(nil, BudgetOpts{})
	assert.Empty(t, sel.SelectedVersions)
	assert.Empty(t, sel.SkippedVersions)
	assert.Equal(t, SelectionStrategyV1, sel.SelectionStrategy)
	assert.Equal(t, 12, sel.LastN)
	assert.Equal(t, 4, sel.MajorLeaves)
}

func TestSelect_FewerThanLastN_SelectsAll(t *testing.T) {
	t.Parallel()
	versions := []string{"v0.1.0", "v0.2.0", "v0.3.0"}
	sel := Select(versions, BudgetOpts{})
	assert.Equal(t, []string{"v0.3.0", "v0.2.0", "v0.1.0"}, sel.SelectedVersions)
	assert.Empty(t, sel.SkippedVersions)
}

func TestSelect_LastNOnly_PicksMostRecentN(t *testing.T) {
	t.Parallel()
	versions := []string{
		"v0.1.0", "v0.2.0", "v0.3.0", "v0.4.0", "v0.5.0",
		"v0.6.0", "v0.7.0", "v0.8.0", "v0.9.0", "v0.10.0",
		"v0.11.0", "v0.12.0", "v0.13.0", "v0.14.0", "v0.15.0",
	}
	sel := Select(versions, BudgetOpts{LastN: 12, MajorLeaves: 0, HardCap: 20})
	require.Len(t, sel.SelectedVersions, 12)
	assert.Equal(t, "v0.15.0", sel.SelectedVersions[0])
	assert.Equal(t, "v0.4.0", sel.SelectedVersions[11])
	assert.Equal(t, []string{"v0.3.0", "v0.2.0", "v0.1.0"}, sel.SkippedVersions)
}

func TestSelect_MajorLeaves_AddHighestPerOlderMajor(t *testing.T) {
	t.Parallel()
	versions := []string{
		// v2 line (most recent) — fills the recent core
		"v2.0.0", "v2.1.0", "v2.2.0",
		// v1 line — leaf candidate
		"v1.0.0", "v1.5.0", "v1.9.0",
		// v0 line — leaf candidate
		"v0.1.0", "v0.5.0", "v0.9.0",
	}
	sel := Select(versions, BudgetOpts{LastN: 3, MajorLeaves: 4, HardCap: 20})

	// Recent core: v2.2, v2.1, v2.0. Leaves: v1.9 (highest of v1),
	// v0.9 (highest of v0). v1.5/v1.0 not selected (leaf is highest
	// per major); v0.5/v0.1 not selected (same).
	require.Len(t, sel.SelectedVersions, 5)
	assert.Contains(t, sel.SelectedVersions, "v2.2.0")
	assert.Contains(t, sel.SelectedVersions, "v2.1.0")
	assert.Contains(t, sel.SelectedVersions, "v2.0.0")
	assert.Contains(t, sel.SelectedVersions, "v1.9.0")
	assert.Contains(t, sel.SelectedVersions, "v0.9.0")
	assert.NotContains(t, sel.SelectedVersions, "v1.5.0")
	assert.NotContains(t, sel.SelectedVersions, "v1.0.0")
	assert.NotContains(t, sel.SelectedVersions, "v0.5.0")
	assert.NotContains(t, sel.SelectedVersions, "v0.1.0")

	// Result is sorted descending.
	assert.Equal(t, "v2.2.0", sel.SelectedVersions[0])
	assert.Equal(t, "v0.9.0", sel.SelectedVersions[4])
}

func TestSelect_MajorAlreadyInCore_NotDuplicatedAsLeaf(t *testing.T) {
	t.Parallel()
	// LastN=3 picks v1.0, v0.5, v0.4 — v0 is in the core. v0.x leaves
	// must NOT be added since v0 is already represented.
	versions := []string{"v1.0.0", "v0.5.0", "v0.4.0", "v0.3.0", "v0.2.0", "v0.1.0"}
	sel := Select(versions, BudgetOpts{LastN: 3, MajorLeaves: 4, HardCap: 20})
	assert.Len(t, sel.SelectedVersions, 3)
	assert.Equal(t, []string{"v1.0.0", "v0.5.0", "v0.4.0"}, sel.SelectedVersions)
}

func TestSelect_HardCap_ClipsToTotal(t *testing.T) {
	t.Parallel()
	versions := []string{
		"v2.0.0", "v2.1.0", "v2.2.0",
		"v1.0.0", "v1.5.0",
		"v0.1.0", "v0.5.0",
	}
	sel := Select(versions, BudgetOpts{LastN: 5, MajorLeaves: 4, HardCap: 3})
	assert.Len(t, sel.SelectedVersions, 3)
	// HardCap clips the end (oldest first); top 3 by recency stay.
	assert.Equal(t, "v2.2.0", sel.SelectedVersions[0])
	assert.Equal(t, "v2.1.0", sel.SelectedVersions[1])
	assert.Equal(t, "v2.0.0", sel.SelectedVersions[2])
}

func TestSelect_HardCapSmallerThanLastN_ClipsBoth(t *testing.T) {
	t.Parallel()
	versions := []string{
		"v0.1.0", "v0.2.0", "v0.3.0", "v0.4.0", "v0.5.0",
	}
	sel := Select(versions, BudgetOpts{LastN: 10, MajorLeaves: 4, HardCap: 3})
	assert.Len(t, sel.SelectedVersions, 3)
	assert.Equal(t, "v0.5.0", sel.SelectedVersions[0])
	assert.Equal(t, "v0.3.0", sel.SelectedVersions[2])
}

// ============================================================
// Select — edge cases
// ============================================================

func TestSelect_DefaultsApplyForZeroOpts(t *testing.T) {
	t.Parallel()
	sel := Select([]string{"v0.1.0"}, BudgetOpts{})
	assert.Equal(t, 12, sel.LastN)
	assert.Equal(t, 4, sel.MajorLeaves)
	assert.Equal(t, SelectionStrategyV1, sel.SelectionStrategy)
}

func TestSelect_NegativeOptsTreatedAsDefault(t *testing.T) {
	t.Parallel()
	sel := Select([]string{"v0.1.0"}, BudgetOpts{LastN: -5, MajorLeaves: -1, HardCap: -1})
	assert.Equal(t, 12, sel.LastN)
	assert.Equal(t, 4, sel.MajorLeaves)
}

func TestSelect_InvalidSemversTolerated(t *testing.T) {
	t.Parallel()
	versions := []string{"v0.1.0", "main", "v0.2.0", "develop"}
	sel := Select(versions, BudgetOpts{LastN: 12, MajorLeaves: 0, HardCap: 20})
	require.Len(t, sel.SelectedVersions, 4)
	// Valid semvers come first in descending order.
	assert.Equal(t, "v0.2.0", sel.SelectedVersions[0])
	assert.Equal(t, "v0.1.0", sel.SelectedVersions[1])
	// Invalid semvers cluster at the end.
	assert.ElementsMatch(t, []string{"main", "develop"}, sel.SelectedVersions[2:])
}

func TestSelect_SkippedListPreservesSortedOrder(t *testing.T) {
	t.Parallel()
	versions := []string{"v0.1.0", "v0.2.0", "v0.3.0", "v0.4.0", "v0.5.0"}
	sel := Select(versions, BudgetOpts{LastN: 2, MajorLeaves: 0, HardCap: 20})
	assert.Equal(t, []string{"v0.5.0", "v0.4.0"}, sel.SelectedVersions)
	// Skipped is sorted-by-recency: v0.3.0, v0.2.0, v0.1.0.
	assert.Equal(t, []string{"v0.3.0", "v0.2.0", "v0.1.0"}, sel.SkippedVersions)
}

func TestSelect_DescendingOrderInOutput(t *testing.T) {
	t.Parallel()
	// Input in arbitrary order; output should be sorted descending.
	versions := []string{"v0.2.0", "v1.0.0", "v0.10.0", "v0.1.0"}
	sel := Select(versions, BudgetOpts{LastN: 12, MajorLeaves: 4, HardCap: 20})
	assert.Equal(t, []string{"v1.0.0", "v0.10.0", "v0.2.0", "v0.1.0"}, sel.SelectedVersions)
}

func TestSelect_CrossMajorWithLeafCapHit_StopsAtMajorLeaves(t *testing.T) {
	t.Parallel()
	// 5 majors total; LastN=2 puts v4 line in core; MajorLeaves=2
	// limits how many older major leaves we add.
	versions := []string{
		"v4.0.0", "v4.1.0", // core (LastN=2)
		"v3.0.0", "v3.5.0",
		"v2.0.0", "v2.5.0",
		"v1.0.0", "v1.5.0",
		"v0.0.0", "v0.5.0",
	}
	sel := Select(versions, BudgetOpts{LastN: 2, MajorLeaves: 2, HardCap: 20})
	// Core: v4.1, v4.0. Leaves: v3.5, v2.5 (only 2 added).
	require.Len(t, sel.SelectedVersions, 4)
	assert.Contains(t, sel.SelectedVersions, "v4.1.0")
	assert.Contains(t, sel.SelectedVersions, "v4.0.0")
	assert.Contains(t, sel.SelectedVersions, "v3.5.0")
	assert.Contains(t, sel.SelectedVersions, "v2.5.0")
	// v1 and v0 majors don't fit within MajorLeaves=2.
	assert.NotContains(t, sel.SelectedVersions, "v1.5.0")
	assert.NotContains(t, sel.SelectedVersions, "v0.5.0")
}

func TestSelect_OnlyOneMajor_NoLeavesAdded(t *testing.T) {
	t.Parallel()
	versions := []string{"v0.1.0", "v0.2.0", "v0.3.0", "v0.4.0", "v0.5.0"}
	sel := Select(versions, BudgetOpts{LastN: 2, MajorLeaves: 4, HardCap: 20})
	// Core: v0.5, v0.4. v0 is in core; no other majors exist.
	assert.Equal(t, []string{"v0.5.0", "v0.4.0"}, sel.SelectedVersions)
}
