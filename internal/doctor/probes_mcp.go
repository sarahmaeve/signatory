// Probes that inspect the project-scoped .mcp.json wiring: that
// the file's command path resolves to the same binary the user
// is running today, and (in conjunction with signatory-db) that
// the env block's SIGNATORY_DB pin matches the shell.
//
// All three of TROUBLESHOOTING's "MCP and Claude Code wiring"
// failure modes route through this file: command-path drift, env
// drift, missing config.
package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// mcpConfig is the minimal subset of .mcp.json we read. Claude
// Code accepts more fields than this; we only care about the
// signatory server's command + env, so a permissive json.Unmarshal
// against this shape ignores everything else without error.
type mcpConfig struct {
	MCPServers map[string]mcpServer `json:"mcpServers"`
}

type mcpServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// readMCPConfig is a small helper used by both mcp-binary-matches
// and signatory-db. Returns (nil, os.ErrNotExist) when the cwd has
// no .mcp.json — callers translate that into the appropriate
// per-probe message rather than fail-loud, since the project may
// just not be a signatory consumer.
func readMCPConfig(r resolved) (*mcpServer, string, error) {
	cwd, err := r.getwd()
	if err != nil {
		return nil, "", fmt.Errorf("resolve working directory: %w", err)
	}
	path := filepath.Join(cwd, ".mcp.json")
	data, err := r.readFile(path)
	if err != nil {
		return nil, path, err
	}
	var cfg mcpConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, path, fmt.Errorf("parse %s: %w", path, err)
	}
	srv, ok := cfg.MCPServers["signatory"]
	if !ok {
		return nil, path, fmt.Errorf("%s has no mcpServers.signatory entry", path)
	}
	return &srv, path, nil
}

// expandEnvVars resolves ${HOME} and $HOME-style placeholders in s
// using the provided getenv. We deliberately roll our own narrow
// expander rather than os.ExpandEnv because the user's process env
// is the wrong source for a doctor probe — the question is "what
// would Claude Code see," and CC reads from the user's login
// environment which (modulo restart drift) is the same one the
// doctor process has via os.Getenv. Using a seam keeps tests
// hermetic regardless.
func expandEnvVars(s string, getenv func(string) string) string {
	return os.Expand(s, getenv)
}

// probeMCPBinaryMatches verifies the binary path declared in
// .mcp.json points at a real file AND matches the binary the user
// has on PATH today. TROUBLESHOOTING calls out both failure modes:
//
//   - GOBIN drift: .mcp.json references ${HOME}/go/bin/signatory
//     but the user's signatory was installed somewhere else, so
//     Claude Code launches a stranger or nothing.
//   - Build drift: .mcp.json command path exists but is older than
//     the signatory the user is using interactively, producing
//     surprising version skew.
//
// Without a .mcp.json we return warn (not fail), deferring the
// "you don't have one" message to mcp-config-present. Two probes
// pointing at the same problem with different recommendations
// would just confuse the user.
func probeMCPBinaryMatches(r resolved) Result {
	const name = "mcp-binary-matches"

	srv, path, err := readMCPConfig(r)
	if errors.Is(err, os.ErrNotExist) {
		return Result{
			Name:    name,
			Status:  StatusWarn,
			Message: fmt.Sprintf("no .mcp.json at %s — cannot verify command path", path),
			Fix:     "see mcp-config-present for the underlying fix",
		}
	}
	if err != nil {
		return Result{
			Name:    name,
			Status:  StatusFail,
			Message: err.Error(),
			Fix:     "fix the .mcp.json syntax or restore it from the signatory repo",
		}
	}

	if srv.Command == "" {
		return Result{
			Name:    name,
			Status:  StatusFail,
			Message: fmt.Sprintf("%s: mcpServers.signatory.command is empty", path),
			Fix:     "set mcpServers.signatory.command to the absolute path of your signatory binary",
		}
	}

	declared := expandEnvVars(srv.Command, r.getenv)
	if _, err := r.stat(declared); errors.Is(err, os.ErrNotExist) {
		return Result{
			Name:    name,
			Status:  StatusFail,
			Message: fmt.Sprintf(".mcp.json command %q resolves to %q which does not exist", srv.Command, declared),
			Fix:     fmt.Sprintf("install signatory at %s, or edit %s to point at your installed binary", declared, path),
		}
	} else if err != nil {
		return Result{
			Name:    name,
			Status:  StatusWarn,
			Message: fmt.Sprintf("stat %s: %v", declared, err),
			Fix:     "investigate the path manually",
		}
	}

	// Compare to os.Executable. Both sides are normalized via
	// filepath.Clean — but NOT EvalSymlinks: a symlink chain that
	// resolves to the same target is fine, and following symlinks
	// here would mask the user-visible distinction between
	// `/usr/local/bin/signatory` (symlink) and the real location.
	// What matters is "does Claude Code launch the same command
	// the user types," which is a textual question.
	exe, err := r.executable()
	if err != nil {
		return Result{
			Name:    name,
			Status:  StatusWarn,
			Message: fmt.Sprintf(".mcp.json command resolves to %s, but cannot determine running binary path: %v", declared, err),
			Fix:     "ignore if you trust the .mcp.json path, otherwise verify with `which signatory`",
		}
	}
	if filepath.Clean(declared) != filepath.Clean(exe) {
		return Result{
			Name:    name,
			Status:  StatusFail,
			Message: fmt.Sprintf(".mcp.json points at %s; running signatory is %s", declared, exe),
			Fix:     fmt.Sprintf("edit mcpServers.signatory.command in %s to %s, or reinstall signatory at the declared path", path, exe),
		}
	}
	return Result{
		Name:    name,
		Status:  StatusOK,
		Message: declared,
	}
}
