package pypi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// crossVersionWindow bounds the number of recent versions consulted
// for longitudinal signals. Matches npm, cargo, gem, and maven.
const crossVersionWindow = 10

// burstThreshold is the maximum duration between oldest and newest
// version in the window that triggers a burst detection. 72 hours
// matches the npm, cargo, gem, and maven collectors.
const burstThreshold = 72 * time.Hour

// source is the collector's name — the value lands in
// profile.Signal.Source for every emission and in the orchestrator's
// progress narration. Kept as a const so the string appears in
// exactly one place. The cascade resolver
// (internal/store/effective_burn.go platformForRegistrySource)
// dispatches on this exact string to map maintainer_count signals
// from this collector to identity:pypi/<login> URIs; renaming this
// requires updating that switch in the same change.
const source = "pypi-registry"

// defaultTTL matches the npm and github collectors. The TTL-expiry
// machinery is should-do; emitting a sensible value keeps the column
// populated.
const defaultTTL = 24 * time.Hour

// EntityStore is the narrow interface the pypi collector uses to
// mint identity:pypi/<login> entity rows for the publisher logins
// extractable from a project's PyPI metadata. Defined here
// (consumer-side) so the collector doesn't depend on the full
// internal/store package — any type that implements
// EnsureEntityByCanonicalURI satisfies it via structural typing.
//
// Optional: nil-safe via the WithEntityStore setter, mirroring the
// github (Path A) and npm (Path C) collectors. Tests that don't
// care about publisher-entity emission construct collectors without
// calling WithEntityStore, and the minting branch in Collect
// silently skips when c.entityStore is nil.
//
// In production, cmd/signatory/collectors.go threads the
// orchestrator's *store.SQLite through opts.EntityStore so every
// analyze run populates publisher entities for pypi-ecosystem
// targets — closing the third major ecosystem after github and
// npm (entity-burn1.md "Pending work #1").
type EntityStore interface {
	EnsureEntityByCanonicalURI(ctx context.Context, uri, shortName string) (*profile.Entity, bool, error)
}

// Collector fetches registry-side signals for PyPI-hosted packages.
// Scheme-filtered: entities whose CanonicalURI does NOT start with
// pkg:pypi/ receive an empty result with no error, so the
// orchestrator can include the collector unconditionally in its
// dispatch list.
//
// v0.1 emits a single signal — maintainer_count — feeding the
// cascade resolver's pypi branch. Additional pypi signals
// (last_publish, license, downloads, ...) are out-of-scope for the
// publisher-entity slice and land separately.
type Collector struct {
	client      *Client
	entityStore EntityStore // optional — see EntityStore docstring
}

// NewCollector returns a Collector bound to the public PyPI registry.
func NewCollector() *Collector {
	return &Collector{client: NewClient()}
}

// NewCollectorWithClient returns a Collector using the supplied
// Client. Primary use case: tests pointing the client at an httptest
// server via NewClientWithBaseURL. Production code uses NewCollector.
func NewCollectorWithClient(c *Client) *Collector {
	return &Collector{client: c}
}

// WithEntityStore wires an EntityStore into the collector so
// publisher-entity minting fires during each Collect run. Returns
// the receiver so the call chains cleanly with the constructors —
// matches the github and npm collectors' setter pattern.
//
// Setter rather than constructor parameter so existing call sites
// (NewCollector / NewCollectorWithClient) keep their signatures
// and pre-publisher-entity tests continue to compile unchanged.
func (c *Collector) WithEntityStore(s EntityStore) *Collector {
	c.entityStore = s
	return c
}

// Name identifies the collector. Returned to orchestrator log
// lines (e.g., "[pypi-registry] 1 signal, 0 absences"). Matches
// the source constant so log narration and stored-signal source
// strings stay consistent.
func (c *Collector) Name() string { return source }

