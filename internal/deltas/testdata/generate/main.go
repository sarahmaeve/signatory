// Command generate produces internal/deltas/testdata/sample.db — a
// programmatically-seeded SQLite database encoding the
// adversary-shaped scenarios from design/deltas.md §"Test scenarios
// modeled on adversary reports."
//
// Run with:
//
//	go run ./internal/deltas/testdata/generate
//
// The resulting sample.db is committed to the repo so e2e tests
// have a stable input. Regenerate when the test scenarios change
// or when a schema migration affects signal storage.
//
// The generator deliberately uses real signal-type names from
// internal/signal/types.go — the test DB exercises the full
// emission path, including the signal-type registry's panic-on-
// unregistered guard. Scenarios that depend on signals not yet
// shipped (e.g., tag_sha_mapping) are covered by the unit tests in
// internal/deltas/diff_test.go and excluded from this DB.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// dbPath is the committed location of the generated sample DB.
// Relative to the repo root.
const dbPath = "internal/deltas/testdata/sample.db"

// Fixed timestamps for deterministic-shape scenarios. Bytes will
// still differ run-to-run because entity IDs and signal IDs are
// random UUIDs — tests assert on URI and signal value, not on
// row IDs.
//
// t1/t2 are the two-observation timestamps used by most scenarios.
// t3/t4 extend the timeline for the range-window probe scenario —
// four observations let a range bracket a subset, a single point,
// or fall entirely outside the data.
var (
	t1 = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	t2 = time.Date(2026, 5, 12, 15, 55, 0, 0, time.UTC)
	t3 = time.Date(2026, 5, 20, 9, 0, 0, 0, time.UTC)
	t4 = time.Date(2026, 5, 25, 18, 0, 0, 0, time.UTC)
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "generate:", err)
		os.Exit(1)
	}
}

func run() error {
	// Delete any prior file so we get a clean schema.
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return fmt.Errorf("resolve dbPath: %w", err)
	}
	_ = os.Remove(abs) //nolint:errcheck // best-effort cleanup

	ctx := context.Background()
	s, err := store.OpenSQLite(ctx, abs)
	if err != nil {
		return fmt.Errorf("open store at %s: %w", abs, err)
	}
	defer s.Close() //nolint:errcheck

	scenarios := []func(context.Context, *store.SQLite) error{
		seedAxiosTrustedPublishingLost,
		seedTanStackUnpublishGap,
		seedTanStackCadenceShift,
		seedWorkflowRefChange,
		seedBotPublisherAppears,
		seedVersionBurstFlips,
		seedMaintainerChurn,
		seedNpmDependenciesAdded,
		seedCargoDependenciesAdded,
		seedRangeWindowProbe,
	}
	for _, seed := range scenarios {
		if err := seed(ctx, s); err != nil {
			return err
		}
	}

	fmt.Printf("wrote %s\n", abs)
	return nil
}

// mintEntity creates an entity row and returns its ID.
func mintEntity(ctx context.Context, s *store.SQLite, uri, shortName string, eco string) (string, error) {
	entity, _, err := s.EnsureEntityByCanonicalURI(ctx, uri, shortName)
	if err != nil {
		return "", fmt.Errorf("mint %s: %w", uri, err)
	}
	// Ecosystem is set on the entity row only when present in the
	// EnsureEntityByCanonicalURI path; some legacy paths leave it
	// empty. We don't update it here — tests query by URI which is
	// the load-bearing identifier.
	_ = eco
	return entity.ID, nil
}

// appendSignal builds a profile.Signal and writes it via the store
// interface. The ID convention follows the codebase's
// `{source}:{entity_id}:{type}:{collected_at_nanos}` shape.
func appendSignal(ctx context.Context, s *store.SQLite,
	entityID, sigType, source string, group profile.SignalGroup,
	resistance profile.ForgeryResistance, value map[string]any,
	collectedAt time.Time) error {

	valBytes, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal value for %s: %w", sigType, err)
	}
	sig := profile.Signal{
		ID:                fmt.Sprintf("%s:%s:%s:%d", source, entityID, sigType, collectedAt.UnixNano()),
		EntityID:          entityID,
		Type:              sigType,
		Group:             group,
		Source:            source,
		ForgeryResistance: resistance,
		Value:             valBytes,
		CollectedAt:       collectedAt,
		ExpiresAt:         collectedAt.Add(24 * time.Hour),
	}
	if err := s.AppendSignals(ctx, []profile.Signal{sig}); err != nil {
		return fmt.Errorf("append %s for %s: %w", sigType, entityID, err)
	}
	return nil
}

// --- Scenario 1: axios trusted-publishing lost ---

func seedAxiosTrustedPublishingLost(ctx context.Context, s *store.SQLite) error {
	id, err := mintEntity(ctx, s, "pkg:npm/axios", "axios", "npm")
	if err != nil {
		return err
	}
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
	if err := appendSignal(ctx, s, id, "trusted_publishing", "npm-registry",
		profile.SignalGroupPublication, profile.ForgeryHigh, prior, t1); err != nil {
		return err
	}
	return appendSignal(ctx, s, id, "trusted_publishing", "npm-registry",
		profile.SignalGroupPublication, profile.ForgeryHigh, current, t2)
}

