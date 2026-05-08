package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/alecthomas/kong"
	"github.com/sarahmaeve/signatory/internal/audit"
	"github.com/sarahmaeve/signatory/internal/pipeline"
	sig "github.com/sarahmaeve/signatory/internal/signal"
	"github.com/sarahmaeve/signatory/internal/store"
)

// CLI defines signatory's command structure.
//
// Commands are organized into groups via Kong's `group:""` tag so the
// help output reads as a workflow guide rather than an alphabetical
// dump. Group ordering is controlled by ExplicitGroups in
// KongOptions(); the struct field order is irrelevant to display.
type CLI struct {
	DB      string `help:"Path to signatory database." default:"~/.signatory/signatory.db" type:"path" env:"SIGNATORY_DB"`
	Verbose bool   `help:"Verbose output." short:"v"`

	// --- Investigate: first verbs a new user reaches for ---
	Summary SummaryCmd `cmd:"" help:"One-call view: canonical URI, posture, burn status, analyses rollup, related identities. The 'start here' verb for any target." group:"investigate"`
	Survey  SurveyCmd  `cmd:"" help:"Assess trust posture of a project's dependency tree." group:"investigate"`
	Analyze AnalyzeCmd `cmd:"" help:"Collect signals (github/git/registry metadata) and display the cached trust profile for a target." group:"investigate"`

	// --- Decide: recording trust decisions ---
	Posture PostureCmd `cmd:"" help:"Set or view dependency posture tier for an entity." group:"decide"`
	Burn    BurnCmd    `cmd:"" help:"Burn an entity, degrading its trust signals." group:"decide"`

	// --- Review: reading stored analysis data ---
	ShowAnalyses    ShowAnalysesCmd    `cmd:"show-analyses" help:"List ingested analyst outputs, optionally filtered by target." group:"review"`
	ShowConclusions ShowConclusionsCmd `cmd:"show-conclusions" help:"Query conclusions across ingested analyst outputs." group:"review"`
	ShowMethodology ShowMethodologyCmd `cmd:"show-methodology" help:"Query methodology patterns across ingested analyst outputs." group:"review"`
	ShowSynthesis   ShowSynthesisCmd   `cmd:"show-synthesis" help:"Render a synthesis output as markdown or static HTML." group:"review"`

	// --- Infrastructure: setup and services ---
	Init    InitCmd    `cmd:"" help:"Scaffold ./templates/, ./filestore/, and signatory.config.toml in a project." group:"infra"`
	Serve   ServeCmd   `cmd:"" help:"Start the pipeline message service (local HTTP API for agent handoffs)." group:"infra"`
	Certs   CertsCmd   `cmd:"" help:"Manage signatory's local TLS trust setup (mkcert CA + NODE_EXTRA_CA_CERTS)." group:"infra"`
	Doctor  DoctorCmd  `cmd:"" help:"Diagnose the local signatory setup: Go, env vars, TLS, MCP wiring, store, service." group:"infra"`
	MCP     MCPCmd     `cmd:"mcp" help:"Serve signatory as a Model Context Protocol server over stdio." group:"infra"`
	Version VersionCmd `cmd:"" help:"Print version information." group:"infra"`

	// --- Pipeline: internal orchestration ---
	Pipeline    PipelineCmd    `cmd:"" help:"Interact with the local pipeline message service (sessions, messages)." group:"pipeline"`
	Analysis    AnalysisCmd    `cmd:"" help:"Manage analysis-session lifecycle (begin, end, list, show, timing)." group:"pipeline"`
	Handoff     HandoffCmd     `cmd:"" help:"Render a handoff prompt for a fresh analyst agent." group:"pipeline"`
	FormatCheck FormatCheckCmd `cmd:"format-check" help:"Check an analyst output file for v1 schema conformance." group:"pipeline"`
	BuildOutput BuildOutputCmd `cmd:"build-output" help:"Convert structured agent text to v1-schema JSON." group:"pipeline"`
	Ingest      IngestCmd      `cmd:"" help:"Ingest a v1-schema analyst output file into the signatory store." group:"pipeline"`
	Prune       PruneCmd       `cmd:"" help:"Delete entities and their child rows from the store. Destructive; use with --yes." group:"pipeline"`
}

