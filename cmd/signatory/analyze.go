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

	"github.com/sarahmaeve/signatory/internal/identity"
	"github.com/sarahmaeve/signatory/internal/manifest/gomod"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	"github.com/sarahmaeve/signatory/internal/signal/registry/gopublish"
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
// is tracked: the original design (design/mcp-dual-analyst-
// architecture.md) envisioned `signatory analyze --depth=full`
// running the LLM-backed pipeline in-process. v0.1 Invariant 1
// ("forbid LLM-client deps and API hostnames" in the binary)
// forced the LLM work out of the CLI and into the Claude-skill
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
	// the matrix row, itself a forgery-resistance HIGH signal —
	// see design/coll7.md D11. Operators who know their clone
	// may be stale relative to the proxy can opt in to the
	// targeted retry.
	AllowFetch bool `name:"allow-fetch" help:"Allow the source-evolution collector to retry missing-SHA reads via 'git fetch origin' once. Default off — a missing SHA after --refresh is preserved as a signal rather than fetched." default:"false"`

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

// resolvableEcosystems is the set of pkg:<ecosystem>/ values for
// which signatory has wired a source-URL resolver in this
// AnalyzeCmd.Run (resolveNpmRepo, resolvePyPIRepo, resolveGoRepo).
//
// Used by the unsupported-ecosystem hint just before collector
// dispatch — entities whose ecosystem ISN'T in this set get a
// stderr note explaining that github + git + repofiles + openssf
// collectors won't fire (no URL → isGitHostedEntity false →
// silent skip otherwise).
//
// Keep in sync with the resolver branches in Run(): adding a new
// resolver means adding its ecosystem here, removing one
// (unlikely) means removing the entry. A resolver missing from
// this set produces noisy notes on supported targets; an entry
// here without a resolver suppresses helpful notes on unsupported
// targets. Either drift is a UX bug, not a correctness one.
var resolvableEcosystems = map[string]bool{
	"npm":    true,
	"pypi":   true,
	"golang": true,
	"go":     true,
}

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
}

