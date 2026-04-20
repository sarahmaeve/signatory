package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/identity"
	"github.com/sarahmaeve/signatory/internal/profile"
	npmregistry "github.com/sarahmaeve/signatory/internal/signal/registry/npm"
	"github.com/sarahmaeve/signatory/internal/store"
)

// AnalyzeCmd retrieves or collects the trust profile for a target.
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
	// passed. See design/v0.1-invariants.md §"Invariant 2" for the
	// "no implicit network" principle this flag serves.
	Path string `name:"path" help:"Filesystem path to an existing local clone of the target. Required with --refresh for git-hosted entities unless --clone is passed." type:"path"`

	// --clone creates a new clone at --path. Always a full clone;
	// shallow clones silently degrade historical signals. Refuses to
	// run if --path is non-empty.
	Clone bool `name:"clone" help:"Create a new clone at --path by fetching from the target's origin. Fails loudly if --path is non-empty."`

	// Stdout and Stderr let tests inject buffers. Production paths
	// leave them nil; Run defaults them to os.Stdout / os.Stderr.
	// stdout/stderr discipline: progress, warnings, and status
	// lines go to stderr so that stdout carries ONLY the final
	// rendered output (JSON payload or human-readable profile).
	// This unblocks `signatory analyze --json … | jq` pipelines.
	Stdout io.Writer `kong:"-"`
	Stderr io.Writer `kong:"-"`
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
			CreatedAt:    time.Now().UTC(),
			UpdatedAt:    time.Now().UTC(),
		}
		switch resolved.Scheme {
		case "repo":
			entity.Type = profile.EntityProject
			entity.ShortName = resolved.Owner + "/" + resolved.ShortName
			entity.URL = resolved.CloneURL
		case "pkg":
			entity.Type = profile.EntityPackage
			entity.Ecosystem = resolved.Ecosystem
			// ShortName is the full package name (scope-preserving
			// for npm), not the last path segment — "@types/node",
			// not "node". ResolvedTarget.ShortName drops the scope
			// for its own reasons; reconstruct here.
			entity.ShortName = strings.TrimPrefix(
				resolved.CanonicalURI, "pkg:"+resolved.Ecosystem+"/")
		default:
			return fmt.Errorf("analyze does not yet support %q-scheme targets (got %q)",
				resolved.Scheme, resolved.CanonicalURI)
		}
		if err := s.PutEntity(ctx, entity); err != nil {
			return fmt.Errorf("create entity: %w", err)
		}
		created = true
	}

	// Resolve the entity's upstream repo URL when it's an npm
	// package that hasn't been resolved yet (A.5 in design/npm-plan.
	// txt). The registry tells us where the package's source lives;
	// the orchestrator stamps it on the entity so downstream
	// collectors (github, git-local-clone) pick it up via entity.URL.
	//
	// Failure here is non-fatal: the npm collector still runs and
	// emits registry signals; only the github-side collectors get
	// skipped (because isGitHostedEntity stays false). A warning to
	// stderr gives the operator a trail.
	if entity.Type == profile.EntityPackage && entity.Ecosystem == "npm" && entity.URL == "" {
		if err := resolveNpmRepo(ctx, s, entity, globals); err != nil {
			// Deliberate `_, _ =` on stderr writes throughout Run():
			// these are diagnostic progress/warning lines, not the
			// command's contract output. A failure to write them
			// (stderr closed, broken pipe to an unusual pipeline
			// configuration) doesn't change what the analysis
			// should report on stdout. The rendered-output path
			// (displayProfile → displayHuman / JSON write) DOES
			// propagate write errors via stickyWriter; this
			// asymmetry is intentional.
			_, _ = fmt.Fprintf(stderr, "warning: npm repo resolution for %s failed: %v\n",
				entity.CanonicalURI, err)
		}
	}

	_, _ = fmt.Fprintf(stderr, "Collecting signals for: %s\n", entity.CanonicalURI)

	// Decide which collectors to run. Tests inject mocks via
	// globals.Collectors (see functional_test.go); in production that
	// field is empty and we build the collector list per-target based
	// on the entity's shape plus --path / --clone.
	collectors := globals.Collectors
	if len(collectors) == 0 {
		c, err := collectorsFor(ctx, entity, CollectOpts{Path: cmd.Path, Clone: cmd.Clone})
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
		allSignals = append(allSignals, result.Signals()...)
		_, _ = fmt.Fprintf(stderr, "[%s] %s\n", collector.Name(), result.Summary())
	}

	if err := s.AppendSignals(ctx, allSignals); err != nil {
		return fmt.Errorf("store signals: %w", err)
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
				retryable := ""
				if r, ok := val["retryable"].(bool); ok && r {
					retryable = " (retryable)"
				}
				reason := ""
				if r, ok := val["reason"].(string); ok {
					reason = r
				}
				sw.Writef("  %-20s [ABSENT]  %s%s\n",
					strings.TrimPrefix(s.Type, "absence:"), reason, retryable)
			} else {
				sw.Writef("  %-20s [%s]  ", s.Type, s.ForgeryResistance)
				printCompactValue(sw, val)
				sw.Writeln()
			}
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
