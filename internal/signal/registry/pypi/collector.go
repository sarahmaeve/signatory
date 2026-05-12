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

	// ----- Build version-file list (shared by Phase A and Phase B) -----
	versions := buildVersionFiles(proj)

	// ----- trusted_publishing: Phase A (snapshot) -----
	//
	// Reordered to run BEFORE artifact_url so the recovered
	// AttestationResponse can feed git_head. The attestation's
	// Fulcio cert carries the publisher-stamped commit SHA in OID
	// 1.3.6.1.4.1.57264.1.13 — extracting it lifts pair confidence
	// downstream from tag_match to exact_gitHead.
	latestAttest, latestKnown := c.recordTrustedPublishing(ctx, result, entity.ID, packageName, versions, collectedAt)

	// ----- artifact_url (publication, handoff to artifact collector) -----
	recordArtifactURL(result, entity.ID, proj, latestAttest, collectedAt)

	// ----- attestation_consistency: Phase B (longitudinal) -----
	// Only fires when Phase A produced a definitive answer (attested or
	// 404). When Phase A errored we have no reliable latest state — Phase B
	// would misinterpret nil as "unattested" and could false-alarm.
	if latestKnown {
		c.recordAttestationConsistency(ctx, result, entity.ID, packageName, versions, latestAttest, collectedAt)
	}

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
	hasSig      bool // true when ANY dist for this version has has_sig=true
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

