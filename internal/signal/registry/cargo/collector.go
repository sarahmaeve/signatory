package cargo

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
// for longitudinal signals. Ten is enough to establish a pattern
// without paying for parsing hundreds of historical entries. Matches
// the npm collector's window.
const crossVersionWindow = 10

// collectorSource is the collector's name, the value that lands in
// profile.Signal.Source for every emission.
const collectorSource = "cargo-registry"

// defaultTTL is how long an emitted signal is considered fresh.
// Matches npm and PyPI.
const defaultTTL = 24 * time.Hour

// EntityStore is the narrow interface the cargo collector uses to
// mint identity:cargo/<login> entity rows for the owners and
// publishers it observes. Optional: nil-safe via WithEntityStore.
type EntityStore interface {
	EnsureEntityByCanonicalURI(ctx context.Context, uri, shortName string) (*profile.Entity, bool, error)
}

// Collector fetches registry-side signals for crates.io-hosted packages.
// Scheme-filtered: entities whose CanonicalURI does NOT start with
// pkg:cargo/ receive an empty result with no error.
type Collector struct {
	client      *Client
	entityStore EntityStore
}

// NewCollector returns a Collector bound to the public crates.io endpoint.
func NewCollector() *Collector {
	return &Collector{client: NewClient()}
}

// NewCollectorWithClient returns a Collector using the supplied Client.
// Primary use: tests with httptest servers.
func NewCollectorWithClient(c *Client) *Collector {
	return &Collector{client: c}
}

// WithEntityStore wires an EntityStore so publisher-entity minting fires.
func (c *Collector) WithEntityStore(s EntityStore) *Collector {
	c.entityStore = s
	return c
}

// Name identifies the collector.
func (c *Collector) Name() string { return collectorSource }

// Collect fetches registry metadata for the entity and emits signals.
// Non-cargo entities yield an empty (non-nil) result with no error.
func (c *Collector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	packageName, ok := extractCargoPackageName(entity)
	if !ok {
		return &signal.CollectionResult{}, nil
	}

	result := &signal.CollectionResult{}
	collectedAt := time.Now().UTC()

	cr, err := c.client.GetCrate(ctx, packageName)
	if err != nil {
		retryable := !errors.Is(err, ErrNotFound)
		result.RecordFailure(entity.ID, "last_publish", collectorSource,
			sanitizeFetchReason(err), retryable, collectedAt)
		return result, nil
	}

	// ----- last_publish (vitality) -----
	recordLastPublish(result, entity.ID, cr, collectedAt)

	// ----- recent_downloads (criticality) -----
	recordRecentDownloads(result, entity.ID, cr, collectedAt)

	// ----- build_script_present + yanked_release_count (publication) -----
	recordBuildScriptPresent(result, entity.ID, cr, collectedAt)
	recordYankedReleaseCount(result, entity.ID, cr, collectedAt)

	// ----- longitudinal: build_script_introduced + publish_origin_consistency -----
	recordCrossVersionSignals(result, entity.ID, cr, collectedAt)

	// ----- owners endpoint (governance) -----
	owners, ownersErr := c.client.GetOwners(ctx, packageName)
	if ownersErr != nil {
		retryable := !errors.Is(ownersErr, ErrNotFound)
		reason := sanitizeFetchReason(ownersErr)
		result.RecordFailure(entity.ID, "maintainer_count", collectorSource, reason, retryable, collectedAt)
		result.RecordFailure(entity.ID, "owner_count", collectorSource, reason, retryable, collectedAt)
		result.RecordFailure(entity.ID, "owner_team_present", collectorSource, reason, retryable, collectedAt)
	} else {
		recordMaintainerCount(result, entity.ID, owners, collectedAt)
		recordOwnerCount(result, entity.ID, owners, collectedAt)
		recordOwnerTeamPresent(result, entity.ID, owners, collectedAt)
	}

	// ----- publisher entity minting -----
	c.ensurePublisherEntities(ctx, cr, owners)

	return result, nil
}

