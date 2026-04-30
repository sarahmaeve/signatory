package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory"
	"github.com/sarahmaeve/signatory/internal/config"
	"github.com/sarahmaeve/signatory/internal/ecosystem"
	"github.com/sarahmaeve/signatory/internal/ecosystem/resolver"
	"github.com/sarahmaeve/signatory/internal/pipeline"
	"github.com/sarahmaeve/signatory/internal/profile"
	ghclient "github.com/sarahmaeve/signatory/internal/signal/github"
	"github.com/sarahmaeve/signatory/internal/store"
	"github.com/sarahmaeve/signatory/internal/synthesis"
)

// HandoffCmd renders a handoff prompt by loading a template from the
// resolver-ordered search path and substituting the caller's
// `{PLACEHOLDER}` values. Output defaults to stdout; pass --output to
// write a file.
//
// Typical invocations:
//
//	signatory handoff security https://github.com/nvbn/thefuck
//	signatory handoff provenance /Users/me/code/thefuck --ecosystem=pypi
//	signatory handoff security ./atuin --language=go
//
// The <role> positional picks between the shipped roles (security,
// provenance). For security, --language=python|go picks which
// pattern-catalog variant to use. --template overrides the inference
// for advanced callers who maintain their own template variants.
//
// All `{PLACEHOLDER}` tokens not filled by flags are left literal in
// the output; the command reports which ones remained on stderr so
// the user can fix them before pasting into an agent prompt. The
// exception is TARGET_NAME — if it can't be inferred from the target
// and the user didn't pass --name, the command errors rather than
// emit a broken handoff.
//
// Network behavior: this command is offline by default. Language and
// ecosystem auto-detection from a remote repo is available via the
// --network-precheck flag, which probes the GitHub API for the
// target's primary language and ecosystem markers. Without it, pass
// --language and --ecosystem explicitly.
type HandoffCmd struct {
	Role   string `arg:"" enum:"security,provenance,synthesist" help:"Analyst role: security, provenance, or synthesist."`
	Target string `arg:"" help:"Target repository URL or local path (e.g., https://github.com/foo/bar or /Users/me/code/foo)."`

	Name       string `help:"Override TARGET_NAME (default: inferred from target)."`
	URL        string `help:"Override TARGET_URL (default: target when target is a URL)."`
	Path       string `help:"Override TARGET_PATH (default: target when target is a local path)."`
	TargetRole string `name:"target-role" default:"" help:"Dependency role for TARGET_ROLE (runtime|validation|build-only|development)." enum:"runtime,validation,build-only,development,"`
	Ecosystem  string `default:"" help:"ECOSYSTEM value for provenance role (pypi|npm|crates|go). Auto-detected with --network-precheck." enum:"pypi,npm,crates,go,"`
	// Language default is "" — kong would otherwise make it
	// impossible to distinguish "user passed --language=python" from
	// "user omitted it and kong filled in the default", and the
	// --network-precheck path needs that distinction to know whether
	// to apply a detected language. Resolved to "python" in Run if
	// still empty after precheck.
	Language   string `help:"Language flavor for security role (python|go). Auto-detected with --network-precheck; falls back to python." default:"" enum:"python,go,"`
	Intake     string `help:"INTAKE_QUESTION body; the user's specific question for this engagement (one-line)."`
	IntakeFile string `name:"intake-file" help:"Path to a file containing the INTAKE_QUESTION (or '-' for stdin). Use this for multi-line briefs that would otherwise need heredoc gymnastics."`
	Template   string `help:"Explicit template name (e.g., handoffs/security-review-v1.md). Bypasses --role/--language inference."`

	TemplateDir  []string `name:"template-dir" help:"Additional template search directory (repeatable, highest priority)."`
	FilestoreDir []string `name:"filestore-dir" help:"Additional filestore output directory (repeatable). Unused unless --output is a bare filename."`
	ConfigFile   string   `name:"config" help:"Path to signatory.config.toml. If unset, discovered from --project-dir." type:"existingfile"`
	ProjectDir   string   `name:"project-dir" help:"Project root used to locate ./templates/, ./filestore/, and signatory.config.toml." default:"." type:"path"`

	NetworkPrecheck bool   `name:"network-precheck" help:"Fill unset --language and --ecosystem by calling the GitHub API (requires a github.com target). Offline by default; this is the opt-in that authorizes network calls."`
	CloneDir        string `name:"clone-dir" help:"Clone the target URL into CLONE_DIR/<repo-name>/ with full history and use that path for TARGET_PATH. Skipped if the destination already exists. Requires target to be a URL." type:"path"`

	AnalysisSessionID string `name:"analysis-session-id" help:"Link the handoff (and the analyst's resulting ingest) to an analysis_sessions.id. Get the id from 'signatory analysis begin'. Validated before render — unknown or already-terminal ids fail here rather than being discovered by the dispatched subagent."`

	Output string `short:"o" help:"Write rendered handoff to this file instead of stdout."`
	Force  bool   `help:"Overwrite --output if it exists."`
	Quiet  bool   `help:"Suppress the stderr report (template source, unfilled placeholders)." short:"q"`

	DepositTo   string `name:"deposit-to" help:"Deposit the rendered handoff into the named pipeline session (UUID from 'signatory pipeline session create'). Conflicts with --output — deposit replaces file/stdout as the destination."`
	PipelineURL string `name:"pipeline-url" help:"Override the pipeline service base URL used by --deposit-to. Ignored when --deposit-to is not set." default:"${pipelineURL}"`

	// PrecheckSource overrides the GitHub-backed ecosystem.Source used
	// by --network-precheck. nil → fall through to
	// ghclient.NewClient(GITHUB_TOKEN) at call time. Exists as a
	// struct-field seam (not a package-level var) so parallel tests
	// can't race each other's injection, and so the dependency graph
	// is visible from HandoffCmd's type definition.
	//
	// kong:"-" excludes the field from the CLI surface — it's a
	// programmatic seam, not a user-tunable flag.
	PrecheckSource ecosystem.Source `kong:"-"`

	// RunGitClone overrides the git subprocess invocation for
	// --clone-dir. nil → fall through to defaultGitClone, which
	// composes argv and routes through defaultRunGit (collectors.go)
	// for the actual gitenv.NewCloneCmd construction — both clone
	// sites in this binary share that one chokepoint, so env-strip
	// and WaitDelay discipline can't drift between them. Same
	// rationale as PrecheckSource: parallel-safe injection, visible
	// dependency.
	//
	// version is the git ref (tag/branch) to check out, or empty
	// for HEAD-of-default-branch. When non-empty, the production
	// implementation passes `--branch <version>` to git clone.
	// Tests can assert on the version arg by inspecting their
	// recorder's captured calls.
	RunGitClone func(ctx context.Context, url, dest, version string) error `kong:"-"`

	// requestedVersion carries the @V suffix the user attached to
	// the target (e.g., `github.com/X/Y@v1.0.0`). Populated by
	// Run() from ResolveTarget's output and consumed by applyClone
	// to pass `--branch` to git clone. Unexported so kong doesn't
	// expose it as a flag.
	requestedVersion string `kong:"-"`

	// EcosystemRegistry overrides the ecosystem resolver registry
	// that --network-precheck consults for pkg:<eco>/<name> targets.
	// nil → fall through to resolver.Default (the process-wide
	// registry shipping npm + go resolvers at init). Tests build a
	// fresh registry with resolver.NewRegistry() and register stubs
	// so they don't hit live registries. Same rationale as
	// PrecheckSource / RunGitClone: parallel-safe injection,
	// dependency visible from the struct definition.
	EcosystemRegistry *resolver.Registry `kong:"-"`
}

