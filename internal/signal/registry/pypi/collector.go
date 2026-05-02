package pypi

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

	info, err := c.client.GetProjectInfo(ctx, packageName)
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
	//
	// extractPyPILogins runs the conservative login-shape filter
	// (login.go); the resulting list is what gets minted (when an
	// EntityStore is wired) AND what populates the maintainer_count
	// signal value. Both sides see the same set, so the cascade
	// resolver's signal-derived candidates align with the entities
	// that actually exist in the store.
	logins := extractPyPILogins(info)
	if len(logins) == 0 {
		// The publisher-supplied metadata had no extractable login
		// (only display names, only emails, or empty entirely).
		// Record absence — the cascade resolver gets no candidates,
		// the maintainer_count column stays empty, and a future
		// collector enhancement (HTML scrape, PyPI API expansion)
		// can fill the gap.
		reason := "no login-shaped value in info.maintainer / info.author / info.maintainers"
		result.RecordAbsence(entity.ID, "maintainer_count", source,
			reason, false, collectedAt)
		return result, nil
	}

	c.ensurePublisherEntities(ctx, logins)

	result.RecordSignal(entity.ID, "maintainer_count", source, collectedAt, defaultTTL,
		map[string]any{
			"count":  len(logins),
			"logins": logins,
		})

	return result, nil
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
