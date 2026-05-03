package gem

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/manifest"
)

func TestParse_Simple(t *testing.T) {
	t.Parallel()

	info, deps, err := Parse("testdata/simple/Gemfile")
	require.NoError(t, err)

	// ProjectInfo from the Gemfile.
	assert.Equal(t, "testdata/simple/Gemfile", info.ManifestPath)
	assert.Equal(t, "gem", info.Ecosystem)
	assert.Equal(t, "3.2.2", info.EcoVersion)

	// With lockfile present, we get all resolved packages.
	// Direct deps from DEPENDENCIES section: rails, pg, puma,
	// rspec-rails, debug, rubocop = 6 direct.
	// Transitives from GEM specs not in DEPENDENCIES.
	require.NotEmpty(t, deps)

	// Check that direct deps are marked correctly.
	byName := map[string]manifest.Dep{}
	for _, d := range deps {
		byName[d.Name] = d
	}

	// Direct deps.
	rails := byName["rails"]
	assert.True(t, rails.Direct, "rails should be direct")
	assert.Equal(t, "pkg:gem/rails", rails.CanonicalURI)
	assert.Equal(t, "7.1.3", rails.Version)
	assert.Equal(t, "gem", rails.Ecosystem)

	pg := byName["pg"]
	assert.True(t, pg.Direct, "pg should be direct")
	assert.Equal(t, "pkg:gem/pg", pg.CanonicalURI)
	assert.Equal(t, "1.5.4", pg.Version)

	puma := byName["puma"]
	assert.True(t, puma.Direct, "puma should be direct")
	assert.Equal(t, "pkg:gem/puma", puma.CanonicalURI)
	assert.Equal(t, "6.4.2", puma.Version)

	rspec := byName["rspec-rails"]
	assert.True(t, rspec.Direct, "rspec-rails should be direct")
	assert.Equal(t, "pkg:gem/rspec-rails", rspec.CanonicalURI)

	debug := byName["debug"]
	assert.True(t, debug.Direct, "debug should be direct")

	rubocop := byName["rubocop"]
	assert.True(t, rubocop.Direct, "rubocop should be direct")

	// Transitive deps.
	nio4r := byName["nio4r"]
	assert.False(t, nio4r.Direct, "nio4r should be transitive")
	assert.Equal(t, "pkg:gem/nio4r", nio4r.CanonicalURI)
	assert.Equal(t, "2.7.0", nio4r.Version)

	rack := byName["rack"]
	assert.False(t, rack.Direct, "rack should be transitive")

	// Total: 6 direct + transitives from the lockfile.
	directCount := 0
	for _, d := range deps {
		if d.Direct {
			directCount++
		}
	}
	assert.Equal(t, 6, directCount, "should have 6 direct deps")
}

func TestParse_WithLocalDeps(t *testing.T) {
	t.Parallel()

	_, deps, err := Parse("testdata/with-local/Gemfile")
	require.NoError(t, err)

	byName := map[string]manifest.Dep{}
	for _, d := range deps {
		byName[d.Name] = d
	}

	// Registry deps should have canonical URIs.
	puma := byName["puma"]
	assert.Equal(t, "pkg:gem/puma", puma.CanonicalURI)
	assert.Equal(t, "gem", puma.Ecosystem)
	assert.True(t, puma.Direct)

	sidekiq := byName["sidekiq"]
	assert.Equal(t, "pkg:gem/sidekiq", sidekiq.CanonicalURI)
	assert.True(t, sidekiq.Direct)

	// Git dep: marked as gem-local, no canonical URI.
	engine := byName["my_engine"]
	assert.Equal(t, "gem-local", engine.Ecosystem)
	assert.Empty(t, engine.CanonicalURI)
	assert.True(t, engine.Direct)

	// Path dep: marked as gem-local, no canonical URI.
	local := byName["local_lib"]
	assert.Equal(t, "gem-local", local.Ecosystem)
	assert.Empty(t, local.CanonicalURI)
	assert.True(t, local.Direct)
}

func TestParse_NoGemfile_ReturnsError(t *testing.T) {
	t.Parallel()

	_, _, err := Parse("/does/not/exist/Gemfile")
	require.Error(t, err)
}

