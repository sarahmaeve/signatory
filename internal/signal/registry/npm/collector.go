package npm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// source is the collector's name, the value that lands in
// profile.Signal.Source for every emission. Kept as a const so the
// string appears in exactly one place.
const source = "npm-registry"

// defaultTTL is how long an emitted signal is considered fresh
// before refresh logic surfaces it for re-collection. Matches the
// github collector's default; the value is not load-bearing for
// Phase A because the TTL-expiry machinery is ROADMAP should-do,
// but emitting a sensible value keeps the column populated.
const defaultTTL = 24 * time.Hour

// Collector fetches registry-side signals for npm-hosted packages.
// Scheme-filtered: entities whose CanonicalURI does NOT start with
// pkg:npm/ receive an empty result with no error, so the orchestrator
// can include the collector unconditionally in its dispatch list.
type Collector struct {
	client *Client
}

// NewCollector returns a Collector bound to the public npm registry.
func NewCollector() *Collector {
	return &Collector{client: NewClient()}
}

// NewCollectorWithClient returns a Collector using the supplied
// Client. Primary use case: cross-package functional tests that
// point the client at an httptest server via NewClientWithBaseURL.
// Production code uses NewCollector.
func NewCollectorWithClient(c *Client) *Collector {
	return &Collector{client: c}
}

// newCollectorWithClient is the same; kept as an internal alias
// for this package's own tests.
func newCollectorWithClient(c *Client) *Collector {
	return NewCollectorWithClient(c)
}

// Name identifies the collector.
func (c *Collector) Name() string { return source }

// Collect fetches registry metadata for the entity and emits the
// Phase-A signal set. Non-npm entities yield an empty (nil, nil)
// result so the orchestrator doesn't need a pre-filter.
//
// The contract per signal.Collector is: only return a non-nil error
// when collection cannot proceed at all (e.g., the entity's URI is
// unparseable). Per-signal failures are recorded as absences in the
// CollectionResult, not returned as errors.
func (c *Collector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	packageName, ok := extractNpmPackageName(entity)
	if !ok {
		// Not an npm entity; nothing to do. Return an empty result
		// (not nil) so callers can safely call Signals()/AbsenceCount()
		// without a nil-guard.
		return &signal.CollectionResult{}, nil
	}

	result := &signal.CollectionResult{}
	collectedAt := time.Now().UTC()

	pkg, err := c.client.GetPackage(ctx, packageName)
	if err != nil {
		// Treat fetch failure (404, network, size-cap, ...) as an
		// absence so the entity's profile reflects "we tried and
		// the registry said no / couldn't reach." Retryable is
		// true except for ErrNotFound, which is definitive.
		retryable := !errors.Is(err, ErrNotFound)
		result.RecordFailure(entity.ID, "last_publish", source,
			sanitizeFetchReason(err), retryable, collectedAt)
		return result, nil
	}

	// ----- last_publish (vitality) -----
	//
	// The latest publish timestamp comes from the `time` map keyed
	// by the dist-tags.latest version. We pick that specific key
	// rather than the `modified` entry because `modified` includes
	// maintainer edits (README update, deprecation flip) and we
	// want the moment-of-last-published-artifact, not the moment-
	// of-last-metadata-edit.
	if err := recordLastPublish(result, entity.ID, pkg, collectedAt); err != nil {
		result.RecordAbsence(entity.ID, "last_publish", source,
			err.Error(), false, collectedAt)
	}

	// ----- maintainer_count (governance) -----
	recordMaintainerCount(result, entity.ID, pkg, collectedAt)

	// ----- postinstall_present + trusted_publishing (publication) -----
	//
	// Both signals read from versions[dist-tags.latest]. Group the
	// lookup so a missing latest-version entry produces one absence
	// per signal rather than re-doing the map fetch twice.
	recordLatestVersionSignals(result, entity.ID, pkg, collectedAt)

	// ----- weekly_downloads (criticality) -----
	//
	// Separate endpoint (api.npmjs.org/downloads) — one extra HTTP
	// call per analyze. Failure is recorded as absence so the other
	// signals still land.
	recordWeeklyDownloads(ctx, c.client, result, entity.ID, packageName, collectedAt)

	return result, nil
}