func (cmd *AnalyzeCmd) Run(globals *Globals) error {
	// Writer defaults: tests inject cmd.Stdout / cmd.Stderr; prod
	// paths fall through to os.Stdout / os.Stderr.
	stdout := cmd.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := cmd.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	// Root context. globals.Context, when set, carries the SIGINT-
	// cancellation wiring from main(); Ctrl-C at the CLI propagates
	// through the HTTP client and cancels in-flight network work.
	// Tests or library callers that don't set it get a fresh
	// background context.
	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}

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

	// Normalize user input to a canonical URI via the single
	// CLI-wide target parser. This is the one place where free-form
	// input crosses into stable internal identifiers — everything
	// downstream uses resolved.CanonicalURI as the lookup key.
	resolved, err := profile.ResolveTarget(cmd.Target)
	if err != nil {
		return fmt.Errorf("parse target %q: %w", cmd.Target, err)
	}

	// Default --path when --clone is set without --path: a stable filestore
	// location keyed by the resolved short-name. Matches the layout used by
	// `signatory handoff --clone-dir filestore/clones/` and the /analyze
	// pipeline's Step 1, so a subsequent `signatory handoff` run reuses
	// the same directory without ceremony.
	//
	// Empty ShortName (some pkg:* canonical URIs) falls through to the
	// existing ErrPathMissing in resolveClonePath — the default depends on
	// having a name to key off.
	if cmd.Clone && cmd.Path == "" && resolved.ShortName != "" {
		cmd.Path = filepath.Join("filestore", "clones", resolved.ShortName)
	}

	// Look up an existing entity by canonical URI. A matching entity
	// means the user has analyzed this target before — we reuse its
	// UUID ID so FK references stay stable.
	entity, err := s.FindEntityByURI(ctx, resolved.CanonicalURI)
	if errors.Is(err, store.ErrNotFound) {
		entity = nil
	} else if err != nil {
		return fmt.Errorf("lookup entity: %w", err)
	}

	// Decide what to do based on cache state and --refresh.
	if !cmd.Refresh {
		if entity == nil {
			// "Nothing to report" messages go to stderr — stdout is
			// reserved for the rendered output, and in this branch
			// there's no output to render. A scripted consumer sees
			// an empty stdout and a zero exit code; diagnostics
			// explaining why are on stderr.
			// Write-error suppression (`_, _ =`) on the last statement
			// before a clean return: the write target is stderr and
			// there's no propagation opportunity. errcheck flags
			// these specifically because they're terminal; the
			// explicit discard matches the intent.
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
		// Cached state is non-empty if we have signals OR analyst
		// outputs. Either qualifies as "we know things about this
		// target." Emptiness in both is the only "go run --refresh"
		// case.
		if len(existingSignals) == 0 && len(analystOutputs) == 0 {
			_, _ = fmt.Fprintf(stderr, "No cached signals or analyst outputs for: %s\n", cmd.Target)
			_, _ = fmt.Fprintln(stderr, "Run with --refresh to collect signals from GitHub,")
			_, _ = fmt.Fprintln(stderr, "or run `signatory ingest <file>` to load an analyst output.")
			return nil
		}
		return cmd.displayProfile(ctx, s, entity, analystOutputs, stdout)
	}

	// --- Refresh path: collect fresh signals. ---

	// Create the entity if it doesn't exist yet. Type, ShortName,
	// URL, and Ecosystem are derived from the resolved target's
	// scheme — repo: entities are github-hosted projects today;
	// pkg: entities are registry packages whose repo URL may be
	// resolved asynchronously by the provider (A.5 will add that
	// step for npm; leaving URL empty is benign for Phase A).
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
			return fmt.Errorf("analyze does not yet support %q-scheme targets (got %q)",
				resolved.Scheme, resolved.CanonicalURI)
		}
		if err := s.PutEntity(ctx, entity); err != nil {
			return fmt.Errorf("create entity: %w", err)
		}
		created = true
	}

	// Defensive backfill: an entity loaded from the store with an
	// empty Ecosystem but a known one from the resolved target is
	// either (a) data created by a pre-fix `ensureEntity` that
	// omitted the field, or (b) a future entity-creation path
	// that forgot it. Either way, the resolver guards below gate
	// on entity.Ecosystem; without backfill they silently skip
	// and Layer-1 collection emits zero signals (the 2026-04-28
	// idna refresh meltdown). Persist the backfilled value so
	// subsequent reads see it without re-running this branch.
	if entity.Ecosystem == "" && resolved.Ecosystem != "" {
		entity.Ecosystem = resolved.Ecosystem
		entity.UpdatedAt = time.Now().UTC()
		if err := s.PutEntity(ctx, entity); err != nil {
			// Don't fail the run on a backfill persistence error —
			// the in-memory entity carries the correct value through
			// the rest of this invocation, just won't survive past
			// it. Surface the warning so an operator notices.
			_, _ = fmt.Fprintf(stderr, "warning: backfill ecosystem on %s failed: %v\n",
				entity.CanonicalURI, err)
		}
	}

	// Defensive backfill of entity.URL: same shape as the Ecosystem
	// backfill above. Stale rows created before ResolveTarget wired
	// CloneURL for pkg:{golang,go}/github.com/* and golang.org/x/Y
	// have empty URL, which keeps isGitHostedEntity false in
	// collectorsFor and silently skips the github + git + repofiles
	// + openssf collectors. Backfill from resolved.CloneURL when
	// known so the next dispatch fires the full collector set.
	//
	// Closes the symptom of the 2026-04-30 dogfood:
	// `signatory analyze --clone --refresh pkg:golang/github.com/alecthomas/kong`
	// returned only the 4 gopublish signals because the entity (from
	// a 7-day-old security analysis) had URL="" and the just-shipped
	// CloneURL wiring only fires during entity creation.
	if entity.URL == "" && resolved.CloneURL != "" {
		entity.URL = resolved.CloneURL
		entity.UpdatedAt = time.Now().UTC()
		if err := s.PutEntity(ctx, entity); err != nil {
			_, _ = fmt.Fprintf(stderr, "warning: backfill URL on %s failed: %v\n",
				entity.CanonicalURI, err)
		}
	}

	// Resolve the entity's upstream repo URL when it's an npm
	// package that hasn't been resolved yet (A.5 in design/npm-plan.
	// txt). The registry tells us where the package's source lives;
	// the orchestrator stamps it on the entity so downstream
	// collectors (github, git-local-clone) pick it up via entity.URL.
	//
	// Failure is always recorded as an absence:repo_declaration signal
	// with retryable=true so the stored profile carries a machine-
	// readable marker: "we tried and the registry failed." This
	// distinguishes the degraded state from a legitimate "no declared
	// repo" result and from "we never tried." On an explicit --refresh,
	// the error is also returned to the caller (fail loud) because the
	// user asked for fresh data and we cannot silently decline.
	//
	// Gates on Ecosystem rather than Type — same rationale as the
	// go-module gate below (and the symmetric pypi gate that follows):
	// pre-PR0 entities in dogfood stores carry Type=EntityProject from
	// the analyst-output ingest path (analyst_output.go) that hardcoded
	// the type before profile.EntityTypeForURI was hoisted out. The
	// Ecosystem field is the load-bearing one; gating on it lets the
	// resolver fire on legacy mistyped rows without requiring a data
	// migration. PR0 fixed the producer; this defensive gate covers
	// the rows that were already in users' stores when PR0 landed.
	if entity.Ecosystem == "npm" && entity.URL == "" {
		if resolveErr := resolveNpmRepo(ctx, s, entity, globals); resolveErr != nil {
			// Always write an absence signal so the profile carries a
			// stored record of the failure — not just ephemeral stderr
			// chatter. This is a standalone AppendSignals write so it
			// persists even when we return an error on the --refresh path.
			absenceSig := signal.MakeAbsence(
				entity.ID,
				"repo_declaration",
				"npm-registry",
				resolveErr.Error(),
				true, // retryable: transient registry failure or attacker-controlled response
				time.Now().UTC(),
			)
			_ = s.AppendSignals(ctx, []profile.Signal{absenceSig.ToSignal()}) //nolint:errcheck // best-effort; primary error is resolveErr

			if cmd.Refresh {
				// Deliberate `_, _ =` on stderr writes throughout Run():
				// these are diagnostic progress/warning lines, not the
				// command's contract output. A failure to write them
				// (stderr closed, broken pipe) doesn't change what we
				// should report.
				_, _ = fmt.Fprintf(stderr, "warning: npm repo resolution for %s failed: %v\n",
					entity.CanonicalURI, resolveErr)
				return fmt.Errorf("refresh npm repo resolution for %s: %w",
					entity.CanonicalURI, resolveErr)
			}
			// Non-refresh path: warn only. The absence signal above
			// gives operators a stored trail; analysis continues with
			// whatever signals the npm collector can still provide.
			_, _ = fmt.Fprintf(stderr, "warning: npm repo resolution for %s failed: %v\n",
				entity.CanonicalURI, resolveErr)
		}
	}

	// PyPI parallel to the npm resolution above. Same shape, same
	// failure semantics — closes the gap surfaced by the 2026-04-28
	// ms dogfood audit where pkg:pypi/ targets reached the analysts
	// with no resolved github source, so the github + git collectors
	// silently skipped and the analysts went to upstream APIs.
	//
	// Gates on Ecosystem rather than Type — see the npm gate above
	// for the rationale (legacy mistyped rows from the pre-PR0
	// analyst_output.go hardcode).
	if entity.Ecosystem == "pypi" && entity.URL == "" {
		if resolveErr := resolvePyPIRepo(ctx, s, entity, globals); resolveErr != nil {
			absenceSig := signal.MakeAbsence(
				entity.ID,
				"repo_declaration",
				"pypi-registry",
				resolveErr.Error(),
				true,
				time.Now().UTC(),
			)
			_ = s.AppendSignals(ctx, []profile.Signal{absenceSig.ToSignal()}) //nolint:errcheck // best-effort; primary error is resolveErr

			if cmd.Refresh {
				_, _ = fmt.Fprintf(stderr, "warning: pypi repo resolution for %s failed: %v\n",
					entity.CanonicalURI, resolveErr)
				return fmt.Errorf("refresh pypi repo resolution for %s: %w",
					entity.CanonicalURI, resolveErr)
			}
			_, _ = fmt.Fprintf(stderr, "warning: pypi repo resolution for %s failed: %v\n",
				entity.CanonicalURI, resolveErr)
		}
	}

	// Go-module vanity-host resolution: parallel to npm/pypi above.
	// Triggers for entities whose Ecosystem is "golang"/"go" and
	// whose URL stayed empty after parse-time CloneURL stamping —
	// i.e., vanity hosts (gopkg.in, modernc.org, k8s.io) where the
	// URI alone doesn't algorithmically yield a github source.
	// Resolver queries proxy.golang.org for an Origin block and,
	// if that fails, falls back to the vanity host's go-import
	// meta tag.
	//
	// Gates on Ecosystem rather than Type because pre-fix entities
	// in some users' stores carry Type=EntityProject (created via
	// MCP ingest paths that didn't classify the URI as a package);
	// the Ecosystem field is the load-bearing one, already stamped
	// by the defensive backfill above. The URI prefix
	// (pkg:golang/* or pkg:go/*) is implied by Ecosystem.
	//
	// Failure semantics match npm/pypi: a transport error during
	// resolution writes an absence:repo_declaration signal AND
	// returns a wrapped error on --refresh (loud-fail). A "no source
	// resolvable" outcome (proxy 404 + no meta tag) is NOT a failure
	// — empty URL stays empty, github + git + repofiles + openssf
	// collectors gate themselves out, and the user still gets
	// gopublish signals.
	if (entity.Ecosystem == "golang" || entity.Ecosystem == "go") &&
		entity.URL == "" {
		if resolveErr := resolveGoRepo(ctx, s, entity, globals); resolveErr != nil {
			absenceSig := signal.MakeAbsence(
				entity.ID,
				"repo_declaration",
				"goproxy",
				resolveErr.Error(),
				true,
				time.Now().UTC(),
			)
			_ = s.AppendSignals(ctx, []profile.Signal{absenceSig.ToSignal()}) //nolint:errcheck // best-effort; primary error is resolveErr

			if cmd.Refresh {
				_, _ = fmt.Fprintf(stderr, "warning: go-module repo resolution for %s failed: %v\n",
					entity.CanonicalURI, resolveErr)
				return fmt.Errorf("refresh go-module repo resolution for %s: %w",
					entity.CanonicalURI, resolveErr)
			}
			_, _ = fmt.Fprintf(stderr, "warning: go-module repo resolution for %s failed: %v\n",
				entity.CanonicalURI, resolveErr)
		}
	}

	// Unsupported-ecosystem hint: when the user --refresh's a
	// pkg:<ecosystem>/<name> target whose ecosystem isn't one
	// signatory has wired a resolver for, every git-side collector
	// (github, git, repofiles, openssf) silently sits out because
	// entity.URL is empty and isGitHostedEntity returns false. The
	// user sees zero collected signals and has no idea why.
	//
	// Surfacing the gap here closes the last user-visible
	// silent-skip path identified in design/openssf-problem.txt.
	// Distinct from npm/pypi/golang's failed-resolution case
	// (which writes absence:repo_declaration); this is the
	// "we didn't try because no resolver exists" case.
	//
	// Gates on Ecosystem rather than Type — same rationale as
	// resolveGoRepo: pre-fix entities in some users' stores carry
	// Type=EntityProject from MCP ingest paths that didn't classify
	// the URI as a package. Ecosystem is the load-bearing field;
	// when it's set and not in the resolvable set, the URI is a
	// pkg:<ecosystem>/* and we're missing a resolver.
	if entity.Ecosystem != "" &&
		entity.URL == "" &&
		!resolvableEcosystems[entity.Ecosystem] {
		_, _ = fmt.Fprintf(stderr,
			"note: ecosystem %q has no source resolver yet — github + git + repofiles + openssf collectors won't fire for this target.\n",
			entity.Ecosystem)
	}

	// Nudge users analyzing a Go module via its github URL form
	// (repo:github/X/Y) toward the canonical pkg:golang/<modpath>
	// form, which gets the gopublish collector. No-op for
	// non-Go-module repos and for entities already using pkg:golang.
	// See maybeWarnGoModuleViaRepoForm for the full rationale.
	maybeWarnGoModuleViaRepoForm(stderr, entity, cmd.Path)

	_, _ = fmt.Fprintf(stderr, "Collecting signals for: %s\n", entity.CanonicalURI)

	// Decide which collectors to run. Tests inject mocks via
	// globals.Collectors (see functional_test.go); in production that
	// field is empty and we build the collector list per-target based
	// on the entity's shape plus --path / --clone.
	//
	// inRunResult accumulates signals as collectors complete so that
	// later collectors can read earlier collectors' emissions in the
	// same run. Currently only the source-evolution collector consumes
	// this — it reads gopublish's version_pin_table to anchor matrix
	// rows to commit SHAs (design/coll7.md D3, Architecture B). The
	// pointer is captured at construction time so subsequent mutations
	// inside this loop are visible to source-evolution's Collect.
	inRunResult := &signal.CollectionResult{}

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
		})
		if err != nil {
			return err
		}
		collectors = c
	}

	var allSignals []profile.Signal
	for _, collector := range collectors {
		result, err := collector.Collect(ctx, entity)
		if err != nil {
			return fmt.Errorf("collect signals (%s): %w", collector.Name(), err)
		}
		// Accumulate into inRunResult so subsequent collectors see
		// this collector's signals. Append to .Collected (not
		// .Signals()) because Signals() flattens absences for
		// storage, but the in-run lookup wants the full record set.
		inRunResult.Collected = append(inRunResult.Collected, result.Collected...)
		allSignals = append(allSignals, result.Signals()...)
		_, _ = fmt.Fprintf(stderr, "[%s] %s\n", collector.Name(), result.Summary())
	}

	if err := s.AppendSignals(ctx, allSignals); err != nil {
		return fmt.Errorf("store signals: %w", err)
	}

	// Storage breadcrumb: tell the user where their freshly-collected
	// signals went and how to query them. Names the resolved DB path
	// so a manual `signatory analyze --refresh` invocation hands the
	// user the next thread to pull (signatory show-conclusions, or a
	// direct sqlite3 inspection of the file). When --clone planted a
	// clone, also surface that path so `cd <path> && git log` is one
	// hop away. ResolvePath errors are swallowed: the path resolution
	// can't fail in any way that matters AFTER a successful AppendSignals
	// against the same DBPath, so a warning would be noise — fall back
	// to just naming the count.
	if dbPath, perr := store.ResolvePath(globals.DBPath); perr == nil {
		_, _ = fmt.Fprintf(stderr, "Stored %d signals in %s\n", len(allSignals), dbPath)
	} else {
		_, _ = fmt.Fprintf(stderr, "Stored %d signals\n", len(allSignals))
	}
	_, _ = fmt.Fprintf(stderr,
		"  query: signatory show-conclusions --target %s\n", entity.CanonicalURI)
	// Only surface the inspect-clone hint if a clone actually exists.
	// --clone may have been requested but never executed (vanity-host
	// Go module with no derivable github source: dispatch gate keeps
	// the git-side collectors out, ensureCloneAtPath never runs). The
	// .git probe is the honest test — promising a clone path that
	// isn't there sends the user on a wild goose chase.
	if cmd.Clone && cmd.Path != "" {
		if absClone, aerr := filepath.Abs(cmd.Path); aerr == nil {
			if _, gitErr := os.Stat(filepath.Join(absClone, ".git")); gitErr == nil {
				_, _ = fmt.Fprintf(stderr, "  inspect clone: %s\n", absClone)
			}
		}
	}

	entity.UpdatedAt = time.Now().UTC()
	if err := s.PutEntity(ctx, entity); err != nil {
		return fmt.Errorf("update entity: %w", err)
	}

	// Audit the analysis. Failure is non-fatal — the signals are
	// already in the store; a missing audit line is a secondary
	// observability concern, not a correctness failure.
	detail, _ := json.Marshal(map[string]any{
		"target":            cmd.Target,
		"canonical_uri":     entity.CanonicalURI,
		"signals_collected": len(allSignals),
		"created_entity":    created,
	})
	if err := auditLog.LogAction(ctx, actor, "analyze", entity.ID, string(detail)); err != nil {
		_, _ = fmt.Fprintf(stderr, "warning: audit log write failed: %v\n", err)
	}

	// Even on a refresh path, surface any cached analyst outputs —
	// they're the Layer 2 picture; the Layer 1 collectors don't
	// touch them. An agent calling `analyze --refresh` after a
	// previous ingest still benefits from seeing that an analyst
	// output exists (and a recent one at that).
	analystOutputs, err := cmd.fetchAnalystOutputs(ctx, s, entity.ID)
	if err != nil {
		return fmt.Errorf("read analyst outputs (post-refresh): %w", err)
	}

	// Blank separator between the progress stream (stderr) and the
	// upcoming rendered output (stdout). On a terminal that
	// interleaves both, this separates the diagnostic chatter from
	// the data; on a pipe (--json | jq), stdout stays clean JSON.
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

	burn, err := s.GetBurn(ctx, entity.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("get burn: %w", err)
	}

	p := &profile.Profile{
		Entity:   *entity,
		Signals:  signals,
		Postures: postures,
		Burn:     burn,
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
		sw.Writef("*** BURNED: %s (by %s, %s) ***\n",
			p.Burn.Reason, p.Burn.BurnedBy, p.Burn.BurnedAt.Format(time.RFC3339))
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

			if strings.HasPrefix(s.Type, "absence:") {
				absenceCount++
				retryable := false
				if r, ok := val["retryable"].(bool); ok {
					retryable = r
				}
				reason := ""
				if r, ok := val["reason"].(string); ok {
					reason = r
				}
				name := strings.TrimPrefix(s.Type, "absence:")
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

// resolveNpmRepo asks the npm registry for the package's declared
// repository URL, normalizes it to a github clone URL (empty if the
// package doesn't declare one or declares a non-github host), and
// stamps the result on the entity. Persists the entity update so
// subsequent reads see the resolved URL.
//
// Lives in analyze.go rather than inside the npm collector per
// decision (a) in design/npm-plan.txt: the provider answers the
// "where is this package's source?" question, the orchestrator
// records it, and downstream collectors work against the resolved
// entity. Keeping the provider out of the collector prevents the
// collector's tight loop (1 call per signal it emits) from bleeding
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

// resolveGoRepo asks proxy.golang.org for the module's declared VCS
// source (Origin block), falling back to the vanity host's
// go-import meta tag for pre-Go-1.20 publishes that lack the proxy
// Origin. Stamps the resolved github URL on the entity. Persists.
//
// Parallel to resolveNpmRepo / resolvePyPIRepo: the provider answers
// the "where is this module's source?" question, the orchestrator
// stamps it, downstream collectors operate against the resolved
// entity. The resolution chain — proxy first, meta tag as fallback,
// neither-resolves means empty stays empty — lives in
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
