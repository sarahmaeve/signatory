package gem

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

const (
	collectorSource = "gem-registry"
	defaultTTL      = 24 * time.Hour

	// crossVersionWindow bounds how many recent versions the
	// longitudinal signals examine. Matches cargo's window.
	crossVersionWindow = 10

	// burstThreshold is the maximum duration between oldest and newest
	// version in the window that triggers a burst detection. 72 hours
	// matches the BufferZoneCorp campaign cadence.
	burstThreshold = 72 * time.Hour
)

// EntityStore is the narrow interface the gem collector uses to mint
// identity:rubygems/<handle> entity rows for discovered owners.
type EntityStore interface {
	EnsureEntityByCanonicalURI(ctx context.Context, uri, shortName string) (*profile.Entity, bool, error)
}

// Collector fetches registry-side signals for rubygems.org-hosted gems.
// Scheme-filtered: entities whose CanonicalURI does NOT start with
// pkg:gem/ receive an empty result with no error.
type Collector struct {
	client      *Client
	entityStore EntityStore
}

// NewCollector returns a Collector bound to the public rubygems.org.
func NewCollector() *Collector {
	return &Collector{client: NewClient()}
}

// NewCollectorWithClient returns a Collector using the supplied Client.
func NewCollectorWithClient(c *Client) *Collector {
	return &Collector{client: c}
}

// WithEntityStore wires an EntityStore so owner-entity minting fires.
func (c *Collector) WithEntityStore(s EntityStore) *Collector {
	c.entityStore = s
	return c
}

// Name identifies the collector.
func (c *Collector) Name() string { return collectorSource }

// Collect fetches registry metadata for the entity and emits signals.
// Non-gem entities yield an empty (non-nil) result with no error.
func (c *Collector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	packageName, ok := extractGemPackageName(entity)
	if !ok {
		return &signal.CollectionResult{}, nil
	}

	result := &signal.CollectionResult{}
	collectedAt := time.Now().UTC()

	// ----- gem info -----
	gem, gemErr := c.client.GetGem(ctx, packageName)
	if gemErr != nil {
		retryable := !errors.Is(gemErr, ErrNotFound)
		result.RecordFailure(entity.ID, "last_publish", collectorSource,
			sanitizeFetchReason(gemErr), retryable, collectedAt)
		return result, nil
	}

	// ----- versions -----
	versions, versionsErr := c.client.GetVersions(ctx, packageName)
	if versionsErr != nil {
		retryable := !errors.Is(versionsErr, ErrNotFound)
		result.RecordFailure(entity.ID, "version_count", collectorSource,
			sanitizeFetchReason(versionsErr), retryable, collectedAt)
	}

	// Emit signals from gem info.
	recordRecentDownloads(result, entity.ID, gem, collectedAt)
	recordMFARequired(result, entity.ID, gem, collectedAt)

	// Emit signals from versions (if available).
	if versionsErr == nil {
		recordLastPublish(result, entity.ID, versions, collectedAt)
		recordVersionCount(result, entity.ID, versions, collectedAt)
		recordYankedReleaseCount(result, entity.ID, versions, collectedAt)
		recordCrossVersionSignals(result, entity.ID, versions, collectedAt)
		recordArtifactURL(result, entity.ID, packageName, versions, collectedAt)
	}

	// ----- owners -----
	owners, ownersErr := c.client.GetOwners(ctx, packageName)
	if ownersErr != nil {
		retryable := !errors.Is(ownersErr, ErrNotFound) && !errors.Is(ownersErr, ErrUnauthorized)
		reason := sanitizeFetchReason(ownersErr)
		result.RecordFailure(entity.ID, "maintainer_count", collectorSource, reason, retryable, collectedAt)
		result.RecordFailure(entity.ID, "owner_count", collectorSource, reason, retryable, collectedAt)
	} else {
		recordMaintainerCount(result, entity.ID, owners, collectedAt)
		recordOwnerCount(result, entity.ID, owners, collectedAt)
	}

	// ----- publisher entity minting -----
	c.ensureOwnerEntities(ctx, owners)

	return result, nil
}

