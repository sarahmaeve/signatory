package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// isGitHubHost reports whether rawURL points at github.com (with or
// without scheme; with or without www. prefix). Cheap host check
// sitting in front of ParseRepoURL so the permissive parser can't
// be tricked into emitting non-github owner/repo pairs from
// codeberg/gitlab/bitbucket-style URLs.
//
// Mirrors the helper of the same name in internal/signal/openssf.
// The two collectors apply the same gate at the same call site
// (Collect's first check); duplicating the helper keeps each
// collector's package self-contained without forcing an
// internal/signal/forge cross-cut for a 15-line predicate.
func isGitHubHost(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	host := u.Host
	if host == "" {
		// Bare "github.com/owner/repo" path-only form.
		host, _, _ = strings.Cut(u.Path, "/")
	}
	host = strings.TrimPrefix(host, "www.")
	return host == "github.com"
}

// EntityStore is the narrow interface the github collector uses to
// mint identity:/org: entity rows for the owners of repos it scans.
// Defined here (consumer-side) so the collector doesn't depend on
// the full internal/store package — any type that implements
// EnsureEntityByCanonicalURI satisfies it via structural typing.
//
// Optional: the field on Collector is nil-safe. Tests that don't
// care about owner-entity emission construct collectors without
// calling WithEntityStore, and the entity-minting branch in
// collectOwnerProfile silently skips when c.entityStore is nil.
//
// In production, cmd/signatory/collectors.go threads the
// orchestrator's *store.SQLite through opts and calls
// WithEntityStore so every analyze run populates owner entities
// for github-hosted targets (Path A; design/entity-burn1.md).
type EntityStore interface {
	EnsureEntityByCanonicalURI(ctx context.Context, uri, shortName string) (*profile.Entity, bool, error)
}

// Collector gathers trust signals from the GitHub API.
type Collector struct {
	client      *Client
	entityStore EntityStore // optional — see EntityStore docstring
}

// NewCollector creates a GitHub signal collector. It reads GITHUB_TOKEN
// from the environment for authenticated API access (5000 req/hr vs. 60).
func NewCollector() *Collector {
	token := os.Getenv("GITHUB_TOKEN")
	return &Collector{client: NewClient(token)}
}

// NewCollectorWithClient creates a collector with a provided client (for testing).
func NewCollectorWithClient(client *Client) *Collector {
	return &Collector{client: client}
}

// WithEntityStore wires an EntityStore into the collector so
// owner-entity minting fires during each Collect run. Returns the
// receiver so the call chains cleanly with the constructors.
//
// Setter rather than constructor parameter to keep backwards-compat
// with the existing constructor surface (NewCollector and
// NewCollectorWithClient are widely used and stay nullary in their
// store-related parameter; tests that don't need entity emission
// continue to compile unchanged).
func (c *Collector) WithEntityStore(s EntityStore) *Collector {
	c.entityStore = s
	return c
}

// Name returns the collector identifier.
func (c *Collector) Name() string { return "github" }

