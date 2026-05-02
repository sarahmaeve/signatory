package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/pipeline"
)

// CLI-level Path D tests: drive the binary's full argv → kong.Parse
// → kctx.Run → exitCodeFor chain, capturing stdout AND stderr in
// parallel so tests can assert on what a real shell user sees.
//
// Companion to path_d_identity_org_smoke_test.go (the Run-level
// layer). The Run-level tests are faster and easier to debug; this
// layer catches argv parsing surprises, command dispatch, exit-code
// translation, and stderr-vs-stdout placement — gaps the Run-level
// tests miss because they bypass kong entirely.
//
// Per existing convention these tests do NOT use t.Parallel():
// runCLI swaps os.Stdout / os.Stderr globally for each invocation,
// and concurrent runs would fight over those file pointers.

// cliRun captures the result of a binary-level test invocation —
// the same three observables a shell user gets from running
// `signatory ...`: exit code, stdout content, stderr content.
type cliRun struct {
	exitCode int
	stdout   string
	stderr   string
}

// runCLI mirrors main()'s flow as closely as a test can: kong.New
// + parser.Parse on the supplied argv, kctx.Run with a Globals
// constructed the same way main() does (DBPath from cli.DB, etc.),
// the deferred stderr echo of the error, and finally exitCodeFor's
// usage-vs-runtime mapping.
//
// The single deviation from main() is Globals.AuditFilePath, which
// has no CLI flag in production and defaults to ~/.signatory/audit.log;
// the helper overrides it to a t.TempDir path so tests don't pollute
// the user's audit log. DBPath is similarly redirected via the
// `--db <path>` argv flag, threaded into argv by the helper so each
// test gets an isolated store.
func runCLI(t *testing.T, dbPath string, args ...string) cliRun {
	t.Helper()

	// Stdout / stderr capture. Swap os.Stdout and os.Stderr to
	// pipes so anything Run prints lands in our buffers rather
	// than the test runner's terminal.
	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	require.NoError(t, err)
	rErr, wErr, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout, os.Stderr = wOut, wErr

	outDone := make(chan string, 1)
	errDone := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(rOut)
		outDone <- buf.String()
	}()
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(rErr)
		errDone <- buf.String()
	}()

	// Restore in all exit paths — including panics from
	// require.* inside the dispatch — so a test failure mid-run
	// doesn't leave os.Stdout/Stderr pointing at a closed pipe
	// and break the rest of the test binary's output.
	defer func() {
		os.Stdout, os.Stderr = origOut, origErr
		_ = wOut.Close()
		_ = wErr.Close()
	}()

	// Thread --db into argv unless the caller already supplied
	// one; this is how production code receives it (env var or
	// flag), so the test exercises the same path.
	hasDB := false
	for _, a := range args {
		if a == "--db" || (len(a) > 5 && a[:5] == "--db=") {
			hasDB = true
			break
		}
	}
	full := args
	if !hasDB {
		full = append([]string{"--db", dbPath}, args...)
	}

	auditPath := filepath.Join(filepath.Dir(dbPath), "audit.log")

	// Build the parser the same way main() does. kong.Exit(noop)
	// is the standard test convention (cli_test.go) — prevents
	// kong from calling os.Exit on parse error so we can return
	// the error code instead.
	cli := &CLI{}
	parser, err := kong.New(cli,
		kong.Name("signatory"),
		kong.Description("Supply chain trust analysis tool."),
		kong.UsageOnError(),
		kong.Exit(func(int) {}),
		kong.Writers(new(bytes.Buffer), new(bytes.Buffer)),
		kong.Vars{
			"version":     "test",
			"commit":      "abc123",
			"pipelineURL": pipeline.DefaultURL,
		},
	)
	require.NoError(t, err)

	kctx, parseErr := parser.Parse(full)

	var runErr error
	if parseErr != nil {
		// kong.UsageOnError prints to its writers; we forward
		// the error to stderr the same way main does so tests
		// can assert on the stderr text.
		fmt.Fprintln(os.Stderr, parseErr)
		runErr = parseErr
	} else {
		runErr = kctx.Run(&Globals{
			DBPath:        cli.DB,
			Verbose:       cli.Verbose,
			Context:       context.Background(),
			AuditFilePath: auditPath,
		})
		if runErr != nil {
			// Mirror main()'s stderr echo before exit (skipping
			// errStatusNotRunning / errSilentFailure isn't relevant
			// for the commands Path D probes).
			fmt.Fprintln(os.Stderr, runErr)
		}
	}

	// Close write ends so the drain goroutines see EOF and the
	// channel reads complete. Capture before deferred restoration
	// runs so the buffers are fully populated.
	_ = wOut.Close()
	_ = wErr.Close()

	exitCode := 0
	if runErr != nil {
		exitCode = exitCodeFor(runErr)
	}

	return cliRun{
		exitCode: exitCode,
		stdout:   <-outDone,
		stderr:   <-errDone,
	}
}

