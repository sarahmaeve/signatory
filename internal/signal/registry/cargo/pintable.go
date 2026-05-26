package cargo

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/gitenv"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	"github.com/sarahmaeve/signatory/internal/sigstore/fulcio"
)

// pinSourceTagMatch is the per-pin provenance-strength label stamped
// on a version_pin_table entry when the SHA was recovered by
// resolving a tag against the local clone. Naming follows AST.md
// §2's "provenance-strength labelling" requirement so an analyst
// reading a matrix row knows the SHA came from publisher-asserted
// tag metadata — weaker than a Fulcio source-repo-digest, stronger
// than nothing. A future commit can add a "cargo-vcs-info" tier
// for the .cargo_vcs_info.json upgrade pass (recent-window only,
// publisher-stamped-but-attacker-controlled, gated by fulcio.IsGit
// ObjectID before persistence) without changing this constant.
const pinSourceTagMatch = "cargo-tag-match"

// PinTableCollector emits the version_pin_table signal for a cargo
// entity by resolving each non-yanked, time-orderable crates.io
// version against the local clone's tags.
//
// crates.io exposes no publisher-asserted commit SHA in registry
// JSON (no equivalent of npm's gitHead or pypi's PEP 740 Fulcio
// digest). Tag-match against the clone is the cheapest available
// pinning method; versions whose tags don't resolve land in
// MissingOriginVersions rather than blocking the whole table.
//
// Separate from cargo.Collector because the clone path is not
// resolved by the dispatch orchestrator until after the registry-
// layer collectors run, and the cargo registry collector must keep
// running for cargo entities WITHOUT a clone (last_publish,
// recent_downloads, owner_count etc. don't need source). The cost
// is one extra GetCrate HTTP call per analyze — acceptable since
// analyze is not run in tight loops and crates.io rate limits are
// generous.
//
// Implements signal.Collector. Self-gates on entity.Ecosystem in
// {cargo, crates}; non-cargo entities receive an empty (non-nil)
// result with no error.
type PinTableCollector struct {
	client    *Client
	clonePath string
}

// NewPinTableCollector returns a PinTableCollector bound to client
// (the same crates.io client the registry collector uses) and
// clonePath (the post-cloneToTempIsolated path the dispatch
// orchestrator passes after resolveClonePath returns successfully).
// An empty clonePath is legal — Collect emits a clear absence
// rather than failing.
func NewPinTableCollector(client *Client, clonePath string) *PinTableCollector {
	return &PinTableCollector{client: client, clonePath: clonePath}
}

// Name identifies the collector. Uses the same "cargo-registry"
// label as the parent cargo.Collector so the analyst-facing source
// string is uniform across cargo signals — the user reads "this
// came from the cargo-registry domain", not which Go struct
// emitted it. Parallel to how npm and pypi emit their version_pin_
// table from the same source string as their other registry signals.
func (c *PinTableCollector) Name() string { return collectorSource }

// Collect emits the version_pin_table for the entity.
//
// Outcome cases:
//   - Non-cargo entity: empty result (the per-ecosystem self-gate).
//   - Empty clone path: absence with retryable=false. The operator
//     must pass --clone to enable tag-match resolution.
//   - GetCrate failure: failure record on version_pin_table itself
//     (retryable unless ErrNotFound). Mirrors the cargo registry
//     collector's degradation pattern.
//   - Happy path: version_pin_table signal lands with one pin per
//     non-yanked, time-orderable version whose tag resolves in the
//     local clone, plus MissingOriginVersions for the rest.
func (c *PinTableCollector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	result := &signal.CollectionResult{}
	packageName, ok := extractCargoPackageName(entity)
	if !ok {
		return result, nil
	}
	collectedAt := time.Now().UTC()

	if c.clonePath == "" {
		result.RecordAbsence(entity.ID, "version_pin_table", collectorSource,
			"no clone path; tag-match pin emission requires --clone",
			false, collectedAt)
		return result, nil
	}

	cr, err := c.client.GetCrate(ctx, packageName)
	if err != nil {
		retryable := !errors.Is(err, ErrNotFound)
		result.RecordFailure(entity.ID, "version_pin_table", collectorSource,
			sanitizeFetchReason(err), retryable, collectedAt)
		return result, nil
	}

	c.recordVersionPinTable(ctx, result, entity.ID, packageName, cr, collectedAt)
	return result, nil
}

// versionPin and versionPinTableValue mirror gopublish's
// VersionPin / VersionPinTableValue JSON shape verbatim. Like the
// npm collector's parallel definitions, these intentionally stay
// independent of source.VersionPin so the registry package has no
// import dependency on source-evolution. The ecosystem-blind
// consumer (source.pinTableFromSignals) matches on signal type
// "version_pin_table" alone.
type versionPin struct {
	Version     string `json:"version"`
	SHA         string `json:"sha"`
	Source      string `json:"source"`
	PublishedAt string `json:"published_at"`
}