// Run executes the handoff render. Errors from template resolution,
// substitution, or write are surfaced directly; the stderr report
// (template source, unfilled placeholders, embedded-fallback notice)
// is informational and goes out independently of Success/failure.
func (cmd *HandoffCmd) Run(globals *Globals) error {
	// Single-destination rule: --output (file), stdout (default), and
	// --deposit-to (pipeline service) are the three mutually exclusive
	// sinks a rendered handoff can go to. Two of them together is a
	// user-input mistake, caught here rather than after clone/render
	// work has happened. --output vs stdout was already single-sink via
	// writeHandoff's logic; this adds --deposit-to to the same rule.
	if cmd.DepositTo != "" && cmd.Output != "" {
		return NewUsageError(fmt.Errorf(
			"--deposit-to and --output are mutually exclusive; choose one destination"))
	}

	// Reconcile --intake / --intake-file (§3.4 agent-facing-contract).
	// Intake is optional — the template falls back to an embedded
	// default — so an empty result is fine; the readFreeText helper
	// only cares about conflict and malformed inputs.
	intake, err := readFreeText("intake", cmd.Intake, cmd.IntakeFile)
	if err != nil {
		return err
	}
	cmd.Intake = intake

	// Resolve the target to canonical form. For any input that
	// ResolveTarget understands (GitHub shorthand, github.com URL,
	// `repo:` canonical URI, etc.) pre-populate --name and --url
	// from the resolved metadata, and rewrite cmd.Target itself to
	// the HTTPS clone URL so downstream classifier-driven code
	// (HandoffSubstitutions, applyClone) sees a form it handles
	// natively.
	//
	// Non-URI / non-shorthand inputs — local filesystem paths like
	// ~/code/thefuck, /Users/me/code/proj, ./relative — cleanly
	// fail ResolveTarget; the rest of Run() then handles them via
	// TargetPath as before. ResolveTarget failure is not a CLI
	// error here; it's a signal that the target isn't a
	// remote-repo form.
	if resolved, err := profile.ResolveTarget(cmd.Target); err == nil {
		if cmd.Name == "" {
			cmd.Name = resolved.ShortName
		}
		// Capture the @version suffix (if any) for applyClone.
		// CloneURL deliberately omits the version (it's the bare
		// HTTPS git URL); the version is a separate clone-time
		// parameter (`git clone --branch`).
		cmd.requestedVersion = resolved.Version
		if resolved.CloneURL != "" {
			if cmd.URL == "" {
				if cmd.Role == "synthesist" {
					// Synthesist's TARGET_URL must carry the
					// canonical URI WITH @V so its output's
					// `target` field matches the analysts' outputs
					// it's synthesizing — same entity, same store
					// row. Without this, the synthesist would set
					// target to the bare clone URL, ingest under
					// the BARE URI, and the resulting analysis
					// would live at a different entity from the
					// security/provenance outputs the synthesist
					// just rolled up. Surfaced by the halted
					// /analyze testify@v1.11.1 dogfood.
					cmd.URL = resolved.CanonicalURI
				} else {
					cmd.URL = resolved.CloneURL
				}
			}
			// Feed the HTTPS URL form to downstream steps. This
			// turns every accepted form — owner/repo,
			// github.com/owner/repo, repo:github/owner/name — into
			// the same effective target for --clone-dir processing
			// (which expects a URL git can clone, not a canonical
			// URI). The version is preserved separately on
			// cmd.requestedVersion for applyClone's --branch arg.
			cmd.Target = resolved.CloneURL
		} else if cmd.Role == "synthesist" && cmd.URL == "" {
			// Canonical-URI fallback for the synthesist role when
			// the target is a pkg URI with no clone URL
			// (pkg:npm/X, pkg:go/...). Without this, {TARGET_URL}
			// renders literal in the handoff body (surfaced by
			// the 2026-04-21 dogfood).
			cmd.URL = resolved.CanonicalURI
		}
	}

	// Network precheck runs early: it may fill --language and
	// --ecosystem, which both influence later steps (template name
	// inference, provenance-role validation).
	var precheckReport string
	if cmd.NetworkPrecheck {
		report, err := cmd.applyNetworkPrecheck(context.Background())
		if err != nil {
			return fmt.Errorf("network-precheck: %w", err)
		}
		precheckReport = report
	}

	// Clone step: full-clone the target URL if --clone-dir was passed.
	// Runs AFTER precheck (precheck may confirm the target is a GitHub URL)
	// but BEFORE template resolution (so TARGET_PATH is available).
	var cloneReport string
	if cmd.CloneDir != "" {
		clonedPath, report, err := cmd.applyClone(context.Background())
		if err != nil {
			return fmt.Errorf("clone-dir: %w", err)
		}
		cloneReport = report
		// --path wins if the user set it explicitly; --clone-dir is
		// the "auto-fill" path. We check cmd.Path here — kong leaves
		// it empty when the user didn't pass the flag.
		if cmd.Path == "" {
			cmd.Path = clonedPath
		} else if !cmd.Quiet {
			// User passed both flags; note the override. Gated by
			// --quiet because that flag's contract is "no stderr
			// output" for automation callers who've made an
			// intentional choice.
			fmt.Fprintf(os.Stderr, "# clone-dir: cloned to %s but --path=%s wins\n", clonedPath, cmd.Path)
		}
	}

	// Language stays as whatever precheck detected (or "" if
	// undetected / no precheck). inferTemplateName maps "" to the
	// generic security template — no hardcoded Python default.

	resolver, err := cmd.buildResolver()
	if err != nil {
		return err
	}

	templateName := cmd.Template
	if templateName == "" {
		templateName = inferTemplateName(cmd.Role, cmd.Language)
	}

	rc, source, embedded, err := resolver.OpenTemplate(templateName)
	if err != nil {
		return fmt.Errorf("load template: %w", err)
	}
	defer rc.Close() //nolint:errcheck // template reader close; errors here are not actionable after the read below
	raw, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("read template %s: %w", source, err)
	}

	// Validate --analysis-session-id BEFORE we open anything else
	// or invoke network precheck side-effects — a typo'd session id
	// should fail cheaply. The subsequent HandoffSubstitutions call
	// simply threads the id through into the SESSION_INSTRUCTION
	// placeholder; this block is the place where correctness is
	// enforced.
	if cmd.AnalysisSessionID != "" {
		if err := cmd.validateAnalysisSession(context.Background(), globals); err != nil {
			return err
		}
	}

	subs, err := config.HandoffSubstitutions(cmd.Target, config.HandoffOverrides{
		Name:              cmd.Name,
		URL:               cmd.URL,
		Path:              cmd.Path,
		Role:              cmd.TargetRole,
		Ecosystem:         cmd.Ecosystem,
		Intake:            cmd.Intake,
		Version:           cmd.requestedVersion,
		AnalysisSessionID: cmd.AnalysisSessionID,
	})
	if err != nil {
		return err
	}

	// Surface ecosystem-required roles before render time so the user
	// doesn't silently get a handoff with `{ECOSYSTEM}` literal.
	if cmd.Role == "provenance" && subs["ECOSYSTEM"] == "" {
		return fmt.Errorf("provenance role requires --ecosystem (one of: pypi, npm, crates, go)")
	}

	// Synthesist role needs the evidence rollup assembled from the
	// store — the synthesis-v1 template embeds it as the body of a
	// fenced JSON block under {EVIDENCE_JSON}. This is the M6c wiring
	// that turns the synthesist from a store-browsing agent into a
	// self-contained one (agent-facing-contract §3.5). Other roles
	// don't need store access; leaving the map untouched keeps the
	// security handoff path offline.
	if cmd.Role == "synthesist" {
		evidenceJSON, err := cmd.assembleSynthesisEvidence(context.Background(), globals)
		if err != nil {
			return err
		}
		subs["EVIDENCE_JSON"] = evidenceJSON
	}

	// Provenance role receives the cached Layer-1 signals block as
	// ground truth, to eliminate the re-derivation antipattern
	// (design/ImproveProvSignals.md §Phase 1). Unlike synthesist's
	// evidence, signal absence is a soft case — a fresh target with
	// no cached signals gets a fallback marker instead of a hard
	// error so the agent can fall back to collecting from scratch
	// against the clone.
	if cmd.Role == "provenance" {
		signalsBlock, err := cmd.assembleProvenanceSignals(context.Background(), globals)
		if err != nil {
			return err
		}
		subs["LAYER_1_SIGNALS"] = signalsBlock
	}

	rendered, unfilled := config.RenderTemplate(raw, subs)

	// Destination fork. Three mutually exclusive sinks, already
	// validated up top:
	//
	//   --deposit-to SID  → POST to the pipeline service as a
	//                       'handoff' message on that session.
	//   --output FILE     → write rendered bytes to a file.
	//   (neither)         → write rendered bytes to stdout.
	if cmd.DepositTo != "" {
		if err := cmd.depositRendered(rendered); err != nil {
			return err
		}
	} else {
		if err := writeHandoff(cmd.Output, cmd.Force, rendered); err != nil {
			return err
		}
	}

	if !cmd.Quiet {
		reportToStderr(source, embedded, unfilled)
		if precheckReport != "" {
			fmt.Fprint(os.Stderr, precheckReport)
		}
		if cloneReport != "" {
			fmt.Fprint(os.Stderr, cloneReport)
		}
		if cmd.DepositTo != "" {
			fmt.Fprintf(os.Stderr, "# deposited: session=%s role=%s bytes=%d\n",
				cmd.DepositTo, cmd.Role, len(rendered))
		}
	}
	return nil
}

