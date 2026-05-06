package htmlreport

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPURLToRegistryURL(t *testing.T) {
	cases := []struct {
		name string
		uri  string
		want string
	}{
		{"empty input", "", ""},
		{"unknown scheme", "identity:gh/sarahmaeve", ""},
		{"unsupported ecosystem", "pkg:nuget/foo", ""},

		{"pypi unversioned", "pkg:pypi/dark-matter", "https://pypi.org/project/dark-matter/"},
		{"pypi versioned", "pkg:pypi/requests@2.31.0", "https://pypi.org/project/requests/2.31.0/"},

		{"npm plain", "pkg:npm/lodash", "https://www.npmjs.com/package/lodash"},
		{"npm versioned", "pkg:npm/lodash@4.17.21", "https://www.npmjs.com/package/lodash/v/4.17.21"},
		{"npm scoped", "pkg:npm/@types/node", "https://www.npmjs.com/package/@types/node"},

		{"cargo plain", "pkg:cargo/serde", "https://crates.io/crates/serde"},
		{"cargo versioned", "pkg:cargo/serde@1.0.219", "https://crates.io/crates/serde/1.0.219"},

		{"gem plain", "pkg:gem/rails", "https://rubygems.org/gems/rails"},
		{"gem versioned", "pkg:gem/rails@7.1.3", "https://rubygems.org/gems/rails/versions/7.1.3"},

		{"maven", "pkg:maven/com.google.guava/guava", "https://central.sonatype.com/artifact/com.google.guava/guava"},

		{"golang plain", "pkg:golang/github.com/rs/xid", "https://pkg.go.dev/github.com/rs/xid"},
		{"golang versioned", "pkg:golang/github.com/rs/xid@v1.5.0", "https://pkg.go.dev/github.com/rs/xid@v1.5.0"},

		{"github repo", "repo:github/sarahmaeve/signatory", "https://github.com/sarahmaeve/signatory"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, PURLToRegistryURL(c.uri))
		})
	}
}