// Collect fetches registry metadata for the entity and emits the
// v0.1 publisher-entity signal set. Non-pypi entities yield an
// empty (nil, nil) result so the orchestrator doesn't need a
// pre-filter.
//
// Per signal.Collector contract: only return a non-nil error when
// collection cannot proceed at all (e.g., the entity URI is
// unparseable as a pypi reference). Per-signal failures are
// recorded as absences in the CollectionResult, not returned as
// errors.
func (c *Collector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	packageName, ok := extractPyPIPackageName(entity)
	if !ok {
		// Not a pypi entity; nothing to do. Return an empty result
		// (not nil) so callers can safely call Signals()/AbsenceCount()
		// without a nil-guard.
		return &signal.CollectionResult{}, nil
	}

	result := &signal.CollectionResult{}
	collectedAt := time.Now().UTC()

	proj, err := c.client.GetProject(ctx, packageName)
	if err != nil {
		// Treat fetch failure (404, network, size-cap, ...) as an
		// absence so the entity profile reflects "we tried, registry
		// said no / couldn't reach." Retryable is true except for
		// ErrNotFound, which is definitive.
		retryable := !errors.Is(err, ErrNotFound)
		result.RecordFailure(entity.ID, "maintainer_count", source,
			sanitizeFetchReason(err), retryable, collectedAt)
		return result, nil
	}

	// ----- maintainer_count + publisher-entity minting -----
	logins := extractPyPILogins(&proj.Info)
	if len(logins) == 0 {
		reason := "no login-shaped value in info.maintainer / info.author / info.maintainers"
		result.RecordAbsence(entity.ID, "maintainer_count", source,
			reason, false, collectedAt)
	} else {
		c.ensurePublisherEntities(ctx, logins)
		result.RecordSignal(entity.ID, "maintainer_count", source, collectedAt, defaultTTL,
			map[string]any{
				"count":  len(logins),
				"logins": logins,
			})
	}

	// ----- version_count (vitality) -----
	recordVersionCount(result, entity.ID, proj, collectedAt)

	// ----- yanked_release_count (publication) -----
	recordYankedReleaseCount(result, entity.ID, proj, collectedAt)

	// ----- last_publish, version_publish_burst, sdist signals -----
	recordReleaseSignals(result, entity.ID, proj, collectedAt)

	return result, nil
}

// versionRecord is the per-version fact-set the longitudinal signals
// operate over. Built once from proj.Releases and iterated by multiple
// signal emitters.
type versionRecord struct {
	version     string
	publishedAt time.Time
	sdistOnly   bool // true when ALL dists for this version are sdist (no wheels)
	yanked      bool // true when ANY dist for this version is yanked
}

// recordVersionCount emits the total number of published versions.
func recordVersionCount(result *signal.CollectionResult, entityID string,
	proj *Project, collectedAt time.Time) {

	result.RecordSignal(entityID, "version_count", source, collectedAt, defaultTTL,
		map[string]any{
			"count": len(proj.Releases),
		})
}

// recordYankedReleaseCount counts versions where any distribution is
// marked yanked. Zero additional HTTP calls — derived from the
// existing releases map.
func recordYankedReleaseCount(result *signal.CollectionResult, entityID string,
	proj *Project, collectedAt time.Time) {

	yanked := 0
	for _, dists := range proj.Releases {
		for _, d := range dists {
			if d.Yanked {
				yanked++
				break // one yanked dist means the whole version is yanked
			}
		}
	}

	result.RecordSignal(entityID, "yanked_release_count", source, collectedAt, defaultTTL,
		map[string]any{
			"count":          yanked,
			"total_versions": len(proj.Releases),
		})
}

// isSdistOnly returns true when every distribution in dists is an
// sdist (no wheels, eggs, or other pre-built formats). An empty
// dist list returns false (no distributions means no install path).
func isSdistOnly(dists []Distribution) bool {
	if len(dists) == 0 {
		return false
	}
	for _, d := range dists {
		if d.PackageType != "sdist" {
			return false
		}
	}
	return true
}

// isYanked returns true when any distribution in the version is
// marked yanked.
func isYanked(dists []Distribution) bool {
	for _, d := range dists {
		if d.Yanked {
			return true
		}
	}
	return false
}

