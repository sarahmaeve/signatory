// Package pranalyzer adapts the github.com/sarahmaeve/pr-analyzer
// mechanistic PR analyzer into a signatory signal collector. It drives
// pr-analyzer's per-PR analysis over signatory's hardened HTTP path and
// folds the per-PR Code Shape / Engineer Profile signals into
// entity-level aggregate signals about a repository's open-PR queue.
package pranalyzer

import (
	"context"

	"github.com/sarahmaeve/pr-analyzer/analyzer"

	"github.com/sarahmaeve/signatory/internal/signal/github"
)

// ghSource adapts signatory's github.Client to pr-analyzer's
// analyzer.PRSource interface. Using our own client (rather than
// pr-analyzer's connectors/github) keeps a single GitHub auth /
// rate-limit / redirect / token-redaction path across signatory.
type ghSource struct {
	client *github.Client
}

func newGitHubSource(client *github.Client) *ghSource {
	return &ghSource{client: client}
}

// FetchPR implements analyzer.PRSource.
func (s *ghSource) FetchPR(ctx context.Context, ref analyzer.PRRef) (analyzer.PR, error) {
	pr, err := s.client.FetchPullRequest(ctx, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		return analyzer.PR{}, err
	}
	return toAnalyzerPR(ref, pr), nil
}

// ListOpenPRs implements analyzer.PRSource.
func (s *ghSource) ListOpenPRs(ctx context.Context, owner, repo string) ([]analyzer.PRRef, error) {
	numbers, err := s.client.ListOpenPullRequestNumbers(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	refs := make([]analyzer.PRRef, len(numbers))
	for i, n := range numbers {
		refs[i] = analyzer.PRRef{Owner: owner, Repo: repo, Number: n}
	}
	return refs, nil
}

// toAnalyzerPR maps a signatory github.PullRequest into pr-analyzer's
// analyzer.PR. The Ref is supplied by the caller (the listing/fetch
// arguments) rather than re-derived from the payload.
func toAnalyzerPR(ref analyzer.PRRef, pr github.PullRequest) analyzer.PR {
	files := make([]analyzer.PRFile, len(pr.Files))
	for i, f := range pr.Files {
		files[i] = analyzer.PRFile{
			Path:      f.Path,
			Status:    f.Status,
			Additions: f.Additions,
			Deletions: f.Deletions,
		}
	}
	return analyzer.PR{
		Ref:               ref,
		Title:             pr.Title,
		Author:            pr.Author,
		URL:               pr.URL,
		State:             pr.State,
		Draft:             pr.Draft,
		BaseRef:           pr.BaseRef,
		HeadRef:           pr.HeadRef,
		BaseSHA:           pr.BaseSHA,
		HeadSHA:           pr.HeadSHA,
		Additions:         pr.Additions,
		Deletions:         pr.Deletions,
		ChangedFiles:      pr.ChangedFiles,
		Labels:            pr.Labels,
		AuthorAssociation: pr.AuthorAssociation,
		AuthorType:        pr.AuthorType,
		CreatedAt:         pr.CreatedAt,
		UpdatedAt:         pr.UpdatedAt,
		Files:             files,
	}
}
