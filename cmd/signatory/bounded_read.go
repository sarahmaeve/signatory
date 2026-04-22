package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Analyst-output file size cap for the CLI ingest and format-check
// paths. Prevents an operator who is socially engineered into running
// `signatory ingest /tmp/dropped.json` against a pathologically large
// file from losing the process to OOM before validation runs.
//
// Threat model (from design/analysis/signatory-security-v1.json F003):
// the adversary is weak — a file must already be on disk AND the
// operator must invoke signatory against it — but the fix is cheap,
// closes the shape entirely, and matches the house TDD posture for
// bounded-input hardening.
//
// Sizing: 10 MiB is ~two orders of magnitude larger than any observed
// legitimate analyst output (typical outputs are tens of KiB; the
// largest seen during dogfood are under 100 KiB). Gives headroom for
// analyst outputs that embed large transcripts or evidence arrays
// without allowing genuine OOM vectors.
//
// MCP ingest (internal/mcp/tools/ingest_analysis.go) doesn't need
// this cap because internal/mcp/jsonrpc.go:maxLineBytes (64 KiB)
// already bounds the entire JSON-RPC frame at the transport layer.
// This helper covers the two CLI-only paths where the frame cap
// doesn't apply.
const maxAnalystFileBytes = 10 * 1024 * 1024

// errAnalystFileTooLarge is returned by readBoundedAnalystFile when
// the input exceeds maxAnalystFileBytes. Sentinel rather than a
// string-only error so tests can errors.Is-check it and future
// CLI-level classification (exitCodeFor) can branch on it if we
// ever want a distinct exit code for size-rejection.
var errAnalystFileTooLarge = errors.New("analyst-output file exceeds size cap")

// readBoundedAnalystFile reads a file with a hard cap on bytes
// consumed, returning errAnalystFileTooLarge (wrapped with size
// context) if the cap is exceeded. Used by `signatory ingest` and
// `signatory format-check` — the two CLI paths where an unbounded
// os.ReadFile would expose the OOM shape.
//
// TOCTOU-proof: uses io.LimitReader(f, maxAnalystFileBytes+1) rather
// than an os.Stat / os.ReadFile pair. With Stat-then-Read, an
// attacker who can grow the file between the two syscalls bypasses
// the check. With LimitReader capped at N+1, any attempt to consume
// more than N bytes is caught at the reader layer regardless of
// concurrent writers. The extra +1 is the "over by at least one
// byte" signal — if we read N+1 bytes, the file is strictly larger
// than the cap.
//
// Allocation: io.ReadAll grows its buffer as it reads, so a small
// legitimate file allocates only what it consumes. Only pathological
// inputs allocate the full cap+1. That's the worst case — bounded,
// predictable, and far below the OOM threshold on any host signatory
// might reasonably run on.
func readBoundedAnalystFile(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: CLI flag --file; operator-supplied path is the intended input
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only file; close error has no recoverable action

	// +1 so we can detect "strictly over the cap" as distinct from
	// "exactly at the cap." Reading exactly maxAnalystFileBytes
	// succeeds; reading maxAnalystFileBytes+1 fails.
	limited := io.LimitReader(f, maxAnalystFileBytes+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	if int64(len(buf)) > maxAnalystFileBytes {
		return nil, fmt.Errorf("%w: %q is larger than the %d-byte cap",
			errAnalystFileTooLarge, path, maxAnalystFileBytes)
	}
	return buf, nil
}