// --- Scenario 2: TanStack unpublish gap appears ---

func seedTanStackUnpublishGap(ctx context.Context, s *store.SQLite) error {
	id, err := mintEntity(ctx, s, "pkg:npm/@tanstack/react-router", "@tanstack/react-router", "npm")
	if err != nil {
		return err
	}
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
	if err := appendSignal(ctx, s, id, "version_unpublish_observed", "npm-registry",
		profile.SignalGroupPublication, profile.ForgeryHigh, prior, t1); err != nil {
		return err
	}
	return appendSignal(ctx, s, id, "version_unpublish_observed", "npm-registry",
		profile.SignalGroupPublication, profile.ForgeryHigh, current, t2)
}

// --- Scenario 3: TanStack cadence shifts post-incident ---

func seedTanStackCadenceShift(ctx context.Context, s *store.SQLite) error {
	// Reuse the @tanstack/react-router entity created in scenario 2.
	id, err := mintEntity(ctx, s, "pkg:npm/@tanstack/react-router", "@tanstack/react-router", "npm")
	if err != nil {
		return err
	}
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
	if err := appendSignal(ctx, s, id, "commit_publish_cadence_divergence", "cadence",
		profile.SignalGroupVitality, profile.ForgeryMediumDeclining, prior, t1); err != nil {
		return err
	}
	return appendSignal(ctx, s, id, "commit_publish_cadence_divergence", "cadence",
		profile.SignalGroupVitality, profile.ForgeryMediumDeclining, current, t2)
}

// --- Scenario 5: workflow ref change (sketch 5 detection axis) ---

func seedWorkflowRefChange(ctx context.Context, s *store.SQLite) error {
	id, err := mintEntity(ctx, s, "pkg:pypi/sample-careful-variant", "sample-careful-variant", "pypi")
	if err != nil {
		return err
	}
	prior := map[string]any{
		"consistent":               true,
		"versions_checked":         float64(3),
		"versions_attested":        float64(3),
		"versions_unattested":      float64(0),
		"versions_skipped":         float64(0),
		"transition_detected":      false,
		"transition_direction":     "",
		"transition_at_version":    "",
		"publisher_changed":        false,
		"workflow_refs":            []any{"pypi-publish.yml", "pypi-publish.yml", "pypi-publish.yml"},
		"latest_workflow_ref":      "pypi-publish.yml",
		"unique_workflow_refs":     float64(1),
		"workflow_ref_transitions": float64(0),
	}
	current := map[string]any{
		"consistent":               true,
		"versions_checked":         float64(3),
		"versions_attested":        float64(3),
		"versions_unattested":      float64(0),
		"versions_skipped":         float64(0),
		"transition_detected":      false,
		"transition_direction":     "",
		"transition_at_version":    "",
		"publisher_changed":        false,
		"workflow_refs":            []any{"release-v2.yml", "pypi-publish.yml", "pypi-publish.yml"},
		"latest_workflow_ref":      "release-v2.yml",
		"unique_workflow_refs":     float64(2),
		"workflow_ref_transitions": float64(1),
	}
	if err := appendSignal(ctx, s, id, "attestation_consistency", "pypi-registry",
		profile.SignalGroupPublication, profile.ForgeryHigh, prior, t1); err != nil {
		return err
	}
	return appendSignal(ctx, s, id, "attestation_consistency", "pypi-registry",
		profile.SignalGroupPublication, profile.ForgeryHigh, current, t2)
}

// --- Scenario 6: bot publisher appears ---

func seedBotPublisherAppears(ctx context.Context, s *store.SQLite) error {
	id, err := mintEntity(ctx, s, "pkg:pypi/sample-bot-target", "sample-bot-target", "pypi")
	if err != nil {
		return err
	}
	prior := map[string]any{
		"logins": []any{
			map[string]any{"login": "alice", "class": "human", "reason": "no automation-naming pattern matched"},
		},
		"total_count":     float64(1),
		"non_human_count": float64(0),
	}
	current := map[string]any{
		"logins": []any{
			map[string]any{"login": "alice", "class": "human", "reason": "no automation-naming pattern matched"},
			map[string]any{
				"login":           "evil-publisher-bot",
				"class":           "service-account",
				"matched_pattern": "-bot",
				"reason":          "login ends with automation-account suffix \"-bot\"",
			},
		},
		"total_count":     float64(2),
		"non_human_count": float64(1),
	}
	if err := appendSignal(ctx, s, id, "publisher_account_class", "pypi-registry",
		profile.SignalGroupGovernance, profile.ForgeryMediumDeclining, prior, t1); err != nil {
		return err
	}
	return appendSignal(ctx, s, id, "publisher_account_class", "pypi-registry",
		profile.SignalGroupGovernance, profile.ForgeryMediumDeclining, current, t2)
}

// --- Scenario 7: bufferzonecorp version-burst flips ---

