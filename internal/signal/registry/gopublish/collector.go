package gopublish

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// source is the value that lands in profile.Signal.Source for
// every emission from this collector. Constant so the string
// appears in exactly one place.
const source = "go-publish"

// defaultTTL matches the npm collector's default — 24 hours of
// freshness before refresh logic surfaces the signal for
// re-collection. Not load-bearing for v0.1 (the TTL-expiry
// machinery is ROADMAP), but emitting a sensible value keeps the
// column populated for when it does land.
const defaultTTL = 24 * time.Hour

// Collector fetches publish-provenance signals for Go modules.
// Scheme-filtered: entities whose CanonicalURI does NOT start with
// pkg:golang/ or pkg:go/ receive an empty result with no error,
// so the orchestrator can include the collector unconditionally
// in its dispatch list.
//
// Phase A signal set (four types):
//
//   - last_publish              — timestamp + version from @latest
//   - version_count             — count from @v/list
//   - transparency_log_present  — boolean from sum.golang.org/lookup
//   - publish_origin            — VCS metadata from @v/<v>.info Origin
//
// Each per-version signal depends on having a known version.
// When @latest fails (404, network, size-cap) the per-version
// signals record absences rather than re-attempting from the
// version list — the version list answers "which versions
// exist," not "which is current."
type Collector struct {
	client *Client

	// jitterMin / jitterMax bracket the random pause between
	// consecutive GetVersionInfo calls in processPinTable. Production
	// values (200-800ms) are set by NewCollector{,WithClient}; tests
	// in this package set both to 0 to bypass jitter for fast runs.
	jitterMin time.Duration
	jitterMax time.Duration
}

// NewCollector returns a Collector bound to the public Go
// data-plane endpoints (proxy.golang.org + sum.golang.org).
func NewCollector() *Collector {
	return &Collector{
		client:    NewClient(),
		jitterMin: pinFetchJitterMin,
		jitterMax: pinFetchJitterMax,
	}
}

// NewCollectorWithClient returns a Collector using the supplied
// Client. Tests typically pass a Client wired to an httptest
// server via NewClientWithBaseURL; production code uses
// NewCollector.
func NewCollectorWithClient(c *Client) *Collector {
	return &Collector{
		client:    c,
		jitterMin: pinFetchJitterMin,
		jitterMax: pinFetchJitterMax,
	}
}

// Name identifies the collector — value flows into source-tracking
// columns and the dogfood-metrics report.
func (c *Collector) Name() string { return source }

