package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/pipeline"
)

// testVars returns the kong.Vars used by all test parsers.
func testVars() kong.Vars {
	return kong.Vars{
		"version":     "test",
		"commit":      "abc123",
		"pipelineURL": pipeline.DefaultURL,
	}
}

// newTestParser constructs a Kong parser wired to the shared
// KongOptions (same group definitions, description, etc. as
// production) with test-appropriate overrides for exit and writers.
func newTestParser(t *testing.T, cli *CLI, stdout, stderr *bytes.Buffer) *kong.Kong {
	t.Helper()

	opts := KongOptions(testVars())
	opts = append(opts,
		kong.Exit(func(int) {}),
		kong.Writers(stdout, stderr),
	)
	parser, err := kong.New(cli, opts...)
	require.NoError(t, err)
	return parser
}

// parseCLI is a test helper that parses args into the CLI struct and returns the parsed context.
func parseCLI(t *testing.T, args ...string) (*kong.Context, *CLI) {
	t.Helper()

	cli := &CLI{}
	parser := newTestParser(t, cli, new(bytes.Buffer), new(bytes.Buffer))
	ctx, err := parser.Parse(args)
	if err != nil {
		t.Fatalf("failed to parse args %v: %v", args, err)
	}
	return ctx, cli
}

// parseCLIExpectError is a test helper that parses args and expects a parse error.
func parseCLIExpectError(t *testing.T, args ...string) error {
	t.Helper()

	cli := &CLI{}
	parser := newTestParser(t, cli, new(bytes.Buffer), new(bytes.Buffer))
	_, err := parser.Parse(args)
	return err
}

// getHelpOutput returns the help text from the parser.
func getHelpOutput(t *testing.T) string {
	t.Helper()

	cli := &CLI{}
	var buf bytes.Buffer
	parser := newTestParser(t, cli, &buf, &buf)
	_, _ = parser.Parse([]string{"--help"})
	return buf.String()
}

// --- Analyze ---

func TestAnalyzeCmd_ValidArgs(t *testing.T) {
	t.Parallel()

	ctx, cli := parseCLI(t, "analyze", "lodash")
	assert.Equal(t, "analyze <target>", ctx.Command())
	assert.Equal(t, "lodash", cli.Analyze.Target)
	assert.False(t, cli.Analyze.Refresh)
	assert.False(t, cli.Analyze.JSON)
}

func TestAnalyzeCmd_WithFlags(t *testing.T) {
	t.Parallel()

	_, cli := parseCLI(t, "analyze", "lodash", "--refresh", "--json")
	assert.Equal(t, "lodash", cli.Analyze.Target)
	assert.True(t, cli.Analyze.Refresh)
	assert.True(t, cli.Analyze.JSON)
}

func TestAnalyzeCmd_MissingTarget(t *testing.T) {
	t.Parallel()

	err := parseCLIExpectError(t, "analyze")
	assert.Error(t, err)
}

// --- Survey ---

func TestSurveyCmd_NoArgs(t *testing.T) {
	t.Parallel()

	ctx, cli := parseCLI(t, "survey")
	assert.Equal(t, "survey", ctx.Command())
	assert.Empty(t, cli.Survey.Manifest)
	assert.False(t, cli.Survey.Refresh)
	assert.False(t, cli.Survey.JSON)
}

func TestSurveyCmd_WithFlags(t *testing.T) {
	t.Parallel()

	_, cli := parseCLI(t, "survey", "--refresh", "--json")
	assert.True(t, cli.Survey.Refresh)
	assert.True(t, cli.Survey.JSON)
}

// --- Burn ---

func TestBurnCmd_ValidArgs(t *testing.T) {
	t.Parallel()

	ctx, cli := parseCLI(t, "burn", "evil-package", "--reason", "malware detected")
	assert.Equal(t, "burn add <target>", ctx.Command())
	assert.Equal(t, "evil-package", cli.Burn.Add.Target)
	assert.Equal(t, "malware detected", cli.Burn.Add.Reason)
}

func TestBurnCmd_ListSubcommand(t *testing.T) {
	t.Parallel()

	ctx, _ := parseCLI(t, "burn", "list")
	assert.Equal(t, "burn list", ctx.Command())
}

func TestBurnCmd_MissingTarget(t *testing.T) {
	t.Parallel()

	err := parseCLIExpectError(t, "burn", "--reason", "test")
	assert.Error(t, err)
}

// TestBurnCmd_MissingReason verifies that omitting both --reason and
// --reason-file produces a Run-level error — kong no longer rejects
// at parse time because either flag can satisfy the requirement.
// See agent-facing-contract §3.4.
func TestBurnCmd_MissingReason(t *testing.T) {
	t.Parallel()

	cmd := &BurnAddCmd{Target: "pkg:npm/evil-package"}
	dir := t.TempDir()
	globals := &Globals{
		DBPath:        filepath.Join(dir, "test.db"),
		AuditFilePath: filepath.Join(dir, "audit.log"),
	}
	err := cmd.Run(globals)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--reason or --reason-file is required")
}

// --- Posture ---

