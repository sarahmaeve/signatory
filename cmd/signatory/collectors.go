package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	gitcollector "github.com/sarahmaeve/signatory/internal/signal/git"
	ghcollector "github.com/sarahmaeve/signatory/internal/signal/github"
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
// v0.1 contract:
//
//   - The github API collector is always included (github is the
//     only hosting platform currently supported; other platforms
//     land with GitLab adoption in a later version).
//   - The git local-clone collector is always required. Either
//     --path must point at an existing clone of the target, or
//     --clone plus --path must request a fresh clone. The absence
//     of both is a hard error — the caller's intent cannot be
//     satisfied silently.
//
// Returns an error (not a signal-level failure) when the operator's
// intent cannot be satisfied, so the orchestrator surfaces a clean
// one-line message and exits non-zero rather than producing a
// half-complete analysis that looks successful.
func collectorsFor(ctx context.Context, entity *profile.Entity, opts CollectOpts) ([]signal.Collector, error) {
	collectors := []signal.Collector{
		ghcollector.NewCollector(),
	}

	clonePath, err := resolveClonePath(ctx, entity, opts)
	if err != nil {
		return nil, err
	}
	collectors = append(collectors, gitcollector.NewCollector(clonePath))
	return collectors, nil
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

	if err := validateExistingClone(ctx, absPath, entity.CanonicalURI); err != nil {
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
func validateExistingClone(ctx context.Context, path, expectedURI string) error {
	info, err := os.Stat(filepath.Join(path, ".git"))
	if err != nil || info == nil {
		return fmt.Errorf("%w: %q", ErrPathNotAClone, path)
	}

	//nolint:gosec // G204: argv-form exec of "git"; path is operator-supplied
	cmd := exec.CommandContext(ctx, "git", "-C", path, "remote", "get-url", "origin")
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
func gitCloneFull(ctx context.Context, url, dest string) error {
	//nolint:gosec // G204: argv-form exec of "git"; url pre-validated by safeGitCloneURL, dest pre-validated by ensurePathEmptyOrMissing
	cmd := exec.CommandContext(ctx, "git", "clone", url, dest)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone %q into %q: %w: %s",
			url, dest, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