// newCLITestDB returns the path a test should hand runCLI as its
// db argument. Mirrors newTestGlobals's role for the Run-level
// tests but produces a path string rather than a *Globals.
func newCLITestDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.db")
}

// TestPathD_CLI_BurnAdd_IdentityURI argv-level: `signatory burn add
// identity:github/<login> --reason "..."` succeeds, exits 0, and
// prints the burn-confirmation block on stdout.
func TestPathD_CLI_BurnAdd_IdentityURI(t *testing.T) {
	r := runCLI(t, newCLITestDB(t),
		"burn", "add", "identity:github/operator-x",
		"--reason", "test: campaign-shaped account",
	)

	assert.Equal(t, 0, r.exitCode, "burn add must exit 0; stderr=%q", r.stderr)
	assert.Contains(t, r.stdout, "Burned: operator-x",
		"burn add must echo the burned entity name on stdout")
	assert.Contains(t, r.stdout, "identity:github/operator-x",
		"burn add must echo the canonical URI on stdout")
}

// TestPathD_CLI_BurnList_RendersIdentityRow argv-level: after a
// burn add, `signatory burn list` surfaces the identity row.
// Two-step argv probe: chains commands the same way a user would
// at the shell, against one shared DB file.
func TestPathD_CLI_BurnList_RendersIdentityRow(t *testing.T) {
	dbPath := newCLITestDB(t)

	add := runCLI(t, dbPath,
		"burn", "add", "identity:github/operator-x",
		"--reason", "test seed",
	)
	require.Equal(t, 0, add.exitCode, "seed step must succeed; stderr=%q", add.stderr)

	list := runCLI(t, dbPath, "burn", "list")
	assert.Equal(t, 0, list.exitCode, "burn list must exit 0; stderr=%q", list.stderr)
	assert.Contains(t, list.stdout, "identity:github/operator-x",
		"burn list must include the identity URI in its rendering")
}

// TestPathD_CLI_Summary_IdentityURI argv-level: `signatory summary
// identity:github/<login>` against an existing identity entity
// renders the cached state on stdout.
func TestPathD_CLI_Summary_IdentityURI(t *testing.T) {
	dbPath := newCLITestDB(t)

	add := runCLI(t, dbPath,
		"burn", "add", "identity:github/operator-x",
		"--reason", "test: render check",
	)
	require.Equal(t, 0, add.exitCode, "seed step must succeed; stderr=%q", add.stderr)

	r := runCLI(t, dbPath, "summary", "identity:github/operator-x")
	assert.Equal(t, 0, r.exitCode, "summary must exit 0; stderr=%q", r.stderr)
	assert.Contains(t, r.stdout, "URI:       identity:github/operator-x")
	assert.Contains(t, r.stdout, "Type:      identity")
	assert.Contains(t, r.stdout, "BURNED")
	assert.Contains(t, r.stdout, "render check")
}

