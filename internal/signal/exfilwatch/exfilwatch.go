// Package exfilwatch scans a source tree for literal references to
// HTTP-capture-as-a-service hosts that have no legitimate purpose
// in published library code.
//
// A literal hit is a strong supply-chain malware signal: the
// BufferZoneCorp campaign (May 2026 — see
// design/threat-landscape/2026-05-02-bufferzonecorp-campaign.md)
// exfiltrated to webhook.site/<UUID> from package init() across all
// 16 packages. Substring match. Obfuscated literals (XOR, base64,
// runtime concatenation) defeat this scan by design — separate
// patterns catch the obfuscation itself.
package exfilwatch

import (
	"bufio"
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Hosts whose primary operational property is exposing a no-account
// public HTTP capture endpoint. A literal of any of these in
// published package source is structurally suspicious — there is no
// scenario in which a library's correct behavior is to POST to a
// publicly-browseable third-party request collector.
//
// The discord.com/api/webhooks entries are a deliberate extension of
// that rule to a dual-use platform. Discord itself is legitimate, but
// the webhook delivery path carries an embedded channel id + secret
// token in the URL; a hardcoded one in published library source is an
// exfil sink, not configuration (real Discord-notification libraries
// take the webhook URL as a runtime parameter, and Discord API clients
// use the /api/v10/ path, not /api/webhooks/). It is the canonical
// gaming/cookie-stealer channel — e.g. the spadata PyPI stealer
// (June 2026) POSTed a decrypted .ROBLOSECURITY cookie there. The
// path-qualified entry keeps the precision: bare discord.com does not
// match. discordapp.com is Discord's legacy domain and needs its own
// entry — it does not contain the "discord.com" substring.
//
// Telegram (api.telegram.org/bot...) is deliberately NOT listed: every
// legitimate Telegram bot library hardcodes that exact base URL, so a
// literal scan would false-positive on benign code and break the
// "a hit is a strong signal" contract. Telegram exfil is genuinely
// dual-use and needs a more discriminating mechanism than a host scan.
var Hosts = []string{
	"webhook.site",
	"requestbin.com",
	"beeceptor.com",
	"pipedream.com/v1/sources",
	"requestcatcher.com",
	"interact.sh",
	"oast.live",
	"oast.fun",
	"oast.online",
	"oast.pro",
	"oast.site",
	"postb.in",
	"smee.io",
	"ngrok-free.app",
	"localhost.run",
	"serveo.net",
	"discord.com/api/webhooks",
	"discordapp.com/api/webhooks",
}

// Hit is one literal occurrence of a Hosts entry on a single line.
// Same line containing two distinct hosts produces two Hits; same
// host appearing twice on one line produces one Hit.
type Hit struct {
	File string `json:"file"` // path relative to the scan root
	Line int    `json:"line"` // 1-indexed
	Host string `json:"host"` // matched entry from Hosts
}

// SkipReason explains why Scan did not read a file in the walked tree.
// Mirrors internal/prdefense's SkipReason strings so the two source-
// scanning surfaces report skips the same way.
type SkipReason string

const (
	// SkipTooLarge: the file exceeded maxScanFileBytes. A host literal in
	// published library source lives in human-written code, which is never
	// this large.
	SkipTooLarge SkipReason = "oversized"
	// SkipIrregular: not a regular file (FIFO/device/socket, or a symlink
	// resolving to one), which could stream without bound if read.
	SkipIrregular SkipReason = "irregular"
)

// SkippedFile records a file Scan did not read, with the reason.
// Surfacing these keeps the size/irregular cap from ever being silent —
// the same contract the artifact walker's Manifest.SkippedScans upholds,
// so an oversize file can never quietly hide a sink past the cap.
type SkippedFile struct {
	File   string     `json:"file"` // path relative to the scan root
	Reason SkipReason `json:"reason"`
}

// Result is the exfil_capture_host signal payload: the host-literal hits
// plus the files Scan skipped without reading. Carrying the skips means a
// sink hidden inside a file too large to scan surfaces as a recorded gap
// rather than silent absence — the analyst sees what was not examined.
type Result struct {
	Hits    []Hit         `json:"hits"`
	Skipped []SkippedFile `json:"skipped"`
}

// maxScanFileBytes bounds the bytes scanFile reads from any single file
// in the walked tree. The streaming entry points (ScanReader/ScanBytes)
// delegate bounding to their callers — the artifact walker caps entries
// at 2 MiB (exfilScanMaxFileBytes), the PR path caps blobs at 2 MiB — but
// the filesystem walk is itself a caller, so it must enforce the same
// ceiling. Without it, one huge file with no newline would be buffered
// whole by scanReader's line read (ReadString) and exhaust memory. A host
// literal in published library source lives in human-written code, which
// is never this large; an oversize file is skipped rather than scanned,
// matching the artifact path's over-MaxSize behavior.
const maxScanFileBytes = 2 << 20

// Scan walks root and returns every literal Hosts match, plus the files
// it skipped without reading (not a regular file, or over
// maxScanFileBytes). Skips are recorded rather than silent so an oversize
// file can never quietly hide a sink past the cap.
//
// Returns the first error from filepath.WalkDir; a file that cannot be
// opened or stat'd is skipped rather than aborting the walk.
func Scan(root string) (hits []Hit, skipped []SkippedFile, err error) {
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		fileHits, skip, _ := scanFile(root, path)
		hits = append(hits, fileHits...)
		if skip != nil {
			skipped = append(skipped, *skip)
		}
		return nil
	})
	return hits, skipped, err
}

