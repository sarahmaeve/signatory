package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/audit"
	"github.com/sarahmaeve/signatory/internal/identity"
	"github.com/sarahmaeve/signatory/internal/manifest/gomod"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	cargoregistry "github.com/sarahmaeve/signatory/internal/signal/registry/cargo"
	gemregistry "github.com/sarahmaeve/signatory/internal/signal/registry/gem"
	"github.com/sarahmaeve/signatory/internal/signal/registry/gopublish"
	mavenregistry "github.com/sarahmaeve/signatory/internal/signal/registry/maven"
	npmregistry "github.com/sarahmaeve/signatory/internal/signal/registry/npm"
	pypiregistry "github.com/sarahmaeve/signatory/internal/signal/registry/pypi"
	"github.com/sarahmaeve/signatory/internal/store"
)

// AnalyzeCmd collects Layer 1 signals for a target and displays the
// cached trust profile the store currently holds for it.
//
// IMPORTANT: despite the verb name, this command does NOT produce a
// trust verdict, posture recommendation, or LLM-backed analysis. It:
//
//   - Runs deterministic collectors (github API, git local clone,
//     npm registry) against the target and persists Layer 1 signals.
//   - Reads back any postures already in the store and displays them.
//
// What it does NOT do:
//
//   - Dispatch analyst agents.
//   - Produce conclusions, positive_absences, methodology.
//   - Set or propose a posture.
//
// For an actual trust verdict (the work that turns a [?] row in
// `signatory survey` into [✓] or [✗]), invoke the /analyze skill
// inside a Claude session with the signatory MCP server connected.
// That path dispatches specialist analyst agents, ingests their
// v1-schema output via signatory_ingest_analysis, and materializes
// a posture via `signatory posture accept <output-id>`.
//
// The scope mismatch between this verb's name and its v0.1 behavior
// is tracked: an earlier design envisioned `signatory analyze
// --depth=full` running the LLM-backed pipeline in-process. v0.1
// Invariant 1 ("forbid LLM-client deps and API hostnames" in the
// binary) forced the LLM work out of the CLI and into the Claude-skill
// harness. This command is today's `--depth=signals` equivalent;
// the other depths remain aspirational until v0.2 reopens the
// question of in-process LLM invocation.
//
// Target resolution: the user-supplied target is parsed via
// profile.ResolveTarget so every accepted input form (GitHub
// shorthand, https URL, SCP-form, or canonical URI) collapses to
// the same entity. This prevents duplicate-entity fragmentation
// (#53) and lets analyze accept package-scheme URIs like
// pkg:npm/express uniformly with repo-scheme URIs.
type AnalyzeCmd struct {
	Target  string        `arg:"" help:"Package name, repo URL, or identity to analyze."`
	Refresh bool          `help:"Collect fresh signals from network sources." default:"false"`
	JSON    bool          `help:"Output as JSON." default:"false"`
	MaxAge  time.Duration `help:"Surface only analyst outputs ingested within this duration (Go duration syntax: 24h, 168h, 720h). 0 = no age filter." default:"0"`

	// --path points at an existing local clone of the target. Required
	// with --refresh for git-hosted entities unless --clone is also
	// passed. Without --clone, the clone must be full (not shallow); a
	// shallow clone is rejected with ErrShallowClone pointing the user
	// toward --clone for unshallow. See design/v0.1-invariants.md
	// §"Invariant 2" for the "no implicit network" principle this flag
	// serves.
	Path string `name:"path" help:"Filesystem path to an existing local clone of the target. Required with --refresh for git-hosted entities unless --clone is passed. Must be a full clone — shallow clones are rejected." type:"path"`

	// --clone ensures --path holds a current, complete clone of the
	// target. Idempotent: clones if --path is missing or empty, fetches
	// if --path holds a valid clone, fetches --unshallow if --path holds
	// a shallow clone, refuses with ErrOriginMismatch if --path holds a
	// clone of a different repo. Requires --refresh (--clone is plumbing
	// for the refresh path's collector dispatch). When --path is unset,
	// defaults to filestore/clones/<short-name>.
	Clone bool `name:"clone" help:"Ensure --path holds a current, complete full clone of the target (clone if missing, fetch if present, fetch --unshallow if shallow). Defaults --path to filestore/clones/<short-name> when unset. Requires --refresh."`

	// --allow-fetch is the opt-in for the source-evolution
	// collector's BlobStreamer fetch-on-missing-SHA retry path.
	// Default false (no fetch): a missing SHA in the local clone
	// surfaces as tag_sha_local_status="missing_from_clone" in
	// the matrix row, itself a forgery-resistance HIGH signal.
	// Operators who know their clone may be stale relative to the
	// proxy can opt in to the targeted retry.
	AllowFetch bool `name:"allow-fetch" help:"Allow the source-evolution collector to retry missing-SHA reads via 'git fetch origin' once. Default off — a missing SHA after --refresh is preserved as a signal rather than fetched." default:"false"`

	// --ignore-burn overrides the pre-collection burn gate. By
	// default, --refresh on a target whose owner is burned (or the
	// target itself) refuses to run collectors and exits non-zero,
	// so a burned vendor can't accidentally re-collect "fresh"
	// signals that would contradict the operator-burn classification.
	// Pass --ignore-burn for forensic / verification cases where you
	// explicitly want signals collected on a known-burned target.
	// Withdrawing the burn (signatory burn remove) is the signal-
	// agnostic alternative — that's the right path when the burn
	// turns out to have been premature.
	IgnoreBurn bool `name:"ignore-burn" help:"Override the pre-collection burn gate. Default refuses to collect signals when the target's owner is burned." default:"false"`

	// RunGit overrides the git subprocess invocation for clone-shaped
	// operations triggered by --clone (fresh clone, fetch on an existing
	// valid clone, fetch --unshallow on a shallow clone). nil → fall
	// through to defaultRunGit, which builds via gitenv.NewCloneCmd.
	//
	// workdir is the working directory for the git operation:
	//   - empty string for clone operations (the destination path is
	//     carried in args, e.g. ["clone", url, dest])
	//   - the existing clone path for fetch/unshallow operations
	//     (e.g. workdir=dest, args=["fetch"] or args=["fetch","--unshallow"])
	//
	// Mirrors HandoffCmd.RunGitClone: struct-field seam for parallel-safe
	// test injection, visible dependency. Tests inject a closure that
	// records calls without spawning real git subprocesses; production
	// leaves it nil and ensureCloneAtPath falls through to defaultRunGit.
	RunGit func(ctx context.Context, workdir string, args ...string) error `kong:"-"`

	// Stdout and Stderr let tests inject buffers. Production paths
	// leave them nil; Run defaults them to os.Stdout / os.Stderr.
	// stdout/stderr discipline: progress, warnings, and status
	// lines go to stderr so that stdout carries ONLY the final
	// rendered output (JSON payload or human-readable profile).
	// This unblocks `signatory analyze --json … | jq` pipelines.
	Stdout io.Writer `kong:"-"`
	Stderr io.Writer `kong:"-"`
}

// formatBurnGateError renders the "analyze refusing to collect"
// error returned by the pre-collection burn gate. Multi-line so
// the user sees the cascade trace AND the override flag in one
// place; no need to re-run with --verbose to figure out which
// ledger entry caused the refusal.
//
// Direct burns and cascaded burns get distinct phrasing — a
// direct burn on the queried entity reads "<URI> is burned",
// whereas a cascaded burn reads "<URI> is burned via <role>
// <owner-URI>" so the user can trace the cascade source.
func formatBurnGateError(canonicalURI string, burn *profile.Burn, ctx *store.EffectiveBurnContext) error {
	var subject string
	if ctx != nil && !ctx.Direct && ctx.ViaOwner != nil {
		subject = fmt.Sprintf("%s is burned via %s %s",
			canonicalURI, ctx.ViaRole, ctx.ViaOwner.CanonicalURI)
	} else {
		subject = fmt.Sprintf("%s is burned", canonicalURI)
	}
	return fmt.Errorf(
		"analyze refusing to collect — %s\n"+
			"  Reason: %s\n"+
			"  Burned by: %s at %s\n"+
			"  Pass --ignore-burn to collect anyway (forensic / verification cases),\n"+
			"  or `signatory burn remove %s` if the burn was premature.",
		subject,
		burn.Reason,
		burn.BurnedBy, burn.BurnedAt.Format(time.RFC3339),
		burnRemoveTargetForGate(canonicalURI, ctx),
	)
}