// extractCargoPackageName pulls the crate name from a pkg:cargo/* URI.
func extractCargoPackageName(entity *profile.Entity) (string, bool) {
	if entity == nil {
		return "", false
	}
	const prefix = "pkg:cargo/"
	if !strings.HasPrefix(entity.CanonicalURI, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(entity.CanonicalURI, prefix)
	if name == "" {
		return "", false
	}
	return name, true
}

// sanitizeFetchReason strips sensitive details from errors before
// recording them as absence reasons. Never embed response bodies.
func sanitizeFetchReason(err error) string {
	if errors.Is(err, ErrNotFound) {
		return "crate not found on crates.io"
	}
	// Surface the error type but not any body content.
	return fmt.Sprintf("crates.io fetch failed: %v", err)
}

// recordLastPublish emits last_publish from the newest non-yanked version.
func recordLastPublish(result *signal.CollectionResult, entityID string,
	cr *CrateResponse, collectedAt time.Time) {

	recent := recentVersionsByPublishTime(cr, 1)
	if len(recent) == 0 {
		result.RecordAbsence(entityID, "last_publish", collectorSource,
			"no orderable versions in response", false, collectedAt)
		return
	}

	latest := recent[0]
	t, err := time.Parse(time.RFC3339, latest.createdAt)
	if err != nil {
		result.RecordAbsence(entityID, "last_publish", collectorSource,
			"cannot parse latest version timestamp", false, collectedAt)
		return
	}

	result.RecordSignal(entityID, "last_publish", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"latest_version": latest.version,
			"published_at":   t.UTC().Format(time.RFC3339),
			"days_ago":       int(collectedAt.Sub(t).Hours() / 24),
		})
}

// recordRecentDownloads emits the crate's recent download count.
func recordRecentDownloads(result *signal.CollectionResult, entityID string,
	cr *CrateResponse, collectedAt time.Time) {

	result.RecordSignal(entityID, "recent_downloads", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"count":  cr.Crate.RecentDownloads,
			"window": "~90d (crates.io recent_downloads)",
		})
}

// recordBuildScriptPresent emits whether the latest non-yanked version
// declares a build.rs.
func recordBuildScriptPresent(result *signal.CollectionResult, entityID string,
	cr *CrateResponse, collectedAt time.Time) {

	recent := recentVersionsByPublishTime(cr, 1)
	if len(recent) == 0 {
		result.RecordAbsence(entityID, "build_script_present", collectorSource,
			"no orderable versions", false, collectedAt)
		return
	}

	result.RecordSignal(entityID, "build_script_present", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"present":         recent[0].hasBuildScript,
			"version_checked": recent[0].version,
		})
}

// recordYankedReleaseCount counts yanked versions.
func recordYankedReleaseCount(result *signal.CollectionResult, entityID string,
	cr *CrateResponse, collectedAt time.Time) {

	yanked := 0
	for _, v := range cr.Versions {
		if v.Yanked {
			yanked++
		}
	}

	result.RecordSignal(entityID, "yanked_release_count", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"count":          yanked,
			"total_versions": len(cr.Versions),
		})
}

// recordMaintainerCount emits owner count + logins (mirrors npm's signal).
func recordMaintainerCount(result *signal.CollectionResult, entityID string,
	owners *OwnersResponse, collectedAt time.Time) {

	if len(owners.Users) == 0 {
		result.RecordAbsence(entityID, "maintainer_count", collectorSource,
			"owners endpoint returned empty list", false, collectedAt)
		return
	}

	logins := make([]string, 0, len(owners.Users))
	for _, o := range owners.Users {
		if o.Login != "" {
			logins = append(logins, o.Login)
		}
	}

	result.RecordSignal(entityID, "maintainer_count", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"count":  len(owners.Users),
			"logins": logins,
		})
}

// recordOwnerCount emits the bus-factor owner count.
func recordOwnerCount(result *signal.CollectionResult, entityID string,
	owners *OwnersResponse, collectedAt time.Time) {

	result.RecordSignal(entityID, "owner_count", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"count": len(owners.Users),
		})
}

// recordOwnerTeamPresent emits whether at least one team owns the crate.
func recordOwnerTeamPresent(result *signal.CollectionResult, entityID string,
	owners *OwnersResponse, collectedAt time.Time) {

	hasTeam := false
	for _, o := range owners.Users {
		if o.Kind == "team" {
			hasTeam = true
			break
		}
	}

	result.RecordSignal(entityID, "owner_team_present", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"present": hasTeam,
		})
}

// versionRecord is the per-version fact-set the longitudinal signals
// operate over.
type versionRecord struct {
	version        string
	createdAt      string
	hasBuildScript bool
	publisher      string
}

