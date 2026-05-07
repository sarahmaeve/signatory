package tools

import (
	"context"
	"errors"

	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// resolveTargetForFilter translates a user-supplied target string to
// an entity ID for use as a store filter (filter.EntityID). It is the
// MCP-side counterpart to cmd/signatory/show.go's resolveTargetEntityID,
// sharing the underlying store.LookupEntityID alternate-URI walk so
// the show_analyses / show_conclusions / show_methodology tools land
// on the same equivalence class summary already used.
//
// Empty target returns ("", nil) — "no filter" semantics.
//
// Errors propagate verbatim from LookupEntityID. Callers should
// hand the error to mapTargetLookupErr to produce the standard
// MCP response for the three classes (not-found, malformed,
// internal). Keeping the classification in a separate helper means
// each show-* tool's Handle stays linear and the policy lives in
// one place — see mapTargetLookupErr for the mapping rationale.
//
// Added 2026-05-07 alongside store.LookupEntityID to retrofit
// alternate-URI walking onto the read-side MCP tools. Pre-fix, a
// caller asking for `pkg:golang/golang.org/x/mod` got CodeNotFound
// even when the analyses lived at the equivalent
// repo:github/golang/mod row.
func resolveTargetForFilter(ctx context.Context, s store.Store, target string) (string, error) {
	return store.LookupEntityID(ctx, s, target)
}

// mapTargetLookupErr converts a resolveTargetForFilter error into
// the appropriate mcp.Response, or returns nil when err is nil
// (the happy-path passthrough).
//
// Mapping rules — chosen to match the show_synthesis precedent in
// internal/mcp/tools/show_synthesis.go:155 so the three show-* tools
// agree with their sibling on what each error class means to the
// caller:
//
//   - nil error → nil response (caller continues with the resolved
//     entity ID).
//   - ErrNotFound → CodeNotFound. The walk completed and matched
//     nothing; the target is well-formed but absent from the store.
//   - profile.ResolveTarget parse failures → CodeSchemaViolation.
//     The input string didn't parse as any recognised URI form —
//     that's caller-shape, not a missing-data condition. Detected
//     by re-running ResolveTarget; cheap (in-memory) and avoids
//     conflating malformed input with absent data.
//   - Other errors → CodeInternalError (DB / lookup-side problems).
//
// CLI parity: cmd/signatory/show.go folds the malformed-input class
// into ErrNotFound to keep the "no entity matches X" prose uniform
// for end-users. MCP keeps the distinction because LLM consumers
// benefit from the typed error class — a CodeSchemaViolation tells
// the agent "you typed something wrong" while CodeNotFound tells it
// "your target is fine, the store just hasn't seen it yet."
func mapTargetLookupErr(err error, target string) *mcp.Response {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return mcp.Err(mcp.CodeNotFound,
			"entity not found: "+target,
			map[string]string{"target": target})
	}
	if _, perr := profile.ResolveTarget(target); perr != nil {
		return mcp.Err(mcp.CodeSchemaViolation,
			"target "+target+" did not resolve: "+perr.Error(),
			map[string]string{"target": target, "phase": "resolve"})
	}
	return mcp.Err(mcp.CodeInternalError,
		"resolve target failed: "+err.Error(),
		map[string]string{"target": target})
}
