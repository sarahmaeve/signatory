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
}

// Hit is one literal occurrence of a Hosts entry on a single line.
// Same line containing two distinct hosts produces two Hits; same
// host appearing twice on one line produces one Hit.
type Hit struct {
	File string `json:"file"` // path relative to the scan root
	Line int    `json:"line"` // 1-indexed
	Host string `json:"host"` // matched entry from Hosts
}

// Scan walks root and returns every literal Hosts match.
//
// Returns the first non-skip error from filepath.WalkDir; a file that
// cannot be opened is skipped rather than aborting the walk.
func Scan(root string) ([]Hit, error) {
	var hits []Hit
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		fileHits, _ := scanFile(root, path)
		hits = append(hits, fileHits...)
		return nil
	})
	return hits, err
}

func scanFile(root, path string) ([]Hit, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	f, err := os.Open(path) //nolint:gosec // G304: scanning a caller-specified source tree is the entire purpose
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return scanReader(rel, f), nil
}

// ScanBytes scans in-memory content (e.g. a blob read from a git object
// database for a PR changelist) for literal Hosts matches, attributing
// hits to rel. Mirrors scanFile without a filesystem read — there is no
// error path, since a bytes reader cannot fail.
func ScanBytes(rel string, content []byte) []Hit {
	return scanReader(rel, bytes.NewReader(content))
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