// extractNpmPackageName pulls the npm package name out of an
// entity's canonical URI. Returns (name, true) for pkg:npm/* URIs
// and (_, false) for anything else. The name preserves scoping
// (`@types/node` stays scoped, `express` stays bare).
func extractNpmPackageName(entity *profile.Entity) (string, bool) {
	if entity == nil {
		return "", false
	}
	const prefix = "pkg:npm/"
	if !strings.HasPrefix(entity.CanonicalURI, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(entity.CanonicalURI, prefix)
	if name == "" {
		return "", false
	}
	return name, true
}

// recordLastPublish extracts the latest-version publish timestamp
// from a RegistryPackage and records it as a signal. Returns a
// non-nil error when the shape of the response doesn't give us a
// usable timestamp — caller converts that into an absence record.
func recordLastPublish(result *signal.CollectionResult, entityID string,
	pkg *RegistryPackage, collectedAt time.Time) error {

	if pkg.DistTags.Latest == "" {
		return fmt.Errorf("registry response has no dist-tags.latest")
	}
	t, ok := pkg.Time[pkg.DistTags.Latest]
	if !ok || t.IsZero() {
		return fmt.Errorf("registry response has no time entry for latest version %q", pkg.DistTags.Latest)
	}

	result.RecordSignal(entityID, "last_publish", source, collectedAt, defaultTTL,
		map[string]any{
			"latest_version": pkg.DistTags.Latest,
			"published_at":   t.UTC().Format(time.RFC3339),
			"days_ago":       int(collectedAt.Sub(t).Hours() / 24),
		})
	return nil
}

// recordMaintainerCount emits the maintainer count and the list of
// maintainer logins (not emails — logins are the stable, public
// identifiers npm displays and revokes against).
func recordMaintainerCount(result *signal.CollectionResult, entityID string,
	pkg *RegistryPackage, collectedAt time.Time) {

	if len(pkg.Maintainers) == 0 {
		result.RecordAbsence(entityID, "maintainer_count", source,
			"registry response has no maintainers field", false, collectedAt)
		return
	}

	logins := make([]string, 0, len(pkg.Maintainers))
	for _, m := range pkg.Maintainers {
		if m.Name != "" {
			logins = append(logins, m.Name)
		}
	}

	result.RecordSignal(entityID, "maintainer_count", source, collectedAt, defaultTTL,
		map[string]any{
			"count":  len(pkg.Maintainers),
			"logins": logins,
		})
}

// recordLatestVersionSignals emits postinstall_present and
// trusted_publishing by inspecting the latest version's per-version
// record. A missing latest-version entry converts into one absence
// per signal — both are "we couldn't look this up" for the same
// reason, so the absence messages are identical.
func recordLatestVersionSignals(result *signal.CollectionResult, entityID string,
	pkg *RegistryPackage, collectedAt time.Time) {

	latest := pkg.DistTags.Latest
	if latest == "" {
		result.RecordAbsence(entityID, "postinstall_present", source,
			"registry response has no dist-tags.latest", false, collectedAt)
		result.RecordAbsence(entityID, "trusted_publishing", source,
			"registry response has no dist-tags.latest", false, collectedAt)
		return
	}
	ver, ok := pkg.Versions[latest]
	if !ok {
		// The registry names a latest version but didn't include a
		// versions[latest] entry. Rare in practice but defensible:
		// record as retryable absence so a re-run can catch a lagging
		// response shape.
		msg := fmt.Sprintf("registry response has no versions[%q] entry", latest)
		result.RecordFailure(entityID, "postinstall_present", source, msg, true, collectedAt)
		result.RecordFailure(entityID, "trusted_publishing", source, msg, true, collectedAt)
		return
	}

	// postinstall_present: true iff the scripts.postinstall field
	// is non-empty on the latest published version. Presence is
	// the signal; the script TEXT is not emitted — it's often
	// multi-line shell/JS and pollutes signal payloads. Analysts
	// who want the content inspect the tarball directly.
	postinstall := ver.Scripts.Postinstall != ""
	result.RecordSignal(entityID, "postinstall_present", source, collectedAt, defaultTTL,
		map[string]any{
			"present":         postinstall,
			"version_checked": latest,
		})

	// trusted_publishing: presence of a non-null attestations block
	// on the latest version. RawMessage semantics:
	//   - field missing       → len == 0            → absent
	//   - field explicit null → string(raw) == "null" → absent
	//   - field object        → len > 0 && != "null"  → present
	att := ver.Dist.Attestations
	hasAttestation := len(att) > 0 && string(att) != "null"
	result.RecordSignal(entityID, "trusted_publishing", source, collectedAt, defaultTTL,
		map[string]any{
			"present":         hasAttestation,
			"version_checked": latest,
		})
}

// recordWeeklyDownloads makes a second registry call to get the
// download stats and emits the signal. Failure (network, 404,
// size-cap) is recorded as absence — the rest of the signal set
// still gets through.
func recordWeeklyDownloads(ctx context.Context, client *Client,
	result *signal.CollectionResult, entityID, packageName string,
	collectedAt time.Time) {

	count, err := client.GetWeeklyDownloads(ctx, packageName)
	if err != nil {
		retryable := !errors.Is(err, ErrNotFound)
		result.RecordFailure(entityID, "weekly_downloads", source,
			sanitizeFetchReason(err), retryable, collectedAt)
		return
	}
	result.RecordSignal(entityID, "weekly_downloads", source, collectedAt, defaultTTL,
		map[string]any{
			"count":  count,
			"window": "last-week",
		})
}

// sanitizeFetchReason converts a fetch error into a reason string
// safe to persist in the signal row. Error strings returned by the
// client do not contain response bodies (by design — see client.go
// and #93), so this is mostly a policy-layer formatter that trims
// wrapping noise and keeps the reason short.
func sanitizeFetchReason(err error) string {
	if errors.Is(err, ErrNotFound) {
		return "package not found in npm registry"
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
