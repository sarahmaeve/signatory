package profile

import "strings"

// NormalizeGemName applies rubygems.org's name normalization: lowercase
// only. RubyGems lookups are case-insensitive, so we normalize to
// lowercase to prevent storage fragmentation (e.g., "Rails" and "rails"
// should resolve to the same entity).
//
// Unlike cargo, hyphens and underscores are NOT equivalent in RubyGems —
// "foo-bar" and "foo_bar" can be distinct gems. They are preserved
// verbatim.
//
// Unlike PyPI, dots and repeated separators are not collapsed — gem
// names may contain dots (e.g., "ruby-lsp-rails") and they are
// meaningful.
//
// Idempotent: NormalizeGemName(NormalizeGemName(x)) == NormalizeGemName(x).
func NormalizeGemName(name string) string {
	return strings.ToLower(name)
}