// TestPathD_CLI_Summary_NeverSeenIdentity argv-level: `signatory
// summary identity:github/<login>` against a never-ingested URI
// soft-absences cleanly (exit 0, "no record" message on stdout).
// Parity with the contract pinned for pkg:/repo: in
// posture_canonical_test.go's TestSummary_UnknownTarget_SoftAbsence.
func TestPathD_CLI_Summary_NeverSeenIdentity(t *testing.T) {
	r := runCLI(t, newCLITestDB(t),
		"summary", "identity:github/never-seen-account",
	)

	assert.Equal(t, 0, r.exitCode,
		"summary on a never-ingested target must exit 0 — soft absence, not error; stderr=%q", r.stderr)
	assert.Contains(t, r.stdout, "No signatory record",
		"the soft-absence message must surface on stdout")
	assert.Contains(t, r.stdout, "never-seen-account",
		"the message must name the queried target")
}

// TestPathD_CLI_Analyze_IdentityURI_FreshDB_ExitsNonZero argv-level:
// `signatory analyze --refresh identity:github/<login>` against a
// fresh DB must exit non-zero with the scheme-rejection error on
// stderr AND the `signatory summary` advisory in the same message.
//
// This is the test that the Run-level layer almost-but-not-quite
// catches: the Run-level test asserts on the returned error, but
// only the CLI-level test asserts on the actual exit code (which
// is what shell-script consumers branch on).
func TestPathD_CLI_Analyze_IdentityURI_FreshDB_ExitsNonZero(t *testing.T) {
	r := runCLI(t, newCLITestDB(t),
		"analyze", "--refresh", "identity:github/never-seen-account",
	)

	assert.NotEqual(t, 0, r.exitCode,
		"analyze on identity scheme must exit non-zero; stdout=%q stderr=%q", r.stdout, r.stderr)
	assert.Contains(t, r.stderr, "identity",
		"stderr must name the rejected scheme")
	assert.Contains(t, r.stderr, "scheme",
		"stderr must call out the scheme limitation")
	assert.Contains(t, r.stderr, "summary",
		"stderr must point to `signatory summary` as the right verb for cached-state queries")
}

// TestPathD_CLI_Analyze_IdentityURI_AlreadyMinted_ExitsNonZero
// argv-level companion: same shell behavior expected even when
// the entity was previously minted via `burn add`. The pre-fix
// silent-skip (exit 0 + "Stored 0 signals") is the failure mode
// this guards against.
func TestPathD_CLI_Analyze_IdentityURI_AlreadyMinted_ExitsNonZero(t *testing.T) {
	dbPath := newCLITestDB(t)

	add := runCLI(t, dbPath,
		"burn", "add", "identity:github/already-minted",
		"--reason", "test: pre-mint for analyze probe",
	)
	require.Equal(t, 0, add.exitCode, "seed step must succeed; stderr=%q", add.stderr)

	r := runCLI(t, dbPath,
		"analyze", "--refresh", "identity:github/already-minted",
	)

	assert.NotEqual(t, 0, r.exitCode,
		"analyze on an already-minted identity must still exit non-zero — the load-path scheme guard must mirror the create-path; stdout=%q stderr=%q",
		r.stdout, r.stderr)
	assert.Contains(t, r.stderr, "identity")
	assert.Contains(t, r.stderr, "summary",
		"the advisory message must be there too")
}

// TestPathD_CLI_Analyze_OrgURI_FreshDB_ExitsNonZero is the org:
// parallel to the identity FreshDB test.
func TestPathD_CLI_Analyze_OrgURI_FreshDB_ExitsNonZero(t *testing.T) {
	r := runCLI(t, newCLITestDB(t),
		"analyze", "--refresh", "org:github/never-seen-org",
	)

	assert.NotEqual(t, 0, r.exitCode,
		"analyze on org scheme must exit non-zero; stdout=%q stderr=%q", r.stdout, r.stderr)
	assert.Contains(t, r.stderr, "org")
	assert.Contains(t, r.stderr, "summary")
}
