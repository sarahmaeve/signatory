package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover the provenance handoff's {LAYER_1_SIGNALS}
// substitution — now a static MCP-instruction string rather than a
// store-assembled JSON block. The agent retrieves signals at runtime
// via signatory_signals MCP tool, eliminating the render-time store
// dependency and the large inlined JSON that inflated token count.

// TestHandoff_Provenance_MCPInstruction verifies that the rendered
// provenance handoff contains the MCP-instruction placeholder text
// directing the agent to query signatory_signals at runtime.
func TestHandoff_Provenance_MCPInstruction(t *testing.T) {
	g := newTestGlobals(t)

	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:      "provenance",
		Target:    "https://github.com/alecthomas/kong",
		Path:      "/tmp/kong",
		Ecosystem: "go",
		Output:    outPath,
		Quiet:     true,
	}
	require.NoError(t, cmd.Run(g))

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	rendered := string(body)

	// The MCP instruction must be present.
	assert.Contains(t, rendered, "signatory_signals MCP tool",
		"rendered provenance handoff must instruct agent to use signatory_signals")
	assert.Contains(t, rendered, "Do not WebFetch data the store already has",
		"rendered provenance handoff must warn against re-derivation")

	// The old JSON envelope must NOT appear — no store assembly happens.
	assert.NotContains(t, rendered, `"collected_for":`,
		"no JSON signals envelope should be rendered (store assembly removed)")
}

// TestHandoff_Provenance_NoStoreRequired confirms that the provenance
// handoff renders successfully without any store content. Before this
// change, an empty store produced a fallback marker; now the handoff
// is fully offline (no store access needed for provenance role).
func TestHandoff_Provenance_NoStoreRequired(t *testing.T) {
	g := newTestGlobals(t)

	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:      "provenance",
		Target:    "https://github.com/nonexistent/target",
		Path:      "/tmp/nonexistent",
		Ecosystem: "npm",
		Output:    outPath,
		Quiet:     true,
	}
	require.NoError(t, cmd.Run(g))

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	rendered := string(body)

	// Same MCP instruction regardless of store state.
	assert.Contains(t, rendered, "signatory_signals MCP tool",
		"MCP instruction renders even for a target with no store entity")
}

// TestHandoff_Security_NoSignalsBlock confirms that security-role
// handoffs remain unaffected by the provenance-signals wiring. The
// security template has no {LAYER_1_SIGNALS} placeholder.
func TestHandoff_Security_NoSignalsBlock(t *testing.T) {
	g := newTestGlobals(t)

	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://github.com/secure/test",
		Path:     "/tmp/secure",
		Language: "python",
		Output:   outPath,
		Quiet:    true,
	}
	require.NoError(t, cmd.Run(g))

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	rendered := string(body)

	// Security handoff must not carry provenance-specific content.
	assert.NotContains(t, rendered, "signatory_signals MCP tool",
		"security handoff must not carry provenance MCP instruction")
	assert.NotContains(t, rendered, `"collected_for":`,
		"security handoff must not carry signals envelope")
}