// depositRendered POSTs the rendered handoff to the pipeline service
// as a 'handoff' message on the named session. Role comes from the
// already-validated cmd.Role positional (kong's enum tag ensures it's
// one of security / provenance / synthesist).
//
// Builds a fresh pipeline client per invocation — handoff commands
// are short-lived (one per skill step), so client reuse buys nothing
// and a per-call construction keeps the signature simple.
func (cmd *HandoffCmd) depositRendered(rendered []byte) error {
	client, err := pipeline.NewClient(cmd.PipelineURL)
	if err != nil {
		return fmt.Errorf("open pipeline client: %w", err)
	}
	_, err = client.DepositMessage(context.Background(),
		cmd.DepositTo, cmd.Role, "handoff", string(rendered), "")
	if err != nil {
		return fmt.Errorf("handoff deposit: %w", err)
	}
	return nil
}

// assembleSynthesisEvidence opens the store, resolves cmd.Target to
// its canonical URI, and composes the structured evidence rollup
// that the synthesist-v1 template embeds under {EVIDENCE_JSON}.
// Returns the pretty-printed JSON body as a string for direct
// substitution into the template.
//
// Failure modes:
//
//   - Target doesn't parse as a recognized form → usage error, no
//     store read.
//   - Target has no entity in the store or zero non-synthesis
//     analyses → refuse to emit a synthesist handoff. Dispatching a
//     synthesist against empty evidence produces either a no-op or a
//     fabricated synthesis; catching the empty case here fails fast
//     with a message that tells the operator the right next step
//     (run /analyze on the target first).
//
// Pretty-prints with two-space indent. The handoff body travels via
// WebFetch to the synthesist agent; compact JSON saves bytes but
// hurts token-level attention across a multi-kilobyte evidence
// block.
// validateAnalysisSession opens the store and confirms
// cmd.AnalysisSessionID names a session that exists and is still
// in_progress. Fails at handoff time so that typos / already-closed
// ids surface here rather than getting threaded into a handoff body
// that the dispatched subagent only discovers is broken when its
// signatory_ingest_analysis call bounces off the FK constraint.
//
// Wraps with NewUsageError so the exit code reflects the
// caller-input nature of the mistake.
func (cmd *HandoffCmd) validateAnalysisSession(ctx context.Context, globals *Globals) error {
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return fmt.Errorf("open store for session validation: %w", err)
	}
	defer s.Close() //nolint:errcheck // store close on function exit; errors not actionable

	sess, err := s.GetAnalysisSession(ctx, cmd.AnalysisSessionID)
	if errors.Is(err, store.ErrNotFound) {
		return NewUsageError(fmt.Errorf(
			"analysis session %q not found; run `signatory analysis begin` to create one",
			cmd.AnalysisSessionID))
	}
	if err != nil {
		return fmt.Errorf("look up analysis session: %w", err)
	}
	if sess.Status.IsTerminal() {
		return NewUsageError(fmt.Errorf(
			"analysis session %q is already %s (terminal); begin a new session for a re-run",
			cmd.AnalysisSessionID, sess.Status))
	}
	return nil
}

func (cmd *HandoffCmd) assembleSynthesisEvidence(ctx context.Context, globals *Globals) (string, error) {
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return "", fmt.Errorf("open store for synthesis evidence: %w", err)
	}
	defer s.Close() //nolint:errcheck // store close on function exit; errors not actionable

	resolved, err := profile.ResolveTarget(cmd.Target)
	if err != nil {
		return "", NewUsageError(fmt.Errorf(
			"synthesist handoff: cannot resolve target %q: %w",
			cmd.Target, err))
	}

	// Build the lookup URI. Two complications:
	//
	//  1. cmd.Target was rewritten to the bare clone URL by Run()
	//     so applyClone could feed it to git clone. That means
	//     resolved.CanonicalURI here is the version-stripped form
	//     (e.g., repo:github/X/Y) even when the original user
	//     input had @V. cmd.requestedVersion still carries the
	//     version, so we re-attach it for the lookup.
	//  2. Different storage paths land at different URIs. Today's
	//     ingest path stores analyst outputs at the FULL URI
	//     (with @V); a Plan-A-canonicalized store would put them
	//     at the BASE. Try FULL first, fall back to BASE.
	//
	// Without (1), versioned /analyze runs hit "no entity matches"
	// even though the analysts' outputs are right there in the
	// store under the versioned URI — the user's halted dogfood
	// run on testify@v1.11.1 surfaced exactly this bug.
	lookupURI := resolved.CanonicalURI
	if cmd.requestedVersion != "" && !strings.Contains(lookupURI, "@") {
		lookupURI = lookupURI + "@" + cmd.requestedVersion
	}
	assembler := synthesis.New(s)
	evidence, err := assembler.Assemble(ctx, lookupURI)
	if errors.Is(err, synthesis.ErrEntityNotFound) {
		if baseURI, version := profile.SplitURIVersion(lookupURI); version != "" && baseURI != lookupURI {
			lookupURI = baseURI
			evidence, err = assembler.Assemble(ctx, lookupURI)
		}
	}
	if err != nil {
		if errors.Is(err, synthesis.ErrEntityNotFound) {
			return "", fmt.Errorf(
				"synthesist handoff: no entity matches %q in the store. "+
					"Run /analyze on this target first so the security and "+
					"provenance analysts can populate the evidence the "+
					"synthesist will consume",
				resolved.CanonicalURI)
		}
		return "", fmt.Errorf("assemble synthesis evidence: %w", err)
	}

	if len(evidence.Analyses) == 0 {
		return "", fmt.Errorf(
			"synthesist handoff: entity %q has no non-synthesis analyses to synthesize. "+
				"Run /analyze on this target so the security and provenance "+
				"analysts deposit conclusions first",
			resolved.CanonicalURI)
	}

	raw, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal synthesis evidence: %w", err)
	}
	return string(raw), nil
}

