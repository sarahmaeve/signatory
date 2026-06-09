package pypiwheel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/artifact/stream"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// CollectorName is stamped on every signal/absence this collector
// emits; the store keys on it.
const CollectorName = "pypi-wheel"

// defaultTTL matches the registry collectors' freshness window: a
// release's wheel bytes are immutable once published, but the daily
// window keeps refresh cycles sweeping the signal back through the
// registry-side wheel_url handoff that supplies its input.
const defaultTTL = 24 * time.Hour

// pthMaxFileBytes bounds the bytes read from any single .pth entry. A
// .pth file is read by Python's site module at startup and is tiny in
// every legitimate case; 1 MiB is a generous DoS ceiling. An entry over
// the cap is recorded in the manifest's SkippedScans, never silently
// dropped.
const pthMaxFileBytes = 1 << 20

// WheelFetcher fetches wheel bytes by URL, returning a ReadCloser the
// caller closes. Structurally identical to the artifact collector's
// ArtifactFetcher so the same NewStreamArtifactFetcher satisfies both —
// declared locally to keep this package free of an artifact-collector
// import. Implementations enforce their own timeout / size cap.
type WheelFetcher interface {
	Fetch(ctx context.Context, url string) (io.ReadCloser, error)
}

// CollectorConfig carries the per-construction wiring. Every field is
// optional; a zero value self-degrades to absence emissions rather than
// failing construction — the nil-safe shape every collector uses.
type CollectorConfig struct {
	// InRun is the orchestrator's accumulated CollectionResult, read
	// for the wheel_url signal the upstream pypi registry collector
	// emits. Nil → no_wheel_url absence.
	InRun *signal.CollectionResult

	// Fetcher downloads the wheel from its URL. Nil → absence on call.
	Fetcher WheelFetcher

	// Limits caps the zip walk's resource consumption. Zero-value
	// fields fall back to stream.DefaultLimits.
	Limits stream.Limits
}

// Collector implements signal.Collector for the narrow wheel-content
// inspection dimension. It opens the published wheel — the surface the
// sdist-only artifact-vs-repo check never reaches — for the specific
// non-build-artifact shapes that signal malice (today: .pth startup
// hooks carrying executable code). See the package doc and
// design/threat-landscape/2026-06-09-miasma-hades-pypi-wheel.md.
type Collector struct {
	cfg CollectorConfig
}

// NewCollector returns a Collector with the supplied config.
func NewCollector(cfg CollectorConfig) *Collector { return &Collector{cfg: cfg} }

// Name identifies the collector.
func (c *Collector) Name() string { return CollectorName }

// PthFileFinding is one wheel-resident .pth file with suspicious
// executable content, carried in the wheel_pth_executable signal.
type PthFileFinding struct {
	Path     string       `json:"path"`
	Findings []PthFinding `json:"findings"`
}

// Collect opens the latest wheel and scans its .pth entries. Every
// failure mode records an absence on wheel_pth_executable rather than
// returning an error — partial failures surface in the entity profile,
// not swallowed. Returned error is reserved for impossible-to-recover
// cases (none today; keeps the interface symmetric).
func (c *Collector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	result := &signal.CollectionResult{}
	collectedAt := time.Now().UTC()

	if entity == nil {
		return result, nil
	}

	info, ok := readWheelURL(c.cfg.InRun, entity.ID)
	if !ok {
		recordAbsence(result, entity.ID, "no wheel_url in run (sdist-only package, or registry collector recorded none)", collectedAt)
		return result, nil
	}
	if c.cfg.Fetcher == nil {
		recordAbsence(result, entity.ID, "no wheel fetcher wired", collectedAt)
		return result, nil
	}

	body, err := c.cfg.Fetcher.Fetch(ctx, info.URL)
	if err != nil {
		recordAbsence(result, entity.ID, fmt.Sprintf("fetch wheel: %v", err), collectedAt)
		return result, nil
	}
	defer func() { _ = body.Close() }()

	var findings []PthFileFinding
	scanned := 0
	scanners := []stream.Scanner{pthScanner(&findings, &scanned)}

	// Wheels are ZIP archives — the same FormatZip walk used for Go
	// module proxy zips, with bounded per-entry reads.
	if _, err := stream.WalkWithScanners(ctx, body, stream.FormatZip, nil, scanners, c.cfg.Limits); err != nil {
		recordAbsence(result, entity.ID, fmt.Sprintf("walk wheel: %v", err), collectedAt)
		return result, nil
	}

	total := 0
	for _, f := range findings {
		total += len(f.Findings)
	}

	// Always emit on a successful open: empty findings is the positive
	// "we opened the wheel and the .pth surface is clean" observation,
	// distinct from the absence emitted when we could not open it.
	result.RecordSignal(entity.ID, "wheel_pth_executable", CollectorName, collectedAt, defaultTTL,
		map[string]any{
			"wheel_url":           info.URL,
			"version":             info.Version,
			"filename":            info.Filename,
			"pth_files_scanned":   scanned,
			"files_with_findings": findings,
			"total_finding_count": total,
		})
	return result, nil
}

// pthScanner returns a stream.Scanner that runs ScanPth over every
// .pth entry, counting how many were scanned and retaining only the
// findings (never the file bytes).
func pthScanner(out *[]PthFileFinding, scanned *int) stream.Scanner {
	return stream.Scanner{
		Name:    "pth",
		MaxSize: pthMaxFileBytes,
		Match:   func(e stream.Entry) bool { return strings.HasSuffix(e.Path, ".pth") },
		Scan: func(path string, body io.Reader) error {
			b, err := io.ReadAll(body)
			if err != nil {
				return err
			}
			*scanned++
			if f := ScanPth(b); len(f) > 0 {
				*out = append(*out, PthFileFinding{Path: path, Findings: f})
			}
			return nil
		},
	}
}

// wheelURLValue is the unmarshal target for the wheel_url signal the
// pypi registry collector emits. Field names match recordWheelURL's
// JSON keys — keep in sync (loose in-run handoff contract, same as the
// artifact collector's urlSignalValue).
type wheelURLValue struct {
	URL       string `json:"url"`
	Version   string `json:"version"`
	Filename  string `json:"filename"`
	Integrity string `json:"integrity"`
}

// readWheelURL scans the in-run CollectionResult for a wheel_url signal
// recorded against entityID. Returns (value, true) on a hit with a
// non-empty URL; (zero, false) when InRun is nil, no wheel_url is
// present (including the absence row), or the payload has an empty URL.
func readWheelURL(inRun *signal.CollectionResult, entityID string) (wheelURLValue, bool) {
	if inRun == nil {
		return wheelURLValue{}, false
	}
	for _, s := range inRun.Signals() {
		if s.EntityID != entityID || s.Type != "wheel_url" {
			continue
		}
		var v wheelURLValue
		if err := json.Unmarshal(s.Value, &v); err != nil {
			continue
		}
		if v.URL == "" {
			continue
		}
		return v, true
	}
	return wheelURLValue{}, false
}

func recordAbsence(result *signal.CollectionResult, entityID, reason string, at time.Time) {
	result.RecordAbsence(entityID, "wheel_pth_executable", CollectorName, reason, false, at)
}

// Ensure Collector satisfies signal.Collector at compile time.
var _ signal.Collector = (*Collector)(nil)
