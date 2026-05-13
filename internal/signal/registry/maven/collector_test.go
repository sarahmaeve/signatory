package maven

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// guavaMetadataXML is a realistic maven-metadata.xml modeled on
// com.google.guava:guava with 5 versions — enough to exercise
// version_count, last_publish, and version_publish_burst (no burst
// in this fixture because versions span months).
const guavaMetadataXML = `<?xml version="1.0" encoding="UTF-8"?>
<metadata>
  <groupId>com.google.guava</groupId>
  <artifactId>guava</artifactId>
  <versioning>
    <latest>33.2.1-jre</latest>
    <release>33.2.1-jre</release>
    <versions>
      <version>32.1.3-jre</version>
      <version>33.0.0-jre</version>
      <version>33.1.0-jre</version>
      <version>33.2.0-jre</version>
      <version>33.2.1-jre</version>
    </versions>
    <lastUpdated>20240617200000</lastUpdated>
  </versioning>
</metadata>`

// versionTimestamps maps version → Last-Modified for the test fixture.
// Spread across several months — no burst.
var versionTimestamps = map[string]time.Time{
	"33.2.1-jre": time.Date(2024, 6, 17, 20, 0, 0, 0, time.UTC),
	"33.2.0-jre": time.Date(2024, 6, 3, 20, 0, 0, 0, time.UTC),
	"33.1.0-jre": time.Date(2024, 5, 4, 20, 0, 0, 0, time.UTC),
	"33.0.0-jre": time.Date(2024, 1, 25, 0, 0, 0, 0, time.UTC),
	"32.1.3-jre": time.Date(2023, 10, 19, 20, 0, 0, 0, time.UTC),
}

