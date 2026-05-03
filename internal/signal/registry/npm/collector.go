package npm

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// crossVersionWindow bounds the number of recent versions consulted
// for longitudinal signals. Ten is enough to establish a pattern
// without paying for parsing hundreds of historical entries on
// popular packages. Widening the window has diminishing returns —
// older transitions are less actionable than recent ones — and
// shrinking it risks missing signal on packages that publish many
// pre-release versions in quick succession.
const crossVersionWindow = 10

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

// EntityStore is the narrow interface the npm collector uses to
// mint identity:npm/<login> entity rows for the maintainers and
// publishers it observes. Defined here (consumer-side) so the
// collector doesn't depend on the full internal/store package —
// any type that implements EnsureEntityByCanonicalURI satisfies
// it via structural typing.
//
// Optional: nil-safe via the WithEntityStore setter, mirroring the
// github collector's pattern (Path A; design/entity-burn1.md). Tests
// that don't care about publisher-entity emission construct
// collectors without calling WithEntityStore, and the minting
// branch in Collect silently skips when c.entityStore is nil.
//
// In production, cmd/signatory/collectors.go threads the
// orchestrator's *store.SQLite through opts.EntityStore so every
// analyze run populates publisher entities for npm-ecosystem
// targets (Path C).
type EntityStore interface {
	EnsureEntityByCanonicalURI(ctx context.Context, uri, shortName string) (*profile.Entity, bool, error)
}