// Collect gathers signals for a GitHub-hosted entity. Each signal group
// is collected independently — a failure in one (e.g., rate limiting on
// the search API) does not prevent other signals from being collected.
// Failed collections are recorded as absence signals.
//
// Self-gate: entities whose URL doesn't resolve to github.com receive
// a non-nil empty CollectionResult with nil error. The orchestrator
// (cmd/signatory/collectors.go) wires this collector unconditionally
// for every git-hosted target — including codeberg/gitlab — so the
// gate must live here. Symmetric with openssf's extractOwnerRepo
// pattern and gopublish's non-Go-entity branch. Without the gate,
// a codeberg URL parses through ParseRepoURL as owner="codeberg.org",
// name="forgejo" and fires a 404 against api.github.com that fails
// the entire analyze run.
//
// Signal Group and ForgeryResistance come from the signal type registry
// (internal/signal/types.go) rather than being hardcoded at each call
// site. If a type is unregistered, RecordSignal panics — that catches
// collector additions that forgot to extend the registry.
func (c *Collector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	// Self-gate: a non-empty URL whose host doesn't resolve to
	// github.com is a non-GitHub entity (codeberg, gitlab, future
	// forges). Return empty before any HTTP. Empty URL is NOT gated
	// here — it falls through to ShortName fallback, preserving the
	// legacy bare-shorthand path that TestCollector_NotFoundError
	// and other ShortName-only callers depend on.
	if entity.URL != "" && !isGitHubHost(entity.URL) {
		return &signal.CollectionResult{}, nil
	}

	target := entity.URL
	if target == "" {
		target = entity.ShortName
	}

	owner, repoName, err := ParseRepoURL(target)
	if err != nil {
		return nil, fmt.Errorf("github collector: %w", err)
	}

	now := time.Now().UTC()
	ttl := 24 * time.Hour
	var result signal.CollectionResult

	// Repo metadata — required. If this fails, we can't proceed.
	r, err := c.client.GetRepo(ctx, owner, repoName)
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}

	result.RecordSignal(entity.ID, "last_push", "github", now, ttl,
		map[string]any{"date": r.PushedAt.Format(time.RFC3339), "era": string(profile.ClassifyEra(r.PushedAt))})
	result.RecordSignal(entity.ID, "repo_age", "github", now, ttl,
		map[string]any{"created": r.CreatedAt.Format(time.RFC3339), "age_days": int(now.Sub(r.CreatedAt).Hours() / 24)})
	result.RecordSignal(entity.ID, "stars", "github", now, ttl,
		map[string]any{"count": r.StargazersCount})
	result.RecordSignal(entity.ID, "forks", "github", now, ttl,
		map[string]any{"count": r.ForksCount})
	result.RecordSignal(entity.ID, "open_issues", "github", now, ttl,
		map[string]any{"count": r.OpenIssuesCount})
	result.RecordSignal(entity.ID, "archived", "github", now, ttl,
		map[string]any{"archived": r.Archived})
	result.RecordSignal(entity.ID, "owner_type", "github", now, ttl,
		map[string]any{"type": r.Owner.Type, "login": r.Owner.Login})

	if r.License != nil {
		result.RecordSignal(entity.ID, "license", "github", now, ttl,
			map[string]any{"spdx_id": r.License.SPDXID})
	} else {
		result.RecordAbsence(entity.ID, "license", "github",
			"no license detected", false, now)
	}

	// Contributors — independent, failures recorded as absence.
	c.collectContributors(ctx, &result, entity.ID, owner, repoName, now, ttl)

	// Recent commits — independent.
	c.collectCommits(ctx, &result, entity.ID, owner, repoName, now, ttl)

	// Total commit count — independent.
	c.collectTotalCommits(ctx, &result, entity.ID, owner, repoName, now, ttl)

	// Tags — independent.
	c.collectTags(ctx, &result, entity.ID, owner, repoName, now, ttl)

	// Owner profile — independent.
	c.collectOwnerProfile(ctx, &result, entity.ID, owner, now, ttl)

	// adoption (inbound go.mod refs) moved to internal/signal/adoption
	// in commit "adoption: lift to standalone collector" — that
	// collector dispatches independently in collectorsFor and handles
	// github / codeberg / gitlab modules from one code path. The
	// stars-aware ratio is preserved across the migration via the
	// inRunResult bridge; the adoption collector reads this github
	// collector's "stars" emission from the orchestrator's accumulator.

	// CI/CD presence — independent.
	c.collectCI(ctx, &result, entity.ID, owner, repoName, now, ttl)

	// Go dependencies — only applicable when the ecosystem is Go or
	// unknown (bare repo targets where ecosystem isn't pre-classified).
	// A "no go.mod found" absence on a PyPI or npm package is
	// nonsensical noise that makes the scan appear suspect.
	if entity.Ecosystem == "" || entity.Ecosystem == "golang" || entity.Ecosystem == "go" {
		c.collectGoDeps(ctx, &result, entity.ID, owner, repoName, now, ttl)
	}

	return &result, nil
}

func (c *Collector) collectContributors(ctx context.Context, result *signal.CollectionResult,
	entityID, owner, repoName string, now time.Time, ttl time.Duration) {

	contributors, err := c.client.GetContributors(ctx, owner, repoName)
	if err != nil {
		result.RecordFailure(entityID, "contributors", "github", sanitizeErrorForStorage(err), isRetryable(err), now)
		return
	}

	topContribs := make([]map[string]any, 0, len(contributors))
	for _, contrib := range contributors {
		topContribs = append(topContribs, map[string]any{
			"login":         contrib.Login,
			"contributions": contrib.Contributions,
		})
	}
	result.RecordSignal(entityID, "contributors", "github", now, ttl,
		map[string]any{"count": len(contributors), "top": topContribs})
}