// recordReleaseSignals derives last_publish, version_publish_burst,
// sdist_only_present, and sdist_only_introduced from the releases map.
// All share the sorted version records, so they're computed together.
func recordReleaseSignals(result *signal.CollectionResult, entityID string,
	proj *Project, collectedAt time.Time) {

	emptyBurst := func() {
		result.RecordSignal(entityID, "version_publish_burst", source, collectedAt, defaultTTL,
			map[string]any{
				"burst_detected":     false,
				"versions_in_window": 0,
				"window_hours":       0,
				"versions_checked":   0,
			})
	}

	if len(proj.Releases) == 0 {
		result.RecordAbsence(entityID, "last_publish", source,
			"no releases in PyPI response", false, collectedAt)
		emptyBurst()
		result.RecordAbsence(entityID, "sdist_only_present", source,
			"no releases in PyPI response", false, collectedAt)
		result.RecordAbsence(entityID, "sdist_only_introduced", source,
			"no releases in PyPI response", false, collectedAt)
		return
	}

	// Build version records from the releases map.
	var records []versionRecord
	for ver, dists := range proj.Releases {
		if len(dists) == 0 {
			continue
		}
		t, err := time.Parse(time.RFC3339, dists[0].UploadTimeISO)
		if err != nil {
			continue
		}
		records = append(records, versionRecord{
			version:     ver,
			publishedAt: t,
			sdistOnly:   isSdistOnly(dists),
			yanked:      isYanked(dists),
		})
	}

	if len(records) == 0 {
		result.RecordAbsence(entityID, "last_publish", source,
			"no parseable timestamps in releases", false, collectedAt)
		emptyBurst()
		result.RecordAbsence(entityID, "sdist_only_present", source,
			"no parseable releases", false, collectedAt)
		result.RecordAbsence(entityID, "sdist_only_introduced", source,
			"no parseable releases", false, collectedAt)
		return
	}

	// Sort newest-first.
	slices.SortFunc(records, func(a, b versionRecord) int {
		if a.publishedAt.Equal(b.publishedAt) {
			return cmp.Compare(b.version, a.version)
		}
		return b.publishedAt.Compare(a.publishedAt)
	})

	// ----- last_publish -----
	newest := records[0]
	result.RecordSignal(entityID, "last_publish", source, collectedAt, defaultTTL,
		map[string]any{
			"latest_version": newest.version,
			"published_at":   newest.publishedAt.UTC().Format(time.RFC3339),
			"days_ago":       int(collectedAt.Sub(newest.publishedAt).Hours() / 24),
		})

	// ----- sdist_only_present -----
	result.RecordSignal(entityID, "sdist_only_present", source, collectedAt, defaultTTL,
		map[string]any{
			"present":         newest.sdistOnly,
			"version_checked": newest.version,
		})

	// ----- version_publish_burst + sdist_only_introduced -----
	window := records
	if len(window) > crossVersionWindow {
		window = window[:crossVersionWindow]
	}

	recordSdistOnlyIntroduced(result, entityID, window, collectedAt)

	if len(window) < 2 {
		result.RecordSignal(entityID, "version_publish_burst", source, collectedAt, defaultTTL,
			map[string]any{
				"burst_detected":     false,
				"versions_in_window": len(window),
				"window_hours":       0,
				"versions_checked":   len(window),
			})
		return
	}

	span := window[0].publishedAt.Sub(window[len(window)-1].publishedAt)
	burst := len(window) >= 3 && span <= burstThreshold

	result.RecordSignal(entityID, "version_publish_burst", source, collectedAt, defaultTTL,
		map[string]any{
			"burst_detected":     burst,
			"versions_in_window": len(window),
			"window_hours":       int(span.Hours()),
			"versions_checked":   len(window),
		})
}

