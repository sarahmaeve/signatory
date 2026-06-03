package source

import (
	"context"
	"encoding/json"
	"io"
	"iter"
	"time"

	"github.com/sarahmaeve/signatory/internal/artifact/stream"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	"github.com/sarahmaeve/signatory/internal/signal/source/astfeature"
)

// artifactSourceName is the collector identifier and signal source for
// the artifact-source path. Distinct from collectorSource
// ("source-evolution") so an analyst can tell clone-derived signals
// from published-artifact-derived ones.
const artifactSourceName = "artifact-source"

// artifactSourceMaxFileBytes bounds the bytes read from any single
// artifact entry for AST analysis. Human-written source is far smaller;
// an entry over the cap is recorded in the walk manifest's SkippedScans
// rather than buffered (a conservative miss, never silent).
const artifactSourceMaxFileBytes = 5 << 20

// ArtifactSourceConcernValue is the artifact_source_concern signal
// payload: the in-situ concern evaluated over the PUBLISHED artifact's
// source instead of the git clone. It catches a weaponized payload
// shipped in the registry artifact that the clone-based matrix can't
// see — because the source repo is clean, absent, or differs from what
// was published (the CVE-2024-3094 / spadata shape).
type ArtifactSourceConcernValue struct {
	ArtifactURL string            `json:"artifact_url"`
	Version     string            `json:"version"`
	Ecosystem   string            `json:"ecosystem"`
	AST         astfeature.Counts `json:"ast"`
	Concern     ConcernValue      `json:"concern"`
}

// analyzeArtifactSource runs the per-ecosystem AST analyzer over a
// stream of (already source-filtered) artifact files and evaluates the
// in-situ concern on the resulting single-version Counts. Returns
// supported=false for an ecosystem with no analyzer (the caller skips
// silently). The version labels the synthetic concern row.
//
// Single version ⇒ concern only: DetectConcern is row-wise, so it works
// on one row; DetectAnomaly is cross-version and has nothing to compare
// against here, so it is deliberately not run.
func analyzeArtifactSource(ctx context.Context, ecosystem, version string,
	files iter.Seq2[astfeature.SourceFile, error]) (astfeature.Counts, ConcernValue, bool, error) {

	_, analyzer, ok := languageProfile(ecosystem)
	if !ok {
		return astfeature.Counts{}, ConcernValue{}, false, nil
	}
	counts, err := analyzer.Analyze(ctx, files)
	if err != nil {
		return astfeature.Counts{}, ConcernValue{}, true, err
	}
	concern := DetectConcern([]MatrixRow{{Version: version, AST: &counts}})
	return counts, concern, true, nil
}

// artifactScanFormat maps an ecosystem to the stream archive format its
// published source artifact uses. gem is intentionally absent: its
// two-pass outer/inner walk isn't handled on this path (a documented
// gap shared with the exfil / build-script artifact scanners).
func artifactScanFormat(ecosystem string) (stream.Format, bool) {
	switch ecosystem {
	case "golang", "go":
		return stream.FormatZip, true
	case "pypi", "npm", "cargo", "crates":
		return stream.FormatTarGzip, true
	default:
		return stream.FormatUnknown, false
	}
}

// ArtifactFetcher fetches an artifact's bytes by URL. Structurally
// identical to the artifact collector's fetcher so the same
// stream-backed implementation satisfies both; declared here to avoid a
// source→artifact import.
type ArtifactFetcher interface {
	Fetch(ctx context.Context, url string) (io.ReadCloser, error)
}

// ArtifactSourceCollector runs the AST concern over the published
// artifact's source. Unlike the clone-based source-evolution Collector,
// it needs no git clone — only the artifact_url emitted by the registry
// collector — so it can flag a born-malicious package whose payload
// ships in the artifact but not the repo. Implements signal.Collector.
type ArtifactSourceCollector struct {
	inRun   *signal.CollectionResult
	fetcher ArtifactFetcher
}

// NewArtifactSourceCollector returns a collector reading artifact_url
// from inRun and fetching via fetcher. Both may be nil; Collect
// degrades to a graceful absence rather than panicking.
func NewArtifactSourceCollector(inRun *signal.CollectionResult, fetcher ArtifactFetcher) *ArtifactSourceCollector {
	return &ArtifactSourceCollector{inRun: inRun, fetcher: fetcher}
}

// Name implements signal.Collector.
func (c *ArtifactSourceCollector) Name() string { return artifactSourceName }

