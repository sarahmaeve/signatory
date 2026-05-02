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
// Returns the first non-skip error from filepath.WalkDir; per-file
// scan errors (e.g. bufio.ErrTooLong on a binary) do not abort the
// walk — partial reads still produce useful hits.
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
	defer f.Close()

	var hits []Hit
	sc := bufio.NewScanner(f)
	// Tolerate long lines from minified/vendored sources. 1 MiB cap.
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for line := 1; sc.Scan(); line++ {
		text := sc.Text()
		for _, h := range Hosts {
			if strings.Contains(text, h) {
				hits = append(hits, Hit{File: rel, Line: line, Host: h})
			}
		}
	}
	// sc.Err() ignored: a partial scan still produced useful hits.
	return hits, nil
}