// guavaTestServer returns an httptest.Server that serves:
//   - maven-metadata.xml at the expected path
//   - HEAD with Last-Modified for each version's jar
//   - HEAD 200 for .jar.asc (all versions signed)
//   - POM with <developers> for every version
func guavaTestServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// Metadata
		case r.URL.Path == "/maven2/com/google/guava/guava/maven-metadata.xml":
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(guavaMetadataXML)) //nolint:errcheck

		// HEAD on jars — return Last-Modified for timestamp resolution.
		case r.Method == http.MethodHead && contains(r.URL.Path, ".jar") && !contains(r.URL.Path, ".asc"):
			for v, ts := range versionTimestamps {
				if contains(r.URL.Path, v) {
					w.Header().Set("Last-Modified", ts.Format(http.TimeFormat))
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)

		// Signature check — all versions signed.
		case r.Method == http.MethodHead && contains(r.URL.Path, ".jar.asc"):
			w.WriteHeader(http.StatusOK)

		// POM with developers for every version.
		case contains(r.URL.Path, ".pom"):
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<project>
  <developers>
    <developer><name>Kevin Bourrillion</name></developer>
    <developer><name>Chris Povirk</name></developer>
  </developers>
</project>`)) //nolint:errcheck

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestCollector_Name(t *testing.T) {
	t.Parallel()
	c := NewCollector()
	assert.Equal(t, "maven-registry", c.Name())
}

func TestCollector_NonMavenEntity(t *testing.T) {
	t.Parallel()

	c := NewCollector()
	entity := &profile.Entity{
		ID:           "test-npm-entity",
		CanonicalURI: "pkg:npm/express",
		Ecosystem:    "npm",
	}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	assert.Equal(t, 0, result.SignalCount())
}

func TestCollector_NilEntity(t *testing.T) {
	t.Parallel()

	c := NewCollector()
	result, err := c.Collect(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 0, result.SignalCount())
}

func TestCollector_Success(t *testing.T) {
	t.Parallel()

	srv := guavaTestServer()
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:           "test-guava",
		CanonicalURI: "pkg:maven/com.google.guava/guava",
		Ecosystem:    "maven",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	// Should emit: last_publish, version_count, version_publish_burst,
	// gpg_signature_present, missing_artifact_count, signature_consistency,
	// maintainer_count, author_drift.
	assert.GreaterOrEqual(t, result.SignalCount(), 8,
		"expected at least 8 signals, got %d: %s", result.SignalCount(), result.Summary())

	// Verify specific signals by type.
	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	// last_publish
	assert.Contains(t, signalMap, "last_publish")
	var lp map[string]any
	require.NoError(t, json.Unmarshal(signalMap["last_publish"], &lp))
	assert.Equal(t, "33.2.1-jre", lp["latest_version"])
	assert.NotEmpty(t, lp["published_at"])

	// version_count
	assert.Contains(t, signalMap, "version_count")
	var vc map[string]any
	require.NoError(t, json.Unmarshal(signalMap["version_count"], &vc))
	assert.Equal(t, float64(5), vc["count"])

	// version_publish_burst
	assert.Contains(t, signalMap, "version_publish_burst")
	var vpb map[string]any
	require.NoError(t, json.Unmarshal(signalMap["version_publish_burst"], &vpb))
	assert.Equal(t, false, vpb["burst_detected"],
		"guava versions are spread over months — no burst")

	// gpg_signature_present
	assert.Contains(t, signalMap, "gpg_signature_present")
	var gpg map[string]any
	require.NoError(t, json.Unmarshal(signalMap["gpg_signature_present"], &gpg))
	assert.Equal(t, true, gpg["present"])
	assert.Equal(t, "33.2.1-jre", gpg["version_checked"])

	// missing_artifact_count — all jars are present in the test server.
	assert.Contains(t, signalMap, "missing_artifact_count")
	var mac map[string]any
	require.NoError(t, json.Unmarshal(signalMap["missing_artifact_count"], &mac))
	assert.Equal(t, float64(0), mac["count"])

	// signature_consistency — all versions signed.
	assert.Contains(t, signalMap, "signature_consistency")
	var sc map[string]any
	require.NoError(t, json.Unmarshal(signalMap["signature_consistency"], &sc))
	assert.Equal(t, true, sc["all_signed"])
	assert.Equal(t, float64(5), sc["signed_count"])

	// maintainer_count — from POM <developers>.
	assert.Contains(t, signalMap, "maintainer_count")
	var mc map[string]any
	require.NoError(t, json.Unmarshal(signalMap["maintainer_count"], &mc))
	assert.Equal(t, float64(2), mc["count"])

	// author_drift — same developers across all versions.
	assert.Contains(t, signalMap, "author_drift")
	var ad map[string]any
	require.NoError(t, json.Unmarshal(signalMap["author_drift"], &ad))
	assert.Equal(t, float64(1), ad["distinct_developer_sets"],
		"all versions have the same developers — no drift")
}

func TestCollector_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:           "test-missing",
		CanonicalURI: "pkg:maven/com.example/nonexistent",
		Ecosystem:    "maven",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	assert.True(t, result.HasFailures())
}

func TestCollector_SignatureAbsent(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/maven2/com/google/guava/guava/maven-metadata.xml":
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(guavaMetadataXML)) //nolint:errcheck

		// HEAD on jars — return timestamps.
		case r.Method == http.MethodHead && r.URL.Path == "/maven2/com/google/guava/guava/33.2.1-jre/guava-33.2.1-jre.jar":
			w.Header().Set("Last-Modified", versionTimestamps["33.2.1-jre"].Format(http.TimeFormat))
		case r.Method == http.MethodHead && r.URL.Path == "/maven2/com/google/guava/guava/33.2.0-jre/guava-33.2.0-jre.jar":
			w.Header().Set("Last-Modified", versionTimestamps["33.2.0-jre"].Format(http.TimeFormat))
		case r.Method == http.MethodHead && r.URL.Path == "/maven2/com/google/guava/guava/33.1.0-jre/guava-33.1.0-jre.jar":
			w.Header().Set("Last-Modified", versionTimestamps["33.1.0-jre"].Format(http.TimeFormat))
		case r.Method == http.MethodHead && r.URL.Path == "/maven2/com/google/guava/guava/33.0.0-jre/guava-33.0.0-jre.jar":
			w.Header().Set("Last-Modified", versionTimestamps["33.0.0-jre"].Format(http.TimeFormat))
		case r.Method == http.MethodHead && r.URL.Path == "/maven2/com/google/guava/guava/32.1.3-jre/guava-32.1.3-jre.jar":
			w.Header().Set("Last-Modified", versionTimestamps["32.1.3-jre"].Format(http.TimeFormat))

		// Signature — absent (404).
		case r.Method == http.MethodHead && r.URL.Path == "/maven2/com/google/guava/guava/33.2.1-jre/guava-33.2.1-jre.jar.asc":
			w.WriteHeader(http.StatusNotFound)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:           "test-guava-nosig",
		CanonicalURI: "pkg:maven/com.google.guava/guava",
		Ecosystem:    "maven",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	assert.Contains(t, signalMap, "gpg_signature_present")
	var gpg map[string]any
	require.NoError(t, json.Unmarshal(signalMap["gpg_signature_present"], &gpg))
	assert.Equal(t, false, gpg["present"])
}

func TestCollector_EntityStore_MintsOrgEntity(t *testing.T) {
	t.Parallel()

	srv := guavaTestServer()
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	store := &mockEntityStore{}
	c := NewCollectorWithClient(client).WithEntityStore(store)

	entity := &profile.Entity{
		ID:           "test-guava",
		CanonicalURI: "pkg:maven/com.google.guava/guava",
		Ecosystem:    "maven",
	}

	_, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	// Should mint org:maven/com.google.guava.
	assert.Contains(t, store.minted, "org:maven/com.google.guava")
}

func TestExtractMavenCoordinate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		entity    *profile.Entity
		wantGroup string
		wantArt   string
		wantOK    bool
	}{
		{
			name:      "valid maven URI",
			entity:    &profile.Entity{CanonicalURI: "pkg:maven/com.google.guava/guava"},
			wantGroup: "com.google.guava",
			wantArt:   "guava",
			wantOK:    true,
		},
		{
			name:      "npm entity",
			entity:    &profile.Entity{CanonicalURI: "pkg:npm/express"},
			wantGroup: "",
			wantArt:   "",
			wantOK:    false,
		},
		{
			name:      "nil entity",
			entity:    nil,
			wantGroup: "",
			wantArt:   "",
			wantOK:    false,
		},
		{
			name:      "maven prefix but no artifact",
			entity:    &profile.Entity{CanonicalURI: "pkg:maven/com.google.guava"},
			wantGroup: "",
			wantArt:   "",
			wantOK:    false,
		},
		{
			name:      "maven prefix but empty after trim",
			entity:    &profile.Entity{CanonicalURI: "pkg:maven/"},
			wantGroup: "",
			wantArt:   "",
			wantOK:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, a, ok := extractMavenCoordinate(tc.entity)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantGroup, g)
			assert.Equal(t, tc.wantArt, a)
		})
	}
}

// TestResolveRepoURL_ParentPOM verifies that when an artifact POM has
// no <scm> section but declares a <parent>, ResolveRepoURL follows the
// parent chain to find the SCM URL. This is the guava pattern: guava's
// POM inherits from guava-parent, which declares the GitHub SCM URL.
func TestResolveRepoURL_ParentPOM(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		// Artifact POM — no <scm>, but has a <parent>.
		case "/maven2/com/google/guava/guava/33.2.1-jre/guava-33.2.1-jre.pom":
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <parent>
    <groupId>com.google.guava</groupId>
    <artifactId>guava-parent</artifactId>
    <version>33.2.1-jre</version>
  </parent>
  <artifactId>guava</artifactId>
  <dependencies>
    <dependency><groupId>com.google.guava</groupId><artifactId>failureaccess</artifactId></dependency>
  </dependencies>
</project>`)) //nolint:errcheck

		// Parent POM — has the <scm> section.
		case "/maven2/com/google/guava/guava-parent/33.2.1-jre/guava-parent-33.2.1-jre.pom":
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>com.google.guava</groupId>
  <artifactId>guava-parent</artifactId>
  <version>33.2.1-jre</version>
  <scm>
    <url>https://github.com/google/guava</url>
    <connection>scm:git:https://github.com/google/guava.git</connection>
  </scm>
