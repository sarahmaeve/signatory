package invariants

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fenceMarker is the distinctive opening sentence of the independence
// rule that must appear in every analyst and synthesist handoff
// template. D9 of the agent-facing contract (and
// design/m6-synthesis-contract.md §4) requires this fence in every
// template that dispatches a reasoning agent. Catching its absence
// deterministically here is cheaper than catching the drift later.
//
// The marker text is the FIRST sentence of the fence — if it's
// missing, the whole fence probably is. We don't check the
// per-template positive guidance that follows (those differ between
// analyst and synthesist), only the shared opening sentence.
const fenceMarker = "Previous reports do not corroborate new conclusions"

// fenceTemplates enumerates handoff templates that MUST carry the
// independence rule. Every dispatched reasoning agent sees one of
// these. Adding a new handoff template means extending this list in
// the same commit.
var fenceTemplates = []string{
	"templates/handoffs/security-review-v1.md",
	"templates/handoffs/security-review-go-v1.md",
	"templates/handoffs/security-review-rust-v1.md",
	"templates/handoffs/security-review-generic-v1.md",
	"templates/handoffs/provenance-review-v1.md",
	"templates/handoffs/synthesis-v1.md",
}

// TestIndependenceFence_PresentInAllHandoffs asserts every analyst
// and synthesist handoff template carries the independence rule.
// Missing templates are a test failure, not a skip — absence of a
// template is itself a regression from the expected set.
func TestIndependenceFence_PresentInAllHandoffs(t *testing.T) {
	root := findModuleRoot(t)
	for _, rel := range fenceTemplates {
		t.Run(rel, func(t *testing.T) {
			full := filepath.Join(root, rel)
			data, err := os.ReadFile(full) //nolint:gosec // G304: path is under module root, build-time fixture
			require.NoError(t, err, "template %s not found", rel)
			if !strings.Contains(string(data), fenceMarker) {
				t.Fatalf(
					"template %s is missing the independence rule "+
						"(marker: %q). Every handoff template must carry "+
						"the cross-pollination prohibition; see "+
						"design/m6-synthesis-contract.md §4 and §7. "+
						"Restore the fence — do not silence this test.",
					rel, fenceMarker,
				)
			}
		})
	}
}

// analystAgentRoles enumerates the reasoning-agent roles that must
// be denied Bash, Write, and MCP read tools in
// .claude/skills/analyze/SKILL.md.
var analystAgentRoles = []string{"signatory-security", "signatory-provenance"}

// synthesistAgentRole is checked separately so a regression back
// into Bash / Write / MCP-read tools surfaces clearly. The D9
// independence rule (no prior-analysis cross-pollination) still
// applies, but as of 2026-04-22 is enforced at the instruction
// layer rather than the tool-capability layer.
//
// Read/Glob/Grep are allowed: Node's TLS stack calls
// fs.readFileSync(NODE_EXTRA_CA_CERTS) on every HTTPS handshake,
// and a subagent without file-read capability cannot satisfy that
// syscall — producing the "unable to verify the first certificate"
// failure class seen during M6 dogfood (3 of 4 runs). See
// design/open-architecture-question.md for the hypothesis test
// that drove this relaxation and for Option A (MCP fetch_handoff)
// which would re-tighten this fence mechanically if invoked.
var synthesistAgentRole = "signatory-synthesis"

// forbiddenSynthesistTools is currently identical to
// forbiddenAnalystTools. The synthesist's D9 prohibition on
// reading prior analyses survives at the prompt layer: the
// handoff body is declared as the sole source of truth and the
// SKILL.md prompt explicitly forbids using Read to browse
// filestore / prior analyses. If Option A ships, Read/Glob/Grep
// return to the forbidden list.
var forbiddenSynthesistTools = forbiddenAnalystTools

// forbiddenAnalystTools is the set of tool names that must NOT appear
// on an analyst Agent block's allowed-tools line.
//
// Why each is forbidden:
//   - Bash: analysts are forbidden from running CLI commands (invariant 2);
//     enumeration is the collector's job, not the analyst's.
//   - Write: analysts write to the store via MCP, never to disk (invariant 3).
//   - mcp__signatory__signatory_*read tools: independence rule — analysts
//     must form judgment from source code and evidence handed to them,
//     not from prior analyses.
var forbiddenAnalystTools = []string{
	"Bash",
	"Write",
	"mcp__signatory__signatory_analyze",
	"mcp__signatory__signatory_detail",
	"mcp__signatory__signatory_show_analyses",
	"mcp__signatory__signatory_show_conclusions",
	"mcp__signatory__signatory_show_methodology",
	"mcp__signatory__signatory_signals",
	"mcp__signatory__signatory_summary",
	"mcp__signatory__signatory_survey",
}

