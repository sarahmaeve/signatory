package source

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	"github.com/sarahmaeve/signatory/internal/signal/source/golang"
	"github.com/sarahmaeve/signatory/internal/signal/source/python"
)

// collectorSource is the value that lands in profile.Signal.Source
// for every emission from this collector. Constant so the string
// appears in exactly one place.
const collectorSource = "source-evolution"

// collectorTTL matches the gopublish collector's default — 24
// hours of freshness before refresh logic surfaces the signal for
// re-collection. Not load-bearing for v0.1.
const collectorTTL = 24 * time.Hour

// Collector is the source-evolution collector. Composes:
//   - VersionPinSource     — reads the version_pin_table signal
//     (gopublish for Go; the pypi registry collector for pypi)
//   - BlobStreamer         — reads source bytes from the local clone
//   - LanguageAnalyzer     — extracts AST features per version
//     (golang.Analyzer for Go; python.Analyzer for pypi)
//   - Assembler            — builds MatrixValue
//   - DetectAnomaly        — derives the anomaly summary
//
// Emits two signals per supported-ecosystem entity:
//   - source_evolution_matrix  (compound per-version table)
//   - source_evolution_anomaly (boolean + pointer summary)
//
// The file filter and analyzer are selected per entity by
// languageProfile. Unsupported ecosystems skip silently (empty
// result, no error). Supported entities with a missing pin table or
// clone produce absences with clear reasons rather than failures.
//
// Implements signal.Collector. Registered into dispatch by
// cmd/signatory/collectors.go for the supported ecosystems.
type Collector struct {
	clonePath  string
	pinSource  VersionPinSource
	allowFetch bool
}

// NewCollector returns a Collector with the given clone path, pin
// source, and fetch policy. The per-language analyzer and source-file
// filter are chosen at Collect time from the entity's ecosystem (see
// languageProfile). Pass allowFetch=true to enable the BlobStreamer's
// opt-in fetch-on-missing-SHA path (--allow-fetch CLI flag).
func NewCollector(clonePath string, pinSource VersionPinSource, allowFetch bool) *Collector {
	return &Collector{
		clonePath:  clonePath,
		pinSource:  pinSource,
		allowFetch: allowFetch,
	}
}

// languageProfile maps an entity ecosystem to its source-file filter
// and AST analyzer. ok=false means source-evolution does not support
// that ecosystem and the collector skips silently.
//
// pypi uses python.Analyzer — the hand-written Python lexer/parser/
// extractor in internal/signal/source/python; it populates the AST
// Counts (dynamic-eval, import-time call sites, install-hook
// overrides, exec/network/base64/sensitive-path) just as
// golang.Analyzer does for Go. Structural and diff signal are
// language-neutral and flow for both.
func languageProfile(ecosystem string) (filter func(path string) bool, analyzer LanguageAnalyzer, ok bool) {
	switch ecosystem {
	case "golang", "go":
		return isGoSourceFile, golang.NewAnalyzer(), true
	case "pypi":
		return isPythonSourceFile, python.NewAnalyzer(), true
	default:
		return nil, nil, false
	}
}

// Name identifies the collector — value flows into source-tracking
// columns and the dogfood-metrics report.
func (c *Collector) Name() string { return collectorSource }

// Collect produces source_evolution_matrix and
// source_evolution_anomaly signals for the entity.
//
// Per signal.Collector's contract, Collect only returns a non-nil
// error when collection cannot proceed at all (currently never —
// even nil-entity returns an empty result). Per-signal failures
// are recorded as absences inside the CollectionResult.
//
// Outcome cases:
//   - entity == nil OR unsupported ecosystem — empty result.
//   - pin source nil OR pin table not available — both signals
//     emit as absences (non-retryable; the operator must run
//     gopublish first).
//   - pin source returns other error — both as failures
//     (retryable; transient store/network issue).
//   - clonePath empty — both as absences (configuration mistake;
//     run analyze with --clone).
//   - BlobStreamer/Assembler error — both as failures (retryable;
//     subprocess plumbing or git state).
//   - happy path — both signals land with full values.
func (c *Collector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	result := &signal.CollectionResult{}
	if entity == nil {
		return result, nil
	}
	srcFilter, analyzer, ok := languageProfile(entity.Ecosystem)
	if !ok {
		return result, nil
	}

	collectedAt := time.Now().UTC()

	if c.pinSource == nil {
		c.recordAbsenceBoth(result, entity,
			"no pin source configured for source-evolution collector",
			false, collectedAt)
		return result, nil
	}

	pinTable, err := c.pinSource.VersionPinTable(ctx, entity)
	if err != nil {
		if errors.Is(err, ErrPinTableNotAvailable) {
			c.recordAbsenceBoth(result, entity,
				"version pin table required; gopublish collector did not run or did not emit",
				false, collectedAt)
			return result, nil
		}
		c.recordFailureBoth(result, entity,
			fmt.Sprintf("get pin table: %v", err), true, collectedAt)
		return result, nil
	}

	if c.clonePath == "" {
		c.recordAbsenceBoth(result, entity,
			"local clone required for source-evolution matrix",
			false, collectedAt)
		return result, nil
	}

	bsOpts := []BlobStreamerOption{WithSourceFileFilter(srcFilter)}
	if c.allowFetch {
		bsOpts = append(bsOpts, WithAllowFetch(true))
	}
	bs, err := NewBlobStreamer(ctx, c.clonePath, bsOpts...)
	if err != nil {
		c.recordFailureBoth(result, entity,
			fmt.Sprintf("start blob streamer at %q: %v", c.clonePath, err),
			true, collectedAt)
		return result, nil
	}
	defer func() { _ = bs.Close() }()

	assembler := NewAssembler(bs, analyzer)
	matrix, err := assembler.Build(ctx, pinTable, BudgetOpts{})
	if err != nil {
		c.recordFailureBoth(result, entity,
			fmt.Sprintf("build matrix: %v", err), true, collectedAt)
		return result, nil
	}

	// Emit the matrix first, then the derived anomaly. Order
	// doesn't affect store correctness but matches the analyst
	// reading order: data → summary.
	result.RecordSignal(entity.ID, "source_evolution_matrix",
		collectorSource, collectedAt, collectorTTL, matrix)

	anomaly := DetectAnomaly(matrix.Rows)
	result.RecordSignal(entity.ID, "source_evolution_anomaly",
		collectorSource, collectedAt, collectorTTL, anomaly)

	return result, nil
}

// recordAbsenceBoth emits absence records for both signals at
// once. Used when the collector can't make any progress — both
// signals are equally affected, so both should appear as absences
// rather than one missing and one absent.
func (c *Collector) recordAbsenceBoth(r *signal.CollectionResult, entity *profile.Entity,
	reason string, retryable bool, t time.Time) {
	r.RecordAbsence(entity.ID, "source_evolution_matrix", collectorSource, reason, retryable, t)
	r.RecordAbsence(entity.ID, "source_evolution_anomaly", collectorSource, reason, retryable, t)
}

// recordFailureBoth is the same pattern for transient failures.
// RecordFailure also tracks the failure in result.Failures so the
// run summary reflects what didn't work.
func (c *Collector) recordFailureBoth(r *signal.CollectionResult, entity *profile.Entity,
	reason string, retryable bool, t time.Time) {
	r.RecordFailure(entity.ID, "source_evolution_matrix", collectorSource, reason, retryable, t)
	r.RecordFailure(entity.ID, "source_evolution_anomaly", collectorSource, reason, retryable, t)
}
