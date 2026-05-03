package resolver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/signal/registry/gem"
)

func TestGemResolver_ResolveSource_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := gem.GemResponse{
			Name:          "rails",
			SourceCodeURI: "https://github.com/rails/rails",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer srv.Close()

	client := gem.NewClientWithBaseURL(srv.URL)
	resolver := NewGemResolverWithClient(client)

	ds, err := resolver.ResolveSource(context.Background(), "rails")
	require.NoError(t, err)
	assert.Equal(t, "repo:github/rails/rails", ds.URI)
	assert.Equal(t, "https://github.com/rails/rails", ds.URL)
	assert.True(t, ds.SelfReported)
}

func TestGemResolver_ResolveSource_NoRepository(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := gem.GemResponse{
			Name:          "no-repo",
			SourceCodeURI: "",
			HomepageURI:   "",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer srv.Close()

	client := gem.NewClientWithBaseURL(srv.URL)
	resolver := NewGemResolverWithClient(client)

	ds, err := resolver.ResolveSource(context.Background(), "no-repo")
	require.NoError(t, err)
	assert.Empty(t, ds.URI)
	assert.Empty(t, ds.URL)
	assert.True(t, ds.SelfReported)
}

func TestGemResolver_ResolveSource_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := gem.NewClientWithBaseURL(srv.URL)
	resolver := NewGemResolverWithClient(client)

	_, err := resolver.ResolveSource(context.Background(), "nonexistent")
	require.Error(t, err)
}

func TestGemResolver_RegisteredInDefault(t *testing.T) {
	t.Parallel()

	ecosystems := Default.Ecosystems()
	assert.Contains(t, ecosystems, "gem",
		"gem should be registered in the default resolver registry")
}
