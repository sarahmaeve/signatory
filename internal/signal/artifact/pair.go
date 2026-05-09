package artifact

import "slices"

// AbsenceReason* are the documented reasons a positive_absence
// row can carry for the artifact_repo_divergence signal. They are
// part of the wire contract: the synthesist's prompt switches on
// these strings to distinguish hygiene observations about a
// project's release process ("we couldn't pair this tarball to a
// commit") from outright collection failures.
//
// Adding a reason here without updating the synthesist guidance
// is a silent regression — keep this set small and meaningful.
const (
	// AbsenceReasonPairUnresolved fires when the artifact has
	// no publisher-stamped commit ref AND no version-string match
	// against the repo's tag list. The project's release pipeline
	// doesn't expose a recoverable tarball↔commit link.
	AbsenceReasonPairUnresolved = "tarball-to-commit pairing unresolved"

	// AbsenceReasonNoArtifactURL fires when the registry collector
	// emitted no artifact_url for the entity (e.g. registry
	// payload had no dist.tarball, or the registry collector
	// itself didn't run). The collector can't compare what it
	// can't fetch.
	AbsenceReasonNoArtifactURL = "no artifact URL available from registry collector"

	// AbsenceReasonNoClone fires when the entity has no local
	// git clone for the repo side of the comparison. Without one,
	// there's no git tree to set-difference against.
	AbsenceReasonNoClone = "no git clone available for repo-side comparison"
)

// PairInputs is the data ResolvePair needs to attempt a
// tarball↔commit pairing. The caller (the artifact collector)
// gathers these from the registry-side payload and the local
// clone before calling.
type PairInputs struct {
	// Version is the package version string as the registry
	// records it ("5.6.1", "1.2.3-beta.2", etc.). Used for
	// tag-list reconciliation when GitHead is empty.
	Version string

	// GitHead, when non-empty, is the publisher-stamped commit
	// SHA recorded in the registry's per-version metadata. npm
	// v≥5 carries this in versions[v].gitHead. Other registries
	// don't, and ResolvePair falls back to tag matching.
	GitHead string

	// Tags is the list of tag names available in the repo's git
	// history. ResolvePair walks this list looking for a name
	// that matches Version under conventional schemes.
	Tags []string
}

// Resolution is what ResolvePair returns: the resolved git side
// (Commit or Ref, depending on confidence), the confidence label
// that travels into the signal payload, and — for the unresolved
// case — the wire-contract reason the collector emits as a
// positive_absence row.
type Resolution struct {
	Commit        string
	Ref           string
	Confidence    string
	AbsenceReason string
}

// ResolvePair attempts to establish a tarball↔commit pairing for
// the artifact. Returns (resolution, true) on success and
// (resolution{Confidence: Unresolved, AbsenceReason: ...}, false)
// when no pairing can be made.
//
// Resolution order:
//
//  1. Exact gitHead — if the registry payload carries a commit
//     SHA, use it. This is the strongest pairing available and
//     wins regardless of what's in the tag list.
//
//  2. Tag match — try "v<version>" first, then bare "<version>".
//     Both forms appear in real repos; "v"-prefix is dominant
//     today, but bare-version tags exist in older Go modules,
//     a chunk of PyPI projects, and historical npm releases.
//     The "v"-prefix wins ties so a repo that publishes both
//     forms (rare but legal) gets the canonical one.
//
//  3. Unresolved — record AbsenceReasonPairUnresolved and let
//     the collector emit a positive_absence row.
func ResolvePair(in PairInputs) (Resolution, bool) {
	if in.GitHead != "" {
		return Resolution{
			Commit:     in.GitHead,
			Confidence: PairConfidenceExactGitHead,
		}, true
	}

	// Try v-prefixed first so the canonical form wins ties.
	candidates := []string{"v" + in.Version, in.Version}
	for _, want := range candidates {
		if slices.Contains(in.Tags, want) {
			return Resolution{
				Ref:        want,
				Confidence: PairConfidenceTagMatch,
			}, true
		}
	}

	return Resolution{
		Confidence:    PairConfidenceUnresolved,
		AbsenceReason: AbsenceReasonPairUnresolved,
	}, false
}
