// Package gomod parses a Go module manifest (go.mod) into the
// ecosystem-neutral types defined in internal/manifest.
//
// Uses golang.org/x/mod/modfile under the hood — the official
// Go parser that handles every go.mod edge case the official
// toolchain handles (block vs line form, replace directives,
// retract blocks, comment positioning, quoted strings). Rolling
// our own parser would replicate that surface poorly; the stdlib-
// adjacent module is the right trust boundary.
package gomod

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/sarahmaeve/signatory/internal/manifest"
)

// Parse reads the go.mod at path and returns ProjectInfo + Deps.
//
// Canonical URI resolution:
//   - github.com/owner/repo → repo:github/owner/repo
//   - everything else       → pkg:go/<verbatim-path>
//
// The pkg:go/ form preserves vanity import paths
// (modernc.org/sqlite, gopkg.in/yaml.v3) exactly as declared in
// go.mod. A user who runs `signatory analyze modernc.org/sqlite`
// would be storing under the same URI — the store key is stable
// across the manifest and CLI views.
//
// Direct vs indirect: go.mod marks indirect deps with a
// "// indirect" comment. modfile exposes this via Require.Indirect.
// Deps with Indirect=true are transitive; Direct=false on the
// returned Dep.
//
// Replace directives: a `replace` entry points one import path at
// a different source. We rewrite the Dep.Name / CanonicalURI to
// the replacement so the survey reflects what actually gets
// built. Local-path replacements (→ ../localfork) are marked with
// ecosystem="go-local-replace" so survey can flag them explicitly
// rather than mis-resolve to a canonical URI.
func Parse(path string) (manifest.ProjectInfo, []manifest.Dep, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied manifest path from --manifest or Detect result
	if err != nil {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("read %q: %w", path, err)
	}

	mf, err := modfile.Parse(path, data, nil)
	if err != nil {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("parse %q: %w", path, err)
	}

	info := manifest.ProjectInfo{
		ManifestPath: path,
		Ecosystem:    "go",
	}
	if mf.Module != nil {
		info.Name = mf.Module.Mod.Path
	}
	if mf.Go != nil {
		info.EcoVersion = mf.Go.Version
	}

	// Build a replace-lookup so we can rewrite deps that are
	// replaced before emitting them. modfile gives us replaces as
	// a list; most go.mods have very few, so linear lookup is fine.
	replaces := map[string]*modfile.Replace{}
	for _, r := range mf.Replace {
		if r == nil {
			continue
		}
		// Key on the original module path. Version-specific
		// replacements (replace X v1.2.3 => ...) narrow further
		// but we don't need that granularity in v0.1 — survey just
		// wants to know the effective source.
		replaces[r.Old.Path] = r
	}

	deps := make([]manifest.Dep, 0, len(mf.Require))
	for _, req := range mf.Require {
		if req == nil {
			continue
		}
		d := manifest.Dep{
			Name:      req.Mod.Path,
			Version:   req.Mod.Version,
			Direct:    !req.Indirect,
			Ecosystem: "go",
		}

		// Apply replace if present.
		if r, ok := replaces[req.Mod.Path]; ok {
			if isLocalPath(r.New.Path) {
				// Local-replace: the effective source is on the
				// filesystem, not in a registry. Flag via the
				// ecosystem slug so survey can render it as
				// "local replacement, not analyzable remotely"
				// rather than trying to canonicalize.
				d.Name = req.Mod.Path + " (local-replaced → " + r.New.Path + ")"
				d.Ecosystem = "go-local-replace"
				d.CanonicalURI = "" // intentionally empty; survey reports specially
				deps = append(deps, d)
				continue
			}
			// Remote-replace: the effective source is a different
			// remote module. Rewrite to that path so the store
			// lookup points at the FORK, not the original.
			d.Name = r.New.Path
			if r.New.Version != "" {
				d.Version = r.New.Version
			}
		}

		d.CanonicalURI = canonicalizeGoImportPath(d.Name)
		deps = append(deps, d)
	}

	return info, deps, nil
}

// canonicalizeGoImportPath maps a Go import path to a signatory
// canonical URI.
//
//	github.com/owner/repo        → repo:github/owner/repo
//	github.com/owner/repo/sub/x  → repo:github/owner/repo    (strip subpackage)
//	gopkg.in/yaml.v3             → pkg:go/gopkg.in/yaml.v3
//	modernc.org/sqlite           → pkg:go/modernc.org/sqlite
//	example.com                  → pkg:go/example.com
//
// Subpackage stripping for GitHub paths: Go allows import paths
// like "github.com/foo/bar/subdir" but the repository is
// "github.com/foo/bar". The canonical entity is the repo, not
// the subdirectory.
//
// Empty input returns empty output; the caller decides how to
// handle that (survey will surface an unanalyzable dep).
func canonicalizeGoImportPath(importPath string) string {
	if importPath == "" {
		return ""
	}
	const githubPrefix = "github.com/"
	if strings.HasPrefix(importPath, githubPrefix) {
		parts := strings.SplitN(importPath[len(githubPrefix):], "/", 3)
		if len(parts) >= 2 {
			return "repo:github/" + parts[0] + "/" + parts[1]
		}
		// Malformed: "github.com/" with no owner. Fall through to
		// the pkg:go/ form so the raw input is preserved.
	}
	return "pkg:go/" + importPath
}

// isLocalPath returns true when a replace target is a filesystem
// path rather than a module path. Filesystem indicators: starts
// with "./", "../", "/", or "~".
//
// This matches what `go mod` considers a local replacement. A
// module path like "github.com/foo/bar" doesn't match any of
// these.
func isLocalPath(s string) bool {
	return strings.HasPrefix(s, "./") ||
		strings.HasPrefix(s, "../") ||
		strings.HasPrefix(s, "/") ||
		strings.HasPrefix(s, "~")
}