// Collect fetches publish-provenance metadata for the entity and
// emits the Phase-A signal set. Non-Go entities yield an empty
// (nil-error, empty-result) so the orchestrator doesn't need a
// pre-filter.
//
// Per signal.Collector's contract, Collect only returns a non-nil
// error when collection cannot proceed at all (e.g., entity is
// nil). Per-signal failures are recorded as absences inside the
// CollectionResult.
func (c *Collector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	modulePath, ok := extractGoModulePath(entity)
	if !ok {
		// Not a Go module entity; return an empty (non-nil)
		// CollectionResult so callers can call Signals() /
		// AbsenceCount() without a nil-guard.
		return &signal.CollectionResult{}, nil
	}

	result := &signal.CollectionResult{}
	collectedAt := time.Now().UTC()

	// ----- @v/list → version_count -----
	//
	// Order-independent of @latest: even when the proxy 404s on
	// @latest (rare but possible), an authoritative answer on the
	// version list is still useful as a vitality signal. Failure
	// here is recorded as an absence; the per-version pipeline
	// continues with whatever else worked.
	versions, listErr := c.client.GetVersionList(ctx, modulePath)
	switch {
	case listErr == nil:
		result.RecordSignal(entity.ID, "version_count", source, collectedAt, defaultTTL,
			map[string]any{
				"count":    len(versions),
				"versions": versions,
			})
	case errors.Is(listErr, ErrNotFound):
		result.RecordAbsence(entity.ID, "version_count", source,
			"module not found in proxy.golang.org", false, collectedAt)
	default:
		result.RecordFailure(entity.ID, "version_count", source,
			sanitizeFetchReason(listErr), true, collectedAt)
	}

	// ----- @latest → last_publish + the version handle for per-version signals -----
	latest, latestErr := c.client.GetLatest(ctx, modulePath)
	if latestErr != nil {
		retryable := !errors.Is(latestErr, ErrNotFound)
		reason := sanitizeFetchReason(latestErr)
		result.RecordFailure(entity.ID, "last_publish", source, reason, retryable, collectedAt)
		// Per-version signals can't fire without a known version;
		// record absences so the schema is uniform across runs.
		result.RecordAbsence(entity.ID, "transparency_log_present", source,
			"no latest version known", retryable, collectedAt)
		result.RecordAbsence(entity.ID, "publish_origin", source,
			"no latest version known", retryable, collectedAt)
		return result, nil
	}

	if latest.Version == "" || latest.Time.IsZero() {
		result.RecordAbsence(entity.ID, "last_publish", source,
			"@latest response missing Version or Time", false, collectedAt)
		result.RecordAbsence(entity.ID, "transparency_log_present", source,
			"no latest version known", false, collectedAt)
		result.RecordAbsence(entity.ID, "publish_origin", source,
			"no latest version known", false, collectedAt)
		return result, nil
	}

	result.RecordSignal(entity.ID, "last_publish", source, collectedAt, defaultTTL,
		map[string]any{
			"latest_version": latest.Version,
			"published_at":   latest.Time.UTC().Format(time.RFC3339),
			"days_ago":       int(collectedAt.Sub(latest.Time).Hours() / 24),
		})

	// ----- /lookup → transparency_log_present -----
	//
	// Distinct shape note: NotFound here is a SIGNAL, not an
	// absence — sum.golang.org definitively answered "no record."
	// Other failure classes (network, 5xx, size cap) ARE recorded
	// as absences because we couldn't tell.
	rec, lookupErr := c.client.LookupTransparency(ctx, modulePath, latest.Version)
	switch {
	case lookupErr == nil:
		result.RecordSignal(entity.ID, "transparency_log_present", source, collectedAt, defaultTTL,
			map[string]any{
				"present":         true,
				"leaf_id":         rec.LeafID,
				"version_checked": latest.Version,
			})
	case errors.Is(lookupErr, ErrNotFound):
		result.RecordSignal(entity.ID, "transparency_log_present", source, collectedAt, defaultTTL,
			map[string]any{
				"present":         false,
				"leaf_id":         int64(0),
				"version_checked": latest.Version,
			})
	default:
		result.RecordFailure(entity.ID, "transparency_log_present", source,
			sanitizeFetchReason(lookupErr), true, collectedAt)
	}

	// ----- @v/<v>.info Origin → publish_origin -----
	//
	// Pre-go-1.20 publishes lack the Origin block entirely. We
	// model that as an absence with a clear reason — analysts
	// reading the absence row understand "the proxy doesn't
	// know" vs "we couldn't ask."
	info, infoErr := c.client.GetVersionInfo(ctx, modulePath, latest.Version)
	switch {
	case infoErr != nil && errors.Is(infoErr, ErrNotFound):
		result.RecordAbsence(entity.ID, "publish_origin", source,
			fmt.Sprintf("proxy has no .info for %s", latest.Version), false, collectedAt)
	case infoErr != nil:
		result.RecordFailure(entity.ID, "publish_origin", source,
			sanitizeFetchReason(infoErr), true, collectedAt)
	case info.Origin.URL == "" && info.Origin.VCS == "" && info.Origin.Hash == "":
		result.RecordAbsence(entity.ID, "publish_origin", source,
			"proxy .info has no Origin block (pre-go-1.20 publish)", false, collectedAt)
	default:
		result.RecordSignal(entity.ID, "publish_origin", source, collectedAt, defaultTTL,
			map[string]any{
				"version_checked": latest.Version,
				"vcs":             info.Origin.VCS,
				"url":             info.Origin.URL,
				"ref":             info.Origin.Ref,
				"hash":            info.Origin.Hash,
			})
	}

	// ----- artifact_url (publication, handoff to artifact collector) -----
	//
	// Always emits when @latest succeeded — the URL is a function of
	// modulePath + version and doesn't require Origin to be present.
	// git_head is filled from Origin.Hash when the .info block has
	// it (go ≥ 1.20 publishes); empty otherwise — downstream falls
	// through to tag-match in that case.
	recordArtifactURL(result, entity.ID, c.client, modulePath, latest.Version, info, collectedAt)

	// ----- @v/<v>.info × N → version_pin_table + version_publish_burst -----
	//
	// Iterates up to maxPinFetches most-recent versions, fetching
	// the Origin hash per version with random jitter between calls.
	// The compound result is the trust anchor source-evolution uses
	// to attach matrix rows to commit SHAs. See pintable.go.
	//
	// version_publish_burst is computed from the same pin timestamps
	// — no additional network calls.
	switch {
	case listErr != nil && errors.Is(listErr, ErrNotFound):
		result.RecordAbsence(entity.ID, "version_pin_table", source,
			"module not found in proxy.golang.org", false, collectedAt)
		result.RecordAbsence(entity.ID, "version_publish_burst", source,
			"module not found in proxy.golang.org", false, collectedAt)
	case listErr != nil:
		result.RecordFailure(entity.ID, "version_pin_table", source,
			sanitizeFetchReason(listErr), true, collectedAt)
		result.RecordFailure(entity.ID, "version_publish_burst", source,
			sanitizeFetchReason(listErr), true, collectedAt)
	case len(versions) == 0:
		result.RecordAbsence(entity.ID, "version_pin_table", source,
			"@v/list returned empty version set", false, collectedAt)
		result.RecordAbsence(entity.ID, "version_publish_burst", source,
			"@v/list returned empty version set", false, collectedAt)
	default:
		pinTable := c.processPinTable(ctx, modulePath, versions)
		result.RecordSignal(entity.ID, "version_pin_table", source, collectedAt, defaultTTL, pinTable)
		recordVersionPublishBurst(result, entity.ID, pinTable, collectedAt)
	}

	return result, nil
}