// burnRemoveTargetForGate picks the URI a user would point
// `signatory burn remove` at to clear the gate. For a cascaded
// burn that's the owner URI (the actual ledger row); for a direct
// burn it's the queried URI itself.
func burnRemoveTargetForGate(canonicalURI string, ctx *store.EffectiveBurnContext) string {
	if ctx != nil && !ctx.Direct && ctx.ViaOwner != nil {
		return ctx.ViaOwner.CanonicalURI
	}
	return canonicalURI
}

// ecosystemResolverEntry binds an entity's Ecosystem field to the
// strategy that asks the corresponding registry "where is this
// package's source?" and stamps the answer on the entity. Match
// holds the ecosystem strings this entry handles (a list, not a
// single value, so synonym ecosystems like "cargo"/"crates" or
// "golang"/"go" share one entry without duplicating scaffolding).
//
// Label is the human-readable name used in stderr warnings and the
// %w-wrapped --refresh error. AbsenceSource is the source field of
// the absence:repo_declaration signal recorded on resolver failure.
//
// Resolve has the same signature for every ecosystem so the
// dispatch loop in (*AnalyzeCmd).Run is a single line: the per-
// ecosystem "ask the registry, parse the response, persist the URL"
// logic lives in resolveNpmRepo / resolvePyPIRepo / etc. below.
type ecosystemResolverEntry struct {
	Match         []string
	Label         string
	AbsenceSource string
	Resolve       func(ctx context.Context, s store.Store, entity *profile.Entity, globals *Globals) error
}

// ecosystemResolvers is the dispatch table for source-of-record
// resolution. (*AnalyzeCmd).Run iterates it once per analyze
// invocation, matching entity.Ecosystem against each entry's Match
// list and calling performEcosystemResolution on the first hit.
//
// Each Match list is disjoint from the others, so order doesn't
// affect behavior — it's chosen for readability (registry order
// roughly matches the order new ecosystems were wired in).
//
// Gates on Ecosystem rather than Type by design: pre-PR0 entities
// in users' dogfood stores carry Type=EntityProject from an
// analyst-output ingest path that hardcoded the type before
// profile.EntityTypeForURI was hoisted out. The Ecosystem field is
// the load-bearing one — already stamped by the defensive backfill
// in (*AnalyzeCmd).Run — so gating on it lets resolvers fire on
// legacy mistyped rows without requiring a data migration.
//
// Adding a new ecosystem: append an entry here. resolvableEcosystems
// (used by the unsupported-ecosystem hint) is derived from this
// table, so the two cannot drift.
var ecosystemResolvers = []ecosystemResolverEntry{
	{Match: []string{"npm"}, Label: "npm", AbsenceSource: "npm-registry", Resolve: resolveNpmRepo},
	{Match: []string{"pypi"}, Label: "pypi", AbsenceSource: "pypi-registry", Resolve: resolvePyPIRepo},
	{Match: []string{"cargo", "crates"}, Label: "cargo", AbsenceSource: "cargo-registry", Resolve: resolveCargoRepo},
	{Match: []string{"gem"}, Label: "gem", AbsenceSource: "gem-registry", Resolve: resolveGemRepo},
	{Match: []string{"maven"}, Label: "maven", AbsenceSource: "maven-registry", Resolve: resolveMavenRepo},
	{Match: []string{"golang", "go"}, Label: "go-module", AbsenceSource: "goproxy", Resolve: resolveGoRepo},
}

// resolvableEcosystems is the set of pkg:<ecosystem>/ values for
// which signatory has wired a source-URL resolver. Used by the
// unsupported-ecosystem hint just before collector dispatch —
// entities whose ecosystem ISN'T in this set get a stderr note
// explaining that github + git + repofiles + openssf collectors
// won't fire (no URL → isGitHostedEntity false → silent skip
// otherwise).
//
// Derived from ecosystemResolvers so the two cannot drift; adding
// a resolver to the table automatically updates this set.
var resolvableEcosystems = func() map[string]bool {
	out := make(map[string]bool, len(ecosystemResolvers)*2)
	for _, entry := range ecosystemResolvers {
		for _, eco := range entry.Match {
			out[eco] = true
		}
	}
	return out
}()

// AnalysisDisplay wraps the runtime profile with any ingested
// analyst outputs (Layer 2 data) so a single render or JSON dump
// presents the full picture: signals (Layer 1) AND
// analyses (Layer 2).
//
// Defined in the cmd package rather than internal/profile to avoid
// coupling profile to the store's summary types — analyst outputs
// are presentation-layer enrichment, not part of the entity-profile
// data model.
type AnalysisDisplay struct {
	*profile.Profile
	AnalystOutputs []store.AnalystOutputSummary `json:"analyst_outputs,omitempty"`
	// BurnVia carries Path B's cascade context: when the displayed
	// entity isn't directly burned but a related identity is, this
	// is non-nil and the renderer surfaces "burned via owner ..."
	// instead of just "burned." Empty when the burn is direct or
	// when there's no burn at all (Profile.Burn is nil).
	BurnVia *BurnViaContext `json:"burn_via,omitempty"`
}

// BurnViaContext is the analyze-display-layer mirror of
// store.EffectiveBurnContext, kept narrow to what the renderer
// needs (Profile.Burn already carries Reason / BurnedBy / BurnedAt).
type BurnViaContext struct {
	OwnerURI string `json:"owner_uri,omitempty"`
	Role     string `json:"role,omitempty"`
}

func (cmd *AnalyzeCmd) Run(globals *Globals) error {
	ctx, stdout, stderr := cmd.applyIODefaults(globals)

	// --clone is plumbing for the --refresh path: it tells the collector
	// dispatch to ensure a current local clone before signal collection.
	// Without --refresh, --clone has nothing to plumb into — the no-refresh
	// branch returns before collectors are built. Reject the combination
	// loudly rather than silently no-op the flag, which surprised users in
	// dogfood. If you only want a clone with no signal collection, use git
	// directly.
	if cmd.Clone && !cmd.Refresh {
		return NewUsageError(fmt.Errorf(
			"--clone requires --refresh; pass --refresh, or use 'git clone' directly if you only want the clone"))
	}

	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error is not actionable

	auditLog := globals.NewAuditLogger(s)
	actor, err := identity.Current()
	if err != nil {
		return fmt.Errorf("resolve team identity: %w", err)
	}

	resolved, entity, err := cmd.resolveTargetAndEntity(ctx, s)
	if err != nil {
		return err
	}

	// Decide what to do based on cache state and --refresh.
	if !cmd.Refresh {
		return cmd.handleNonRefresh(ctx, s, entity, resolved, stdout, stderr)
	}

	// --- Refresh path: collect fresh signals. ---

	entity, created, err := cmd.prepareEntityForRefresh(ctx, s, entity, resolved, stderr)
	if err != nil {
		return err
	}

	// Source-of-record resolution: for package-form entities
	// (pkg:npm/X, pkg:pypi/X, pkg:cargo/X, pkg:gem/X, pkg:maven/X,
	// pkg:golang/X), ask the relevant registry where the source lives
	// and stamp the answer on the entity so downstream collectors
	// (github, git-local-clone, repofiles, openssf) pick it up via
	// entity.URL.
	//
	// Each ecosystem's resolver is registered in ecosystemResolvers
	// (defined above); performEcosystemResolution carries the shared
	// failure-recording + fail-loud-on-refresh contract. See the
	// godoc on those two for the full rationale (especially: why we
	// gate on Ecosystem rather than Type, and why a resolver error
	// always writes absence:repo_declaration even on the non-refresh
	// path).
	//
	// The Match lists are disjoint, so at most one entry fires per
	// invocation. break after the first match avoids redundant
	// comparisons on the rest of the table.
	for _, entry := range ecosystemResolvers {
		if !slices.Contains(entry.Match, entity.Ecosystem) {
			continue
		}
		if err := performEcosystemResolution(ctx, s, entity, globals, entry, cmd.Refresh, stderr); err != nil {
			return err
		}
		break
	}

	cmd.printPreCollectionDiagnostics(stderr, entity)

	allSignals, err := cmd.collectFreshSignals(ctx, s, entity, globals, stderr)
	if err != nil {
		return err
	}

	if err := s.AppendSignals(ctx, allSignals); err != nil {
		return fmt.Errorf("store signals: %w", err)
	}

	cmd.printStorageBreadcrumb(stderr, globals.DBPath, entity.CanonicalURI, len(allSignals))

	entity.UpdatedAt = time.Now().UTC()
	if err := s.PutEntity(ctx, entity); err != nil {
		return fmt.Errorf("update entity: %w", err)
	}

	return cmd.finalizeRefreshOutput(ctx, s, entity, allSignals, created, auditLog, actor, stdout, stderr)
}

