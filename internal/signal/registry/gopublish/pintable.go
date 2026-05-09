package gopublish

import (
	"context"
	"errors"
	mathrandv2 "math/rand/v2"
	"slices"
	"time"

	"golang.org/x/mod/semver"
)

// maxPinFetches caps the number of GetVersionInfo requests per
// analysis. 12 covers source-evolution's typical budget (last-N +
// leaves-of-each-major) with headroom; long-history modules get the
// most-recent 12 only. A future version may surface this as a
// tunable knob; v0.1 keeps it constant for predictability.
//
// 12 versions inside a short window (e.g., 72 hours) is itself a
// signal — version-padding is a known pre-attack pattern. The
// PublishedAt field on each pin lets a downstream analyzer compute
// span/velocity directly without re-fetching.
const maxPinFetches = 12

// pinFetchJitterMin / pinFetchJitterMax bracket the random delay
// between consecutive GetVersionInfo calls. Sequential pacing avoids
// burst-shape against the proxy; the random component slightly
// randomizes our request fingerprint vs. a fixed-rate scraper.
//
// Tests in this package may set Collector.jitterMin / jitterMax to 0
// directly to bypass the sleep — same-package field access is the
// established test seam pattern in this codebase.
const (
	pinFetchJitterMin = 200 * time.Millisecond
	pinFetchJitterMax = 800 * time.Millisecond
)

// VersionPinTableValue is the JSON-marshaled value of the
// version_pin_table signal. Source-evolution reads this through the
// VersionPinSource interface (internal/signal/source/) to anchor
// matrix rows to commit SHAs.
//
// Schema invariants:
//   - Pins, MissingOriginVersions, FetchFailedVersions are always
//     non-nil (empty slice, not nil) so consumer code doesn't need
//     nil-guards on JSON-roundtripped values.
//   - VersionCountTotal is the size of @v/list at fetch time.
//   - VersionCountProcessed is min(VersionCountTotal, maxPinFetches).
//   - len(Pins) + len(MissingOriginVersions) + len(FetchFailedVersions)
//     == VersionCountProcessed (every processed version lands in
//     exactly one bucket, even if context cancellation cut the run
//     short).
type VersionPinTableValue struct {
	ModulePath            string       `json:"module_path"`
	VersionCountTotal     int          `json:"version_count_total"`
	VersionCountProcessed int          `json:"version_count_processed"`
	Pins                  []VersionPin `json:"pins"`
	MissingOriginVersions []string     `json:"missing_origin_versions"`
	FetchFailedVersions   []string     `json:"fetch_failed_versions"`
}

// VersionPin is one (version, sha, published_at) tuple. Source is
// always "proxy.golang.org" in v0.1; the field is retained for
// forward compatibility with future registry-side pin sources.
type VersionPin struct {
	Version     string `json:"version"`
	SHA         string `json:"sha"`
	Source      string `json:"source"`
	PublishedAt string `json:"published_at"` // RFC3339 UTC
}

// processPinTable iterates the most-recent N versions and assembles
// a VersionPinTableValue. Sequential GetVersionInfo calls with
// random jitter between them.
//
// Always returns a value (never nil); empty slices when no versions
// or all fetches fail. The caller decides whether to record the
// result as a signal or absence based on context.
func (c *Collector) processPinTable(ctx context.Context, modulePath string, allVersions []string) VersionPinTableValue {
	sorted := sortByMostRecent(allVersions)
	processCount := min(len(sorted), maxPinFetches)

	value := VersionPinTableValue{
		ModulePath:            modulePath,
		VersionCountTotal:     len(allVersions),
		VersionCountProcessed: processCount,
		Pins:                  []VersionPin{},
		MissingOriginVersions: []string{},
		FetchFailedVersions:   []string{},
	}

	for i, version := range sorted[:processCount] {
		if err := ctx.Err(); err != nil {
			// Context cancelled before this iteration. Mark the
			// remaining selected versions as fetch-failed so the
			// consumer sees the gap explicitly (every selected
			// version must land in exactly one bucket per the
			// schema invariant).
			value.FetchFailedVersions = append(value.FetchFailedVersions, sorted[i:processCount]...)
			return value
		}

		// Jitter between calls (not before the first).
		if i > 0 {
			if !c.sleepWithJitter(ctx) {
				value.FetchFailedVersions = append(value.FetchFailedVersions, sorted[i:processCount]...)
				return value
			}
		}

		info, err := c.client.GetVersionInfo(ctx, modulePath, version)
		switch {
		case errors.Is(err, ErrNotFound):
			// 404 / 410 from the proxy. Unusual when the version is
			// in @v/list (means the proxy is mid-reindex or we're
			// seeing a flap), but possible.
			value.FetchFailedVersions = append(value.FetchFailedVersions, version)
		case err != nil:
			value.FetchFailedVersions = append(value.FetchFailedVersions, version)
		case info.Origin.Hash == "":
			// 200 OK but Origin block empty — pre-Go-1.20 publish.
			// Source-evolution falls back to local refs/tags for
			// these when assembling matrix rows.
			value.MissingOriginVersions = append(value.MissingOriginVersions, version)
		default:
			value.Pins = append(value.Pins, VersionPin{
				Version:     version,
				SHA:         info.Origin.Hash,
				Source:      "proxy.golang.org",
				PublishedAt: info.Time.UTC().Format(time.RFC3339),
			})
		}
	}

	return value
}

// sortByMostRecent returns versions sorted with most-recent first.
// Valid semvers are sorted descending by version; non-semver strings
// (rare in @v/list, but possible) are appended at the end so they
// only consume budget when there are fewer than maxPinFetches valid
// versions.
func sortByMostRecent(versions []string) []string {
	var valid, invalid []string
	for _, v := range versions {
		if semver.IsValid(v) {
			valid = append(valid, v)
		} else {
			invalid = append(invalid, v)
		}
	}
	semver.Sort(valid)
	slices.Reverse(valid)
	return append(valid, invalid...)
}

// sleepWithJitter pauses for a random duration in
// [c.jitterMin, c.jitterMax]. Returns false if the context was
// cancelled during the sleep — caller treats this as a signal to
// abort the iteration loop.
//
// When c.jitterMax is zero (test seam), no sleep happens; the
// function returns ctx.Err() == nil so context cancellation still
// propagates.
func (c *Collector) sleepWithJitter(ctx context.Context) bool {
	if c.jitterMax == 0 {
		return ctx.Err() == nil
	}
	spanMs := int((c.jitterMax-c.jitterMin)/time.Millisecond) + 1
	//nolint:gosec // G404: retry jitter only — unguessability not required, math/rand/v2 is the right fit
	delay := c.jitterMin + time.Duration(mathrandv2.IntN(spanMs))*time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