</project>`)) //nolint:errcheck

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	url, err := client.ResolveRepoURL(context.Background(),
		"com.google.guava", "guava", "33.2.1-jre")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/google/guava", url,
		"should resolve SCM URL from parent POM")
}

// TestResolveRepoURL_ParentPOM_MaxDepth verifies that parent chain
// resolution stops at the depth limit to prevent infinite loops from
// circular parent references.
func TestResolveRepoURL_ParentPOM_MaxDepth(t *testing.T) {
	t.Parallel()

	// Every POM points to a parent but never declares <scm>.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<project>
  <parent>
    <groupId>com.example</groupId>
    <artifactId>infinite-parent</artifactId>
    <version>1.0.0</version>
  </parent>
  <artifactId>child</artifactId>
</project>`)) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	url, err := client.ResolveRepoURL(context.Background(),
		"com.example", "child", "1.0.0")
	require.NoError(t, err)
	assert.Empty(t, url, "should return empty after hitting parent depth limit")
}

// TestResolveRepoURL_DirectSCM verifies that when the artifact POM
// has its own <scm> section, no parent chasing is needed. The
// expected URL is the case-folded canonical form produced by
// CloneURLForRepoPlatform — the maven client matches the cross-
// ecosystem contract (every other ecosystem's NormalizeDeclaredRepoURL
// also lowercases via ResolveTarget), so the FasterXML casing in the
// POM resolves to the canonical fasterxml form here.
func TestResolveRepoURL_DirectSCM(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<project>
  <scm>
    <url>https://github.com/FasterXML/jackson-databind</url>
  </scm>
</project>`)) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	url, err := client.ResolveRepoURL(context.Background(),
		"com.fasterxml.jackson.core", "jackson-databind", "2.18.0")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/fasterxml/jackson-databind", url)
}

// --- Tests for new longitudinal/governance signals ---

// TestCollector_MaintainerCount verifies maintainer_count is emitted
// from the POM's <developers> section.
func TestCollector_MaintainerCount(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/maven2/com/google/guava/guava/maven-metadata.xml":
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(guavaMetadataXML)) //nolint:errcheck

		// HEAD on jars — timestamps.
		case r.Method == http.MethodHead && contains(r.URL.Path, ".jar") && !contains(r.URL.Path, ".asc"):
			for v, ts := range versionTimestamps {
				if contains(r.URL.Path, v) {
					w.Header().Set("Last-Modified", ts.Format(http.TimeFormat))
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)

		// Signature — present.
		case r.Method == http.MethodHead && contains(r.URL.Path, ".jar.asc"):
			w.WriteHeader(http.StatusOK)

		// POM with <developers> for latest version.
		case r.URL.Path == "/maven2/com/google/guava/guava/33.2.1-jre/guava-33.2.1-jre.pom":
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<project>
  <developers>
    <developer>
      <name>Kevin Bourrillion</name>
      <email>kevinb@google.com</email>
    </developer>
    <developer>
      <name>Chris Povirk</name>
      <email>cpovirk@google.com</email>
    </developer>
    <developer>
      <name>Kurt Kluever</name>
    </developer>
  </developers>
</project>`)) //nolint:errcheck

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:           "test-guava-mc",
		CanonicalURI: "pkg:maven/com.google.guava/guava",
		Ecosystem:    "maven",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	require.Contains(t, signalMap, "maintainer_count")
	var mc map[string]any
	require.NoError(t, json.Unmarshal(signalMap["maintainer_count"], &mc))
	assert.Equal(t, float64(3), mc["count"])
	names := mc["names"].([]any)
	assert.Contains(t, names, "Kevin Bourrillion")
	assert.Contains(t, names, "Chris Povirk")
	assert.Contains(t, names, "Kurt Kluever")
}

