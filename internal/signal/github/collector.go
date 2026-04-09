package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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

	return signals, nil
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
