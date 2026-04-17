package git

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// firstCommitDateFormat is the git log format for the one-line
// pass that returns the author date of a given commit. %ai is
// ISO-8601 strict-form; we keep it verbatim in the signal value
// so consumers can parse it with time.Parse(time.RFC3339) (or the
// equivalent in their language).
const firstCommitDateFormat = "--format=%ai"

// collectFirstCommitDate finds the repository's root commit and
// emits first_commit_date with its author date.
//
// Naive `git log --reverse --format=%ai -n 1 HEAD` does NOT work:
// per git-log(1), --reverse reverses commits "chosen to be shown"
// AFTER -n takes its slice from the default descending order. So
// `-n 1 --reverse` returns the single newest commit (sliced first,
// then reversed to itself), not the oldest.
//
// Correct approach: `git rev-list --max-parents=0 HEAD` returns
// the root commit SHA(s) — parents=0 means "no parent," which
// uniquely identifies root commits. Multi-root repos (octopus
// grafts, imported histories) can have several; we take the first,
// which is a defensible choice when "first commit" is inherently
// fuzzy in that topology.
//
// A shallow clone would report the oldest commit inside the depth
// window, not the repo's actual inception; the registry entry's
// caveats document that. This collector does not detect or
// annotate shallow-clone shortening — shallow-clone handling is
// explicitly a v0.2 concern per the plan.
func (c *Collector) collectFirstCommitDate(
	ctx context.Context,
	result *signal.CollectionResult,
	entityID string,
	now time.Time,
	ttl time.Duration,
) {
	// Empty-repo pre-check. Same pattern as commit-signing and
	// authorship — distinguishes "no commits" (absence) from "git
	// log failed" (failure).
	if _, err := runGit(ctx, c.path, "rev-parse", "--verify", "HEAD"); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			reason := c.sanitize(err.Error())
			result.RecordFailure(entityID, "first_commit_date", sourceName, reason, true, now)
			return
		}
		result.RecordAbsence(entityID, "first_commit_date", sourceName,
			"repo has no commits on HEAD", false, now)
		return
	}

	rootOut, err := runGit(ctx, c.path, "rev-list", "--max-parents=0", "HEAD")
	if err != nil {
		retryable := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		result.RecordFailure(entityID, "first_commit_date", sourceName,
			c.sanitize(err.Error()), retryable, now)
		return
	}
	rootShas := strings.Fields(strings.TrimSpace(string(bytes.TrimRight(rootOut, "\n"))))
	if len(rootShas) == 0 {
		// Defensive: rev-parse succeeded but rev-list found no
		// root. Should not happen — every HEAD reachable from a
		// non-empty repo has at least one root — but record as a
		// failure rather than silently moving on.
		result.RecordFailure(entityID, "first_commit_date", sourceName,
			"no root commits found on HEAD", false, now)
		return
	}

	// Multi-root repos exist (octopus / imported histories). Take
	// the first root SHA; any choice is arguable here and this is
	// reproducible.
	rootSha := rootShas[0]

	out, err := runGit(ctx, c.path, "log", "-1", firstCommitDateFormat, rootSha)
	if err != nil {
		retryable := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		result.RecordFailure(entityID, "first_commit_date", sourceName,
			c.sanitize(err.Error()), retryable, now)
		return
	}

	raw := strings.TrimSpace(string(bytes.TrimRight(out, "\n")))
	if raw == "" {
		result.RecordFailure(entityID, "first_commit_date", sourceName,
			"git log returned no date for root commit", false, now)
		return
	}

	t, err := time.Parse("2006-01-02 15:04:05 -0700", raw)
	if err != nil {
		result.RecordFailure(entityID, "first_commit_date", sourceName,
			"unparseable commit date: "+c.sanitize(err.Error()), false, now)
		return
	}

	result.RecordSignal(entityID, "first_commit_date", sourceName, now, ttl,
		map[string]any{
			"date":     t.UTC().Format(time.RFC3339),
			"era":      string(profile.ClassifyEra(t)),
			"days_ago": int(now.Sub(t).Hours() / 24),
		})
}
