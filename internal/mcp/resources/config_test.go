package resources_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/mcp/resources"
)

func TestConfigResource_URIPattern(t *testing.T) {
	t.Parallel()
	r := &resources.ConfigResource{}
	assert.Equal(t, "signatory://config", r.URIPattern())
}

func TestConfigResource_HappyPath(t *testing.T) {
	t.Parallel()
	r := &resources.ConfigResource{
		DBPath:  "/home/user/.signatory/signatory.db",
		Version: "0.1.0-test",
	}

	resp := r.Read(t.Context(), "signatory://config")

	require.Equal(t, "ok", resp.Status)
	require.Nil(t, resp.Error)
	// Metadata.ServerVersion is intentionally empty here — the Server's
	// dispatch layer stamps it at emission time, so handlers called
	// directly (as in this unit test) see the zero value. The
	// server-level test in internal/mcp/server_test.go verifies the
	// stamped value.

	raw := mustMarshal(t, resp.Data)
	var decoded struct {
		MCPVersion            string `json:"mcp_version"`
		Transport             string `json:"transport"`
		DirectAPIActivated    bool   `json:"direct_api_activated"`
		LLMSynthesisAvailable bool   `json:"llm_synthesis_available"`
		DBPath                string `json:"db_path"`
	}
	require.NoError(t, unmarshal(raw, &decoded))

	assert.Equal(t, "0.1.0-test", decoded.MCPVersion, "MCPVersion comes from the injected Version field")
	assert.Equal(t, "stdio", decoded.Transport, "v0.1 transport must be stdio")
	assert.False(t, decoded.DirectAPIActivated, "direct API must be false in v0.1")
	assert.True(t, decoded.LLMSynthesisAvailable, "LLM synthesis must be available")
	assert.Equal(t, "/home/user/.signatory/signatory.db", decoded.DBPath)
}

func TestConfigResource_EmptyDBPath(t *testing.T) {
	t.Parallel()
	// DBPath empty (e.g. in tests that don't need it) — must not error.
	r := &resources.ConfigResource{}

	resp := r.Read(t.Context(), "signatory://config")
	require.Equal(t, "ok", resp.Status)

	raw := mustMarshal(t, resp.Data)
	var decoded struct {
		DBPath string `json:"db_path"`
	}
	require.NoError(t, unmarshal(raw, &decoded))
	assert.Empty(t, decoded.DBPath)
}

// TestConfigResource_NoSecretsInResponse verifies that the response
// does not contain any of the field names commonly used for secrets.
// This is a canary-style test: if a developer adds an API key or token
// field to configData, this test will catch it at review time.
//
// Security: the config resource is consumed by LLM agents. Any secret
// that appears here ends up in the model's context window. Defence must
// be structural.
func TestConfigResource_NoSecretsInResponse(t *testing.T) {
	t.Parallel()
	r := &resources.ConfigResource{DBPath: "/tmp/test.db"}

	resp := r.Read(t.Context(), "signatory://config")
	raw := mustMarshal(t, resp.Data)
	payload := string(raw)

	// None of these strings should appear as JSON keys in the config payload.
	forbiddenKeys := []string{
		"api_key", "apikey", "secret", "token", "credential",
		"password", "passwd", "private_key",
	}
	for _, key := range forbiddenKeys {
		assert.NotContains(t, payload, `"`+key+`"`,
			"config response must not contain secret field %q", key)
	}
}

// TestConfigResource_MutationVerify_DBPathReflectsConstructorValue verifies
// that the DBPath in the response comes from the struct field, not a
// hardcoded string. Changing the field changes the output.
func TestConfigResource_MutationVerify_DBPathReflectsConstructorValue(t *testing.T) {
	t.Parallel()
	r1 := &resources.ConfigResource{DBPath: "/path/to/db-a"}
	r2 := &resources.ConfigResource{DBPath: "/path/to/db-b"}

	resp1 := r1.Read(t.Context(), "signatory://config")
	resp2 := r2.Read(t.Context(), "signatory://config")

	raw1 := mustMarshal(t, resp1.Data)
	raw2 := mustMarshal(t, resp2.Data)

	var d1, d2 struct {
		DBPath string `json:"db_path"`
	}
	require.NoError(t, unmarshal(raw1, &d1))
	require.NoError(t, unmarshal(raw2, &d2))

	assert.NotEqual(t, d1.DBPath, d2.DBPath,
		"mutation-verify: different DBPath fields must produce different db_path values")
	assert.Equal(t, "/path/to/db-a", d1.DBPath)
	assert.Equal(t, "/path/to/db-b", d2.DBPath)
}

// TestConfigResource_URIIgnored verifies the uri argument is ignored.
func TestConfigResource_URIIgnored(t *testing.T) {
	t.Parallel()
	r := &resources.ConfigResource{}
	resp := r.Read(t.Context(), "signatory://config?any=param")
	assert.Equal(t, "ok", resp.Status)
}
