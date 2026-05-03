package main

// Smoke tests for registry source resolution. These use REALISTIC
// response shapes from real registries — the exact JSON that broke
// `--clone` in production when fixtures used sanitized URLs.
//
// Each test proves that the resolved entity.URL is a cloneable https
// URL (no /tree/<ref> paths, no git+https:// prefixes, no .git
// suffixes). A clone operation against entity.URL must succeed; if
// it wouldn't, the test must fail.
//
// Adding a new ecosystem resolver? Add a case here with the REAL
// response shape from that registry's API. Copy-paste from curl,
// not from imagination.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	gemregistry "github.com/sarahmaeve/signatory/internal/signal/registry/gem"
	"github.com/sarahmaeve/signatory/internal/store"
)

// TestSmoke_GemResolution_RealWorldShapes exercises the gem source
// resolver against response shapes observed from rubygems.org in the
// wild. Each subtest is named after the gem whose actual API response
// exposed the pattern.
//
// The assert is always: entity.URL must be a bare cloneable URL —
// something `git clone <url>` would accept. No /tree/, /blob/,
// /commit/ paths. No query strings. No .git suffix.
func TestSmoke_GemResolution_RealWorldShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		gemName       string
		sourceCodeURI string
		homepageURI   string
		wantURL       string
	}{
		{
			// rails: source_code_uri points at version tree view.
			// curl https://rubygems.org/api/v1/gems/rails.json | jq .source_code_uri
			// → "https://github.com/rails/rails/tree/v8.1.3"
			name:          "rails (tree/v8.1.3)",
			gemName:       "rails",
			sourceCodeURI: "https://github.com/rails/rails/tree/v8.1.3",
			homepageURI:   "https://rubyonrails.org",
			wantURL:       "https://github.com/rails/rails",
		},
		{
			// nokogiri: source_code_uri is the bare repo.
			// curl https://rubygems.org/api/v1/gems/nokogiri.json | jq .source_code_uri
			// → "https://github.com/sparklemotion/nokogiri"
			name:          "nokogiri (clean)",
			gemName:       "nokogiri",
			sourceCodeURI: "https://github.com/sparklemotion/nokogiri",
			homepageURI:   "https://nokogiri.org",
			wantURL:       "https://github.com/sparklemotion/nokogiri",
		},
		{
			// devise: source_code_uri has trailing slash.
			name:          "devise (trailing slash)",
			gemName:       "devise",
			sourceCodeURI: "https://github.com/heartcombo/devise/",
			homepageURI:   "https://github.com/heartcombo/devise",
			wantURL:       "https://github.com/heartcombo/devise",
		},
		{
			// puma: source_code_uri with .git suffix.
			name:          "puma (.git suffix)",
			gemName:       "puma",
			sourceCodeURI: "https://github.com/puma/puma.git",
			homepageURI:   "https://puma.io",
			wantURL:       "https://github.com/puma/puma",
		},
		{
			// sidekiq: source_code_uri with /blob/main/README.md.
			name:          "sidekiq (blob path)",
			gemName:       "sidekiq",
			sourceCodeURI: "https://github.com/sidekiq/sidekiq/blob/main/README.md",
			homepageURI:   "https://sidekiq.org",
			wantURL:       "https://github.com/sidekiq/sidekiq",
		},
		{
			// oldgem: no source_code_uri, homepage is github.
			name:          "oldgem (homepage fallback)",
			gemName:       "oldgem",
			sourceCodeURI: "",
			homepageURI:   "https://github.com/someone/oldgem",
			wantURL:       "https://github.com/someone/oldgem",
		},
		{
			// rspec-core: source_code_uri with /tree/main.
			name:          "rspec-core (tree/main)",
			gemName:       "rspec-core",
			sourceCodeURI: "https://github.com/rspec/rspec-core/tree/main",
			homepageURI:   "https://rspec.info",
			wantURL:       "https://github.com/rspec/rspec-core",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				expected := fmt.Sprintf("/api/v1/gems/%s.json", tc.gemName)
				if r.URL.Path == expected {
					json.NewEncoder(w).Encode(gemregistry.GemResponse{
						Name:          tc.gemName,
						SourceCodeURI: tc.sourceCodeURI,
						HomepageURI:   tc.homepageURI,
					}) //nolint:errcheck
				} else {
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			dir := t.TempDir()
			dbPath := filepath.Join(dir, "test.db")
			{
				s, err := store.OpenSQLite(t.Context(), dbPath)
				require.NoError(t, err)
				e := &profile.Entity{
					ID:           profile.NewEntityID(),
					CanonicalURI: "pkg:gem/" + tc.gemName,
					Type:         profile.EntityPackage,
					ShortName:    tc.gemName,
					Ecosystem:    "gem",
					URL:          "",
					CreatedAt:    time.Now().UTC(),
					UpdatedAt:    time.Now().UTC(),
				}
				require.NoError(t, s.PutEntity(t.Context(), e))
				require.NoError(t, s.Close())
			}

			var stderr bytes.Buffer
			globals := &Globals{
				DBPath:         dbPath,
				Collectors:     []signal.Collector{newMockCollector()},
				AuditFilePath:  filepath.Join(dir, "audit.log"),
				GemRegistryURL: srv.URL,
			}
			cmd := &AnalyzeCmd{
				Target:  "pkg:gem/" + tc.gemName,
				Refresh: true,
				Stderr:  &stderr,
			}
			require.NoError(t, cmd.Run(globals))

			s, err := store.OpenSQLite(t.Context(), dbPath)
			require.NoError(t, err)
			defer s.Close() //nolint:errcheck

			entity, err := s.FindEntityByURI(t.Context(), "pkg:gem/"+tc.gemName)
			require.NoError(t, err)
			assert.Equal(t, tc.wantURL, entity.URL,
				"entity.URL must be a bare cloneable URL — `git clone %s` must work", entity.URL)
		})
	}
}