func scanFile(root, path string) ([]Hit, *SkippedFile, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	f, err := os.Open(path) //nolint:gosec // G304: scanning a caller-specified source tree is the entire purpose
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()

	// Bound the read so the filesystem walk honors the same content cap
	// the streaming callers do. Stat the open fd (no TOCTOU gap with the
	// open) and skip — recording the reason — anything that is not a
	// regular file (a FIFO/device, or a symlink resolving to one, would
	// stream without bound) or that exceeds the size cap.
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, &SkippedFile{File: rel, Reason: SkipIrregular}, nil
	}
	if fi.Size() > maxScanFileBytes {
		return nil, &SkippedFile{File: rel, Reason: SkipTooLarge}, nil
	}
	return scanReader(rel, f), nil, nil
}

// ScanBytes scans in-memory content (e.g. a blob read from a git object
// database for a PR changelist) for literal Hosts matches, attributing
// hits to rel. Mirrors scanFile without a filesystem read — there is no
// error path, since a bytes reader cannot fail.
func ScanBytes(rel string, content []byte) []Hit {
	return scanReader(rel, bytes.NewReader(content))
}

// ScanReader scans an arbitrary reader for literal Hosts matches,
// attributing hits to rel. It is the streaming companion to ScanBytes:
// the artifact walker hands it a bounded archive-entry body so a
// published tarball can be scanned for exfil sinks without the caller
// buffering each file into a []byte first. Same line-scan semantics as
// ScanBytes (case-insensitive, 1-indexed lines, over-long lines never
// halt the scan); the caller is responsible for bounding the reader.
func ScanReader(rel string, r io.Reader) []Hit {
	return scanReader(rel, r)
}

// scanReader is the shared line-scan core. It reports every literal Hosts
// match, case-insensitively (DNS is case-insensitive, so a lowercase-only
// match would be a free bypass), attributing each to its 1-indexed line.
//
// Lines are read whole via bufio.Reader rather than bufio.Scanner: a
// minified/obfuscated line longer than Scanner's token cap made Scan()
// return false and silently halt the rest of the file, letting a host
// literal placed after — or inside — an over-long line slip past this
// pre-merge gate. We never stop scanning on line length. Callers bound
// total content (the PR path caps blobs at 2 MiB), so an unbounded line
// cannot be forced here in practice.
func scanReader(rel string, r io.Reader) []Hit {
	var hits []Hit
	br := bufio.NewReader(r)
	for line := 1; ; line++ {
		text, err := br.ReadString('\n')
		if len(text) > 0 {
			lower := strings.ToLower(text)
			for _, h := range Hosts {
				if strings.Contains(lower, h) {
					hits = append(hits, Hit{File: rel, Line: line, Host: h})
				}
			}
		}
		if err != nil {
			return hits
		}
	}
}
