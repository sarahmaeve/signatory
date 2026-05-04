//go:build network_access_ok

// These tests hit the real Maven Central repo1.maven.org endpoint.
// They are NOT run by default.
//
// To run:   go test -tags network_access_ok ./internal/signal/registry/maven/ -v
//
// Requirements:
//   - Network access to repo1.maven.org
//
// These tests validate the full client + collector path against live
// Maven Central data using four packages of varying size and shape.
// They should be run manually before releases, not in CI.

package maven

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// --- Client-level tests: verify our HTTP plumbing against real responses ---

func TestLive_FetchMetadata_Dropwizard(t *testing.T) {
	client := NewClient()
	meta, err := client.FetchMetadata(context.Background(), "io.dropwizard", "dropwizard-core")
	require.NoError(t, err, "FetchMetadata must succeed against live repo1")

	assert.NotEmpty(t, meta.Versioning.Versions, "versions list must be populated")
	assert.NotEmpty(t, meta.Versioning.Release, "release version must be set")
	assert.Greater(t, len(meta.Versioning.Versions), 50,
		"dropwizard-core has 200+ versions; got %d", len(meta.Versioning.Versions))

	t.Logf("dropwizard-core: %d versions, latest=%s, release=%s",
		len(meta.Versioning.Versions), meta.Versioning.Latest, meta.Versioning.Release)
}

func TestLive_HeadTimestamp_Dropwizard(t *testing.T) {
	client := NewClient()

	// Get the latest version from metadata first.
	meta, err := client.FetchMetadata(context.Background(), "io.dropwizard", "dropwizard-core")
	require.NoError(t, err)
	version := meta.Versioning.Release
	require.NotEmpty(t, version)

	ts, err := client.HeadTimestamp(context.Background(), "io.dropwizard", "dropwizard-core", version)
	require.NoError(t, err, "HeadTimestamp must succeed for a real artifact")
	assert.False(t, ts.IsZero(), "timestamp must be non-zero")

	t.Logf("dropwizard-core %s: published at %s", version, ts)
}

func TestLive_CheckSignature_Guava(t *testing.T) {
	client := NewClient()

	meta, err := client.FetchMetadata(context.Background(), "com.google.guava", "guava")
	require.NoError(t, err)
	version := meta.Versioning.Release
	require.NotEmpty(t, version)

	present, err := client.CheckSignature(context.Background(), "com.google.guava", "guava", version)
	require.NoError(t, err, "CheckSignature must not error on a real artifact")
	assert.True(t, present, "guava %s should have a GPG signature (.jar.asc)", version)

	t.Logf("guava %s: signature present = %v", version, present)
}

func TestLive_ResolveRepoURL_CommonsLang(t *testing.T) {
	client := NewClient()

	meta, err := client.FetchMetadata(context.Background(), "org.apache.commons", "commons-lang3")
	require.NoError(t, err)
	version := meta.Versioning.Release
	require.NotEmpty(t, version)

	repoURL, err := client.ResolveRepoURL(context.Background(),
		"org.apache.commons", "commons-lang3", version)
	require.NoError(t, err, "ResolveRepoURL must not error")
	assert.NotEmpty(t, repoURL, "commons-lang3 POM should declare an SCM URL")

	t.Logf("commons-lang3 %s: SCM URL = %s", version, repoURL)
}

func TestLive_ResolveRepoURL_JacksonDatabind(t *testing.T) {
	client := NewClient()

	meta, err := client.FetchMetadata(context.Background(), "com.fasterxml.jackson.core", "jackson-databind")
	require.NoError(t, err)
	version := meta.Versioning.Release
	require.NotEmpty(t, version)

	repoURL, err := client.ResolveRepoURL(context.Background(),
		"com.fasterxml.jackson.core", "jackson-databind", version)
	require.NoError(t, err, "ResolveRepoURL must not error")
	assert.NotEmpty(t, repoURL, "jackson-databind POM should declare an SCM URL")

	t.Logf("jackson-databind %s: SCM URL = %s", version, repoURL)
}

func TestLive_ResolveRepoURL_Guava_ParentChain(t *testing.T) {
	client := NewClient()

	meta, err := client.FetchMetadata(context.Background(), "com.google.guava", "guava")
	require.NoError(t, err)
	version := meta.Versioning.Release
	require.NotEmpty(t, version)

	repoURL, err := client.ResolveRepoURL(context.Background(),
		"com.google.guava", "guava", version)
	require.NoError(t, err, "ResolveRepoURL must not error")
	assert.NotEmpty(t, repoURL,
		"guava inherits SCM from parent POM — parent chain must resolve")
	assert.Contains(t, repoURL, "github.com/google/guava",
		"guava SCM should point to github.com/google/guava")

	t.Logf("guava %s: SCM URL = %s (resolved via parent POM chain)", version, repoURL)
}

// --- Collector-level tests: verify end-to-end signal emission ---