// applyIODefaults wires command-level I/O knobs to their defaults.
// Tests inject cmd.Stdout / cmd.Stderr; production paths fall through
// to os.Stdout / os.Stderr. globals.Context, when set, carries the
// SIGINT-cancellation wiring from main(); Ctrl-C at the CLI propagates
// through the HTTP client and cancels in-flight network work. Tests
// or library callers that don't set it get a fresh background
// context.
func (cmd *AnalyzeCmd) applyIODefaults(globals *Globals) (context.Context, io.Writer, io.Writer) {
	stdout := cmd.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := cmd.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}
	return ctx, stdout, stderr
}

// resolveTargetAndEntity normalizes the user-supplied target string
// to a canonical URI and looks up any existing entity row for it.
// Composes four distinct phases:
//
//  1. Parse cmd.Target via profile.ResolveTarget — the one place
//     where free-form input crosses into stable internal identifiers.
//     Everything downstream uses resolved.CanonicalURI as the
//     lookup key.
//  2. Scheme guard: analyze is for repo: and pkg: targets only.
//     identity:, org:, and patch: URIs identify entities the system
//     stores and burns against, but no Layer-1 collection path
//     exists for them today (every collector in collectorsFor gates
//     itself out). Failing loud at the top unifies fresh-DB and
//     load-path errors with one message; the per-scheme switch in
//     prepareEntityForRefresh keeps its default branch as
//     defense-in-depth.
//  3. Default --path when --clone is set without --path: a stable
//     filestore location keyed by the resolved short-name. Matches
//     the layout used by `signatory handoff --clone-dir
//     filestore/clones/`, so a subsequent handoff reuses the same
//     directory without ceremony. Empty ShortName falls through to
//     the existing ErrPathMissing in resolveClonePath. This step
//     mutates cmd.Path; everything downstream that reads it sees
//     the resolved value.
//  4. FindEntityByURI: a matching entity means the user has
//     analyzed this target before; we reuse its UUID ID so FK
//     references stay stable. ErrNotFound becomes a nil entity
//     (caller decides whether to mint or error); other errors
//     propagate.
func (cmd *AnalyzeCmd) resolveTargetAndEntity(
	ctx context.Context,
	s store.Store,
) (*profile.ResolvedTarget, *profile.Entity, error) {
	resolved, err := profile.ResolveTarget(cmd.Target)
	if err != nil {
		return nil, nil, fmt.Errorf("parse target %q: %w", cmd.Target, err)
	}
	if resolved.Scheme != "repo" && resolved.Scheme != "pkg" {
		return nil, nil, fmt.Errorf("analyze does not yet support %q-scheme targets (got %q); "+
			"use `signatory summary %s` to view cached state",
			resolved.Scheme, resolved.CanonicalURI, resolved.CanonicalURI)
	}
	if cmd.Clone && cmd.Path == "" && resolved.ShortName != "" {
		cmd.Path = filepath.Join("filestore", "clones", resolved.ShortName)
	}
	entity, err := s.FindEntityByURI(ctx, resolved.CanonicalURI)
	if errors.Is(err, store.ErrNotFound) {
		entity = nil
	} else if err != nil {
		return nil, nil, fmt.Errorf("lookup entity: %w", err)
	}
	return resolved, entity, nil
}

// handleNonRefresh serves the cache-display path: --refresh is unset,
// so we either render whatever the store already knows about the
// target or print a "nothing cached" hint. Always returns from the
// command's perspective — the caller (Run) does `return cmd.handleNonRefresh(...)`.
//
// Three terminal states:
//
//  1. No entity row: print "no cached data" + the resolved canonical
//     URI + a nudge to use --refresh, exit 0.
//  2. Entity row exists but neither signals nor analyst outputs:
//     print "no cached signals or analyst outputs" + nudges for both
//     ways to populate (--refresh, ingest), exit 0.
//  3. Entity row with signals OR analyst outputs (either qualifies
//     as "we know things"): delegate to displayProfile.
//
// Diagnostic strings go to stderr; stdout is reserved for rendered
// output. A scripted consumer sees an empty stdout + zero exit on
// the first two cases. Write-error suppression (`_, _ =`) on the
// terminal stderr writes is deliberate: errcheck flags them
// because they're the last statement before return — explicit
// discard matches the intent.
func (cmd *AnalyzeCmd) handleNonRefresh(
	ctx context.Context,
	s store.Store,
	entity *profile.Entity,
	resolved *profile.ResolvedTarget,
	stdout, stderr io.Writer,
) error {
	if entity == nil {
		_, _ = fmt.Fprintf(stderr, "No cached data for: %s\n", cmd.Target)
		_, _ = fmt.Fprintf(stderr, "Resolved to: %s\n", resolved.CanonicalURI)
		_, _ = fmt.Fprintln(stderr, "Run with --refresh to collect signals from GitHub.")
		return nil
	}
	existingSignals, err := s.GetLatestSignals(ctx, entity.ID)
	if err != nil {
		return fmt.Errorf("read cached signals: %w", err)
	}
	analystOutputs, err := cmd.fetchAnalystOutputs(ctx, s, entity.ID)
	if err != nil {
		return fmt.Errorf("read analyst outputs: %w", err)
	}
	if len(existingSignals) == 0 && len(analystOutputs) == 0 {
		_, _ = fmt.Fprintf(stderr, "No cached signals or analyst outputs for: %s\n", cmd.Target)
		_, _ = fmt.Fprintln(stderr, "Run with --refresh to collect signals from GitHub,")
		_, _ = fmt.Fprintln(stderr, "or run `signatory ingest <file>` to load an analyst output.")
		return nil
	}
	return cmd.displayProfile(ctx, s, entity, analystOutputs, stdout)
}

// prepareEntityForRefresh runs the pre-collection burn gate and
// ensures an entity row exists for the resolved target with its
// Ecosystem and URL fields stamped. Returns the prepared entity,
// a flag indicating whether this run minted it (used for the
// audit log's `created_entity` field), or an error if the burn
// gate fires or entity creation fails.
//
// Three kinds of work:
//
//  1. Burn gate: if the URI is burned and --ignore-burn is not set,
//     return formatBurnGateError. EffectiveBurnByURI walks both
//     URI-derived and signal-derived candidates, so the gate fires
//     whether or not we've analyzed this target before. Withdrawing
//     the burn (signatory burn remove) is the cleaner path when
//     the burn turns out to have been premature — that re-opens
//     analyze for everyone, not just this invocation.
//
//  2. Create-on-miss: when entity is nil, mint a fresh row with
//     scheme-appropriate Type/ShortName/URL/Ecosystem. The
//     unsupported-scheme `default` branch stays as defense-in-depth
//     even though the top-of-Run guard already rejects non-repo,
//     non-pkg schemes.
//
//  3. Defensive backfill: stale rows whose Ecosystem or URL are
//     empty but derivable from the resolved target get patched and
//     persisted. The Ecosystem backfill closes the 2026-04-28 idna
//     refresh meltdown (resolvers gate on Ecosystem); the URL
//     backfill closes the 2026-04-30 kong dogfood (collectors gate
//     on isGitHostedEntity, which checks URL). Persistence errors
//     warn to stderr but don't fail the run — the in-memory entity
//     carries correct values through the rest of the invocation.
func (cmd *AnalyzeCmd) prepareEntityForRefresh(
	ctx context.Context,
	s store.Store,
	entity *profile.Entity,
	resolved *profile.ResolvedTarget,
	stderr io.Writer,
) (*profile.Entity, bool, error) {
	if !cmd.IgnoreBurn {
		gateBurn, gateCtx, gateErr := s.EffectiveBurnByURI(ctx, resolved.CanonicalURI)
		if gateErr != nil && !errors.Is(gateErr, store.ErrNotFound) {
			return nil, false, fmt.Errorf("pre-collection burn gate: %w", gateErr)
		}
		if gateErr == nil {
			return nil, false, formatBurnGateError(resolved.CanonicalURI, gateBurn, gateCtx)
		}
	}

	created := false
	if entity == nil {
		entity = &profile.Entity{
			ID:           profile.NewEntityID(),
			CanonicalURI: resolved.CanonicalURI,
			// Type derives from the URI scheme via the shared helper —
			// single source of truth across analyze, posture set, and
			// analyst-output ingest. The per-scheme switch below sets
			// the OTHER fields (ShortName, URL, Ecosystem) that vary
			// by scheme; the unsupported-scheme error case stays as
			// analyze's narrower contract (only repo: and pkg: are
			// wired into Layer-1 collection today).
			Type:      profile.EntityTypeForScheme(resolved.Scheme),
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}
		switch resolved.Scheme {
		case "repo":
			entity.ShortName = resolved.Owner + "/" + resolved.ShortName
			entity.URL = resolved.CloneURL
		case "pkg":
			entity.Ecosystem = resolved.Ecosystem
			// ShortName is the full package name (scope-preserving
			// for npm), not the last path segment — "@types/node",
			// not "node". ResolvedTarget.ShortName drops the scope
			// for its own reasons; reconstruct here.
			entity.ShortName = strings.TrimPrefix(
				resolved.CanonicalURI, "pkg:"+resolved.Ecosystem+"/")
			// Stamp URL from the parse-time CloneURL when available.
			// For pkg:golang/github.com/* and pkg:golang/golang.org/x/*
			// (and pkg:go aliases), ResolveTarget derives a github URL
			// algorithmically — no network. Empty for ecosystems that
			// resolve via network (npm, pypi) or for vanity Go hosts
			// without algorithmic mapping (gopkg.in, modernc.org, …);
			// resolveNpmRepo / resolvePyPIRepo still populate URL on
			// --refresh for those.
			entity.URL = resolved.CloneURL
		default:
			return nil, false, fmt.Errorf("analyze does not yet support %q-scheme targets (got %q)",
				resolved.Scheme, resolved.CanonicalURI)
		}
		if err := s.PutEntity(ctx, entity); err != nil {
			return nil, false, fmt.Errorf("create entity: %w", err)
		}
		created = true
	}

	if entity.Ecosystem == "" && resolved.Ecosystem != "" {
		entity.Ecosystem = resolved.Ecosystem
		entity.UpdatedAt = time.Now().UTC()
		if err := s.PutEntity(ctx, entity); err != nil {
			_, _ = fmt.Fprintf(stderr, "warning: backfill ecosystem on %s failed: %v\n",
				entity.CanonicalURI, err)
		}
	}

	if entity.URL == "" && resolved.CloneURL != "" {
		entity.URL = resolved.CloneURL
		entity.UpdatedAt = time.Now().UTC()
		if err := s.PutEntity(ctx, entity); err != nil {
			_, _ = fmt.Fprintf(stderr, "warning: backfill URL on %s failed: %v\n",
				entity.CanonicalURI, err)
		}
	}

	return entity, created, nil
}

