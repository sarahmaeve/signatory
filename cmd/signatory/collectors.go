package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sarahmaeve/signatory/internal/gitenv"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	gitcollector "github.com/sarahmaeve/signatory/internal/signal/git"
	ghcollector "github.com/sarahmaeve/signatory/internal/signal/github"
	openssfcollector "github.com/sarahmaeve/signatory/internal/signal/openssf"
	gopublishcollector "github.com/sarahmaeve/signatory/internal/signal/registry/gopublish"
	npmcollector "github.com/sarahmaeve/signatory/internal/signal/registry/npm"
	repofilescollector "github.com/sarahmaeve/signatory/internal/signal/repofiles"
)

// CollectOpts carries per-invocation options from AnalyzeCmd's
// flags into the collector-assembly path. Currently only git
// local-clone resolution is option-driven; additional options
// (alternate clone strategies, ecosystem-specific flags) land
// here as they're needed.
type CollectOpts struct {
	// Path is an absolute-or-relative filesystem path. In "read
	// mode" (Clone == false) it must already contain a valid git
	// clone of the analyze target. In "create mode"
	// (Clone == true) it must either not exist or be empty, and
	// the collector will clone the target into it.
	Path string

	// Clone, when true, creates a new clone at Path. Fails loudly
	// if Path is non-empty. Always a full clone — shallow clones
	// would silently degrade first_commit_date and historical
	// authorship signals; shallow-clone support is a v0.2 concern.
	Clone bool
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
//     collector (npm, pypi, ...) is added separately by Phase A.4
//     wiring; this function returns an empty slice for them today.
//     --path/--clone are NOT required in this case; the sentinel
//     ErrCloneRequired only fires for git-hosted entities.
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
			collectors = append(collectors, npmcollector.NewCollector())
		case "golang", "go":
			collectors = append(collectors, gopublishcollector.NewCollector())
		}
	}

	// Git-hosted collectors. An entity qualifies when its URL is
	// populated — either from a repo: scheme target at creation
	// time, or from an npm package whose A.5 resolution found a
	// github-hosted repository. Empty URL → no git origin → skip
	// the github/git collector pair and do not require --path/--clone.
	if isGitHostedEntity(entity) {
		clonePath, err := resolveClonePath(ctx, entity, opts)
		if err != nil {
			return nil, err
		}
		collectors = append(collectors,
			ghcollector.NewCollector(),
			gitcollector.NewCollector(clonePath),
			repofilescollector.NewCollector(clonePath),
			// OpenSSF Scorecard — additional hygiene signal for
			// github-hosted entities. Caches the API response so
			// dispatched analysts read scorecard data via
			// signatory_signals instead of re-fetching from
			// api.securityscorecards.dev each run (the cache-miss
			// pattern flagged repeatedly in the dogfood reports).
			openssfcollector.NewCollector(),
		)
	}

	return collectors, nil
}

// isGitHostedEntity reports whether an entity has a git origin the
// github + git-local-clone collectors can operate against.
//
// Non-empty URL is the gate: upstream code sets URL only after
// validation — resolved.CloneURL for repo: entities is github-only
// (other platforms yield an error before reaching this point); the
// npm provider's github-allowlist check gates pkg: entities in A.5;
// tests inject filesystem paths for local-clone-without-network
// scenarios. An empty URL is the unambiguous "nothing to clone"
// signal — unresolved npm packages, gitlab repos before the
// collector lands, etc.
func isGitHostedEntity(entity *profile.Entity) bool {
	return entity != nil && entity.URL != ""
}

// resolveClonePath enforces the --path / --clone contract and
// returns an absolute path to a verified local clone of entity.
//
// The happy paths produce a path; every unhappy path returns a
// sentinel-wrapped error that `signatory analyze` surfaces
// directly to the operator.
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
		if err := ensurePathEmptyOrMissing(absPath); err != nil {
			return "", err
		}
		if err := safeGitCloneURL(entity.URL); err != nil {
			return "", fmt.Errorf("entity URL unsafe for clone: %w", err)
		}
		if err := gitCloneFull(ctx, entity.URL, absPath); err != nil {
			return "", err
		}
		return absPath, nil
	}

	// validateExistingClone compares the clone's origin against a
	// repo:github/... URI. For repo-scheme entities the entity's
	// CanonicalURI is already that form. For pkg-scheme entities
	// (npm packages whose source has been resolved to a github
	// repo in A.5), CanonicalURI is pkg:npm/<name>, which would
	// never match the clone's resolved origin URI. Derive the
	// expected URI from entity.URL instead — that's the declared
	// github source regardless of entity type.
	expectedURI, _, _, err := profile.NormalizeGitHubRepoInput(entity.URL)
	if err != nil {
		return "", fmt.Errorf("entity URL %q not parseable as a github target: %w", entity.URL, err)
	}
	if err := validateExistingClone(ctx, absPath, expectedURI); err != nil {
		return "", err
	}
	return absPath, nil
}

// ensurePathEmptyOrMissing returns nil if path is a non-existent
// directory (we'll create it) or an existing empty directory (we'll
// clone into it). Returns a wrapped ErrPathNotEmpty otherwise.
//
// This is the "never clobber" half of the --clone contract: if the
// operator points --clone at a directory with existing content —
// another clone, their personal work, the wrong target's checkout —
// we refuse rather than overwrite.
func ensurePathEmptyOrMissing(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat --path %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: --path %q is not a directory", ErrPathNotEmpty, path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("read --path %q: %w", path, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: %q contains %d entries", ErrPathNotEmpty, path, len(entries))
	}
	return nil
}

// validateExistingClone checks path is a git clone whose origin
// URL normalizes to expectedURI.
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
func validateExistingClone(ctx context.Context, path, expectedURI string) error {
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
	originURI, _, _, err := profile.NormalizeGitHubRepoInput(originRaw)
	if err != nil {
		return fmt.Errorf("clone origin URL %q not parseable as a github target: %w",
			originRaw, err)
	}

	if originURI != expectedURI {
		return fmt.Errorf("%w: origin resolves to %q, target resolves to %q",
			ErrOriginMismatch, originURI, expectedURI)
	}
	return nil
}

// gitCloneFull performs `git clone <url> <dest>` with full history
// (no --depth). Full-history is non-negotiable for v0.1 because
// several git-collector signals (first_commit_date, windowed
// authorship tallies over a 12-month window, commit-signing
// ratios) silently degrade on shallow clones. The registry caveats
// document which signals are affected; the --clone contract
// prevents the operator from hitting those caveats accidentally.
//
// Caller must have validated url via safeGitCloneURL and dest via
// ensurePathEmptyOrMissing before invoking this.
//
// Subprocess discipline (env scrubbing + post-kill pipe-drain
// bound) comes from gitenv.NewCloneCmd — clone-shaped operations
// may fork ssh/askpass/credential-helper grandchildren that won't
// inherit SIGKILL, so WaitDelay caps cmd.Wait's pipe-drain. See
// the gitenv package doc for the threat shape and why WaitDelay
// is scoped to clone-shaped sites only. Symmetric with
// defaultGitClone in handoff.go.
func gitCloneFull(ctx context.Context, url, dest string) error {
	cmd := gitenv.NewCloneCmd(ctx, "clone", url, dest)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone %q into %q: %w: %s",
			url, dest, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