// TestSmoke_NpmResolution_RealWorldShapes exercises the npm source
// resolver against response shapes observed from registry.npmjs.org.
// The npm registry's repository.url field uses formats like:
//   - "git+https://github.com/org/repo.git"
//   - "git://github.com/org/repo.git"
//   - "https://github.com/org/repo"
//
// All must resolve to a bare cloneable https URL.
func TestSmoke_NpmResolution_RealWorldShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		pkg     string
		json    string // raw JSON response body
		wantURL string
	}{
		{
			// express: git+https:// prefix + .git suffix.
			// curl https://registry.npmjs.org/express | jq .repository
			// → {"type":"git","url":"git+https://github.com/expressjs/express.git"}
			name:    "express (git+https .git)",
			pkg:     "express",
			json:    `{"name":"express","repository":{"type":"git","url":"git+https://github.com/expressjs/express.git"}}`,
			wantURL: "https://github.com/expressjs/express",
		},
		{
			// lodash: git+https without .git.
			name:    "lodash (git+https no .git)",
			pkg:     "lodash",
			json:    `{"name":"lodash","repository":{"type":"git","url":"git+https://github.com/lodash/lodash.git"}}`,
			wantURL: "https://github.com/lodash/lodash",
		},
		{
			// chalk: bare https URL.
			name:    "chalk (bare https)",
			pkg:     "chalk",
			json:    `{"name":"chalk","repository":{"type":"git","url":"https://github.com/chalk/chalk"}}`,
			wantURL: "https://github.com/chalk/chalk",
		},
		{
			// commander: string shorthand (github:owner/repo).
			// Some packages use the shorthand form; npm's client normalizes
			// it but the registry stores whatever was in package.json.
			name:    "commander (full url in field)",
			pkg:     "commander",
			json:    `{"name":"commander","repository":{"type":"git","url":"https://github.com/tj/commander.js.git"}}`,
			wantURL: "https://github.com/tj/commander.js",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.json)
			}))
			defer srv.Close()

			dir := t.TempDir()
			dbPath := filepath.Join(dir, "test.db")
			{
				s, err := store.OpenSQLite(t.Context(), dbPath)
				require.NoError(t, err)
				e := &profile.Entity{
					ID:           profile.NewEntityID(),
					CanonicalURI: "pkg:npm/" + tc.pkg,
					Type:         profile.EntityPackage,
					ShortName:    tc.pkg,
					Ecosystem:    "npm",
					URL:          "",
					CreatedAt:    time.Now().UTC(),
					UpdatedAt:    time.Now().UTC(),
				}
				require.NoError(t, s.PutEntity(t.Context(), e))
				require.NoError(t, s.Close())
			}

			var stderr bytes.Buffer
			globals := &Globals{
				DBPath:         dbPath,
				Collectors:     []signal.Collector{newMockCollector()},
				AuditFilePath:  filepath.Join(dir, "audit.log"),
				NpmRegistryURL: srv.URL,
			}
			cmd := &AnalyzeCmd{
				Target:  "pkg:npm/" + tc.pkg,
				Refresh: true,
				Stderr:  &stderr,
			}
			require.NoError(t, cmd.Run(globals))

			s, err := store.OpenSQLite(t.Context(), dbPath)
			require.NoError(t, err)
			defer s.Close() //nolint:errcheck

			entity, err := s.FindEntityByURI(t.Context(), "pkg:npm/"+tc.pkg)
			require.NoError(t, err)
			assert.Equal(t, tc.wantURL, entity.URL,
				"entity.URL must be a bare cloneable URL — `git clone %s` must work", entity.URL)
		})
	}
}
