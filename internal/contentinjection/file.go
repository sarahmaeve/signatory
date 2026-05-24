package contentinjection

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// MaxScanFileBytes is the in-cap byte limit for ScanFile. A
// pathologically large agent-config file or README cannot stall the
// scan or consume unbounded memory. Findings reflect the in-cap
// prefix; ScanResult.Truncated is set when the file exceeded the cap.
//
// Tuned at 2 MiB. Legitimate agent-config files are tens of KiB at
// most in practice; legitimate README files are similar; PR
// descriptions are bounded by platform limits well below this. A
// malicious 100 MB carrier file would not realistically render in
// any editor or agent context, so the cap costs zero detection.
const MaxScanFileBytes = 2 * 1024 * 1024

// ErrEmptyPath indicates the caller passed an empty string to
// ScanFile. Returned in preference to a generic os.PathError so the
// caller can distinguish a configuration mistake from a missing-file
// case.
var ErrEmptyPath = errors.New("contentinjection: empty file path")

// ScanFile opens path, reads up to MaxScanFileBytes, runs Scan on
// the prefix, and returns the result with Truncated set if the file
// exceeded the cap. Equivalent to
// ScanFileWithOptions(path, ScanOptions{}).
func ScanFile(path string) (ScanResult, error) {
	return ScanFileWithOptions(path, ScanOptions{})
}

// ScanFileWithOptions reads the file and runs the primitives with
// caller-supplied options. Errors from the underlying file IO are
// wrapped with context; the ScanResult is the zero value on error.
func ScanFileWithOptions(path string, opts ScanOptions) (ScanResult, error) {
	if path == "" {
		return ScanResult{}, ErrEmptyPath
	}
	f, err := os.Open(path) //nolint:gosec // G304: ScanFile is the documented contract for caller-supplied paths
	if err != nil {
		return ScanResult{}, fmt.Errorf("contentinjection: open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	// Read MaxScanFileBytes+1 so we can distinguish "exactly at cap"
	// from "beyond cap" without a separate Stat call. The extra byte
	// is discarded.
	buf := make([]byte, MaxScanFileBytes+1)
	n, err := io.ReadFull(f, buf)
	switch {
	case err == nil:
		// Read the full buffer; file is at least MaxScanFileBytes+1
		// bytes — strictly beyond the cap.
		result := ScanWithOptions(buf[:MaxScanFileBytes], opts)
		result.Truncated = true
		return result, nil
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		// Read less than MaxScanFileBytes+1 — file fits inside the
		// cap.
		return ScanWithOptions(buf[:n], opts), nil
	default:
		return ScanResult{}, fmt.Errorf("contentinjection: read %q: %w", path, err)
	}
}
