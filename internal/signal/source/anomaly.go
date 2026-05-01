package source

import "github.com/sarahmaeve/signatory/internal/signal/source/golang"

// AnomalyValue is the JSON-marshaled value of the
// source_evolution_anomaly signal. Boolean+pointer summary derived
// from the matrix rows: AnomalyPresent indicates whether some pair
// of consecutive selected versions exhibits the multi-feature joint
// spike pattern (design/coll7.md D9), and the optional fields name
// where it happened and which features spiked.
//
// Empty (zero) value means "no anomaly" — omitempty on the
// optional fields keeps the JSON terse for the common case where
// the matrix is benign.
type AnomalyValue struct {
	// AnomalyPresent is true iff DetectAnomaly found a pair where
	// >= MinSpikedFeatures features crossed from zero to non-zero
	// between the older and newer row.
	AnomalyPresent bool `json:"anomaly_present"`

	// FirstAnomalousVersion is the *newer* version of the
	// oldest pair where the spike was detected. The convention
	// is "the version where the spike first manifests" —
	// chronologically the earliest row that introduces the
	// joint feature change.
	FirstAnomalousVersion string `json:"first_anomalous_version,omitempty"`

	// PreviousVersion is the older version paired against
	// FirstAnomalousVersion. Recorded so the analyst can
	// reconstruct the exact pair without re-walking the matrix.
	PreviousVersion string `json:"previous_version,omitempty"`

	// SpikedFeatures lists the snake_case feature names that
	// crossed from zero in PreviousVersion to non-zero in
	// FirstAnomalousVersion. Sorted by appearance in the
	// canonical golang.Features field order so output is stable.
	SpikedFeatures []string `json:"spiked_features,omitempty"`
}

// MinSpikedFeatures is the threshold for triggering an anomaly:
// at least this many features must cross from zero baseline in
// the same version pair. Conservative (false-negative-heavy by
// design) — false negatives are recoverable because the matrix
// itself stays in the handoff and the analyst can still notice;
// false positives erode analyst trust in the boolean.
//
// Lifted as a public constant so a future tuning experiment
// (different threshold for different posture decisions) can flip
// this value without surgery elsewhere. v0.1: 2.
const MinSpikedFeatures = 2

// DetectAnomaly walks the matrix rows (sorted semver-descending,
// most-recent first) and returns the AnomalyValue summarizing the
// FIRST chronological pair where >= MinSpikedFeatures features
// crossed from zero in the older row to non-zero in the newer
// row.
//
// "First chronological" = oldest pair where the spike happens.
// This matches the analyst's question — "when did this start?" —
// rather than "what's the most recent spike?"
//
// Returns the zero AnomalyValue (AnomalyPresent=false, optional
// fields omitted in JSON) when:
//   - rows has fewer than 2 entries
//   - all rows have nil AST (no analyzable versions)
//   - no pair exhibits the threshold-meeting joint spike
//
// Pairs where either row's AST is nil (missing-from-clone,
// missing-origin, fetch-failed) are skipped — we can't compare
// against features we don't have. The result still uses
// "chronologically first analyzable pair where the spike
// occurred" as the anomaly's FirstAnomalousVersion.
func DetectAnomaly(rows []MatrixRow) AnomalyValue {
	if len(rows) < 2 {
		return AnomalyValue{}
	}
	// Walk pairs from oldest to newest so we report the FIRST
	// chronological occurrence. rows[len-1] is oldest, rows[0] is
	// newest; pair = (older=rows[i+1], newer=rows[i]); iterating
	// i from len-2 down to 0 walks pairs in chronological order.
	for i := len(rows) - 2; i >= 0; i-- {
		newer := rows[i]
		older := rows[i+1]
		if newer.AST == nil || older.AST == nil {
			continue
		}
		spiked := spikedFeatures(*older.AST, *newer.AST)
		if len(spiked) >= MinSpikedFeatures {
			return AnomalyValue{
				AnomalyPresent:        true,
				FirstAnomalousVersion: newer.Version,
				PreviousVersion:       older.Version,
				SpikedFeatures:        spiked,
			}
		}
	}
	return AnomalyValue{}
}

// spikedFeatures returns the snake_case feature names that crossed
// from zero in older to non-zero in newer. Order matches the
// golang.Features field declaration order so the output is stable
// and matches the analyst-facing JSON tag names.
func spikedFeatures(older, newer golang.Features) []string {
	var spiked []string
	if older.InitCount == 0 && newer.InitCount > 0 {
		spiked = append(spiked, "init_count")
	}
	if older.NetworkCallSites == 0 && newer.NetworkCallSites > 0 {
		spiked = append(spiked, "network_call_sites")
	}
	if older.SensitivePathReads == 0 && newer.SensitivePathReads > 0 {
		spiked = append(spiked, "sensitive_path_reads")
	}
	if older.ExecCalls == 0 && newer.ExecCalls > 0 {
		spiked = append(spiked, "exec_calls")
	}
	if older.XORAssignments == 0 && newer.XORAssignments > 0 {
		spiked = append(spiked, "xor_assignments")
	}
	if older.Base64DecodeCalls == 0 && newer.Base64DecodeCalls > 0 {
		spiked = append(spiked, "base64_decode_calls")
	}
	return spiked
}