// collectFreshSignals builds the per-target collector set and runs
// each one against the prepared entity, returning the flat slice of
// signals to persist. Tests inject mocks via globals.Collectors (see
// functional_test.go); in production that field is empty and the
// list is built per-target by collectorsFor based on the entity's
// shape plus --path / --clone.
//
// inRunResult accumulates signals as collectors complete so that
// later collectors can read earlier collectors' emissions in the
// same run. Currently only the source-evolution collector consumes
// this — it reads gopublish's version_pin_table to anchor matrix
// rows to commit SHAs. The pointer is captured at construction
// time so subsequent mutations inside the loop are visible to
// source-evolution's Collect.
//
// .Collected (not .Signals()) is appended to inRunResult because
// Signals() flattens absences for storage, but the in-run lookup
// wants the full record set.
func (cmd *AnalyzeCmd) collectFreshSignals(
	ctx context.Context,
	s store.Store,
	entity *profile.Entity,
	globals *Globals,
	stderr io.Writer,
) ([]profile.Signal, error) {
	inRunResult := &signal.CollectionResult{}

	// cleanups carries deferred-cleanup callbacks registered by
	// resolveClonePath (specifically the tempdir-removal for the
	// file-vector clone-isolation defense — Fix #2 from
	// design/analysis/cve-2025-41390.md). Drained in LIFO order
	// after every collector finishes, regardless of success or
	// failure of the collector loop.
	var cleanups []func() error
	defer func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			if err := cleanups[i](); err != nil {
				_, _ = fmt.Fprintf(stderr, "warning: post-collect cleanup: %v\n", err)
			}
		}
	}()

	collectors := globals.Collectors
	if len(collectors) == 0 {
		c, err := collectorsFor(ctx, entity, CollectOpts{
			Path:        cmd.Path,
			Clone:       cmd.Clone,
			RunGit:      cmd.RunGit,
			Stderr:      stderr,
			AllowFetch:  cmd.AllowFetch,
			InRunResult: inRunResult,
			Store:       s,
			// Same concrete *store.SQLite value as Store, viewed
			// through the narrow ghcollector.EntityStore interface
			// so the github collector can mint identity:/org: rows
			// for repo owners (Path A; entity-burn1.md §3.1).
			EntityStore: s,
			// File-vector clone-isolation cleanup registry. See
			// CollectOpts.Cleanups doc for the threat model.
			Cleanups: &cleanups,
		})
		if err != nil {
			return nil, err
		}
		collectors = c
	}

	var allSignals []profile.Signal
	for _, collector := range collectors {
		result, err := collector.Collect(ctx, entity)
		if err != nil {
			return nil, fmt.Errorf("collect signals (%s): %w", collector.Name(), err)
		}
		inRunResult.Collected = append(inRunResult.Collected, result.Collected...)
		allSignals = append(allSignals, result.Signals()...)
		_, _ = fmt.Fprintf(stderr, "[%s] %s\n", collector.Name(), result.Summary())
	}
	return allSignals, nil
}

// printStorageBreadcrumb writes the post-AppendSignals user trail to
// stderr: where the signals went, how to query them, and (if a
// clone landed) where to find it. Names the resolved DB path so a
// manual `signatory analyze --refresh` invocation hands the user
// the next thread to pull (signatory show-conclusions, or a direct
// sqlite3 inspection of the file).
//
// ResolvePath errors are swallowed: the resolution can't fail in
// any way that matters AFTER a successful AppendSignals against
// the same DBPath, so a warning would be noise — fall back to
// just naming the count.
//
// The inspect-clone hint only surfaces when a clone actually
// exists. --clone may have been requested but never executed
// (vanity-host Go module with no derivable github source: the
// dispatch gate keeps the git-side collectors out, ensureCloneAtPath
// never runs). The .git probe is the honest test — promising a
// clone path that isn't there sends the user on a wild goose chase.
func (cmd *AnalyzeCmd) printStorageBreadcrumb(
	stderr io.Writer,
	dbPathRaw, canonicalURI string,
	signalCount int,
) {
	if dbPath, perr := store.ResolvePath(dbPathRaw); perr == nil {
		_, _ = fmt.Fprintf(stderr, "Stored %d signals in %s\n", signalCount, dbPath)
	} else {
		_, _ = fmt.Fprintf(stderr, "Stored %d signals\n", signalCount)
	}
	_, _ = fmt.Fprintf(stderr,
		"  query: signatory show-conclusions --target %s\n", canonicalURI)
	if cmd.Clone && cmd.Path != "" {
		if absClone, aerr := filepath.Abs(cmd.Path); aerr == nil {
			if _, gitErr := os.Stat(filepath.Join(absClone, ".git")); gitErr == nil {
				_, _ = fmt.Fprintf(stderr, "  inspect clone: %s\n", absClone)
			}
		}
	}
}

// printPreCollectionDiagnostics writes the three operator-facing
// stderr nudges that fire after entity preparation but before the
// collector loop:
//
//  1. Unsupported-ecosystem hint: when the entity carries a known
//     ecosystem string but no resolver is wired for it, every
//     git-side collector (github, git, repofiles, openssf) silently
//     sits out (no URL → isGitHostedEntity false). Without this
//     hint the user sees zero collected signals and has no idea
//     why. Closes the silent-skip path identified in
//     design/openssf-problem.txt. Distinct from the
//     resolver-failure case, which writes absence:repo_declaration
//     via performEcosystemResolution; this is "we didn't try
//     because no resolver exists." Gates on Ecosystem rather than
//     Type for the same reason resolveGoRepo does (pre-fix
//     EntityProject rows from MCP ingest paths).
//
//  2. Go-module-via-repo-form hint: maybeWarnGoModuleViaRepoForm
//     nudges users analyzing a Go module via its github URL form
//     toward the canonical pkg:golang/<modpath> form, which gets
//     the gopublish collector. No-op for non-Go-module repos and
//     for entities already using pkg:golang.
//
//  3. GITHUB_TOKEN absence warning: a missing token doesn't fail
//     the run, but the github collector hits a much lower rate
//     limit unauthenticated. Surface so users aren't surprised
//     when API-derived signals are sparse.
//
// Followed by the "Collecting signals for: <URI>" header that
// frames the collector loop's per-line summaries.
func (cmd *AnalyzeCmd) printPreCollectionDiagnostics(stderr io.Writer, entity *profile.Entity) {
	if entity.Ecosystem != "" &&
		entity.URL == "" &&
		!resolvableEcosystems[entity.Ecosystem] {
		_, _ = fmt.Fprintf(stderr,
			"note: ecosystem %q has no source resolver yet — github + git + repofiles + openssf collectors won't fire for this target.\n",
			entity.Ecosystem)
	}

	maybeWarnGoModuleViaRepoForm(stderr, entity, cmd.Path)

	if os.Getenv("GITHUB_TOKEN") == "" {
		_, _ = fmt.Fprintf(stderr, "warning: GITHUB_TOKEN not set — GitHub API signals may be absent or rate-limited\n")
	}

	_, _ = fmt.Fprintf(stderr, "Collecting signals for: %s\n", entity.CanonicalURI)
}

