package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/sarahmaeve/signatory/internal/certs"
	"github.com/sarahmaeve/signatory/internal/ecosystem"
	"github.com/sarahmaeve/signatory/internal/ecosystem/resolver"
	"github.com/sarahmaeve/signatory/internal/identity"
	"github.com/sarahmaeve/signatory/internal/pipeline"
	"github.com/sarahmaeve/signatory/internal/profile"
)

// PrepareManifest is the JSON contract returned by
// `signatory pipeline prepare`. Every field the orchestrator needs
// to dispatch analyst agents is pre-computed here so the LLM never
// threads shell variables or parses stdout.
type PrepareManifest struct {
	SessionID         string   `json:"session_id"`
	AnalysisSessionID string   `json:"analysis_session_id"`
	Target            string   `json:"target"`
	TargetName        string   `json:"target_name"`
	TargetURL         string   `json:"target_url"`
	ClonePath         string   `json:"clone_path"`
	HandoffsDeposited []string `json:"handoffs_deposited"`
	SignalsRefreshed  bool     `json:"signals_refreshed"`
	Status            string   `json:"status"`
}

// PipelinePrepareCmd collapses the /analyze skill's Steps 0a–1b
// into a single deterministic command. It accepts a target and
// returns a JSON manifest with every downstream variable pre-
// computed, so the orchestrating LLM never threads shell variables
// or parses stdout.
//
// See design/deterministic-orchestration.md for the motivation.
type PipelinePrepareCmd struct {
	Target           string   `arg:"" help:"Target URI (GitHub URL, pkg URI, owner/repo shorthand)."`
	ExpectedAnalysts []string `name:"expected-analyst" help:"Analyst role ID the pipeline plans to dispatch (repeatable)." optional:""`
	CloneDir         string   `name:"clone-dir" help:"Clone the target into this directory." default:"filestore/clones/" type:"path"`
	PipelineURL      string   `name:"pipeline-url" help:"Override the pipeline service base URL." default:"${pipelineURL}"`

	Stdout io.Writer `kong:"-"`
	Stderr io.Writer `kong:"-"`

	// --- Test injection seams ----------------------------------------

	// CertsChecker overrides the TLS environment check. nil → certs.Check().
	CertsChecker func() certs.CheckResult `kong:"-"`

	// RunGitClone overrides the git clone subprocess. nil → the
	// HandoffCmd's default clone implementation.
	RunGitClone func(ctx context.Context, url, dest, version string) error `kong:"-"`

	// PrecheckSource overrides GitHub-backed ecosystem detection.
	// nil → HandoffCmd builds a real github client from GITHUB_TOKEN.
	PrecheckSource ecosystem.Source `kong:"-"`

	// EcosystemRegistry overrides the package-ecosystem source resolver.
	// nil → HandoffCmd uses resolver.Default.
	EcosystemRegistry *resolver.Registry `kong:"-"`
}