// version, commit, and buildDate are stamped by the Makefile via
// -ldflags at install time. Defaults are deliberately bland strings
// so a plain `go build` (without ldflags) produces a binary that
// still works and self-identifies as an unstamped build — useful
// during local development where `go run` or `go build` skips make.
//
// Without a stamped buildDate, a stale binary is invisible to the
// user; the 2026-04-21 M6 dogfood ran against a ~5-hour-old binary
// and the template drift wasn't caught until the output was weird.
// `signatory version` now surfaces the build timestamp so drift is
// one command away from being spotted.
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

// KongOptions returns the Kong configuration options shared between
// production (main) and tests (parseCLI, getHelpOutput). Keeping
// them in one place prevents the help-grouping tests from diverging
// from the real CLI surface.
//
// vars must supply "version", "commit", and "pipelineURL".
func KongOptions(vars kong.Vars) []kong.Option {
	return []kong.Option{
		kong.Name("signatory"),
		kong.Description("Supply chain trust analysis tool."),
		kong.UsageOnError(),
		vars,
		kong.ExplicitGroups([]kong.Group{
			{Key: "investigate", Title: "Investigate", Description: "Look up a target or scan a dependency tree."},
			{Key: "decide", Title: "Decide", Description: "Record trust decisions."},
			{Key: "review", Title: "Review", Description: "Read stored analysis data."},
			{Key: "infra", Title: "Infrastructure", Description: "Setup and services."},
			{Key: "pipeline", Title: "Pipeline", Description: "Internal orchestration — most users won't need these directly."},
		}),
	}
}

func main() {
	cli := CLI{}
	kctx := kong.Parse(&cli, KongOptions(kong.Vars{
		"version":     version,
		"commit":      commit,
		"pipelineURL": pipeline.DefaultURL,
	})...)

	// Root context. signal.NotifyContext routes SIGINT (Ctrl-C)
	// and SIGTERM to context cancellation, so every long-running
	// command that threads globals.Context through network / DB
	// calls aborts cleanly instead of leaving half-written state.
	// A second signal while shutdown is in progress escalates to
	// raw os.Exit via NotifyContext's default behavior (stop
	// delivering, return to default handler).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := kctx.Run(&Globals{
		DBPath:  cli.DB,
		Verbose: cli.Verbose,
		Context: ctx,
		// Globals.Collectors is intentionally left nil in
		// production — AnalyzeCmd.Run builds the collector list
		// per-target via collectorsFor(), which knows about
		// --path / --clone. Tests override this field with
		// mocks; see functional_test.go's testGlobals helper.
	})
	if err != nil {
		// Some commands print their own human-readable diagnostic
		// to stdout and return a sentinel purely to drive a non-
		// zero exit. For those, we skip the stderr echo that would
		// otherwise duplicate content the user already saw.
		//
		//   errStatusNotRunning — signatory serve status
		//   errSilentFailure    — signatory certs doctor (and future
		//                         diagnostic-style commands)
		if !errors.Is(err, errStatusNotRunning) &&
			!errors.Is(err, errSilentFailure) {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(exitCodeFor(err))
	}
}

// exitCodeFor maps an error to a Unix-style exit code. v0.1
// distinguishes usage errors (64, EX_USAGE) from runtime errors
// (1, generic). The usage class covers the --path/--clone sentinel
// errors because they reflect operator-intent issues that scripts
// may want to branch on specifically. Future ecosystem adoption
// may add EX_UNAVAILABLE (69) for registry-unavailable failures
// when those get sentinel types.
func exitCodeFor(err error) int {
	switch {
	case errors.Is(err, ErrUsage),
		errors.Is(err, ErrCloneRequired),
		errors.Is(err, ErrPathMissing),
		errors.Is(err, ErrPathNotEmpty),
		errors.Is(err, ErrPathNotAClone),
		errors.Is(err, ErrOriginMismatch):
		return 64 // EX_USAGE
	default:
		return 1
	}
}

