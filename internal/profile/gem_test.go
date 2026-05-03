package profile

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeGemName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  string
	}{
		// Already lowercase — no change.
		{"rails", "rails"},
		{"nokogiri", "nokogiri"},

		// Mixed case → lowercase.
		{"Rails", "rails"},
		{"ActiveRecord", "activerecord"},
		{"RSpec", "rspec"},

		// Hyphens preserved (NOT equivalent to underscores).
		{"rspec-rails", "rspec-rails"},
		{"ruby-lsp", "ruby-lsp"},

		// Underscores preserved (distinct from hyphens).
		{"active_support", "active_support"},
		{"my_gem", "my_gem"},

		// Dots preserved.
		{"foo.bar", "foo.bar"},

		// Empty string edge case.
		{"", ""},

		// Idempotence.
		{"PUMA", "puma"},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got := NormalizeGemName(tc.input)
			assert.Equal(t, tc.want, got)

			// Idempotence check.
			assert.Equal(t, got, NormalizeGemName(got),
				"NormalizeGemName must be idempotent")
		})
	}
}

func TestCanonicalPackageURI_Gem(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		want string
	}{
		{"rails", "pkg:gem/rails"},
		{"Rails", "pkg:gem/rails"},
		{"rspec-rails", "pkg:gem/rspec-rails"},
		{"active_support", "pkg:gem/active_support"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, CanonicalPackageURI("gem", tc.name))
		})
	}
}
