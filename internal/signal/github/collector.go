package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
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

// Collect gathers signals for a GitHub-hosted entity.
// The entity URL should be a GitHub repo URL or owner/repo string.
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
	var signals []profile.Signal

	// Repo metadata — vitality, governance, criticality.
	r, err := c.client.GetRepo(ctx, owner, repoName)
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}

	signals = append(signals,
		makeSignal(entity.ID, "last_push", profile.SignalGroupVitality,
			profile.ForgeryMediumDeclining, now, ttl,
			map[string]interface{}{"date": r.PushedAt.Format(time.RFC3339), "era": string(profile.ClassifyEra(r.PushedAt))}),
		makeSignal(entity.ID, "repo_age", profile.SignalGroupVitality,
			profile.ForgeryVeryHigh, now, ttl,
			map[string]interface{}{"created": r.CreatedAt.Format(time.RFC3339), "age_days": int(now.Sub(r.CreatedAt).Hours() / 24)}),
		makeSignal(entity.ID, "stars", profile.SignalGroupCriticality,
			profile.ForgeryMediumDeclining, now, ttl,
			map[string]interface{}{"count": r.StargazersCount}),
		makeSignal(entity.ID, "forks", profile.SignalGroupCriticality,
			profile.ForgeryMediumDeclining, now, ttl,
			map[string]interface{}{"count": r.ForksCount}),
		makeSignal(entity.ID, "open_issues", profile.SignalGroupVitality,
			profile.ForgeryMediumDeclining, now, ttl,
			map[string]interface{}{"count": r.OpenIssuesCount}),
		makeSignal(entity.ID, "archived", profile.SignalGroupVitality,
			profile.ForgeryHigh, now, ttl,
			map[string]interface{}{"archived": r.Archived}),
		makeSignal(entity.ID, "owner_type", profile.SignalGroupGovernance,
			profile.ForgeryHigh, now, ttl,
			map[string]interface{}{"type": r.Owner.Type, "login": r.Owner.Login}),
	)

	if r.License != nil {
		signals = append(signals,
			makeSignal(entity.ID, "license", profile.SignalGroupHygiene,
				profile.ForgeryLowDeclining, now, ttl,
				map[string]interface{}{"spdx_id": r.License.SPDXID}))
	}

	// Contributors — governance.
	contributors, err := c.client.GetContributors(ctx, owner, repoName)
	if err == nil && len(contributors) > 0 {
		topContribs := make([]map[string]interface{}, 0, len(contributors))
		for _, contrib := range contributors {
			topContribs = append(topContribs, map[string]interface{}{
				"login":         contrib.Login,
				"contributions": contrib.Contributions,
			})
		}
		signals = append(signals,
			makeSignal(entity.ID, "contributors", profile.SignalGroupGovernance,
				profile.ForgeryHigh, now, ttl,
				map[string]interface{}{"count": len(contributors), "top": topContribs}))
	}

	// Recent commits — vitality, governance (signing).
	commits, err := c.client.GetRecentCommits(ctx, owner, repoName)
	if err == nil && len(commits) > 0 {
		signedCount := 0
		for _, cm := range commits {
			if cm.Commit.Verification.Verified {
				signedCount++
			}
		}

		signals = append(signals,
			makeSignal(entity.ID, "last_commit", profile.SignalGroupVitality,
				profile.ForgeryMediumDeclining, now, ttl,
				map[string]interface{}{
					"date":     commits[0].Commit.Author.Date.Format(time.RFC3339),
					"era":      string(profile.ClassifyEra(commits[0].Commit.Author.Date)),
					"days_ago": int(now.Sub(commits[0].Commit.Author.Date).Hours() / 24),
				}),
			makeSignal(entity.ID, "commit_signing", profile.SignalGroupGovernance,
				profile.ForgeryVeryHigh, now, ttl,
				map[string]interface{}{
					"signed_count": signedCount,
					"total_count":  len(commits),
					"ratio":        float64(signedCount) / float64(len(commits)),
				}),
		)
	}

	// Total commit count — vitality.
	totalCommits, err := c.client.GetTotalCommitCount(ctx, owner, repoName)
	if err == nil {
		signals = append(signals,
			makeSignal(entity.ID, "total_commits", profile.SignalGroupVitality,
				profile.ForgeryHigh, now, ttl,
				map[string]interface{}{"count": totalCommits}))
	}

	// Tags — publication integrity.
	tags, err := c.client.GetTags(ctx, owner, repoName)
	if err == nil {
		tagNames := make([]string, 0, len(tags))
		for _, t := range tags {
			tagNames = append(tagNames, t.Name)
		}
		signals = append(signals,
			makeSignal(entity.ID, "tags", profile.SignalGroupPublication,
				profile.ForgeryHigh, now, ttl,
				map[string]interface{}{"count": len(tags), "recent": tagNames}))
	}

	// Owner profile — governance.
	ownerUser, err := c.client.GetUser(ctx, owner)
	if err == nil {
		signals = append(signals,
			makeSignal(entity.ID, "owner_profile", profile.SignalGroupGovernance,
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
				}))
	}

	// Adoption — criticality.
	refCount, err := c.client.GetGoModRefCount(ctx, owner+"/"+repoName)
	if err == nil {
		ratio := float64(0)
		if r.StargazersCount > 0 {
			ratio = float64(refCount) / float64(r.StargazersCount)
		}
		adoptionType := "direct"
		if ratio > 10 {
			adoptionType = "mostly-transitive"
		} else if ratio > 1 {
			adoptionType = "mixed"
		}
		signals = append(signals,
			makeSignal(entity.ID, "adoption", profile.SignalGroupCriticality,
				profile.ForgeryHigh, now, ttl,
				map[string]interface{}{
					"go_mod_refs":   refCount,
					"stars":         r.StargazersCount,
					"refs_to_stars": ratio,
					"adoption_type": adoptionType,
				}))
	}

	// CI/CD presence — hygiene.
	ciSignals := c.collectCIPresence(ctx, owner, repoName)
	if len(ciSignals) > 0 {
		signals = append(signals,
			makeSignal(entity.ID, "ci_cd", profile.SignalGroupHygiene,
				profile.ForgeryMediumDeclining, now, ttl,
				map[string]interface{}{"providers": ciSignals}))
	}

	// Transitive dependencies — governance (dependency count and health).
	goModContent, err := c.client.GetFileRaw(ctx, owner, repoName, "go.mod")
	if err == nil && goModContent != nil {
		deps := parseGoModDeps(string(goModContent))
		signals = append(signals,
			makeSignal(entity.ID, "go_dependencies", profile.SignalGroupGovernance,
				profile.ForgeryHigh, now, ttl,
				map[string]interface{}{
					"direct_count":   deps.directCount,
					"indirect_count": deps.indirectCount,
					"total_count":    deps.directCount + deps.indirectCount,
					"direct":         deps.direct,
				}))
	}

	return signals, nil
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