// Collect fetches the published artifact, runs the per-ecosystem AST
// analyzer over its source files, and emits artifact_source_concern.
//
// Unsupported ecosystems (no analyzer, or an archive format this path
// doesn't handle) skip silently — symmetric with the source-evolution
// collector. Missing inputs (no artifact_url, no fetcher, fetch/walk
// failure) record a non-retryable absence so the profile shows the
// check was attempted.
func (c *ArtifactSourceCollector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	result := &signal.CollectionResult{}
	if entity == nil {
		return result, nil
	}
	filter, _, ok := languageProfile(entity.Ecosystem)
	if !ok {
		return result, nil
	}
	format, ok := artifactScanFormat(entity.Ecosystem)
	if !ok {
		return result, nil
	}

	collectedAt := time.Now().UTC()

	url, version, ok := readArtifactURLFromInRun(c.inRun, entity.ID)
	if !ok {
		result.RecordAbsence(entity.ID, "artifact_source_concern", artifactSourceName,
			"no artifact_url in run; registry collector did not emit one", false, collectedAt)
		return result, nil
	}
	if c.fetcher == nil {
		result.RecordAbsence(entity.ID, "artifact_source_concern", artifactSourceName,
			"no artifact fetcher wired", false, collectedAt)
		return result, nil
	}

	body, err := c.fetcher.Fetch(ctx, url)
	if err != nil {
		result.RecordAbsence(entity.ID, "artifact_source_concern", artifactSourceName,
			"fetch artifact: "+err.Error(), false, collectedAt)
		return result, nil
	}
	defer func() { _ = body.Close() }()

	var files []astfeature.SourceFile
	scanner := stream.Scanner{
		Name:    "source-ast",
		MaxSize: artifactSourceMaxFileBytes,
		Match:   func(e stream.Entry) bool { return e.Type == stream.EntryFile && filter(e.Path) },
		Scan: func(path string, rdr io.Reader) error {
			b, err := io.ReadAll(rdr)
			if err != nil {
				return err
			}
			files = append(files, astfeature.SourceFile{Path: path, Content: b})
			return nil
		},
	}
	if _, err := stream.WalkWithScanners(ctx, body, format, nil, []stream.Scanner{scanner}, stream.Limits{}); err != nil {
		result.RecordAbsence(entity.ID, "artifact_source_concern", artifactSourceName,
			"walk artifact: "+err.Error(), false, collectedAt)
		return result, nil
	}

	counts, concern, _, err := analyzeArtifactSource(ctx, entity.Ecosystem, version, filesSeq(files))
	if err != nil {
		result.RecordFailure(entity.ID, "artifact_source_concern", artifactSourceName,
			"analyze artifact source: "+err.Error(), true, collectedAt)
		return result, nil
	}

	result.RecordSignal(entity.ID, "artifact_source_concern", artifactSourceName,
		collectedAt, collectorTTL, ArtifactSourceConcernValue{
			ArtifactURL: url,
			Version:     version,
			Ecosystem:   entity.Ecosystem,
			AST:         counts,
			Concern:     concern,
		})
	return result, nil
}

// filesSeq adapts a buffered SourceFile slice to the analyzer's stream
// API. The error arm is never exercised (the bytes are already in
// memory), keeping the analyzer's mid-stream-error contract intact.
func filesSeq(files []astfeature.SourceFile) iter.Seq2[astfeature.SourceFile, error] {
	return func(yield func(astfeature.SourceFile, error) bool) {
		for _, f := range files {
			if !yield(f, nil) {
				return
			}
		}
	}
}

// readArtifactURLFromInRun pulls the (url, version) from the
// artifact_url signal the registry collector emitted for entityID.
// Mirrors the artifact collector's reader; replicated rather than shared
// to keep the loose in-run handoff contract (and avoid a source→artifact
// import).
func readArtifactURLFromInRun(inRun *signal.CollectionResult, entityID string) (url, version string, ok bool) {
	if inRun == nil {
		return "", "", false
	}
	for _, s := range inRun.Signals() {
		if s.EntityID != entityID || s.Type != "artifact_url" {
			continue
		}
		var v struct {
			URL     string `json:"url"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal(s.Value, &v); err != nil {
			continue
		}
		if v.URL == "" {
			continue
		}
		return v.URL, v.Version, true
	}
	return "", "", false
}

// Compile-time assertion that the collector satisfies the interface.
var _ signal.Collector = (*ArtifactSourceCollector)(nil)
