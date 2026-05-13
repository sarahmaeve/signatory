package deltas

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests model real attack shapes from design/threat-landscape/.
// Each (prior, current) pair is taken from a documented incident; the
// expected diff is what an analyst-layer or human-readable view should
// surface. See design/deltas.md §"Test scenarios modeled on adversary
// reports" for the source-of-truth list.

// ------------------------------------------------------------------
// Foundational behavior: empty inputs, no-change, scalar transitions
// ------------------------------------------------------------------

func TestDiff_Empty(t *testing.T) {
	t.Parallel()
	d := Diff(map[string]any{}, map[string]any{})
	assert.False(t, d.HasChanges(), "two empty maps must report no changes")
	assert.Empty(t, d.Added)
	assert.Empty(t, d.Removed)
	assert.Empty(t, d.Changed)
}

func TestDiff_NoChange(t *testing.T) {
	t.Parallel()
	prior := map[string]any{"present": true, "version_checked": "1.0.0"}
	current := map[string]any{"present": true, "version_checked": "1.0.0"}
	d := Diff(prior, current)
	assert.False(t, d.HasChanges(), "identical maps must report no changes")
}

func TestDiff_ScalarChange(t *testing.T) {
	t.Parallel()
	prior := map[string]any{"count": float64(5)}
	current := map[string]any{"count": float64(7)}
	d := Diff(prior, current)
	require.Contains(t, d.Changed, "count")
	change := d.Changed["count"]
	assert.Equal(t, ChangeKindScalar, change.Kind)
	assert.Equal(t, float64(5), change.Before)
	assert.Equal(t, float64(7), change.After)
}

func TestDiff_KeyAddedAndRemoved(t *testing.T) {
	t.Parallel()
	prior := map[string]any{"only_in_prior": "x"}
	current := map[string]any{"only_in_current": "y"}
	d := Diff(prior, current)
	assert.Equal(t, map[string]any{"only_in_current": "y"}, d.Added)
	assert.Equal(t, map[string]any{"only_in_prior": "x"}, d.Removed)
	assert.Empty(t, d.Changed)
}

// ------------------------------------------------------------------
// Adversary scenario 1: axios — trusted-publishing lost
// Source: design/threat-landscape/example-axios-attack.md
// ------------------------------------------------------------------

func TestDiff_AxiosTrustedPublishingLost(t *testing.T) {
	t.Parallel()
	prior := map[string]any{
		"present":           true,
		"version_checked":   "1.13.0",
		"publisher_kind":    "GitHub",
		"source_repository": "axios/axios",
		"workflow":          "release.yml",
	}
	current := map[string]any{
		"present":         false,
		"version_checked": "1.14.1",
	}
	d := Diff(prior, current)

	require.Contains(t, d.Changed, "present")
	require.Contains(t, d.Changed, "version_checked")
	assert.Equal(t, true, d.Changed["present"].Before)
	assert.Equal(t, false, d.Changed["present"].After)

	assert.Contains(t, d.Removed, "publisher_kind")
	assert.Contains(t, d.Removed, "source_repository")
	assert.Contains(t, d.Removed, "workflow")
	assert.Empty(t, d.Added)
}

// ------------------------------------------------------------------
// Adversary scenario 2: TanStack — unpublish gap after npm Security cleanup
// Source: design/threat-landscape/2026-05-12-tanstack-mini-shai-hulud.md
// ------------------------------------------------------------------

func TestDiff_TanStackUnpublishGapAppears(t *testing.T) {
	t.Parallel()
	prior := map[string]any{
		"unpublished_count":    float64(0),
		"unpublished_versions": []any{},
		"list_capped":          false,
	}
	current := map[string]any{
		"unpublished_count": float64(2),
		"unpublished_versions": []any{
			map[string]any{"version": "1.169.8", "published_at": "2026-05-11T19:26:17Z"},
			map[string]any{"version": "1.169.5", "published_at": "2026-05-11T19:20:42Z"},
		},
		"most_recent_unpublished_publish_time": "2026-05-11T19:26:17Z",
		"list_capped":                          false,
	}
	d := Diff(prior, current)

	// Scalar transition.
	require.Contains(t, d.Changed, "unpublished_count")
	assert.Equal(t, float64(0), d.Changed["unpublished_count"].Before)
	assert.Equal(t, float64(2), d.Changed["unpublished_count"].After)

	// Array of objects: stable-key alignment on "version" field.
	require.Contains(t, d.Changed, "unpublished_versions")
	arrayChange := d.Changed["unpublished_versions"]
	assert.Equal(t, ChangeKindArray, arrayChange.Kind)
	require.Len(t, arrayChange.Elements, 2,
		"both versions should appear as added entries")
	for _, el := range arrayChange.Elements {
		assert.Equal(t, ElementAdded, el.Kind)
		assert.Contains(t, []string{"1.169.5", "1.169.8"}, el.Key)
	}

	// New top-level key.
	assert.Contains(t, d.Added, "most_recent_unpublished_publish_time")

	// Unchanged keys must not surface.
	assert.NotContains(t, d.Changed, "list_capped")
}

