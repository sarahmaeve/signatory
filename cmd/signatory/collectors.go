package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/gitenv"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	adoptioncollector "github.com/sarahmaeve/signatory/internal/signal/adoption"
	artifactcollector "github.com/sarahmaeve/signatory/internal/signal/artifact"
	cadencecollector "github.com/sarahmaeve/signatory/internal/signal/cadence"
	exfilwatchcollector "github.com/sarahmaeve/signatory/internal/signal/exfilwatch"
	forgejocollector "github.com/sarahmaeve/signatory/internal/signal/forgejo"
	gitcollector "github.com/sarahmaeve/signatory/internal/signal/git"
	ghcollector "github.com/sarahmaeve/signatory/internal/signal/github"
	gitlabcollector "github.com/sarahmaeve/signatory/internal/signal/gitlab"
	openssfcollector "github.com/sarahmaeve/signatory/internal/signal/openssf"
	cargocollector "github.com/sarahmaeve/signatory/internal/signal/registry/cargo"
	gemcollector "github.com/sarahmaeve/signatory/internal/signal/registry/gem"
	gopublishcollector "github.com/sarahmaeve/signatory/internal/signal/registry/gopublish"
	mavencollector "github.com/sarahmaeve/signatory/internal/signal/registry/maven"
	npmcollector "github.com/sarahmaeve/signatory/internal/signal/registry/npm"
	pypicollector "github.com/sarahmaeve/signatory/internal/signal/registry/pypi"
	repofilescollector "github.com/sarahmaeve/signatory/internal/signal/repofiles"
	sourcecollector "github.com/sarahmaeve/signatory/internal/signal/source"
)

// Artifact-vs-repo collector tuning. artifactHTTPCap caps the HTTP
// body read; artifactHTTPBudget caps the per-fetch wallclock. The
// archive-side limits (uncompressed total, per-entry size, entry
// count, compression ratio) live in stream.DefaultLimits and apply
// when CollectorConfig.Limits is left zero-valued — keeping the
// caps centralized in the stream package matches the rest of the
// defensive surface (see internal/artifact/stream).
const (
	artifactHTTPCap    = 256 << 20 // 256 MiB on the wire
	artifactHTTPBudget = 60 * time.Second
)

// CollectOpts carries per-invocation options from AnalyzeCmd's
// flags into the collector-assembly path. Currently only git
// local-clone resolution is option-driven; additional options
// (alternate clone strategies, ecosystem-specific flags) land
// here as they're needed.
type CollectOpts struct {
	// Path is an absolute-or-relative filesystem path. In "read
	// mode" (Clone == false) it must already contain a valid full
	// (non-shallow) git clone of the analyze target. In "manage
	// mode" (Clone == true) ensureCloneAtPath drives it to that
	// state — clone if missing/empty, fetch if present, fetch
	// --unshallow if shallow.
	Path string

	// Clone, when true, makes ensureCloneAtPath responsible for
	// driving Path to a current, complete full clone of the
	// target's git origin. Idempotent against pre-existing clones:
	// fetches refs (and unshallows shallow clones) rather than
	// re-cloning. Origin mismatch is loud-fail.
	Clone bool

	// RunGit injects the git subprocess runner for clone-shaped
	// operations (clone / fetch / fetch --unshallow). nil → fall
	// through to defaultRunGit, which builds via gitenv.NewCloneCmd.
	// Tests pass a recorder closure to assert on subcommand shape
	// without spawning real git. Threaded from AnalyzeCmd.RunGit.
	RunGit func(ctx context.Context, workdir string, args ...string) error

	// Stderr captures progress narration from the clone phase
	// (cloning / refreshing / unshallowing). nil suppresses
	// narration entirely — programmatic callers and tests that
	// don't care about the chatter just leave it nil.
	// AnalyzeCmd.Run threads its own stderr writer here so manual
	// CLI invocations narrate to the user's terminal.
	Stderr io.Writer

	// AllowFetch enables the source-evolution collector's
	// BlobStreamer fetch-on-missing-SHA path (--allow-fetch CLI
	// flag). Default false: missing SHAs surface as
	// tag_sha_local_status="missing_from_clone" in the matrix
	// row rather than triggering a remote fetch — preserving the
	// missing-SHA observation as a forgery-resistant signal rather
	// than expanding the network surface to fetch and overwrite it.
	AllowFetch bool

	// InRunResult is a pointer to the orchestrator's accumulated
	// CollectionResult. The source-evolution collector reads
	// gopublish's just-emitted version_pin_table from here. The
	// orchestrator (AnalyzeCmd.Run) updates the pointed-to struct
	// as each collector returns; source-evolution sees the latest
	// state when its Collect runs (last in the dispatch order for
	// Go entities, after gopublish).
	//
	// nil disables the in-run lookup; the source collector falls
	// back to Store-only.
	InRunResult *signal.CollectionResult

	// Store is the persistent signal store passed through to the
	// source-evolution collector for fallback pin-table lookup
	// (when a previous analysis ran gopublish but the current run
	// hasn't, e.g., querying a re-run without --refresh).
	//
	// Defined as the narrow sourcecollector.SignalStore interface
	// so cmd/signatory doesn't have to import the full store
	// package's interface; store.Store satisfies it via
	// structural typing.
	Store sourcecollector.SignalStore

	// EntityStore is the persistent store viewed through the
	// EntityStore narrow interface that EVERY ecosystem collector
	// supporting publisher / signer entity minting uses — only the
	// EnsureEntityByCanonicalURI primitive needed to mint:
	//
	//   - identity:github/<login> + org:github/<name> for repo
	//     owners (Path A; entity-burn1.md §3.1)
	//   - identity:npm/<login> for npm maintainers + per-version
	//     publishers (Path C)
	//   - identity:pypi/<login> for pypi maintainers / authors
	//     (Path E)
	//   - identity:gpg/<keyid> for git per-developer signing keys
	//     (Path F)
	//
	// Same concrete value as Store in production (analyze.go threads
	// the orchestrator's *store.SQLite into both fields); typed as
	// the github collector's interface here for historical reasons
	// — every collector's narrow EntityStore has the same method set
	// and the same *store.SQLite satisfies all of them via
	// structural typing at the call sites.
	//
	// Optional. nil disables entity minting in every collector —
	// tests that don't care about that side effect leave it nil and
	// each collector silently skips its mint branch. The base
	// signals (owner_profile, maintainer_count, etc.) still emit
	// regardless.
	EntityStore ghcollector.EntityStore

	// Cleanups, when non-nil, is the registry resolveClonePath
	// appends deferred-cleanup callbacks to. Currently used for the
	// temp-clone isolation step that defends against attacker-
	// controlled `.git/config` and `.git/hooks/` shipped in the
	// operator-supplied --path directory: the structural sibling of
	// the gitenv `-c` override defense for CVE-2025-41390 / CWE-829
	// (design/analysis/cve-2025-41390.md). resolveClonePath clones
	// the validated path into a fresh tempdir so collectors observe
	// a freshly-minted .git/config (only `core` defaults +
	// `[remote "origin"]`) and only the default sample-only
	// .git/hooks/, then registers the tempdir-removal callback here.
	//
	// The orchestrator (collectFreshSignals in analyze.go) drains
	// these in LIFO order after every collector completes. Tests
	// that don't run collectors and accept tempdir leakage between
	// test functions can leave nil; the OS reclaims /tmp space
	// eventually.
	Cleanups *[]func() error
}

