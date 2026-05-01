package source

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/sarahmaeve/signatory/internal/signal/source/golang"
)

// row is a tiny test helper — builds a MatrixRow with status=
// present and an AST inline. version is the only required field;
// AST defaults to all zeros.
func row(version string, feats golang.Features) MatrixRow {
	return MatrixRow{
		Version:           version,
		TagSHALocalStatus: TagSHALocalPresent,
		AST:               &feats,
	}
}

// nullRow is a MatrixRow with nil AST (e.g., missing-from-clone).
// Used to test that anomaly detection skips pairs with no
// analyzable AST on either side.
func nullRow(version, status string) MatrixRow {
	return MatrixRow{
		Version:           version,
		TagSHALocalStatus: status,
	}
}

// ============================================================
// Threshold semantics
// ============================================================

func TestDetectAnomaly_FewerThanTwoRows_NoAnomaly(t *testing.T) {
	t.Parallel()
	assert.Equal(t, AnomalyValue{}, DetectAnomaly(nil))
	assert.Equal(t, AnomalyValue{}, DetectAnomaly([]MatrixRow{
		row("v0.1.0", golang.Features{}),
	}))
}

func TestDetectAnomaly_FlatBaseline_NoAnomaly(t *testing.T) {
	t.Parallel()
	rows := []MatrixRow{
		row("v0.3.0", golang.Features{}),
		row("v0.2.0", golang.Features{}),
		row("v0.1.0", golang.Features{}),
	}
	got := DetectAnomaly(rows)
	assert.False(t, got.AnomalyPresent)
}

func TestDetectAnomaly_SingleFeatureCrosses_BelowThreshold(t *testing.T) {
	t.Parallel()
	// Only one feature crosses zero — under the joint threshold
	// of 2. A network library legitimately adding its first
	// network call should NOT fire the anomaly.
	rows := []MatrixRow{
		row("v0.2.0", golang.Features{NetworkCallSites: 3}),
		row("v0.1.0", golang.Features{}),
	}
	got := DetectAnomaly(rows)
	assert.False(t, got.AnomalyPresent)
	assert.Empty(t, got.SpikedFeatures)
}

func TestDetectAnomaly_TwoFeaturesCross_FiresAnomaly(t *testing.T) {
	t.Parallel()
	rows := []MatrixRow{
		row("v0.2.0", golang.Features{InitCount: 1, NetworkCallSites: 3}),
		row("v0.1.0", golang.Features{}),
	}
	got := DetectAnomaly(rows)
	assert.True(t, got.AnomalyPresent)
	assert.Equal(t, "v0.2.0", got.FirstAnomalousVersion)
	assert.Equal(t, "v0.1.0", got.PreviousVersion)
	assert.ElementsMatch(t, []string{"init_count", "network_call_sites"}, got.SpikedFeatures)
}

func TestDetectAnomaly_BufferZoneCorpFingerprint_AllFeaturesSpike(t *testing.T) {
	t.Parallel()
	// All six features cross zero — the BufferZoneCorp grpc-client
	// init payload fingerprint. Anomaly fires; SpikedFeatures
	// enumerates all six in canonical order.
	rows := []MatrixRow{
		row("v0.2.0", golang.Features{
			InitCount:          1,
			NetworkCallSites:   1,
			SensitivePathReads: 8,
			ExecCalls:          1,
			XORAssignments:     1,
			Base64DecodeCalls:  1,
		}),
		row("v0.1.0", golang.Features{}),
	}
	got := DetectAnomaly(rows)
	assert.True(t, got.AnomalyPresent)
	assert.Equal(t, "v0.2.0", got.FirstAnomalousVersion)
	assert.Equal(t, []string{
		"init_count",
		"network_call_sites",
		"sensitive_path_reads",
		"exec_calls",
		"xor_assignments",
		"base64_decode_calls",
	}, got.SpikedFeatures, "SpikedFeatures order must match canonical Features field declaration order")
}

// ============================================================
// "Crossing" semantics
// ============================================================

func TestDetectAnomaly_StableHighBaseline_NoAnomaly(t *testing.T) {
	t.Parallel()
	// Every version has the same legitimate non-zero feature
	// counts. Nothing CROSSES from zero, so no anomaly. This is
	// the "library that has had network calls since v0.1.0"
	// scenario the threshold is designed not to false-positive on.
	feats := golang.Features{NetworkCallSites: 3, InitCount: 1}
	rows := []MatrixRow{
		row("v0.3.0", feats),
		row("v0.2.0", feats),
		row("v0.1.0", feats),
	}
	got := DetectAnomaly(rows)
	assert.False(t, got.AnomalyPresent)
}

func TestDetectAnomaly_FeatureGrowsButWasNonzero_NoAnomaly(t *testing.T) {
	t.Parallel()
	// Older row has 1 network call; newer has 5. NOT a zero
	// crossing — both are non-zero. v0.1 strict-zero threshold:
	// no anomaly, even though count grew significantly.
	rows := []MatrixRow{
		row("v0.2.0", golang.Features{InitCount: 2, NetworkCallSites: 5}),
		row("v0.1.0", golang.Features{InitCount: 1, NetworkCallSites: 1}),
	}
	got := DetectAnomaly(rows)
	assert.False(t, got.AnomalyPresent)
}