// TestCollector_MissingArtifactCount verifies that versions listed in
// maven-metadata.xml but returning 404 on HEAD are counted as "missing"
// (Maven's analog of yanked releases).
func TestCollector_MissingArtifactCount(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/maven2/com/google/guava/guava/maven-metadata.xml":
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(guavaMetadataXML)) //nolint:errcheck

		// All .asc present.
		case r.Method == http.MethodHead && contains(r.URL.Path, ".jar.asc"):
			w.WriteHeader(http.StatusOK)

		// HEAD on jars — 2 versions are missing (404), 3 have timestamps.
		case r.Method == http.MethodHead && contains(r.URL.Path, "33.2.1-jre"):
			w.Header().Set("Last-Modified", versionTimestamps["33.2.1-jre"].Format(http.TimeFormat))
		case r.Method == http.MethodHead && contains(r.URL.Path, "33.2.0-jre"):
			w.Header().Set("Last-Modified", versionTimestamps["33.2.0-jre"].Format(http.TimeFormat))
		case r.Method == http.MethodHead && contains(r.URL.Path, "33.1.0-jre"):
			w.Header().Set("Last-Modified", versionTimestamps["33.1.0-jre"].Format(http.TimeFormat))
		case r.Method == http.MethodHead && contains(r.URL.Path, "33.0.0-jre"):
			// Missing — 404
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodHead && contains(r.URL.Path, "32.1.3-jre"):
			// Missing — 404
			w.WriteHeader(http.StatusNotFound)

		// POM for maintainer_count.
		case contains(r.URL.Path, ".pom"):
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<project><developers><developer><name>Dev</name></developer></developers></project>`)) //nolint:errcheck

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:           "test-missing-art",
		CanonicalURI: "pkg:maven/com.google.guava/guava",
		Ecosystem:    "maven",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	require.Contains(t, signalMap, "missing_artifact_count")
	var mac map[string]any
	require.NoError(t, json.Unmarshal(signalMap["missing_artifact_count"], &mac))
	assert.Equal(t, float64(2), mac["count"])
	assert.Equal(t, float64(5), mac["versions_checked"])
}

// TestCollector_SignatureConsistency verifies that the collector checks
// .asc presence across the version window, not just the latest version.
func TestCollector_SignatureConsistency(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/maven2/com/google/guava/guava/maven-metadata.xml":
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(guavaMetadataXML)) //nolint:errcheck

		// HEAD on jars — all return timestamps.
		case r.Method == http.MethodHead && contains(r.URL.Path, ".jar") && !contains(r.URL.Path, ".asc"):
			for v, ts := range versionTimestamps {
				if contains(r.URL.Path, v) {
					w.Header().Set("Last-Modified", ts.Format(http.TimeFormat))
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)

		// Signatures: latest 3 versions signed, older 2 not signed.
		case r.Method == http.MethodHead && contains(r.URL.Path, ".jar.asc"):
			if contains(r.URL.Path, "33.2.1-jre") ||
				contains(r.URL.Path, "33.2.0-jre") ||
				contains(r.URL.Path, "33.1.0-jre") {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}

		// POM for maintainer_count.
		case contains(r.URL.Path, ".pom"):
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<project><developers><developer><name>Dev</name></developer></developers></project>`)) //nolint:errcheck

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:           "test-sig-consistency",
		CanonicalURI: "pkg:maven/com.google.guava/guava",
		Ecosystem:    "maven",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	require.Contains(t, signalMap, "signature_consistency")
	var sc map[string]any
	require.NoError(t, json.Unmarshal(signalMap["signature_consistency"], &sc))
	assert.Equal(t, float64(3), sc["signed_count"])
	assert.Equal(t, float64(2), sc["unsigned_count"])
	assert.Equal(t, float64(5), sc["versions_checked"])
	assert.Equal(t, false, sc["all_signed"],
		"2 of 5 versions lack .asc — not all signed")
}

