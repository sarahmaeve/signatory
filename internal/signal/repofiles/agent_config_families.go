package repofiles

import "regexp"

// AgentConfigFamilies returns the AI-agent configuration file family
// list in a deterministic, declared order. Parallel to Families() but
// targets a distinct detection surface: files that AI coding agents
// (Claude Code, Cursor, aider, Zed AI, Continue.dev, Windsurf,
// Codex CLI, …) read as instruction or configuration input.
//
// The corpus is motivated by the Trapdoor 2026-05 campaign
// (design/threat-landscape/2026-05-24-trapdoor-crypto-stealer.md),
// which weaponized .cursorrules and CLAUDE.md as carriers for
// zero-width-Unicode prompt-injection payloads. The same carrier
// shape generalizes across every AI-coding-agent tool that reads a
// repo-local instruction file: any of these files arriving in a repo
// without contributor review (postinstall write, malicious PR, drop
// in a tarball) is the same payload-delivery surface.
//
// Returns a fresh slice on each call so callers cannot mutate the
// package-level declaration.
func AgentConfigFamilies() []Family {
	out := make([]Family, len(agentConfigFamilies))
	copy(out, agentConfigFamilies)
	return out
}

// agentConfigFamilies is the canonical declaration. Order matters: it
// drives the iteration order of the emitted signal's value map and
// the test assertions that lock in coverage.
//
// Detector style differs slightly from Families() because agent-config
// filenames are typically specific (no extension tolerance — the agent
// reads the exact filename the tool documents) and many are dotfiles.
// Where the tool's documented filename is a single fixed string we use
// exact(); where the family covers multiple files of the same shape
// in a dedicated directory (cursor_rules_dir's .mdc files) we use a
// scoped regex.
var agentConfigFamilies = []Family{
	{
		// .cursorrules — Cursor IDE's original per-repo agent
		// instruction file. Read on workspace open, contents fed to
		// the model as a system-prompt prefix. Trapdoor 2026-05 IOC.
		Name:      "cursorrules",
		Dirs:      []string{"."},
		Detector:  exact(".cursorrules"),
		Preferred: []string{".cursorrules"},
	},
	{
		// CLAUDE.md — Claude Code's per-repo custom-instructions
		// file. Read on session start, contents prepended to the
		// system prompt. Trapdoor 2026-05 IOC. Case-insensitive
		// match: the canonical form is uppercase, but lowercased
		// claude.md is observed in older projects.
		Name:      "claude_md",
		Dirs:      []string{"."},
		Detector:  stemWithExt("CLAUDE"),
		Preferred: []string{"CLAUDE.md"},
	},
	{
		// AGENTS.md — emerging cross-tool convention for per-repo
		// agent instructions (Codex CLI and others). Same carrier
		// shape as CLAUDE.md.
		Name:      "agents_md",
		Dirs:      []string{"."},
		Detector:  stemWithExt("AGENTS"),
		Preferred: []string{"AGENTS.md"},
	},
	{
		// .claude/settings.json — Claude Code's per-repo settings
		// (model, tool permissions, hooks). A malicious settings
		// file can configure agent behavior without touching prose
		// instructions — e.g. adding allow-lists for tools that
		// would otherwise prompt the user.
		Name:      "claude_dir_settings",
		Dirs:      []string{".claude"},
		Detector:  exact("settings.json"),
		Preferred: []string{"settings.json"},
	},
	{
		// .claude/CLAUDE.md — alternate location for Claude Code's
		// custom-instructions file. Distinct from the root CLAUDE.md
		// family because the analyst may want to distinguish
		// location (root vs subdir) when interpreting the finding.
		Name:      "claude_dir_claude_md",
		Dirs:      []string{".claude"},
		Detector:  stemWithExt("CLAUDE"),
		Preferred: []string{"CLAUDE.md"},
	},
	{
		// .cursor/rules/*.mdc — Cursor's newer per-repo rules
		// directory. Files have arbitrary user-chosen names
		// (001-base.mdc, language-style.mdc, …) following the
		// .mdc extension convention. The family has no single
		// canonical filename — every matched file is of interest;
		// Preferred is empty by design.
		Name:      "cursor_rules_dir",
		Dirs:      []string{".cursor/rules"},
		Detector:  regexp.MustCompile(`(?i)^.+\.mdc$`),
		Preferred: nil,
	},
	{
		// .aider.conf.yml — aider's repo-level config file. Read on
		// startup; supports model selection, system-prompt overrides,
		// and read/write file lists. Detector accepts both .yml and
		// .yaml extensions per common practice.
		Name:      "aider_conf",
		Dirs:      []string{"."},
		Detector:  regexp.MustCompile(`(?i)^\.aider\.conf\.ya?ml$`),
		Preferred: []string{".aider.conf.yml"},
	},
	{
		// .zed/settings.json — Zed editor's per-repo configuration.
		// Loaded on workspace open ahead of user-level config; can
		// override LSP commands and AI-feature defaults.
		Name:      "zed_settings",
		Dirs:      []string{".zed"},
		Detector:  exact("settings.json"),
		Preferred: []string{"settings.json"},
	},
	{
		// .continue/config.json — Continue.dev's per-repo
		// configuration. Defines models, prompt templates, and tool
		// connections; arbitrary instruction content via the
		// systemMessage field.
		Name:      "continue_config",
		Dirs:      []string{".continue"},
		Detector:  exact("config.json"),
		Preferred: []string{"config.json"},
	},
	{
		// .windsurfrules — Windsurf's per-repo agent instruction
		// file. Parallel role to .cursorrules in the Windsurf
		// (formerly Codeium) coding-agent toolchain.
		Name:      "windsurfrules",
		Dirs:      []string{"."},
		Detector:  exact(".windsurfrules"),
		Preferred: []string{".windsurfrules"},
	},
}