// TestAnalystAgents_AllowedToolsMinimized reads
// .claude/skills/analyze/SKILL.md and asserts every analyst Agent
// block's allowed-tools line excludes the forbidden set. Makes the
// independence rule enforceable by CI rather than by prose
// compliance alone.
//
// The regex captures everything from the Agent(<role>) declaration
// to its first subsequent allowed-tools: line — non-greedy so
// multi-agent blocks don't smear into each other.
func TestAnalystAgents_AllowedToolsMinimized(t *testing.T) {
	root := findModuleRoot(t)
	skillPath := filepath.Join(root, ".claude", "skills", "analyze", "SKILL.md")
	data, err := os.ReadFile(skillPath) //nolint:gosec // G304: path is under module root, build-time fixture
	require.NoError(t, err, "SKILL.md not found at %s", skillPath)
	text := string(data)

	for _, role := range analystAgentRoles {
		t.Run(role, func(t *testing.T) {
			re := regexp.MustCompile(
				`(?s)Agent\(` + regexp.QuoteMeta(role) + `\).*?allowed-tools:\s*([^\n]+)`)
			matches := re.FindAllStringSubmatch(text, -1)
			require.NotEmpty(t, matches,
				"no Agent(%s) block with allowed-tools: found in SKILL.md", role)

			for i, match := range matches {
				toolsLine := match[1]
				for _, forbidden := range forbiddenAnalystTools {
					// Word-boundary guard: "Write" must not match as a
					// substring of "WriteToolName" if such a thing
					// existed. Build the pattern fresh per forbidden
					// token so edge cases are obvious.
					b := regexp.MustCompile(`\b` + regexp.QuoteMeta(forbidden) + `\b`)
					assert.False(t, b.MatchString(toolsLine),
						"Agent(%s) block #%d: allowed-tools must not grant %q "+
							"(independence rule / data minimization). "+
							"Line: %s", role, i, forbidden, toolsLine)
				}
			}
		})
	}
}

// TestSynthesistAgent_AllowedToolsMinimized enforces the synthesist
// tool fence: Bash, Write, and every signatory_* read MCP tool are
// forbidden. The synthesist's evidence arrives via WebFetch in the
// handoff body, and its output lands via signatory_ingest_analysis.
// Read/Glob/Grep are permitted as of 2026-04-22 only so Claude
// Code's HTTPS client can load NODE_EXTRA_CA_CERTS at TLS handshake
// — the D9 independence rule is enforced by the prompt body rather
// than tool capability. See design/open-architecture-question.md.
func TestSynthesistAgent_AllowedToolsMinimized(t *testing.T) {
	root := findModuleRoot(t)
	skillPath := filepath.Join(root, ".claude", "skills", "analyze", "SKILL.md")
	data, err := os.ReadFile(skillPath) //nolint:gosec // G304: path is under module root, build-time fixture
	require.NoError(t, err, "SKILL.md not found at %s", skillPath)
	text := string(data)

	re := regexp.MustCompile(
		`(?s)Agent\(` + regexp.QuoteMeta(synthesistAgentRole) + `\).*?allowed-tools:\s*([^\n]+)`)
	matches := re.FindAllStringSubmatch(text, -1)
	require.NotEmpty(t, matches,
		"no Agent(%s) block with allowed-tools: found in SKILL.md", synthesistAgentRole)

	for i, match := range matches {
		toolsLine := match[1]
		for _, forbidden := range forbiddenSynthesistTools {
			b := regexp.MustCompile(`\b` + regexp.QuoteMeta(forbidden) + `\b`)
			assert.False(t, b.MatchString(toolsLine),
				"Agent(synthesist) block #%d: allowed-tools must not grant %q "+
					"(synthesist fence / inputs-are-the-handoff-body). "+
					"Line: %s", i, forbidden, toolsLine)
		}
	}
}
