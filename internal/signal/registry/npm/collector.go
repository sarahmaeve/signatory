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

	// ----- Phase A signal: last_publish -----
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