type versionPinTableValue struct {
	ModulePath            string       `json:"module_path"`
	VersionCountTotal     int          `json:"version_count_total"`
	VersionCountProcessed int          `json:"version_count_processed"`
	Pins                  []versionPin `json:"pins"`
	MissingOriginVersions []string     `json:"missing_origin_versions"`
	FetchFailedVersions   []string     `json:"fetch_failed_versions"`
}

// recordVersionPinTable synthesizes the pin table by tag-matching
// each non-yanked, time-orderable version against the local clone.
//
// A version is *processed* when it carries an RFC3339-parseable
// CreatedAt (the chronological axis source-evolution's matrix
// orders by). A processed version is *pinned* when at least one of
// its tag-candidate forms — "v<version>" first, then bare
// "<version>" — resolves in the local clone via
// `git rev-parse --verify <tag>^{commit}`. The "v"-prefix is tried
// first so the canonical form wins ties, matching the artifact
// pair-resolver's precedence (internal/signal/artifact/pair.go).
//
// Every resolved SHA passes through fulcio.IsGitObjectID before
// joining the pin list — same trust-boundary discipline npm
// applies to gitHead and pypi applies to Fulcio source-repo-
// digests (AST.md §2.1 trust boundary). `git rev-parse --verify
// ^{commit}` already returns a real 40-char SHA-1, so the gate is
// belt-and-braces against any future code path that bypasses
// rev-parse.
//
// Versions whose tags resolve in neither form land in
// MissingOriginVersions — they were processed but unpinnable. The
// matrix consumer renders these as missing-origin rows with no AST
// data, the same shape npm uses for processed-but-not-pinned
// versions.
func (c *PinTableCollector) recordVersionPinTable(ctx context.Context,
	result *signal.CollectionResult, entityID, packageName string,
	cr *CrateResponse, collectedAt time.Time) {

	processed := 0
	pins := make([]versionPin, 0, len(cr.Versions))
	var missingOrigin []string

	for _, v := range cr.Versions {
		if v.Yanked {
			continue
		}
		t, err := time.Parse(time.RFC3339, v.CreatedAt)
		if err != nil {
			// Unorderable on the chronological axis — skip, mirroring
			// npm's "skip versions with no time entry" discipline.
			continue
		}
		processed++

		sha, ok := c.resolveTagSHA(ctx, v.Num)
		if !ok || !fulcio.IsGitObjectID(sha) {
			missingOrigin = append(missingOrigin, v.Num)
			continue
		}

		pins = append(pins, versionPin{
			Version:     v.Num,
			SHA:         sha,
			Source:      pinSourceTagMatch,
			PublishedAt: t.UTC().Format(time.RFC3339),
		})
	}

	// ModulePath is the trust-boundary-validated entity name
	// (extractCargoPackageName already gated pkg:cargo/<name> on the
	// purl grammar), NOT cr.Crate.Name — the registry-supplied name
	// is publisher-controlled and could disagree with the canonical
	// purl. The validated identifier is what downstream source-
	// evolution renders into module_path.
	result.RecordSignal(entityID, "version_pin_table", collectorSource,
		collectedAt, defaultTTL,
		versionPinTableValue{
			ModulePath:            packageName,
			VersionCountTotal:     len(cr.Versions),
			VersionCountProcessed: processed,
			Pins:                  pins,
			MissingOriginVersions: missingOrigin,
		})
}

// resolveTagSHA tries to resolve a version string to a commit SHA
// via `git rev-parse --verify <tag>^{commit}`. Two candidates per
// version: "v<version>" first (the canonical form, dominant in
// modern crates), then bare "<version>" (older crates and some
// crate2nix-style projects).
//
// On both misses returns ("", false); the caller treats the
// version as missing_origin. Per-attempt subprocess errors (the
// dominant cause is "tag does not exist") are silently treated as
// misses — this is by design because the pin table is a best-
// effort signal and surfacing a per-missing-version git error
// would flood operator logs without adding diagnostic value.
//
// `--verify` + the `^{commit}` peel ensures rev-parse refuses to
// echo a SHA-shaped string for unresolvable refs and that the
// resolved target is a commit object, not a tree or blob. Same
// discipline artifact.gitInspector.CommitForRef uses.
//
// Each candidate runs its own subprocess. Bounded at most two
// rev-parse calls per version; on serde-sized crates (~200
// versions) that's ~400 subprocess calls, ~2-4 seconds added to an
// analyze. Acceptable since analyze is not in a tight loop.
func (c *PinTableCollector) resolveTagSHA(ctx context.Context, version string) (string, bool) {
	for _, tag := range []string{"v" + version, version} {
		cmd := gitenv.NewCmd(ctx, "-C", c.clonePath,
			"rev-parse", "--verify", tag+"^{commit}")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			continue
		}
		sha := strings.TrimSpace(stdout.String())
		if sha != "" {
			return sha, true
		}
	}
	return "", false
}

// Compile-time assertion that PinTableCollector satisfies the
// signal.Collector contract. Lives in the producer's file so a
// future signature drift on the contract fails build here rather
// than at the dispatch site.
var _ signal.Collector = (*PinTableCollector)(nil)
