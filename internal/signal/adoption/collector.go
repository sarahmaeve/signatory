package adoption

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// sourceName is the value stamped on every emitted signal.
// "adoption" matches the collector identity, per the codebase
// convention. See package doc for the source-name-change note vs.
// the pre-lift github collector emission.
const sourceName = "adoption"

// signalTTL is the cache lifetime stamped on each emitted signal.
// 24h matches the per-forge metadata collectors — the search-API
// data shape and staleness tolerance are the same.
const signalTTL = 24 * time.Hour

// forgeHostPrefix is the recognized forge-host set for module-path
// derivation. Same hosts the URL gate in profile/target.go admits:
// github.com, codeberg.org, gitlab.com. Module import paths for
// targets on other hosts can't be derived confidently from the URL
// alone (vanity-host resolution isn't implemented here), so the
// collector skips them.
var forgeHostPrefix = map[string]bool{
	"github.com":   true,
	"codeberg.org": true,
	"gitlab.com":   true,
}

// Collector emits the "adoption" signal for git-hosted Go modules.
// One signal per Collect call: an inbound-go.mod-references count
// from GitHub's code-search API, paired with the entity's star
// count (read from inRunResult) into a refs_to_stars ratio and an
// adoption_type classification.
//
// inRun is the orchestrator's accumulated CollectionResult passed
// through CollectOpts; the adoption collector reads it for the
// just-emitted stars signal from whichever per-forge metadata
// collector ran earlier in the dispatch order. nil-safe — when
// inRun is nil or doesn't carry a stars signal for this entity,
// adoption emits with stars=0 and refs_to_stars=0 (the
// graceful-degradation path; see TestCollector_NoStarsInRun_StillEmits).
type Collector struct {
	client *Client
	inRun  *signal.CollectionResult
}

// NewCollector creates a Collector with a production search client.
func NewCollector() *Collector {
	return &Collector{client: NewClient()}
}

// NewCollectorWithClient creates a Collector with a caller-provided
// search client. Test injection point — production code uses
// NewCollector.
func NewCollectorWithClient(c *Client) *Collector {
	return &Collector{client: c}
}

// WithInRun wires the orchestrator's accumulated CollectionResult
// into the collector so the adoption emission can read sibling
// collectors' just-emitted "stars" signal. Returns the receiver for
// chaining; mirrors github/forgejo/gitlab collectors' WithEntityStore
// setter shape.
//
// Production wiring (cmd/signatory/collectors.go) calls WithInRun
// with opts.InRunResult; tests pass a hand-built CollectionResult
// pre-seeded with stars to exercise the ratio path.
func (c *Collector) WithInRun(r *signal.CollectionResult) *Collector {
	c.inRun = r
	return c
}

// Name returns the collector identifier the orchestrator's progress
// narration keys on ("[adoption] Collected N signals").
func (c *Collector) Name() string { return sourceName }

// Collect emits the adoption signal for a Go-ecosystem entity hosted
// on a recognized forge.
//
// Gates (in order):
//  1. entity is non-nil
//  2. ecosystem is Go-shaped ("" / "golang" / "go") — non-Go entities
//     skip because the filename:go.mod search would always return 0
//  3. URL is non-empty and host is in forgeHostPrefix — non-forge
//     URLs can't be turned into a confident Go import path
//
// On hit, derives the module path "<host>/<owner>/<repo>" from the
// entity URL, runs the search, reads stars from inRunResult (best
// effort, 0 if absent), and emits one "adoption" signal.
//
// Search-API failures (rate limit, 5xx) surface as RecordFailure on
// the result — same shape the per-forge metadata collectors' error
// branches use. Does NOT return an error: the orchestrator's later
// collectors must still run.
func (c *Collector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	if entity == nil {
		return &signal.CollectionResult{}, nil
	}
	if !isGoEcosystem(entity.Ecosystem) {
		return &signal.CollectionResult{}, nil
	}
	modulePath, ok := deriveModulePath(entity.URL)
	if !ok {
		return &signal.CollectionResult{}, nil
	}

	now := time.Now().UTC()
	var result signal.CollectionResult

	refCount, err := c.client.GoModRefCount(ctx, modulePath)
	if err != nil {
		// API failure is retryable for rate-limit, not for non-rate-limit
		// status errors. Same classification the github collector
		// applied pre-lift.
		retryable := errors.Is(err, ErrRateLimit)
		result.RecordFailure(entity.ID, "adoption", sourceName, sanitizeForStorage(err), retryable, now)
		return &result, nil
	}

	stars := readStarsFromInRun(c.inRun, entity.ID)
	ratio := float64(0)
	if stars > 0 {
		ratio = float64(refCount) / float64(stars)
	}
	adoptionType := classifyAdoption(ratio)

	result.RecordSignal(entity.ID, "adoption", sourceName, now, signalTTL,
		map[string]any{
			"go_mod_refs":   refCount,
			"stars":         stars,
			"refs_to_stars": ratio,
			"adoption_type": adoptionType,
		})

	return &result, nil
}

