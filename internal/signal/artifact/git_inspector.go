package artifact

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/sarahmaeve/signatory/internal/gitenv"
)

// gitInspector is the production GitInspector backed by git
// subprocesses against a local clone path. Routes every git
// invocation through gitenv.NewCmd so the env-strip + WaitDelay
// discipline applies (matters because attacker-controlled refs in
// .git/config could otherwise direct git to fork helpers we don't
// want to inherit).
//
// All three methods are read-only ports against an already-cloned
// repo. None of them write into .git/, so concurrent inspection
// against the same path is safe (relevant for future parallelism).
type gitInspector struct {
	clonePath string
}

// NewGitInspector returns a GitInspector reading from clonePath.
// clonePath is expected to be the post-cloneToTempIsolated path
// (the orchestrator's already-validated isolated clone) — the
// inspector does not re-validate origin or shallow-ness.
func NewGitInspector(clonePath string) GitInspector {
	return &gitInspector{clonePath: clonePath}
}

// Tags returns the list of tag names in the clone.
//
// `git tag --list` prints one tag per line. Empty output (no tags)
// returns an empty slice, not an error: a fresh repo without
// releases is a legitimate state, and the resolver's downstream
// logic treats an empty tag list as "no tag-match candidates"
// rather than as a collection failure.
func (g *gitInspector) Tags(ctx context.Context) ([]string, error) {
	stdout, err := g.runCapture(ctx, "tag", "--list")
	if err != nil {
		return nil, fmt.Errorf("git tag --list: %w", err)
	}
	return splitNonEmptyLines(stdout), nil
}

// PathsAtRef returns the file paths under ref's tree, equivalent to
// `git ls-tree -r --name-only <ref>`. Only blob (file) entries are
// emitted — tree (directory) entries are intentionally excluded
// by ls-tree's contract, which is what makes the file-set
// comparison in ComputeDiff symmetric.
//
// A non-existent ref returns an error from git; the caller (the
// collector) records that as an absence with the failure reason
// included for operator diagnostics.
func (g *gitInspector) PathsAtRef(ctx context.Context, ref string) ([]string, error) {
	stdout, err := g.runCapture(ctx, "ls-tree", "-r", "--name-only", ref)
	if err != nil {
		return nil, fmt.Errorf("git ls-tree -r --name-only %q: %w", ref, err)
	}
	return splitNonEmptyLines(stdout), nil
}

// CommitForRef resolves ref to its commit SHA via `git rev-parse
// --verify`. The --verify flag asks git to ensure the resolved
// object is reachable; without it, rev-parse would happily echo
// back a syntactically-valid SHA-shaped string the caller might
// then mistake for a real commit.
func (g *gitInspector) CommitForRef(ctx context.Context, ref string) (string, error) {
	stdout, err := g.runCapture(ctx, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("git rev-parse --verify %q: %w", ref, err)
	}
	return strings.TrimSpace(stdout), nil
}

// runCapture is the shared subprocess primitive: builds a git
// command via gitenv.NewCmd (so env-strip + WaitDelay apply),
// runs it -C clonePath, and returns stdout on success or the
// stderr-tail-tagged error on failure.
//
// Stderr is captured into the error so operators see WHY git
// refused — the most common failure mode is "ref doesn't exist
// in this clone," and surfacing git's own message is more
// actionable than a generic "subprocess failed."
func (g *gitInspector) runCapture(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"-C", g.clonePath}, args...)
	cmd := gitenv.NewCmd(ctx, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// splitNonEmptyLines splits s on newline and drops empty trailing
// entries. git's porcelain commands typically end with a trailing
// newline; without this, every result would have a phantom empty
// string at the end that downstream set comparisons would
// mistakenly count as a real entry.
func splitNonEmptyLines(s string) []string {
	raw := strings.Split(strings.TrimRight(s, "\n"), "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}