func TestLive_Collector_Dropwizard(t *testing.T) {
	c := NewCollector()
	entity := &profile.Entity{
		ID:           "live-dropwizard",
		CanonicalURI: "pkg:maven/io.dropwizard/dropwizard-core",
		Ecosystem:    "maven",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err, "Collect must not return an error for a real Maven artifact")

	// Should emit at least: last_publish, version_count,
	// version_publish_burst, gpg_signature_present, missing_artifact_count,
	// signature_consistency. (maintainer_count + author_drift depend on POM.)
	assert.GreaterOrEqual(t, result.SignalCount(), 6,
		"expected at least 6 signals from live Maven Central, got %d", result.SignalCount())

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	// last_publish: must exist, must have a version string.
	require.Contains(t, signalMap, "last_publish")
	var lp map[string]any
	require.NoError(t, json.Unmarshal(signalMap["last_publish"], &lp))
	assert.NotEmpty(t, lp["latest_version"], "latest_version must be set")
	assert.NotEmpty(t, lp["published_at"], "published_at must be set")
	daysAgo, ok := lp["days_ago"].(float64)
	assert.True(t, ok, "days_ago must be a number")
	assert.GreaterOrEqual(t, daysAgo, float64(0), "days_ago must be non-negative")
	t.Logf("last_publish: version=%s, published_at=%s, days_ago=%.0f",
		lp["latest_version"], lp["published_at"], daysAgo)

	// version_count: dropwizard-core has 200+ versions.
	require.Contains(t, signalMap, "version_count")
	var vc map[string]any
	require.NoError(t, json.Unmarshal(signalMap["version_count"], &vc))
	count, ok := vc["count"].(float64)
	assert.True(t, ok)
	assert.Greater(t, count, float64(50),
		"dropwizard-core has 200+ versions on Maven Central; got %.0f", count)
	t.Logf("version_count: %.0f", count)

	// version_publish_burst: must exist.
	require.Contains(t, signalMap, "version_publish_burst")
	var vpb map[string]any
	require.NoError(t, json.Unmarshal(signalMap["version_publish_burst"], &vpb))
	t.Logf("version_publish_burst: burst_detected=%v, versions_in_window=%v, window_hours=%v",
		vpb["burst_detected"], vpb["versions_in_window"], vpb["window_hours"])

	// gpg_signature_present: Maven Central requires GPG signing.
	require.Contains(t, signalMap, "gpg_signature_present")
	var gpg map[string]any
	require.NoError(t, json.Unmarshal(signalMap["gpg_signature_present"], &gpg))
	assert.Equal(t, true, gpg["present"],
		"Maven Central requires GPG signing; dropwizard should be signed")
	t.Logf("gpg_signature_present: present=%v, version_checked=%v",
		gpg["present"], gpg["version_checked"])

	// missing_artifact_count: well-maintained artifact should have 0 missing.
	require.Contains(t, signalMap, "missing_artifact_count")
	var mac map[string]any
	require.NoError(t, json.Unmarshal(signalMap["missing_artifact_count"], &mac))
	t.Logf("missing_artifact_count: count=%v, versions_checked=%v",
		mac["count"], mac["versions_checked"])

	// signature_consistency: Maven Central requires signing — should be all signed.
	require.Contains(t, signalMap, "signature_consistency")
	var sc map[string]any
	require.NoError(t, json.Unmarshal(signalMap["signature_consistency"], &sc))
	assert.Equal(t, true, sc["all_signed"],
		"Maven Central requires GPG signing; all versions should be signed")
	t.Logf("signature_consistency: all_signed=%v, signed=%v, unsigned=%v",
		sc["all_signed"], sc["signed_count"], sc["unsigned_count"])
}

func TestLive_Collector_Guava(t *testing.T) {
	c := NewCollector()
	entity := &profile.Entity{
		ID:           "live-guava",
		CanonicalURI: "pkg:maven/com.google.guava/guava",
		Ecosystem:    "maven",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, result.SignalCount(), 6)

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	var vc map[string]any
	require.NoError(t, json.Unmarshal(signalMap["version_count"], &vc))
	count := vc["count"].(float64)
	assert.Greater(t, count, float64(100),
		"guava has hundreds of versions; got %.0f", count)
	t.Logf("guava: %d signals, %.0f versions", result.SignalCount(), count)
}

func TestLive_Collector_CommonsLang3(t *testing.T) {
	c := NewCollector()
	entity := &profile.Entity{
		ID:           "live-commons-lang3",
		CanonicalURI: "pkg:maven/org.apache.commons/commons-lang3",
		Ecosystem:    "maven",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, result.SignalCount(), 6)

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	var gpg map[string]any
	require.NoError(t, json.Unmarshal(signalMap["gpg_signature_present"], &gpg))
	assert.Equal(t, true, gpg["present"],
		"commons-lang3 is an Apache project; should be GPG-signed")
	t.Logf("commons-lang3: %d signals, signature present=%v",
		result.SignalCount(), gpg["present"])
}

func TestLive_Collector_JacksonDatabind(t *testing.T) {
	c := NewCollector()
	entity := &profile.Entity{
		ID:           "live-jackson-databind",
		CanonicalURI: "pkg:maven/com.fasterxml.jackson.core/jackson-databind",
		Ecosystem:    "maven",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, result.SignalCount(), 6)
	t.Logf("jackson-databind: %d signals", result.SignalCount())
}