// provenanceSignalsFallback is the placeholder text substituted for
// {LAYER_1_SIGNALS} when the store has nothing cached for this target
// — a fresh target, or a target whose entity exists but has no
// signals yet. Written as a markdown-formatted note so it reads
// clearly in the handoff body. Kept as a package-level const so the
// tests can assert on it without duplicating the string.
const provenanceSignalsFallback = "_No cached Layer-1 signals for this target — collect from scratch per Standard Methodology. This is expected for a fresh target signatory hasn't analyzed before._"

// assembleProvenanceSignals returns a JSON-formatted block of the
// cached Layer-1 signals for cmd.Target, ready to substitute into the
// provenance handoff's {LAYER_1_SIGNALS} placeholder. Wraps the
// signal payload in a `{"signals": ..., "collected_for": "<uri>"}`
// envelope so the agent can cite both the facts and the URI they
// were cached under.
//
// Signal absence is a soft case. Unlike assembleSynthesisEvidence
// (which errors when the store has nothing, because the synthesist
// has no alternative path), provenance falls back to a marked-up
// text message that tells the agent "no cache; collect yourself."
// The agent still has the clone on disk + network access for
// non-cached sources. Errors are reserved for cases where the store
// access itself fails (open failure, scan error) — those genuinely
// need to surface.
//
// Target resolution mirrors assembleSynthesisEvidence's handling:
// resolve through profile.ResolveTarget, tolerate failure (fall back
// gracefully), attempt lookup at the canonical URI, fall back to the
// version-stripped base URI if the versioned form isn't in the
// store.
func (cmd *HandoffCmd) assembleProvenanceSignals(ctx context.Context, globals *Globals) (string, error) {
	// Graceful-degrade philosophy: the signals block is an
	// optimization that eliminates re-derivation by the agent, not
	// a prerequisite for a correct handoff. Any failure path below
	// (store unreachable, entity missing, no signals cached, URI
	// unresolvable) returns the fallback marker — the agent sees a
	// clearly-labeled "no cached signals" note and falls back to
	// collecting from scratch per the Standard Methodology. Only
	// truly programmer-error conditions (JSON marshal failure of a
	// structure we just built) propagate as errors.
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return provenanceSignalsFallback, nil
	}
	defer s.Close() //nolint:errcheck // store close on function exit; errors not actionable

	resolved, rerr := profile.ResolveTarget(cmd.Target)
	if rerr != nil {
		// Non-URI / non-shorthand targets (bare paths etc.) don't
		// resolve to a canonical URI and therefore have no entity
		// to look up.
		return provenanceSignalsFallback, nil
	}

	lookupURI := resolved.CanonicalURI
	if cmd.requestedVersion != "" && !strings.Contains(lookupURI, "@") {
		lookupURI = lookupURI + "@" + cmd.requestedVersion
	}

	entity, err := s.FindEntityByURI(ctx, lookupURI)
	if errors.Is(err, store.ErrNotFound) {
		// Try the version-stripped form. Signals are collected per
		// entity, and a pkg:npm/X@V with nothing at the versioned
		// URI may have signals at pkg:npm/X (the unversioned
		// canonical form). Mirrors the Plan-A fallback in
		// assembleSynthesisEvidence.
		if baseURI, version := profile.SplitURIVersion(lookupURI); version != "" && baseURI != lookupURI {
			entity, err = s.FindEntityByURI(ctx, baseURI)
			if errors.Is(err, store.ErrNotFound) {
				return provenanceSignalsFallback, nil
			}
		} else {
			return provenanceSignalsFallback, nil
		}
	}
	if err != nil {
		// Non-ErrNotFound lookup error (DB closed mid-query, scan
		// error, etc.) — degrade silently. The next /analyze run
		// will retry cleanly; the current handoff stays valid.
		return provenanceSignalsFallback, nil
	}

	signals, err := s.GetLatestSignals(ctx, entity.ID)
	if err != nil || len(signals) == 0 {
		return provenanceSignalsFallback, nil
	}

	// Envelope: the signal summary plus the URI the cache was
	// looked up under. The URI is informational — lets the agent
	// note discrepancies if the caller-asked target and the cache
	// URI diverge (unusual, but a diagnostic worth surfacing).
	envelope := struct {
		CollectedFor string                 `json:"collected_for"`
		Signals      profile.SignalsSummary `json:"signals"`
	}{
		CollectedFor: entity.CanonicalURI,
		Signals:      profile.Summarize(signals),
	}
	raw, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal provenance signals: %w", err)
	}
	return string(raw), nil
}

