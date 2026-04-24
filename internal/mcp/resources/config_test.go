package resources_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/mcp/resources"
)

// URIPattern() is covered by the registration contract test in
// cmd/signatory (TestMCPRegistration_Contract).

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

	// None of these strings should appear as JSON keys in the config
	// payload. The list is intentionally broad: each category
	// (auth, session, key material) has multiple common spellings so
	// a developer adding a new secret-shaped field is likely to trip
	// one. If a legitimate field name collides with an entry here,
	// prefer renaming the field over removing the entry.
	forbiddenKeys := []string{
		// Keys / tokens
		"api_key", "apikey", "access_key", "private_key", "signing_key",
		"ssh_key", "gpg_key",
		// Secrets / credentials
		"secret", "client_secret", "credential", "password", "passwd",
		// Auth / tokens / bearers
		"token", "refresh_token", "access_token", "bearer", "authorization",
		// Session / cookie material
		"session", "cookie",
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

// TestConfigResource_URIIgnored verifies the URI argument has no effect
// on the response payload. Two Reads with different query strings must
// produce byte-identical Data — a regression that started routing
// config content by query param would fail the equality check.
func TestConfigResource_URIIgnored(t *testing.T) {
	t.Parallel()
	r := &resources.ConfigResource{
		DBPath:  "/tmp/test.db",
		Version: "0.1.0-test",
	}

	respA := r.Read(t.Context(), "signatory://config")
	respB := r.Read(t.Context(), "signatory://config?any=param&another=x")

	require.Equal(t, "ok", respA.Status)
	require.Equal(t, "ok", respB.Status)

	assert.Equal(t, mustMarshal(t, respA.Data), mustMarshal(t, respB.Data),
		"ConfigResource.Read must produce identical Data regardless of URI query params")
}