// Globals holds flags and dependencies shared across all commands.
type Globals struct {
	DBPath  string
	Verbose bool

	// Context is the root context for command execution. In
	// production, main() populates it with signal.NotifyContext-
	// wrapped cancellation so Ctrl-C and SIGTERM propagate through
	// network/DB calls cleanly. Commands that thread globals.Context
	// through (AnalyzeCmd does today) abort mid-operation rather
	// than leaving partial state. Tests leave this nil; individual
	// Run methods default to context.Background() when nil.
	Context context.Context //nolint:containedctx // intentional CLI-root propagation

	// Collectors overrides the per-target collector list produced
	// by cmd/signatory/collectors.go's collectorsFor. Set by tests
	// (see functional_test.go's testGlobals) to inject mock
	// collectors without needing to stand up real git/github
	// plumbing. Left nil in production; AnalyzeCmd.Run calls
	// collectorsFor when this is empty.
	Collectors []sig.Collector

	// AuditFilePath overrides the audit log file path. Empty means
	// "use the default (~/.signatory/audit.log)". Tests set this to a
	// temp path; production leaves it empty.
	AuditFilePath string

	// NpmRegistryURL overrides the base URL for npm registry calls
	// made during analyze-orchestration repo resolution. Empty means
	// the production registry (https://registry.npmjs.org). Tests
	// point this at an httptest server so analyze runs don't hit the
	// live npm registry.
	NpmRegistryURL string

	// PypiRegistryURL overrides the base URL for PyPI registry
	// calls made during analyze-orchestration repo resolution (the
	// PyPI parallel to NpmRegistryURL — closes the v0.1 gap where
	// pkg:pypi/ targets had no project_urls→github resolution and
	// downstream github+git collectors silently skipped). Empty
	// means the production registry (https://pypi.org). Tests
	// point this at an httptest server.
	PypiRegistryURL string

	// GoProxyURL overrides the base URL for proxy.golang.org calls
	// made during analyze-orchestration repo resolution for vanity-
	// host Go modules (gopkg.in, modernc.org, k8s.io). Empty means
	// the production proxy (https://proxy.golang.org). Tests point
	// this at an httptest server. Used by resolveGoRepo when the
	// canonical pkg:golang/<modpath> URI doesn't have an algorithmic
	// github mapping (vanity hosts) and we need to query the proxy
	// for an Origin block.
	GoProxyURL string

	// GoVanityURL overrides the base URL prefix for go-import meta
	// tag fetches during the resolution fallback. Empty means
	// resolveGoRepo fetches the live "https://<modulePath>?go-get=1"
	// URL. Tests point this at an httptest server so the meta-tag
	// fallback is exercised without contacting real vanity hosts.
	// Production callers leave it empty.
	GoVanityURL string

	// CargoRegistryURL overrides the base URL for crates.io API calls
	// made during analyze-orchestration source resolution for
	// pkg:cargo/<name> targets. Empty means the production registry
	// (https://crates.io). Tests point this at an httptest server.
	CargoRegistryURL string

	// GemRegistryURL overrides the base URL for rubygems.org API calls
	// made during analyze-orchestration source resolution for
	// pkg:gem/<name> targets. Empty means the production registry
	// (https://rubygems.org). Tests point this at an httptest server.
	GemRegistryURL string

	// MavenRegistryURL overrides the base URL for Maven Central repo
	// access (repo1.maven.org) — metadata, POM fetch, signature
	// checks, timestamp resolution. Everything goes through one host.
	// Empty means the production endpoint (https://repo1.maven.org).
	// Tests point this at an httptest server.
	MavenRegistryURL string
}

// OpenStore resolves the database path and opens the SQLite store.
// ctx is threaded into OpenSQLite's Ping + PRAGMA setup + migrations,
// so a cancelled caller context aborts store initialization cleanly.
// Every Run method on a command already creates or receives a context;
// that same context is the right thing to pass here.
func (g *Globals) OpenStore(ctx context.Context) (store.Store, error) {
	path, err := store.ResolvePath(g.DBPath)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	return store.OpenSQLite(ctx, path)
}

// NewAuditLogger constructs an audit logger wired to the given store
// and the configured file path. Falls back to database-only logging
// if the default file path cannot be resolved (e.g., $HOME unset).
func (g *Globals) NewAuditLogger(s store.Store) *audit.Logger {
	path := g.AuditFilePath
	if path == "" {
		defaultPath, err := audit.DefaultFilePath()
		if err == nil {
			path = defaultPath
		}
	}
	return audit.New(s, path)
}
