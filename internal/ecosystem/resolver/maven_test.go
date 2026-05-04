package resolver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/signal/registry/maven"
)

func TestMavenResolver_ResolveSource_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/maven2/com/google/guava/guava/maven-metadata.xml":
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<?xml version="1.0"?>
<metadata>
  <groupId>com.google.guava</groupId>
  <artifactId>guava</artifactId>
  <versioning>
    <latest>33.2.1-jre</latest>
    <release>33.2.1-jre</release>
    <versions><version>33.2.1-jre</version></versions>
  </versioning>
</metadata>`)) //nolint:errcheck
		case "/maven2/com/google/guava/guava/33.2.1-jre/guava-33.2.1-jre.pom":
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<?xml version="1.0"?>
<project>
  <scm>
    <url>https://github.com/google/guava</url>
  </scm>
</project>`)) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := maven.NewClientWithBaseURL(srv.URL)
	resolver := NewMavenResolverWithClient(client)

	ds, err := resolver.ResolveSource(context.Background(), "com.google.guava/guava")
	require.NoError(t, err)
	assert.Equal(t, "repo:github/google/guava", ds.URI)
	assert.Equal(t, "https://github.com/google/guava", ds.URL)
	assert.True(t, ds.SelfReported)
}

func TestMavenResolver_ResolveSource_NoRepository(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/maven2/com/example/no-scm/maven-metadata.xml":
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<?xml version="1.0"?>
<metadata>
  <groupId>com.example</groupId>
  <artifactId>no-scm</artifactId>
  <versioning>
    <latest>1.0.0</latest>
    <release>1.0.0</release>
    <versions><version>1.0.0</version></versions>
  </versioning>
</metadata>`)) //nolint:errcheck
		case "/maven2/com/example/no-scm/1.0.0/no-scm-1.0.0.pom":
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<?xml version="1.0"?>
<project>
  <groupId>com.example</groupId>
  <artifactId>no-scm</artifactId>
</project>`)) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := maven.NewClientWithBaseURL(srv.URL)
	resolver := NewMavenResolverWithClient(client)

	ds, err := resolver.ResolveSource(context.Background(), "com.example/no-scm")
	require.NoError(t, err)
	assert.Empty(t, ds.URI)
	assert.Empty(t, ds.URL)
	assert.True(t, ds.SelfReported)
}

func TestMavenResolver_ResolveSource_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := maven.NewClientWithBaseURL(srv.URL)
	resolver := NewMavenResolverWithClient(client)

	_, err := resolver.ResolveSource(context.Background(), "com.example/nonexistent")
	require.Error(t, err)
}

func TestMavenResolver_ResolveSource_InvalidName(t *testing.T) {
	t.Parallel()

	resolver := NewMavenResolver()

	// Name without slash separator should error.
	_, err := resolver.ResolveSource(context.Background(), "just-a-name")
	require.Error(t, err)
}

func TestMavenResolver_RegisteredInDefault(t *testing.T) {
	t.Parallel()

	ecosystems := Default.Ecosystems()
	assert.Contains(t, ecosystems, "maven",
		"maven should be registered in the default resolver registry")
}
