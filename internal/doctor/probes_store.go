package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// openStoreTimeout bounds the signatory-db open check. SQLite
// opens are normally sub-millisecond, but a flock contention against
// a long-running migration could legitimately stall — we'd rather
// fail with a timeout than let `signatory doctor` hang indefinitely.
const openStoreTimeout = 3 * time.Second

// probeSignatoryDB does two distinct checks under one probe name:
//
//  1. The DB at the resolved path opens cleanly (fail on error).
//  2. The shell's $SIGNATORY_DB matches what .mcp.json's
//     mcpServers.signatory.env.SIGNATORY_DB pins (warn on mismatch).
//
// The second is exactly the failure mode TROUBLESHOOTING describes:
// "MCP server starts but tools error immediately" because the CLI
// and MCP looked at different SQLite files.
//
// One probe rather than two so the user sees a single line per
// concern, and the Fix can address whichever check failed first.
//
// If the OpenStore seam is nil (default — production wires it
// from cmd/signatory) the open check is skipped and we still
// report the consistency check; this lets internal/doctor be used
// from contexts that don't depend on internal/store.
func probeSignatoryDB(r resolved) Result {
	const name = "signatory-db"

	// Layer 1: open check (only when the seam is wired).
	if r.openStore != nil && r.dbPath != "" {
		ctx, cancel := context.WithTimeout(context.Background(), openStoreTimeout)
		defer cancel()
		if err := r.openStore(ctx, r.dbPath); err != nil {
			return Result{
				Name:    name,
				Status:  StatusFail,
				Message: fmt.Sprintf("open %s: %v", r.dbPath, err),
				Fix:     "check the path is writable and not held open by another process; try `rm` if the file is corrupt and re-run a signatory verb to recreate it",
			}
		}
	}

	// Layer 2: consistency check between $SIGNATORY_DB and .mcp.json.
	envValue := r.getenv("SIGNATORY_DB")
	srv, _, err := readMCPConfig(r)
	if errors.Is(err, os.ErrNotExist) || err != nil || srv == nil {
		// No mcp.json — nothing to compare against. We've already
		// done the open check above, so this is the success path.
		return Result{
			Name:    name,
			Status:  StatusOK,
			Message: fmt.Sprintf("opens cleanly at %s", coalesceDBPath(r.dbPath)),
		}
	}

	mcpValue := ""
	if srv.Env != nil {
		mcpValue = srv.Env["SIGNATORY_DB"]
	}
	mcpExpanded := expandEnvVars(mcpValue, r.getenv)
	envExpanded := expandEnvVars(envValue, r.getenv)

	// Both unset: both sides will use the default; consistent.
	if mcpExpanded == "" && envExpanded == "" {
		return Result{
			Name:    name,
			Status:  StatusOK,
			Message: fmt.Sprintf("opens cleanly at %s; no SIGNATORY_DB pin in env or .mcp.json (both use the default)", coalesceDBPath(r.dbPath)),
		}
	}
	// Either side empty while the other is set is a mismatch — Claude
	// Code's MCP server and the interactive CLI will look at
	// different stores.
	if mcpExpanded != envExpanded {
		return Result{
			Name:    name,
			Status:  StatusWarn,
			Message: fmt.Sprintf("SIGNATORY_DB drift: shell=%q .mcp.json=%q", envExpanded, mcpExpanded),
			Fix:     "set SIGNATORY_DB to the same value in both places, or unset it in both and rely on the default",
		}
	}
	return Result{
		Name:    name,
		Status:  StatusOK,
		Message: fmt.Sprintf("opens cleanly at %s; SIGNATORY_DB consistent across env and .mcp.json", envExpanded),
	}
}

// coalesceDBPath surfaces "(default)" when no path was supplied,
// so the message "opens cleanly at (default)" reads correctly
// rather than "opens cleanly at " with a dangling space.
func coalesceDBPath(p string) string {
	if p == "" {
		return "(default ~/.signatory/signatory.db)"
	}
	return p
}