// ------------------------------------------------------------------
// Adversary scenario 3: TanStack — cadence divergence post-incident
// Source: design/threat-landscape/2026-05-12-tanstack-mini-shai-hulud.md
// ------------------------------------------------------------------

func TestDiff_TanStackCadenceShifts(t *testing.T) {
	t.Parallel()
	prior := map[string]any{
		"commit_days_ago":  float64(1),
		"publish_days_ago": float64(0),
		"divergence_days":  float64(-1),
		"shape":            "synchronized",
	}
	current := map[string]any{
		"commit_days_ago":  float64(0),
		"publish_days_ago": float64(6),
		"divergence_days":  float64(6),
		"shape":            "active-repo-paused-publishes",
	}
	d := Diff(prior, current)

	for _, field := range []string{"commit_days_ago", "publish_days_ago", "divergence_days", "shape"} {
		require.Contains(t, d.Changed, field,
			"all four cadence fields should appear in Changed")
		assert.Equal(t, ChangeKindScalar, d.Changed[field].Kind)
	}
	assert.Equal(t, "synchronized", d.Changed["shape"].Before)
	assert.Equal(t, "active-repo-paused-publishes", d.Changed["shape"].After)
}

// ------------------------------------------------------------------
// Adversary scenario 4: tj-actions — tag rewrite (forward-looking)
// Source: design/threat-landscape/2025-03-14-tj-actions-changed-files.md
// ------------------------------------------------------------------

func TestDiff_TjActionsTagRewrite(t *testing.T) {
	t.Parallel()
	prior := map[string]any{
		"tag_name":       "v45.0.7",
		"current_sha":    "abc1234567890abcdef1234567890abcdef12345",
		"first_observed": true,
	}
	current := map[string]any{
		"tag_name":       "v45.0.7",
		"current_sha":    "0e58ed8671d6b60d0890c21b07f8835ace038e67",
		"first_observed": false,
	}
	d := Diff(prior, current)

	// The headline change: SHA flipped. The tj-actions attack
	// fingerprint.
	require.Contains(t, d.Changed, "current_sha")
	assert.Equal(t, ChangeKindScalar, d.Changed["current_sha"].Kind)

	// first_observed boolean transition.
	require.Contains(t, d.Changed, "first_observed")

	// tag_name itself is stable.
	assert.NotContains(t, d.Changed, "tag_name")
}

// ------------------------------------------------------------------
// Adversary scenario 5: TanStack careful-variant — workflow ref changed
// Source: design/threat-landscape/2026-05-12-tanstack-mini-shai-hulud.md
//          §"What this exposes as a gap"
// ------------------------------------------------------------------

func TestDiff_WorkflowRefChange(t *testing.T) {
	t.Parallel()
	prior := map[string]any{
		"consistent":               true,
		"workflow_refs":            []any{"pypi-publish.yml", "pypi-publish.yml", "pypi-publish.yml"},
		"latest_workflow_ref":      "pypi-publish.yml",
		"unique_workflow_refs":     float64(1),
		"workflow_ref_transitions": float64(0),
	}
	current := map[string]any{
		"consistent":               true,
		"workflow_refs":            []any{"release-v2.yml", "pypi-publish.yml", "pypi-publish.yml"},
		"latest_workflow_ref":      "release-v2.yml",
		"unique_workflow_refs":     float64(2),
		"workflow_ref_transitions": float64(1),
	}
	d := Diff(prior, current)

	// Same-length primitive array → positional diff.
	require.Contains(t, d.Changed, "workflow_refs")
	arrayChange := d.Changed["workflow_refs"]
	assert.Equal(t, ChangeKindArray, arrayChange.Kind)
	require.Len(t, arrayChange.Elements, 1,
		"only position 0 changed between the two workflow_refs arrays")
	assert.Equal(t, ElementChanged, arrayChange.Elements[0].Kind)
	assert.Equal(t, 0, arrayChange.Elements[0].Position)
	assert.Equal(t, "pypi-publish.yml", arrayChange.Elements[0].Before)
	assert.Equal(t, "release-v2.yml", arrayChange.Elements[0].After)

	// Three scalar transitions.
	assert.Contains(t, d.Changed, "latest_workflow_ref")
	assert.Contains(t, d.Changed, "unique_workflow_refs")
	assert.Contains(t, d.Changed, "workflow_ref_transitions")

	// consistent stayed true.
	assert.NotContains(t, d.Changed, "consistent")
}

// ------------------------------------------------------------------
// Adversary scenario 6: publisher_account_class — bot publisher appears
// Source: tj-actions @tj-actions-bot pattern, generalized.
// ------------------------------------------------------------------

