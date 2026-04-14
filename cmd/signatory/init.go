package main

import (
	"fmt"
	"io"
	"os"

	"github.com/sarahmaeve/signatory"
	"github.com/sarahmaeve/signatory/internal/config"
)

// InitCmd scaffolds the directory layout signatory expects in a
// user's project:
//
//   - ./templates/ populated with the default prompt templates
//     compiled into the binary.
//   - ./filestore/ and ./filestore/analysis/ created for tool output.
//   - ./signatory.config.toml scaffolded (all keys commented out).
//
// `signatory init` is idempotent: running it again without --force
// never overwrites files that already exist. This is the intended
// upgrade path — after updating the signatory binary, re-running
// init pulls in any new templates shipped by the new version while
// preserving any local edits.
//
// Use --force to re-pave the local copies with the embedded
// originals (e.g., to discard a broken customization).
type InitCmd struct {
	// Dir is positional with a default of "." so both `signatory init`
	// (scaffold CWD) and `signatory init path/to/proj` are natural. This
	// matches the convention of `git init [dir]`, `npm init [dir]`, etc.
	Dir   string `arg:"" optional:"" help:"Directory to initialize. Defaults to the current directory." default:"." type:"path"`
	Force bool   `help:"Overwrite existing files with embedded originals. Skipped files are the default."`
	Quiet bool   `help:"Suppress per-file progress output; errors still print." short:"q"`
}

// Run executes the init. Progress lines go to stdout; the final
// summary line goes to stdout too. Errors propagate as usual.
func (cmd *InitCmd) Run(globals *Globals) error {
	var out io.Writer = os.Stdout
	if cmd.Quiet {
		out = io.Discard
	}

	result, err := config.InitProject(config.InitOptions{
		Dir:            cmd.Dir,
		Force:          cmd.Force,
		Out:            out,
		EmbeddedFS:     signatory.EmbeddedTemplates,
		EmbeddedPrefix: "templates",
	})
	if err != nil {
		return fmt.Errorf("init %s: %w", cmd.Dir, err)
	}

	if !cmd.Quiet {
		fmt.Printf("\ntemplates: %d copied, %d skipped\n", result.TemplatesCopied, result.TemplatesSkipped)
		fmt.Printf("filestore: %d directories ready\n", len(result.DirectoriesCreated))
		if result.ConfigWritten {
			fmt.Printf("config:    wrote %s\n", result.ConfigPath)
		} else {
			fmt.Printf("config:    preserved %s\n", result.ConfigPath)
		}
	}
	return nil
}