// extractGemPackageName pulls the gem name from a pkg:gem/* URI.
func extractGemPackageName(entity *profile.Entity) (string, bool) {
	if entity == nil {
		return "", false
	}
	const prefix = "pkg:gem/"
	if !strings.HasPrefix(entity.CanonicalURI, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(entity.CanonicalURI, prefix)
	if name == "" {
		return "", false
	}
	return name, true
}

// sanitizeFetchReason produces a safe absence/failure reason string.
func sanitizeFetchReason(err error) string {
	if errors.Is(err, ErrNotFound) {
		return "gem not found on rubygems.org"
	}
	if errors.Is(err, ErrUnauthorized) {
		return "rubygems.org owners endpoint requires authentication"
	}
	return fmt.Sprintf("rubygems.org fetch failed: %v", err)
}

// recordLastPublish emits last_publish from the newest non-yanked,
// non-prerelease version.
func recordLastPublish(result *signal.CollectionResult, entityID string,
	versions []VersionEntry, collectedAt time.Time) {

	for _, v := range versions {
		if v.Yanked || v.Prerelease {
			continue
		}
		t, err := time.Parse(time.RFC3339, v.CreatedAt)
		if err != nil {
			continue
		}
		result.RecordSignal(entityID, "last_publish", collectorSource, collectedAt, defaultTTL,
			map[string]any{
				"latest_version": v.Number,
				"published_at":   t.UTC().Format(time.RFC3339),
				"days_ago":       int(collectedAt.Sub(t).Hours() / 24),
			})
		return
	}

	result.RecordAbsence(entityID, "last_publish", collectorSource,
		"no non-yanked, non-prerelease versions found", false, collectedAt)
}

// recordVersionCount emits the total number of versions.
func recordVersionCount(result *signal.CollectionResult, entityID string,
	versions []VersionEntry, collectedAt time.Time) {

	result.RecordSignal(entityID, "version_count", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"count": len(versions),
		})
}

// recordRecentDownloads emits the version_downloads count from the
// gem info response (downloads of the current version, roughly
// equivalent to "recent" downloads).
func recordRecentDownloads(result *signal.CollectionResult, entityID string,
	gem *GemResponse, collectedAt time.Time) {

	result.RecordSignal(entityID, "recent_downloads", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"count":  gem.VersionDownloads,
			"window": "current version (rubygems.org version_downloads)",
		})
}

// recordYankedReleaseCount counts yanked versions.
func recordYankedReleaseCount(result *signal.CollectionResult, entityID string,
	versions []VersionEntry, collectedAt time.Time) {

	yanked := 0
	for _, v := range versions {
		if v.Yanked {
			yanked++
		}
	}

	result.RecordSignal(entityID, "yanked_release_count", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"count":          yanked,
			"total_versions": len(versions),
		})
}

// recordMaintainerCount emits the owner count.
func recordMaintainerCount(result *signal.CollectionResult, entityID string,
	owners []OwnerEntry, collectedAt time.Time) {

	if len(owners) == 0 {
		result.RecordAbsence(entityID, "maintainer_count", collectorSource,
			"owners endpoint returned empty list", false, collectedAt)
		return
	}

	handles := make([]string, 0, len(owners))
	for _, o := range owners {
		if o.Handle != "" {
			handles = append(handles, o.Handle)
		}
	}

	result.RecordSignal(entityID, "maintainer_count", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"count":  len(owners),
			"logins": handles,
		})
}

// recordOwnerCount emits the bus-factor owner count.
func recordOwnerCount(result *signal.CollectionResult, entityID string,
	owners []OwnerEntry, collectedAt time.Time) {

	result.RecordSignal(entityID, "owner_count", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"count": len(owners),
		})
}