// isGoEcosystem returns true when the ecosystem string indicates a
// Go target. Empty (unclassified — bare repo: targets) admits to
// preserve the github collector's pre-lift behavior of running
// adoption on repo-shaped entities regardless of detected ecosystem.
func isGoEcosystem(eco string) bool {
	return eco == "" || eco == "golang" || eco == "go"
}

// deriveModulePath turns an entity URL into a Go module import-path
// shape (host/owner/repo or host/group/subgroup/proj for nested
// gitlab namespaces). Returns ("", false) when the URL isn't on a
// recognized forge or when owner/repo can't be parsed.
//
// Permissive prefix-strip then minimum-path-length check. Does NOT
// validate against per-forge name grammar — upstream
// profile.NormalizeForgeRepoInput gates that before the entity
// reaches the collector.
func deriveModulePath(rawURL string) (string, bool) {
	if rawURL == "" {
		return "", false
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", false
	}
	host := u.Host
	if host == "" {
		host, _, _ = strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	}
	host = strings.TrimPrefix(host, "www.")
	if !forgeHostPrefix[host] {
		return "", false
	}

	path := strings.TrimPrefix(u.Path, "/")
	if host == strings.Split(rawURL, "/")[0] {
		// Bare-host input ("github.com/owner/repo" without scheme):
		// url.Parse stores everything in Path with no Host. Already
		// handled above; this branch is unreachable in production
		// because the URL gate produces scheme-prefixed URLs, but the
		// defensive walk through u.Path keeps the helper robust.
		_, path, _ = strings.Cut(u.Path, "/")
		_, path, _ = strings.Cut(path, "/")
	}
	path = strings.TrimSuffix(path, ".git")
	path = strings.TrimSuffix(path, "/")

	// Need at least owner/repo. Nested namespaces (gitlab) pass
	// because strings.Count returns >= 1 either way.
	if !strings.ContainsRune(path, '/') || path == "" {
		return "", false
	}
	return host + "/" + path, true
}

// readStarsFromInRun walks the in-run accumulator for the entity's
// most-recently-emitted "stars" signal. Returns 0 when no such
// signal exists, when inRun is nil, or when the signal value
// doesn't carry the expected "count" field. Best-effort —
// adoption's contract is to emit even without a stars input.
//
// Last-write-wins on duplicate stars signals (the forge metadata
// collectors only ever emit one stars signal per entity per run,
// but the iteration order is intentionally tolerant).
func readStarsFromInRun(inRun *signal.CollectionResult, entityID string) int {
	if inRun == nil {
		return 0
	}
	stars := 0
	for _, sig := range inRun.Signals() {
		if sig.EntityID != entityID || sig.Type != "stars" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal(sig.Value, &v); err != nil {
			continue
		}
		if c, ok := v["count"].(float64); ok {
			stars = int(c)
		}
	}
	return stars
}

// classifyAdoption maps a refs_to_stars ratio to a category label.
// Matches the pre-lift github collector's thresholds verbatim so
// existing posture rules and analyst training data keep the same
// boundaries.
func classifyAdoption(ratio float64) string {
	switch {
	case ratio > 10:
		return "mostly-transitive"
	case ratio > 1:
		return "mixed"
	default:
		return "direct"
	}
}

// sanitizeForStorage produces a short, redaction-safe description of
// an API error for the CollectionError.Reason field. Mirrors the
// shape the github collector's sanitizeErrorForStorage used —
// classified categories rather than raw error strings, so accidental
// token/URL/body leakage can't propagate to stderr / persisted store.
func sanitizeForStorage(err error) string {
	switch {
	case errors.Is(err, ErrRateLimit):
		return "rate limit exceeded"
	case err == nil:
		return ""
	default:
		// Keep the error message but strip anything that could carry
		// a token, URL with credentials, or response body. The Client
		// error messages above are token-free and body-free by
		// construction; the fmt.Errorf path-wrap is safe to surface.
		s := err.Error()
		// Bound the length so a pathological wrap chain doesn't blow
		// up the persisted record.
		const maxLen = 200
		if len(s) > maxLen {
			s = s[:maxLen] + "…"
		}
		return s
	}
}
