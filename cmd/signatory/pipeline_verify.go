package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/sarahmaeve/signatory/internal/profile"
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

	result, err := verifyAnalystLanding(ctx, s, cmd.SessionID)
	if err != nil {
		return err
	}
	return writeJSON(stdout, result)
}

// verifyAnalystLanding computes the landed/missing rollup for an
// analysis session. Extracted from PipelineVerifyCmd.Run so callers
// that compose verification with later stages — the orchestrator
// command in pipeline_run.go — can branch on the structured result
// instead of parsing the JSON they themselves emitted.
//
// Returns a clear "not found" error when the session id is unknown
// (ErrNotFound is wrapped); other store errors propagate verbatim
// with context.
func verifyAnalystLanding(
	ctx context.Context,
	s storeReader,
	sessionID string,
) (*VerifyResult, error) {
	sess, err := s.GetAnalysisSession(ctx, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("analysis session %q not found", sessionID)
		}
		return nil, fmt.Errorf("get analysis session: %w", err)
	}

	outputs, err := s.ListOutputsForSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list outputs for session: %w", err)
	}

	landedSet := make(map[string]string, len(outputs))
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

	return &VerifyResult{
		Status:    status,
		Expected:  expected,
		Landed:    landed,
		Missing:   missing,
		OutputIDs: outputIDs,
	}, nil
}

// storeReader is the narrow read surface verifyAnalystLanding needs.
// Keeping the signature small — instead of taking the full store.Store
// — documents that this helper is read-only and avoids dragging the
// write surface into callers' test mocks.
type storeReader interface {
	GetAnalysisSession(ctx context.Context, id string) (*profile.AnalysisSession, error)
	ListOutputsForSession(ctx context.Context, sessionID string) ([]store.AnalystOutputSummary, error)
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