// finalizeRefreshOutput closes out the refresh path: writes the
// audit-log line, fetches any cached analyst outputs (Layer 2 — not
// touched by the Layer 1 collectors that just ran), and renders the
// combined profile to stdout.
//
// Audit failure is non-fatal: the signals are already in the store
// at this point, and a missing audit line is a secondary
// observability concern, not a correctness failure. The warning
// goes to stderr so an operator notices.
//
// fetchAnalystOutputs failure IS fatal — if the store can't read
// what it should be able to read, something is wrong enough to
// warrant a loud exit rather than a half-rendered display.
//
// The blank stderr line before display separates the progress
// stream from the upcoming rendered output. On a terminal that
// interleaves both, this divides diagnostic chatter from data; on
// a pipe (--json | jq), stdout stays clean JSON regardless.
func (cmd *AnalyzeCmd) finalizeRefreshOutput(
	ctx context.Context,
	s store.Store,
	entity *profile.Entity,
	allSignals []profile.Signal,
	created bool,
	auditLog *audit.Logger,
	actor string,
	stdout, stderr io.Writer,
) error {
	detail, _ := json.Marshal(map[string]any{
		"target":            cmd.Target,
		"canonical_uri":     entity.CanonicalURI,
		"signals_collected": len(allSignals),
		"created_entity":    created,
	})
	if err := auditLog.LogAction(ctx, actor, "analyze", entity.ID, string(detail)); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: audit log write failed: %v\n", err)
	}

	analystOutputs, err := cmd.fetchAnalystOutputs(ctx, s, entity.ID)
	if err != nil {
		return fmt.Errorf("read analyst outputs (post-refresh): %w", err)
	}

	_, _ = fmt.Fprintln(stderr)
	return cmd.displayProfile(ctx, s, entity, analystOutputs, stdout)
}

// fetchAnalystOutputs returns the AnalystOutput summaries for an
// entity, respecting the --max-age filter when set. Newest-ingested
// first.
//
// This is the core of the freshness check: an agent invoking
// `signatory analyze` should be able to see, at a glance, what's
// been ingested for the target and how recently — without having
// to fall back to `signatory show-analyses` or grep design/analysis/.
func (cmd *AnalyzeCmd) fetchAnalystOutputs(
	ctx context.Context, s store.Store, entityID string,
) ([]store.AnalystOutputSummary, error) {
	filter := store.AnalystOutputFilter{EntityID: entityID}
	if cmd.MaxAge > 0 {
		filter.Since = time.Now().Add(-cmd.MaxAge)
	}
	return s.ListAnalystOutputs(ctx, filter)
}

// displayProfile reads the current-state view for an entity and
// renders it to w (typically stdout). Uses GetLatestSignals so
// superseded signals are filtered out; uses GetPostures to show the
// latest posture plus a hint when multiple versions have recorded
// decisions.
//
// analystOutputs (typically from fetchAnalystOutputs) is woven into
// both the JSON and human-readable presentations. Pass nil if no
// outputs should be surfaced (e.g., for a profile-only display).
//
// The writer parameter is load-bearing: `--json` writes nothing but
// the JSON payload to w, so a caller piping to jq gets a clean
// parseable document. Diagnostic output from AnalyzeCmd.Run lands on
// the separate stderr stream before displayProfile is invoked.
func (cmd *AnalyzeCmd) displayProfile(
	ctx context.Context, s store.Store, entity *profile.Entity,
	analystOutputs []store.AnalystOutputSummary,
	w io.Writer,
) error {
	signals, err := s.GetLatestSignals(ctx, entity.ID)
	if err != nil {
		return fmt.Errorf("read signals: %w", err)
	}

	postures, err := s.GetPostures(ctx, entity.ID)
	if err != nil {
		return fmt.Errorf("read postures: %w", err)
	}

	// EffectiveBurn surfaces direct OR cascaded burns (Path B).
	// When the cascade fires, the rendering shows "via owner ..."
	// so users know which ledger entry caused the degradation.
	burn, ebCtx, err := s.EffectiveBurn(ctx, entity.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("get burn: %w", err)
	}

	p := &profile.Profile{
		Entity:   *entity,
		Signals:  signals,
		Postures: postures,
		Burn:     burn,
	}
	var burnVia *BurnViaContext
	if ebCtx != nil && !ebCtx.Direct && ebCtx.ViaOwner != nil {
		burnVia = &BurnViaContext{
			OwnerURI: ebCtx.ViaOwner.CanonicalURI,
			Role:     ebCtx.ViaRole,
		}
	}
	if len(postures) > 0 {
		// Postures are returned newest-first; highlight the latest as
		// the "current" posture for backward-compat display.
		latest := postures[0]
		p.Posture = &latest
	}

	display := &AnalysisDisplay{
		Profile:        p,
		AnalystOutputs: analystOutputs,
		BurnVia:        burnVia,
	}

	if cmd.JSON {
		data, err := json.MarshalIndent(display, "", "  ")
		if err != nil {
			return err
		}
		// Single final write: explicit error check instead of a
		// stickyWriter (which earns its keep on multi-write paths
		// like displayHuman). Broken-pipe on the JSON payload —
		// caller did `analyze --json … | head -c 100` — propagates
		// up so the shell sees exit != 0 and scripts can react.
		if _, err := fmt.Fprintln(w, string(data)); err != nil {
			return fmt.Errorf("write json output: %w", err)
		}
		return nil
	}

	return displayHuman(w, display, cmd.MaxAge)
}