func seedVersionBurstFlips(ctx context.Context, s *store.SQLite) error {
	id, err := mintEntity(ctx, s, "pkg:npm/burst-shape-sample", "burst-shape-sample", "npm")
	if err != nil {
		return err
	}
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
	if err := appendSignal(ctx, s, id, "version_publish_burst", "npm-registry",
		profile.SignalGroupPublication, profile.ForgeryHigh, prior, t1); err != nil {
		return err
	}
	return appendSignal(ctx, s, id, "version_publish_burst", "npm-registry",
		profile.SignalGroupPublication, profile.ForgeryHigh, current, t2)
}

// --- Scenario 9: range-window probe ---
//
// Four observations of a single signal at t1, t2, t3, t4. Lets the
// e2e suite exercise --range bracketing: include all, include a
// subset, include exactly one observation (no transitions to render),
// and include none (empty-window output). The signal-type pick
// (maintainer_count) is arbitrary — any registered scalar-bearing
// type would do; this one already has a renderer-friendly shape.
func seedRangeWindowProbe(ctx context.Context, s *store.SQLite) error {
	id, err := mintEntity(ctx, s, "pkg:npm/range-window-probe", "range-window-probe", "npm")
	if err != nil {
		return err
	}
	values := []map[string]any{
		{"count": float64(1), "logins": []any{"alice"}},
		{"count": float64(2), "logins": []any{"alice", "bob"}},
		{"count": float64(3), "logins": []any{"alice", "bob", "carol"}},
		{"count": float64(4), "logins": []any{"alice", "bob", "carol", "dave"}},
	}
	timestamps := []time.Time{t1, t2, t3, t4}
	for i, v := range values {
		if err := appendSignal(ctx, s, id, "maintainer_count", "npm-registry",
			profile.SignalGroupGovernance, profile.ForgeryMediumDeclining, v, timestamps[i]); err != nil {
			return err
		}
	}
	return nil
}

// --- Scenario 8: maintainer churn ---

func seedMaintainerChurn(ctx context.Context, s *store.SQLite) error {
	id, err := mintEntity(ctx, s, "pkg:npm/maintainer-churn-sample", "maintainer-churn-sample", "npm")
	if err != nil {
		return err
	}
	prior := map[string]any{
		"count":  float64(2),
		"logins": []any{"alice", "bob"},
	}
	current := map[string]any{
		"count":  float64(3),
		"logins": []any{"alice", "bob", "newcomer"},
	}
	if err := appendSignal(ctx, s, id, "maintainer_count", "npm-registry",
		profile.SignalGroupGovernance, profile.ForgeryMediumDeclining, prior, t1); err != nil {
		return err
	}
	return appendSignal(ctx, s, id, "maintainer_count", "npm-registry",
		profile.SignalGroupGovernance, profile.ForgeryMediumDeclining, current, t2)
}

// --- Scenario 9: npm dependency added ---
//
// Two npm_dependencies observations whose `direct` array gains one
// entry. Exercises the dependency-drift path end to end through the
// real `signatory deltas` command — the transition the live dogfood
// could not produce because real packages did not change deps
// between observations.
func seedNpmDependenciesAdded(ctx context.Context, s *store.SQLite) error {
	id, err := mintEntity(ctx, s, "pkg:npm/dependency-added-sample", "dependency-added-sample", "npm")
	if err != nil {
		return err
	}
	prior := map[string]any{
		"direct_count":   float64(2),
		"indirect_count": float64(0),
		"total_count":    float64(2),
		"direct":         []any{"express", "lodash"},
	}
	current := map[string]any{
		"direct_count":   float64(3),
		"indirect_count": float64(0),
		"total_count":    float64(3),
		"direct":         []any{"express", "left-pad", "lodash"},
	}
	if err := appendSignal(ctx, s, id, "npm_dependencies", "npm-registry",
		profile.SignalGroupGovernance, profile.ForgeryHigh, prior, t1); err != nil {
		return err
	}
	return appendSignal(ctx, s, id, "npm_dependencies", "npm-registry",
		profile.SignalGroupGovernance, profile.ForgeryHigh, current, t2)
}

// --- Scenario 10: cargo dependency added ---
//
// Same shape as scenario 9 for the cargo signal, confirming the
// byte-identical value shape renders an identical transition through
// the CLI across ecosystems.
func seedCargoDependenciesAdded(ctx context.Context, s *store.SQLite) error {
	id, err := mintEntity(ctx, s, "pkg:cargo/dependency-added-sample", "dependency-added-sample", "cargo")
	if err != nil {
		return err
	}
	prior := map[string]any{
		"direct_count":   float64(2),
		"indirect_count": float64(0),
		"total_count":    float64(2),
		"direct":         []any{"libc", "mio"},
	}
	current := map[string]any{
		"direct_count":   float64(3),
		"indirect_count": float64(0),
		"total_count":    float64(3),
		"direct":         []any{"libc", "mio", "tokio-macros"},
	}
	if err := appendSignal(ctx, s, id, "cargo_dependencies", "cargo-registry",
		profile.SignalGroupGovernance, profile.ForgeryHigh, prior, t1); err != nil {
		return err
	}
	return appendSignal(ctx, s, id, "cargo_dependencies", "cargo-registry",
		profile.SignalGroupGovernance, profile.ForgeryHigh, current, t2)
}
