package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// PairConfidence labels how the tarball↔commit pairing was
// established. Travels through into the signal payload so the
// synthesist can weight evidence: an exact gitHead pairing
// (npm v≥5 publishes carry the publisher-recorded commit SHA)
// is stronger than a tag-name match (PyPI, autotools, GitHub
// releases — we infer the commit by matching the version string
// against repo tags).
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
// that knowledge lives upstream in the dispatch / pair-resolver
// stage.
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

	// MaxBytes caps the gunzipped stream — see WalkOptions.MaxBytes.
	// Required for production calls; zero means unlimited (test only).
	MaxBytes int64

	// SampleCap bounds the extras_in_tarball_sample and
	// extras_in_repo_sample slice lengths. Zero or negative
	// means no cap.
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

// Compare reads the tarball stream, walks its headers, and
// computes the divergence against the supplied git path list.
// It also computes the SHA-256 of the tarball bytes as it reads,
// so the comparison's reproducibility anchor lands in the result
// without a second pass over the data.
//
// Returns the Comparison on success. On Walk failure (bomb cap,
// malformed archive) returns the wrapped error from Walk; the
// caller (the collector) converts that into a positive_absence
// row rather than aborting the whole collection.
//
// The reader is consumed exactly once. Callers that need the
// tarball bytes for something else (re-emit, archive) should
// io.TeeReader before calling — Compare itself does not
// double-buffer.
func Compare(r io.Reader, gitPaths []string, opts CompareOptions) (Comparison, error) {
	confidence := opts.PairConfidence
	if confidence == "" {
		confidence = PairConfidenceUnresolved
	}

	// Tee the tarball stream into a sha256 hasher so we get the
	// content hash for free as Walk consumes the bytes. No
	// double-buffering, no second pass.
	hasher := sha256.New()
	teed := io.TeeReader(r, hasher)

	entries, err := Walk(teed, WalkOptions{MaxBytes: opts.MaxBytes})
	if err != nil {
		return Comparison{}, fmt.Errorf("walk artifact: %w", err)
	}

	diff := ComputeDiff(entries, gitPaths, opts.SampleCap)

	return Comparison{
		ArtifactURL:           opts.ArtifactURL,
		ArtifactSHA256:        hex.EncodeToString(hasher.Sum(nil)),
		GitRef:                opts.GitRef,
		GitCommit:             opts.GitCommit,
		PairConfidence:        confidence,
		ExtrasInTarballCount:  diff.ExtrasInTarballCount,
		ExtrasInTarballSample: diff.ExtrasInTarballSample,
		Categories:            diff.Categories,
	}, nil
}
