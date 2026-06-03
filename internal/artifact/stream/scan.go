package stream

import (
	"bytes"
	"fmt"
)

// matchingScanners returns the scanners whose Match accepts e. A
// scanner with a nil Match never matches (a misconfigured scanner is
// inert, not a panic).
func matchingScanners(scanners []Scanner, e Entry) []Scanner {
	var out []Scanner
	for _, s := range scanners {
		if s.Match != nil && s.Match(e) {
			out = append(out, s)
		}
	}
	return out
}

// anyScannerAccepts reports whether at least one scanner's MaxSize
// admits an entry of the given size. When none do, the walker can skip
// the body unread rather than buffer attacker-controlled bytes it would
// only discard.
func anyScannerAccepts(scanners []Scanner, size int64) bool {
	for _, s := range scanners {
		if size <= s.MaxSize {
			return true
		}
	}
	return false
}

// runScanners feeds body to every scanner that admits its size and
// records an oversize skip for the rest. body is the entry's full bytes
// (a capture buffer or a transient scan buffer); each scanner reads its
// own bytes.Reader, so fan-out across multiple scanners is independent.
// A scanner error aborts the walk.
func runScanners(m *Manifest, scanners []Scanner, path string, size int64, body []byte) error {
	for _, s := range scanners {
		if size > s.MaxSize {
			m.SkippedScans[path] = scanSkipReason(s, size)
			continue
		}
		if s.Scan == nil {
			continue
		}
		if err := s.Scan(path, bytes.NewReader(body)); err != nil {
			return fmt.Errorf("stream: scanner %q on %q: %w", s.Name, path, err)
		}
	}
	return nil
}

// recordScanSkips records an oversize skip for every scanner whose
// MaxSize is below size. Used on the body-skipped path so a matching
// scanner that was too small to run still leaves a trace.
func recordScanSkips(m *Manifest, scanners []Scanner, path string, size int64) {
	for _, s := range scanners {
		if size > s.MaxSize {
			m.SkippedScans[path] = scanSkipReason(s, size)
		}
	}
}

func scanSkipReason(s Scanner, size int64) string {
	return fmt.Sprintf("%s: oversize entry %d bytes > scan cap %d", s.Name, size, s.MaxSize)
}
