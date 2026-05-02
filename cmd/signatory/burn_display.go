package main

import (
	"fmt"
	"time"
)

// burnDisplayInput is the minimal shape for rendering the canonical
// "*** BURNED: ... ***" line. The three commands that surface burns
// (signatory analyze, summary, show-analyses) each carry a slightly
// different cascade-context wrapper — summary.BurnSnapshot,
// AnalysisDisplay.BurnVia, store.EffectiveBurnContext — but the
// rendering depends only on the four-or-five primitive fields below.
// Keeping the formatter narrow on these primitives means it doesn't
// have to know about three carrier types.
//
// ViaOwnerURI and ViaRole are the cascade context. Both empty
// indicates a direct burn (no "via ..." clause in the rendered
// line); both populated produces the cascade form. Treat them as
// a unit — populating one without the other is a programming
// error and falls through to the direct form (defensively).
type burnDisplayInput struct {
	Reason      string
	BurnedBy    string
	BurnedAt    time.Time
	ViaOwnerURI string
	ViaRole     string
}

// formatBurnLine returns the canonical "*** BURNED: ... ***" line
// — direct form by default, cascade form when both ViaOwnerURI and
// ViaRole are populated. The returned string has no surrounding
// whitespace; callers add their own newlines / blank lines as
// fits their surface (summary uses leading + trailing \n; analyze
// uses just trailing; show-analyses uses trailing + extra blank).
//
// The format is intentionally identical across the three commands
// so cascaded-burn output reads the same regardless of which verb
// the user ran. Changing the format here is a UI-level change
// that affects every burn surface at once — preferred over
// per-command drift.
func formatBurnLine(in burnDisplayInput) string {
	if in.ViaOwnerURI != "" && in.ViaRole != "" {
		return fmt.Sprintf("*** BURNED: %s (via %s %s, by %s, %s) ***",
			in.Reason, in.ViaRole, in.ViaOwnerURI,
			in.BurnedBy, in.BurnedAt.Format(time.RFC3339))
	}
	return fmt.Sprintf("*** BURNED: %s (by %s, %s) ***",
		in.Reason, in.BurnedBy, in.BurnedAt.Format(time.RFC3339))
}
