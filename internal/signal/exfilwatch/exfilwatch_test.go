package exfilwatch_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sarahmaeve/signatory/internal/signal/exfilwatch"
)

func TestScan_NoHitsOnCleanTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, err := exfilwatch.Scan(dir)
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
	hits, err := exfilwatch.Scan(dir)
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

func TestScan_SubdomainCounts(t *testing.T) {
	dir := t.TempDir()
	content := "var u = \"https://abc-def-1234.webhook.site/x\"\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, err := exfilwatch.Scan(dir)
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
	hits, err := exfilwatch.Scan(dir)
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
	hits, err := exfilwatch.Scan(dir)
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
	hits, err := exfilwatch.Scan(dir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d: %+v", len(hits), hits)
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
