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

	"github.com/sarahmaeve/signatory/internal/gitenv"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	gitcollector "github.com/sarahmaeve/signatory/internal/signal/git"
	ghcollector "github.com/sarahmaeve/signatory/internal/signal/github"
	openssfcollector "github.com/sarahmaeve/signatory/internal/signal/openssf"
	gopublishcollector "github.com/sarahmaeve/signatory/internal/signal/registry/gopublish"
	npmcollector "github.com/sarahmaeve/signatory/internal/signal/registry/npm"
	repofilescollector "github.com/sarahmaeve/signatory/internal/signal/repofiles"
	sourcecollector "github.com/sarahmaeve/signatory/internal/signal/source"
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
	// row rather than triggering a remote fetch. See
	// design/coll7.md D11 for the network-surface rationale.
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

	// EntityStore is the persistent store viewed through the github
	// collector's narrow EntityStore interface — only the
	// EnsureEntityByCanonicalURI primitive needed to mint
	// identity:github/<login> and org:github/<name> rows for repo
	// owners (Path A; design/entity-burn1.md §3.1).
	//
	// Same concrete value as Store in production (analyze.go threads
	// the orchestrator's *store.SQLite into both fields); separate
	// field because the two narrow interfaces have different methods
	// and Go's structural typing applies at the call site, not the
	// field type.
	//
	// Optional. nil disables owner-entity minting in the github
	// collector — tests that don't care about that side effect leave
	// it nil and the collector silently skips the mint. The
	// owner_profile signal still emits regardless.
	EntityStore ghcollector.EntityStore
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
			ghcollector.NewCollector().WithEntityStore(opts.EntityStore),
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

		// Source-evolution: per-version AST feature matrix
		// (design/coll7.md). Requires clonePath AND a Go ecosystem
		// classification — depends on gopublish's version_pin_table
		// emission via opts.InRunResult / opts.Store.
		//
		// Appended LAST in the dispatch order so by the time it
		// runs, the orchestrator's in-run accumulator already
		// holds gopublish's version_pin_table from the same run.
		if entity.Ecosystem == "golang" || entity.Ecosystem == "go" {
			pinSource := sourcecollector.NewPinSource(opts.InRunResult, opts.Store)
			collectors = append(collectors,
				sourcecollector.NewCollector(clonePath, pinSource, opts.AllowFetch),
			)
		}
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
		return absPath, nil
	}

	// --path-only branch: entity.URL must be a github-parseable URI so
	// validateExistingClone can compare origins. For repo-scheme entities
	// the entity's CanonicalURI is already that form; for pkg-scheme
	// entities (npm/pypi packages whose source has been resolved to a
	// github repo), CanonicalURI is pkg:<eco>/<name> — not what we need.
	// Derive expectedURI from entity.URL instead, which is the declared
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

	// Path exists with content. We need a github-parseable cloneURL to
	// compare origins. Filesystem-path cloneURLs (test fixtures) get
	// rejected here, which is the right outcome — a real existing clone
	// can only have a real github origin under signatory's threat model.
	expectedURI, _, _, err := profile.NormalizeGitHubRepoInput(cloneURL)
	if err != nil {
		return fmt.Errorf("entity URL %q not parseable as a github target: %w", cloneURL, err)
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
func defaultRunGit(ctx context.Context, workdir string, args ...string) error {
	full := args
	if workdir != "" {
		full = append([]string{"-C", workdir}, args...)
	}
	cmd := gitenv.NewCloneCmd(ctx, full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %v: %w: %s", args, err, strings.TrimSpace(stderr.String()))
	}
	return nil
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
	cmd := gitenv.NewCloneCmd(ctx, "clone", url, dest)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone %q into %q: %w: %s",
			url, dest, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