// Sentinel errors for each failure mode of collectorsFor.
// Surfaced as errors.Is checks in tests and as clear top-level
// messages at the CLI; v0.1 Invariant 2 requires fail-loudly on
// each of these rather than silent skip of the git collector.
var (
	ErrCloneRequired  = errors.New("git-hosted entity requires --path (existing clone) or --clone (create clone at --path)")
	ErrPathMissing    = errors.New("--clone requires --path")
	ErrPathNotEmpty   = errors.New("refusing to clone: --path directory is not empty")
	ErrPathNotAClone  = errors.New("--path does not point at a git clone (no .git directory)")
	ErrOriginMismatch = errors.New("clone origin does not match target entity")
	// ErrShallowClone is returned by validateExistingClone (no --clone,
	// just --path) when the path holds a shallow clone. Shallow clones
	// degrade historical signals (first_commit_date, authorship windows,
	// signing ratios); callers should re-run with --clone to unshallow.
	// The --clone branch's ensureCloneAtPath does NOT return this — it
	// remediates the shallow state by issuing `git fetch --unshallow`.
	ErrShallowClone = errors.New("clone at --path is shallow; re-run with --clone to unshallow")
	// ErrCloneAuthRequiredOrMissing classifies the failure mode where
	// the forge returns an HTTP 401 and our discipline (credential.helper=
	// in gitenv.safeOverrides + GIT_TERMINAL_PROMPT=0 in gitenv.SafeEnv)
	// correctly refuses to supply or prompt for credentials. The
	// underlying git message ("could not read Username ...: terminal
	// prompts disabled") is technically accurate but operator-misleading:
	// it points at our termination of prompts, not at the actual cause —
	// the repo does not exist, is private to the operator's account, or
	// has moved. classifyGitCloneError detects the stderr pattern and
	// rewrites the error with this sentinel + an actionable explanation.
	//
	// Security invariant: this sentinel is a UX classification, not a
	// security control. The rewrite happens AFTER the subprocess has
	// exited; argv, env, safeOverrides, and SafeEnv remain unmodified.
	// "Fixing the error message" must not become a path to admitting
	// credentials or enabling prompts — see classifyGitCloneError's
	// godoc for the full design rationale.
	ErrCloneAuthRequiredOrMissing = errors.New(
		"forge returned an authentication challenge; repository does not exist, is private, or has moved")
)