func TestParse_GemfileOnly_NoLockfile(t *testing.T) {
	t.Parallel()

	// Create a temp dir with only a Gemfile, no lock.
	dir := t.TempDir()
	gemfilePath := filepath.Join(dir, "Gemfile")
	writeTestFile(t, gemfilePath, `source "https://rubygems.org"

ruby "3.3.0"

gem "sinatra", "~> 4.0"
gem "puma"
gem "local_thing", path: "./lib"
`)

	info, deps, err := Parse(gemfilePath)
	require.NoError(t, err)

	assert.Equal(t, "gem", info.Ecosystem)
	assert.Equal(t, "3.3.0", info.EcoVersion)

	// Without a lockfile, we get deps from Gemfile scanning only.
	// No resolved versions available.
	byName := map[string]manifest.Dep{}
	for _, d := range deps {
		byName[d.Name] = d
	}

	sinatra := byName["sinatra"]
	assert.True(t, sinatra.Direct)
	assert.Equal(t, "pkg:gem/sinatra", sinatra.CanonicalURI)
	assert.Empty(t, sinatra.Version, "no lockfile means no resolved version")

	puma := byName["puma"]
	assert.True(t, puma.Direct)
	assert.Equal(t, "pkg:gem/puma", puma.CanonicalURI)

	local := byName["local_thing"]
	assert.True(t, local.Direct)
	assert.Equal(t, "gem-local", local.Ecosystem)
	assert.Empty(t, local.CanonicalURI)
}

func TestParseGraph_Simple(t *testing.T) {
	t.Parallel()

	g, err := ParseGraph("testdata/simple/Gemfile.lock")
	require.NoError(t, err)

	assert.NotEmpty(t, g.Edges)

	// Build an edge set for easy lookup.
	type edge struct{ parent, child string }
	edges := map[edge]bool{}
	for _, e := range g.Edges {
		edges[edge{e.Parent, e.Child}] = true
	}

	// rails depends on actioncable, actionpack, activesupport.
	assert.True(t, edges[edge{"pkg:gem/rails", "pkg:gem/actioncable"}],
		"rails → actioncable edge missing")
	assert.True(t, edges[edge{"pkg:gem/rails", "pkg:gem/actionpack"}],
		"rails → actionpack edge missing")
	assert.True(t, edges[edge{"pkg:gem/rails", "pkg:gem/activesupport"}],
		"rails → activesupport edge missing")

	// actioncable depends on actionpack, nio4r.
	assert.True(t, edges[edge{"pkg:gem/actioncable", "pkg:gem/actionpack"}],
		"actioncable → actionpack edge missing")
	assert.True(t, edges[edge{"pkg:gem/actioncable", "pkg:gem/nio4r"}],
		"actioncable → nio4r edge missing")

	// puma depends on nio4r.
	assert.True(t, edges[edge{"pkg:gem/puma", "pkg:gem/nio4r"}],
		"puma → nio4r edge missing")

	// activesupport depends on concurrent-ruby.
	assert.True(t, edges[edge{"pkg:gem/activesupport", "pkg:gem/concurrent-ruby"}],
		"activesupport → concurrent-ruby edge missing")
}

func TestParseGraph_WithLocal(t *testing.T) {
	t.Parallel()

	g, err := ParseGraph("testdata/with-local/Gemfile.lock")
	require.NoError(t, err)

	// GIT/PATH section specs should produce edges too.
	type edge struct{ parent, child string }
	edges := map[edge]bool{}
	for _, e := range g.Edges {
		edges[edge{e.Parent, e.Child}] = true
	}

	// sidekiq depends on concurrent-ruby, connection_pool, redis-client.
	assert.True(t, edges[edge{"pkg:gem/sidekiq", "pkg:gem/concurrent-ruby"}],
		"sidekiq → concurrent-ruby edge missing")
	assert.True(t, edges[edge{"pkg:gem/sidekiq", "pkg:gem/connection-pool"}] ||
		edges[edge{"pkg:gem/sidekiq", "pkg:gem/connection_pool"}],
		"sidekiq → connection_pool edge missing")
}

func TestParseGraph_MissingFile(t *testing.T) {
	t.Parallel()

	_, err := ParseGraph("/does/not/exist/Gemfile.lock")
	require.Error(t, err)
	assert.ErrorIs(t, err, manifest.ErrGraphUnavailable)
}

// writeTestFile is a test helper for creating temp fixture files.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}
