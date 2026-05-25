package agentconfig

import (
	"path"
	"regexp"
	"slices"
)

// Locus describes one AI-agent toolchain's repo-level surface. The
// fields pair the file-detector shape (consumed by inventory
// scanners) with the runtime-path substring patterns (consumed by
// runtime-write detectors).
type Locus struct {
	// Name is the stable key used by every consumer (signal value
	// keys, test assertions). Lowercase, underscore-separated.
	Name string

	// Dirs is the list of clone-relative directories to scan for
	// this locus's filename detector. "." means clone root.
	Dirs []string

	// Detector is the anchored, case-insensitive regex a candidate
	// filename must match to belong to this locus. Operates on the
	// basename only (the scanner walks Dirs and applies Detector
	// per entry).
	Detector *regexp.Regexp

	// Preferred lists canonical filenames in precedence order, used
	// by repofiles' Evaluate() ranker. May be empty when no single
	// canonical filename exists (e.g. .cursor/rules/*.mdc).
	Preferred []string

	// RuntimePathPrefixes are substring patterns the source-AST
	// persistence-write catalog matches against a writer's resolved
	// first-arg path. May be broader than Dirs because a runtime
	// write to anywhere under the locus directory is suspicious,
	// not just to the specific Detector-matched file:
	//
	//   - Family detector for aider:  .aider.conf.yml (specific file)
	//   - Runtime prefix for aider:   /.aider/ (any path under .aider/)
	//
	// All entries must start with "/" so the substring match is
	// anchored at a path boundary, not a substring of an unrelated
	// directory name. Use a leading "/" + filename for file-level
	// (e.g. "/CLAUDE.md"); use a leading "/" + dir + "/" for
	// directory-level (e.g. "/.aider/").
	RuntimePathPrefixes []string
}

// Loci returns the AI-agent locus declarations in deterministic
// order. Returns a fresh slice on each call so callers cannot
// mutate the package-level declaration — the inner Dirs, Preferred,
// and RuntimePathPrefixes slices are deep-copied because a plain
// copy() would leave their backing arrays aliased to the singleton.
//
// Order matters: it drives the iteration order of derived outputs
// (repofiles.AgentConfigFamilies, astfeature's appended runtime
// prefixes) and the test assertions that lock in coverage.
func Loci() []Locus {
	out := make([]Locus, len(loci))
	for i, l := range loci {
		out[i] = Locus{
			Name:                l.Name,
			Dirs:                slices.Clone(l.Dirs),
			Detector:            l.Detector,
			Preferred:           slices.Clone(l.Preferred),
			RuntimePathPrefixes: slices.Clone(l.RuntimePathPrefixes),
		}
	}
	return out
}

// IsConfigPath reports whether a posix-style path identifies a
// file that any Locus would detect. Operates on a single path
// string — no filesystem access — so it composes cleanly with
// consumers that work on virtual paths (tarball entries, PR diff
// entries) rather than on a cloned working tree.
//
// Path conventions: posix separators ("/"), no leading slash.
// Empty string is never agent-config.
func IsConfigPath(p string) bool {
	if p == "" {
		return false
	}
	dir := path.Dir(p)
	base := path.Base(p)
	for _, l := range loci {
		if !slices.Contains(l.Dirs, dir) {
			continue
		}
		if l.Detector != nil && l.Detector.MatchString(base) {
			return true
		}
	}
	return false
}

// RuntimePersistencePrefixes returns the union of every locus's
// RuntimePathPrefixes — the flat substring catalog source-AST
// persistence-write detectors should append to their own non-AI
// catalog.
//
// Order matches Loci(); duplicates are dropped (a future Locus
// might legitimately share a prefix with another). Returns a fresh
// slice on each call.
func RuntimePersistencePrefixes() []string {
	seen := make(map[string]struct{}, 16)
	out := make([]string, 0, 16)
	for _, l := range loci {
		for _, prefix := range l.RuntimePathPrefixes {
			if _, dup := seen[prefix]; dup {
				continue
			}
			seen[prefix] = struct{}{}
			out = append(out, prefix)
		}
	}
	return out
}