// collectorsFor returns the collectors appropriate for an entity,
// applying runtime options like --path / --clone for local-clone
// based collectors.
//
// v0.1 contract (per-entity dispatch):
//
//   - Git-hosted entities (resolves to a github clone URL): the
//     github API collector AND the git local-clone collector run.
//     --path or --clone+--path is REQUIRED; absence is a hard error.
//     This is the legacy `signatory analyze <owner/repo>` flow.
//   - Registry-package entities with no resolved repo URL
//     (EntityPackage + empty URL): neither github nor git-local-clone
//     apply — the entity has no git origin to examine. The ecosystem
//     collector (npm, pypi, golang, ...) is added by the per-ecosystem
//     switch below; ecosystems without a wired collector return an
//     empty slice. --path/--clone are NOT required in this case; the
//     sentinel ErrCloneRequired only fires for git-hosted entities.
//
// The contract's generalization from "always [github, git]" to
// "dispatch by entity shape" is the Phase A.2 refactor. Prior to it,
// npm targets would spuriously trigger ErrCloneRequired because the
// git-local-clone branch fired unconditionally.
func collectorsFor(ctx context.Context, entity *profile.Entity, opts CollectOpts) ([]signal.Collector, error) {
	var collectors []signal.Collector

	// Ecosystem-specific registry collectors. Each ecosystem's
	// collector lands additively as it ships — npm and Go modules
	// are the wired ecosystems today; PyPI and others come next.
	//
	// The Go collector matches both "golang" (purl-spec canonical,
	// post-2026-04-28) and "go" (older signatory convention, kept
	// for backwards compat with entities created before the URI
	// canonicalization). Symmetric with the resolver registry's
	// dual registration in internal/ecosystem/resolver/gomod.go.
	if entity != nil {
		switch entity.Ecosystem {
		case "npm":
			// WithEntityStore wires the publisher-entity minting
			// branch in the npm collector (Path C). nil-safe — when
			// opts.EntityStore is nil (e.g., a test that doesn't
			// care about the side effect) the branch silently skips.
			collectors = append(collectors, npmcollector.NewCollector().WithEntityStore(opts.EntityStore))
		case "pypi":
			// PyPI parallel to the npm branch — mints
			// identity:pypi/<login> rows for the publisher logins
			// extractable from info.maintainer / info.author / the
			// PEP 639 maintainers list, and emits a maintainer_count
			// signal feeding the cascade resolver's pypi-registry
			// dispatch (entity-burn1.md "Pending work #1").
			collectors = append(collectors, pypicollector.NewCollector().WithEntityStore(opts.EntityStore))
		case "cargo", "crates":
			collectors = append(collectors, cargocollector.NewCollector().WithEntityStore(opts.EntityStore))
		case "gem":
			collectors = append(collectors, gemcollector.NewCollector().WithEntityStore(opts.EntityStore))
		case "maven":
			collectors = append(collectors, mavencollector.NewCollector().WithEntityStore(opts.EntityStore))
		case "golang", "go":
			collectors = append(collectors, gopublishcollector.NewCollector())
		}
	}

	// API-only collectors that need entity.URL but NOT a local
	// clone. These run regardless of --path/--clone state — landing
	// them before the clone-gated block means a bare repo: target
	// without --clone still gets every API-derived signal that can
	// be collected.
	//
	// Membership: every collector here makes HTTP requests against
	// a forge or third-party API and reads nothing from the local
	// filesystem.
	//
	//   - github             api.github.com           (forge metadata)
	//   - forgejo            codeberg.org/api/v1      (forge metadata)
	//   - gitlab             gitlab.com/api/v4        (forge metadata)
	//   - adoption           api.github.com search    (cross-ecosystem)
	//   - openssf-scorecard  api.securityscorecards.dev
	//
	// Each collector owns its own host self-gate — github skips
	// non-github URLs, forgejo skips non-codeberg, gitlab skips
	// non-gitlab, adoption skips non-Go-shaped entities, openssf
	// skips non-github. The dispatcher stays host-agnostic; the
	// "[name] Collected 0 signals" noise from a fired self-gate is
	// suppressed at the narration layer by
	// signal.CollectionResult.WorthNarrating.
	//
	// Adoption's WithInRun is load-bearing: it reads "stars" from
	// the in-run accumulator emitted by whichever forge metadata
	// collector ran first. Dispatch order here puts the three
	// per-forge collectors BEFORE adoption so the stars signal is
	// available when adoption.Collect runs. Reordering would break
	// the refs_to_stars ratio (silently zero) — pinned by adoption
	// package's TestCollector_NoStarsInRun_StillEmits.
	if isGitHostedEntity(entity) {
		collectors = append(collectors,
			ghcollector.NewCollector().WithEntityStore(opts.EntityStore),
			forgejocollector.NewCollector(),
			gitlabcollector.NewCollector(),
			adoptioncollector.NewCollector().WithInRun(opts.InRunResult),
			openssfcollector.NewCollector(),
			// cadence is a derived collector — reads sibling collectors'
			// last_commit (or last_push) and last_publish emissions from
			// opts.InRunResult and emits one signal per entity. Appended
			// LAST in the API-only block so it runs after the forge and
			// registry collectors have populated inRun. Internal gate:
			// emits nothing when either side is missing, so repo-only
			// and registry-only entities skip silently.
			cadencecollector.NewCollector().WithInRun(opts.InRunResult),
		)
	}

	// Clone-required collectors. An entity qualifies for these when
	// its URL is populated — either from a repo: scheme target at
	// creation time, or from an npm package whose A.5 resolution
	// found a github-hosted repository. Empty URL → no git origin →
	// skip every clone-required collector and do not require
	// --path/--clone.
	//
	// Membership: each collector reads from the local clone (.git/
	// directory or the worktree filesystem).
	//
	//   - git         reads .git/ for commit history / signing / tags
	//   - repofiles   reads filesystem for governance files
	//   - exfilwatch  scans worktree for HTTP-capture hostnames
	//   - source      reads .git/ for tag-shaped source evolution
	//   - artifact    diffs clone tree against registry tarball
	//
	// All forge-agnostic: a clone from github / codeberg / gitlab
	// (or a local-path test fixture, validated as a forge clone by
	// validateExistingClone) reads identically.
	//
	// Graceful degradation: when registry or API-only collectors are
	// already queued and the clone path can't be satisfied (no
	// --path, no --clone), warn and skip clone-required collectors
	// rather than aborting the entire run. The user's `--refresh`
	// intent is "collect whatever you can"; refusing all signals
	// because a clone wasn't requested is hostile. Hard errors
	// (origin mismatch, path not a clone) still fail loudly — those
	// indicate user misconfiguration, not a missing optional flag.
	if isGitHostedEntity(entity) {
		clonePath, err := resolveClonePath(ctx, entity, opts)
		if err != nil {
			// If the ONLY problem is that --path/--clone wasn't
			// passed, and we already have registry / API-only
			// collectors, degrade gracefully: skip clone-required
			// collectors, warn the user.
			if errors.Is(err, ErrCloneRequired) && len(collectors) > 0 {
				if opts.Stderr != nil {
					_, _ = fmt.Fprintf(opts.Stderr,
						"note: pass --clone to also collect git + repofiles + exfilwatch signals from %s\n",
						entity.URL)
				}
				return collectors, nil
			}
			return nil, err
		}
		collectors = append(collectors,
			// WithEntityStore wires the GPG signer-entity minting
			// branch in the git collector (Path F). nil-safe — when
			// opts.EntityStore is nil the branch silently skips.
			// Mirrors the github / npm / pypi wiring pattern.
			gitcollector.NewCollector(clonePath).WithEntityStore(opts.EntityStore),
			repofilescollector.NewCollector(clonePath),
			// exfilwatch — literal scan of the clone tree for
			// HTTP-capture-as-a-service hostnames. Cheap deterministic
			// signal motivated by the BufferZoneCorp campaign (May 2026,
			// design/threat-landscape/2026-05-02-bufferzonecorp-campaign.md).
			// Ecosystem-agnostic: every git-hosted entity gets the scan.
			exfilwatchcollector.NewCollector(clonePath),
		)

		// Source-evolution: per-version AST feature matrix.
		// Requires clonePath AND a supported ecosystem — depends on
		// the version_pin_table signal via opts.InRunResult /
		// opts.Store. For Go that table comes from gopublish; for
		// pypi from the pypi registry collector's attestation sweep.
		// The collector itself selects the per-language file filter
		// and analyzer from entity.Ecosystem: Go uses go/parser, pypi
		// the hand-written Python analyzer, npm the hand-written
		// JS/TS analyzer — all emit the full AST feature matrix;
		// structural + diff flow for every supported ecosystem. The
		// npm pin table comes from the npm registry collector's
		// gitHead/attestation synthesis (same in-run accumulator
		// path as gopublish for Go and the attestation sweep for
		// pypi).
		//
		// Appended LAST in the dispatch order so by the time it
		// runs, the orchestrator's in-run accumulator already holds
		// the version_pin_table emitted earlier in the same run.
		if entity.Ecosystem == "golang" || entity.Ecosystem == "go" ||
			entity.Ecosystem == "pypi" || entity.Ecosystem == "npm" {
			pinSource := sourcecollector.NewPinSource(opts.InRunResult, opts.Store)
			collectors = append(collectors,
				sourcecollector.NewCollector(clonePath, pinSource, opts.AllowFetch),
			)
		}

		// Artifact-vs-repo: compares the registry-published source
		// tarball against the git tree at the resolved tag. Closes
		// the highest-leverage signal gap documented in
		// design/threat-landscape/example-xz-utils-cve-2024-3094.md
		// (CVE-2024-3094 — backdoor in the dist tarball, absent from
		// git at the same tag).
		//
		// Gated on (a) entity has a registry collector queued
		// (so artifact_url will be in InRun), and (b) clone path is
		// resolved (we have a repo side to diff against). Currently
		// covers npm, cargo, pypi, gem, and golang:
		//
		//   - npm: registry supplies gitHead in versions[v].gitHead
		//     for v≥5 publishes; tag-match fallback otherwise.
		//   - cargo: registry exposes no gitHead, so the collector's
		//     CaptureIntent recovers the publisher-stamped SHA from
		//     .cargo_vcs_info.json inside the tarball (written by
		//     `cargo publish` itself, not user input).
		//   - pypi: registry exposes no gitHead and there's no
		//     equivalent of cargo's vcs_info embedded in the sdist.
		//     Pair resolution falls through to tag-match against the
		//     local clone's tag list. Confidence: pair_match.
		//   - gem: outer .gem is a plain (uncompressed) tar holding
		//     data.tar.gz + metadata.gz + checksums.yaml.gz. The
		//     collector walks the outer with FormatTar, captures
		//     data.tar.gz via CaptureIntent, then re-walks those
		//     bytes as FormatTarGzip — the inner manifest feeds the
		//     diff. rubygems.org exposes no gitHead, so pair
		//     resolution also falls through to tag-match.
		//   - golang/go: proxy.golang.org records Origin.Hash in the
		//     .info block — the publisher-stamped commit SHA, same
		//     provenance strength as cargo's vcs_info. Pair resolves
		//     at exact_gitHead. Modules ship as zip with a multi-
		//     segment "<module>@<version>/" wrapping prefix.
		//
		// maven: extend the gate when its collector learns to emit
		// artifact_url. Note: source-jars only — class-jars compare
		// bytecode to source, a category error.
		//
		// Known limitation: the tag-match fallback in
		// internal/signal/artifact/pair.go only handles bare
		// "<version>" and "v<version>" tag names. Repos that use
		// "release-<version>" or "<name>-<version>" prefixes will
		// fall through to AbsenceReasonPairUnresolved unless their
		// ecosystem path supplies an exact gitHead (npm registry
		// or cargo vcs_info). Tracking as a separate hardening item.
		//
		// Appended LAST so the in-run accumulator already holds the
		// upstream registry collector's artifact_url emission by the
		// time Collect runs. Same dispatch-order discipline as
		// source-evolution above.
		//
		// Streams the tarball: no tempdir, no on-disk artifact. The
		// HTTP fetcher's response body feeds directly into the
		// header-only walker. Per the smallest-and-safest design,
		// no bytes are ever written to disk (or persisted in
		// filestore/) — re-runs re-fetch, but tarballs are small
		// and the cache invalidation cost beats the bandwidth.
		if entity.Ecosystem == "npm" || entity.Ecosystem == "cargo" || entity.Ecosystem == "pypi" || entity.Ecosystem == "gem" || entity.Ecosystem == "golang" || entity.Ecosystem == "go" {
			collectors = append(collectors,
				artifactcollector.NewCollector(artifactcollector.CollectorConfig{
					InRun:     opts.InRunResult,
					ClonePath: clonePath,
					Fetcher: artifactcollector.NewStreamArtifactFetcher(
						artifactcollector.StreamFetcherOptions{
							MaxBytes: artifactHTTPCap,
							Timeout:  artifactHTTPBudget,
						}),
					Git: artifactcollector.NewGitInspector(clonePath),
					// Limits left zero-valued — stream.Walk fills with
					// stream.DefaultLimits (256MiB total / 64MiB entry /
					// 100k entries / 100:1 ratio / 256MiB compressed).
					// Same effective cap as the legacy MaxArchiveBytes
					// (256 MiB) that was wired here before.
				}),
			)
		}
	}

	return collectors, nil
}