func (c *Collector) collectCommits(ctx context.Context, result *signal.CollectionResult,
	entityID, owner, repoName string, now time.Time, ttl time.Duration) {

	commits, err := c.client.GetRecentCommits(ctx, owner, repoName)
	if err != nil {
		// One upstream error produces two absences (both last_commit
		// and commit_signing come from the same API call). The single
		// CollectionError is keyed to "commits" to reflect the real
		// source of the failure, not either individual signal.
		reason := sanitizeErrorForStorage(err)
		retryable := isRetryable(err)
		result.RecordAbsence(entityID, "last_commit", "github", reason, retryable, now)
		result.RecordAbsence(entityID, "commit_signing", "github", reason, retryable, now)
		result.Failures = append(result.Failures, signal.CollectionError{
			SignalType: "commits", Source: "github", Reason: reason, Retryable: retryable,
		})
		return
	}

	if len(commits) == 0 {
		result.RecordAbsence(entityID, "last_commit", "github", "no commits found", false, now)
		result.RecordAbsence(entityID, "commit_signing", "github", "no commits found", false, now)
		return
	}

	signedCount := 0
	for _, cm := range commits {
		if cm.Commit.Verification.Verified {
			signedCount++
		}
	}

	result.RecordSignal(entityID, "last_commit", "github", now, ttl,
		map[string]any{
			"date":     commits[0].Commit.Author.Date.Format(time.RFC3339),
			"era":      string(profile.ClassifyEra(commits[0].Commit.Author.Date)),
			"days_ago": int(now.Sub(commits[0].Commit.Author.Date).Hours() / 24),
		})
	result.RecordSignal(entityID, "commit_signing", "github", now, ttl,
		map[string]any{
			"signed_count": signedCount,
			"total_count":  len(commits),
			"ratio":        float64(signedCount) / float64(len(commits)),
		})
}

func (c *Collector) collectTotalCommits(ctx context.Context, result *signal.CollectionResult,
	entityID, owner, repoName string, now time.Time, ttl time.Duration) {

	totalCommits, err := c.client.GetTotalCommitCount(ctx, owner, repoName)
	if err != nil {
		result.RecordFailure(entityID, "total_commits", "github", sanitizeErrorForStorage(err), isRetryable(err), now)
		return
	}

	result.RecordSignal(entityID, "total_commits", "github", now, ttl,
		map[string]any{"count": totalCommits})
}

func (c *Collector) collectTags(ctx context.Context, result *signal.CollectionResult,
	entityID, owner, repoName string, now time.Time, ttl time.Duration) {

	tags, err := c.client.GetTags(ctx, owner, repoName)
	if err != nil {
		result.RecordFailure(entityID, "tags", "github", sanitizeErrorForStorage(err), isRetryable(err), now)
		return
	}

	tagNames := make([]string, 0, len(tags))
	for _, t := range tags {
		tagNames = append(tagNames, t.Name)
	}
	result.RecordSignal(entityID, "tags", "github", now, ttl,
		map[string]any{"count": len(tags), "recent": tagNames})
}

