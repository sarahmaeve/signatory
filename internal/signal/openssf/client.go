package openssf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrNotFound is returned (wrapped via %w) when the Scorecard API
// responds 404. Distinct from network/5xx errors because it carries
// product meaning: the project hasn't been indexed by Scorecard,
// not that we couldn't reach the service.
var ErrNotFound = errors.New("openssf: scorecard not found")

// maxResponseSize bounds upstream response bodies. Real Scorecard
// payloads are 5-50 KiB (one entry per ~18 standard checks plus
// metadata); 1 MiB is generous slack and a hard stop on a
// misbehaving upstream.
const maxResponseSize = 1 * 1024 * 1024

// ownerRepoMaxLen caps owner / repo path components before
// validation runs. Real GitHub limits are 39 chars (owner) and 100
// chars (repo); 256 is generous defense-in-depth. The point is
// "rejecting absurd lengths cheaply," not enforcing GitHub's exact
// grammar — the API itself returns 404 on a non-existent repo.
const ownerRepoMaxLen = 256

// ValidateOwnerRepo enforces a narrow grammar on the two path
// components before they land in the URL. GitHub's grammar (per
// https://docs.github.com/repositories) accepts ASCII letters,
// digits, hyphens, underscores, and dots — we accept the same
// set and reject anything else, including path/query/fragment
// metacharacters that would re-parse the request.
//
// Validation at the function boundary lets a future caller thread
// in attacker-controlled strings without smuggling URL syntax into
// the request. Symmetric with gopublish's ValidateModulePath.
func ValidateOwnerRepo(owner, repo string) error {
	if err := validatePathComponent("owner", owner); err != nil {
		return err
	}
	if err := validatePathComponent("repo", repo); err != nil {
		return err
	}
	return nil
}

// validatePathComponent runs the shared grammar check on owner or
// repo. Empty, oversize, or any character outside [A-Za-z0-9._-] is
// a hard reject.
func validatePathComponent(label, val string) error {
	if val == "" {
		return fmt.Errorf("%s is empty", label)
	}
	if len(val) > ownerRepoMaxLen {
		return fmt.Errorf("%s exceeds %d-byte cap (got %d)", label, ownerRepoMaxLen, len(val))
	}
	for _, r := range val {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			// allowed
		default:
			return fmt.Errorf("%s %q contains disallowed character %q", label, val, r)
		}
	}
	if strings.HasPrefix(val, ".") || strings.HasSuffix(val, ".") {
		return fmt.Errorf("%s %q must not start or end with '.'", label, val)
	}
	return nil
}

// Scorecard is the parsed top-level Scorecard API response. Field
// set is intentionally narrow — what the collector emits as a
// signal — but the underlying response carries more (per-check
// `details` and `documentation`) which we drop to keep the stored
// blob small and the schema stable. Adding fields later is easier
// than removing them.
type Scorecard struct {
	// AggregateScore is the headline 0-10 number Scorecard exposes
	// at the top of the response. Distinct from individual check
	// scores; computed by Scorecard as a weighted average.
	AggregateScore float64 `json:"score"`

	// AsOf is the date Scorecard last ran for this project, in
	// YYYY-MM-DD form (Scorecard runs roughly weekly per indexed
	// project). Stored as the upstream string rather than parsed
	// time.Time so a malformed date doesn't fail the whole signal.
	AsOf string `json:"date,omitempty"`

	// Repo identifies the GitHub repo and the commit Scorecard
	// scored. Useful for auditing — a future analyst can confirm
	// the score was computed against a specific commit, not a
	// floating tip.
	Repo RepoRef `json:"repo,omitzero"`

	// ScorecardVersion records which Scorecard release produced
	// this result. The check set evolves; callers comparing
	// across versions should compare ScorecardVersion too.
	ScorecardVersion VersionRef `json:"scorecard,omitzero"`

	// Checks is the per-check breakdown — the granular evidence
	// behind AggregateScore. Stored as a slice (matching the wire
	// shape) rather than a map so iteration order is the upstream
	// order and JSON round-trips are byte-stable.
	Checks []Check `json:"checks,omitempty"`
}