// isGitHostedEntity reports whether an entity has a git origin the
// forge + git-local-clone collectors can operate against.
//
// Non-empty URL is the gate: upstream code sets URL only after
// validation — resolved.CloneURL for repo: entities is populated by
// the recognized forges (github, gitlab, codeberg) and unsupported
// platforms yield an error before reaching this point; the npm
// provider's github-allowlist check gates pkg: entities in A.5;
// tests inject filesystem paths for local-clone-without-network
// scenarios. An empty URL is the unambiguous "nothing to clone"
// signal — unresolved npm packages, repos whose source could not be
// resolved, etc.
func isGitHostedEntity(entity *profile.Entity) bool {
	return entity != nil && entity.URL != ""
}

// resolveClonePath enforces the --path / --clone contract and
// returns an absolute path to a sanitized local clone of entity.
//
// The happy paths produce a path; every unhappy path returns a
// sentinel-wrapped error that `signatory analyze` surfaces
// directly to the operator.
//
// File-vector defense (Fix #2 from design/analysis/cve-2025-41390.md):
// the returned path is NOT the operator's --path directly. After
// validation succeeds, cloneToTempIsolated clones the validated path
// into a fresh tempdir, and that tempdir is what collectors operate
// against. The clone produces a minted `.git/config` (no attacker
// directives carry over) and only the default sample `.git/hooks/`
// (no executable hooks carry over). When opts.Cleanups is non-nil
// (production wires this), the tempdir-removal callback is registered
// there for the orchestrator to drain after collectors finish.
//
// Normalization of entity.URL to a github canonical URI is deferred
// to the call sites that actually need it (validateExistingClone, and
// ensureCloneAtPath's existing-clone branches). The fresh-clone path
// only needs entity.URL to be a safe `git clone` argument — which
// includes filesystem-path URLs that test fixtures use as local
// "remotes." Lifting the normalization up here would block that.
func resolveClonePath(ctx context.Context, entity *profile.Entity, opts CollectOpts) (string, error) {
	if opts.Clone && opts.Path == "" {
		return "", ErrPathMissing
	}
	if opts.Path == "" {
		return "", ErrCloneRequired
	}

	absPath, err := filepath.Abs(opts.Path)
	if err != nil {
		return "", fmt.Errorf("resolve --path %q: %w", opts.Path, err)
	}

	if opts.Clone {
		if err := safeGitCloneURL(entity.URL); err != nil {
			return "", fmt.Errorf("entity URL unsafe for clone: %w", err)
		}
		if err := ensureCloneAtPath(ctx, opts.Stderr, opts.RunGit, absPath, entity.URL); err != nil {
			return "", err
		}
		return isolatedClonePath(ctx, absPath, opts)
	}

	// --path-only branch: entity.URL must be a forge-parseable URI so
	// validateExistingClone can compare origins. For repo-scheme entities
	// the entity's CanonicalURI is already that form; for pkg-scheme
	// entities (npm/pypi packages whose source has been resolved to a
	// github repo), CanonicalURI is pkg:<eco>/<name> — not what we need.
	// Derive expectedURI from entity.URL instead, which is the declared
	// source regardless of entity type.
	//
	// NormalizeForgeRepoInput accepts URL and SCP forms across the
	// recognized forges (github, codeberg, gitlab); the github-only
	// NormalizeGitHubRepoInput it replaces left the host segment in
	// owner ("codeberg.org:forgejo") and rejected SCP-form codeberg/
	// gitlab origins via validPathSegment.
	expectedURI, _, _, _, err := profile.NormalizeForgeRepoInput(entity.URL)
	if err != nil {
		return "", fmt.Errorf("entity URL %q not parseable as a forge target: %w", entity.URL, err)
	}
	if err := validateExistingClone(ctx, absPath, expectedURI); err != nil {
		return "", err
	}
	return isolatedClonePath(ctx, absPath, opts)
}