func TestDetectAnomaly_GradualGrowth_NoAnomaly(t *testing.T) {
	t.Parallel()
	// One feature grows per version, never multi-jointly within
	// a single pair. Validates the pair-by-pair threshold: each
	// individual transition has only one spike, so anomaly stays
	// false even though many features eventually become non-zero.
	rows := []MatrixRow{
		// v0.4.0: adds sensitive-path on top of network and exec
		row("v0.4.0", golang.Features{NetworkCallSites: 1, ExecCalls: 1, SensitivePathReads: 2}),
		// v0.3.0: adds exec on top of network
		row("v0.3.0", golang.Features{NetworkCallSites: 1, ExecCalls: 1}),
		// v0.2.0: adds first network call
		row("v0.2.0", golang.Features{NetworkCallSites: 1}),
		// v0.1.0: clean
		row("v0.1.0", golang.Features{}),
	}
	got := DetectAnomaly(rows)
	assert.False(t, got.AnomalyPresent)
}

// ============================================================
// Chronological ordering
// ============================================================

func TestDetectAnomaly_FirstAnomalousVersionIsOldestSpikePair(t *testing.T) {
	t.Parallel()
	// Two anomalous pairs in the matrix: v0.2.0 vs v0.1.0
	// (init+network appear) AND v0.5.0 vs v0.4.0 (exec+xor
	// appear). FirstAnomalousVersion must be v0.2.0 — the
	// chronologically earliest spike — not v0.5.0.
	rows := []MatrixRow{
		row("v0.5.0", golang.Features{
			InitCount: 1, NetworkCallSites: 1, ExecCalls: 1, XORAssignments: 1,
		}),
		row("v0.4.0", golang.Features{
			InitCount: 1, NetworkCallSites: 1,
		}),
		row("v0.3.0", golang.Features{
			InitCount: 1, NetworkCallSites: 1,
		}),
		row("v0.2.0", golang.Features{
			InitCount: 1, NetworkCallSites: 1,
		}),
		row("v0.1.0", golang.Features{}),
	}
	got := DetectAnomaly(rows)
	assert.True(t, got.AnomalyPresent)
	assert.Equal(t, "v0.2.0", got.FirstAnomalousVersion,
		"chronologically first spike pair must win, not the most recent")
	assert.Equal(t, "v0.1.0", got.PreviousVersion)
	assert.ElementsMatch(t, []string{"init_count", "network_call_sites"}, got.SpikedFeatures)
}

// ============================================================
// Skip behavior on partial rows
// ============================================================

func TestDetectAnomaly_NullASTSkipped(t *testing.T) {
	t.Parallel()
	// Older row is missing-from-clone (nil AST). Newer row has
	// features that crossed-from-zero relative to a hypothetical
	// older — but we can't compare against nil. The pair is
	// skipped; if no other anomaly exists, AnomalyPresent stays
	// false.
	rows := []MatrixRow{
		row("v0.2.0", golang.Features{InitCount: 1, NetworkCallSites: 1}),
		nullRow("v0.1.0", TagSHALocalMissingFromClone),
	}
	got := DetectAnomaly(rows)
	assert.False(t, got.AnomalyPresent)
}

func TestDetectAnomaly_NullPairSkipped_SubsequentPairStillEvaluated(t *testing.T) {
	t.Parallel()
	// Pair v0.3.0/v0.2.0: v0.2.0 is missing-from-clone. Pair
	// skipped.
	// Pair v0.2.0/v0.1.0: v0.2.0 is missing-from-clone. Pair
	// skipped.
	// No analyzable pairs → no anomaly even though v0.3.0 has
	// features.
	rows := []MatrixRow{
		row("v0.3.0", golang.Features{InitCount: 1, NetworkCallSites: 1}),
		nullRow("v0.2.0", TagSHALocalMissingFromClone),
		row("v0.1.0", golang.Features{}),
	}
	got := DetectAnomaly(rows)
	assert.False(t, got.AnomalyPresent)
}

func TestDetectAnomaly_PartialMatrixSpikesInGoodPair(t *testing.T) {
	t.Parallel()
	// v0.3.0 → v0.2.0 is a clean pair with anomaly.
	// v0.1.0 is missing-from-clone, but that pair is downstream
	// (chronologically older) and skipped. Anomaly still fires
	// at v0.3.0 vs v0.2.0.
	rows := []MatrixRow{
		row("v0.3.0", golang.Features{InitCount: 1, NetworkCallSites: 1}),
		row("v0.2.0", golang.Features{}),
		nullRow("v0.1.0", TagSHALocalMissingOrigin),
	}
	got := DetectAnomaly(rows)
	assert.True(t, got.AnomalyPresent)
	assert.Equal(t, "v0.3.0", got.FirstAnomalousVersion)
	assert.Equal(t, "v0.2.0", got.PreviousVersion)
}

// ============================================================
// Constants and exported API
// ============================================================

func TestMinSpikedFeatures_Is2(t *testing.T) {
	t.Parallel()
	// Pinned by test so a future tuning change requires explicit
	// review. The threshold value has design-doc semantic
	// implications (false-negative-heavy by design).
	assert.Equal(t, 2, MinSpikedFeatures)
}
