package github

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// Collector gathers trust signals from the GitHub API.
type Collector struct {
	client *Client
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

// Name returns the collector identifier.
func (c *Collector) Name() string { return "github" }

// Collect gathers signals for a GitHub-hosted entity. Each signal group
// is collected independently — a failure in one (e.g., rate limiting on
// the search API) does not prevent other signals from being collected.
// Failed collections are recorded as absence signals.
//
// Signal Group and ForgeryResistance come from the signal type registry
// (internal/signal/types.go) rather than being hardcoded at each call
// site. If a type is unregistered, RecordSignal panics — that catches
// collector additions that forgot to extend the registry.
func (c *Collector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
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
		map[string]interface{}{"date": r.PushedAt.Format(time.RFC3339), "era": string(profile.ClassifyEra(r.PushedAt))})
	result.RecordSignal(entity.ID, "repo_age", "github", now, ttl,
		map[string]interface{}{"created": r.CreatedAt.Format(time.RFC3339), "age_days": int(now.Sub(r.CreatedAt).Hours() / 24)})
	result.RecordSignal(entity.ID, "stars", "github", now, ttl,
		map[string]interface{}{"count": r.StargazersCount})
	result.RecordSignal(entity.ID, "forks", "github", now, ttl,
		map[string]interface{}{"count": r.ForksCount})
	result.RecordSignal(entity.ID, "open_issues", "github", now, ttl,
		map[string]interface{}{"count": r.OpenIssuesCount})
	result.RecordSignal(entity.ID, "archived", "github", now, ttl,
		map[string]interface{}{"archived": r.Archived})
	result.RecordSignal(entity.ID, "owner_type", "github", now, ttl,
		map[string]interface{}{"type": r.Owner.Type, "login": r.Owner.Login})

	if r.License != nil {
		result.RecordSignal(entity.ID, "license", "github", now, ttl,
			map[string]interface{}{"spdx_id": r.License.SPDXID})
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

	// Adoption (go.mod refs) — independent, uses search API with its own rate limit.
	c.collectAdoption(ctx, &result, entity.ID, owner, repoName, r.StargazersCount, now, ttl)

	// CI/CD presence — independent.
	c.collectCI(ctx, &result, entity.ID, owner, repoName, now, ttl)

	// Go dependencies — independent.
	c.collectGoDeps(ctx, &result, entity.ID, owner, repoName, now, ttl)

	return &result, nil
}

func (c *Collector) collectContributors(ctx context.Context, result *signal.CollectionResult,
	entityID, owner, repoName string, now time.Time, ttl time.Duration) {

	contributors, err := c.client.GetContributors(ctx, owner, repoName)
	if err != nil {
		result.RecordFailure(entityID, "contributors", "github", sanitizeErrorForStorage(err), isRetryable(err), now)
		return
	}

	topContribs := make([]map[string]interface{}, 0, len(contributors))
	for _, contrib := range contributors {
		topContribs = append(topContribs, map[string]interface{}{
			"login":         contrib.Login,
			"contributions": contrib.Contributions,
		})
	}
	result.RecordSignal(entityID, "contributors", "github", now, ttl,
		map[string]interface{}{"count": len(contributors), "top": topContribs})
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
		map[string]interface{}{
			"date":     commits[0].Commit.Author.Date.Format(time.RFC3339),
			"era":      string(profile.ClassifyEra(commits[0].Commit.Author.Date)),
			"days_ago": int(now.Sub(commits[0].Commit.Author.Date).Hours() / 24),
		})
	result.RecordSignal(entityID, "commit_signing", "github", now, ttl,
		map[string]interface{}{
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
		map[string]interface{}{"count": totalCommits})
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
		map[string]interface{}{"count": len(tags), "recent": tagNames})
}

func (c *Collector) collectOwnerProfile(ctx context.Context, result *signal.CollectionResult,
	entityID, owner string, now time.Time, ttl time.Duration) {

	ownerUser, err := c.client.GetUser(ctx, owner)
	if err != nil {
		result.RecordFailure(entityID, "owner_profile", "github", sanitizeErrorForStorage(err), isRetryable(err), now)
		return
	}

	result.RecordSignal(entityID, "owner_profile", "github", now, ttl,
		map[string]interface{}{
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

func (c *Collector) collectAdoption(ctx context.Context, result *signal.CollectionResult,
	entityID, owner, repoName string, stars int, now time.Time, ttl time.Duration) {

	refCount, err := c.client.GetGoModRefCount(ctx, "github.com/"+owner+"/"+repoName)
	if err != nil {
		result.RecordFailure(entityID, "adoption", "github", sanitizeErrorForStorage(err), isRetryable(err), now)
		return
	}

	ratio := float64(0)
	if stars > 0 {
		ratio = float64(refCount) / float64(stars)
	}
	adoptionType := "direct"
	if ratio > 10 {
		adoptionType = "mostly-transitive"
	} else if ratio > 1 {
		adoptionType = "mixed"
	}
	result.RecordSignal(entityID, "adoption", "github", now, ttl,
		map[string]interface{}{
			"go_mod_refs":   refCount,
			"stars":         stars,
			"refs_to_stars": ratio,
			"adoption_type": adoptionType,
		})
}

func (c *Collector) collectCI(ctx context.Context, result *signal.CollectionResult,
	entityID, owner, repoName string, now time.Time, ttl time.Duration) {

	providers, hadErrors := c.collectCIPresence(ctx, owner, repoName)
	switch {
	case len(providers) > 0:
		result.RecordSignal(entityID, "ci_cd", "github", now, ttl,
			map[string]interface{}{"providers": providers})
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
		map[string]interface{}{
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
// For signatory-owned sentinels (ErrNotFound, RateLimitError) we use
// errors.Is / errors.As so wrapping doesn't break the classification.
// For stdlib transport errors (context deadline, "no such host") we
// still string-match — those errors have no exported sentinel and
// their text is stable across Go versions by policy.
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
	errMsg := err.Error()
	if strings.Contains(errMsg, "context deadline exceeded") ||
		strings.Contains(errMsg, "Client.Timeout") {
		return "request timeout"
	}
	if strings.Contains(errMsg, "context canceled") {
		return "request cancelled"
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
	if strings.Contains(errMsg, "GitHub API returned status 5") {
		return "server error"
	}
	if strings.Contains(errMsg, "GitHub API returned status 4") {
		return "client error"
	}
	if strings.Contains(errMsg, "decode response") {
		return "invalid response"
	}
	return "collection failed"
}

// isRetryable determines if an error is likely to succeed on retry.
// Rate limits, timeouts, and transient server errors are retryable.
// 404s and parse errors are not.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	// Rate limit errors (using errors.As to handle wrapped errors).
	var rateLimitErr *RateLimitError
	if errors.As(err, &rateLimitErr) {
		return true
	}
	errMsg := err.Error()
	// Timeouts and connection errors.
	if strings.Contains(errMsg, "context deadline exceeded") ||
		strings.Contains(errMsg, "Client.Timeout") ||
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
		if strings.HasPrefix(line, "require ") && !strings.HasPrefix(line, "require (") {
			dep := strings.TrimPrefix(line, "require ")
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
	return deps, nil
}

