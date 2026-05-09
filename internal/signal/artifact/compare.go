package artifact

import (
	"github.com/sarahmaeve/signatory/internal/artifact/stream"
)

// PairConfidence labels how the tarball↔commit pairing was
// established. Travels through into the signal payload so the
// synthesist can weight evidence: an exact gitHead pairing
// (npm v≥5 publishes carry the publisher-recorded commit SHA;
// cargo .crate tarballs carry the SHA in .cargo_vcs_info.json
// stamped by `cargo publish` itself) is stronger than a tag-name
// match (PyPI, autotools, GitHub releases — we infer the commit
// by matching the version string against repo tags).
//
// Constants are strings rather than an enum type because they
// land directly in the signal payload's JSON; bare strings
// round-trip through the analyst surface without enum-decoding
// ceremony.
const (
	PairConfidenceExactGitHead = "exact_gitHead"
	PairConfidenceTagMatch     = "tag_match"
	PairConfidenceUnresolved   = "unresolved"
)

// CompareOptions carries the per-comparison metadata the caller
// (the artifact collector) supplies. Compare itself doesn't know
// how the pairing was resolved or where the tarball came from —
// that knowledge lives upstream in the collector's pair-resolver
// and fetcher stages.
type CompareOptions struct {
	// ArtifactURL is the URL the tarball was fetched from. Goes
	// into the signal payload so a reviewer can re-fetch and
	// re-diff the same bytes.
	ArtifactURL string

	// GitRef is the resolved git reference (tag name typically:
	// "v5.6.1"). May be empty when PairConfidence is Unresolved.
	GitRef string

	// GitCommit is the resolved git commit SHA. Empty when
	// PairConfidence is Unresolved or when only a tag match was
	// possible without commit lookup.
	GitCommit string

	// PairConfidence is one of the PairConfidence* constants.
	// Required: callers MUST set this; an empty string falls
	// through to Unresolved.
	PairConfidence string

	// SampleCap bounds the extras_in_tarball_sample slice length.
	// Zero or negative means no cap.
	SampleCap int
}

// Comparison is the structured form of the artifact_repo_divergence
// signal payload. Marshalling this struct as JSON is what lands in
// signal.Value at the collector boundary.
//
// One-directional by design: only files present in the tarball but
// absent from the repo are surfaced. See Diff for the rationale.
type Comparison struct {
	ArtifactURL    string `json:"artifact_url"`
	ArtifactSHA256 string `json:"artifact_sha256"`
	GitRef         string `json:"git_ref"`
	GitCommit      string `json:"git_commit"`
	PairConfidence string `json:"pair_confidence"`

	// Diff fields are flattened onto Comparison rather than nested
	// under a "diff" object so the JSON shape stays a flat read on
	// the analyst side.
	ExtrasInTarballCount  int               `json:"files_extra_in_tarball"`
	ExtrasInTarballSample []ClassifiedEntry `json:"extras_in_tarball_sample"`
	Categories            map[string]int    `json:"categories"`
}

// Compare builds a Comparison from a stream-walked archive manifest
// and a git path list. The manifest is the upstream output of
// stream.Walk (or stream.FetchAndWalk); the collector calls Walk
// itself so it can register ecosystem-specific CaptureIntents and
// post-process the manifest (e.g. cargo's .cargo_vcs_info.json SHA
// recovery) before reaching this function.
//
// No error return: every failure mode lives upstream — Walk's
// errors surface in the collector, which converts them into
// positive_absence rows. Once we have a manifest, comparison is
// pure computation against the supplied gitPaths.
//
// The archive's sha256 comes from manifest.ArchiveSHA256, computed
// by the walker as it consumed the stream — no second pass.
func Compare(manifest *stream.Manifest, gitPaths []string, opts CompareOptions) Comparison {
	confidence := opts.PairConfidence
	if confidence == "" {
		confidence = PairConfidenceUnresolved
	}
	diff := ComputeDiff(manifest, gitPaths, opts.SampleCap)
	return Comparison{
		ArtifactURL:           opts.ArtifactURL,
		ArtifactSHA256:        manifest.ArchiveSHA256,
		GitRef:                opts.GitRef,
		GitCommit:             opts.GitCommit,
		PairConfidence:        confidence,
		ExtrasInTarballCount:  diff.ExtrasInTarballCount,
		ExtrasInTarballSample: diff.ExtrasInTarballSample,
		Categories:            diff.Categories,
	}
}