// isolatedClonePath wraps cloneToTempIsolated with the
// opts.Cleanups registration step. Pulled out as a helper so the two
// resolveClonePath branches (--clone and --path-only) share one
// implementation of the file-vector defense's last-mile plumbing.
func isolatedClonePath(ctx context.Context, validatedPath string, opts CollectOpts) (string, error) {
	tempPath, cleanup, err := cloneToTempIsolated(ctx, validatedPath)
	if err != nil {
		return "", err
	}
	if opts.Cleanups != nil {
		*opts.Cleanups = append(*opts.Cleanups, cleanup)
	}
	return tempPath, nil
}

// cloneToTempIsolated runs `git clone <srcPath> <tempdir>` and
// returns the tempdir path, a cleanup callback that removes it, and
// any error from the clone subprocess.
//
// File-vector defense (Fix #2 from design/analysis/cve-2025-41390.md):
// the structural sibling of gitenv's `-c` override prefix. Where
// gitenv's overrides neutralize specific attacker directives at every
// invocation, this helper neutralizes the entire on-disk attack
// surface by producing a fresh clone whose `.git/config` is git's
// minted default (only `core` + `[remote "origin"]`) and whose
// `.git/hooks/` contains only the .sample template files git
// installs from /usr/share/git-core/templates. Attacker-controlled
// versions of either CANNOT carry over: `git clone <local> <dest>`
// does not copy hooks, and the destination's config is freshly
// written rather than copied.
//
// The clone subprocess goes through gitenv.NewCloneCmd, so:
//
//   - The `-c` override prefix from Fix #1 protects THIS clone from
//     directives in srcPath's `.git/config` that would otherwise
//     fire during the clone itself (e.g., `core.hooksPath` for the
//     reference-transaction hook on dest). Fix #1 and Fix #2 thus
//     compose: Fix #1 protects the clone-time read, Fix #2 the
//     downstream-collector reads.
//   - Env-strip applies (no GIT_DIR / GIT_CONFIG_KEY_* leaking from
//     the parent).
//   - WaitDelay applies, even though `git clone` of a local path
//     doesn't typically fork ssh/askpass — the clone-shaped
//     constructor is the right discipline by analogy.
//
// On error, tempPath is empty and cleanup is nil so the caller can
// neither use the path nor double-call cleanup. The partial tempdir
// is removed before the error returns.
func cloneToTempIsolated(ctx context.Context, srcPath string) (tempPath string, cleanup func() error, err error) {
	dest, err := os.MkdirTemp("", "signatory-iso-clone-*")
	if err != nil {
		return "", nil, fmt.Errorf("create isolation tempdir: %w", err)
	}
	// Remove the empty tempdir that os.MkdirTemp created. `git
	// clone` requires its destination to be missing OR empty; we
	// pass it the path and let git create the directory. (Empty
	// works too on most git versions, but missing-or-empty is the
	// documented contract — the empty branch lets us race with
	// other processes that might create files in the destination
	// between MkdirTemp and clone start.)
	if err := os.Remove(dest); err != nil {
		return "", nil, fmt.Errorf("clear isolation tempdir before clone: %w", err)
	}

	cmd := gitenv.NewCloneCmd(ctx, "clone", "--quiet", srcPath, dest)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		// Best-effort cleanup of any partial state git left behind.
		// Ignore the RemoveAll error; the underlying clone failure is
		// what the caller needs to see.
		_ = os.RemoveAll(dest)
		return "", nil, fmt.Errorf("clone %q to isolation tempdir: %w: %s",
			srcPath, runErr, strings.TrimSpace(stderr.String()))
	}

	cleanup = func() error {
		return os.RemoveAll(dest)
	}

	// Restore the operator's origin URL on the temp clone. `git clone
	// <local> <dest>` defaults dest's origin to <local> (the operator's
	// path), which silently breaks any downstream `git fetch origin` —
	// notably source-evolution's --allow-fetch recovery, which expects
	// to reach the real upstream remote (typically github), not loop
	// back into the operator's already-stale clone.
	//
	// Best-effort: an origin-less source repo (test fixtures, fresh
	// `git init` without `git remote add origin`) is tolerated. The
	// production validation chain (validateCloneOrigin / safeGitCloneURL)
	// guarantees a github-shaped origin upstream, so this branch is
	// only ever a no-op outside of tests.
	//
	// Security note. The origin URL we read from srcPath is, in
	// production, already validated as github-parseable by
	// validateCloneOrigin (in the --path-only flow) or set by an
	// already-validated entity.URL through safeGitCloneURL (in the
	// --clone flow). Re-applying the same value is safe; no exec
	// directive fires at config-set time, and any future fetch goes
	// through gitenv.NewCloneCmd's `-c protocol.ext.allow=never` etc.
	// override prefix.
	if originErr := preserveOperatorOrigin(ctx, srcPath, dest); originErr != nil {
		_ = cleanup()
		return "", nil, fmt.Errorf("preserve operator origin on isolation clone: %w", originErr)
	}

	return dest, cleanup, nil
}