// loci is the canonical declaration. Order matters per Loci()'s
// contract. The list grew from the Trapdoor 2026-05 campaign's
// IOC corpus (.cursorrules, CLAUDE.md) plus the broader
// AI-coding-agent landscape.
var loci = []Locus{
	{
		// .cursorrules — Cursor IDE's original per-repo agent
		// instruction file. Read on workspace open; contents fed to
		// the model as a system-prompt prefix. Trapdoor 2026-05 IOC.
		Name:                "cursorrules",
		Dirs:                []string{"."},
		Detector:            Exact(".cursorrules"),
		Preferred:           []string{".cursorrules"},
		RuntimePathPrefixes: []string{"/.cursorrules"},
	},
	{
		// CLAUDE.md — Claude Code's per-repo custom-instructions file.
		// Trapdoor 2026-05 IOC. Case-insensitive detector matches
		// claude.md / Claude.md too; canonical form is uppercase.
		Name:                "claude_md",
		Dirs:                []string{"."},
		Detector:            StemWithExt("CLAUDE"),
		Preferred:           []string{"CLAUDE.md"},
		RuntimePathPrefixes: []string{"/CLAUDE.md"},
	},
	{
		// AGENTS.md — cross-tool convention (Codex CLI, GitHub
		// Copilot per-agent override, and others) for per-repo
		// agent instructions. Same carrier shape as CLAUDE.md.
		Name:                "agents_md",
		Dirs:                []string{"."},
		Detector:            StemWithExt("AGENTS"),
		Preferred:           []string{"AGENTS.md"},
		RuntimePathPrefixes: []string{"/AGENTS.md"},
	},
	{
		// GEMINI.md — Google Gemini agent's per-repo instruction
		// file (also read by GitHub Copilot as a per-agent override
		// per the Copilot docs). Same carrier shape as CLAUDE.md.
		Name:                "gemini_md",
		Dirs:                []string{"."},
		Detector:            StemWithExt("GEMINI"),
		Preferred:           []string{"GEMINI.md"},
		RuntimePathPrefixes: []string{"/GEMINI.md"},
	},
	{
		// .github/copilot-instructions.md — GitHub Copilot's
		// repository-wide custom instructions. Loaded automatically
		// into every Copilot request once present. Single fixed
		// filename at a fixed path; see
		// https://docs.github.com/en/copilot/how-tos/copilot-on-github/customize-copilot/add-custom-instructions/add-repository-instructions
		Name:                "copilot_repo_instructions",
		Dirs:                []string{".github"},
		Detector:            Exact("copilot-instructions.md"),
		Preferred:           []string{"copilot-instructions.md"},
		RuntimePathPrefixes: []string{"/.github/copilot-instructions.md"},
	},
	{
		// .github/instructions/*.instructions.md — GitHub Copilot's
		// path-scoped instructions. Filename pattern is
		// "<name>.instructions.md"; required YAML frontmatter
		// applyTo field controls which files the instructions apply
		// to. Multiple files per project; Preferred is nil because
		// no single canonical name exists. The Copilot docs note
		// optional subdirectories under .github/instructions/ —
		// v0.1 scans only the flat directory (subdirectory walking
		// is a documented conservative gap).
		Name:                "copilot_path_instructions",
		Dirs:                []string{".github/instructions"},
		Detector:            regexp.MustCompile(`(?i)^.+\.instructions\.md$`),
		Preferred:           nil,
		RuntimePathPrefixes: []string{"/.github/instructions/"},
	},
	{
		// .claude/settings.json — Claude Code's per-repo settings
		// (model, tool permissions, hooks). A malicious settings
		// file configures agent behavior without touching prose
		// instructions — e.g. allow-listing tools that would
		// otherwise prompt.
		Name:                "claude_dir_settings",
		Dirs:                []string{".claude"},
		Detector:            Exact("settings.json"),
		Preferred:           []string{"settings.json"},
		RuntimePathPrefixes: []string{"/.claude/"},
	},
	{
		// .claude/CLAUDE.md — alternate location for Claude Code's
		// custom-instructions file. Same RuntimePathPrefixes as the
		// settings locus (any write under .claude/ is suspicious);
		// the prefix dedupe in RuntimePersistencePrefixes handles
		// the overlap.
		Name:                "claude_dir_claude_md",
		Dirs:                []string{".claude"},
		Detector:            StemWithExt("CLAUDE"),
		Preferred:           []string{"CLAUDE.md"},
		RuntimePathPrefixes: []string{"/.claude/"},
	},
	{
		// .cursor/rules/*.mdc — Cursor's newer per-repo rules dir.
		// Files have arbitrary user-chosen names (001-base.mdc,
		// language-style.mdc, …) following the .mdc convention.
		// Preferred is empty by design — every matched file is of
		// interest, no single canonical name.
		Name:                "cursor_rules_dir",
		Dirs:                []string{".cursor/rules"},
		Detector:            regexp.MustCompile(`(?i)^.+\.mdc$`),
		Preferred:           nil,
		RuntimePathPrefixes: []string{"/.cursor/"},
	},
	{
		// .aider.conf.yml — aider's repo-level config file. Supports
		// model selection, system-prompt overrides, and read/write
		// file lists. Detector accepts both .yml and .yaml.
		Name:                "aider_conf",
		Dirs:                []string{"."},
		Detector:            regexp.MustCompile(`(?i)^\.aider\.conf\.ya?ml$`),
		Preferred:           []string{".aider.conf.yml"},
		RuntimePathPrefixes: []string{"/.aider/"},
	},
	{
		// .zed/settings.json — Zed editor's per-repo configuration.
		// Loaded on workspace open ahead of user-level config; can
		// override LSP commands and AI-feature defaults.
		Name:                "zed_settings",
		Dirs:                []string{".zed"},
		Detector:            Exact("settings.json"),
		Preferred:           []string{"settings.json"},
		RuntimePathPrefixes: []string{"/.zed/"},
	},
	{
		// .codex/instructions.md — OpenAI Codex CLI's per-repo
		// instruction file. The runtime prefix /.codex/ was already
		// in astfeature's persistence-path catalog; this Locus closes
		// the divergence by giving it a Family for inventory and
		// content-injection scanning too.
		//
		// Preferred lists every Detector-accepted form in canonical
		// precedence order: instructions.md is preferred (the named
		// instruction file), then the four config-file variants in
		// json → yaml → yml → toml order. Listing all five keeps
		// canonical-form ranking deterministic regardless of which
		// file a repo ships, and avoids falling through to
		// repofiles.rank's Phase-3 alphabetical fallback.
		Name:                "codex_instructions",
		Dirs:                []string{".codex"},
		Detector:            regexp.MustCompile(`(?i)^(instructions\.md|config\.(json|yaml|yml|toml))$`),
		Preferred:           []string{"instructions.md", "config.json", "config.yaml", "config.yml", "config.toml"},
		RuntimePathPrefixes: []string{"/.codex/"},
	},
	{
		// .continue/config.json — Continue.dev's per-repo
		// configuration. Defines models, prompt templates, and tool
		// connections; arbitrary instruction content via the
		// systemMessage field.
		Name:                "continue_config",
		Dirs:                []string{".continue"},
		Detector:            Exact("config.json"),
		Preferred:           []string{"config.json"},
		RuntimePathPrefixes: []string{"/.continue/"},
	},
	{
		// .windsurfrules — Windsurf's per-repo agent instruction
		// file. Parallel role to .cursorrules in the Windsurf
		// (formerly Codeium) coding-agent toolchain.
		Name:                "windsurfrules",
		Dirs:                []string{"."},
		Detector:            Exact(".windsurfrules"),
		Preferred:           []string{".windsurfrules"},
		RuntimePathPrefixes: []string{"/.windsurfrules", "/.windsurf/"},
	},
}

// StemWithExt builds a detector for filenames matching <stem>.<ext>
// with single-extension tolerance. The extension is [^.]+ rather
// than .+ so multi-dot artifacts (README.md.bak, .CLAUDE.md.swp)
// don't match — those aren't the canonical file, they're editor
// backups.
//
// Exported as the single source of truth for the stem-tolerant
// filename pattern. The hygiene-file scanner in repofiles consumes
// it for README / SECURITY / CODEOWNERS / etc.; the AI-agent locus
// declarations consume it for CLAUDE / AGENTS / GEMINI / etc.
// Keeping one implementation prevents the two surfaces from
// drifting apart.
func StemWithExt(stem string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)^` + regexp.QuoteMeta(stem) + `(\.[^.]+)?$`)
}

// Exact builds a detector for filenames matching exactly the given
// name (case-insensitive, no extension tolerance). Companion to
// StemWithExt; shared with the hygiene-file scanner for the same
// drift-prevention reason.
func Exact(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)^` + regexp.QuoteMeta(name) + `$`)
}