// displayHuman writes a human-readable entity profile to w,
// including any analyst outputs surfaced by the freshness check.
// maxAge is passed in only for display ("Cached analyses (last %s):")
// — the filtering itself happened at fetch time.
//
// All output goes through the writer — no global os.Stdout
// references — so tests can inject per-call buffers and parallel
// tests stay race-free.
//
// Write errors propagate: once any individual Writef/Writeln fails
// (broken pipe is the realistic case — `analyze … | head -5`), the
// stickyWriter short-circuits the remaining calls and the first
// error surfaces via the function's return. That's the whole reason
// for the sticky wrapper: without it, a broken pipe mid-render
// would silently waste CPU on ~30 more format calls that go
// nowhere.
func displayHuman(w io.Writer, d *AnalysisDisplay, maxAge time.Duration) error {
	p := d.Profile
	sw := &stickyWriter{w: w}

	sw.Writef("Entity:    %s\n", p.Entity.ShortName)
	sw.Writef("URI:       %s\n", p.Entity.CanonicalURI)
	sw.Writef("Type:      %s\n", p.Entity.Type)
	if p.Entity.Description != "" {
		sw.Writef("Note:      %s\n", p.Entity.Description)
	}
	if p.Entity.Ecosystem != "" {
		sw.Writef("Ecosystem: %s\n", p.Entity.Ecosystem)
	}
	sw.Writeln()

	// Surface ingested analyst outputs before signals — they're
	// usually the higher-information-density artifact a human or
	// agent wants to see first ("we ran security review 3 days
	// ago, here's the headline").
	if len(d.AnalystOutputs) > 0 {
		header := "=== Cached analyses ==="
		if maxAge > 0 {
			header = fmt.Sprintf("=== Cached analyses (last %s) ===", maxAge)
		}
		sw.Writeln(header)
		for _, ao := range d.AnalystOutputs {
			ageStr := analystOutputAge(ao.IngestedAt)
			sw.Writef("  %s  %s round=%d  %s\n",
				ao.OutputID[:8], ao.AnalystID, ao.Round, ageStr)
			sw.Writef("    model=%s  ingested=%s\n",
				ao.Model, ao.IngestedAt)
			sw.Writef("    %d conclusion(s), %d positive absence(s), %d observation(s), %d methodology pattern(s)\n",
				ao.ConclusionsCount, ao.PositiveAbsenceCount,
				ao.ObservationCount, ao.PatternCount)
			if ao.SourcePath != "" {
				sw.Writef("    source: %s\n", ao.SourcePath)
			}
		}
		sw.Writef("Use `signatory show-conclusions --target %s` for cross-output conclusion queries.\n",
			p.Entity.CanonicalURI)
		sw.Writeln()
	}

	// Posture: show latest + hint about other versions.
	if len(p.Postures) > 0 {
		latest := p.Postures[0]
		if latest.Version != "" {
			sw.Writef("Posture:   %s (version %s)\n", latest.Tier, latest.Version)
		} else {
			sw.Writef("Posture:   %s\n", latest.Tier)
		}
		sw.Writef("Rationale: %s\n", latest.Rationale)
		sw.Writef("Set by:    %s\n", latest.SetBy)
		if len(p.Postures) > 1 {
			sw.Writef("           (%d other version%s recorded — `signatory posture get %s --all` to see all)\n",
				len(p.Postures)-1, pluralS(len(p.Postures)-1), p.Entity.CanonicalURI)
		}
		sw.Writeln()
	}

	if p.Burn != nil {
		// Shared formatter (cmd/signatory/burn_display.go); see
		// summary.go and show.go for the parallel uses. Empty
		// BurnVia falls through to the direct form via the
		// formatter's internal check.
		var viaURI, viaRole string
		if d.BurnVia != nil {
			viaURI = d.BurnVia.OwnerURI
			viaRole = d.BurnVia.Role
		}
		sw.Writef("%s\n", formatBurnLine(burnDisplayInput{
			Reason:      p.Burn.Reason,
			BurnedBy:    p.Burn.BurnedBy,
			BurnedAt:    p.Burn.BurnedAt,
			ViaOwnerURI: viaURI,
			ViaRole:     viaRole,
		}))
		sw.Writeln()
	}

	// Group signals for display.
	groups := map[profile.SignalGroup][]profile.Signal{}
	for _, s := range p.Signals {
		groups[s.Group] = append(groups[s.Group], s)
	}

	groupOrder := []struct {
		group profile.SignalGroup
		label string
	}{
		{profile.SignalGroupVitality, "Vitality"},
		{profile.SignalGroupGovernance, "Governance"},
		{profile.SignalGroupPublication, "Publication Integrity"},
		{profile.SignalGroupHygiene, "Hygiene"},
		{profile.SignalGroupCriticality, "Criticality"},
		{profile.SignalGroupPosture, "Posture"},
	}

	// Collected once during the per-group walk so the consolidated
	// "=== Absences ===" section below can render without a second
	// pass over p.Signals. Each entry preserves what the in-group
	// row already shows, plus the structured retryable bit so the
	// summary section can annotate "(retryable)" without re-parsing
	// the JSON value.
	type absenceRow struct {
		name      string
		reason    string
		retryable bool
	}
	var absences []absenceRow
	absenceCount := 0
	for _, g := range groupOrder {
		sigs, ok := groups[g.group]
		if !ok {
			continue
		}
		sw.Writef("=== %s ===\n", g.label)
		for _, s := range sigs {
			// Render-path unmarshal: a corrupt Signal.Value should not
			// abort the whole display. On decode failure val stays nil
			// and the downstream type-assertion guards (`if r, ok := …`)
			// render an empty row rather than crashing. The store is
			// the canonical source for the raw bytes; rendering
			// degrades gracefully.
			var val map[string]any
			_ = json.Unmarshal(s.Value, &val) //nolint:errcheck // see comment above: nil-safe render on decode failure

			if name, ok := strings.CutPrefix(s.Type, "absence:"); ok {
				absenceCount++
				retryable := false
				if r, ok := val["retryable"].(bool); ok {
					retryable = r
				}
				reason := ""
				if r, ok := val["reason"].(string); ok {
					reason = r
				}
				absences = append(absences, absenceRow{
					name: name, reason: reason, retryable: retryable,
				})
				retryStr := ""
				if retryable {
					retryStr = " (retryable)"
				}
				sw.Writef("  %-20s [ABSENT]  %s%s\n", name, reason, retryStr)
			} else {
				sw.Writef("  %-20s [%s]  ", s.Type, s.ForgeryResistance)
				printCompactValue(sw, val)
				sw.Writeln()
			}
		}
		sw.Writeln()
	}

	// Consolidated absences section: surfaces every [ABSENT] marker
	// in one place at the bottom of the display, so a user scanning
	// the output doesn't have to walk every group looking for them.
	// The in-group rows above still show absences alongside their
	// peers (preserves Governance vs Criticality vs Vitality semantic
	// context); this section is the consolidated capstone.
	//
	// Skipped when there are zero absences — an empty "=== Absences ==="
	// header would be visual noise on every clean target.
	if len(absences) > 0 {
		sw.Writeln("=== Absences ===")
		for _, a := range absences {
			retryStr := ""
			if a.retryable {
				retryStr = " (retryable)"
			}
			sw.Writef("  %-30s %s%s\n", a.name, a.reason, retryStr)
		}
		sw.Writeln()
	}

	sw.Writef("Total signals: %d (%d absent)\n", len(p.Signals), absenceCount)
	return sw.Err()
}

// analystOutputAge produces a human-friendly relative-age string
// for an AnalystOutput's ingested_at timestamp, e.g. "3 days ago",
// "2 weeks ago". Falls back to the raw timestamp on parse error so
// the display never breaks on a malformed value.
func analystOutputAge(ingestedAt string) string {
	t, err := time.Parse(time.RFC3339, ingestedAt)
	if err != nil {
		return "(" + ingestedAt + ")"
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dw ago", int(d.Hours()/(24*7)))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/(24*30)))
	default:
		return fmt.Sprintf("%dy ago", int(d.Hours()/(24*365)))
	}
}

