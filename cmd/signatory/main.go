package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kong"
	"github.com/sarahmaeve/signatory/internal/audit"
	"github.com/sarahmaeve/signatory/internal/signal"
	ghcollector "github.com/sarahmaeve/signatory/internal/signal/github"
	"github.com/sarahmaeve/signatory/internal/store"
)

// CLI defines signatory's command structure.
type CLI struct {
	DB      string `help:"Path to signatory database." default:"~/.signatory/signatory.db" type:"path" env:"SIGNATORY_DB"`
	Verbose bool   `help:"Verbose output." short:"v"`

	Analyze         AnalyzeCmd         `cmd:"" help:"Analyze trust signals for a package, repo, or identity."`
	Survey          SurveyCmd          `cmd:"" help:"Assess trust posture of a project's dependency tree."`
	Burn            BurnCmd            `cmd:"" help:"Burn an entity, degrading its trust signals."`
	Posture         PostureCmd         `cmd:"" help:"Set or view dependency posture tier for an entity."`
	Init            InitCmd            `cmd:"" help:"Scaffold ./templates/, ./filestore/, and signatory.config.toml in a project."`
	FormatCheck     FormatCheckCmd     `cmd:"format-check" help:"Check an analyst output file (JSON or markdown) for v1 schema conformance."`
	Ingest          IngestCmd          `cmd:"" help:"Ingest a v1-schema analyst output file into the signatory store."`
	ShowAnalyses    ShowAnalysesCmd    `cmd:"show-analyses" help:"List ingested analyst outputs, optionally filtered by target."`
	ShowFindings    ShowFindingsCmd    `cmd:"show-findings" help:"Query findings across ingested analyst outputs."`
	ShowMethodology ShowMethodologyCmd `cmd:"show-methodology" help:"Query methodology patterns across ingested analyst outputs."`
	Version         VersionCmd         `cmd:"" help:"Print version information."`
}

var (
	version = "dev"
	commit  = "none"
)

func main() {
	cli := CLI{}
	ctx := kong.Parse(&cli,
		kong.Name("signatory"),
		kong.Description("Supply chain trust analysis tool."),
		kong.UsageOnError(),
		kong.Vars{
			"version": version,
			"commit":  commit,
		},
	)
	err := ctx.Run(&Globals{
		DBPath:     cli.DB,
		Verbose:    cli.Verbose,
		Collectors: defaultCollectors(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Globals holds flags and dependencies shared across all commands.
type Globals struct {
	DBPath     string
	Verbose    bool
	Collectors []signal.Collector

	// AuditFilePath overrides the audit log file path. Empty means
	// "use the default (~/.signatory/audit.log)". Tests set this to a
	// temp path; production leaves it empty.
	AuditFilePath string
}

// OpenStore resolves the database path and opens the SQLite store.
func (g *Globals) OpenStore() (store.Store, error) {
	path, err := store.ResolvePath(g.DBPath)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	return store.OpenSQLite(path)
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

func defaultCollectors() []signal.Collector {
	return []signal.Collector{
		ghcollector.NewCollector(),
	}
}
