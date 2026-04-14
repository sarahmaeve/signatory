package main

import (
	"fmt"
	"io"
	"os"

	"github.com/sarahmaeve/signatory"
	"github.com/sarahmaeve/signatory/internal/config"
)

// HandoffCmd renders a handoff prompt by loading a template from the
// resolver-ordered search path and substituting the caller's
// `{PLACEHOLDER}` values. Output defaults to stdout; pass --output to
// write a file.
//
// Typical invocations:
//
//	signatory handoff security https://github.com/nvbn/thefuck
//	signatory handoff provenance /Users/me/code/thefuck --ecosystem=pypi
//	signatory handoff security ./atuin --language=go
//
// The <role> positional picks between the shipped roles (security,
// provenance). For security, --language=python|go picks which
// pattern-catalog variant to use. --template overrides the inference
// for advanced callers who maintain their own template variants.
//
// All `{PLACEHOLDER}` tokens not filled by flags are left literal in
// the output; the command reports which ones remained on stderr so
// the user can fix them before pasting into an agent prompt. The
// exception is TARGET_NAME — if it can't be inferred from the target
// and the user didn't pass --name, the command errors rather than
// emit a broken handoff.
//
// Network behavior: this command is offline. Auto-detecting language
// or ecosystem from a remote repo would require network access and
// isn't implemented yet — pass --language and --ecosystem explicitly.
// A future --network-precheck flag will probe remote registries.
type HandoffCmd struct {
	Role   string `arg:"" enum:"security,provenance" help:"Analyst role: security or provenance."`
	Target string `arg:"" help:"Target repository URL or local path (e.g., https://github.com/foo/bar or /Users/me/code/foo)."`

	Name       string `help:"Override TARGET_NAME (default: inferred from target)."`
	URL        string `help:"Override TARGET_URL (default: target when target is a URL)."`
	Path       string `help:"Override TARGET_PATH (default: target when target is a local path)."`
	TargetRole string `name:"target-role" default:"" help:"Dependency role for TARGET_ROLE (runtime|validation|build-only|development)." enum:"runtime,validation,build-only,development,"`
	Ecosystem  string `default:"" help:"ECOSYSTEM value for provenance role (pypi|npm|crates|go)." enum:"pypi,npm,crates,go,"`
	Language   string `help:"Language flavor for security role." enum:"python,go" default:"python"`
	Intake     string `help:"INTAKE_QUESTION body; the user's specific question for this engagement."`
	Template   string `help:"Explicit template name (e.g., handoffs/security-review-v1.md). Bypasses --role/--language inference."`

	TemplateDir  []string `name:"template-dir" help:"Additional template search directory (repeatable, highest priority)."`
	FilestoreDir []string `name:"filestore-dir" help:"Additional filestore output directory (repeatable). Unused unless --output is a bare filename."`
	ConfigFile   string   `name:"config" help:"Path to signatory.config.toml. If unset, discovered from --project-dir." type:"existingfile"`
	ProjectDir   string   `name:"project-dir" help:"Project root used to locate ./templates/, ./filestore/, and signatory.config.toml." default:"." type:"path"`

	Output string `short:"o" help:"Write rendered handoff to this file instead of stdout."`
	Force  bool   `help:"Overwrite --output if it exists."`
	Quiet  bool   `help:"Suppress the stderr report (template source, unfilled placeholders)." short:"q"`
}