// hasGPGSig returns true when any distribution in the version was
// uploaded with a GPG signature (the legacy has_sig field, deprecated
// May 2023 in favor of PEP 740 Sigstore attestations).
func hasGPGSig(dists []Distribution) bool {
	for _, d := range dists {
		if d.HasSig {
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
			hasSig:      hasGPGSig(dists),
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

	// ----- gpg_signature_present (legacy has_sig) -----
	result.RecordSignal(entityID, "gpg_signature_present", source, collectedAt, defaultTTL,
		map[string]any{
			"present":         newest.hasSig,
			"version_checked": newest.version,
		})

	// ----- version_publish_burst + sdist_only_introduced -----
	window := records[:min(len(records), crossVersionWindow)]

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

// recordArtifactURL emits the artifact_url handoff signal that the
// artifact-vs-repo divergence collector consumes from the in-run
// accumulator.
//
// PyPI differs from npm and cargo in two ways the downstream
// collector relies on:
//
//  1. The sdist URL lives on Distribution.url directly (parsed
//     from the registry response) rather than constructed (cargo)
//     or carried on a different field shape (npm's dist.tarball).
//
//  2. PyPI exposes no publisher-stamped commit SHA in registry
//     metadata directly, but PEP 740 trusted-publishing
//     attestations DO carry one in the Fulcio cert's source-repo-
//     digest OID extension. When attest is non-nil and the cert
//     parses cleanly, git_head ships the recovered SHA — pair
//     confidence downstream becomes exact_gitHead. When attest
//     is nil (no trusted publishing, or fetch failure) git_head
//     ships empty and pair resolution falls through to tag-match.
//
// Sdist (packagetype=="sdist"), not wheel, is the right surface:
// wheels are build outputs (compiled .pyc, sometimes C extensions,
// regenerated metadata), so wheel-vs-repo is a category error for
// the xz-shaped check. The signal is "what does the publication
// channel ship that the source-of-record git tree doesn't?" — for
// Python that's exclusively the sdist.
//
// integrity carries the sdist's digests.sha256. Opaque to current
// consumers — see urlSignalValue's declared caveat in
// internal/signal/types.go — but kept on the wire for the future
// cross-check against the hash signatory computes during fetch.
func recordArtifactURL(result *signal.CollectionResult, entityID string,
	proj *Project, attest *AttestationResponse, collectedAt time.Time) {

	const sigType = "artifact_url"

	// Walk releases newest-first; pick the first version that has a
	// non-yanked sdist distribution. Versions whose timestamps don't
	// parse are skipped — the same policy buildVersionFiles uses.
	var candidates []versionRecord
	for ver, dists := range proj.Releases {
		if len(dists) == 0 {
			continue
		}
		t, err := time.Parse(time.RFC3339, dists[0].UploadTimeISO)
		if err != nil {
			continue
		}
		candidates = append(candidates, versionRecord{
			version:     ver,
			publishedAt: t,
			yanked:      isYanked(dists),
		})
	}
	slices.SortFunc(candidates, func(a, b versionRecord) int {
		if a.publishedAt.Equal(b.publishedAt) {
			return cmp.Compare(b.version, a.version)
		}
		return b.publishedAt.Compare(a.publishedAt)
	})

	// Recover the publisher-stamped commit SHA from the trusted-
	// publishing attestation when available. All artifacts in a
	// release share the same source-repo-digest because they're
	// all built from the same workflow run, so the SHA recovered
	// from any attestation in the response is the right SHA for
	// the sdist regardless of which file the attestation was
	// fetched against.
	gitHead, _ := extractGitHeadFromAttestation(attest)

	for _, rec := range candidates {
		if rec.yanked {
			continue
		}
		dists := proj.Releases[rec.version]
		for _, d := range dists {
			if d.PackageType != "sdist" {
				continue
			}
			if d.URL == "" {
				continue
			}
			result.RecordSignal(entityID, sigType, source, collectedAt, defaultTTL,
				map[string]any{
					"url":       d.URL,
					"version":   rec.version,
					"integrity": d.Digests.SHA256,
					"git_head":  gitHead, // populated from PEP 740 cert when present; empty otherwise (tag-match fallback)
				})
			return
		}
	}

	// No non-yanked sdist with a URL across the entire release history.
	// Recorded as absence so the artifact collector's downstream
	// readArtifactURL lookup surfaces AbsenceReasonNoArtifactURL rather
	// than silently no-opping.
	result.RecordAbsence(entityID, sigType, source,
		"no non-yanked sdist with a download URL in pypi response", false, collectedAt)
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

// versionFile pairs a version string with its first distribution filename
// and publish timestamp. Used by attestation-related signals (Phase A and
// Phase B) which need the filename to query the Integrity API per-version.
// Built once per Collect run and shared across phases.
type versionFile struct {
	version  string
	filename string
	ts       time.Time
}

// buildVersionFiles extracts the per-version (version, filename, timestamp)
// triples from a Project's releases. Each version uses its first distribution's
// filename and upload timestamp. Versions without a parseable timestamp or
// without a filename are skipped. The result is sorted newest-first.
func buildVersionFiles(proj *Project) []versionFile {
	var candidates []versionFile
	for ver, dists := range proj.Releases {
		if len(dists) == 0 {
			continue
		}
		t, err := time.Parse(time.RFC3339, dists[0].UploadTimeISO)
		if err != nil {
			continue
		}
		if dists[0].Filename == "" {
			continue
		}
		candidates = append(candidates, versionFile{
			version:  ver,
			filename: dists[0].Filename,
			ts:       t,
		})
	}

	// Sort newest-first.
	slices.SortFunc(candidates, func(a, b versionFile) int {
		if a.ts.Equal(b.ts) {
			return cmp.Compare(b.version, a.version)
		}
		return b.ts.Compare(a.ts)
	})

	return candidates
}

// recordTrustedPublishing checks the PyPI Integrity API for a PEP 740
// Sigstore attestation on the latest version's first distribution file.
// One additional HTTP call per Collect run (Phase A scope).
//
// The signal is emitted as present=true when an attestation bundle
// exists (publisher OIDC identity confirms the artifact was built in
// a known CI environment), or present=false when the Integrity API
// returns 404 (publisher hasn't opted in to trusted publishing).
//
// Network/server errors on the Integrity API are recorded as absence
// (retryable) so they don't block the rest of signal collection.
//
// Returns the *AttestationResponse for the latest version so Phase B
// (attestation_consistency) can reuse it without re-fetching.
// The bool indicates whether the attestation state is known: true means
// we got a definitive answer (attested or 404), false means we errored
// and the latest version's state is unknown. Phase B must not run when
// the state is unknown — nil+false is "don't know", nil+true is "absent".
func (c *Collector) recordTrustedPublishing(ctx context.Context, result *signal.CollectionResult,
	entityID, packageName string, versions []versionFile, collectedAt time.Time) (*AttestationResponse, bool) {

	if len(versions) == 0 {
		// No releases with valid timestamps and filenames — skip.
		// The absence for related signals is already recorded by
		// recordReleaseSignals.
		return nil, false
	}

	latest := versions[0]

	attest, err := c.client.GetAttestation(ctx, packageName, latest.version, latest.filename)
	if err != nil {
		// Integrity API failure — record as retryable absence so the
		// next analyze run re-attempts without failing the whole collection.
		reason := sanitizeFetchReason(err)
		result.RecordAbsence(entityID, "trusted_publishing", source, reason, true, collectedAt)
		result.RecordAbsence(entityID, "latest_attestation_builder", source, reason, true, collectedAt)
		return nil, false
	}

	if attest == nil {
		// 404 — no attestation exists. Emit present=false on both
		// signals; their absent-shape is informative (the package
		// isn't on trusted publishing).
		result.RecordSignal(entityID, "trusted_publishing", source, collectedAt, defaultTTL,
			map[string]any{
				"present":         false,
				"version_checked": latest.version,
			})
		result.RecordSignal(entityID, "latest_attestation_builder", source, collectedAt, defaultTTL,
			map[string]any{
				"present":           false,
				"version_checked":   latest.version,
				"extraction_status": "no_attestation",
			})
		return nil, true // state is known: definitively absent
	}

	// Attestation exists — extract publisher identity from the first bundle.
	value := map[string]any{
		"present":         true,
		"version_checked": latest.version,
	}
	if len(attest.Bundles) > 0 {
		pub := attest.Bundles[0].Publisher
		value["publisher_kind"] = pub.Kind
		value["source_repository"] = pub.Repository
		value["workflow"] = pub.Workflow
		if pub.Environment != "" {
			value["environment"] = pub.Environment
		}
	}

	result.RecordSignal(entityID, "trusted_publishing", source, collectedAt, defaultTTL, value)
	recordLatestAttestationBuilder(result, entityID, latest.version, attest, collectedAt)
	return attest, true
}

// recordLatestAttestationBuilder emits the latest_attestation_builder
// signal — a consolidating contract over the publisher identity the
// attestation binds to. Provides a stable, single-signal view of
// (builder_kind, source_repository, workflow, environment,
// source_revision) for sketch 5 (workflow_ref_transitions) and
// future composites to consume without merging fields from
// trusted_publishing (publisher block) and artifact_url (Fulcio-
// extracted git_head).
//
// The data is largely already on trusted_publishing; this signal
// re-emits it under a stable namespace and adds extraction_status
// reporting. source_revision is the hex SHA the Fulcio cert's
// source-repo-digest extension stamps — extracted via the same
// extractGitHeadFromAttestation helper artifact_url uses.
//
// Caller guarantees attest is non-nil. The 404 and fetch-error
// paths are handled at the recordTrustedPublishing dispatch site.
func recordLatestAttestationBuilder(result *signal.CollectionResult,
	entityID, latestVersion string, attest *AttestationResponse, collectedAt time.Time) {

	value := map[string]any{
		"present":           true,
		"version_checked":   latestVersion,
		"extraction_status": "ok",
	}
	if len(attest.Bundles) > 0 {
		pub := attest.Bundles[0].Publisher
		value["builder_kind"] = pub.Kind
		value["source_repository"] = pub.Repository
		value["workflow"] = pub.Workflow
		if pub.Environment != "" {
			value["environment"] = pub.Environment
		}
	}
	if sha, ok := extractGitHeadFromAttestation(attest); ok {
		value["source_revision"] = sha
	}
	result.RecordSignal(entityID, "latest_attestation_builder", source, collectedAt, defaultTTL, value)
}

// attestationWindow bounds the number of prior versions checked for
// attestation consistency. Separate from crossVersionWindow (used by
// release-metadata longitudinal signals) because each version here
// costs an HTTP call to the Integrity API.
const attestationWindow = 5

// recordAttestationConsistency checks whether PEP 740 attestations are
// consistent across recent versions. Detects the broken-chain
// fingerprint: a package that was continuously attested then publishes
// a version without attestation (the axios-2026 attack shape on PyPI).
//
// Uses a progressive probe to minimize cost: checks the first prior
// version (1 call). If both latest and first prior are unattested, the
// package never adopted trusted publishing and no signal is emitted.
// Only proceeds to a full sweep when a transition is detected or the
// chain needs depth verification.
//
// Emits nothing when: len(versions) < 2 (no history), or the probe
// shows the package never adopted trusted publishing, or the probe
// call errors out (records absence instead).
func (c *Collector) recordAttestationConsistency(ctx context.Context, result *signal.CollectionResult,
	entityID, packageName string, versions []versionFile, latestAttest *AttestationResponse,
	collectedAt time.Time) {

	if len(versions) < 2 {
		return // no history to compare
	}

	// Phase A already checked versions[0] (latest). Probe versions[1].
	latestHas := latestAttest != nil

	firstPrior := versions[1]
	firstAttest, err := c.client.GetAttestation(ctx, packageName, firstPrior.version, firstPrior.filename)
	if err != nil {
		result.RecordAbsence(entityID, "attestation_consistency", source,
			sanitizeFetchReason(err), true, collectedAt)
		return
	}
	firstPriorHas := firstAttest != nil

	// Early exit: latest and immediate prior are both unattested.
	// Package never adopted trusted publishing — no chain to verify.
	if !latestHas && !firstPriorHas {
		return
	}

	// --- Full sweep: check remaining prior versions ---

	// publisherID is a comparable struct used as a map key for detecting
	// distinct publishers. Using a struct avoids delimiter-based
	// serialization which would be ambiguous if any field contained the
	// delimiter character.
	type publisherID struct {
		kind       string
		repository string
		workflow   string
	}

	type versionAttest struct {
		version   string
		attested  bool
		publisher publisherID
	}

	extractPublisher := func(attest *AttestationResponse) publisherID {
		if attest == nil || len(attest.Bundles) == 0 {
			return publisherID{}
		}
		pub := attest.Bundles[0].Publisher
		return publisherID{kind: pub.Kind, repository: pub.Repository, workflow: pub.Workflow}
	}

	checked := []versionAttest{
		{version: versions[0].version, attested: latestHas, publisher: extractPublisher(latestAttest)},
		{version: firstPrior.version, attested: firstPriorHas, publisher: extractPublisher(firstAttest)},
	}

	// Check remaining prior versions (bounded to attestationWindow total).
	remaining := versions[2:]
	remaining = remaining[:min(len(remaining), attestationWindow-2)]

	versionsSkipped := 0
	for _, vf := range remaining {
		attest, err := c.client.GetAttestation(ctx, packageName, vf.version, vf.filename)
		if err != nil {
			// Degrade gracefully: skip this version rather than aborting.
			versionsSkipped++
			continue
		}
		checked = append(checked, versionAttest{
			version:   vf.version,
			attested:  attest != nil,
			publisher: extractPublisher(attest),
		})
	}

	// --- Assess consistency ---
	versionsAttested := 0
	versionsUnattested := 0
	publishers := map[publisherID]struct{}{}
	emptyPub := publisherID{}
	for _, r := range checked {
		if r.attested {
			versionsAttested++
			if r.publisher != emptyPub {
				publishers[r.publisher] = struct{}{}
			}
		} else {
			versionsUnattested++
		}
	}

	consistent := versionsUnattested == 0 || versionsAttested == 0

	// Detect transition: latest differs from the prior versions' state.
	transitionDetected := false
	transitionDirection := ""
	transitionAtVersion := ""

	if !consistent {
		priorAttested := 0
		for i := 1; i < len(checked); i++ {
			if checked[i].attested {
				priorAttested++
			}
		}

		if !latestHas && priorAttested > 0 {
			// Axios pattern: latest lost attestation that priors had.
			transitionDetected = true
			transitionDirection = "attested_to_unattested"
			transitionAtVersion = checked[0].version
		} else if latestHas && priorAttested < len(checked)-1 {
			// Adoption: latest gained attestation that priors lacked.
			transitionDetected = true
			transitionDirection = "unattested_to_attested"
			transitionAtVersion = checked[0].version
		}
	}

	publisherChanged := len(publishers) > 1

	// Build prior_publisher from the most recent attested prior version.
	var priorPublisher map[string]any
	for i := 1; i < len(checked); i++ {
		if checked[i].attested && checked[i].publisher != emptyPub {
			pub := checked[i].publisher
			priorPublisher = map[string]any{
				"kind":       pub.kind,
				"repository": pub.repository,
				"workflow":   pub.workflow,
			}
			break
		}
	}

	// Workflow-ref tracking (sketch 5): captures attesting-workflow
	// identity changes across the window. The existing
	// transition_detected boolean covers attestation PRESENCE
	// transitions (axios shape: attestation lost or gained). It
	// misses the TanStack-shape careful-variant where every version
	// is attested but the attesting workflow ref changes. The new
	// fields close that gap:
	//
	//   - workflow_refs:           per-version list (newest-first),
	//                              empty string when unattested
	//   - latest_workflow_ref:     workflow on the latest version
	//   - unique_workflow_refs:    count of distinct non-empty
	//                              workflows seen in the window
	//   - workflow_ref_transitions: adjacent-pair workflow differences
	//                              (counts both presence- and
	//                              workflow-changes, since
	//                              "" → "X" is an adjacent diff)
	workflowRefs := make([]string, len(checked))
	uniqueWorkflows := map[string]struct{}{}
	for i, r := range checked {
		workflowRefs[i] = r.publisher.workflow
		if r.publisher.workflow != "" {
			uniqueWorkflows[r.publisher.workflow] = struct{}{}
		}
	}
	workflowRefTransitions := 0
	for i := 1; i < len(workflowRefs); i++ {
		if workflowRefs[i] != workflowRefs[i-1] {
			workflowRefTransitions++
		}
	}

	value := map[string]any{
		"consistent":               consistent,
		"versions_checked":         len(checked),
		"versions_attested":        versionsAttested,
		"versions_unattested":      versionsUnattested,
		"versions_skipped":         versionsSkipped,
		"transition_detected":      transitionDetected,
		"transition_direction":     transitionDirection,
		"transition_at_version":    transitionAtVersion,
		"publisher_changed":        publisherChanged,
		"workflow_refs":            workflowRefs,
		"latest_workflow_ref":      workflowRefs[0],
		"unique_workflow_refs":     len(uniqueWorkflows),
		"workflow_ref_transitions": workflowRefTransitions,
	}
	if priorPublisher != nil {
		value["prior_publisher"] = priorPublisher
	}

	result.RecordSignal(entityID, "attestation_consistency", source, collectedAt, defaultTTL, value)
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
	name, ok := strings.CutPrefix(entity.CanonicalURI, "pkg:pypi/")
	if !ok || name == "" {
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
