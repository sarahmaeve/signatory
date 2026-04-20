package npm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// newTestCollector wires a Collector to an httptest-backed client
// pointed at srv. All collector tests use this pattern so the
// server's response shape is the single axis of variation.
func newTestCollector(srv *httptest.Server) *Collector {
	return newCollectorWithClient(newClientWithBaseURL(srv.URL))
}

// npmEntity returns a minimal npm entity for a given package name.
// Entities in production come out of analyze.go's scheme-branched
// creation (Phase A.1); the tests build them directly.
func npmEntity(name string) *profile.Entity {
	return &profile.Entity{
		ID:           "e-" + name,
		CanonicalURI: "pkg:npm/" + name,
		Type:         profile.EntityPackage,
		Ecosystem:    "npm",
		ShortName:    name,
	}
}

// ----- happy path -----

func TestCollector_Collect_HappyPath_EmitsLastPublish(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, sampleRegistryResponse)
	}))
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("express"))
	require.NoError(t, err)
	require.NotNil(t, result)

	// One signal recorded, zero absences.
	assert.Equal(t, 1, result.SignalCount(), "one last_publish signal expected")
	assert.Equal(t, 0, result.AbsenceCount(), "happy path should not record absences")
	require.Len(t, result.Signals(), 1)

	sig := result.Signals()[0]
	assert.Equal(t, "last_publish", sig.Type)
	assert.Equal(t, source, sig.Source)
	assert.Equal(t, profile.SignalGroupVitality, sig.Group)
	assert.Equal(t, "e-express", sig.EntityID)

	// Value payload should carry the version + publish timestamp.
	var value map[string]any
	require.NoError(t, json.Unmarshal(sig.Value, &value))
	assert.Equal(t, "4.18.2", value["latest_version"])
	assert.Equal(t, "2022-10-08T19:08:35Z", value["published_at"])
	assert.NotNil(t, value["days_ago"],
		"days_ago should be computed and present for humans to eyeball")
}

// ----- non-npm entity → empty result -----

func TestCollector_Collect_NonNpmEntity_ReturnsEmpty(t *testing.T) {
	t.Parallel()

	// Server must not be hit for non-npm entities. Counter stays
	// zero; if it increments, the scheme filter is leaky.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		calls++
	}))
	defer srv.Close()

	for _, e := range []*profile.Entity{
		{CanonicalURI: "repo:github/expressjs/express"},
		{CanonicalURI: "pkg:pypi/requests"},
		{CanonicalURI: "identity:github/alecthomas"},
		{CanonicalURI: ""},
		nil, // nil entity must not panic
	} {
		result, err := newTestCollector(srv).Collect(context.Background(), e)
		require.NoError(t, err)
		require.NotNil(t, result, "result must be non-nil even for unfiltered entities")
		assert.Equal(t, 0, result.SignalCount())
		assert.Equal(t, 0, result.AbsenceCount())
	}
	assert.Equal(t, 0, calls, "non-npm entities must not trigger a registry request")
}

// ----- 404: absence, non-retryable -----

func TestCollector_Collect_NotFound_RecordsNonRetryableAbsence(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("nonexistent"))
	require.NoError(t, err, "collection returns nil error on upstream 404; absence is in the result")
	assert.Equal(t, 1, result.AbsenceCount())
	require.Len(t, result.Failures, 1)
	assert.False(t, result.Failures[0].Retryable,
		"404 is definitive, not retryable")
	assert.Contains(t, result.Failures[0].Reason, "not found")
}

// ----- 500: absence, retryable -----

func TestCollector_Collect_ServerError_RecordsRetryableAbsence(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("express"))
	require.NoError(t, err)
	assert.Equal(t, 1, result.AbsenceCount())
	require.Len(t, result.Failures, 1)
	assert.True(t, result.Failures[0].Retryable,
		"500 can be transient; mark retryable")
}

// ----- missing latest-version time: absence -----

func TestCollector_Collect_NoLatestVersionTime_RecordsAbsence(t *testing.T) {
	t.Parallel()

	// Response has dist-tags.latest but no corresponding time entry.
	// Real registry responses always pair these; an absent pairing
	// is a malformed response we surface rather than silently emit
	// a zero-value timestamp.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"x","dist-tags":{"latest":"1.0.0"},"time":{}}`)
	}))
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("x"))
	require.NoError(t, err)
	assert.Equal(t, 0, result.SignalCount())
	assert.Equal(t, 1, result.AbsenceCount(),
		"no time entry for latest version should surface as absence")
}

func TestCollector_Collect_NoDistTagsLatest_RecordsAbsence(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"x","time":{"1.0.0":"2024-01-01T00:00:00Z"}}`)
	}))
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("x"))
	require.NoError(t, err)
	assert.Equal(t, 0, result.SignalCount())
	assert.Equal(t, 1, result.AbsenceCount())
}

// ----- scoped package path -----

func TestCollector_Collect_ScopedPackage_UsesFullName(t *testing.T) {
	t.Parallel()

	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"@types/node","dist-tags":{"latest":"20.0.0"},"time":{"20.0.0":"2024-01-01T00:00:00Z"}}`)
	}))
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("@types/node"))
	require.NoError(t, err)
	assert.Equal(t, 1, result.SignalCount())
	assert.Equal(t, "/@types/node", seenPath,
		"scope should be preserved in the request path")
}

// ----- extractNpmPackageName edge cases -----

func TestExtractNpmPackageName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		uri         string
		wantName    string
		wantMatched bool
	}{
		{"unscoped", "pkg:npm/express", "express", true},
		{"scoped", "pkg:npm/@types/node", "@types/node", true},
		{"scoped with hyphens", "pkg:npm/@angular/core", "@angular/core", true},
		{"empty after prefix", "pkg:npm/", "", false},
		{"different ecosystem", "pkg:pypi/requests", "", false},
		{"repo uri", "repo:github/x/y", "", false},
		{"identity", "identity:github/alecthomas", "", false},
		{"empty uri", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractNpmPackageName(&profile.Entity{CanonicalURI: tc.uri})
			assert.Equal(t, tc.wantMatched, ok)
			assert.Equal(t, tc.wantName, got)
		})
	}

	// nil entity must return (_, false) without panicking.
	got, ok := extractNpmPackageName(nil)
	assert.False(t, ok)
	assert.Equal(t, "", got)
}