func TestPostureListCmd_SubcommandParsing(t *testing.T) {
	t.Parallel()

	ctx, _ := parseCLI(t, "posture", "list")
	assert.Equal(t, "posture list", ctx.Command())
}

func TestPostureGetCmd_ValidArgs(t *testing.T) {
	t.Parallel()

	ctx, cli := parseCLI(t, "posture", "lodash")
	assert.Equal(t, "posture get <target>", ctx.Command())
	assert.Equal(t, "lodash", cli.Posture.Get.Target)
}

func TestPostureSetCmd_ValidArgs(t *testing.T) {
	t.Parallel()

	ctx, cli := parseCLI(t, "posture", "set", "lodash", "--tier", "vetted-frozen", "--rationale", "audited by security team")
	assert.Equal(t, "posture set <target>", ctx.Command())
	assert.Equal(t, "lodash", cli.Posture.Set.Target)
	assert.Equal(t, "vetted-frozen", cli.Posture.Set.Tier)
	assert.Equal(t, "audited by security team", cli.Posture.Set.Rationale)
}

func TestPostureSetCmd_WithVersion(t *testing.T) {
	t.Parallel()

	_, cli := parseCLI(t, "posture", "set", "lodash", "--tier", "vetted-frozen", "--rationale", "audited", "--version", "4.17.21")
	assert.Equal(t, "4.17.21", cli.Posture.Set.Version)
}

func TestPostureSetCmd_InvalidTier(t *testing.T) {
	t.Parallel()

	err := parseCLIExpectError(t, "posture", "set", "lodash", "--tier", "invalid-tier", "--rationale", "test")
	assert.Error(t, err)
}

func TestPostureSetCmd_MissingTier(t *testing.T) {
	t.Parallel()

	err := parseCLIExpectError(t, "posture", "set", "lodash", "--rationale", "test")
	assert.Error(t, err)
}

// TestPostureSetCmd_MissingRationale verifies that omitting both
// --rationale and --rationale-file produces a Run-level error — kong
// no longer rejects at parse time because either flag can satisfy
// the requirement. See agent-facing-contract §3.4.
func TestPostureSetCmd_MissingRationale(t *testing.T) {
	t.Parallel()

	cmd := &PostureSetCmd{Target: "pkg:npm/lodash", Tier: "vetted-frozen"}
	dir := t.TempDir()
	globals := &Globals{
		DBPath:        filepath.Join(dir, "test.db"),
		AuditFilePath: filepath.Join(dir, "audit.log"),
	}
	err := cmd.Run(globals)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--rationale or --rationale-file is required")
}

func TestPostureSetCmd_AllValidTiers(t *testing.T) {
	t.Parallel()

	validTiers := []string{"vetted-frozen", "trusted-for-now", "unexamined", "unknown-provenance"}
	for _, tier := range validTiers {
		t.Run(tier, func(t *testing.T) {
			t.Parallel()
			_, cli := parseCLI(t, "posture", "set", "pkg", "--tier", tier, "--rationale", "test")
			assert.Equal(t, tier, cli.Posture.Set.Tier)
		})
	}
}

// TestPostureSetCmd_URIVersion_InheritedWhenFlagUnset covers the
// happy-path M1 semantics: a versioned URI with no --version flag
// inherits the version from the URI into the stored posture.
func TestPostureSetCmd_URIVersion_InheritedWhenFlagUnset(t *testing.T) {
	t.Parallel()

	cmd := &PostureSetCmd{
		Target:    "pkg:npm/lodash@4.17.21",
		Tier:      "vetted-frozen",
		Rationale: "audited",
		// Version intentionally empty — must be inherited from URI.
	}
	dir := t.TempDir()
	globals := &Globals{
		DBPath:        filepath.Join(dir, "test.db"),
		AuditFilePath: filepath.Join(dir, "audit.log"),
	}
	require.NoError(t, cmd.Run(globals))
	assert.Equal(t, "4.17.21", cmd.Version,
		"PostureSetCmd.Run must populate Version from URI @V when flag is unset")
}

// TestPostureSetCmd_URIVersion_AgreesWithFlag covers the redundant-
// but-consistent case: URI and flag both carry the same version.
// Accepted without complaint; the stored version is preserved.
func TestPostureSetCmd_URIVersion_AgreesWithFlag(t *testing.T) {
	t.Parallel()

	cmd := &PostureSetCmd{
		Target:    "pkg:npm/lodash@4.17.21",
		Tier:      "vetted-frozen",
		Rationale: "audited",
		Version:   "4.17.21",
	}
	dir := t.TempDir()
	globals := &Globals{
		DBPath:        filepath.Join(dir, "test.db"),
		AuditFilePath: filepath.Join(dir, "audit.log"),
	}
	require.NoError(t, cmd.Run(globals))
	assert.Equal(t, "4.17.21", cmd.Version)
}

