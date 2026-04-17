package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/signal"
)

// Identity-signal tuning.
const (
	// topDomainsLimit bounds how many domain entries land in
	// identity_domain_consistency.top_domains. The full list would
	// bloat the signal value on polyglot repos without adding
	// analytical value — top N is the typical consumption shape.
	topDomainsLimit = 10

	// topAuthorsLimit bounds the per-signal top-author list size.
	// Same reasoning as topDomainsLimit.
	topAuthorsLimit = 10

	// topAuthorsShareCutoff is the N in "top-N author share" —
	// the concentration metric surfaced alongside the top-author
	// share, e.g. "top 3 hold 75% of commits over the window."
	topAuthorsShareCutoff = 3
)

// authorshipFormat is the git log format for the batched
// identity-signal pass: author name and author email, 0x1F-
// separated, newline-terminated. Fields are small and do not
// contain newlines, so line-based record splitting works.
//
// Git applies .mailmap normalization to %aN / %aE by default —
// log.mailmap is on unless explicitly disabled in repo or user
// config — so identities are canonicalized before we see them.
// That's the behavior we want: the mailmap is there precisely so
// tools compute identity stats on the canonical form.
const authorshipFormat = "--format=%aN\x1f%aE"

// authorshipRow is one parsed record — a (name, email) pair.
type authorshipRow struct {
	Name  string
	Email string
}

// parseAuthorshipLog parses the newline-separated, 0x1F-field-
// separated output of git log in authorshipFormat. Malformed
// records (fewer than two fields) are skipped silently. Returns
// an empty slice for empty input rather than nil.
func parseAuthorshipLog(data []byte) []authorshipRow {
	lines := bytes.Split(data, []byte{'\n'})
	out := make([]authorshipRow, 0, len(lines))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		fields := bytes.Split(line, []byte{0x1F})
		if len(fields) < 2 {
			continue
		}
		out = append(out, authorshipRow{
			Name:  string(fields[0]),
			Email: string(fields[1]),
		})
	}
	return out
}

// extractEmailDomain returns the domain portion of an email
// address (the text after the last '@'). Empty string for
// malformed or empty input — callers treat that as "unknown
// domain" without special-casing.
//
// Lowercased for case-insensitive tallying. "Alice@Example.Com"
// and "bob@example.com" should share one bucket, since DNS is
// case-insensitive and the mixed-case form is purely cosmetic.
func extractEmailDomain(email string) string {
	email = strings.TrimSpace(email)
	if email == "" {
		return ""
	}
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}

// mailmapPath returns the filesystem path to the repo's .mailmap
// file. Git looks for .mailmap at the repo root; we follow suit.
func (c *Collector) mailmapPath() string {
	return filepath.Join(c.path, ".mailmap")
}

// parseMailmap counts non-blank, non-comment lines in a .mailmap.
// Each such line is one identity-mapping entry. Blank lines and
// lines beginning with '#' (after optional leading whitespace)
// are skipped — matching git's own .mailmap parser behavior.
//
// Returns the count; does NOT return the individual entries.
// The depth signal is about mapping-count volume as a trust
// posture ("this project actively reconciles contributor
// identities"); specific mappings are PII and not trust-relevant.
func parseMailmap(data []byte) int {
	var count int
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		if trimmed[0] == '#' {
			continue
		}
		count++
	}
	return count
}

// collectIdentityGraphDepth emits the identity_graph_depth signal
// by parsing .mailmap at the repo root. Absent file → absence;
// read error → failure.
func (c *Collector) collectIdentityGraphDepth(
	_ context.Context,
	result *signal.CollectionResult,
	entityID string,
	now time.Time,
	ttl time.Duration,
) {
	path := c.mailmapPath()
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is c.path + ".mailmap" by construction, not user-supplied at this layer
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			result.RecordAbsence(entityID, "identity_graph_depth", sourceName,
				"no .mailmap file at repo root", false, now)
			return
		}
		result.RecordFailure(entityID, "identity_graph_depth", sourceName,
			c.sanitize(err.Error()), false, now)
		return
	}

	entries := parseMailmap(data)
	result.RecordSignal(entityID, "identity_graph_depth", sourceName, now, ttl,
		map[string]any{
			"mailmap_entries": entries,
			"present":         true,
		})
}