func (c *Collector) collectOwnerProfile(ctx context.Context, result *signal.CollectionResult,
	entityID, owner string, now time.Time, ttl time.Duration) {

	ownerUser, err := c.client.GetUser(ctx, owner)
	if err != nil {
		result.RecordFailure(entityID, "owner_profile", "github", sanitizeErrorForStorage(err), isRetryable(err), now)
		return
	}

	// Mint or refresh the owner-entity row alongside the
	// owner_profile signal. The signal carries the metadata
	// (account_age_days, followers, ...) that downstream synthesis
	// reads; the entity row is what `signatory burn add identity:
	// github/<login>` attaches to and what future cascade resolvers
	// will walk through. Path A; design/entity-burn1.md §3.
	//
	// Only fires when an EntityStore was wired via WithEntityStore.
	// Tests and callers that pre-date Path A construct collectors
	// without a store and silently skip this branch.
	//
	// Failure is non-fatal: a transient store error is recorded as
	// a stderr warning (TODO: structured failure surface) but the
	// owner_profile signal still gets emitted. Worst case the entity
	// row gets minted on the next run; the signal carries the data
	// regardless.
	if c.entityStore != nil {
		var ownerURI string
		if ownerUser.Type == "Organization" {
			ownerURI = profile.CanonicalOrgURI("github", ownerUser.Login)
		} else {
			// "User" is the dominant case; any unrecognized Type
			// (rare — GitHub's API returns one of two values)
			// defaults to identity:, matching the semantics that
			// individual humans get identity URIs.
			ownerURI = profile.CanonicalIdentityURI("github", ownerUser.Login)
		}
		if _, _, err := c.entityStore.EnsureEntityByCanonicalURI(ctx, ownerURI, ownerUser.Login); err != nil {
			// Don't fail the owner_profile signal on a store error —
			// the signal is independent of the entity row, and a
			// failure here doesn't change what we observed about
			// the owner. Re-running analyze re-attempts the mint.
			// Log to stderr so an operator notices systematic store
			// failures rather than silently missing entity rows.
			fmt.Fprintf(os.Stderr, "warning: failed to ensure owner entity %s: %v\n", ownerURI, err)
		}
	}

	result.RecordSignal(entityID, "owner_profile", "github", now, ttl,
		map[string]any{
			"login":            ownerUser.Login,
			"name":             ownerUser.Name,
			"company":          ownerUser.Company,
			"created":          ownerUser.CreatedAt.Format(time.RFC3339),
			"account_age_days": int(now.Sub(ownerUser.CreatedAt).Hours() / 24),
			"public_repos":     ownerUser.PublicRepos,
			"followers":        ownerUser.Followers,
			"type":             ownerUser.Type,
		})
}

func (c *Collector) collectCI(ctx context.Context, result *signal.CollectionResult,
	entityID, owner, repoName string, now time.Time, ttl time.Duration) {

	providers, hadErrors := c.collectCIPresence(ctx, owner, repoName)
	switch {
	case len(providers) > 0:
		result.RecordSignal(entityID, "ci_cd", "github", now, ttl,
			map[string]any{"providers": providers})
	case hadErrors:
		// We couldn't check — retryable absence, not a definitive "no CI".
		result.RecordFailure(entityID, "ci_cd", "github",
			"could not check CI/CD configuration", true, now)
	default:
		// We checked everything and found nothing — definitive negative.
		result.RecordAbsence(entityID, "ci_cd", "github",
			"no CI/CD configuration detected", false, now)
	}
}

func (c *Collector) collectGoDeps(ctx context.Context, result *signal.CollectionResult,
	entityID, owner, repoName string, now time.Time, ttl time.Duration) {

	goModContent, err := c.client.GetFileRaw(ctx, owner, repoName, "go.mod")
	if err != nil {
		result.RecordFailure(entityID, "go_dependencies", "github", sanitizeErrorForStorage(err), isRetryable(err), now)
		return
	}
	if goModContent == nil {
		// Not a Go project — record absence but not as a failure.
		result.RecordAbsence(entityID, "go_dependencies", "github",
			"no go.mod found (not a Go project)", false, now)
		return
	}

	deps, err := parseGoModDeps(string(goModContent))
	if err != nil {
		// Oversized go.mod (#108). Record as a failure — not retryable
		// because the file won't shrink on retry. Sanitize the parser
		// error so the byte counts don't leak the parsed-but-rejected
		// content size to attackers (defense in depth).
		result.RecordFailure(entityID, "go_dependencies", "github",
			"go.mod too large to parse safely", false, now)
		return
	}
	result.RecordSignal(entityID, "go_dependencies", "github", now, ttl,
		map[string]any{
			"direct_count":   deps.directCount,
			"indirect_count": deps.indirectCount,
			"total_count":    deps.directCount + deps.indirectCount,
			"direct":         deps.direct,
		})
}