// applyNetworkPrecheck resolves the target to a GitHub owner/name,
// calls the ecosystem detector, and fills cmd.Language / cmd.Ecosystem
// for any field the user left empty. Returns a short stderr-report
// string describing what was detected (or "" when nothing is worth
// reporting, e.g. the user had already set everything).
//
// Behavior:
//   - Target must be a github.com URL (scheme + host checked against
//     whitelisted forms). Other hosts error immediately — we have no
//     detection code for them yet.
//   - Token comes from GITHUB_TOKEN if set; unauthenticated otherwise
//     (60/hour quota, but two calls per run fits comfortably).
//   - A detected ecosystem like "go" overrides an empty --ecosystem;
//     it never clobbers a user-provided value.
//   - A detected language of "Go" maps to --language=go; anything
//     else leaves --language empty so the later default ("python")
//     takes effect. We'll extend this as we add more language
//     variants of the security template.
func (cmd *HandoffCmd) applyNetworkPrecheck(ctx context.Context) (string, error) {
	// Pkg-target pre-resolution: if the caller passed a pkg:<eco>/
	// URI (or an npmjs.com URL that ResolveTarget canonicalized to
	// one), consult the ecosystem resolver registry to find its
	// declared source repository. Precheck is GitHub-only; this
	// bridge lets users paste a pkg URI without hand-translating
	// to the source repo first. Works for any registered ecosystem
	// (npm, go, future pypi/cargo) — the registry dispatches.
	//
	// Security note: for most ecosystems (npm, Go via vanity URLs)
	// the declared source is self-reported by the publisher and
	// not cryptographically bound. A malicious package could
	// declare a famous GitHub URL as its "source." The precheck
	// report below discloses DeclaredSource.SelfReported verbatim
	// so a human skimming the output can sanity-check before
	// trusting the downstream analysis.
	var pkgDisclosure string
	if resolved, rerr := profile.ResolveTarget(cmd.Target); rerr == nil &&
		resolved.Scheme == "pkg" && resolved.Ecosystem != "" {
		// Strip the pkg:<eco>/ prefix to get the ecosystem-internal
		// name. resolved.ShortName drops the scope for scoped npm
		// packages ("@types/node" → "node"), so we reconstruct the
		// full name from the canonical URI. Trailing @version (if
		// present) is stripped — resolvers operate per-package, not
		// per-version.
		nameWithVersion := strings.TrimPrefix(resolved.CanonicalURI, "pkg:"+resolved.Ecosystem+"/")
		name := nameWithVersion
		if atIdx := strings.LastIndex(name, "@"); atIdx > 0 {
			// Only strip @version when the @ is AFTER a character
			// (scoped names start with @ at position 0).
			name = name[:atIdx]
		}

		registry := cmd.EcosystemRegistry
		if registry == nil {
			registry = resolver.Default
		}
		source, err := registry.Resolve(ctx, resolved.Ecosystem, name)
		if err != nil {
			if errors.Is(err, resolver.ErrNoResolver) {
				return "", fmt.Errorf("no source resolver registered for ecosystem %q; pass the source URL explicitly (supported: %v)", resolved.Ecosystem, registry.Ecosystems())
			}
			return "", fmt.Errorf("resolving %s package %q source: %w", resolved.Ecosystem, name, err)
		}
		if source.URL == "" {
			return "", fmt.Errorf("%s package %q declares no resolvable source repository; pass the source URL explicitly", resolved.Ecosystem, name)
		}
		if !looksLikeGitHubURL(source.URL) {
			return "", fmt.Errorf("%s package %q declares source %q; only github.com sources are supported by --network-precheck", resolved.Ecosystem, name, source.URL)
		}
		verifiedNote := "self-reported in registry metadata, not cryptographically verified"
		if !source.SelfReported && source.VerifiedBy != "" {
			verifiedNote = "verified via " + source.VerifiedBy
		}
		pkgDisclosure = fmt.Sprintf("# precheck: %s package %q declares source %s (%s)\n",
			resolved.Ecosystem, name, source.URL, verifiedNote)

		// Rewrite the target so downstream steps (substitution,
		// clone, ecosystem detector) see the github URL. Mirror the
		// Run-level pre-population: fill cmd.URL if empty so the
		// TARGET_URL substitution picks up the resolved source.
		cmd.Target = source.URL
		if cmd.URL == "" {
			cmd.URL = source.URL
		}
	}

	// Reject non-GitHub URLs early. NormalizeGitHubRepoInput is
	// lenient — it accepts any URL with 2+ path segments and
	// would happily parse gitlab.com/foo/bar as owner="gitlab.com"
	// name="foo". The precheck only works with GitHub's API, so
	// we guard URL-form targets with the stricter host check.
	// Bare owner/repo shorthand (no scheme) is allowed through —
	// NormalizeGitHubRepoInput assumes GitHub for those.
	if strings.Contains(cmd.Target, "://") && !looksLikeGitHubURL(cmd.Target) {
		return "", fmt.Errorf("--network-precheck requires a github.com URL; got %q", cmd.Target)
	}

	// Accept both full GitHub URLs and owner/repo shorthand. The
	// NormalizeGitHubRepoInput function handles all forms:
	//   https://github.com/owner/repo
	//   github.com/owner/repo
	//   owner/repo
	//   git@github.com:owner/repo
	// If it can extract owner+name, the target is GitHub-resolvable.
	_, owner, name, err := profile.NormalizeGitHubRepoInput(cmd.Target)
	if err != nil {
		return "", fmt.Errorf("target %q is not a recognizable GitHub reference (expected https://github.com/owner/repo or owner/repo): %w", cmd.Target, err)
	}

	source := cmd.PrecheckSource
	if source == nil {
		// Production path: construct a real github client. Reading the
		// env here (rather than inside the swap-able factory) means
		// tests that don't inject PrecheckSource exercise this exact
		// code, and env-var behavior is discoverable from go-to-definition
		// on applyNetworkPrecheck.
		source = ghclient.NewClient(os.Getenv("GITHUB_TOKEN"))
	}
	detector := ecosystem.NewDetector(source)

	result, err := detector.Detect(ctx, owner, name)
	if err != nil {
		return "", err
	}

	// Ecosystem: fill only if user didn't pass --ecosystem.
	ecoApplied := ""
	if cmd.Ecosystem == "" && result.Primary != ecosystem.EcosystemUnknown {
		cmd.Ecosystem = string(result.Primary)
		ecoApplied = string(result.Primary)
	}

	// Language flavor: fill only if user didn't pass --language.
	// languageToFlavor maps the top ten GitHub languages to stable
	// slugs. For anything else detected (Haskell, Kotlin, etc.) the
	// slug is "" and inferTemplateName routes to the generic security
	// template — which is language-agnostic and correct, unlike the
	// old Python fallback.
	langApplied := ""
	langWarning := ""
	if cmd.Language == "" {
		switch flavor := languageToFlavor(result.Language); {
		case flavor != "":
			cmd.Language = flavor
			langApplied = flavor
		case result.Language != "":
			// Detected a language, but no flavor for it. The generic
			// template handles this correctly; the warning is
			// informational so the user knows what happened.
			langWarning = fmt.Sprintf("# precheck warning: detected language %q has no flavor mapping; using generic security template (consider passing --template for a language-specific review)\n", result.Language)
		}
	}

	return pkgDisclosure + formatPrecheckReport(owner, name, result, ecoApplied, langApplied) + langWarning, nil
}

// defaultCloneTimeout bounds the wall-clock budget applyClone gives
// a single `git clone` invocation. The value is policy, not
// hardening: when it fires, signatory has decided the operator is
// better served by a clear "your clone exceeded N minutes" failure
// than by hanging indefinitely. Picked at 2 minutes because that's
// generous for a full clone of any project signatory typically
// targets (focused libraries and tools — kong, idna, yaml.v3,
// testify) while still well under typical CI step deadlines. If a
// future target outgrows this budget, raise the constant rather
// than silently failing — the timeout exists to bound surprises,
// not to hard-cap legitimate work.
//
// On timeout, applyClone wraps the underlying git error via
// wrapCloneTimeoutError so the operator sees "exceeded signatory's
// 2m0s timeout" rather than the bare "signal: killed" that
// CommandContext+SIGKILL produces.
//
// Distinct from gitenv.WaitDelay, which bounds the *post-kill*
// pipe-drain wait — that's hardening (preventing an indefinite
// hang in cmd.Wait if a grandchild holds the pipes). The clone
// timeout is the deadline policy that triggers the kill in the
// first place.
const defaultCloneTimeout = 2 * time.Minute

// wrapCloneTimeoutError augments err with a human-legible
// explanation when the cause was the defaultCloneTimeout firing
// (and not a parent-context cancellation or some unrelated git
// failure). Pure function — no side effects, no subprocess —
// safe to test in isolation with hand-built contexts.
//
// The "child timed out AND parent is alive" gate distinguishes
// "signatory's clone budget elapsed" from "the caller cancelled
// us" or "the caller's deadline elapsed." When the caller's
// context is already in error, we propagate err unchanged because
// the caller already knows the real cause; blaming our clone
// timeout would be misleading.
//
// Wrapping uses %w so callers can still errors.Is/As against the
// underlying *exec.ExitError or context.DeadlineExceeded chain.
func wrapCloneTimeoutError(target string, cloneCtx, parentCtx context.Context, err error) error {
	if errors.Is(cloneCtx.Err(), context.DeadlineExceeded) && parentCtx.Err() == nil {
		return fmt.Errorf(
			"clone of %q exceeded signatory's %s timeout (pass --path with a pre-cloned tree to bypass): %w",
			target, defaultCloneTimeout, err)
	}
	return err
}

// defaultGitClone is the production git-clone implementation that
// applyClone falls back to when HandoffCmd.RunGitClone is nil. It is
// a regular package-level function (not a var) to remove any
// accidental-mutation surface — tests inject via HandoffCmd.RunGitClone
// field assignment, not via swap-the-global.
//
// Full clone (no --depth=1) is the only supported shape. Handoff is
// invoked exclusively as Step 1 of the /analyze pipeline (see the
// SKILL.md), and Step 1b runs validateExistingClone against the
// planted clone via `signatory analyze --refresh --path …`.
// validateExistingClone rejects shallow clones with ErrShallowClone
// (defending the store from poisoned positive signals — total_commits=1,
// top_author_share=1.0 — that history-dependent collectors emit when
// reading a depth=1 clone). A shallow plant here would hard-fail the
// pipeline at Step 1b on every invocation. The previous --depth=1
// default was vestigial from a pre-pipeline design.
//
// Argv composition lives here so this site owns the policy (`--branch
// <version>` for ref pinning; full history; URL/dest positional ordering).
// Subprocess construction routes through defaultRunGit (collectors.go),
// which gives both this site and the analyze-side ensureCloneAtPath
// identical env-strip + WaitDelay discipline by construction; drift
// between the two would be a silent inconsistency in subprocess
// hardening.
//
// Argv validation upstream: url by safeGitCloneURL (rejects ?, #,
// userinfo, NUL); dest by the containment check in applyClone
// (filepath.Rel against an EvalSymlinks-resolved parent) and
// safeCloneRepoName; version by safeGitVersion (rejects leading -,
// shell metacharacters, path traversal).
func defaultGitClone(ctx context.Context, url, dest, version string) error {
	args := []string{"clone"}
	if version != "" {
		// --branch accepts both tag names and branch names. Git
		// resolves to the named ref and detaches HEAD on a tag, or
		// keeps HEAD on the branch tip. If the ref doesn't exist
		// remotely, git fails with a clear "Remote branch X not
		// found" message and we propagate it.
		args = append(args, "--branch", version)
	}
	args = append(args, url, dest)
	// workdir="" — the dest is in args, not a pre-existing repo to
	// operate against. defaultRunGit honors that by NOT prepending
	// "-C <workdir>" when workdir is empty. See its doc.
	return defaultRunGit(ctx, "", args...)
}