// TestPostureSetCmd_URIVersion_ConflictsWithFlag covers the
// loud-failure case: URI says one version, flag says another.
// Signatory refuses to guess which one the caller meant.
func TestPostureSetCmd_URIVersion_ConflictsWithFlag(t *testing.T) {
	t.Parallel()

	cmd := &PostureSetCmd{
		Target:    "pkg:npm/lodash@4.17.21",
		Tier:      "vetted-frozen",
		Rationale: "audited",
		Version:   "4.18.0", // disagrees with URI
	}
	dir := t.TempDir()
	globals := &Globals{
		DBPath:        filepath.Join(dir, "test.db"),
		AuditFilePath: filepath.Join(dir, "audit.log"),
	}
	err := cmd.Run(globals)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conflicts",
		"error must name the conflict explicitly rather than silently preferring one source")
	assert.Contains(t, err.Error(), "4.17.21")
	assert.Contains(t, err.Error(), "4.18.0")
}

// --- Version ---

func TestVersionCmd_ValidArgs(t *testing.T) {
	t.Parallel()

	ctx, _ := parseCLI(t, "version")
	assert.Equal(t, "version", ctx.Command())
}

// --- Global Flags ---

func TestGlobalFlags_DB(t *testing.T) {
	t.Parallel()

	_, cli := parseCLI(t, "--db", "/custom/path.db", "analyze", "test")
	assert.Equal(t, "/custom/path.db", cli.DB)
}

func TestGlobalFlags_Verbose(t *testing.T) {
	t.Parallel()

	_, cli := parseCLI(t, "-v", "analyze", "test")
	assert.True(t, cli.Verbose)
}

func TestGlobalFlags_VerboseLong(t *testing.T) {
	t.Parallel()

	_, cli := parseCLI(t, "--verbose", "analyze", "test")
	assert.True(t, cli.Verbose)
}

// --- Help Output ---

func TestHelpOutput_ContainsAllCommands(t *testing.T) {
	t.Parallel()

	help := getHelpOutput(t)

	expectedCommands := []string{"analyze", "survey", "burn", "posture", "version"}
	for _, cmd := range expectedCommands {
		assert.True(t, strings.Contains(help, cmd),
			"help output should contain command %q, got:\n%s", cmd, help)
	}
}

func TestHelpOutput_ContainsDescription(t *testing.T) {
	t.Parallel()

	help := getHelpOutput(t)
	assert.Contains(t, help, "Supply chain trust analysis tool")
}

// TestHelpOutput_GroupHeaders verifies that the help output contains
// the five group headers in the expected order. A new user scanning
// the help should see commands organized by workflow stage, not a
// flat alphabetical list.
func TestHelpOutput_GroupHeaders(t *testing.T) {
	t.Parallel()

	help := getHelpOutput(t)

	// Groups must appear in this order — workflow-first, internals last.
	groups := []string{
		"Investigate",
		"Decide",
		"Review",
		"Infrastructure",
		"Pipeline",
	}
	for _, g := range groups {
		assert.Contains(t, help, g,
			"help output must contain group header %q", g)
	}

	// Verify ordering: each group header appears after the previous one.
	lastIdx := -1
	for _, g := range groups {
		idx := strings.Index(help, g)
		require.NotEqual(t, -1, idx,
			"group header %q not found in help output", g)
		assert.Greater(t, idx, lastIdx,
			"group %q (at %d) must appear after the previous group (at %d);\nhelp output:\n%s",
			g, idx, lastIdx, help)
		lastIdx = idx
	}
}

// TestHelpOutput_CommandsInExpectedGroups spot-checks that key commands
// appear under the correct group header, not just anywhere in the output.
func TestHelpOutput_CommandsInExpectedGroups(t *testing.T) {
	t.Parallel()

	help := getHelpOutput(t)

	// Map from group header to commands expected under it.
	groupCommands := map[string][]string{
		"Investigate":    {"summary", "survey", "analyze"},
		"Decide":         {"posture", "burn"},
		"Review":         {"show-analyses", "show-conclusions", "show-synthesis"},
		"Infrastructure": {"init", "serve", "certs", "mcp", "version"},
		"Pipeline":       {"pipeline", "analysis", "handoff", "ingest", "prune"},
	}

	for group, cmds := range groupCommands {
		groupIdx := strings.Index(help, group)
		require.NotEqual(t, -1, groupIdx,
			"group header %q not found in help output", group)

		// Find the start of the NEXT group after this one (or end of string).
		allGroups := []string{"Investigate", "Decide", "Review", "Infrastructure", "Pipeline"}
		nextGroupIdx := len(help)
		for i, g := range allGroups {
			if g == group && i+1 < len(allGroups) {
				next := strings.Index(help[groupIdx+len(group):], allGroups[i+1])
				if next != -1 {
					nextGroupIdx = groupIdx + len(group) + next
				}
				break
			}
		}

		section := help[groupIdx:nextGroupIdx]
		for _, cmd := range cmds {
			assert.Contains(t, section, cmd,
				"command %q should appear under group %q, but found in section:\n%s", cmd, group, section)
		}
	}
}

// --- No Subcommand ---

func TestCLI_NoSubcommand(t *testing.T) {
	t.Parallel()

	err := parseCLIExpectError(t)
	assert.Error(t, err)
}
