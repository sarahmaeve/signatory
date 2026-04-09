package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseCLI is a test helper that parses args into the CLI struct and returns the parsed context.
func parseCLI(t *testing.T, args ...string) (*kong.Context, *CLI) {
	t.Helper()

	cli := &CLI{}
	parser, err := kong.New(cli,
		kong.Name("signatory"),
		kong.Description("Supply chain trust analysis tool."),
		kong.Exit(func(int) {}), // Prevent os.Exit in tests
		kong.Writers(new(bytes.Buffer), new(bytes.Buffer)),
		kong.Vars{
			"version": "test",
			"commit":  "abc123",
		},
	)
	require.NoError(t, err)

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
	parser, err := kong.New(cli,
		kong.Name("signatory"),
		kong.Description("Supply chain trust analysis tool."),
		kong.Exit(func(int) {}),
		kong.Writers(new(bytes.Buffer), new(bytes.Buffer)),
		kong.Vars{
			"version": "test",
			"commit":  "abc123",
		},
	)
	require.NoError(t, err)

	_, err = parser.Parse(args)
	return err
}

// getHelpOutput returns the help text from the parser.
func getHelpOutput(t *testing.T) string {
	t.Helper()

	cli := &CLI{}
	var buf bytes.Buffer
	parser, err := kong.New(cli,
		kong.Name("signatory"),
		kong.Description("Supply chain trust analysis tool."),
		kong.Exit(func(int) {}),
		kong.Writers(&buf, &buf),
		kong.Vars{
			"version": "test",
			"commit":  "abc123",
		},
	)
	require.NoError(t, err)

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

func TestAnalyzeCmd_Run(t *testing.T) {
	t.Parallel()

	cmd := &AnalyzeCmd{Target: "lodash", Refresh: true}
	globals := &Globals{DBPath: filepath.Join(t.TempDir(), "test.db"), Verbose: false}
	err := cmd.Run(globals)
	assert.NoError(t, err)
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

func TestSurveyCmd_Run(t *testing.T) {
	t.Parallel()

	cmd := &SurveyCmd{Refresh: true}
	globals := &Globals{DBPath: filepath.Join(t.TempDir(), "test.db")}
	err := cmd.Run(globals)
	assert.NoError(t, err)
}

// --- Compare ---

func TestCompareCmd_ValidArgs(t *testing.T) {
	t.Parallel()

	ctx, cli := parseCLI(t, "compare", "lodash", "underscore")
	assert.Equal(t, "compare <target-a> <target-b>", ctx.Command())
	assert.Equal(t, "lodash", cli.Compare.TargetA)
	assert.Equal(t, "underscore", cli.Compare.TargetB)
	assert.False(t, cli.Compare.JSON)
}

func TestCompareCmd_WithJSON(t *testing.T) {
	t.Parallel()

	_, cli := parseCLI(t, "compare", "a", "b", "--json")
	assert.True(t, cli.Compare.JSON)
}

func TestCompareCmd_MissingBothTargets(t *testing.T) {
	t.Parallel()

	err := parseCLIExpectError(t, "compare")
	assert.Error(t, err)
}

func TestCompareCmd_MissingSecondTarget(t *testing.T) {
	t.Parallel()

	err := parseCLIExpectError(t, "compare", "lodash")
	assert.Error(t, err)
}

func TestCompareCmd_Run(t *testing.T) {
	t.Parallel()

	cmd := &CompareCmd{TargetA: "a", TargetB: "b"}
	globals := &Globals{DBPath: filepath.Join(t.TempDir(), "test.db")}
	err := cmd.Run(globals)
	assert.NoError(t, err)
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

func TestBurnCmd_MissingReason(t *testing.T) {
	t.Parallel()

	err := parseCLIExpectError(t, "burn", "evil-package")
	assert.Error(t, err)
}

func TestBurnAddCmd_Run(t *testing.T) {
	t.Parallel()

	cmd := &BurnAddCmd{Target: "evil-package", Reason: "malware"}
	globals := &Globals{DBPath: filepath.Join(t.TempDir(), "test.db")}
	err := cmd.Run(globals)
	assert.NoError(t, err)
}

// --- Posture ---

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

func TestPostureSetCmd_MissingRationale(t *testing.T) {
	t.Parallel()

	err := parseCLIExpectError(t, "posture", "set", "lodash", "--tier", "vetted-frozen")
	assert.Error(t, err)
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

func TestPostureGetCmd_Run(t *testing.T) {
	t.Parallel()

	cmd := &PostureGetCmd{Target: "lodash"}
	globals := &Globals{DBPath: filepath.Join(t.TempDir(), "test.db")}
	err := cmd.Run(globals)
	assert.NoError(t, err)
}

func TestPostureSetCmd_Run(t *testing.T) {
	t.Parallel()

	cmd := &PostureSetCmd{Target: "lodash", Tier: "vetted-frozen", Rationale: "audited"}
	globals := &Globals{DBPath: filepath.Join(t.TempDir(), "test.db")}
	err := cmd.Run(globals)
	assert.NoError(t, err)
}

// --- Version ---

func TestVersionCmd_ValidArgs(t *testing.T) {
	t.Parallel()

	ctx, _ := parseCLI(t, "version")
	assert.Equal(t, "version", ctx.Command())
}

func TestVersionCmd_Run(t *testing.T) {
	t.Parallel()

	cmd := &VersionCmd{}
	globals := &Globals{DBPath: filepath.Join(t.TempDir(), "test.db")}
	err := cmd.Run(globals)
	assert.NoError(t, err)
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

	expectedCommands := []string{"analyze", "survey", "compare", "burn", "posture", "version"}
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

// --- No Subcommand ---

func TestCLI_NoSubcommand(t *testing.T) {
	t.Parallel()

	err := parseCLIExpectError(t)
	assert.Error(t, err)
}
