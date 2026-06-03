package source

import "github.com/sarahmaeve/signatory/internal/signal/source/astfeature"

// ConcernValue is the JSON-marshaled value of the
// source_evolution_concern signal. Boolean+pointer summary derived
// from the matrix rows: ConcernPresent indicates whether some single
// version's AST counts on its OWN — independent of any cross-version
// diff — already cross the "this looks suspicious" threshold, and
// the optional fields name which version and which features fired.
//
// This is the in-situ companion to source_evolution_anomaly. Where
// the anomaly detector compares ADJACENT versions for zero→non-zero
// crossings (catching hijacks / clean→weaponized transitions), the
// concern detector evaluates EACH version independently (catching
// born-malicious packages where every published version is
// weaponized — the dominant typo-squat shape, including the
// 2026-05-24 Trapdoor cargo crates which published v0.1.0 already
// carrying the credential-stealer payload).
//
// Together the two signals close the differential vs absolute
// dichotomy: anomaly catches "this used to be safe and now isn't";
// concern catches "this was never safe."
//
// Empty (zero) value means "no concern" — omitempty on the optional
// fields keeps the JSON terse for the common case.
type ConcernValue struct {
	// ConcernPresent is true iff DetectConcern found any row whose
	// AST counts spike >= MinConcernFeatures rare-on-benign fields
	// in a single version's data, independent of cross-version
	// context.
	ConcernPresent bool `json:"concern_present"`

	// FirstConcernVersion is the chronologically FIRST (oldest)
	// concerning version. Convention: "the version where the
	// concern first manifests" — the earliest row whose catalog
	// hits crossed the threshold. Subsequent rows may also be
	// concerning; the analyst reads the raw matrix for that view.
	FirstConcernVersion string `json:"first_concern_version,omitempty"`

	// ConcerningFeatures lists the snake_case feature names whose
	// counts are non-zero in FirstConcernVersion AND that belong to
	// the rare-on-benign subset (see concerningFeatures). Sorted by
	// canonical astfeature.Counts field order for stable output.
	ConcerningFeatures []string `json:"concerning_features,omitempty"`
}

// MinConcernFeatures is the threshold for firing the in-situ
// concern: at least this many rare-on-benign features must be
// non-zero on the same row. Conservative (false-negative-heavy by
// design, matching MinSpikedFeatures=2 semantics):
//
//   - false negatives are recoverable because the matrix itself
//     stays in the handoff and the analyst can still notice
//   - false positives erode analyst trust in the boolean
//
// The Trapdoor cargo payloads spike 5+ rare-on-benign fields on
// their first published version, so a 2-feature threshold is amply
// over the floor for the named-incident corpus.
//
// Lifted as a public constant so a future tuning experiment can flip
// this value without surgery elsewhere.
const MinConcernFeatures = 2

// DetectConcern walks the matrix rows (sorted semver-descending,
// most-recent first) and returns the ConcernValue summarizing the
// FIRST chronological row whose AST counts independently fire >=
// MinConcernFeatures rare-on-benign features.
//
// "First chronological" matches the analyst's question: "when did
// this start being concerning?" — for born-malicious crates that's
// the v0.1.0 row; for hijacks that's the same version DetectAnomaly
// also names as FirstAnomalousVersion. The two signals coexist
// without redundancy: anomaly answers "is there a transition,
// where, and into what?"; concern answers "is there a row that
// stands on its own as suspicious, regardless of history?".
//
// Returns the zero ConcernValue (ConcernPresent=false, optional
// fields omitted in JSON) when:
//   - rows is empty
//   - all rows have nil AST (no analyzable versions)
//   - no row independently crosses the threshold
//
// Rows with nil AST (missing-from-clone, missing-origin,
// fetch-failed) are skipped — the in-situ evaluation requires data
// on the row itself.
func DetectConcern(rows []MatrixRow) ConcernValue {
	// rows[len-1] is oldest, rows[0] is newest. Iterate from oldest
	// to newest to find the FIRST chronological concerning row.
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].AST == nil {
			continue
		}
		fired := concerningFeatures(*rows[i].AST)
		if len(fired) >= MinConcernFeatures {
			return ConcernValue{
				ConcernPresent:      true,
				FirstConcernVersion: rows[i].Version,
				ConcerningFeatures:  fired,
			}
		}
	}
	return ConcernValue{}
}

// concerningFeatures returns the snake_case feature names that are
// non-zero in c AND belong to the "rare on benign" subset of
// astfeature.Counts — fields where any non-zero value is itself
// signal-bearing for an in-situ evaluation.
//
// Three fields are deliberately EXCLUDED from this subset:
//
//   - ImportTimeCallSites: naturally non-zero on every real
//     package (cargo's `println!("cargo:rerun-if-changed=...")`
//     idiom alone populates it; python's logging.getLogger() at
//     module top; npm's require-time function calls). AST.md §4
//     Architecture lesson: "their value is the spike, never the
//     absolute."
//   - NetworkCallSites: any HTTP-client crate or web framework
//     legitimately populates this. Without per-package-purpose
//     understanding, an absolute threshold here would false-
//     positive on every legitimate networking package.
//   - Base64DecodeCalls: crypto crates (e.g. sigstore had 18 on
//     the live dogfood) routinely decode base64-encoded
//     certificates / signatures / payloads. Same purpose-blindness
//     argument as NetworkCallSites.
//
// The remaining 10 fields constitute the rare-on-benign subset.
// Dogfood-validated zero across anyhow / serde / kong / ms /
// sigstore on every selected row, with the exclusions explicitly
// allowing the non-zero baseline cases noted above.
// CredentialDecryptCalls (Windows DPAPI) joined the subset with the
// spadata 2026-06 follow-up — legitimate code essentially never
// decrypts an OS-protected secret, so any non-zero count is signal.
//
// Output order matches the canonical astfeature.Counts field
// declaration order so the emitted JSON is stable across runs and
// matches the analyst-facing JSON tag names.
func concerningFeatures(c astfeature.Counts) []string {
	var fired []string
	if c.InitCount > 0 {
		fired = append(fired, "init_count")
	}
	// NetworkCallSites deliberately not in subset (see doc comment).
	if c.SensitivePathReads > 0 {
		fired = append(fired, "sensitive_path_reads")
	}
	if c.ExecCalls > 0 {
		fired = append(fired, "exec_calls")
	}
	if c.XORAssignments > 0 {
		fired = append(fired, "xor_assignments")
	}
	// Base64DecodeCalls deliberately not in subset (see doc comment).
	if c.DynamicEvalCalls > 0 {
		fired = append(fired, "dynamic_eval_calls")
	}
	// ImportTimeCallSites deliberately not in subset (see doc comment).
	if c.InstallHookOverrides > 0 {
		fired = append(fired, "install_hook_overrides")
	}
	if c.EnvCredentialReads > 0 {
		fired = append(fired, "env_credential_reads")
	}
	if c.SensitivePathWrites > 0 {
		fired = append(fired, "sensitive_path_writes")
	}
	if c.CloudMetadataCalls > 0 {
		fired = append(fired, "cloud_metadata_calls")
	}
	if c.CredentialDecryptCalls > 0 {
		fired = append(fired, "credential_decrypt_calls")
	}
	return fired
}
