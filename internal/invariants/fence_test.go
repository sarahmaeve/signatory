package invariants

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
// be denied Bash, Write, and MCP judgment-read tools.
var analystAgentRoles = []string{"signatory-security", "signatory-provenance"}

// dispatchRoleMap maps fence agent-role IDs (signatory-<function>)
// to dispatch role names in pipeline_dispatch.go. After the
// deterministic-orchestration rewrite, pipeline_dispatch.go is the
// single source of truth for allowed tools — SKILL.md no longer
// carries inline Agent() blocks.
var dispatchRoleMap = map[string]string{
	"signatory-security":   "security",
	"signatory-provenance": "provenance",
	"signatory-synthesis":  "synthesist",
}

// provenanceAllowedCacheTools are read-side MCP tools the provenance
// analyst IS allowed to call. These return cached Layer-1 mechanical
// data (the same GitHub/registry/git responses a WebFetch would
// return) — not prior analyst judgment. Granting these eliminates
// the re-derivation antipattern where the provenance analyst makes
// 14+ external API calls for data the orchestrator already collected.
// See dogfood session c684d13b metrics report.
var provenanceAllowedCacheTools = []string{
	"mcp__signatory__signatory_signals",
	"mcp__signatory__signatory_summary",
	"mcp__signatory__signatory_detail",
}

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
// cmd/signatory/pipeline_dispatch.go and asserts every dispatch
// role's allowedTools string excludes the forbidden set. Makes the
// independence rule enforceable by CI rather than by prose
// compliance alone.
//
// After the deterministic-orchestration rewrite, the dispatch roles
// map in pipeline_dispatch.go is the single source of truth for
// agent tool grants. The authoritative same-package fence lives in
// cmd/signatory/pipeline_dispatch_test.go (TestDispatchRoles_-
// AllowedToolsFence); this test is the independent cross-package
// check that catches drift even if someone bypasses the cmd tests.
func TestAnalystAgents_AllowedToolsMinimized(t *testing.T) {
	root := findModuleRoot(t)
	dispatchPath := filepath.Join(root, "cmd", "signatory", "pipeline_dispatch.go")
	data, err := os.ReadFile(dispatchPath) //nolint:gosec // G304: path is under module root, build-time fixture
	require.NoError(t, err, "pipeline_dispatch.go not found at %s", dispatchPath)
	text := string(data)

	for _, role := range analystAgentRoles {
		t.Run(role, func(t *testing.T) {
			dname, ok := dispatchRoleMap[role]
			require.True(t, ok, "no dispatch mapping for role %q", role)

			re := regexp.MustCompile(
				`(?s)"` + regexp.QuoteMeta(dname) + `":\s*\{[^}]*allowedTools:\s*"([^"]+)"`)
			matches := re.FindAllStringSubmatch(text, -1)
			require.NotEmpty(t, matches,
				"no dispatch role %q with allowedTools found in pipeline_dispatch.go", dname)

			for i, match := range matches {
				toolsLine := match[1]
				for _, forbidden := range forbiddenAnalystTools {
					// The provenance analyst is allowed read-side cache
					// tools (Layer-1 mechanical data). These return the
					// same GitHub/registry/git responses a WebFetch would
					// — not prior analyst judgment. Skip them.
					if role == "signatory-provenance" && isAllowedCacheTool(forbidden) {
						continue
					}
					b := regexp.MustCompile(`\b` + regexp.QuoteMeta(forbidden) + `\b`)
					assert.False(t, b.MatchString(toolsLine),
						"dispatch role %q block #%d: allowedTools must not grant %q "+
							"(independence rule / data minimization). "+
							"Line: %s", dname, i, forbidden, toolsLine)
				}
			}
		})
	}
}