// RepoRef is the {name, commit} pair Scorecard records for the
// scored project. Name is "github.com/owner/repo" form.
type RepoRef struct {
	Name   string `json:"name,omitempty"`
	Commit string `json:"commit,omitempty"`
}

// VersionRef is the {version, commit} pair identifying which
// Scorecard release computed the score.
type VersionRef struct {
	Version string `json:"version,omitempty"`
	Commit  string `json:"commit,omitempty"`
}

// Check is one entry from the Scorecard checks array. Score is
// either -1 (not applicable / could not be determined) or 0-10.
// Reason is Scorecard's human-readable summary of why the score
// landed where it did.
type Check struct {
	Name   string `json:"name"`
	Score  int    `json:"score"`
	Reason string `json:"reason,omitempty"`
}

// Client is a narrow Scorecard HTTP client. Surface is one method
// (GetScorecard) plus a constructor; extending to additional
// Scorecard endpoints (badge, raw) should follow the same TDD
// shape — test the response model, then add the method.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// NewClient returns a Client bound to the public Scorecard API.
// Timeout matches the gopublish + npm + github clients' 60s for
// uniform cancellation behavior across collectors.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout:       60 * time.Second,
			CheckRedirect: checkRedirect,
		},
		baseURL: "https://api.securityscorecards.dev",
	}
}

// NewClientWithBaseURL returns a Client bound to base. Tests pass
// an httptest.Server URL; production wires the public API.
func NewClientWithBaseURL(base string) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout:       60 * time.Second,
			CheckRedirect: checkRedirect,
		},
		baseURL: base,
	}
}

// checkRedirect refuses scheme downgrades and bounds the redirect
// chain. Symmetric with the other ecosystem clients — uniform
// policy lowers the review burden when a future collector ships
// against another endpoint.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
		return fmt.Errorf("refusing redirect to non-HTTP(S) URL %s", req.URL.Redacted())
	}
	// Scheme-downgrade defense: once we started on https, never
	// follow a redirect to http. tests use the http httptest base
	// URL exclusively, so the policy gate is "never downgrade,"
	// not "https-only."
	if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing scheme downgrade to %s", req.URL.Redacted())
	}
	if len(via) >= 10 {
		return fmt.Errorf("too many redirects")
	}
	return nil
}

// GetScorecard fetches /projects/github.com/{owner}/{repo} and
// parses the response. Returns ErrNotFound (wrapped) on 404 — the
// caller distinguishes that case from network / 5xx via errors.Is
// to record the right kind of absence.
func (c *Client) GetScorecard(ctx context.Context, owner, repo string) (*Scorecard, error) {
	if err := ValidateOwnerRepo(owner, repo); err != nil {
		return nil, fmt.Errorf("get scorecard: %w", err)
	}
	url := c.baseURL + "/projects/github.com/" + owner + "/" + repo

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s/%s: %w", owner, repo, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "signatory/0.1")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request for %s/%s failed: %w", owner, repo, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close after read; err is not actionable

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s/%s", ErrNotFound, owner, repo)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Discard body without surfacing it — the response can
		// carry server-debug noise. Same approach as gopublish.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseSize))
		return nil, fmt.Errorf("upstream returned status %d for %s/%s", resp.StatusCode, owner, repo)
	}

	limited := io.LimitReader(resp.Body, maxResponseSize+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read response for %s/%s: %w", owner, repo, err)
	}
	if int64(len(body)) > maxResponseSize {
		return nil, fmt.Errorf("response for %s/%s exceeds %d-byte cap", owner, repo, maxResponseSize)
	}

	var sc Scorecard
	if err := json.Unmarshal(body, &sc); err != nil {
		return nil, fmt.Errorf("decode scorecard response for %s/%s: %w", owner, repo, err)
	}
	return &sc, nil
}
