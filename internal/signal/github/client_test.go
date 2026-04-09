package github

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRepoURL(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{name: "owner/repo", input: "alecthomas/kong", wantOwner: "alecthomas", wantRepo: "kong"},
		{name: "github.com prefix", input: "github.com/stretchr/testify", wantOwner: "stretchr", wantRepo: "testify"},
		{name: "https URL", input: "https://github.com/spf13/cobra", wantOwner: "spf13", wantRepo: "cobra"},
		{name: "http URL", input: "http://github.com/spf13/cobra", wantOwner: "spf13", wantRepo: "cobra"},
		{name: "trailing .git", input: "https://github.com/spf13/cobra.git", wantOwner: "spf13", wantRepo: "cobra"},
		{name: "with whitespace", input: "  alecthomas/kong  ", wantOwner: "alecthomas", wantRepo: "kong"},
		{name: "with subpath", input: "github.com/owner/repo/tree/main", wantOwner: "owner", wantRepo: "repo"},
		{name: "empty", input: "", wantErr: true},
		{name: "just owner", input: "alecthomas", wantErr: true},
		{name: "just slash", input: "/", wantErr: true},
		{name: "empty owner", input: "/repo", wantErr: true},
		{name: "empty repo", input: "owner/", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			owner, repo, err := ParseRepoURL(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantOwner, owner)
			assert.Equal(t, tt.wantRepo, repo)
		})
	}
}

func TestParseTotalFromLink(t *testing.T) {
	tests := []struct {
		name string
		link string
		want int
	}{
		{
			name: "standard GitHub pagination",
			link: `<https://api.github.com/repositories/128887107/commits?per_page=1&page=2>; rel="next", <https://api.github.com/repositories/128887107/commits?per_page=1&page=467>; rel="last"`,
			want: 467,
		},
		{name: "empty", link: "", want: 1},
		{name: "no last rel", link: `<https://example.com?page=2>; rel="next"`, want: 1},
		{
			name: "single page",
			link: `<https://api.github.com/repos/x/y/commits?per_page=1&page=1>; rel="last"`,
			want: 1,
		},
		{
			name: "large count",
			link: `<https://api.github.com/repos/x/y/commits?per_page=1&page=2>; rel="next", <https://api.github.com/repos/x/y/commits?per_page=1&page=15432>; rel="last"`,
			want: 15432,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseTotalFromLink(tt.link)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseRateLimitReset(t *testing.T) {
	t.Run("valid timestamp", func(t *testing.T) {
		got := parseRateLimitReset("1712700000")
		assert.False(t, got.IsZero())
	})

	t.Run("empty", func(t *testing.T) {
		got := parseRateLimitReset("")
		assert.False(t, got.IsZero(), "should return a fallback time")
	})

	t.Run("invalid", func(t *testing.T) {
		got := parseRateLimitReset("notanumber")
		assert.False(t, got.IsZero(), "should return a fallback time")
	})
}