// Collector fetches registry-side signals for npm-hosted packages.
// Scheme-filtered: entities whose CanonicalURI does NOT start with
// pkg:npm/ receive an empty result with no error, so the orchestrator
// can include the collector unconditionally in its dispatch list.
type Collector struct {
	client      *Client
	entityStore EntityStore // optional — see EntityStore docstring
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

// WithEntityStore wires an EntityStore into the collector so
// publisher-entity minting fires during each Collect run. Returns
// the receiver so the call chains cleanly with the constructors —
// matches the github collector's setter pattern (Path A).
//
// Setter rather than constructor parameter so existing call sites
// (NewCollector / NewCollectorWithClient) keep their signatures and
// pre-Path-C tests continue to compile unchanged.
func (c *Collector) WithEntityStore(s EntityStore) *Collector {
	c.entityStore = s
	return c
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

	// ----- postinstall_introduced + publish_origin_consistency -----
	//
	// Longitudinal signals. Same wire payload as the snapshot
	// signals above — the full versions map is already in pkg —
	// so the marginal cost is parsing, not network. This is where
	// the axios-style "compromised publish breaks established
	// patterns" shape gets caught.
	recordCrossVersionSignals(result, entity.ID, pkg, collectedAt)

	// ----- publisher entity minting (Path C) -----
	//
	// Mint identity:npm/<login> entity rows for every maintainer
	// (top-level Maintainers list) and every per-version publisher
	// (_npmUser.name across the recent-versions window). Both come
	// from the same registry payload we already parsed for the
	// signal emissions above; this is parsing, not network.
	//
	// Idempotent on overlap: a login appearing in both the
	// Maintainers list and as a version publisher gets minted
	// once — EnsureEntityByCanonicalURI's "find OR mint" contract
	// makes the second call a no-op on the persistence layer.
	//
	// Skipped silently when no EntityStore was wired (pre-Path-C
	// tests construct collectors without one and continue to work).
	c.ensurePublisherEntities(ctx, pkg)

	return result, nil
}

// ensurePublisherEntities walks pkg.Maintainers and the per-version
// _npmUser.name set, building the union of distinct npm logins, and
// calls EnsureEntityByCanonicalURI for each. Tracks a local seen-set
// to avoid redundant store roundtrips when a login appears in both
// branches (the lodash shape: jdalton is a current maintainer AND
// was the publisher of the latest version).
//
// Failures are logged-and-continued: a transient store error on one
// login doesn't abort the whole sweep, because each entity row is
// independent and the next analyze run re-attempts. The per-error
// stderr line surfaces systemic issues so an operator notices.
func (c *Collector) ensurePublisherEntities(ctx context.Context, pkg *RegistryPackage) {
	if c.entityStore == nil || pkg == nil {
		return
	}

	seen := map[string]struct{}{}
	mint := func(login string) {
		if login == "" {
			return
		}
		uri := profile.CanonicalIdentityURI("npm", login)
		if _, already := seen[uri]; already {
			return
		}
		seen[uri] = struct{}{}
		if _, _, err := c.entityStore.EnsureEntityByCanonicalURI(ctx, uri, login); err != nil {
			// Don't propagate — the signal emissions are independent
			// of entity-row minting, and the next analyze run re-
			// attempts. Surface to stderr so systemic store failures
			// are visible. Matches the github collector's policy.
			fmt.Fprintf(os.Stderr, "warning: failed to ensure npm publisher entity %s: %v\n", uri, err)
		}
	}

	for _, m := range pkg.Maintainers {
		mint(m.Name)
	}
	for _, ver := range pkg.Versions {
		mint(ver.NpmUser.Name)
	}
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

// versionRecord is the per-version fact-set the longitudinal
// signals operate over. Built once from pkg.Versions + pkg.Time
// and iterated twice (once per emitted signal) rather than
// re-walking the map.
type versionRecord struct {
	version        string
	publishedAt    time.Time
	postinstall    bool
	hasAttestation bool
	publisher      string
}

// recentVersionsByPublishTime returns up to n versions from pkg's
// versions map, sorted newest-first by publish timestamp. Versions
// without a corresponding entry in pkg.Time are skipped — ordering
// them would require a fallback (semver? lexical?) whose mistakes
// would silently corrupt the longitudinal signals. A version we
// can't order is a version we don't emit signals about.
func recentVersionsByPublishTime(pkg *RegistryPackage, n int) []versionRecord {
	records := make([]versionRecord, 0, len(pkg.Versions))
	for ver, pv := range pkg.Versions {
		t, ok := pkg.Time[ver]
		if !ok || t.IsZero() {
			continue
		}
		records = append(records, versionRecord{
			version:        ver,
			publishedAt:    t,
			postinstall:    pv.Scripts.Postinstall != "",
			hasAttestation: len(pv.Dist.Attestations) > 0 && string(pv.Dist.Attestations) != "null",
			publisher:      pv.NpmUser.Name,
		})
	}
	// Stable sort + explicit tiebreaker: two versions with
	// identical registry timestamps would otherwise produce
	// nondeterministic ordering of recent[0], which drives
	// latest_publisher, latest_has_attestation, and
	// introduced_at_version. Timestamp collisions at second
	// granularity are plausible (the npm registry records time to
	// millisecond but storage round-trips through RFC3339 truncate
	// to second precision for many fixtures), so the tiebreaker
	// makes signal output reproducible across runs.
	//
	// Lexically-greater version string sorts first within a time
	// tie. For the common case (semver-shaped, matching-length
	// components), this aligns lexical and semver order; for
	// pathological cases (10.0.0 vs 2.0.0) lexical picks wrong but
	// still picks deterministically — any consistent choice
	// satisfies the tiebreaker's job.
	slices.SortStableFunc(records, func(a, b versionRecord) int {
		if a.publishedAt.Equal(b.publishedAt) {
			return cmp.Compare(b.version, a.version)
		}
		return b.publishedAt.Compare(a.publishedAt)
	})
	if len(records) > n {
		records = records[:n]
	}
	return records
}

// burstThreshold is the maximum duration between oldest and newest
// version in the window that triggers a burst detection. 72 hours
// matches the BufferZoneCorp campaign cadence (4 versions in 3 days)
// and the gem collector's threshold.
const burstThreshold = 72 * time.Hour

// recordCrossVersionSignals emits the longitudinal signals
// (postinstall_introduced, publish_origin_consistency,
// version_publish_burst) from a shared window of recent versions.
// Returning an empty window is recorded as absence for all — a
// package with no orderable versions produces no cross-version
// evidence either way.
func recordCrossVersionSignals(result *signal.CollectionResult, entityID string,
	pkg *RegistryPackage, collectedAt time.Time) {

	recent := recentVersionsByPublishTime(pkg, crossVersionWindow)
	if len(recent) == 0 {
		reason := "no orderable versions in registry response (missing time entries)"
		result.RecordAbsence(entityID, "postinstall_introduced", source,
			reason, false, collectedAt)
		result.RecordAbsence(entityID, "publish_origin_consistency", source,
			reason, false, collectedAt)
		result.RecordAbsence(entityID, "version_publish_burst", source,
			reason, false, collectedAt)
		return
	}

	recordPostinstallIntroduced(result, entityID, recent, collectedAt)
	recordPublishOriginConsistency(result, entityID, recent, collectedAt)
	recordVersionPublishBurst(result, entityID, recent, collectedAt)
}

// recordPostinstallIntroduced detects the axios-2026 shape: a
// postinstall script present in the latest version where one or
// more recent older versions published without one.
//
// We report the transition, not absolute state — postinstall_present
// already covers the snapshot. A "consistent absence" (no postinstall
// in the window) is the healthy case for packages like zod and
// axios-pre-compromise. A "consistent presence" is typical for
// native-binding packages and not anomalous.
func recordPostinstallIntroduced(result *signal.CollectionResult, entityID string,
	recent []versionRecord, collectedAt time.Time) {

	latestPresent := recent[0].postinstall

	// Count how many older versions in the window lacked a
	// postinstall. A non-zero count paired with latestPresent=true
	// is the transition signal.
	priorWithout := 0
	for i := 1; i < len(recent); i++ {
		if !recent[i].postinstall {
			priorWithout++
		}
	}

	introduced := latestPresent && priorWithout > 0

	// When introduced, find the oldest version in the window that
	// still has it — that's where the transition happened (the
	// first version, walking oldest-to-newest, where postinstall
	// flipped to true).
	introducedAtVersion := ""
	if introduced {
		for i := len(recent) - 1; i >= 0; i-- {
			if recent[i].postinstall {
				introducedAtVersion = recent[i].version
				break
			}
		}
	}

	result.RecordSignal(entityID, "postinstall_introduced", source, collectedAt, defaultTTL,
		map[string]any{
			"present_in_latest":      latestPresent,
			"introduced_recently":    introduced,
			"introduced_at_version":  introducedAtVersion,
			"prior_versions_without": priorWithout,
			"versions_checked":       len(recent),
		})
}

// recordPublishOriginConsistency captures two dimensions of
// publish-provenance continuity: transitions in OIDC-attestation
// presence, and the set of distinct _npmUser accounts that
// published within the window.
//
// Stable patterns (all-attested + single-publisher, or
// none-attested + single-publisher) are the healthy shapes.
// Transitions are flags, not verdicts: a maintainer handoff or a
// CI-pipeline migration will produce a transition that an honest
// investigation would clear.
func recordPublishOriginConsistency(result *signal.CollectionResult, entityID string,
	recent []versionRecord, collectedAt time.Time) {

	// Count transitions in attestation-presence across adjacent
	// versions (sorted newest-first). A "transition" is any flip
	// — direction-agnostic. A lost attestation is the axios shape;
	// a gained one is a maintainer adopting trusted publishing.
	// Both deserve a look.
	attestationTransitions := 0
	for i := 1; i < len(recent); i++ {
		if recent[i-1].hasAttestation != recent[i].hasAttestation {
			attestationTransitions++
		}
	}

	publishers := map[string]struct{}{}
	for _, r := range recent {
		if r.publisher != "" {
			publishers[r.publisher] = struct{}{}
		}
	}
	publisherList := slices.Sorted(maps.Keys(publishers))

	result.RecordSignal(entityID, "publish_origin_consistency", source, collectedAt, defaultTTL,
		map[string]any{
			"versions_checked":        len(recent),
			"latest_has_attestation":  recent[0].hasAttestation,
			"attestation_transitions": attestationTransitions,
			"unique_publishers":       len(publishers),
			"publishers":              publisherList,
			"latest_publisher":        recent[0].publisher,
		})
}

// recordVersionPublishBurst detects whether multiple versions were
// published within a short time window (burstThreshold). A rapid-fire
// publish cadence is characteristic of version-pumping campaigns:
// ship benign versions quickly to build download/version history,
// then weaponize the latest.
func recordVersionPublishBurst(result *signal.CollectionResult, entityID string,
	recent []versionRecord, collectedAt time.Time) {

	if len(recent) < 2 {
		result.RecordSignal(entityID, "version_publish_burst", source, collectedAt, defaultTTL,
			map[string]any{
				"burst_detected":     false,
				"versions_in_window": len(recent),
				"window_hours":       0,
				"versions_checked":   len(recent),
			})
		return
	}

	// recent is sorted newest-first: recent[0] is newest,
	// recent[len-1] is oldest.
	newest := recent[0].publishedAt
	oldest := recent[len(recent)-1].publishedAt
	span := newest.Sub(oldest)

	burst := len(recent) >= 3 && span <= burstThreshold

	result.RecordSignal(entityID, "version_publish_burst", source, collectedAt, defaultTTL,
		map[string]any{
			"burst_detected":     burst,
			"versions_in_window": len(recent),
			"window_hours":       int(span.Hours()),
			"versions_checked":   len(recent),
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
