package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kong"
	"github.com/sarahmaeve/signatory/internal/signal"
	ghcollector "github.com/sarahmaeve/signatory/internal/signal/github"
	"github.com/sarahmaeve/signatory/internal/store"
)

// CLI defines signatory's command structure.
type CLI struct {
	DB      string `help:"Path to signatory database." default:"~/.signatory/signatory.db" type:"path" env:"SIGNATORY_DB"`
	Verbose bool   `help:"Verbose output." short:"v"`

	Analyze AnalyzeCmd `cmd:"" help:"Analyze trust signals for a package, repo, or identity."`
	Survey  SurveyCmd  `cmd:"" help:"Assess trust posture of a project's dependency tree."`
	Compare CompareCmd `cmd:"" help:"Compare trust profiles of two packages or repos."`
	Burn    BurnCmd    `cmd:"" help:"Burn an entity, degrading its trust signals."`
	Posture PostureCmd `cmd:"" help:"Set or view dependency posture tier for an entity."`
	Version VersionCmd `cmd:"" help:"Print version information."`
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
}

// OpenStore resolves the database path and opens the SQLite store.
func (g *Globals) OpenStore() (*store.SQLite, error) {
	path, err := store.ResolvePath(g.DBPath)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	return store.OpenSQLite(path)
}

func defaultCollectors() []signal.Collector {
	return []signal.Collector{
		ghcollector.NewCollector(),
	}
}
