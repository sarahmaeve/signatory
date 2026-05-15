package maven

import (
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
// for longitudinal signals. Matches npm, cargo, and gem collectors.
const crossVersionWindow = 10

// collectorSource is the collector's name, the value that lands in
// profile.Signal.Source for every emission.
const collectorSource = "maven-registry"

// defaultTTL is how long an emitted signal is considered fresh.
// Matches npm, cargo, and gem.
const defaultTTL = 24 * time.Hour

// burstThreshold is the maximum duration between oldest and newest
// version in the window that triggers a burst detection. 72 hours
// matches the gem and npm collectors.
const burstThreshold = 72 * time.Hour

// EntityStore is the narrow interface the maven collector uses to
// mint org:maven/<groupID> entity rows. Optional: nil-safe via
// WithEntityStore.
type EntityStore interface {
	EnsureEntityByCanonicalURI(ctx context.Context, uri, shortName string) (*profile.Entity, bool, error)
}

// Collector fetches registry-side signals for Maven Central-hosted
// packages. Scheme-filtered: entities whose CanonicalURI does NOT
// start with pkg:maven/ receive an empty result with no error.
type Collector struct {
	client      *Client
	entityStore EntityStore
}

// NewCollector returns a Collector bound to the public Maven Central endpoints.
func NewCollector() *Collector {
	return &Collector{client: NewClient()}
}

// NewCollectorWithClient returns a Collector using the supplied Client.
// Primary use: tests with httptest servers.
func NewCollectorWithClient(c *Client) *Collector {
	return &Collector{client: c}
}

// WithEntityStore wires an EntityStore so org-entity minting fires.
func (c *Collector) WithEntityStore(s EntityStore) *Collector {
	c.entityStore = s
	return c
}

// Name identifies the collector.
func (c *Collector) Name() string { return collectorSource }

// Collect fetches registry metadata for the entity and emits signals.
// Non-maven entities yield an empty (non-nil) result with no error.
func (c *Collector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	groupID, artifactID, ok := extractMavenCoordinate(entity)
	if !ok {
		return &signal.CollectionResult{}, nil
	}

	result := &signal.CollectionResult{}
	collectedAt := time.Now().UTC()

	// Fetch maven-metadata.xml from repo1 — version list + latest.
	meta, err := c.client.FetchMetadata(ctx, groupID, artifactID)
	if err != nil {
		retryable := true
		if strings.Contains(err.Error(), "not found") {
			retryable = false
		}
		result.RecordFailure(entity.ID, "last_publish", collectorSource,
			sanitizeFetchReason(err), retryable, collectedAt)
		return result, nil
	}

	if len(meta.Versioning.Versions) == 0 {
		result.RecordFailure(entity.ID, "last_publish", collectorSource,
			"no versions found on Maven Central", false, collectedAt)
		return result, nil
	}

	latestVersion := meta.Versioning.Release
	if latestVersion == "" {
		latestVersion = meta.Versioning.Latest
	}
	if latestVersion == "" {
		// Fallback: last entry in the version list.
		latestVersion = meta.Versioning.Versions[len(meta.Versioning.Versions)-1]
	}

	// ----- version_count (vitality) -----
	recordVersionCount(result, entity.ID, len(meta.Versioning.Versions), collectedAt)

	// Resolve timestamps for the most recent versions via HEAD on
	// each jar. Take the tail of the version list (metadata lists
	// versions oldest-first).
	versions := meta.Versioning.Versions
	windowStart := max(len(versions)-crossVersionWindow, 0)
	recentVersions := versions[windowStart:]

	var stamps []VersionTimestamp
	missingCount := 0
	for _, v := range recentVersions {
		t, headErr := c.client.HeadTimestamp(ctx, groupID, artifactID, v)
		if headErr != nil {
			if errors.Is(headErr, ErrNotFound) {
				missingCount++
			}
			continue
		}
		if t.IsZero() {
			continue
		}
		stamps = append(stamps, VersionTimestamp{
			Version:   v,
			Timestamp: t.UnixMilli(),
		})
	}

	// Sort newest-first by timestamp.
	slices.SortFunc(stamps, func(a, b VersionTimestamp) int {
		if b.Timestamp != a.Timestamp {
			if b.Timestamp > a.Timestamp {
				return 1
			}
			return -1
		}
		return 0
	})

	// ----- last_publish (vitality) -----
	if len(stamps) > 0 {
		recordLastPublish(result, entity.ID, latestVersion, stamps[0], collectedAt)
	} else {
		// No timestamps resolved — emit with metadata's lastUpdated.
		result.RecordSignal(entity.ID, "last_publish", collectorSource, collectedAt, defaultTTL,
			map[string]any{
				"latest_version": latestVersion,
				"published_at":   "unknown",
				"days_ago":       -1,
			})
	}

	// ----- version_publish_burst (publication) -----
	recordVersionPublishBurst(result, entity.ID, stamps, collectedAt)

	// ----- missing_artifact_count (publication) -----
	recordMissingArtifactCount(result, entity.ID, missingCount, len(recentVersions), collectedAt)

	// ----- gpg_signature_present (publication, latest version) -----
	sigPresent, sigErr := c.client.CheckSignature(ctx, groupID, artifactID, latestVersion)
	if sigErr != nil {
		result.RecordFailure(entity.ID, "gpg_signature_present", collectorSource,
			fmt.Sprintf("signature check failed: %v", sigErr), true, collectedAt)
	} else {
		recordGPGSignaturePresent(result, entity.ID, latestVersion, sigPresent, collectedAt)
	}

	// ----- signature_consistency (publication, cross-version) -----
	signedCount, unsignedCount := 0, 0
	for _, v := range recentVersions {
		present, ascErr := c.client.CheckSignature(ctx, groupID, artifactID, v)
		if ascErr != nil {
			continue
		}
		if present {
			signedCount++
		} else {
			unsignedCount++
		}
	}
	recordSignatureConsistency(result, entity.ID, signedCount, unsignedCount,
		len(recentVersions), collectedAt)

	// ----- maintainer_count + author_drift (governance, POM-based) -----
	c.collectPOMSignals(ctx, result, entity.ID, groupID, artifactID,
		latestVersion, recentVersions, collectedAt)

	// ----- maven_dependencies (governance, POM-based) -----
	c.recordMavenDependencies(ctx, result, entity.ID, groupID, artifactID,
		latestVersion, collectedAt)

	// ----- org entity minting -----
	c.ensureOrgEntity(ctx, groupID)

	return result, nil
}

// extractMavenCoordinate pulls groupID and artifactID from a
// pkg:maven/<groupID>/<artifactID> URI.
func extractMavenCoordinate(entity *profile.Entity) (groupID, artifactID string, ok bool) {
	if entity == nil {
		return "", "", false
	}
	const prefix = "pkg:maven/"
	if !strings.HasPrefix(entity.CanonicalURI, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(entity.CanonicalURI, prefix)
	if rest == "" {
		return "", "", false
	}
	group, artifact, found := strings.Cut(rest, "/")
	if !found || group == "" || artifact == "" {
		return "", "", false
	}
	return group, artifact, true
}

// sanitizeFetchReason strips sensitive details from errors.
func sanitizeFetchReason(err error) string {
	if strings.Contains(err.Error(), "not found") {
		return "artifact not found on Maven Central"
	}
	return fmt.Sprintf("Maven Central fetch failed: %v", err)
}

// recordLastPublish emits last_publish from the newest timestamped version.
func recordLastPublish(result *signal.CollectionResult, entityID, latestVersion string,
	newest VersionTimestamp, collectedAt time.Time) {

	t := time.UnixMilli(newest.Timestamp).UTC()

	result.RecordSignal(entityID, "last_publish", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"latest_version": latestVersion,
			"published_at":   t.Format(time.RFC3339),
			"days_ago":       int(collectedAt.Sub(t).Hours() / 24),
		})
}

// recordVersionCount emits the total version count.
func recordVersionCount(result *signal.CollectionResult, entityID string,
	count int, collectedAt time.Time) {

	result.RecordSignal(entityID, "version_count", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"count": count,
		})
}

