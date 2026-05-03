package gem

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

const (
	collectorSource = "gem-registry"
	defaultTTL      = 24 * time.Hour
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

// recordMFARequired emits whether the gem mandates MFA for pushes.
func recordMFARequired(result *signal.CollectionResult, entityID string,
	gem *GemResponse, collectedAt time.Time) {

	result.RecordSignal(entityID, "mfa_required", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"required": gem.MFARequired,
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