// preserveOperatorOrigin reads srcPath's origin URL and writes it as
// destPath's origin URL, replacing whatever git's clone subprocess
// initialized (which would be srcPath itself).
//
// Quietly succeeds when srcPath has no origin set: that's an origin-
// less source repo (a fresh `git init` without `git remote add
// origin`), legal in test fixtures and not a failure mode the
// production --path-only path can reach because validateCloneOrigin
// requires an origin.
//
// Returns an error from set-url. Read errors get classified as "no
// origin" (most common reason: no `[remote "origin"]` block in the
// source's config); all other read failures still surface via
// cmd.Run's exec error path, which is fine for diagnostics.
func preserveOperatorOrigin(ctx context.Context, srcPath, destPath string) error {
	readCmd := gitenv.NewCmd(ctx, "-C", srcPath, "remote", "get-url", "origin")
	var stdout bytes.Buffer
	readCmd.Stdout = &stdout
	if err := readCmd.Run(); err != nil {
		// Treat any read failure as "no origin to preserve." In
		// production, validateCloneOrigin has already proven the
		// origin is github-shaped before reaching this code; a
		// later disappearance is not something we'd want to
		// fail-loud on. In tests, an origin-less fixture is the
		// expected case.
		return nil //nolint:nilerr // intentional: read-failure → graceful degradation
	}
	originURL := strings.TrimSpace(stdout.String())
	if originURL == "" {
		return nil
	}

	writeCmd := gitenv.NewCmd(ctx, "-C", destPath, "remote", "set-url", "origin", originURL)
	var stderr bytes.Buffer
	writeCmd.Stderr = &stderr
	if err := writeCmd.Run(); err != nil {
		return fmt.Errorf("set isolation clone origin %q: %w: %s",
			originURL, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// validateCloneOrigin checks path is a git clone whose origin
// URL normalizes to expectedURI. Does NOT check shallow-ness —
// callers that want to allow shallow (the --clone branch, which
// will unshallow) call this directly; callers that want to reject
// shallow (the --path-only branch) call validateExistingClone,
// which composes this with cloneIsShallow.
//
// The normalization cross-check is load-bearing: an operator who
// cloned the wrong repo into --path would otherwise get a
// collector run that emits signals attributed to the declared
// entity but computed over a different repo's history. That's a
// trust-model violation (attribution without grounding), not a
// stylistic issue.
//
// Subprocess discipline (env scrubbing) comes from gitenv.NewCmd.
// This is local porcelain — `git -C path remote get-url origin`
// reads the repo's config and does not fork network helpers — so
// WaitDelay is intentionally NOT applied here. See the gitenv
// package doc "Why clone-only" for the rationale.
func validateCloneOrigin(ctx context.Context, path, expectedURI string) error {
	info, err := os.Stat(filepath.Join(path, ".git"))
	if err != nil || info == nil {
		return fmt.Errorf("%w: %q", ErrPathNotAClone, path)
	}

	cmd := gitenv.NewCmd(ctx, "-C", path, "remote", "get-url", "origin")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("read clone origin for %q: %w: %s",
			path, err, strings.TrimSpace(stderr.String()))
	}

	originRaw := strings.TrimSpace(stdout.String())
	originURI, _, _, _, err := profile.NormalizeForgeRepoInput(originRaw)
	if err != nil {
		return fmt.Errorf("clone origin URL %q not parseable as a forge target: %w",
			originRaw, err)
	}

	if originURI != expectedURI {
		return fmt.Errorf("%w: origin resolves to %q, target resolves to %q",
			ErrOriginMismatch, originURI, expectedURI)
	}
	return nil
}

// validateExistingClone is the --path-only contract: path must be a
// git clone of the expected target AND must not be shallow. Shallow
// clones silently degrade history-dependent signals (first_commit_date,
// authorship windows, signing ratios), so refusing them here defends
// the store from poisoned positive signals — the failure mode flagged
// in design/openssf-problem.txt.
//
// Callers that want to ALLOW (and remediate) a shallow clone — the
// --clone branch's ensureCloneAtPath, which unshallows — call
// validateCloneOrigin directly and check cloneIsShallow themselves.
func validateExistingClone(ctx context.Context, path, expectedURI string) error {
	if err := validateCloneOrigin(ctx, path, expectedURI); err != nil {
		return err
	}
	shallow, err := cloneIsShallow(path)
	if err != nil {
		return err
	}
	if shallow {
		return fmt.Errorf("%w: %q (re-run with --clone to unshallow)", ErrShallowClone, path)
	}
	return nil
}

// cloneIsShallow reports whether path holds a shallow git clone.
// Returns (true, nil) iff path/.git/shallow exists; (false, nil) when
// it doesn't; (false, err) on unexpected stat errors. Plain Lstat —
// no subprocess — keeps the cost trivial and removes one possible
// failure mode from the inspection step.
func cloneIsShallow(path string) (bool, error) {
	_, err := os.Lstat(filepath.Join(path, ".git", "shallow"))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("stat .git/shallow at %q: %w", path, err)
}

// ensureCloneAtPath drives absPath to a current, complete clone of
// cloneURL. Four cases:
//
//  1. Path missing or empty → fresh full clone (`git clone <url> <path>`).
//  2. Path holds a valid full clone of the expected target → `git fetch`.
//  3. Path holds a valid SHALLOW clone of the expected target →
//     `git fetch --unshallow`.
//  4. Path holds a clone of a DIFFERENT target → ErrOriginMismatch (no
//     git operation performed; we must not clobber unrelated work).
//
// Any other state (a non-empty non-clone directory, a non-directory
// file at path, an unreadable directory) is a refusal with the
// appropriate sentinel — same "never clobber" property the older
// ensurePathEmptyOrMissing guarded.
//
// cloneURL is passed verbatim to `git clone` for the fresh-clone case;
// callers must have validated it via safeGitCloneURL upstream. For
// existing-clone cases, cloneURL is also normalized via
// NormalizeGitHubRepoInput to derive the expectedURI compared against
// the clone's recorded origin — done lazily inside this function so a
// filesystem-path cloneURL (test fixtures) is acceptable for fresh
// clones even though it wouldn't normalize as github.
//
// stderr captures one progress line per action (cloning / refreshing /
// unshallowing) emitted BEFORE the runner is invoked, so the user sees
// intent first and the failure reason second if the operation fails.
// nil suppresses narration entirely — programmatic callers and tests
// that don't care about chatter pass nil rather than a discard writer.
//
// runner is the git subprocess injection; nil falls through to
// defaultRunGit. The runner is responsible for routing through
// gitenv.NewCloneCmd so env-strip + WaitDelay discipline applies.
//
// The function does NOT acquire any lock — callers running concurrently
// against the same path will race. v0.1 is single-process; concurrent
// /analyze runs against the same target are out of scope and can be
// added with a flock when the use case appears.
func ensureCloneAtPath(
	ctx context.Context,
	stderr io.Writer,
	runner func(ctx context.Context, workdir string, args ...string) error,
	absPath, cloneURL string,
) error {
	if runner == nil {
		runner = defaultRunGit
	}

	// progress emits one stderr line if a writer was provided. Errors
	// from the write are intentionally swallowed: this is diagnostic
	// chatter, not contract output, and propagating a broken-pipe
	// error here would mask the underlying clone/fetch error.
	progress := func(format string, args ...any) {
		if stderr == nil {
			return
		}
		_, _ = fmt.Fprintf(stderr, format, args...)
	}

	// Case 1a: path doesn't exist → fresh clone.
	info, err := os.Stat(absPath)
	if errors.Is(err, os.ErrNotExist) {
		progress("Cloning %s into %s ...\n", cloneURL, absPath)
		return runner(ctx, "", "clone", cloneURL, absPath)
	}
	if err != nil {
		return fmt.Errorf("stat --path %q: %w", absPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: --path %q is not a directory", ErrPathNotEmpty, absPath)
	}

	// Case 1b: path exists but is empty → fresh clone.
	entries, err := os.ReadDir(absPath)
	if err != nil {
		return fmt.Errorf("read --path %q: %w", absPath, err)
	}
	if len(entries) == 0 {
		progress("Cloning %s into %s ...\n", cloneURL, absPath)
		return runner(ctx, "", "clone", cloneURL, absPath)
	}

	// Path exists with content. We need a forge-parseable cloneURL to
	// compare origins. Filesystem-path cloneURLs (test fixtures) get
	// rejected here, which is the right outcome — a real existing clone
	// can only have a real forge origin under signatory's threat model.
	expectedURI, _, _, _, err := profile.NormalizeForgeRepoInput(cloneURL)
	if err != nil {
		return fmt.Errorf("entity URL %q not parseable as a forge target: %w", cloneURL, err)
	}
	if err := validateCloneOrigin(ctx, absPath, expectedURI); err != nil {
		return err
	}

	shallow, err := cloneIsShallow(absPath)
	if err != nil {
		return err
	}
	if shallow {
		// Case 3: shallow → unshallow + refresh refs. --tags is omitted;
		// `--unshallow` already implies a refs refresh as part of its
		// download.
		progress("Unshallowing clone at %s ...\n", absPath)
		return runner(ctx, absPath, "fetch", "--unshallow")
	}
	// Case 2: full clone → refresh refs.
	progress("Refreshing clone at %s ...\n", absPath)
	return runner(ctx, absPath, "fetch")
}

// defaultRunGit is the production runner that ensureCloneAtPath
// falls back to when the caller passes a nil runner. Routes every
// invocation through gitenv.NewCloneCmd: clone and fetch operations
// both talk to a remote (and may fork ssh/askpass/credential-helper
// grandchildren), so the WaitDelay + env-strip discipline applies
// to both.
//
// workdir, when non-empty, is prepended as `-C workdir` so the
// subcommand operates against an existing clone without an
// os.Chdir() race. workdir empty means args already carry the
// destination (the clone case: `clone <url> <dest>`).
//
// Errors are routed through classifyGitCloneError before the existing
// "git %v: %w: %s" wrap fires — this rewrites the misleading
// "could not read Username … terminal prompts disabled" stderr into
// an operator-actionable "repo does not exist, is private, or has
// moved" message without altering the no-credentials discipline.
// See classifyGitCloneError and ErrCloneAuthRequiredOrMissing.
func defaultRunGit(ctx context.Context, workdir string, args ...string) error {
	full := args
	if workdir != "" {
		full = append([]string{"-C", workdir}, args...)
	}
	cmd := gitenv.NewCloneCmd(ctx, full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if classified := classifyGitCloneError(args, err, stderr.String()); classified != nil {
			return classified
		}
		return fmt.Errorf("git %v: %w: %s", args, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// classifyGitCloneError detects the "could not read Username" +
// "terminal prompts disabled" stderr pair git emits when the forge
// returns HTTP 401 and our credential discipline (credential.helper=
// in gitenv.safeOverrides + GIT_TERMINAL_PROMPT=0 in gitenv.SafeEnv)
// correctly refuses to supply or prompt for credentials. Returns a
// wrapped error tagged with ErrCloneAuthRequiredOrMissing and an
// operator-facing explanation; returns nil when the stderr does not
// match this pattern, leaving the caller's existing wrap in place.
//
// Both phrases must be present: each alone has innocent causes —
// "could not read Username" appears in unrelated SSH-helper failures,
// "terminal prompts disabled" appears in any GIT_TERMINAL_PROMPT=0
// stderr — but the pair is unambiguous because git emits them
// together exactly when 401 meets disabled-prompts.
//
// # Security invariant
//
// This helper does NOT alter argv, env, or the subprocess in any
// way. It only inspects stderr after the subprocess has exited and
// rewrites the Go error string. Reviewers must reject any change
// here that:
//
//   - touches gitenv (SafeEnv / safeOverrides / NewCloneCmd)
//   - supplies credentials (via netrc, GIT_ASKPASS, env vars, etc.)
//   - enables terminal prompts (clearing GIT_TERMINAL_PROMPT)
//   - admits a credential.helper override
//
// "Fixing the error message" must not become a path to weakening
// the env-vector / file-vector defenses gitenv enforces. The
// classification is a UX layer over the security primitive; the
// primitive stays intact.
//
// # Wrap shape
//
// The returned error chains both ErrCloneAuthRequiredOrMissing
// (sentinel for errors.Is) and runErr (sentinel for callers that
// match exit-status-shaped failures), and includes the original
// stderr verbatim so debugging is unaffected. A future patch that
// drops the original stderr "for cleanliness" would silently
// degrade incident response and is rejected by the test suite.
func classifyGitCloneError(args []string, runErr error, stderr string) error {
	if !strings.Contains(stderr, "could not read Username") ||
		!strings.Contains(stderr, "terminal prompts disabled") {
		return nil
	}
	return fmt.Errorf(
		"git %v: %w (signatory deliberately does not pass credentials to forge servers; "+
			"verify the URL with 'git ls-remote <url>' using your normal credential setup, then re-run): %w: %s",
		args, ErrCloneAuthRequiredOrMissing, runErr, strings.TrimSpace(stderr))
}

// gitCloneFull performs `git clone <url> <dest>` with full history
// (no --depth). Full-history is non-negotiable for v0.1 because
// several git-collector signals (first_commit_date, windowed
// authorship tallies over a 12-month window, commit-signing
// ratios) silently degrade on shallow clones. The registry caveats
// document which signals are affected.
//
// In production, ensureCloneAtPath dispatches `clone` operations
// through the runner injection (defaultRunGit fallback); this helper
// is retained as the package-private full-clone primitive used by
// tests that need a deterministic local fixture clone (see the
// RunGit closures in analyze_clone_test.go).
//
// Caller must have validated url via safeGitCloneURL before invoking
// this; ensureCloneAtPath enforces "dest is missing or empty" before
// the dispatched runner reaches a `clone` op, so this helper does not
// re-check.
//
// Subprocess discipline (env scrubbing + post-kill pipe-drain
// bound) comes from gitenv.NewCloneCmd — clone-shaped operations
// may fork ssh/askpass/credential-helper grandchildren that won't
// inherit SIGKILL, so WaitDelay caps cmd.Wait's pipe-drain. See
// the gitenv package doc for the threat shape and why WaitDelay
// is scoped to clone-shaped sites only. Symmetric with
// defaultGitClone in handoff.go.
func gitCloneFull(ctx context.Context, url, dest string) error {
	args := []string{"clone", url, dest}
	cmd := gitenv.NewCloneCmd(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Same auth-required classification as defaultRunGit so the
		// two near-identical helpers don't drift on operator-facing
		// error shape. See classifyGitCloneError for the pattern and
		// the security invariant (this is a UX rewrite, not a
		// security relaxation).
		if classified := classifyGitCloneError(args, err, stderr.String()); classified != nil {
			return classified
		}
		return fmt.Errorf("git clone %q into %q: %w: %s",
			url, dest, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