// recordArtifactURL emits the artifact_url handoff signal that the
// artifact-vs-repo divergence collector consumes from the in-run
// accumulator.
//
// Gem differs from npm/cargo/pypi in two ways the downstream
// collector relies on:
//
//  1. The .gem URL is constructed from name + version: the
//     canonical form is https://rubygems.org/downloads/{name}-{version}.gem.
//     Registry metadata never carries the URL directly.
//
//  2. rubygems.org exposes no publisher-stamped commit SHA. git_head
//     is emitted empty; pair resolution falls through to tag-match
//     against the local clone.
//
// Only the latest non-yanked, non-prerelease, ruby-platform version
// is chosen. Native-platform builds (e.g. "x86_64-linux") are
// pre-compiled artifacts whose contents diverge from source by
// design and would produce false-positive divergence noise — the
// `.gem`-vs-repo signal is meaningful only against the pure-ruby
// platform that ships actual source code.
//
// integrity carries the rubygems.org-supplied sha256 from the
// version entry.
func recordArtifactURL(result *signal.CollectionResult, entityID, packageName string,
	versions []VersionEntry, collectedAt time.Time) {

	const sigType = "artifact_url"

	for _, v := range versions {
		if v.Yanked || v.Prerelease {
			continue
		}
		// Platform "ruby" or empty means pure-ruby gem (the source-
		// shipping form). Anything else (e.g. "x86_64-linux",
		// "java") is a pre-compiled artifact and the wrong surface
		// for tarball-vs-repo comparison.
		if v.Platform != "" && v.Platform != "ruby" {
			continue
		}
		url := fmt.Sprintf("https://rubygems.org/downloads/%s-%s.gem",
			packageName, v.Number)
		result.RecordSignal(entityID, sigType, collectorSource, collectedAt, defaultTTL,
			map[string]any{
				"url":       url,
				"version":   v.Number,
				"integrity": v.SHA,
				"git_head":  "", // rubygems.org does not expose this; falls through to tag-match.
			})
		return
	}

	result.RecordAbsence(entityID, sigType, collectorSource,
		"no non-yanked, non-prerelease, ruby-platform version", false, collectedAt)
}

// recordMFARequired emits whether the gem mandates MFA for pushes.
func recordMFARequired(result *signal.CollectionResult, entityID string,
	gem *GemResponse, collectedAt time.Time) {

	result.RecordSignal(entityID, "mfa_required", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"required": gem.MFARequired,
		})
}

// --- Longitudinal (cross-version) signals ---

// gemVersionRecord is the per-version fact-set the longitudinal signals
// operate over. Decoupled from the wire type.
type gemVersionRecord struct {
	version   string
	createdAt time.Time
	platform  string
	authors   string
}

// recentGemVersions returns up to n non-yanked, non-prerelease versions
// sorted newest-first by publish timestamp.
func recentGemVersions(versions []VersionEntry, n int) []gemVersionRecord {
	records := make([]gemVersionRecord, 0, len(versions))
	for _, v := range versions {
		if v.Yanked || v.Prerelease {
			continue
		}
		t, err := time.Parse(time.RFC3339, v.CreatedAt)
		if err != nil {
			continue
		}
		records = append(records, gemVersionRecord{
			version:   v.Number,
			createdAt: t,
			platform:  v.Platform,
			authors:   v.Authors,
		})
	}

	// Sort newest-first by publish time. Tiebreaker: lexically-greater
	// version string first.
	slices.SortStableFunc(records, func(a, b gemVersionRecord) int {
		if a.createdAt.Equal(b.createdAt) {
			return cmp.Compare(b.version, a.version)
		}
		if a.createdAt.After(b.createdAt) {
			return -1
		}
		return 1
	})

	if len(records) > n {
		records = records[:n]
	}
	return records
}

// recordCrossVersionSignals emits the four longitudinal signals from a
// window of recent versions.
func recordCrossVersionSignals(result *signal.CollectionResult, entityID string,
	versions []VersionEntry, collectedAt time.Time) {

	recent := recentGemVersions(versions, crossVersionWindow)
	if len(recent) == 0 {
		reason := "no orderable non-yanked, non-prerelease versions"
		result.RecordAbsence(entityID, "native_extension_present", collectorSource, reason, false, collectedAt)
		result.RecordAbsence(entityID, "native_extension_introduced", collectorSource, reason, false, collectedAt)
		result.RecordAbsence(entityID, "version_publish_burst", collectorSource, reason, false, collectedAt)
		result.RecordAbsence(entityID, "author_drift", collectorSource, reason, false, collectedAt)
		return
	}

	recordNativeExtensionPresent(result, entityID, recent, collectedAt)
	recordNativeExtensionIntroduced(result, entityID, recent, collectedAt)
	recordVersionPublishBurst(result, entityID, recent, collectedAt)
	recordAuthorDrift(result, entityID, recent, collectedAt)
}

