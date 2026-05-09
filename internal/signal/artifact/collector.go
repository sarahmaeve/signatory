package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// CollectorName is the source string stamped on every signal /
// absence the artifact collector emits. Stable across refactors —
// the store keys on it.
const CollectorName = "artifact-vs-repo"

// defaultTTL matches the registry collectors' freshness window.
// The artifact-vs-repo signal is a function of (tarball bytes, git
// tree at ref), and both are immutable once a release is cut, so
// strictly speaking the TTL could be very long. We keep it at the
// daily window so refresh cycles still sweep the signal back through
// the registry-side check that supplies its inputs.
const defaultTTL = 24 * time.Hour

// CollectorConfig carries the per-construction wiring. Every field
// is optional; a zero-value Config produces a collector that
// gracefully records absences for everything it can't do.
//
// This lets dispatch register the collector unconditionally and
// have it self-degrade when upstream dependencies aren't present —
// the same shape every other collector uses for nil-safe wiring.
type CollectorConfig struct {
	// InRun is the orchestrator's accumulated CollectionResult.
	// Read for the artifact_url signal emitted by the upstream
	// registry collector. Nil → AbsenceReasonNoArtifactURL.
	InRun *signal.CollectionResult

	// ClonePath is the absolute path to the isolated git clone
	// (post-cloneToTempIsolated). The collector runs git ls-tree
	// and tag-list against this. Empty → AbsenceReasonNoClone.
	ClonePath string

	// Fetcher downloads the tarball from a URL to a tempfile.
	// Nil → falls back to a default that records absence on call;
	// production wiring sets a real net/http-backed fetcher.
	Fetcher ArtifactFetcher

	// Git is the inspector that reads tag list + ls-tree from
	// ClonePath. Nil → absence-on-call. Production wires a
	// gitenv-backed implementation.
	Git GitInspector

	// MaxArchiveBytes caps the gunzipped stream during Walk.
	// Zero → 256 MiB default; explicit override for tests that
	// want a smaller cap.
	MaxArchiveBytes int64

	// SampleCap bounds the sample slices in the emitted signal.
	// Zero → 50.
	SampleCap int
}

// ArtifactFetcher fetches tarball bytes by URL. Returns a
// ReadCloser the caller must close. Implementations are expected
// to enforce their own timeout / size cap; the collector does not
// re-cap on top.
type ArtifactFetcher interface {
	Fetch(ctx context.Context, url string) (io.ReadCloser, error)
}

// GitInspector reads from a local clone the data the collector
// needs to compare against. Methods are scoped narrowly so a test
// fake can implement just what each test exercises.
type GitInspector interface {
	// Tags returns the list of tag names in the clone.
	Tags(ctx context.Context) ([]string, error)
	// PathsAtRef returns the file paths under ref's tree
	// (equivalent to `git ls-tree -r --name-only ref`).
	PathsAtRef(ctx context.Context, ref string) ([]string, error)
	// CommitForRef resolves ref to its commit SHA.
	CommitForRef(ctx context.Context, ref string) (string, error)
}

// Collector implements signal.Collector for the artifact-vs-repo
// dimension. See the package doc for threat model and design
// constraints.
type Collector struct {
	cfg CollectorConfig
}

// NewCollector returns a Collector with the supplied config. Every
// field of the config is optional; missing inputs yield absence
// emissions at Collect time rather than construction errors.
func NewCollector(cfg CollectorConfig) *Collector {
	if cfg.MaxArchiveBytes <= 0 {
		cfg.MaxArchiveBytes = 256 << 20 // 256 MiB
	}
	if cfg.SampleCap <= 0 {
		cfg.SampleCap = 50
	}
	return &Collector{cfg: cfg}
}

// Name identifies the collector.
func (c *Collector) Name() string { return CollectorName }