// collectCIPresence checks for common CI/CD configuration.
// Returns the list of detected providers and whether any errors occurred
// (so the caller can distinguish "found nothing" from "couldn't check").
func (c *Collector) collectCIPresence(ctx context.Context, owner, repoName string) (providers []string, hadErrors bool) {
	// GitHub Actions.
	workflows, err := c.client.GetDirectoryContents(ctx, owner, repoName, ".github/workflows")
	if err != nil {
		hadErrors = true
	} else if len(workflows) > 0 {
		providers = append(providers, "github-actions")
	}

	// Check for other CI config files.
	ciFiles := []struct {
		path     string
		provider string
	}{
		{".travis.yml", "travis-ci"},
		{".circleci/config.yml", "circleci"},
		{".gitlab-ci.yml", "gitlab-ci"},
		{"Jenkinsfile", "jenkins"},
		{".github/dependabot.yml", "dependabot"},
		{"renovate.json", "renovate"},
	}

	for _, cf := range ciFiles {
		content, err := c.client.GetFileRaw(ctx, owner, repoName, cf.path)
		if err != nil {
			hadErrors = true
		} else if content != nil {
			providers = append(providers, cf.provider)
		}
	}

	return providers, hadErrors
}

// sanitizeErrorForStorage returns a safe error description for persistence.
// Raw error messages may contain tokens, URLs with credentials, or other
// sensitive data from HTTP responses. This function classifies the error
// and returns only the category, never the raw content.
//
// For signatory-owned sentinels (ErrNotFound, RateLimitError) and the
// stdlib context sentinels (context.DeadlineExceeded, context.Canceled)
// we use errors.Is / errors.As so any wrapping layer — fmt.Errorf %w,
// custom error types with their own Error() messages, *url.Error from
// net/http — still unwraps cleanly to the underlying classification.
// String matching on err.Error() (the previous approach for the
// context sentinels) silently dropped any wrapper whose surface
// message didn't include the sentinel's standard text.
//
// For stdlib net errors with no exported sentinel ("connection
// refused", "no such host") we still string-match — those messages
// are stable across Go versions by policy. Client.Timeout is also
// kept as a string-match fallback: modern net/http unwraps to
// context.DeadlineExceeded, but the literal "Client.Timeout" surface
// is preserved as belt-and-suspenders for any *url.Error path that
// surfaces the timeout without a sentinel chain.
func sanitizeErrorForStorage(err error) string {
	if err == nil {
		return ""
	}
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return "rate limited"
	}
	if errors.Is(err, ErrNotFound) {
		return "not found"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "request timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "request cancelled"
	}
	errMsg := err.Error()
	if strings.Contains(errMsg, "Client.Timeout") {
		return "request timeout"
	}
	if strings.Contains(errMsg, "connection refused") ||
		strings.Contains(errMsg, "no such host") {
		return "connection failed"
	}
	// Match the format produced by client.get / client.getWithLinkHeader
	// after #93's body-removal: "GitHub API returned status NNN".
	// (The previous format "GitHub API error N: <body>" was changed in
	// #93 to drop the attacker-influenceable body; these classifier
	// branches were not updated at that time and silently became dead
	// code, with all 4xx/5xx falling through to "collection failed".)
	//
	// Surface the numeric status code so a manual operator can
	// distinguish 401 (auth) from 403 (perms) from 404 (gone) from 422
	// (validation) from 500/502/503 (transient infrastructure). The
	// status code is the only attacker-uninfluenced field in the
	// upstream error after #93's sanitization; the body is already
	// gone by the time we reach this classifier.
	if code := extractGitHubAPIStatusCode(errMsg); code != "" {
		switch {
		case code == "401":
			return "GitHub API 401 — set GITHUB_TOKEN to authenticate"
		case code[0] == '5':
			return "GitHub API " + code + " (server error)"
		case code[0] == '4':
			return "GitHub API " + code
		}
	}
	if strings.Contains(errMsg, "decode response") {
		return "invalid response"
	}
	return "collection failed"
}

// extractGitHubAPIStatusCode pulls the 3-digit status code out of an
// errMsg that follows the "GitHub API returned status NNN" sanitized
// format. Returns "" when the prefix isn't present or no digits follow.
//
// Stops at 3 digits — defensive against a future format change that
// might append additional content; we only want the numeric status,
// not whatever came after it.
func extractGitHubAPIStatusCode(errMsg string) string {
	_, rest, ok := strings.Cut(errMsg, "GitHub API returned status ")
	if !ok {
		return ""
	}
	end := 0
	for end < 3 && end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	return rest[:end]
}