// collectAuthorshipSignals runs one batched git-log pass and
// emits both identity_domain_consistency and
// effective_maintainer_concentration.
//
// Sharing a single subprocess call across two signals matches the
// v0.1 principle of reducing subprocess invocations where the
// underlying data is identical. The cost of separating them would
// be one extra `git log` call; the benefit would be independent
// failure modes. Since the two failure modes here really ARE
// coupled (if log fails, neither signal has data), the shared-
// pass form is the simpler, honest choice.
func (c *Collector) collectAuthorshipSignals(
	ctx context.Context,
	result *signal.CollectionResult,
	entityID string,
	now time.Time,
	ttl time.Duration,
) {
	// Empty-repo pre-check — same pattern as commit-signing.
	// Distinguishes "repo has no commits" (absence) from "git
	// log failed for another reason" (failure).
	if _, err := runGit(ctx, c.path, "rev-parse", "--verify", "HEAD"); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			reason := c.sanitize(err.Error())
			result.RecordFailure(entityID, "identity_domain_consistency", sourceName, reason, true, now)
			result.RecordFailure(entityID, "effective_maintainer_concentration", sourceName, reason, true, now)
			return
		}
		reason := "repo has no commits on HEAD"
		result.RecordAbsence(entityID, "identity_domain_consistency", sourceName, reason, false, now)
		result.RecordAbsence(entityID, "effective_maintainer_concentration", sourceName, reason, false, now)
		return
	}

	since := fmt.Sprintf("--since=%s", now.Add(-c.window).Format(time.RFC3339))
	maxCount := fmt.Sprintf("-n%d", c.commitCap)

	out, err := runGit(ctx, c.path, "log", since, authorshipFormat, maxCount, "HEAD")
	if err != nil {
		retryable := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		reason := c.sanitize(err.Error())
		result.RecordFailure(entityID, "identity_domain_consistency", sourceName, reason, retryable, now)
		result.RecordFailure(entityID, "effective_maintainer_concentration", sourceName, reason, retryable, now)
		return
	}

	rows := parseAuthorshipLog(out)
	if len(rows) == 0 {
		reason := fmt.Sprintf("no commits within the %s window", c.window)
		result.RecordAbsence(entityID, "identity_domain_consistency", sourceName, reason, false, now)
		result.RecordAbsence(entityID, "effective_maintainer_concentration", sourceName, reason, false, now)
		return
	}

	c.emitDomainConsistency(result, entityID, rows, now, ttl)
	c.emitMaintainerConcentration(result, entityID, rows, now, ttl)
}

// emitDomainConsistency tallies email domains from the batched
// log pass and records identity_domain_consistency.
func (c *Collector) emitDomainConsistency(
	result *signal.CollectionResult,
	entityID string,
	rows []authorshipRow,
	now time.Time,
	ttl time.Duration,
) {
	domainCounts := map[string]int{}
	for _, r := range rows {
		d := extractEmailDomain(r.Email)
		if d == "" {
			d = "unknown"
		}
		domainCounts[d]++
	}

	top := topNFromMap(domainCounts, topDomainsLimit)
	topShare := 0.0
	if len(top) > 0 && len(rows) > 0 {
		topShare = float64(top[0].Count) / float64(len(rows))
	}
	topList := make([]map[string]any, 0, len(top))
	for _, e := range top {
		topList = append(topList, map[string]any{
			"domain": e.Key,
			"count":  e.Count,
		})
	}

	result.RecordSignal(entityID, "identity_domain_consistency", sourceName, now, ttl,
		map[string]any{
			"total_commits":    len(rows),
			"unique_domains":   len(domainCounts),
			"top_domains":      topList,
			"top_domain_share": topShare,
			"window":           c.window.String(),
		})
}

// emitMaintainerConcentration tallies canonical authors
// ("Name <email>") from the batched log pass and records
// effective_maintainer_concentration with top-N and top-K share.
func (c *Collector) emitMaintainerConcentration(
	result *signal.CollectionResult,
	entityID string,
	rows []authorshipRow,
	now time.Time,
	ttl time.Duration,
) {
	authorCounts := map[string]int{}
	for _, r := range rows {
		key := fmt.Sprintf("%s <%s>", r.Name, r.Email)
		authorCounts[key]++
	}

	top := topNFromMap(authorCounts, topAuthorsLimit)
	topAuthorShare := 0.0
	if len(top) > 0 && len(rows) > 0 {
		topAuthorShare = float64(top[0].Count) / float64(len(rows))
	}

	// Top-K share (K = topAuthorsShareCutoff). Sum commit counts
	// from the first K entries; bounded by the total top count.
	topKCount := 0
	for i, e := range top {
		if i >= topAuthorsShareCutoff {
			break
		}
		topKCount += e.Count
	}
	topKShare := 0.0
	if len(rows) > 0 {
		topKShare = float64(topKCount) / float64(len(rows))
	}

	topList := make([]map[string]any, 0, len(top))
	for _, e := range top {
		topList = append(topList, map[string]any{
			"author": e.Key,
			"count":  e.Count,
		})
	}

	result.RecordSignal(entityID, "effective_maintainer_concentration", sourceName, now, ttl,
		map[string]any{
			"total_commits":    len(rows),
			"unique_authors":   len(authorCounts),
			"top_authors":      topList,
			"top_author_share": topAuthorShare,
			"top_k":            topAuthorsShareCutoff,
			"top_k_share":      topKShare,
			"window":           c.window.String(),
		})
}

// topNEntry is a (key, count) pair in a sorted top-N slice.
type topNEntry struct {
	Key   string
	Count int
}

// topNFromMap sorts a map by descending count (ties broken by
// ascending key for deterministic output) and returns the first
// n entries. Used for both domain and author tallies; the pattern
// is small enough that one helper serves both without polymorphism
// machinery.
func topNFromMap(m map[string]int, n int) []topNEntry {
	if len(m) == 0 {
		return nil
	}
	all := make([]topNEntry, 0, len(m))
	for k, v := range m {
		all = append(all, topNEntry{Key: k, Count: v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Count != all[j].Count {
			return all[i].Count > all[j].Count
		}
		return all[i].Key < all[j].Key
	})
	if n > 0 && len(all) > n {
		all = all[:n]
	}
	return all
}