// recordNativeExtensionPresent emits whether the latest version has
// a platform other than "ruby" (indicating native C extension / extconf.rb).
func recordNativeExtensionPresent(result *signal.CollectionResult, entityID string,
	recent []gemVersionRecord, collectedAt time.Time) {

	latest := recent[0]
	hasExtension := latest.platform != "" && latest.platform != "ruby"

	result.RecordSignal(entityID, "native_extension_present", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"present":          hasExtension,
			"latest_platform":  latest.platform,
			"versions_checked": len(recent),
		})
}

// recordNativeExtensionIntroduced detects whether a native extension
// appeared in a recent version where older versions were pure Ruby —
// the gem analog of build_script_introduced.
func recordNativeExtensionIntroduced(result *signal.CollectionResult, entityID string,
	recent []gemVersionRecord, collectedAt time.Time) {

	latestHasExtension := recent[0].platform != "" && recent[0].platform != "ruby"

	priorWithout := 0
	for i := 1; i < len(recent); i++ {
		if recent[i].platform == "" || recent[i].platform == "ruby" {
			priorWithout++
		}
	}

	introduced := latestHasExtension && priorWithout > 0

	introducedAtVersion := ""
	if introduced {
		// Walk oldest→newest to find the first version with extension.
		for i := len(recent) - 1; i >= 0; i-- {
			if recent[i].platform != "" && recent[i].platform != "ruby" {
				introducedAtVersion = recent[i].version
				break
			}
		}
	}

	result.RecordSignal(entityID, "native_extension_introduced", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"present_in_latest":      latestHasExtension,
			"introduced_recently":    introduced,
			"introduced_at_version":  introducedAtVersion,
			"prior_versions_without": priorWithout,
			"versions_checked":       len(recent),
		})
}

// recordVersionPublishBurst detects whether multiple versions were
// published within a short time window (burstThreshold). The
// BufferZoneCorp campaign published 4 versions in 72 hours.
func recordVersionPublishBurst(result *signal.CollectionResult, entityID string,
	recent []gemVersionRecord, collectedAt time.Time) {

	if len(recent) < 2 {
		result.RecordSignal(entityID, "version_publish_burst", collectorSource, collectedAt, defaultTTL,
			map[string]any{
				"burst_detected":     false,
				"versions_in_window": len(recent),
				"window_hours":       0,
				"versions_checked":   len(recent),
			})
		return
	}

	// newest is recent[0], oldest is recent[len-1]
	newest := recent[0].createdAt
	oldest := recent[len(recent)-1].createdAt
	span := newest.Sub(oldest)

	burst := len(recent) >= 3 && span <= burstThreshold

	result.RecordSignal(entityID, "version_publish_burst", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"burst_detected":     burst,
			"versions_in_window": len(recent),
			"window_hours":       int(span.Hours()),
			"versions_checked":   len(recent),
		})
}

// recordAuthorDrift counts distinct author strings across the version
// window. A change in authors between versions may indicate account
// takeover or maintainer handoff.
func recordAuthorDrift(result *signal.CollectionResult, entityID string,
	recent []gemVersionRecord, collectedAt time.Time) {

	authors := map[string]struct{}{}
	for _, r := range recent {
		if r.authors != "" {
			authors[r.authors] = struct{}{}
		}
	}

	authorList := make([]string, 0, len(authors))
	for a := range authors {
		authorList = append(authorList, a)
	}
	slices.Sort(authorList)

	result.RecordSignal(entityID, "author_drift", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"distinct_authors": len(authors),
			"authors":          authorList,
			"versions_checked": len(recent),
		})
}

// ensureOwnerEntities mints identity:rubygems/<handle> entity rows.
func (c *Collector) ensureOwnerEntities(ctx context.Context, owners []OwnerEntry) {
	if c.entityStore == nil || owners == nil {
		return
	}

	for _, o := range owners {
		if o.Handle == "" {
			continue
		}
		uri := "identity:rubygems/" + o.Handle
		if _, _, err := c.entityStore.EnsureEntityByCanonicalURI(ctx, uri, o.Handle); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to ensure gem owner entity %s: %v\n", uri, err)
		}
	}
}