// TestCollector_AuthorDrift verifies that the collector detects changes
// in the POM <developers> section across the version window.
func TestCollector_AuthorDrift(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/maven2/com/google/guava/guava/maven-metadata.xml":
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(guavaMetadataXML)) //nolint:errcheck

		// HEAD on jars — all return timestamps.
		case r.Method == http.MethodHead && contains(r.URL.Path, ".jar") && !contains(r.URL.Path, ".asc"):
			for v, ts := range versionTimestamps {
				if contains(r.URL.Path, v) {
					w.Header().Set("Last-Modified", ts.Format(http.TimeFormat))
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)

		// All .asc present.
		case r.Method == http.MethodHead && contains(r.URL.Path, ".jar.asc"):
			w.WriteHeader(http.StatusOK)

		// POMs with different developers per version.
		case contains(r.URL.Path, "33.2.1-jre") && contains(r.URL.Path, ".pom"):
			w.Write([]byte(`<project><developers>
				<developer><name>Alice</name></developer>
				<developer><name>Bob</name></developer>
			</developers></project>`)) //nolint:errcheck
		case contains(r.URL.Path, "33.2.0-jre") && contains(r.URL.Path, ".pom"):
			w.Write([]byte(`<project><developers>
				<developer><name>Alice</name></developer>
				<developer><name>Bob</name></developer>
			</developers></project>`)) //nolint:errcheck
		case contains(r.URL.Path, "33.1.0-jre") && contains(r.URL.Path, ".pom"):
			w.Write([]byte(`<project><developers>
				<developer><name>Alice</name></developer>
			</developers></project>`)) //nolint:errcheck
		case contains(r.URL.Path, "33.0.0-jre") && contains(r.URL.Path, ".pom"):
			w.Write([]byte(`<project><developers>
				<developer><name>Charlie</name></developer>
			</developers></project>`)) //nolint:errcheck
		case contains(r.URL.Path, "32.1.3-jre") && contains(r.URL.Path, ".pom"):
			w.Write([]byte(`<project><developers>
				<developer><name>Charlie</name></developer>
			</developers></project>`)) //nolint:errcheck

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:           "test-author-drift",
		CanonicalURI: "pkg:maven/com.google.guava/guava",
		Ecosystem:    "maven",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	require.Contains(t, signalMap, "author_drift")
	var ad map[string]any
	require.NoError(t, json.Unmarshal(signalMap["author_drift"], &ad))
	// 3 distinct developer sets: "Alice, Bob", "Alice", "Charlie"
	assert.Equal(t, float64(3), ad["distinct_developer_sets"])
	assert.Equal(t, float64(5), ad["versions_checked"])
}

