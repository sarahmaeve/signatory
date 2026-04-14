// Package signatory serves a single purpose: to host the //go:embed
// directive that ships signatory's default prompt templates inside
// the binary. The directive must live at or above the templates/
// directory in the module tree, and templates/ is at module root, so
// this file lives at module root too.
//
// This package intentionally has no other exports. Business logic
// stays in internal/ subpackages.
package signatory

import "embed"

// EmbeddedTemplates is the compiled-in fallback copy of every file
// under templates/. The path resolver (internal/config) uses it when
// no filesystem copy of a requested template is found.
//
// The `all:` prefix ensures dotfiles and directories named with a
// leading underscore are included — future-proofing against template
// maintainers adding `.meta` sidecars or `_partials/` without having
// to remember to update this directive.
//
//go:embed all:templates
var EmbeddedTemplates embed.FS