// safeGitVersion validates a git ref string before it's passed to
// `git clone --branch`. Empty input is permitted (it signals
// HEAD-of-default-branch upstream). Non-empty input must:
//
//   - be ≤256 bytes (refs can be long but anything past that is
//     pasted prose, not a ref)
//   - contain only [A-Za-z0-9._+/-] (the ecosystem-native
//     characters across semver, calendar versioning, git tag
//     conventions, and refspec paths like refs/tags/v1.0.0)
//   - NOT begin with `-` (otherwise git interprets it as a flag —
//     the canonical "argument injection" hazard for any CLI that
//     forwards user input to a tool with positional+flag args)
//   - NOT contain `..` (path-traversal in ref name space; valid
//     git ref names disallow this per git-check-ref-format)
//   - NOT begin or end with `/` or `.` (git-check-ref-format)
//
// This is deliberately stricter than full git-check-ref-format
// (which has many edge cases around control characters, ASCII
// '~', '^', etc.) — we only need to admit refs that are well-formed
// version identifiers, branches, or tags as users typically write
// them. Genuinely-named refs that fail this check (e.g. with `~`
// in them) get a clear error and the user can rename or skip.
func safeGitVersion(v string) error {
	if v == "" {
		return nil
	}
	if len(v) > 256 {
		return fmt.Errorf("git ref exceeds 256-byte limit (got %d bytes)", len(v))
	}
	if v[0] == '-' {
		return fmt.Errorf("git ref %q must not begin with '-' (would be parsed as a flag)", v)
	}
	if v[0] == '/' || v[0] == '.' {
		return fmt.Errorf("git ref %q must not begin with %q", v, v[0:1])
	}
	if last := v[len(v)-1]; last == '/' || last == '.' {
		return fmt.Errorf("git ref %q must not end with %q", v, v[len(v)-1:])
	}
	if strings.Contains(v, "..") {
		return fmt.Errorf("git ref %q must not contain '..'", v)
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		ok := (c >= 'A' && c <= 'Z') ||
			(c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == '+' || c == '-' || c == '/'
		if !ok {
			return fmt.Errorf("git ref %q contains invalid character %q at position %d", v, c, i)
		}
	}
	return nil
}

// applyClone clones the target URL with full history into
// CloneDir/<repo-name>/ and returns (clonedPath, stderrReport, error).
// Analogous to applyNetworkPrecheck: encapsulates one pre-render
// side-effect and returns a human-readable report string the caller
// emits to stderr.
//
// Full clone is the only supported shape — see defaultGitClone's doc
// for the chain-integrity rationale. The /analyze pipeline's Step 1b
// runs validateExistingClone against this clone, which would reject
// any shallow plant.
//
// Invariants enforced here:
//   - Target must classify as TargetURL (ClassifyTarget).
//   - CloneDir parent must exist and be writable.
//   - Derived dest must be strictly inside CloneDir (no symlink/.. escapes).
//   - If dest already exists as a directory, reuse it without cloning.
//   - Clone uses a 2-minute context timeout; a cancelled ctx propagates.
func (cmd *HandoffCmd) applyClone(ctx context.Context) (clonedPath, report string, err error) {
	// --clone-dir only makes sense when the target is a URL. If it's a
	// local path the classification already filled TARGET_PATH, and the
	// flag is documented as a no-op for local paths — but we still guard
	// explicitly so a confused invocation fails loudly rather than silently
	// doing nothing.
	if config.ClassifyTarget(cmd.Target) != config.TargetURL {
		return "", "", fmt.Errorf("--clone-dir requires a URL target; %q is not a URL", cmd.Target)
	}

	// Validate the clone URL before doing anything else. This rejects
	// query strings (?upload-pack=evil), fragments, embedded credentials,
	// and null bytes — all forms that can be misinterpreted by git.
	if err := safeGitCloneURL(cmd.Target); err != nil {
		return "", "", fmt.Errorf("unsafe clone URL: %w", err)
	}

	repoName := config.InferNameFromURL(cmd.Target)
	// Validate the inferred name before using it as a directory component.
	// InferNameFromURL uses url.Parse which percent-decodes the path, so a
	// URL with %00 in the repo segment would produce a null byte in the name.
	if err := safeCloneRepoName(repoName); err != nil {
		return "", "", fmt.Errorf("cannot derive safe clone directory name from target %q: %w", cmd.Target, err)
	}

	// Resolve the parent dir to an absolute, symlink-free path before the
	// containment check. filepath.Abs handles relative paths but does NOT
	// resolve symlinks; filepath.EvalSymlinks does. Using the resolved path
	// prevents an attacker-controlled symlink at clone-dir from redirecting
	// the parent to an arbitrary location.
	absParent, err := filepath.Abs(cmd.CloneDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve clone-dir %q: %w", cmd.CloneDir, err)
	}

	// Create the parent if it doesn't exist. This matches the user
	// model of `git clone URL DEST` (which creates DEST) and
	// `signatory init DIR` (which creates DIR). Insisting that the
	// user pre-create the parent was a UX paper-cut surfaced during
	// dogfood — the friction had no defensive value because the
	// writability probe below catches real permission problems.
	//
	// MkdirAll is idempotent: an existing directory is fine, a
	// missing one is created with 0755, an existing FILE at the path
	// errors (which we want — the IsDir check below also catches that
	// case for race conditions).
	// 0o750: owner rwx, group rx, others none. Matches the init-time
	// scaffold dir perms in internal/config/init.go.
	if err := os.MkdirAll(absParent, 0o750); err != nil {
		return "", "", fmt.Errorf("create clone-dir parent %q: %w", absParent, err)
	}
	info, err := os.Stat(absParent)
	if err != nil {
		return "", "", fmt.Errorf("stat clone-dir parent %q: %w", absParent, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("clone-dir %q is not a directory", absParent)
	}
	// Probe writability with a temp file rather than inspecting mode
	// bits — ACLs and mount options can make a 0755 dir unwritable.
	probe, err := os.CreateTemp(absParent, ".signatory-clone-probe-*")
	if err != nil {
		return "", "", fmt.Errorf("clone-dir %q is not writable: %w", absParent, err)
	}
	_ = probe.Close()           // probe was just created; close errors don't affect the writability check
	_ = os.Remove(probe.Name()) // best-effort cleanup of the probe file

	// Resolve all symlinks in the parent so the containment check below
	// compares real filesystem paths. Without this, a symlink at
	// --clone-dir itself could redirect the parent to an arbitrary path
	// and cause the containment check to compare unrelated absolute paths.
	parent, err := filepath.EvalSymlinks(absParent)
	if err != nil {
		return "", "", fmt.Errorf("resolve symlinks in clone-dir %q: %w", absParent, err)
	}

	// Build dest and verify it is strictly under parent (belt-and-suspenders
	// against any edge case where the inferred repo name contains unexpected
	// path components). We do NOT call EvalSymlinks on dest here — it likely
	// doesn't exist yet. The name-validation above (safeCloneRepoName) already
	// rejected names containing path separators, "..", or null bytes; the Rel
	// check below catches any remaining escapes.
	dest := filepath.Join(parent, repoName)
	destClean := filepath.Clean(dest)
	parentClean := filepath.Clean(parent)
	// filepath.Rel returns an error or a path starting with ".." when dest
	// is outside parent.
	rel, err := filepath.Rel(parentClean, destClean)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return "", "", fmt.Errorf("derived clone path %q escapes clone-dir %q; refusing to clone", destClean, parentClean)
	}

	// Skip-if-exists: reuse the directory without pulling or fetching.
	// Intentional design: the analyst may have frozen state, applied
	// patches, or be on a slow network. Silently updating would be
	// surprising and potentially unsafe (new commits since last analysis
	// would silently change the reviewed surface).
	//
	// Lstat (not Stat) is load-bearing: os.Stat follows symlinks, so a
	// pre-existing symlink at destClean pointing outside parent (planted
	// by a prior-compromise attacker, a crashed earlier run, or a shared
	// /tmp with loose permissions) would satisfy IsDir() and be silently
	// returned as "the verified clone". The caller would then propagate
	// that symlink path to TARGET_PATH and analyst agents would operate
	// on the attacker's tree instead of a real clone of the target —
	// attribution without grounding. The string-based filepath.Rel
	// containment check above cannot see that a symlink resolves outside
	// parent, which is why this guard exists here rather than being
	// subsumed by Rel. Refusing loudly is the right outcome; the
	// operator can inspect and remove the symlink if it was legitimate.
	//
	// The reuse note is returned through the report channel (gated by
	// --quiet upstream), not written directly to stderr — consistent
	// with the rest of the precheck/clone reporting.
	if fi, err := os.Lstat(destClean); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", "", fmt.Errorf(
				"refuse to reuse clone destination %q: path is a symlink; remove it or pick a different --clone-dir",
				destClean)
		}
		if fi.IsDir() {
			report := fmt.Sprintf("# clone: %s already exists, reusing\n", destClean)
			// Stale-shallow detection: a leftover --depth=1 clone from
			// before the chain-integrity fix will now silently survive
			// skip-if-exists here, then trip ErrShallowClone in Step 1b's
			// `analyze --refresh --path`. Emit an upstream warning so
			// the operator knows the remediation BEFORE the loud-fail.
			// Errors from cloneIsShallow are absorbed: a probe failure
			// shouldn't block reuse, and the analyze-side validator
			// will surface any underlying problem on its own.
			if shallow, _ := cloneIsShallow(destClean); shallow {
				report += fmt.Sprintf(
					"# warning: %s is shallow (--depth=1); analyze --refresh --path will reject it. Remove the directory or run `signatory analyze --clone --refresh <target>` to unshallow it in place.\n",
					destClean)
			}
			return destClean, report, nil
		}
	}

	// Shallow clone with a bounded timeout. Use a child context so the
	// timeout doesn't bleed into the rest of the Run() pipeline.
	cloneCtx, cancel := context.WithTimeout(ctx, defaultCloneTimeout)
	defer cancel()

	// Validate the requested git ref before passing to clone. Empty
	// is fine (HEAD-of-default-branch); non-empty must pass shape
	// checks to keep shell-meta and flag-injection out of `git
	// clone --branch`. See safeGitVersion for the rule set.
	if err := safeGitVersion(cmd.requestedVersion); err != nil {
		return "", "", fmt.Errorf("unsafe git ref: %w", err)
	}

	clone := cmd.RunGitClone
	if clone == nil {
		clone = defaultGitClone
	}
	if err := clone(cloneCtx, cmd.Target, destClean, cmd.requestedVersion); err != nil {
		return "", "", wrapCloneTimeoutError(cmd.Target, cloneCtx, ctx, err)
	}

	if cmd.requestedVersion != "" {
		report = fmt.Sprintf("# clone: %s @ %s → %s\n",
			cmd.Target, cmd.requestedVersion, destClean)
	} else {
		report = fmt.Sprintf("# clone: %s → %s\n", cmd.Target, destClean)
	}
	return destClean, report, nil
}

