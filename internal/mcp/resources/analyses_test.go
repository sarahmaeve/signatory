package resources_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/mcp/resources"
)

// loadAnalystFixture reads the shared exchange testdata fixture. The path
// is relative to this package, walking up three levels to the repo root,
// then into internal/exchange/testdata/.
func loadAnalystFixture(t *testing.T) *exchange.AnalystOutput {
	t.Helper()
	path := filepath.Join("..", "..", "exchange", "testdata", "atuin-schema-trial.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "exchange testdata fixture must be readable")
	var out exchange.AnalystOutput
	require.NoError(t, json.Unmarshal(raw, &out))
	return &out
}

func TestAnalysesResource_URIPattern(t *testing.T) {
	t.Parallel()
	r := &resources.AnalysesResource{}
	assert.Equal(t, "signatory://analyses", r.URIPattern())
}

func TestAnalysesResource_EmptyStore(t *testing.T) {
	t.Parallel()
	s := openTestDB(t)
	r := &resources.AnalysesResource{Store: s}

	resp := r.Read(t.Context(), "signatory://analyses")

	require.Equal(t, "ok", resp.Status)
	require.Nil(t, resp.Error)

	raw := mustMarshal(t, resp.Data)
	var decoded struct {
		Outputs []any `json:"outputs"`
		Total   int   `json:"total"`
	}
	require.NoError(t, unmarshal(raw, &decoded))
	assert.Equal(t, 0, decoded.Total)
	assert.Empty(t, decoded.Outputs)
}

func TestAnalysesResource_HappyPath_NoFilter(t *testing.T) {
	t.Parallel()
	s := openTestDB(t)
	ctx := context.Background()

	fixture := loadAnalystFixture(t)
	_, err := s.IngestAnalystOutput(ctx, fixture, "test-fixture")
	require.NoError(t, err)

	r := &resources.AnalysesResource{Store: s}
	resp := r.Read(ctx, "signatory://analyses")

	require.Equal(t, "ok", resp.Status)
	raw := mustMarshal(t, resp.Data)
	var decoded struct {
		Outputs []struct {
			OutputID  string `json:"OutputID"`
			EntityURI string `json:"EntityURI"`
		} `json:"outputs"`
		Total int `json:"total"`
	}
	require.NoError(t, unmarshal(raw, &decoded))
	assert.Equal(t, 1, decoded.Total)
	assert.Len(t, decoded.Outputs, 1)
}

func TestAnalysesResource_WithTargetFilter_Matching(t *testing.T) {
	t.Parallel()
	s := openTestDB(t)
	ctx := context.Background()

	fixture := loadAnalystFixture(t)
	_, err := s.IngestAnalystOutput(ctx, fixture, "test-fixture")
	require.NoError(t, err)

	r := &resources.AnalysesResource{Store: s}

	// Use the fixture's canonical target URI as the filter.
	target := fixture.Target
	uri := "signatory://analyses?target=" + target
	resp := r.Read(ctx, uri)

	require.Equal(t, "ok", resp.Status, "matching target should return ok")
	raw := mustMarshal(t, resp.Data)
	var decoded struct {
		Total int `json:"total"`
	}
	require.NoError(t, unmarshal(raw, &decoded))
	assert.Equal(t, 1, decoded.Total, "exactly one output for the fixture target")
}

func TestAnalysesResource_WithTargetFilter_NoMatch(t *testing.T) {
	t.Parallel()
	s := openTestDB(t)
	ctx := context.Background()

	fixture := loadAnalystFixture(t)
	_, err := s.IngestAnalystOutput(ctx, fixture, "test-fixture")
	require.NoError(t, err)

	r := &resources.AnalysesResource{Store: s}

	// A valid canonical URI that doesn't exist in the store.
	resp := r.Read(ctx, "signatory://analyses?target=pkg:npm/totally-unknown-pkg")
	require.Equal(t, "error", resp.Status, "unknown target should return error")
	require.NotNil(t, resp.Error)
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code)
}

func TestAnalysesResource_WithTargetFilter_EmptyParam(t *testing.T) {
	t.Parallel()
	s := openTestDB(t)
	ctx := context.Background()

	fixture := loadAnalystFixture(t)
	_, err := s.IngestAnalystOutput(ctx, fixture, "test-fixture")
	require.NoError(t, err)

	r := &resources.AnalysesResource{Store: s}

	// ?target= with empty value → treated as no filter → all outputs.
	resp := r.Read(ctx, "signatory://analyses?target=")
	require.Equal(t, "ok", resp.Status)
	raw := mustMarshal(t, resp.Data)
	var decoded struct {
		Total int `json:"total"`
	}
	require.NoError(t, unmarshal(raw, &decoded))
	assert.Equal(t, 1, decoded.Total)
}

func TestAnalysesResource_MalformedURI_ReturnsSchemaError(t *testing.T) {
	t.Parallel()
	s := openTestDB(t)
	r := &resources.AnalysesResource{Store: s}

	// A URI that net/url.Parse will reject.
	resp := r.Read(t.Context(), "://\x00bad")
	require.Equal(t, "error", resp.Status)
	require.NotNil(t, resp.Error)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
}

// TestAnalysesResource_MutationVerify_IngestAppearsInListing is the
// mutation-verification test: ingesting a second analyst output causes
// the total count to increase from 1 to 2.
func TestAnalysesResource_MutationVerify_IngestAppearsInListing(t *testing.T) {
	t.Parallel()
	s := openTestDB(t)
	ctx := context.Background()

	fixture := loadAnalystFixture(t)

	// First ingest.
	_, err := s.IngestAnalystOutput(ctx, fixture, "first")
	require.NoError(t, err)

	r := &resources.AnalysesResource{Store: s}

	resp1 := r.Read(ctx, "signatory://analyses")
	var d1 struct {
		Total int `json:"total"`
	}
	require.NoError(t, unmarshal(mustMarshal(t, resp1.Data), &d1))
	assert.Equal(t, 1, d1.Total, "mutation-verify: before second ingest, total must be 1")

	// Mutate the fixture slightly so it's not an idempotent re-ingest:
	// change the analyst ID to produce a new content hash.
	fixture2 := *fixture
	fixture2.Attribution.AnalystID = "different-analyst-mutation-test"
	_, err = s.IngestAnalystOutput(ctx, &fixture2, "second")
	require.NoError(t, err)

	resp2 := r.Read(ctx, "signatory://analyses")
	var d2 struct {
		Total int `json:"total"`
	}
	require.NoError(t, unmarshal(mustMarshal(t, resp2.Data), &d2))
	assert.Equal(t, 2, d2.Total, "mutation-verify: after second ingest, total must be 2")
}