// TestDispatchRoles_AnalystIDMatchesDefaultExpected enforces the
// invariant that every dispatchRole.analystID is in
// defaultExpectedAnalysts() and vice-versa: the orchestrator's
// dispatched-role analyst_id and the verify-time expected
// analyst_id are the same set, parsed from the same source.
//
// Catches the failure mode where someone updates dispatchRoles
// but forgets defaultExpectedAnalysts (or vice-versa), letting
// the orchestrator dispatch one analyst_id but check for another
// at resume time — exactly the asciify-image dogfood failure
// shape, but now caused by orchestrator bookkeeping rather than
// agent drift.
//
// Cross-package text-parsing check (no import dependency on the
// cmd package). Authoritative complement to the same-package
// TestDispatchRoles_AnalystIDIsCanonical in
// cmd/signatory/pipeline_dispatch_test.go.
func TestDispatchRoles_AnalystIDMatchesDefaultExpected(t *testing.T) {
	root := findModuleRoot(t)

	dispatchPath := filepath.Join(root, "cmd", "signatory", "pipeline_dispatch.go")
	dispatchData, err := os.ReadFile(dispatchPath) //nolint:gosec // G304: path under module root, build-time fixture
	require.NoError(t, err, "pipeline_dispatch.go not found")

	runPath := filepath.Join(root, "cmd", "signatory", "pipeline_run.go")
	runData, err := os.ReadFile(runPath) //nolint:gosec // G304: path under module root, build-time fixture
	require.NoError(t, err, "pipeline_run.go not found")

	// Extract every analystID literal from dispatchRoles map entries.
	dispatchAnalystIDRe := regexp.MustCompile(`analystID:\s*"([^"]+)"`)
	dispatchMatches := dispatchAnalystIDRe.FindAllStringSubmatch(string(dispatchData), -1)
	require.NotEmpty(t, dispatchMatches,
		"no analystID literals found in pipeline_dispatch.go — has the field been removed or renamed?")
	dispatchSet := make(map[string]struct{}, len(dispatchMatches))
	for _, m := range dispatchMatches {
		dispatchSet[m[1]] = struct{}{}
	}

	// Extract the defaultExpectedAnalysts() literal slice contents.
	defaultBlockRe := regexp.MustCompile(
		`(?s)func defaultExpectedAnalysts\(\) \[\]string \{[^}]*return \[\]string\{([^}]+)\}`)
	defaultBlock := defaultBlockRe.FindStringSubmatch(string(runData))
	require.Len(t, defaultBlock, 2,
		"could not locate defaultExpectedAnalysts() body in pipeline_run.go")
	literalRe := regexp.MustCompile(`"([^"]+)"`)
	literalMatches := literalRe.FindAllStringSubmatch(defaultBlock[1], -1)
	defaultSet := make(map[string]struct{}, len(literalMatches))
	for _, m := range literalMatches {
		defaultSet[m[1]] = struct{}{}
	}

	// Bi-directional set equality. Surface the offending side
	// concretely so a reader doesn't have to guess which list
	// drifted.
	for id := range dispatchSet {
		_, ok := defaultSet[id]
		assert.True(t, ok,
			"dispatchRoles has analystID %q but it's not in "+
				"defaultExpectedAnalysts() — orchestrator would dispatch "+
				"this role but verify wouldn't expect it. Add %q to "+
				"defaultExpectedAnalysts() in pipeline_run.go.", id, id)
	}
	for id := range defaultSet {
		_, ok := dispatchSet[id]
		assert.True(t, ok,
			"defaultExpectedAnalysts() has %q but no dispatchRole "+
				"declares this analystID — verify would expect this "+
				"role but the orchestrator never dispatches it. Add a "+
				"dispatchRole entry with analystID: %q in "+
				"pipeline_dispatch.go.", id, id)
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
	dispatchPath := filepath.Join(root, "cmd", "signatory", "pipeline_dispatch.go")
	data, err := os.ReadFile(dispatchPath) //nolint:gosec // G304: path is under module root, build-time fixture
	require.NoError(t, err, "pipeline_dispatch.go not found at %s", dispatchPath)
	text := string(data)

	dname, ok := dispatchRoleMap[synthesistAgentRole]
	require.True(t, ok, "no dispatch mapping for role %q", synthesistAgentRole)

	re := regexp.MustCompile(
		`(?s)"` + regexp.QuoteMeta(dname) + `":\s*\{[^}]*allowedTools:\s*"([^"]+)"`)
	matches := re.FindAllStringSubmatch(text, -1)
	require.NotEmpty(t, matches,
		"no dispatch role %q with allowedTools found in pipeline_dispatch.go", dname)

	for i, match := range matches {
		toolsLine := match[1]
		for _, forbidden := range forbiddenSynthesistTools {
			b := regexp.MustCompile(`\b` + regexp.QuoteMeta(forbidden) + `\b`)
			assert.False(t, b.MatchString(toolsLine),
				"dispatch role %q block #%d: allowedTools must not grant %q "+
					"(synthesist fence / inputs-are-the-handoff-body). "+
					"Line: %s", dname, i, forbidden, toolsLine)
		}
	}
}

// isAllowedCacheTool reports whether tool is in
// provenanceAllowedCacheTools — the read-side MCP tools that return
// cached Layer-1 mechanical data (not prior analyst judgment).
func isAllowedCacheTool(tool string) bool {
	return slices.Contains(provenanceAllowedCacheTools, tool)
}
