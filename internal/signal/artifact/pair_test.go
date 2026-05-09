package artifact

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestResolvePair_ExactGitHeadWins verifies the high-confidence
// path: when the registry payload carries a gitHead (npm v≥5
// records the commit SHA the publisher minted from), use it
// directly without consulting the tag list. This is the strongest
// pairing signatory can get and the synthesist should weight it
// accordingly.
func TestResolvePair_ExactGitHeadWins(t *testing.T) {
	res, ok := ResolvePair(PairInputs{
		Version: "5.6.1",
		GitHead: "deadbeefcafebabe1234567890abcdef12345678",
		Tags:    []string{"v5.6.0", "v5.6.1"},
	})
	assert.True(t, ok, "gitHead present must yield a successful resolution")
	assert.Equal(t, "deadbeefcafebabe1234567890abcdef12345678", res.Commit)
	assert.Equal(t, PairConfidenceExactGitHead, res.Confidence,
		"a publisher-stamped gitHead is the strongest pairing — it must "+
			"surface as exact_gitHead, not get downgraded by tag-list "+
			"reconciliation")
}

// TestResolvePair_TagMatchWhenNoGitHead verifies the medium-
// confidence fallback: no gitHead from the registry, but the
// repo has a tag whose name matches the version. Try both
// "vX.Y.Z" and bare "X.Y.Z" forms — autotools projects, npm
// older releases, and PyPI sdist all sit somewhere in this
// space.
func TestResolvePair_TagMatchWhenNoGitHead(t *testing.T) {
	cases := []struct {
		name    string
		version string
		tags    []string
		wantRef string
	}{
		{
			name:    "v-prefixed tag",
			version: "5.6.1",
			tags:    []string{"v5.6.0", "v5.6.1", "v5.6.2"},
			wantRef: "v5.6.1",
		},
		{
			name:    "bare-version tag",
			version: "1.2.3",
			tags:    []string{"1.2.2", "1.2.3"},
			wantRef: "1.2.3",
		},
		{
			name:    "v-prefix preferred over bare when both exist",
			version: "1.0.0",
			tags:    []string{"1.0.0", "v1.0.0"},
			wantRef: "v1.0.0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, ok := ResolvePair(PairInputs{
				Version: tc.version,
				GitHead: "",
				Tags:    tc.tags,
			})
			assert.True(t, ok, "matching tag must yield a successful resolution")
			assert.Equal(t, tc.wantRef, res.Ref)
			assert.Equal(t, PairConfidenceTagMatch, res.Confidence)
			assert.Empty(t, res.Commit,
				"tag-match returns the ref name only; commit-SHA lookup "+
					"is the caller's responsibility (it has the clone)")
		})
	}
}

// TestResolvePair_UnresolvedYieldsAbsenceReason is the failure
// case Test 6 in the design plan covers: registry payload
// without a gitHead and with no tag in the repo matching the
// version. ResolvePair returns ok=false and an AbsenceReason
// the collector turns into a positive_absence row.
//
// The reason string is part of the wire contract — the
// synthesist reads it to distinguish "we couldn't pair" (a
// hygiene observation about the project's release process)
// from "we tried and the artifact diverged" (the actual
// signal). Pinning the exact string here keeps that contract
// from drifting silently.
func TestResolvePair_UnresolvedYieldsAbsenceReason(t *testing.T) {
	res, ok := ResolvePair(PairInputs{
		Version: "5.6.1",
		GitHead: "",
		// Tag list deliberately doesn't contain v5.6.1 / 5.6.1.
		// This is the shape an autotools project or an older npm
		// release with no gitHead AND a non-canonical tag scheme
		// produces.
		Tags: []string{"release-5.6.0", "release-5.6.1"},
	})
	assert.False(t, ok,
		"no gitHead and no version-tag match must fail resolution")
	assert.Equal(t, PairConfidenceUnresolved, res.Confidence)
	assert.Equal(t, AbsenceReasonPairUnresolved, res.AbsenceReason,
		"the absence reason is the wire contract the collector emits — "+
			"changing it without coordinating with the synthesist's prompt "+
			"breaks downstream interpretation")
}