// recentVersionsByPublishTime returns up to n non-yanked versions sorted
// newest-first by publish timestamp.
func recentVersionsByPublishTime(cr *CrateResponse, n int) []versionRecord {
	records := make([]versionRecord, 0, len(cr.Versions))
	for _, v := range cr.Versions {
		if v.Yanked {
			continue
		}
		publisher := ""
		if v.PublishedBy != nil {
			publisher = v.PublishedBy.Login
		}
		records = append(records, versionRecord{
			version:        v.Num,
			createdAt:      v.CreatedAt,
			hasBuildScript: v.HasBuildScript,
			publisher:      publisher,
		})
	}

	// Sort newest-first by CreatedAt. Tiebreaker: lexically-greater
	// version string first (matches npm collector pattern).
	slices.SortStableFunc(records, func(a, b versionRecord) int {
		if a.createdAt == b.createdAt {
			return cmp.Compare(b.version, a.version)
		}
		// Reverse chronological — RFC3339 strings sort lexically.
		return cmp.Compare(b.createdAt, a.createdAt)
	})

	if len(records) > n {
		records = records[:n]
	}
	return records
}

// recordCrossVersionSignals emits build_script_introduced and
// publish_origin_consistency from a window of recent versions.
func recordCrossVersionSignals(result *signal.CollectionResult, entityID string,
	cr *CrateResponse, collectedAt time.Time) {

	recent := recentVersionsByPublishTime(cr, crossVersionWindow)
	if len(recent) == 0 {
		reason := "no orderable non-yanked versions"
		result.RecordAbsence(entityID, "build_script_introduced", collectorSource,
			reason, false, collectedAt)
		result.RecordAbsence(entityID, "publish_origin_consistency", collectorSource,
			reason, false, collectedAt)
		return
	}

	recordBuildScriptIntroduced(result, entityID, recent, collectedAt)
	recordPublishOriginConsistency(result, entityID, recent, collectedAt)
}

// recordBuildScriptIntroduced detects whether build.rs appeared in the
// latest version where older versions lacked it — the cargo analog of
// postinstall_introduced.
func recordBuildScriptIntroduced(result *signal.CollectionResult, entityID string,
	recent []versionRecord, collectedAt time.Time) {

	latestPresent := recent[0].hasBuildScript

	priorWithout := 0
	for i := 1; i < len(recent); i++ {
		if !recent[i].hasBuildScript {
			priorWithout++
		}
	}

	introduced := latestPresent && priorWithout > 0

	introducedAtVersion := ""
	if introduced {
		// Walk oldest→newest to find the first version with build script.
		for i := len(recent) - 1; i >= 0; i-- {
			if recent[i].hasBuildScript {
				introducedAtVersion = recent[i].version
				break
			}
		}
	}

	result.RecordSignal(entityID, "build_script_introduced", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"present_in_latest":      latestPresent,
			"introduced_recently":    introduced,
			"introduced_at_version":  introducedAtVersion,
			"prior_versions_without": priorWithout,
			"versions_checked":       len(recent),
		})
}

// recordPublishOriginConsistency captures the set of distinct publisher
// accounts within the version window.
func recordPublishOriginConsistency(result *signal.CollectionResult, entityID string,
	recent []versionRecord, collectedAt time.Time) {

	publishers := map[string]struct{}{}
	unknownCount := 0
	for _, r := range recent {
		if r.publisher == "" {
			unknownCount++
			continue
		}
		publishers[r.publisher] = struct{}{}
	}

	logins := make([]string, 0, len(publishers))
	for login := range publishers {
		logins = append(logins, login)
	}
	slices.Sort(logins)

	result.RecordSignal(entityID, "publish_origin_consistency", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"distinct_publishers":        len(publishers),
			"publisher_logins":           logins,
			"versions_checked":           len(recent),
			"versions_without_publisher": unknownCount,
		})
}

// ensurePublisherEntities mints identity:cargo/<login> entity rows for
// owners and per-version publishers.
func (c *Collector) ensurePublisherEntities(ctx context.Context, cr *CrateResponse, owners *OwnersResponse) {
	if c.entityStore == nil {
		return
	}

	seen := map[string]struct{}{}
	mint := func(login string) {
		if login == "" {
			return
		}
		uri := profile.CanonicalIdentityURI("cargo", login)
		if _, already := seen[uri]; already {
			return
		}
		seen[uri] = struct{}{}
		if _, _, err := c.entityStore.EnsureEntityByCanonicalURI(ctx, uri, login); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to ensure cargo publisher entity %s: %v\n", uri, err)
		}
	}

	// Owners.
	if owners != nil {
		for _, o := range owners.Users {
			mint(o.Login)
		}
	}

	// Per-version publishers from the crate response.
	if cr != nil {
		for _, v := range cr.Versions {
			if v.PublishedBy != nil {
				mint(v.PublishedBy.Login)
			}
		}
	}
}