// recordVersionPublishBurst detects whether 3+ versions were published
// within 72 hours. Matches the gem and npm collectors' pattern.
func recordVersionPublishBurst(result *signal.CollectionResult, entityID string,
	stamps []VersionTimestamp, collectedAt time.Time) {

	if len(stamps) < 2 {
		result.RecordSignal(entityID, "version_publish_burst", collectorSource, collectedAt, defaultTTL,
			map[string]any{
				"burst_detected":     false,
				"versions_in_window": len(stamps),
				"window_hours":       0,
				"versions_checked":   len(stamps),
			})
		return
	}

	newest := time.UnixMilli(stamps[0].Timestamp)
	oldest := time.UnixMilli(stamps[len(stamps)-1].Timestamp)
	span := newest.Sub(oldest)

	burst := len(stamps) >= 3 && span <= burstThreshold

	result.RecordSignal(entityID, "version_publish_burst", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"burst_detected":     burst,
			"versions_in_window": len(stamps),
			"window_hours":       int(span.Hours()),
			"versions_checked":   len(stamps),
		})
}

// recordGPGSignaturePresent emits whether a .asc signature exists for
// the latest version on repo1.maven.org.
func recordGPGSignaturePresent(result *signal.CollectionResult, entityID string,
	version string, present bool, collectedAt time.Time) {

	result.RecordSignal(entityID, "gpg_signature_present", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"present":         present,
			"version_checked": version,
		})
}

// recordMissingArtifactCount emits the number of versions listed in
// maven-metadata.xml whose jars returned 404 — Maven's analog of yanked
// releases. Zero additional HTTP calls: derived from the existing HEAD loop.
func recordMissingArtifactCount(result *signal.CollectionResult, entityID string,
	missing, versionsChecked int, collectedAt time.Time) {

	result.RecordSignal(entityID, "missing_artifact_count", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"count":            missing,
			"versions_checked": versionsChecked,
		})
}

