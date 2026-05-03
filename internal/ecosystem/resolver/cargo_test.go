package resolver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/signal/registry/cargo"
)

func TestCargoResolver_ResolveSource_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := cargo.CrateResponse{
			Crate: cargo.Crate{
				Name:       "serde",
				Repository: "https://github.com/serde-rs/serde",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer srv.Close()

	client := cargo.NewClientWithBaseURL(srv.URL)
	resolver := NewCargoResolverWithClient(client)

	ds, err := resolver.ResolveSource(context.Background(), "serde")
	require.NoError(t, err)
	assert.Equal(t, "repo:github/serde-rs/serde", ds.URI)
	assert.Equal(t, "https://github.com/serde-rs/serde", ds.URL)
	assert.True(t, ds.SelfReported)
}

func TestCargoResolver_ResolveSource_NoRepository(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := cargo.CrateResponse{
			Crate: cargo.Crate{
				Name:       "no-repo",
				Repository: "",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer srv.Close()

	client := cargo.NewClientWithBaseURL(srv.URL)
	resolver := NewCargoResolverWithClient(client)

	ds, err := resolver.ResolveSource(context.Background(), "no-repo")
	require.NoError(t, err)
	assert.Empty(t, ds.URI)
	assert.Empty(t, ds.URL)
	assert.True(t, ds.SelfReported)
}

func TestCargoResolver_ResolveSource_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := cargo.NewClientWithBaseURL(srv.URL)
	resolver := NewCargoResolverWithClient(client)

	_, err := resolver.ResolveSource(context.Background(), "nonexistent")
	require.Error(t, err)
}

func TestCargoResolver_RegisteredInDefault(t *testing.T) {
	t.Parallel()

	ecosystems := Default.Ecosystems()
	assert.Contains(t, ecosystems, "cargo",
		"cargo should be registered in the default resolver registry")
	assert.Contains(t, ecosystems, "crates",
		"crates should be registered as an alias")
}