// looksLikeGitHubURL returns true when the target string starts with
// an http(s) scheme whose host is github.com. Bare "owner/repo"
// inputs are rejected here because the classifier in
// internal/config treats them as TargetUnknown — we want to force
// URL-form explicitness when the user opts into network calls.
func looksLikeGitHubURL(target string) bool {
	lower := strings.ToLower(target)
	return strings.HasPrefix(lower, "https://github.com/") ||
		strings.HasPrefix(lower, "http://github.com/")
}

// allowedCloneSchemes is the scheme whitelist safeGitCloneURL enforces.
// Empty scheme is admitted because git clone accepts bare local paths
// (e.g., "/var/folders/xx/.../src-repo") and the analyze-path
// integration tests rely on that form — TestCollectorsFor_CloneHappyPath
// uses a t.TempDir() path as the entity URL to avoid network
// dependencies.
//
// ssh:// is deliberately absent. Signatory's production paths normalize
// targets through NormalizeGitHubRepoInput which yields https URLs;
// ssh only arrives here via an adversarial or misconfigured caller,
// and its conventional `git@host` form already collides with the
// embedded-credentials rule. The narrower whitelist keeps the attack
// surface small without regressing any real flow.
var allowedCloneSchemes = map[string]bool{
	"":      true, // bare local path
	"http":  true,
	"https": true,
	"git":   true,
}

