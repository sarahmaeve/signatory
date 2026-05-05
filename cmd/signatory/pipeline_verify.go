package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/sarahmaeve/signatory/internal/store"
)

// VerifyResult is the JSON contract returned by
// `signatory pipeline verify`. It tells the orchestrator whether
// every expected analyst has landed output for the session.
type VerifyResult struct {
	Status    string            `json:"status"`
	Expected  []string          `json:"expected"`
	Landed    []string          `json:"landed"`
	Missing   []string          `json:"missing"`
	OutputIDs map[string]string `json:"output_ids,omitempty"`
}

// PipelineVerifyCmd checks whether all expected analysts have
// ingested output for an analysis session. Returns structured JSON
// so the orchestrator never parses prose.
//
// See design/deterministic-orchestration.md Proposal #3.
type PipelineVerifyCmd struct {
	SessionID string `arg:"" help:"Analysis session ID to verify."`

	Stdout io.Writer `kong:"-"`
	Stderr io.Writer `kong:"-"`
}

func (cmd *PipelineVerifyCmd) Run(globals *Globals) error {
	stdout, _ := cmd.resolveWriters()
	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}

	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit

	// Load the session — surfaces ErrNotFound as a clear message.
	sess, err := s.GetAnalysisSession(ctx, cmd.SessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("analysis session %q not found", cmd.SessionID)
		}
		return fmt.Errorf("get analysis session: %w", err)
	}

	// List outputs linked to this session.
	outputs, err := s.ListOutputsForSession(ctx, cmd.SessionID)
	if err != nil {
		return fmt.Errorf("list outputs for session: %w", err)
	}

	// Build the landed set and output_ids map.
	landedSet := make(map[string]string, len(outputs)) // analyst_id → output_id
	for _, o := range outputs {
		landedSet[o.AnalystID] = o.OutputID
	}

	expected := make([]string, len(sess.ExpectedAnalysts))
	copy(expected, sess.ExpectedAnalysts)
	sort.Strings(expected)

	var landed, missing []string
	outputIDs := make(map[string]string, len(landedSet))
	for _, analystID := range expected {
		if oid, ok := landedSet[analystID]; ok {
			landed = append(landed, analystID)
			outputIDs[analystID] = oid
		} else {
			missing = append(missing, analystID)
		}
	}

	status := "ready_for_synthesis"
	if len(missing) > 0 {
		status = "missing_analysts"
	}

	result := &VerifyResult{
		Status:    status,
		Expected:  expected,
		Landed:    landed,
		Missing:   missing,
		OutputIDs: outputIDs,
	}
	return writeJSON(stdout, result)
}

func (cmd *PipelineVerifyCmd) resolveWriters() (io.Writer, io.Writer) {
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