func TestDiff_BotPublisherAppears(t *testing.T) {
	t.Parallel()
	prior := map[string]any{
		"logins": []any{
			map[string]any{"login": "alice", "class": "human"},
		},
		"total_count":     float64(1),
		"non_human_count": float64(0),
	}
	current := map[string]any{
		"logins": []any{
			map[string]any{"login": "alice", "class": "human"},
			map[string]any{
				"login":           "evil-publisher-bot",
				"class":           "service-account",
				"matched_pattern": "-bot",
			},
		},
		"total_count":     float64(2),
		"non_human_count": float64(1),
	}
	d := Diff(prior, current)

	// Array of objects: stable-key alignment on "login".
	require.Contains(t, d.Changed, "logins")
	arrayChange := d.Changed["logins"]
	assert.Equal(t, ChangeKindArray, arrayChange.Kind)
	require.Len(t, arrayChange.Elements, 1,
		"exactly one entry added (alice is unchanged)")
	assert.Equal(t, ElementAdded, arrayChange.Elements[0].Kind)
	assert.Equal(t, "evil-publisher-bot", arrayChange.Elements[0].Key)

	// Two scalar transitions.
	assert.Contains(t, d.Changed, "total_count")
	assert.Contains(t, d.Changed, "non_human_count")
}

// ------------------------------------------------------------------
// Adversary scenario 7: bufferzonecorp — version-burst flag flip
// Source: design/threat-landscape/2026-05-02-bufferzonecorp-campaign.md
// ------------------------------------------------------------------

func TestDiff_VersionBurstFlips(t *testing.T) {
	t.Parallel()
	prior := map[string]any{
		"burst_detected":     false,
		"versions_checked":   float64(5),
		"versions_in_window": float64(5),
		"window_hours":       float64(168),
	}
	current := map[string]any{
		"burst_detected":     true,
		"versions_checked":   float64(10),
		"versions_in_window": float64(10),
		"window_hours":       float64(72),
	}
	d := Diff(prior, current)
	require.Contains(t, d.Changed, "burst_detected")
	assert.Equal(t, false, d.Changed["burst_detected"].Before)
	assert.Equal(t, true, d.Changed["burst_detected"].After)
	for _, field := range []string{"versions_checked", "versions_in_window", "window_hours"} {
		assert.Contains(t, d.Changed, field)
	}
}

// ------------------------------------------------------------------
// Adversary scenario 8: maintainer churn — list addition
// Source: identity-surface-exposure pattern, parallels the
//         2026-04-21-vercel-contextai-incident.md framing.
// ------------------------------------------------------------------

func TestDiff_MaintainerChurnAdds(t *testing.T) {
	t.Parallel()
	prior := map[string]any{
		"count":  float64(2),
		"logins": []any{"alice", "bob"},
	}
	current := map[string]any{
		"count":  float64(3),
		"logins": []any{"alice", "bob", "newcomer"},
	}
	d := Diff(prior, current)

	require.Contains(t, d.Changed, "count")
	assert.Equal(t, float64(2), d.Changed["count"].Before)
	assert.Equal(t, float64(3), d.Changed["count"].After)

	// Different-length primitive array: shows up as a change. The
	// exact kind (ChangeKindArray with element list, vs Opaque) is
	// an implementation choice; the contract is that the diff
	// surfaces "newcomer" as added somehow.
	require.Contains(t, d.Changed, "logins")
}

// ------------------------------------------------------------------
// Object recursion: nested object change
// ------------------------------------------------------------------

func TestDiff_NestedObjectChange(t *testing.T) {
	t.Parallel()
	prior := map[string]any{
		"publisher": map[string]any{
			"kind":     "GitHub",
			"workflow": "release.yml",
		},
	}
	current := map[string]any{
		"publisher": map[string]any{
			"kind":     "GitHub",
			"workflow": "release-v2.yml",
		},
	}
	d := Diff(prior, current)

	require.Contains(t, d.Changed, "publisher")
	change := d.Changed["publisher"]
	assert.Equal(t, ChangeKindObject, change.Kind)
	require.NotNil(t, change.Nested)

	// Recursion exposes only the workflow change; kind is unchanged.
	assert.NotContains(t, change.Nested.Changed, "kind")
	require.Contains(t, change.Nested.Changed, "workflow")
	assert.Equal(t, "release.yml", change.Nested.Changed["workflow"].Before)
	assert.Equal(t, "release-v2.yml", change.Nested.Changed["workflow"].After)
}

// ------------------------------------------------------------------
// Edge case: type mismatch (string vs number on same key)
// ------------------------------------------------------------------

func TestDiff_TypeMismatchIsScalarChange(t *testing.T) {
	t.Parallel()
	// A signal that historically emitted strings but now emits
	// numbers (or vice versa) — render as scalar before/after.
	prior := map[string]any{"value": "5"}
	current := map[string]any{"value": float64(5)}
	d := Diff(prior, current)
	require.Contains(t, d.Changed, "value")
	assert.Equal(t, ChangeKindScalar, d.Changed["value"].Kind)
}

// ------------------------------------------------------------------
// Edge case: same-length primitive array, no positional change
// ------------------------------------------------------------------

func TestDiff_SameArrayNoChange(t *testing.T) {
	t.Parallel()
	prior := map[string]any{
		"workflow_refs": []any{"a", "b", "c"},
	}
	current := map[string]any{
		"workflow_refs": []any{"a", "b", "c"},
	}
	d := Diff(prior, current)
	assert.False(t, d.HasChanges(), "identical arrays must not surface")
}