// recordSdistOnlyIntroduced detects whether the latest version is
// sdist-only where prior versions in the window had wheels. The
// Python analog of postinstall_introduced: dropping wheels forces
// setup.py execution on every pip install.
func recordSdistOnlyIntroduced(result *signal.CollectionResult, entityID string,
	recent []versionRecord, collectedAt time.Time) {

	if len(recent) == 0 {
		result.RecordAbsence(entityID, "sdist_only_introduced", source,
			"no orderable versions", false, collectedAt)
		return
	}

	latestSdistOnly := recent[0].sdistOnly

	// Count older versions that were NOT sdist-only (i.e., had wheels).
	priorWithout := 0
	for i := 1; i < len(recent); i++ {
		if !recent[i].sdistOnly {
			priorWithout++
		}
	}

	introduced := latestSdistOnly && priorWithout > 0

	introducedAtVersion := ""
	if introduced {
		// Walk oldest→newest to find the first version that is sdist-only.
		for i := len(recent) - 1; i >= 0; i-- {
			if recent[i].sdistOnly {
				introducedAtVersion = recent[i].version
				break
			}
		}
	}

	result.RecordSignal(entityID, "sdist_only_introduced", source, collectedAt, defaultTTL,
		map[string]any{
			"present_in_latest":      latestSdistOnly,
			"introduced_recently":    introduced,
			"introduced_at_version":  introducedAtVersion,
			"prior_versions_without": priorWithout,
			"versions_checked":       len(recent),
		})
}

// ensurePublisherEntities mints identity:pypi/<login> rows for each
// extracted login. Failures are logged-and-continued: a transient
// store error on one login doesn't abort the whole sweep, because
// each entity row is independent and the next analyze run re-
// attempts. The per-error stderr line surfaces systemic store
// issues so an operator notices.
//
// Skipped silently when no EntityStore was wired (pre-EntityStore
// tests construct collectors without one and continue to work).
func (c *Collector) ensurePublisherEntities(ctx context.Context, logins []string) {
	if c.entityStore == nil {
		return
	}
	for _, login := range logins {
		uri := profile.CanonicalIdentityURI("pypi", login)
		if _, _, err := c.entityStore.EnsureEntityByCanonicalURI(ctx, uri, login); err != nil {
			// Don't propagate — the signal emission is independent
			// of entity-row minting, and the next analyze run re-
			// attempts. Surface to stderr so systemic store failures
			// are visible. Matches the github and npm collectors'
			// policy.
			fmt.Fprintf(os.Stderr, "warning: failed to ensure pypi publisher entity %s: %v\n", uri, err)
		}
	}
}

// extractPyPIPackageName pulls the pypi package name out of an
// entity's canonical URI. Returns (name, true) for pkg:pypi/* URIs
// and (_, false) for anything else.
//
// The name is the path-segment value verbatim — no PEP 503
// re-normalization. Upstream URI construction
// (profile.CanonicalPackageURI for "pypi") already normalized; if
// a future caller bypasses that path, the registry endpoint accepts
// both forms and resolves to the same project.
func extractPyPIPackageName(entity *profile.Entity) (string, bool) {
	if entity == nil {
		return "", false
	}
	const prefix = "pkg:pypi/"
	if !strings.HasPrefix(entity.CanonicalURI, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(entity.CanonicalURI, prefix)
	if name == "" {
		return "", false
	}
	return name, true
}

// sanitizeFetchReason converts a fetch error into a reason string
// safe to persist in the signal row. The pypi client (client.go)
// guarantees no response body is in the error string (#93's lesson
// applies symmetrically to PyPI), so this is mostly a policy-layer
// formatter that trims wrapping noise and keeps the reason short.
func sanitizeFetchReason(err error) string {
	if errors.Is(err, ErrNotFound) {
		return "package not found in pypi registry"
	}
	if errors.Is(err, context.Canceled) {
		return "collection canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "collection timed out"
	}
	// Unknown error classes: fall through to the message. The client
	// guarantees no response body is inside; the remaining text is
	// safe to persist.
	msg := err.Error()
	const maxLen = 200
	if len(msg) > maxLen {
		msg = msg[:maxLen] + "…"
	}
	return msg
}