// Run executes the handoff render. Errors from template resolution,
// substitution, or write are surfaced directly; the stderr report
// (template source, unfilled placeholders, embedded-fallback notice)
// is informational and goes out independently of Success/failure.
func (cmd *HandoffCmd) Run(globals *Globals) error {
	resolver, err := cmd.buildResolver()
	if err != nil {
		return err
	}

	templateName := cmd.Template
	if templateName == "" {
		templateName = inferTemplateName(cmd.Role, cmd.Language)
	}

	rc, source, embedded, err := resolver.OpenTemplate(templateName)
	if err != nil {
		return fmt.Errorf("load template: %w", err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("read template %s: %w", source, err)
	}

	subs, err := config.HandoffSubstitutions(cmd.Target, config.HandoffOverrides{
		Name:      cmd.Name,
		URL:       cmd.URL,
		Path:      cmd.Path,
		Role:      cmd.TargetRole,
		Ecosystem: cmd.Ecosystem,
		Intake:    cmd.Intake,
	})
	if err != nil {
		return err
	}

	// Surface ecosystem-required roles before render time so the user
	// doesn't silently get a handoff with `{ECOSYSTEM}` literal.
	if cmd.Role == "provenance" && subs["ECOSYSTEM"] == "" {
		return fmt.Errorf("provenance role requires --ecosystem (one of: pypi, npm, crates, go)")
	}

	rendered, unfilled := config.RenderTemplate(raw, subs)

	if err := writeHandoff(cmd.Output, cmd.Force, rendered); err != nil {
		return err
	}

	if !cmd.Quiet {
		reportToStderr(source, embedded, unfilled)
	}
	return nil
}

// buildResolver wires the CLI flags, optional config file, and
// embedded fallback into a single Resolver for this command. The
// config file is loaded explicitly when --config is passed; otherwise
// we look for signatory.config.toml in --project-dir via
// DiscoverAndLoad (absence is OK).
func (cmd *HandoffCmd) buildResolver() (*config.Resolver, error) {
	var cfg *config.Config
	var err error
	switch {
	case cmd.ConfigFile != "":
		cfg, err = config.LoadConfig(cmd.ConfigFile)
	default:
		cfg, err = config.DiscoverAndLoad(cmd.ProjectDir)
	}
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	return &config.Resolver{
		CLITemplateDirs:  cmd.TemplateDir,
		CLIFilestoreDirs: cmd.FilestoreDir,
		Config:           cfg,
		EmbeddedFS:       signatory.EmbeddedTemplates,
		EmbeddedPrefix:   "templates",
		BaseDir:          cmd.ProjectDir,
	}, nil
}

// inferTemplateName maps the role/language CLI arguments to a
// specific template file under handoffs/. The mapping is small and
// explicit to keep behavior predictable; callers with non-standard
// variants should pass --template.
func inferTemplateName(role, language string) string {
	switch role {
	case "security":
		if language == "go" {
			return "handoffs/security-review-go-v1.md"
		}
		return "handoffs/security-review-v1.md"
	case "provenance":
		// Language doesn't fork provenance — the template covers
		// PyPI, npm, crates.io, and Go modules in one file.
		return "handoffs/provenance-review-v1.md"
	default:
		// Enum validation ensures we never reach this; keep the
		// fallthrough explicit to catch programmer error in tests.
		return ""
	}
}

// writeHandoff sends rendered to either stdout (when output is
// empty) or to the named file (respecting --force for overwrite
// safety). Nothing is written if rendered is zero-length — callers
// should treat that as an internal error, but it shouldn't happen
// because RenderTemplate always returns at least the original bytes.
func writeHandoff(output string, force bool, rendered []byte) error {
	if output == "" {
		if _, err := os.Stdout.Write(rendered); err != nil {
			return fmt.Errorf("write stdout: %w", err)
		}
		return nil
	}

	flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !force {
		flag |= os.O_EXCL
	}
	f, err := os.OpenFile(output, flag, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s already exists; pass --force to overwrite", output)
		}
		return fmt.Errorf("open %s: %w", output, err)
	}
	defer f.Close()
	if _, err := f.Write(rendered); err != nil {
		return fmt.Errorf("write %s: %w", output, err)
	}
	return nil
}

// reportToStderr prints the informational post-run report: which
// template was used, whether the embedded fallback was in play, and
// which `{PLACEHOLDER}` tokens remained unfilled. The report is
// stderr-only so `signatory handoff … > file.md` captures only the
// template content.
func reportToStderr(source string, embedded bool, unfilled []string) {
	fmt.Fprintf(os.Stderr, "# template: %s\n", source)
	if embedded {
		fmt.Fprintln(os.Stderr, "# note: read from embedded fallback; run `signatory init` to materialize ./templates/")
	}
	if len(unfilled) > 0 {
		fmt.Fprintf(os.Stderr, "# unfilled placeholders (pass flags to substitute): %v\n", unfilled)
	}
}