// safeGitCloneURL validates that raw is safe to pass as a `git clone` URL
// argument. It parses the URL and rejects any form that could be
// misinterpreted by git or that carries unexpected data:
//
//   - Query strings (?upload-pack=evil) — git's URL parser may interpret
//     query-encoded protocol options differently from the scheme, leading
//     to unexpected behavior or remote code execution via git helpers.
//   - URL fragments (#...) — meaningless to git and a sign of injection.
//   - Userinfo (@user:pass) — credentials should never be embedded in
//     URLs passed to git; they belong in netrc or the credential store.
//   - Null bytes in any component — always a path-injection signal.
//   - Non-whitelisted schemes — file://, javascript:, data:, etc.
//     file:// in particular would clone from an arbitrary local path,
//     bypassing --path's vetting. See allowedCloneSchemes for the
//     admitted set and its rationale.
//
// safeGitCloneURL is a belt-and-suspenders check: it does NOT replace
// looksLikeGitHubURL or the ClassifyTarget guard; it fires on the same
// URL that will reach `git clone` as its argv[1].
func safeGitCloneURL(raw string) error {
	if strings.ContainsRune(raw, 0) {
		return fmt.Errorf("clone URL contains a null byte")
	}
	// Check for query string and fragment on the raw string before parsing.
	// url.Parse sets RawQuery="" for a bare "?" suffix and Fragment="" for a
	// bare "#" suffix, so checking parsed fields misses those forms.
	// Checking the raw string catches both.
	if strings.ContainsRune(raw, '?') {
		return fmt.Errorf("clone URL must not contain a query string; pass a bare repo URL")
	}
	if strings.ContainsRune(raw, '#') {
		return fmt.Errorf("clone URL must not contain a fragment (#...); pass a bare repo URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("clone URL is not parseable: %w", err)
	}
	if u.User != nil {
		return fmt.Errorf("clone URL must not embed credentials; use git's credential store instead")
	}
	if !allowedCloneSchemes[strings.ToLower(u.Scheme)] {
		return fmt.Errorf(
			"clone URL scheme %q is not allowed; expected one of https, http, git, or a bare local path",
			u.Scheme)
	}
	return nil
}

// safeCloneRepoName validates that name — derived from the URL by
// InferNameFromURL — is safe to use as a single directory component
// under the clone parent. It rejects:
//
//   - Empty strings (InferNameFromURL couldn't derive a name).
//   - Names containing path separators (would escape the parent dir).
//   - Names that are "." or ".." (classic traversal).
//   - Names containing null bytes (path-injection on most OSes).
//
// This is a second line of defense behind InferNameFromURL, which uses
// url.Parse and takes the last path segment. InferNameFromURL can still
// produce unsafe names from percent-encoded bytes in crafted URLs.
func safeCloneRepoName(name string) error {
	if name == "" {
		return fmt.Errorf("inferred repo name is empty")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("inferred repo name %q is a reserved path component", name)
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("inferred repo name %q contains a null byte", name)
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("inferred repo name %q contains a path separator", name)
	}
	return nil
}

// languageToFlavor maps GitHub's primary-language string to the
// language flavor slug we use in template filenames. Recognized
// languages return a stable slug; unrecognized languages return ""
// so the caller falls back to the generic security template.
//
// The top ten languages by GitHub usage are mapped here so that
// --network-precheck can route to the right template without
// manual --language overrides. Not all flavors have dedicated
// templates today — inferTemplateName handles the fallback.
func languageToFlavor(primaryLanguage string) string {
	switch strings.ToLower(primaryLanguage) {
	case "go":
		return "go"
	case "python":
		return "python"
	case "rust":
		return "rust"
	case "javascript":
		return "javascript"
	case "typescript":
		return "typescript"
	case "java":
		return "java"
	case "c#":
		return "csharp"
	case "c++":
		return "cpp"
	case "c":
		return "c"
	case "php":
		return "php"
	default:
		return ""
	}
}

// formatPrecheckReport renders the "what the network call found"
// block that lands on stderr. It's informational — it tells the user
// what was detected and which flags it filled in. When nothing was
// applied (e.g., user passed --ecosystem and --language explicitly)
// the report still shows the detection result for transparency.
func formatPrecheckReport(owner, name string, result *ecosystem.DetectionResult, ecoApplied, langApplied string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# precheck(%s/%s): ", owner, name)
	parts := []string{}
	if result.Primary != ecosystem.EcosystemUnknown {
		parts = append(parts, fmt.Sprintf("ecosystem=%s", result.Primary))
	} else {
		parts = append(parts, "ecosystem=unknown")
	}
	if result.Language != "" {
		parts = append(parts, fmt.Sprintf("language=%s", result.Language))
	}
	fmt.Fprintln(&b, strings.Join(parts, " "))

	applied := []string{}
	if ecoApplied != "" {
		applied = append(applied, fmt.Sprintf("--ecosystem=%s", ecoApplied))
	}
	if langApplied != "" {
		applied = append(applied, fmt.Sprintf("--language=%s", langApplied))
	}
	if len(applied) > 0 {
		fmt.Fprintf(&b, "# precheck applied: %s\n", strings.Join(applied, " "))
	}
	return b.String()
}

// buildResolver wires the CLI flags, optional config file, and
// embedded fallback into a single Resolver for this command. The
// config file is loaded explicitly when --config is passed; otherwise
// we look for signatory.config.toml in --project-dir via
// DiscoverAndLoad (absence is OK).
func (cmd *HandoffCmd) buildResolver() (*config.Resolver, error) {
	var cfg *config.Config
	var err error
	switch {
	case cmd.ConfigFile != "":
		cfg, err = config.LoadConfig(cmd.ConfigFile)
	default:
		cfg, err = config.DiscoverAndLoad(cmd.ProjectDir)
	}
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	return &config.Resolver{
		CLITemplateDirs:  cmd.TemplateDir,
		CLIFilestoreDirs: cmd.FilestoreDir,
		Config:           cfg,
		EmbeddedFS:       signatory.EmbeddedTemplates,
		EmbeddedPrefix:   "templates",
		BaseDir:          cmd.ProjectDir,
	}, nil
}

// inferTemplateName maps the role/language CLI arguments to a
// specific template file under handoffs/. Languages with dedicated
// templates (go, python, rust) get their own file; everything else
// falls back to the generic security template. Callers with
// non-standard variants should pass --template.
func inferTemplateName(role, language string) string {
	switch role {
	case "security":
		return inferSecurityTemplate(language)
	case "provenance":
		// Language doesn't fork provenance — the template covers
		// PyPI, npm, crates.io, and Go modules in one file.
		return "handoffs/provenance-review-v1.md"
	case "synthesist":
		// The synthesist reads the signatory store, not source —
		// no language/ecosystem fork needed.
		return "handoffs/synthesis-v1.md"
	default:
		// Enum validation ensures we never reach this; keep the
		// fallthrough explicit to catch programmer error in tests.
		return ""
	}
}

// inferSecurityTemplate returns the security-review template path
// for the given language flavor. Languages with dedicated templates
// get their own file; everything else gets the generic template.
//
// Adding a new language-specific template:
//  1. Create handoffs/security-review-<flavor>-v1.md
//  2. Add the flavor to this switch
//  3. Add the GitHub language string to languageToFlavor
func inferSecurityTemplate(language string) string {
	switch language {
	case "go":
		return "handoffs/security-review-go-v1.md"
	case "python":
		return "handoffs/security-review-v1.md"
	case "rust":
		return "handoffs/security-review-rust-v1.md"
	default:
		// Covers "" (undetected), and recognized flavors like
		// "javascript", "java", etc. that don't have dedicated
		// templates yet.
		return "handoffs/security-review-generic-v1.md"
	}
}

// writeHandoff sends rendered to either stdout (when output is
// empty) or to the named file (respecting --force for overwrite
// safety). Nothing is written if rendered is zero-length — callers
// should treat that as an internal error, but it shouldn't happen
// because RenderTemplate always returns at least the original bytes.
func writeHandoff(output string, force bool, rendered []byte) error {
	if output == "" {
		if _, err := os.Stdout.Write(rendered); err != nil {
			return fmt.Errorf("write stdout: %w", err)
		}
		return nil
	}

	flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !force {
		flag |= os.O_EXCL
	}
	// G304 rationale: output is the user's explicit --output argument
	// (kong's type:"path"); writing to that file is writeHandoff's
	// purpose. G302 rationale: handoff templates are not secrets and
	// users pipe them into agents / inspect them in editors running
	// as different uids; 0o644 is the right default.
	f, err := os.OpenFile(output, flag, 0o644) //nolint:gosec // G304,G302: caller-supplied path, user-facing content, not secrets
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s already exists; pass --force to overwrite", output)
		}
		return fmt.Errorf("open %s: %w", output, err)
	}
	// Defer is a safety net against early returns below. The primary
	// close is explicit after the write so flush errors surface as a
	// real error to the caller — a silent close error on a write path
	// can mean data wasn't actually persisted.
	defer f.Close() //nolint:errcheck // safety net; the primary close is below
	if _, err := f.Write(rendered); err != nil {
		return fmt.Errorf("write %s: %w", output, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", output, err)
	}
	return nil
}

// reportToStderr prints the informational post-run report: which
// template was used, whether the embedded fallback was in play, and
// which `{PLACEHOLDER}` tokens remained unfilled. The report is
// stderr-only so `signatory handoff … > file.md` captures only the
// template content.
func reportToStderr(source string, embedded bool, unfilled []string) {
	fmt.Fprintf(os.Stderr, "# template: %s\n", source)
	if embedded {
		fmt.Fprintln(os.Stderr, "# note: read from embedded fallback; run `signatory init` to materialize ./templates/")
	}
	if len(unfilled) > 0 {
		fmt.Fprintf(os.Stderr, "# unfilled placeholders (pass flags to substitute): %v\n", unfilled)
	}
}