// recordSignatureConsistency emits whether .asc signatures are present
// across the version window, not just the latest version. A transition
// from signed→unsigned (or vice versa) is a governance change worth noting.
func recordSignatureConsistency(result *signal.CollectionResult, entityID string,
	signed, unsigned, versionsChecked int, collectedAt time.Time) {

	result.RecordSignal(entityID, "signature_consistency", collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"all_signed":       unsigned == 0 && signed > 0,
			"signed_count":     signed,
			"unsigned_count":   unsigned,
			"versions_checked": versionsChecked,
		})
}

// collectPOMSignals fetches POMs to derive maintainer_count (latest
// version only) and author_drift (across the version window).
func (c *Collector) collectPOMSignals(ctx context.Context, result *signal.CollectionResult,
	entityID, groupID, artifactID, latestVersion string,
	recentVersions []string, collectedAt time.Time) {

	// ----- maintainer_count (governance) -----
	latestDevs, err := c.client.FetchDevelopers(ctx, groupID, artifactID, latestVersion)
	if err != nil {
		// POM fetch failed — record absence, don't block.
		result.RecordAbsence(entityID, "maintainer_count", collectorSource,
			fmt.Sprintf("POM fetch failed: %v", err), true, collectedAt)
	} else if len(latestDevs) == 0 {
		result.RecordAbsence(entityID, "maintainer_count", collectorSource,
			"no <developers> section in POM", false, collectedAt)
	} else {
		result.RecordSignal(entityID, "maintainer_count", collectorSource, collectedAt, defaultTTL,
			map[string]any{
				"count": len(latestDevs),
				"names": latestDevs,
			})
	}

	// ----- author_drift (governance, longitudinal) -----
	developerSets := map[string]struct{}{}
	for _, v := range recentVersions {
		devs, devErr := c.client.FetchDevelopers(ctx, groupID, artifactID, v)
		if devErr != nil || len(devs) == 0 {
			continue
		}
		// Canonical key: sorted, joined developer names.
		sorted := make([]string, len(devs))
		copy(sorted, devs)
		slices.Sort(sorted)
		key := strings.Join(sorted, ", ")
		developerSets[key] = struct{}{}
	}

	if len(developerSets) == 0 {
		result.RecordAbsence(entityID, "author_drift", collectorSource,
			"no <developers> found in any version POM", false, collectedAt)
	} else {
		setList := make([]string, 0, len(developerSets))
		for k := range developerSets {
			setList = append(setList, k)
		}
		slices.Sort(setList)

		result.RecordSignal(entityID, "author_drift", collectorSource, collectedAt, defaultTTL,
			map[string]any{
				"distinct_developer_sets": len(developerSets),
				"developer_sets":          setList,
				"versions_checked":        len(recentVersions),
			})
	}
}

// recordMavenDependencies emits maven_dependencies: the declared
// direct-dependency surface of the latest release POM, as
// groupId:artifactId coordinates. The value shape is byte-identical
// to go_dependencies / npm_dependencies / cargo_dependencies
// (direct_count, indirect_count, total_count, direct[]) so the deltas
// diff engine surfaces Maven dependency drift through the same
// set-diff path with no deltas-side changes.
//
// One POM GET for the latest version (the same POM the developers
// fetch reads; kept as a separate call to mirror FetchDevelopers
// rather than refactor the existing maintainer_count/author_drift
// path). A fetch failure records a retryable failure for this signal
// alone — non-retryable only for ErrNotFound — and never blocks the
// rest of the collection, mirroring the cargo dependencies endpoint.
//
// Zero declared dependencies emits a zero-surface signal rather than
// an absence: "this artifact declares no dependencies" is a
// load-bearing fact, consistent with the go/npm/cargo signals.
func (c *Collector) recordMavenDependencies(ctx context.Context,
	result *signal.CollectionResult, entityID, groupID, artifactID,
	latestVersion string, collectedAt time.Time) {

	const sigType = "maven_dependencies"

	deps, err := c.client.FetchDependencies(ctx, groupID, artifactID, latestVersion)
	if err != nil {
		retryable := !errors.Is(err, ErrNotFound)
		result.RecordFailure(entityID, sigType, collectorSource,
			sanitizeFetchReason(err), retryable, collectedAt)
		return
	}

	// nil-when-empty, mirroring parseGoModDeps' []string semantics so
	// the empty case marshals identically across ecosystems.
	var direct []string
	direct = append(direct, deps...)
	slices.Sort(direct)
	direct = slices.Compact(direct)

	result.RecordSignal(entityID, sigType, collectorSource, collectedAt, defaultTTL,
		map[string]any{
			"direct_count":   len(direct),
			"indirect_count": 0,
			"total_count":    len(direct),
			"direct":         direct,
		})
}

// ensureOrgEntity mints an org:maven/<groupID> entity row.
func (c *Collector) ensureOrgEntity(ctx context.Context, groupID string) {
	if c.entityStore == nil {
		return
	}
	uri := "org:maven/" + strings.ToLower(groupID)
	if _, _, err := c.entityStore.EnsureEntityByCanonicalURI(ctx, uri, groupID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to ensure maven org entity %s: %v\n", uri, err)
	}
}
