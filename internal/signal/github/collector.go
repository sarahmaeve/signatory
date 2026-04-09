package github

import (
	"context"
	"encoding/json"
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
func (c *Collector) Collect(ctx context.Context, entity *profile.Entity) ([]profile.Signal, error) {
	target := entity.URL
	if target == "" {
		target = entity.Name
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

	result.Collected = append(result.Collected,
		signal.MakeSignal(makeSignal(entity.ID, "last_push", profile.SignalGroupVitality,
			profile.ForgeryMediumDeclining, now, ttl,
			map[string]interface{}{"date": r.PushedAt.Format(time.RFC3339), "era": string(profile.ClassifyEra(r.PushedAt))})),
		signal.MakeSignal(makeSignal(entity.ID, "repo_age", profile.SignalGroupVitality,
			profile.ForgeryVeryHigh, now, ttl,
			map[string]interface{}{"created": r.CreatedAt.Format(time.RFC3339), "age_days": int(now.Sub(r.CreatedAt).Hours() / 24)})),
		signal.MakeSignal(makeSignal(entity.ID, "stars", profile.SignalGroupCriticality,
			profile.ForgeryMediumDeclining, now, ttl,
			map[string]interface{}{"count": r.StargazersCount})),
		signal.MakeSignal(makeSignal(entity.ID, "forks", profile.SignalGroupCriticality,
			profile.ForgeryMediumDeclining, now, ttl,
			map[string]interface{}{"count": r.ForksCount})),
		signal.MakeSignal(makeSignal(entity.ID, "open_issues", profile.SignalGroupVitality,
			profile.ForgeryMediumDeclining, now, ttl,
			map[string]interface{}{"count": r.OpenIssuesCount})),
		signal.MakeSignal(makeSignal(entity.ID, "archived", profile.SignalGroupVitality,
			profile.ForgeryHigh, now, ttl,
			map[string]interface{}{"archived": r.Archived})),
		signal.MakeSignal(makeSignal(entity.ID, "owner_type", profile.SignalGroupGovernance,
			profile.ForgeryHigh, now, ttl,
			map[string]interface{}{"type": r.Owner.Type, "login": r.Owner.Login})),
	)

	if r.License != nil {
		result.Collected = append(result.Collected,
			signal.MakeSignal(makeSignal(entity.ID, "license", profile.SignalGroupHygiene,
				profile.ForgeryLowDeclining, now, ttl,
				map[string]interface{}{"spdx_id": r.License.SPDXID})))
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

	// Convert results to signals (including absence records).
	signals := make([]profile.Signal, 0, len(result.Collected))
	for _, s := range result.Collected {
		signals = append(signals, s.ToSignal())
	}

	return signals, nil
}

func (c *Collector) collectContributors(ctx context.Context, result *signal.CollectionResult,
	entityID, owner, repoName string, now time.Time, ttl time.Duration) {

	contributors, err := c.client.GetContributors(ctx, owner, repoName)
	if err != nil {
		result.Collected = append(result.Collected,
			signal.MakeAbsence(entityID, "contributors", "github", sanitizeErrorForStorage(err), isRetryable(err), now))
		result.Failures = append(result.Failures,
			signal.CollectionFailure{SignalType: "contributors", Source: "github", Reason: sanitizeErrorForStorage(err), Retryable: isRetryable(err)})
		return
	}

	topContribs := make([]map[string]interface{}, 0, len(contributors))
	for _, contrib := range contributors {
		topContribs = append(topContribs, map[string]interface{}{
			"login":         contrib.Login,
			"contributions": contrib.Contributions,
		})
	}
	result.Collected = append(result.Collected,
		signal.MakeSignal(makeSignal(entityID, "contributors", profile.SignalGroupGovernance,
			profile.ForgeryHigh, now, ttl,
			map[string]interface{}{"count": len(contributors), "top": topContribs})))
}

func (c *Collector) collectCommits(ctx context.Context, result *signal.CollectionResult,
	entityID, owner, repoName string, now time.Time, ttl time.Duration) {

	commits, err := c.client.GetRecentCommits(ctx, owner, repoName)
	if err != nil {
		result.Collected = append(result.Collected,
			signal.MakeAbsence(entityID, "last_commit", "github", sanitizeErrorForStorage(err), isRetryable(err), now),
			signal.MakeAbsence(entityID, "commit_signing", "github", sanitizeErrorForStorage(err), isRetryable(err), now))
		result.Failures = append(result.Failures,
			signal.CollectionFailure{SignalType: "commits", Source: "github", Reason: sanitizeErrorForStorage(err), Retryable: isRetryable(err)})
		return
	}

	if len(commits) == 0 {
		return
	}

	signedCount := 0
	for _, cm := range commits {
		if cm.Commit.Verification.Verified {
			signedCount++
		}
	}

	result.Collected = append(result.Collected,
		signal.MakeSignal(makeSignal(entityID, "last_commit", profile.SignalGroupVitality,
			profile.ForgeryMediumDeclining, now, ttl,
			map[string]interface{}{
				"date":     commits[0].Commit.Author.Date.Format(time.RFC3339),
				"era":      string(profile.ClassifyEra(commits[0].Commit.Author.Date)),
				"days_ago": int(now.Sub(commits[0].Commit.Author.Date).Hours() / 24),
			})),
		signal.MakeSignal(makeSignal(entityID, "commit_signing", profile.SignalGroupGovernance,
			profile.ForgeryVeryHigh, now, ttl,
			map[string]interface{}{
				"signed_count": signedCount,
				"total_count":  len(commits),
				"ratio":        float64(signedCount) / float64(len(commits)),
			})),
	)
}

func (c *Collector) collectTotalCommits(ctx context.Context, result *signal.CollectionResult,
	entityID, owner, repoName string, now time.Time, ttl time.Duration) {

	totalCommits, err := c.client.GetTotalCommitCount(ctx, owner, repoName)
	if err != nil {
		result.Collected = append(result.Collected,
			signal.MakeAbsence(entityID, "total_commits", "github", sanitizeErrorForStorage(err), isRetryable(err), now))
		result.Failures = append(result.Failures,
			signal.CollectionFailure{SignalType: "total_commits", Source: "github", Reason: sanitizeErrorForStorage(err), Retryable: isRetryable(err)})
		return
	}

	result.Collected = append(result.Collected,
		signal.MakeSignal(makeSignal(entityID, "total_commits", profile.SignalGroupVitality,
			profile.ForgeryHigh, now, ttl,
			map[string]interface{}{"count": totalCommits})))
}

func (c *Collector) collectTags(ctx context.Context, result *signal.CollectionResult,
	entityID, owner, repoName string, now time.Time, ttl time.Duration) {

	tags, err := c.client.GetTags(ctx, owner, repoName)
	if err != nil {
		result.Collected = append(result.Collected,
			signal.MakeAbsence(entityID, "tags", "github", sanitizeErrorForStorage(err), isRetryable(err), now))
		result.Failures = append(result.Failures,
			signal.CollectionFailure{SignalType: "tags", Source: "github", Reason: sanitizeErrorForStorage(err), Retryable: isRetryable(err)})
		return
	}

	tagNames := make([]string, 0, len(tags))
	for _, t := range tags {
		tagNames = append(tagNames, t.Name)
	}
	result.Collected = append(result.Collected,
		signal.MakeSignal(makeSignal(entityID, "tags", profile.SignalGroupPublication,
			profile.ForgeryHigh, now, ttl,
			map[string]interface{}{"count": len(tags), "recent": tagNames})))
}

func (c *Collector) collectOwnerProfile(ctx context.Context, result *signal.CollectionResult,
	entityID, owner string, now time.Time, ttl time.Duration) {

	ownerUser, err := c.client.GetUser(ctx, owner)
	if err != nil {
		result.Collected = append(result.Collected,
			signal.MakeAbsence(entityID, "owner_profile", "github", sanitizeErrorForStorage(err), isRetryable(err), now))
		result.Failures = append(result.Failures,
			signal.CollectionFailure{SignalType: "owner_profile", Source: "github", Reason: sanitizeErrorForStorage(err), Retryable: isRetryable(err)})
		return
	}

	result.Collected = append(result.Collected,
		signal.MakeSignal(makeSignal(entityID, "owner_profile", profile.SignalGroupGovernance,
			profile.ForgeryVeryHigh, now, ttl,
			map[string]interface{}{
				"login":            ownerUser.Login,
				"name":             ownerUser.Name,
				"company":          ownerUser.Company,
				"created":          ownerUser.CreatedAt.Format(time.RFC3339),
				"account_age_days": int(now.Sub(ownerUser.CreatedAt).Hours() / 24),
				"public_repos":     ownerUser.PublicRepos,
				"followers":        ownerUser.Followers,
				"type":             ownerUser.Type,
			})))
}

func (c *Collector) collectAdoption(ctx context.Context, result *signal.CollectionResult,
	entityID, owner, repoName string, stars int, now time.Time, ttl time.Duration) {

	refCount, err := c.client.GetGoModRefCount(ctx, owner+"/"+repoName)
	if err != nil {
		result.Collected = append(result.Collected,
			signal.MakeAbsence(entityID, "adoption", "github", sanitizeErrorForStorage(err), isRetryable(err), now))
		result.Failures = append(result.Failures,
			signal.CollectionFailure{SignalType: "adoption", Source: "github", Reason: sanitizeErrorForStorage(err), Retryable: isRetryable(err)})
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
	result.Collected = append(result.Collected,
		signal.MakeSignal(makeSignal(entityID, "adoption", profile.SignalGroupCriticality,
			profile.ForgeryHigh, now, ttl,
			map[string]interface{}{
				"go_mod_refs":   refCount,
				"stars":         stars,
				"refs_to_stars": ratio,
				"adoption_type": adoptionType,
			})))
}

func (c *Collector) collectCI(ctx context.Context, result *signal.CollectionResult,
	entityID, owner, repoName string, now time.Time, ttl time.Duration) {

	providers := c.collectCIPresence(ctx, owner, repoName)
	if len(providers) > 0 {
		result.Collected = append(result.Collected,
			signal.MakeSignal(makeSignal(entityID, "ci_cd", profile.SignalGroupHygiene,
				profile.ForgeryMediumDeclining, now, ttl,
				map[string]interface{}{"providers": providers})))
	} else {
		result.Collected = append(result.Collected,
			signal.MakeAbsence(entityID, "ci_cd", "github",
				"no CI/CD configuration detected", false, now))
	}
}

func (c *Collector) collectGoDeps(ctx context.Context, result *signal.CollectionResult,
	entityID, owner, repoName string, now time.Time, ttl time.Duration) {

	goModContent, err := c.client.GetFileRaw(ctx, owner, repoName, "go.mod")
	if err != nil {
		result.Collected = append(result.Collected,
			signal.MakeAbsence(entityID, "go_dependencies", "github", sanitizeErrorForStorage(err), isRetryable(err), now))
		result.Failures = append(result.Failures,
			signal.CollectionFailure{SignalType: "go_dependencies", Source: "github", Reason: sanitizeErrorForStorage(err), Retryable: isRetryable(err)})
		return
	}
	if goModContent == nil {
		// Not a Go project — record absence but not as a failure.
		result.Collected = append(result.Collected,
			signal.MakeAbsence(entityID, "go_dependencies", "github",
				"no go.mod found (not a Go project)", false, now))
		return
	}

	deps := parseGoModDeps(string(goModContent))
	result.Collected = append(result.Collected,
		signal.MakeSignal(makeSignal(entityID, "go_dependencies", profile.SignalGroupGovernance,
			profile.ForgeryHigh, now, ttl,
			map[string]interface{}{
				"direct_count":   deps.directCount,
				"indirect_count": deps.indirectCount,
				"total_count":    deps.directCount + deps.indirectCount,
				"direct":         deps.direct,
			})))
}

// collectCIPresence checks for common CI/CD configuration.
func (c *Collector) collectCIPresence(ctx context.Context, owner, repoName string) []string {
	var providers []string

	// GitHub Actions.
	workflows, err := c.client.GetDirectoryContents(ctx, owner, repoName, ".github/workflows")
	if err == nil && len(workflows) > 0 {
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
		if err == nil && content != nil {
			providers = append(providers, cf.provider)
		}
	}

	return providers
}

// sanitizeErrorForStorage returns a safe error description for persistence.
// Raw error messages may contain tokens, URLs with credentials, or other
// sensitive data from HTTP responses. This function classifies the error
// and returns only the category, never the raw content.
func sanitizeErrorForStorage(err error) string {
	if err == nil {
		return ""
	}
	if _, ok := err.(*RateLimitError); ok {
		return "rate limited"
	}
	errMsg := err.Error()
	if strings.Contains(errMsg, "not found") {
		return "not found"
	}
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
	if strings.Contains(errMsg, "GitHub API error 5") {
		return "server error"
	}
	if strings.Contains(errMsg, "GitHub API error 4") {
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
	// Rate limit errors.
	if _, ok := err.(*RateLimitError); ok {
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
	// Server errors (5xx).
	if strings.Contains(errMsg, "GitHub API error 5") {
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

// parseGoModDeps extracts dependency information from go.mod content.
func parseGoModDeps(content string) goModDeps {
	var deps goModDeps
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
	return deps
}

func makeSignal(entityID, signalType string, group profile.SignalGroup,
	forgery profile.ForgeryResistance, collectedAt time.Time, ttl time.Duration,
	value map[string]interface{}) profile.Signal {

	valueBytes, _ := json.Marshal(value)
	return profile.Signal{
		ID:                fmt.Sprintf("github:%s:%s", entityID, signalType),
		EntityID:          entityID,
		Type:              signalType,
		Group:             group,
		Source:            "github",
		ForgeryResistance: forgery,
		Value:             json.RawMessage(valueBytes),
		CollectedAt:       collectedAt,
		ExpiresAt:         collectedAt.Add(ttl),
	}
}