// isRetryable determines if an error is likely to succeed on retry.
// Rate limits, timeouts, and transient server errors are retryable.
// 404s, parse errors, and deliberately-cancelled requests are not.
//
// context.DeadlineExceeded routes through errors.Is so wrapping
// layers (fmt.Errorf %w, *url.Error, custom error types) don't
// silently break the classification. context.Canceled is explicitly
// NOT retryable: a cancelled request was cancelled by the caller,
// and retrying would fight the cancellation intent. Stdlib net
// errors with no exported sentinel ("connection refused", "no such
// host") and net/http's "Client.Timeout" surface keep their string
// matching — those are stable across Go versions by policy and have
// no errors.Is hook.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	// Rate limit errors (using errors.As to handle wrapped errors).
	var rateLimitErr *RateLimitError
	if errors.As(err, &rateLimitErr) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	errMsg := err.Error()
	// Connection errors and Client.Timeout (no stdlib sentinel).
	if strings.Contains(errMsg, "Client.Timeout") ||
		strings.Contains(errMsg, "connection refused") ||
		strings.Contains(errMsg, "no such host") {
		return true
	}
	// Server errors (5xx). Format after #93: "GitHub API returned status NNN".
	if strings.Contains(errMsg, "GitHub API returned status 5") {
		return true
	}
	return false
}

// goModDeps holds parsed dependency information from a go.mod file.
type goModDeps struct {
	directCount   int
	indirectCount int
	direct        []string
}

// maxGoModSize bounds the input to parseGoModDeps to prevent memory
// pressure / GC churn from a malicious or compromised upstream returning
// a giant go.mod (e.g., 10MB of newlines, the upstream maxResponseSize
// limit). Real-world go.mod files are well under 64KB — even Kubernetes,
// one of the largest Go projects, has a go.mod under 50KB. 64KB is
// generous slack with a hard cap that prevents the DoS class. Issue #108.
const maxGoModSize = 64 * 1024

// parseGoModDeps extracts dependency information from go.mod content.
// Returns an error if the content exceeds maxGoModSize — callers should
// treat that as an absence signal (not retryable; the file won't shrink).
func parseGoModDeps(content string) (goModDeps, error) {
	var deps goModDeps
	if len(content) > maxGoModSize {
		return deps, fmt.Errorf("go.mod content exceeds maximum parseable size of %d bytes (got %d)",
			maxGoModSize, len(content))
	}
	lines := strings.Split(content, "\n")
	inRequire := false

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if line == "require (" {
			inRequire = true
			continue
		}
		if inRequire && line == ")" {
			inRequire = false
			continue
		}

		// Single-line require: "require github.com/pkg/foo v1.0.0 // indirect"
		if dep, ok := strings.CutPrefix(line, "require "); ok && !strings.HasPrefix(dep, "(") {
			if strings.Contains(dep, "// indirect") {
				deps.indirectCount++
			} else {
				deps.directCount++
				parts := strings.Fields(dep)
				if len(parts) >= 1 {
					deps.direct = append(deps.direct, parts[0])
				}
			}
			continue
		}

		if !inRequire {
			continue
		}

		if strings.HasPrefix(line, "//") || line == "" {
			continue
		}

		if strings.Contains(line, "// indirect") {
			deps.indirectCount++
		} else {
			deps.directCount++
			parts := strings.Fields(line)
			if len(parts) >= 1 {
				deps.direct = append(deps.direct, parts[0])
			}
		}
	}

	// Sort + de-duplicate the direct list and reconcile directCount to
	// the de-duplicated set. Parity with the registry collectors
	// (npm/cargo/maven/gem/pypi all slices.Sort + slices.Compact):
	// canonical order makes two observations of an unchanged go.mod
	// diff clean regardless of require-block ordering, and dedup keeps
	// a hand-edited duplicate require from inflating the count. A valid
	// go.mod has neither, but parseGoModDeps is defensive against
	// adversarial or hand-mangled upstream content.
	slices.Sort(deps.direct)
	deps.direct = slices.Compact(deps.direct)
	deps.directCount = len(deps.direct)

	return deps, nil
}