func (cmd *PipelinePrepareCmd) Run(globals *Globals) error {
	stdout, stderr := cmd.resolveWriters()
	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}

	// ── Step 1: Certs preflight ─────────────────────────────────
	checker := cmd.CertsChecker
	if checker == nil {
		checker = certs.Check
	}
	cr := checker()
	if !cr.OK {
		if cr.Fix != "" {
			return fmt.Errorf("certs preflight: %s\nfix: %s", cr.Message, cr.Fix)
		}
		return fmt.Errorf("certs preflight: %s", cr.Message)
	}
	fmt.Fprintln(stderr, "# certs: OK")

	// ── Step 2: Resolve target ──────────────────────────────────
	resolved, err := profile.ResolveTarget(cmd.Target)
	if err != nil {
		return fmt.Errorf("resolve target %q: %w", cmd.Target, err)
	}
	targetName := resolved.ShortName
	targetURL := resolved.CloneURL
	if targetURL == "" {
		targetURL = resolved.CanonicalURI
	}

	// ── Step 3: Create pipeline session ─────────────────────────
	pipelineClient, err := pipeline.NewClient(cmd.PipelineURL)
	if err != nil {
		return fmt.Errorf("open pipeline client: %w", err)
	}
	sess, err := pipelineClient.CreateSession(ctx, cmd.Target, "")
	if err != nil {
		return fmt.Errorf("pipeline session create: %w", err)
	}
	sessionID := sess.ID
	fmt.Fprintf(stderr, "# pipeline session: %s\n", sessionID)

	// ── Step 4: Create analysis session ─────────────────────────
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit

	entity, err := ensureEntity(ctx, s, cmd.Target)
	if err != nil {
		return fmt.Errorf("resolve target entity %q: %w", cmd.Target, err)
	}

	actor, err := identity.Current()
	if err != nil {
		return fmt.Errorf("resolve team identity: %w", err)
	}

	analysisSess := &profile.AnalysisSession{
		ID:                uuid.NewString(),
		EntityID:          entity.ID,
		TargetURI:         cmd.Target,
		InvokedBy:         actor,
		PipelineSessionID: sessionID,
		ExpectedAnalysts:  normalizeExpectedAnalysts(cmd.ExpectedAnalysts),
		StartedAt:         time.Now().UTC(),
		Status:            profile.AnalysisSessionInProgress,
	}
	if err := s.CreateAnalysisSession(ctx, analysisSess); err != nil {
		return fmt.Errorf("create analysis session: %w", err)
	}
	analysisSessionID := analysisSess.ID
	fmt.Fprintf(stderr, "# analysis session: %s\n", analysisSessionID)

	// ── Step 5: Security handoff (clone + render + deposit) ─────
	securityHandoff := &HandoffCmd{
		Role:              "security",
		Target:            cmd.Target,
		CloneDir:          cmd.CloneDir,
		NetworkPrecheck:   true,
		AnalysisSessionID: analysisSessionID,
		DepositTo:         sessionID,
		PipelineURL:       cmd.PipelineURL,
		Quiet:             true,
		RunGitClone:       cmd.RunGitClone,
		PrecheckSource:    cmd.PrecheckSource,
		EcosystemRegistry: cmd.EcosystemRegistry,
	}
	if err := securityHandoff.Run(globals); err != nil {
		return fmt.Errorf("security handoff: %w", err)
	}
	clonePath := securityHandoff.Path
	fmt.Fprintf(stderr, "# security handoff: deposited, clone at %s\n", clonePath)

	// ── Step 6: Provenance handoff (reuse clone + deposit) ──────
	provenanceHandoff := &HandoffCmd{
		Role:              "provenance",
		Target:            cmd.Target,
		Path:              clonePath,
		NetworkPrecheck:   true,
		AnalysisSessionID: analysisSessionID,
		DepositTo:         sessionID,
		PipelineURL:       cmd.PipelineURL,
		Quiet:             true,
		PrecheckSource:    cmd.PrecheckSource,
		EcosystemRegistry: cmd.EcosystemRegistry,
	}
	if err := provenanceHandoff.Run(globals); err != nil {
		return fmt.Errorf("provenance handoff: %w", err)
	}
	fmt.Fprintln(stderr, "# provenance handoff: deposited")

	// ── Step 7: Refresh Layer-1 signals ─────────────────────────
	signalsRefreshed := false
	if clonePath != "" {
		analyzeCmd := &AnalyzeCmd{
			Target:  cmd.Target,
			Refresh: true,
			Path:    clonePath,
			Stdout:  io.Discard,
			Stderr:  stderr,
		}
		if err := analyzeCmd.Run(globals); err != nil {
			return fmt.Errorf("signal refresh: %w", err)
		}
		signalsRefreshed = true
		fmt.Fprintln(stderr, "# signals: refreshed")
	}

	// ── Step 8: Return JSON manifest ────────────────────────────
	manifest := &PrepareManifest{
		SessionID:         sessionID,
		AnalysisSessionID: analysisSessionID,
		Target:            cmd.Target,
		TargetName:        targetName,
		TargetURL:         targetURL,
		ClonePath:         clonePath,
		HandoffsDeposited: []string{"security", "provenance"},
		SignalsRefreshed:  signalsRefreshed,
		Status:            "ready",
	}
	return writeJSON(stdout, manifest)
}

func (cmd *PipelinePrepareCmd) resolveWriters() (io.Writer, io.Writer) {
	stdout := cmd.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := cmd.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	return stdout, stderr
}