// recordArtifactURL emits the artifact_url handoff signal that the
// artifact-vs-repo divergence collector consumes from the in-run
// accumulator.
//
// Go modules differ from npm/cargo/pypi/gem in how strong the
// publisher provenance is:
//
//  1. The .zip URL is constructed from the proxy base + encoded
//     module path + encoded version. Stable canonical form per
//     the module-proxy spec (https://go.dev/ref/mod#goproxy-protocol).
//
//  2. git_head is populated from Origin.Hash — the publisher-
//     stamped commit SHA the proxy records at publish time when
//     `go mod` ≥ 1.20 publishes against a VCS. Same provenance
//     strength as cargo's vcs_info (the SHA is recorded by the
//     proxy from the VCS, not supplied by the publisher),
//     yielding PairConfidenceExactGitHead at the downstream
//     collector. For pre-go-1.20 publishes Origin is absent and
//     git_head ships empty — pair resolution falls through to
//     tag-match.
//
// integrity ships empty: the proxy serves the ziphash on a
// separate /@v/<version>.ziphash endpoint we don't currently
// fetch. The downstream collector tolerates empty integrity
// (it's opaque to current consumers; reserved for future
// cross-checking against signatory's own computed sha256).
func recordArtifactURL(result *signal.CollectionResult, entityID string,
	client *Client, modulePath, version string, info *VersionInfo,
	collectedAt time.Time) {

	const sigType = "artifact_url"

	gitHead := ""
	if info != nil {
		gitHead = info.Origin.Hash
	}

	result.RecordSignal(entityID, sigType, source, collectedAt, defaultTTL,
		map[string]any{
			"url":       client.ZipURL(modulePath, version),
			"version":   version,
			"integrity": "", // ziphash endpoint not yet wired
			"git_head":  gitHead,
		})
}

// burstThreshold is the maximum time span across the version window
// that triggers burst detection. 72 hours matches the BufferZoneCorp
// campaign cadence and the gem/npm collectors' threshold.
const burstThreshold = 72 * time.Hour

// recordVersionPublishBurst detects whether multiple versions were
// published within a short time window (burstThreshold). Computed from
// the pin table's PublishedAt timestamps — no additional HTTP calls
// beyond what the pin table already fetched.
func recordVersionPublishBurst(result *signal.CollectionResult, entityID string,
	pinTable VersionPinTableValue, collectedAt time.Time) {

	// Only pins with successful fetches have timestamps.
	if len(pinTable.Pins) < 2 {
		result.RecordSignal(entityID, "version_publish_burst", source, collectedAt, defaultTTL,
			map[string]any{
				"burst_detected":     false,
				"versions_in_window": len(pinTable.Pins),
				"window_hours":       0,
				"versions_checked":   pinTable.VersionCountProcessed,
			})
		return
	}

	// Parse timestamps and find oldest/newest.
	var newest, oldest time.Time
	for _, pin := range pinTable.Pins {
		t, err := time.Parse(time.RFC3339, pin.PublishedAt)
		if err != nil {
			continue
		}
		if newest.IsZero() || t.After(newest) {
			newest = t
		}
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
	}

	if newest.IsZero() || oldest.IsZero() {
		result.RecordSignal(entityID, "version_publish_burst", source, collectedAt, defaultTTL,
			map[string]any{
				"burst_detected":     false,
				"versions_in_window": len(pinTable.Pins),
				"window_hours":       0,
				"versions_checked":   pinTable.VersionCountProcessed,
			})
		return
	}

	span := newest.Sub(oldest)
	burst := len(pinTable.Pins) >= 3 && span <= burstThreshold

	result.RecordSignal(entityID, "version_publish_burst", source, collectedAt, defaultTTL,
		map[string]any{
			"burst_detected":     burst,
			"versions_in_window": len(pinTable.Pins),
			"window_hours":       int(span.Hours()),
			"versions_checked":   pinTable.VersionCountProcessed,
		})
}

// extractGoModulePath pulls the module path out of a Go-ecosystem
// entity's canonical URI. Accepts both the canonical pkg:golang/
// prefix and the legacy pkg:go/ prefix — the URI canonicalization
// migration moved the project to pkg:golang, but in-store entities
// from before that change still carry pkg:go.
func extractGoModulePath(entity *profile.Entity) (string, bool) {
	if entity == nil {
		return "", false
	}
	for _, prefix := range []string{"pkg:golang/", "pkg:go/"} {
		if rest, ok := strings.CutPrefix(entity.CanonicalURI, prefix); ok {
			if rest == "" {
				return "", false
			}
			return rest, true
		}
	}
	return "", false
}

// sanitizeFetchReason converts a fetch error into a reason string
// safe to persist in the signal row. Mirrors the npm collector's
// handler — same shape so cross-collector failure diagnostics
// stay uniform.
func sanitizeFetchReason(err error) string {
	if errors.Is(err, ErrNotFound) {
		return "module not found"
	}
	if errors.Is(err, context.Canceled) {
		return "collection canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "collection timed out"
	}
	msg := err.Error()
	const maxLen = 200
	if len(msg) > maxLen {
		msg = msg[:maxLen] + "…"
	}
	return msg
}