// printCompactValue writes a signal's value map as compact
// key=value pairs to sw. Keys are sorted so the same signal renders
// identically across runs — Go map iteration is randomized, and
// nondeterministic order bites anyone diffing captured output or
// eyeballing analyze runs for drift.
//
// Takes a *stickyWriter so it participates in the displayHuman
// error chain: if a prior write in the enclosing render errored,
// the format calls here become no-ops instead of racing to append
// garbage to a closed stream.
func printCompactValue(sw *stickyWriter, val map[string]any) {
	for i, k := range slices.Sorted(maps.Keys(val)) {
		if i > 0 {
			sw.Writef(", ")
		}
		sw.Writef("%s=%v", k, val[k])
	}
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// stickyWriter is a sticky-error wrapper around an io.Writer, used
// by the display functions so a broken-pipe failure partway through
// rendering short-circuits the remaining writes instead of wasting
// work on a closed file descriptor.
//
// The concrete scenario: a user runs `signatory analyze foo | head
// -5`. After `head` closes its end of the pipe, every subsequent
// write to stdout returns a broken-pipe error. Without this wrapper,
// we'd silently continue formatting ~30 lines of irrelevant output;
// with it, the first error marks the writer failed, all later
// Writef/Writeln calls no-op, and the caller returns the error.
//
// Modeled on bufio.Writer's sticky-error internal behavior (see
// Go src/bufio/bufio.go): once an error is captured, subsequent
// writes are no-ops and the error is preserved until Flush/check.
//
// Design note: this wrapper exists in lieu of per-call-site
// `if _, err := fmt.Fprintf(w, ...); err != nil { return err }`
// boilerplate, which would triple the LOC in displayHuman and
// bury the formatting intent.
type stickyWriter struct {
	w   io.Writer
	err error
}

// Writef formats and writes to the underlying writer. If a previous
// call errored, this is a no-op and the stored error is preserved.
func (s *stickyWriter) Writef(format string, args ...any) {
	if s.err != nil {
		return
	}
	_, s.err = fmt.Fprintf(s.w, format, args...)
}

// Writeln formats and writes (with trailing newline). If a previous
// call errored, this is a no-op and the stored error is preserved.
func (s *stickyWriter) Writeln(args ...any) {
	if s.err != nil {
		return
	}
	_, s.err = fmt.Fprintln(s.w, args...)
}

// Err returns the first error encountered, or nil.
func (s *stickyWriter) Err() error {
	return s.err
}

// performEcosystemResolution runs one entry from ecosystemResolvers
// and records the outcome. On resolver success it returns nil; on
// failure it always writes an absence:repo_declaration signal with
// retryable=true so the stored profile carries a machine-readable
// "we tried and the registry failed" marker — distinct from the
// "no declared repo" case (resolver returns nil with empty URL)
// and from "we never tried" (no resolver wired for this ecosystem).
//
// Behavior split on refresh:
//
//   - --refresh: warn to stderr AND return a wrapped error so the
//     caller fails loud. The user asked for fresh data; we cannot
//     silently decline.
//   - non-refresh: warn to stderr only. Analysis continues with
//     whatever signals the registry-side collector can still
//     produce on its own.
//
// stderr writes intentionally use `_, _ =` discard: these are
// diagnostic progress lines, not the command's contract output.
// A failure to write them (stderr closed, broken pipe) doesn't
// change what we should report.
func performEcosystemResolution(
	ctx context.Context,
	s store.Store,
	entity *profile.Entity,
	globals *Globals,
	entry ecosystemResolverEntry,
	refresh bool,
	stderr io.Writer,
) error {
	resolveErr := entry.Resolve(ctx, s, entity, globals)
	if resolveErr == nil {
		return nil
	}

	absenceSig := signal.MakeAbsence(
		entity.ID,
		"repo_declaration",
		entry.AbsenceSource,
		resolveErr.Error(),
		true, // retryable: transient registry failure or attacker-controlled response
		time.Now().UTC(),
	)
	_ = s.AppendSignals(ctx, []profile.Signal{absenceSig.ToSignal()}) //nolint:errcheck // best-effort; primary error is resolveErr

	_, _ = fmt.Fprintf(stderr, "warning: %s repo resolution for %s failed: %v\n",
		entry.Label, entity.CanonicalURI, resolveErr)
	if refresh {
		return fmt.Errorf("refresh %s repo resolution for %s: %w",
			entry.Label, entity.CanonicalURI, resolveErr)
	}
	return nil
}

// resolveNpmRepo asks the npm registry for the package's declared
// repository URL, normalizes it to a github clone URL (empty if the
// package doesn't declare one or declares a non-github host), and
// stamps the result on the entity. Persists the entity update so
// subsequent reads see the resolved URL.
//
// Lives in analyze.go rather than inside the npm collector by
// design: the provider answers the "where is this package's
// source?" question, the orchestrator records it, and downstream
// collectors work against the resolved entity. Keeping the
// provider out of the collector prevents the collector's tight
// loop (1 call per signal it emits) from bleeding
// into orchestration (1 call per analyze invocation).
func resolveNpmRepo(ctx context.Context, s store.Store, entity *profile.Entity, globals *Globals) error {
	packageName := strings.TrimPrefix(entity.CanonicalURI, "pkg:npm/")
	if packageName == "" || packageName == entity.CanonicalURI {
		return fmt.Errorf("entity %q is not an npm package URI", entity.CanonicalURI)
	}

	client := npmregistry.NewClient()
	if globals != nil && globals.NpmRegistryURL != "" {
		client = npmregistry.NewClientWithBaseURL(globals.NpmRegistryURL)
	}

	repoURL, err := client.ResolveRepoURL(ctx, packageName)
	if err != nil {
		return fmt.Errorf("query npm registry: %w", err)
	}
	if repoURL == "" {
		// Package doesn't declare a github-hosted repository. Nothing
		// to stamp; stay silent. Downstream dispatch will skip the
		// github + git collectors via isGitHostedEntity.
		return nil
	}

	entity.URL = repoURL
	entity.UpdatedAt = time.Now().UTC()
	if err := s.PutEntity(ctx, entity); err != nil {
		return fmt.Errorf("persist resolved URL on entity: %w", err)
	}
	return nil
}

// maybeWarnGoModuleViaRepoForm emits a stderr hint when the user
// is analyzing a Go module via its github URL form
// (`repo:github/X/Y`) instead of the canonical
// `pkg:golang/<modpath>` form. The repo form misses go-publish
// provenance signals because the gopublish collector's filter
// is `entity.Ecosystem in {"golang","go"}` and repo-scheme
// entities have no Ecosystem set.
//
// Triggers iff:
//
//   - entity uses the `repo:github/` scheme prefix
//   - clonePath is non-empty and contains a parseable go.mod
//   - the parsed go.mod declares a module path
//
// Otherwise: no-op. Informational; never blocks analysis. The
// proper fix (URI canonicalization at resolve time) requires
// network-dependent resolver work and is deferred — this is
// the cheap nudge that surfaces the gap to users today.
//
// Closes Gap 6c from the 2026-04-28 ms dogfood audit; see
// design/dogfood-errors.md (when it lands) for the trade-off
// discussion that landed on "document and nudge" rather than
// "rewrite the resolver."
func maybeWarnGoModuleViaRepoForm(stderr io.Writer, entity *profile.Entity, clonePath string) {
	if entity == nil || clonePath == "" {
		return
	}
	if !strings.HasPrefix(entity.CanonicalURI, "repo:github/") {
		return
	}
	info, _, err := gomod.Parse(filepath.Join(clonePath, "go.mod"))
	if err != nil || info.Name == "" {
		return
	}
	_, _ = fmt.Fprintf(stderr,
		"note: %s declares Go module %q; analyze pkg:golang/%s for full Go-publish provenance signals\n",
		entity.CanonicalURI, info.Name, info.Name)
}

// resolvePyPIRepo is the PyPI parallel to resolveNpmRepo. Same
// shape: query the registry for a project, walk its declared
// project_urls (and the legacy home_page fallback) for a github
// source, normalize, stamp on the entity, persist.
//
// Failure modes match the npm equivalent: registry-fetch errors
// (network, 5xx, body cap) return wrapped errors so the caller's
// --refresh path can fail loud. Returns nil with entity.URL
// unchanged when the project has no resolvable github source —
// distinct from a fetch failure, lets downstream isGitHostedEntity
// gracefully skip the github + git collectors.
//
// Lives in analyze.go for the same reason resolveNpmRepo does:
// repo resolution is orchestration, not signal collection. The
// pypi package's ResolveRepoURL handles the project_urls priority
// walk and github-URL normalization.
func resolvePyPIRepo(ctx context.Context, s store.Store, entity *profile.Entity, globals *Globals) error {
	packageName := strings.TrimPrefix(entity.CanonicalURI, "pkg:pypi/")
	if packageName == "" || packageName == entity.CanonicalURI {
		return fmt.Errorf("entity %q is not a pypi package URI", entity.CanonicalURI)
	}

	client := pypiregistry.NewClient()
	if globals != nil && globals.PypiRegistryURL != "" {
		client = pypiregistry.NewClientWithBaseURL(globals.PypiRegistryURL)
	}

	repoURL, err := client.ResolveRepoURL(ctx, packageName)
	if err != nil {
		return fmt.Errorf("query pypi registry: %w", err)
	}
	if repoURL == "" {
		// No github-hosted repository declared in project_urls or
		// home_page. Stay silent; downstream dispatch will skip the
		// github + git collectors via isGitHostedEntity.
		return nil
	}

	entity.URL = repoURL
	entity.UpdatedAt = time.Now().UTC()
	if err := s.PutEntity(ctx, entity); err != nil {
		return fmt.Errorf("persist resolved URL on entity: %w", err)
	}
	return nil
}

// resolveCargoRepo asks crates.io for the crate's declared repository
// URL, normalizes it to a github clone URL (empty if not declared or
// non-github), and stamps the result on the entity. Persists the
// entity update so subsequent reads see the resolved URL.
//
// Parallel to resolveNpmRepo / resolvePyPIRepo: the provider answers
// the "where is this crate's source?" question, the orchestrator
// records it, and downstream collectors work against the resolved
// entity. The crate's Repository field in the crates.io API response
// is publisher-declared (self-reported, not cryptographically bound).
//
// Package name extraction handles both pkg:cargo/ and pkg:crates/
// prefixes (the latter is the ecosystem alias from detect.go).
func resolveCargoRepo(ctx context.Context, s store.Store, entity *profile.Entity, globals *Globals) error {
	packageName := entity.CanonicalURI
	for _, prefix := range []string{"pkg:cargo/", "pkg:crates/"} {
		if rest, ok := strings.CutPrefix(packageName, prefix); ok {
			packageName = rest
			break
		}
	}
	if packageName == "" || packageName == entity.CanonicalURI {
		return fmt.Errorf("entity %q is not a cargo package URI", entity.CanonicalURI)
	}

	client := cargoregistry.NewClient()
	if globals != nil && globals.CargoRegistryURL != "" {
		client = cargoregistry.NewClientWithBaseURL(globals.CargoRegistryURL)
	}

	repoURL, err := client.ResolveRepoURL(ctx, packageName)
	if err != nil {
		return fmt.Errorf("query crates.io registry: %w", err)
	}
	if repoURL == "" {
		// Crate doesn't declare a github-hosted repository. Nothing
		// to stamp; stay silent. Downstream dispatch will skip the
		// github + git collectors via isGitHostedEntity.
		return nil
	}

	entity.URL = repoURL
	entity.UpdatedAt = time.Now().UTC()
	if err := s.PutEntity(ctx, entity); err != nil {
		return fmt.Errorf("persist resolved URL on entity: %w", err)
	}
	return nil
}

// resolveGemRepo asks rubygems.org for the gem's declared source
// repository URL (source_code_uri, falling back to homepage_uri),
// normalizes it to a github clone URL (empty if not declared or
// non-github), and stamps the result on the entity. Persists the
// entity update so subsequent reads see the resolved URL.
//
// Parallel to resolveNpmRepo / resolvePyPIRepo / resolveCargoRepo:
// the provider answers the "where is this gem's source?" question, the
// orchestrator records it, and downstream collectors work against the
// resolved entity. The gem's source_code_uri field in the rubygems.org
// API response is publisher-declared (self-reported, not
// cryptographically bound).
func resolveGemRepo(ctx context.Context, s store.Store, entity *profile.Entity, globals *Globals) error {
	packageName := strings.TrimPrefix(entity.CanonicalURI, "pkg:gem/")
	if packageName == "" || packageName == entity.CanonicalURI {
		return fmt.Errorf("entity %q is not a gem package URI", entity.CanonicalURI)
	}

	client := gemregistry.NewClient()
	if globals != nil && globals.GemRegistryURL != "" {
		client = gemregistry.NewClientWithBaseURL(globals.GemRegistryURL)
	}

	repoURL, err := client.ResolveRepoURL(ctx, packageName)
	if err != nil {
		return fmt.Errorf("query rubygems.org registry: %w", err)
	}
	if repoURL == "" {
		// Gem doesn't declare a github-hosted repository. Nothing to
		// stamp; stay silent. Downstream dispatch will skip the github
		// + git collectors via isGitHostedEntity.
		return nil
	}

	entity.URL = repoURL
	entity.UpdatedAt = time.Now().UTC()
	if err := s.PutEntity(ctx, entity); err != nil {
		return fmt.Errorf("persist resolved URL on entity: %w", err)
	}
	return nil
}

// resolveMavenRepo asks Maven Central for the artifact's declared
// source repository URL by fetching the POM and extracting the <scm>
// section. Maven requires a two-step flow: first search for the latest
// version (since POM URLs include the version), then fetch that
// version's POM to extract the SCM URL.
//
// Parallel to resolveNpmRepo / resolvePyPIRepo / resolveCargoRepo /
// resolveGemRepo: the POM's <scm><url> answers the "where is this
// artifact's source?" question, the orchestrator stamps it, and
// downstream collectors operate against the resolved entity. The POM's
// SCM URL is publisher-declared (self-reported, not cryptographically
// bound).
func resolveMavenRepo(ctx context.Context, s store.Store, entity *profile.Entity, globals *Globals) error {
	const prefix = "pkg:maven/"
	rest := strings.TrimPrefix(entity.CanonicalURI, prefix)
	if rest == "" || rest == entity.CanonicalURI {
		return fmt.Errorf("entity %q is not a maven package URI", entity.CanonicalURI)
	}

	groupID, artifactID, found := strings.Cut(rest, "/")
	if !found || groupID == "" || artifactID == "" {
		return fmt.Errorf("entity %q: cannot extract groupId/artifactId", entity.CanonicalURI)
	}

	// Strip any @version suffix from the artifactID (the entity URI
	// may carry pkg:maven/g/a@v from versioned resolution).
	if name, _, ok := strings.Cut(artifactID, "@"); ok {
		artifactID = name
	}

	client := mavenregistry.NewClient()
	if globals != nil && globals.MavenRegistryURL != "" {
		client = mavenregistry.NewClientWithBaseURL(globals.MavenRegistryURL)
	}

	// Step 1: fetch metadata for the latest version.
	meta, err := client.FetchMetadata(ctx, groupID, artifactID)
	if err != nil {
		return fmt.Errorf("query Maven Central metadata: %w", err)
	}
	latestVersion := meta.Versioning.Release
	if latestVersion == "" {
		latestVersion = meta.Versioning.Latest
	}
	if latestVersion == "" && len(meta.Versioning.Versions) > 0 {
		latestVersion = meta.Versioning.Versions[len(meta.Versioning.Versions)-1]
	}
	if latestVersion == "" {
		return fmt.Errorf("no versions found for %s:%s on Maven Central", groupID, artifactID)
	}

	// Step 2: fetch the POM and extract the SCM URL.
	repoURL, err := client.ResolveRepoURL(ctx, groupID, artifactID, latestVersion)
	if err != nil {
		return fmt.Errorf("fetch POM for %s:%s:%s: %w", groupID, artifactID, latestVersion, err)
	}
	if repoURL == "" {
		// Artifact's POM doesn't declare an SCM URL. Nothing to stamp;
		// stay silent. Downstream dispatch will skip the github + git
		// collectors via isGitHostedEntity.
		return nil
	}

	entity.URL = repoURL
	entity.UpdatedAt = time.Now().UTC()
	if err := s.PutEntity(ctx, entity); err != nil {
		return fmt.Errorf("persist resolved URL on entity: %w", err)
	}
	return nil
}

// resolveGoRepo asks proxy.golang.org for the module's declared VCS
// source (Origin block), falling back to the vanity host's
// go-import meta tag for pre-Go-1.20 publishes that lack the proxy
// Origin. Stamps the resolved github URL on the entity. Persists.
//
// Parallel to resolveNpmRepo / resolvePyPIRepo / resolveCargoRepo:
// the provider answers the "where is this module's source?" question,
// the orchestrator stamps it, downstream collectors operate against
// the resolved entity. The resolution chain — proxy first, meta tag
// as fallback, neither-resolves means empty stays empty — lives in
// gopublish.Client.ResolveRepoURL; this wrapper handles the
// CanonicalURI parsing, the test-time URL injection, and the
// entity-stamp persistence.
//
// Module path extraction is identical for pkg:golang/ and pkg:go/
// (the latter is the gomod parser's preferred prefix; both decode
// to the same module). The TrimPrefix tries each in turn.
func resolveGoRepo(ctx context.Context, s store.Store, entity *profile.Entity, globals *Globals) error {
	modulePath := entity.CanonicalURI
	for _, prefix := range []string{"pkg:golang/", "pkg:go/"} {
		if rest, ok := strings.CutPrefix(modulePath, prefix); ok {
			modulePath = rest
			break
		}
	}
	if modulePath == "" || modulePath == entity.CanonicalURI {
		return fmt.Errorf("entity %q is not a Go module URI", entity.CanonicalURI)
	}
	// Strip @version if present — resolveGoRepo always queries the
	// module root, not a specific version. proxy.golang.org's
	// @latest gives us the version pointer to read .info from.
	if idx := strings.LastIndexByte(modulePath, '@'); idx > 0 {
		modulePath = modulePath[:idx]
	}

	// Construct client. Production: live proxy + sum, vanity-host
	// fetch goes to https://<modulePath>?go-get=1. Tests: Globals
	// fields point at httptest servers for both proxy and vanity.
	client := gopublish.NewClient()
	if globals != nil && (globals.GoProxyURL != "" || globals.GoVanityURL != "") {
		proxy := globals.GoProxyURL
		if proxy == "" {
			proxy = "https://proxy.golang.org"
		}
		// Tests don't typically inject sumURL for resolveGoRepo (the
		// resolver doesn't query the transparency log); reuse proxy
		// as a safe default that works against the same multiplexing
		// httptest server. Production uses NewClient.
		sum := proxy
		client = gopublish.NewClientWithBaseURLs(proxy, sum, globals.GoVanityURL)
	}

	repoURL, err := client.ResolveRepoURL(ctx, modulePath)
	if err != nil {
		return fmt.Errorf("query go proxy: %w", err)
	}
	if repoURL == "" {
		// Module has no resolvable github source (proxy 404 + no
		// meta tag, or non-github source declared). Stay silent;
		// downstream dispatch will skip the github + git collectors
		// via isGitHostedEntity, and the user gets gopublish
		// (publication-integrity) signals only.
		return nil
	}

	entity.URL = repoURL
	entity.UpdatedAt = time.Now().UTC()
	if err := s.PutEntity(ctx, entity); err != nil {
		return fmt.Errorf("persist resolved URL on entity: %w", err)
	}
	return nil
}
