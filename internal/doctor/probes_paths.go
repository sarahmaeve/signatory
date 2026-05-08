// Probes that inspect filesystem paths for the local setup:
// ~/.signatory/ data dir, the project-scoped .mcp.json, and
// the .claude/skills/ directory. All three are warn-class probes
// (not fail) because their absence degrades the workflow without
// blocking the binary itself — and their detection logic shares
// helpers (cwd resolution, stat-as-directory) collected here.
package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// probeHomeSignatoryDir checks that the user's signatory data dir
// (~/.signatory/) is in a usable state. We deliberately tolerate
// "doesn't exist yet" because every signatory verb that needs the
// dir creates it on demand; what we're catching here is the rare
// but real "path is wedged" failure mode (e.g., a regular file
// where a directory should be, or an unresolvable $HOME).
func probeHomeSignatoryDir(r resolved) Result {
	const name = "home-signatory-dir"

	home, err := r.userHomeDir()
	if err != nil {
		return Result{
			Name:    name,
			Status:  StatusFail,
			Message: fmt.Sprintf("cannot resolve user home directory: %v", err),
			Fix:     "ensure $HOME is set in your environment",
		}
	}
	path := filepath.Join(home, ".signatory")

	info, err := r.stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Result{
			Name:    name,
			Status:  StatusOK,
			Message: fmt.Sprintf("%s does not exist yet (will be created on first use)", path),
		}
	}
	if err != nil {
		return Result{
			Name:    name,
			Status:  StatusFail,
			Message: fmt.Sprintf("stat %s: %v", path, err),
			Fix:     fmt.Sprintf("inspect %s manually; expected a directory or no entry", path),
		}
	}
	if !info.IsDir() {
		return Result{
			Name:    name,
			Status:  StatusFail,
			Message: fmt.Sprintf("%s exists but is not a directory", path),
			Fix:     fmt.Sprintf("remove the file at %s; signatory will recreate it as a directory", path),
		}
	}
	return Result{
		Name:    name,
		Status:  StatusOK,
		Message: path,
	}
}

// probeMCPConfigPresent looks for a project-scoped .mcp.json in
// the current working directory. TROUBLESHOOTING's "skills aren't
// visible in Claude Code" entry points at this exact failure mode:
// Claude Code was launched from somewhere that didn't have the
// project config, so neither the MCP server nor the slash commands
// loaded.
//
// Warn (not fail): the binary is fully usable from the CLI without
// MCP, and the user may simply not be in a project that wires
// signatory in yet.
func probeMCPConfigPresent(r resolved) Result {
	const name = "mcp-config-present"

	cwd, err := r.getwd()
	if err != nil {
		return Result{
			Name:    name,
			Status:  StatusWarn,
			Message: fmt.Sprintf("cannot resolve working directory: %v", err),
			Fix:     "run signatory doctor from a directory you have read access to",
		}
	}
	path := filepath.Join(cwd, ".mcp.json")
	info, err := r.stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Result{
			Name:    name,
			Status:  StatusWarn,
			Message: fmt.Sprintf("no .mcp.json in %s — Claude Code will not load the signatory MCP server here", cwd),
			Fix:     "cd into the signatory clone (or copy .mcp.json into the project you want to evaluate from)",
		}
	}
	if err != nil {
		return Result{
			Name:    name,
			Status:  StatusWarn,
			Message: fmt.Sprintf("stat %s: %v", path, err),
			Fix:     "inspect the file manually",
		}
	}
	if info.IsDir() {
		return Result{
			Name:    name,
			Status:  StatusFail,
			Message: fmt.Sprintf("%s is a directory; expected a JSON file", path),
			Fix:     fmt.Sprintf("remove %s and replace with the .mcp.json from the signatory repo", path),
		}
	}
	return Result{
		Name:    name,
		Status:  StatusOK,
		Message: path,
	}
}

// probeSkillsPresent checks for the analyze + vet-dependency skill
// directories under .claude/skills/ relative to cwd. Mirrors the
// same launched-from-wrong-place failure mode mcp-config-present
// catches, but distinct because a project may legitimately ship
// .mcp.json without the skills (e.g., a downstream that only wants
// the MCP cache lookup, not the full pipeline).
func probeSkillsPresent(r resolved) Result {
	const name = "skills-present"

	cwd, err := r.getwd()
	if err != nil {
		return Result{
			Name:    name,
			Status:  StatusWarn,
			Message: fmt.Sprintf("cannot resolve working directory: %v", err),
			Fix:     "run signatory doctor from a directory you have read access to",
		}
	}
	skillsDir := filepath.Join(cwd, ".claude", "skills")
	required := []string{"analyze", "vet-dependency"}

	var missing []string
	for _, s := range required {
		path := filepath.Join(skillsDir, s)
		info, statErr := r.stat(path)
		if statErr != nil || !info.IsDir() {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		return Result{
			Name:    name,
			Status:  StatusWarn,
			Message: fmt.Sprintf("missing skills under %s: %v", skillsDir, missing),
			Fix:     "cd into the signatory clone (or copy .claude/skills/ into the project you want to evaluate from)",
		}
	}
	return Result{
		Name:    name,
		Status:  StatusOK,
		Message: skillsDir,
	}
}
