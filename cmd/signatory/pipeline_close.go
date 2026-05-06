package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/identity"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// CloseResult is the JSON contract returned by
// `signatory pipeline close`. It reports whether the posture was
// accepted and the session closed, or just presents the proposal
// for interactive confirmation.
type CloseResult struct {
	Status            string `json:"status"`
	SynthesisOutputID string `json:"synthesis_output_id"`
	ProposedTier      string `json:"proposed_tier"`
	PostureAccepted   bool   `json:"posture_accepted"`
}

// PipelineCloseCmd replaces the manual Steps 5–6 of the /analyze
// skill: it finds the synthesis output in the session, extracts
// the proposed posture, and (with --yes) accepts it + closes the
// session atomically.
//
// See design/deterministic-orchestration.md Proposal #4.
type PipelineCloseCmd struct {
	SessionID string `arg:"" help:"Analysis session ID to close."`
	Status    string `name:"status" help:"Terminal status (completed, failed, partial)." default:"completed"`
	Yes       bool   `help:"Accept the proposed posture without interactive confirmation." short:"y"`

	Stdout io.Writer `kong:"-"`
	Stderr io.Writer `kong:"-"`
}

func (cmd *PipelineCloseCmd) Run(globals *Globals) error {
	stdout, stderr := cmd.resolveWriters()
	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}

	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit

	// ── Step 1: Load the session ────────────────────────────────
	_, err = s.GetAnalysisSession(ctx, cmd.SessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("analysis session %q not found", cmd.SessionID)
		}
		return fmt.Errorf("get analysis session: %w", err)
	}

	// ── Step 2: Find the synthesis output ───────────────────────
	outputs, err := s.ListOutputsForSession(ctx, cmd.SessionID)
	if err != nil {
		return fmt.Errorf("list outputs for session: %w", err)
	}

	var synthOutputID string
	for _, o := range outputs {
		if exchange.IsSynthesistRole(o.AnalystID) {
			synthOutputID = o.OutputID
			break
		}
	}
	if synthOutputID == "" {
		return fmt.Errorf("no synthesis output found in session %q", cmd.SessionID)
	}
	fmt.Fprintf(stderr, "# synthesis output: %s\n", synthOutputID)

	// ── Step 3: Load the proposed posture ───────────────────────
	proposal, err := s.GetSynthesisProposal(ctx, synthOutputID)
	if err != nil {
		return fmt.Errorf("get synthesis proposal: %w", err)
	}

	// ── Step 4: Dry-run path (no --yes) ─────────────────────────
	if !cmd.Yes {
		result := &CloseResult{
			Status:            "proposal",
			SynthesisOutputID: synthOutputID,
			ProposedTier:      proposal.Tier,
			PostureAccepted:   false,
		}
		return writeJSON(stdout, result)
	}

	// ── Step 5: Accept posture ──────────────────────────────────
	entity, err := s.GetOutputEntity(ctx, synthOutputID)
	if err != nil {
		return fmt.Errorf("get output entity: %w", err)
	}

	actor, err := identity.Current()
	if err != nil {
		return fmt.Errorf("resolve team identity: %w", err)
	}

	posture := &profile.Posture{
		EntityID:  entity.ID,
		Tier:      profile.PostureTier(proposal.Tier),
		Version:   proposal.VersionScope,
		Rationale: proposal.RationaleSummary,
		SetBy:     actor,
		SetAt:     time.Now().UTC(),
	}
	if err := s.SetPosture(ctx, posture); err != nil {
		return fmt.Errorf("accept posture: %w", err)
	}
	fmt.Fprintf(stderr, "# posture accepted: %s → %s\n", entity.CanonicalURI, proposal.Tier)

	// ── Step 6: Close the session ───────────────────────────────
	closeParams := profile.AnalysisSessionCloseParams{
		Status:            profile.AnalysisSessionStatus(cmd.Status),
		EndedAt:           time.Now().UTC(),
		SynthesisOutputID: synthOutputID,
	}
	if err := s.CloseAnalysisSession(ctx, cmd.SessionID, closeParams); err != nil {
		return fmt.Errorf("close analysis session: %w", err)
	}
	fmt.Fprintf(stderr, "# session closed: %s (%s)\n", cmd.SessionID, cmd.Status)

	result := &CloseResult{
		Status:            "closed",
		SynthesisOutputID: synthOutputID,
		ProposedTier:      proposal.Tier,
		PostureAccepted:   true,
	}
	return writeJSON(stdout, result)
}

func (cmd *PipelineCloseCmd) resolveWriters() (io.Writer, io.Writer) {
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