// TestParseDevelopers verifies the POM <developers> parser.
func TestParseDevelopers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pom  string
		want []string
	}{
		{
			name: "three developers",
			pom: `<project><developers>
				<developer><name>Alice</name></developer>
				<developer><name>Bob</name></developer>
				<developer><name>Charlie</name></developer>
			</developers></project>`,
			want: []string{"Alice", "Bob", "Charlie"},
		},
		{
			name: "no developers section",
			pom:  `<project><groupId>com.example</groupId></project>`,
			want: nil,
		},
		{
			name: "empty developers",
			pom:  `<project><developers></developers></project>`,
			want: nil,
		},
		{
			name: "developer with id but no name",
			pom: `<project><developers>
				<developer><id>alice</id></developer>
			</developers></project>`,
			want: []string{"alice"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseDevelopers([]byte(tc.pom))
			assert.Equal(t, tc.want, got)
		})
	}
}

// contains is a test helper for URL path matching.
func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

// mockEntityStore tracks which entity URIs were minted.
type mockEntityStore struct {
	minted []string
}

func (m *mockEntityStore) EnsureEntityByCanonicalURI(_ context.Context, uri, shortName string) (*profile.Entity, bool, error) {
	m.minted = append(m.minted, uri)
	return &profile.Entity{ID: "mock-" + shortName, CanonicalURI: uri}, true, nil
}
