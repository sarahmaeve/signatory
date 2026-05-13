package maven

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNormalizeDeclaredRepoURL covers the SCM URL shapes that maven
// POMs emit in practice. Maven's <scm><url> and <scm><connection>
// elements admit several variants that signatory must reduce to a
// canonical https://<forge>/<owner>/<name> form before they reach
// safeGitCloneURL (which url.Parses the input and rejects SCP-form
// shorthand outright with "first path segment in URL cannot contain
// colon").
//
// Mirrors npm.TestNormalizeDeclaredRepoURL — the contract is "either
// a clean clone URL or empty," with empty being the unambiguous
// "no source signatory can clone" signal. The SCP-shorthand cases
// are the regression motivating this function: micrometer-metrics's
// POM ships its <scm><url> as git@github.com:micrometer-metrics/
// micrometer.git and signatory's analyze --refresh --clone path
// rejected it pre-fix.
func TestNormalizeDeclaredRepoURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		// Accepted forms → normalized https URL. Owner/name are
		// case-folded to lowercase by CloneURLForRepoPlatform, matching
		// every other ecosystem's normalizer.
		{"https no .git", "https://github.com/FasterXML/jackson-databind",
			"https://github.com/fasterxml/jackson-databind"},
		{"https with .git", "https://github.com/google/guava.git",
			"https://github.com/google/guava"},
		{"SCP shorthand with .git", "git@github.com:micrometer-metrics/micrometer.git",
			"https://github.com/micrometer-metrics/micrometer"},
		{"SCP shorthand no .git", "git@github.com:foo/bar",
			"https://github.com/foo/bar"},
		{"scm:git: prefix with https", "scm:git:https://github.com/foo/bar.git",
			"https://github.com/foo/bar"},
		{"scm:git: prefix with SCP", "scm:git:git@github.com:foo/bar.git",
			"https://github.com/foo/bar"},
		{"scm:svn: prefix defensive", "scm:svn:https://github.com/foo/bar",
			"https://github.com/foo/bar"},
		{"codeberg https", "https://codeberg.org/forgejo/forgejo.git",
			"https://codeberg.org/forgejo/forgejo"},
		{"codeberg SCP shorthand", "git@codeberg.org:forgejo/forgejo.git",
			"https://codeberg.org/forgejo/forgejo"},
		{"gitlab https", "https://gitlab.com/gitlab-org/gitlab.git",
			"https://gitlab.com/gitlab-org/gitlab"},

		// Rejected forms → empty string. The contract is symmetric with
		// every other ecosystem's normalizer: a POM declaring a SCM URL
		// on a forge signatory doesn't first-class produces the same
		// "no clone source" signal as a POM declaring no SCM at all.
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"bitbucket", "https://bitbucket.org/foo/bar.git", ""},
		{"sourceforge", "https://sourceforge.net/projects/foo/", ""},
		{"self-hosted host", "https://git.example.com/foo/bar", ""},
		{"garbage", "not a url", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, NormalizeDeclaredRepoURL(tc.in))
		})
	}
}

// TestResolveRepoURL_SSHShorthandInURL is the integration regression
// test for the bug the cadence cross-ecosystem smoke run surfaced on
// 2026-05-13: micrometer-metrics's POM declares
// <scm><url>git@github.com:micrometer-metrics/micrometer.git</url>
// — SSH shorthand in the <url> element, not the more common
// <connection> element where parseSCMURL would strip the scm:git:
// prefix. Pre-fix, the maven analyze flow stamped that raw string on
// entity.URL and safeGitCloneURL rejected it because Go's url.Parse
// can't accept the SCP form. With NormalizeDeclaredRepoURL wired
// into ResolveRepoURL, the same POM produces a clean https clone URL.
func TestResolveRepoURL_SSHShorthandInURL(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<project>
  <scm>
    <url>git@github.com:micrometer-metrics/micrometer.git</url>
  </scm>
</project>`)) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	url, err := client.ResolveRepoURL(context.Background(),
		"io.micrometer", "micrometer-core", "1.13.0")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/micrometer-metrics/micrometer", url,
		"SSH-shorthand <url> must normalize to https clone URL")
}

// TestResolveRepoURL_NonForgeSCMFallsThroughToParent verifies that
// when an artifact's own POM declares an SCM URL on a forge signatory
// can't clone (bitbucket, self-hosted git, etc.), ResolveRepoURL
// treats the artifact's SCM as "unresolvable" and falls through to
// the parent chain. The alternative — returning the non-forge URL —
// would stamp a URL on entity.URL that downstream collectors can't
// use, when a usable URL might exist on the parent. This is the
// design choice flagged in the fix's commit message.
func TestResolveRepoURL_NonForgeSCMFallsThroughToParent(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		// Artifact POM — bitbucket SCM (not first-classed).
		case "/maven2/com/example/child/1.0.0/child-1.0.0.pom":
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<project>
  <parent>
    <groupId>com.example</groupId>
    <artifactId>parent-artifact</artifactId>
    <version>1.0.0</version>
  </parent>
  <scm>
    <url>https://bitbucket.org/example/child</url>
  </scm>
  <artifactId>child</artifactId>
</project>`)) //nolint:errcheck

		// Parent POM — github SCM.
		case "/maven2/com/example/parent-artifact/1.0.0/parent-artifact-1.0.0.pom":
			w.Header().Set("Content-Type", "application/xml")
			w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<project>
  <groupId>com.example</groupId>
  <artifactId>parent-artifact</artifactId>
  <version>1.0.0</version>
  <scm>
    <url>https://github.com/example/parent-artifact</url>
  </scm>
</project>`)) //nolint:errcheck

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	url, err := client.ResolveRepoURL(context.Background(),
		"com.example", "child", "1.0.0")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/example/parent-artifact", url,
		"non-forge SCM on the artifact should fall through to the usable parent SCM")
}