// Collect runs the artifact-vs-repo comparison for entity. Every
// failure mode produces a positive_absence on artifact_repo_divergence
// rather than a returned error — partial failures are recorded so
// they show up in the entity's profile, not swallowed.
//
// Returned error is reserved for impossible-to-recover cases (the
// orchestrator wouldn't get a result back at all) — none currently
// exist; this contract keeps the interface symmetric with the other
// collectors.
func (c *Collector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	result := &signal.CollectionResult{}
	collectedAt := time.Now().UTC()

	if entity == nil {
		// No entity → no absence row to write against; return empty.
		// Symmetric with how npm collector handles the no-entity case.
		return result, nil
	}

	// 1. Read the upstream artifact_url signal from the in-run
	// accumulator. Missing → no_artifact_url absence.
	urlInfo, ok := readArtifactURL(c.cfg.InRun, entity.ID)
	if !ok {
		recordDivergenceAbsence(result, entity.ID, AbsenceReasonNoArtifactURL, collectedAt)
		return result, nil
	}

	// 2. Verify a clone is available. Missing → no_clone absence.
	if c.cfg.ClonePath == "" || c.cfg.Git == nil {
		recordDivergenceAbsence(result, entity.ID, AbsenceReasonNoClone, collectedAt)
		return result, nil
	}

	// 3. Resolve the tarball↔commit pairing.
	tags, err := c.cfg.Git.Tags(ctx)
	if err != nil {
		recordDivergenceAbsence(result, entity.ID,
			fmt.Sprintf("read git tags: %v", err), collectedAt)
		return result, nil
	}
	res, ok := ResolvePair(PairInputs{
		Version: urlInfo.version,
		GitHead: urlInfo.gitHead,
		Tags:    tags,
	})
	if !ok {
		recordDivergenceAbsence(result, entity.ID, res.AbsenceReason, collectedAt)
		return result, nil
	}

	// 4. Determine the ref to read paths from. ExactGitHead returned
	// a commit SHA; tag-match returned a tag name. PathsAtRef accepts
	// both (git ls-tree resolves either to a tree).
	ref := res.Ref
	if ref == "" {
		ref = res.Commit
	}

	gitPaths, err := c.cfg.Git.PathsAtRef(ctx, ref)
	if err != nil {
		recordDivergenceAbsence(result, entity.ID,
			fmt.Sprintf("read git tree at %q: %v", ref, err), collectedAt)
		return result, nil
	}

	// Resolve commit SHA when we only have a ref. Best-effort: if
	// the lookup fails, we still emit the divergence signal — the
	// commit field just goes empty in the payload.
	commit := res.Commit
	if commit == "" {
		if sha, err := c.cfg.Git.CommitForRef(ctx, ref); err == nil {
			commit = sha
		}
	}

	// 5. Fetch the tarball. Missing fetcher → can't proceed; record
	// absence rather than crash.
	if c.cfg.Fetcher == nil {
		recordDivergenceAbsence(result, entity.ID,
			"no artifact fetcher wired", collectedAt)
		return result, nil
	}
	body, err := c.cfg.Fetcher.Fetch(ctx, urlInfo.url)
	if err != nil {
		recordDivergenceAbsence(result, entity.ID,
			fmt.Sprintf("fetch artifact: %v", err), collectedAt)
		return result, nil
	}
	defer body.Close()

	// 6. Compare.
	cmp, err := Compare(body, gitPaths, CompareOptions{
		ArtifactURL:    urlInfo.url,
		GitRef:         ref,
		GitCommit:      commit,
		PairConfidence: res.Confidence,
		MaxBytes:       c.cfg.MaxArchiveBytes,
		SampleCap:      c.cfg.SampleCap,
	})
	if err != nil {
		recordDivergenceAbsence(result, entity.ID,
			fmt.Sprintf("compare: %v", err), collectedAt)
		return result, nil
	}

	// 7. Emit the signal.
	result.RecordSignal(entity.ID, "artifact_repo_divergence", CollectorName,
		collectedAt, defaultTTL, cmp)

	return result, nil
}

// recordDivergenceAbsence is the one-line absence emitter used by
// every failure branch in Collect. Centralised so the absence
// type, source string, and reason-formatting stay consistent.
//
// Retryable=false: every failure mode here represents a stable
// state of the upstream inputs (no URL, no clone, no pair). A
// retry without changing those inputs reproduces the same answer.
// Operators who fix the underlying gap re-run signatory; that's
// the recovery path, not collector-level retry.
func recordDivergenceAbsence(result *signal.CollectionResult, entityID, reason string, at time.Time) {
	result.RecordAbsence(entityID, "artifact_repo_divergence", CollectorName,
		reason, false, at)
}

// urlSignalValue is the unmarshal target for the artifact_url
// signal payload the npm collector emits. Field names match the
// JSON keys recordArtifactURL writes — keep these in sync. (A
// shared types package would couple the two collectors more
// tightly than the loose "in-run signal handoff" contract wants.)
type urlSignalValue struct {
	URL       string `json:"url"`
	Version   string `json:"version"`
	GitHead   string `json:"git_head"`
	Integrity string `json:"integrity"`
}

// urlInfo is the small bundle of fields readArtifactURL hands back
// to Collect. Trimmed to what Collect actually consumes — Integrity
// is captured but not used today; kept on the wire for future
// cross-checking work.
type urlInfo struct {
	url       string
	version   string
	gitHead   string
	integrity string
}

// readArtifactURL scans the in-run CollectionResult for an
// artifact_url signal recorded against entityID. Returns
// (urlInfo, true) on a hit; (zero, false) when the in-run is nil,
// when no artifact_url is present, or when the recorded payload
// has an empty url.
//
// An absence row for artifact_url is treated identically to a
// missing signal — both mean "no URL is available," which Collect
// surfaces as AbsenceReasonNoArtifactURL on its own divergence
// signal type.
func readArtifactURL(inRun *signal.CollectionResult, entityID string) (urlInfo, bool) {
	if inRun == nil {
		return urlInfo{}, false
	}
	for _, s := range inRun.Signals() {
		if s.EntityID != entityID {
			continue
		}
		if s.Type != "artifact_url" {
			continue
		}
		var v urlSignalValue
		if err := json.Unmarshal(s.Value, &v); err != nil {
			continue
		}
		if v.URL == "" {
			continue
		}
		return urlInfo{
			url:       v.URL,
			version:   v.Version,
			gitHead:   v.GitHead,
			integrity: v.Integrity,
		}, true
	}
	return urlInfo{}, false
}

// Ensure Collector satisfies signal.Collector at compile time.
// Catches accidental signature drift across refactors with a
// near-zero-cost assertion.
var _ signal.Collector = (*Collector)(nil)
