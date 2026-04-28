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
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/sarahmaeve/signatory/internal/manifest"
	"github.com/sarahmaeve/signatory/internal/profile"
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
//	github.com/Owner/Repo        → repo:github/owner/repo        (case-folded)
//	github.com/owner/repo/sub/x  → repo:github/owner/repo        (strip subpackage)
//	gopkg.in/yaml.v3             → pkg:golang/gopkg.in/yaml.v3
//	modernc.org/sqlite           → pkg:golang/modernc.org/sqlite
//	example.com                  → pkg:golang/example.com
//
// Subpackage stripping for GitHub paths: Go allows import paths
// like "github.com/foo/bar/subdir" but the repository is
// "github.com/foo/bar". The canonical entity is the repo, not
// the subdirectory.
//
// Case folding for GitHub paths: GitHub treats owner/name as
// case-insensitive at the API and clone layer, so equivalent
// references (BurntSushi/toml, burntsushi/toml, BURNTSUSHI/TOML)
// must collapse to one canonical URI. Delegated to
// profile.CanonicalRepoURI to keep the canonical form in one
// place — see its doc comment for the underlying invariant.
//
// pkg:golang/ scheme: the gomod parser deliberately produces the
// repo:github/ lens for github-hosted modules (the survey/CLI
// repo-identity workflow) but the pkg:golang/ lens for non-github
// vanity paths. pkg:golang/ matches the [purl spec](https://github.com/package-url/purl-spec)
// type identifier for Go modules — see design/entity-model-v2.md
// "Standard purl." Earlier versions of this parser emitted "pkg:go/"
// (commit bfe5df8, 2026-04-20); that was an oversight against the
// design-doc target and got corrected once the dogfood walk surfaced
// it. Pre-existing pkg:go/ rows in the store are reachable via
// profile.AlternateURIs as a backwards-compat alternate.
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
			return profile.CanonicalRepoURI("github", parts[0], parts[1])
		}
		// Malformed: "github.com/" with no owner. Fall through to
		// the pkg:golang/ form so the raw input is preserved.
	}
	return "pkg:golang/" + importPath
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

// runGoModGraph is the seam for executing `go mod graph` against
// a manifest directory. Package-level variable so tests can
// substitute a fake-output function without needing the Go
// toolchain on PATH or a fully resolvable go.sum. Production
// code calls defaultRunGoModGraph; tests assign their own.
var runGoModGraph = defaultRunGoModGraph

func defaultRunGoModGraph(ctx context.Context, manifestDir string) ([]byte, error) {
	//nolint:gosec // G204: argv-form exec of "go" with literal subcommand and flags;
	// manifestDir is set via cmd.Dir, not injected into args
	cmd := exec.CommandContext(ctx, "go", "mod", "graph")
	cmd.Dir = manifestDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("`go mod graph` in %q: %w: %s",
			manifestDir, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// ParseGraph extracts the transitive dependency graph for the
// go.mod at path by running `go mod graph` and parsing its
// output (one edge per line, "parent@version child@version").
// Both sides of each edge are converted to canonical URIs via
// canonicalizeGoImportPath, and the @version suffix is dropped
// — reachability questions are URI-level, not version-pinned.
//
// The root module is the one parent that appears WITHOUT an
// @version suffix in the output (`go mod graph` formats the
// current module that way). The first such parent we encounter
// becomes Graph.RootURI.
//
// Returns manifest.ErrGraphUnavailable (wrapped with the
// underlying cause) if the toolchain isn't available, the
// subprocess fails, or the output is unparseable. Survey treats
// this as non-fatal and falls back to a no-graph rendering.
func ParseGraph(ctx context.Context, path string) (manifest.Graph, error) {
	raw, err := runGoModGraph(ctx, filepath.Dir(path))
	if err != nil {
		return manifest.Graph{}, fmt.Errorf("%w: %w", manifest.ErrGraphUnavailable, err)
	}
	g, err := parseGoModGraphOutput(raw)
	if err != nil {
		return manifest.Graph{}, fmt.Errorf("%w: %w", manifest.ErrGraphUnavailable, err)
	}
	return g, nil
}

// parseGoModGraphOutput is the pure parser for `go mod graph`
// stdout. Each non-empty line is "parent[@version] child@version";
// anything else is a parse error (we'd rather fail loud than
// silently drop edges).
//
// Split out from ParseGraph so unit tests exercise the parser
// without needing a real subprocess or go.sum. Tolerates blank
// lines and a trailing newline.
func parseGoModGraphOutput(raw []byte) (manifest.Graph, error) {
	g := manifest.Graph{}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	// Allow long lines — module paths plus version suffixes can
	// add up. 1 MiB per line is well above any realistic edge.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parent, child, ok := strings.Cut(line, " ")
		if !ok {
			return manifest.Graph{}, fmt.Errorf(
				"line %d: expected `parent child`, got %q", lineNo, line)
		}
		parentURI, parentHasVersion := splitModulePathVersion(parent)
		childURI, _ := splitModulePathVersion(child)
		if parentURI == "" || childURI == "" {
			return manifest.Graph{}, fmt.Errorf(
				"line %d: empty module path on one side of edge %q", lineNo, line)
		}
		// First parent without an @version suffix is the root
		// module (the project itself per `go mod graph` semantics).
		if !parentHasVersion && g.RootURI == "" {
			g.RootURI = parentURI
		}
		g.Edges = append(g.Edges, manifest.Edge{Parent: parentURI, Child: childURI})
	}
	if err := scanner.Err(); err != nil {
		return manifest.Graph{}, fmt.Errorf("scan graph output: %w", err)
	}
	if len(g.Edges) == 0 {
		// Empty graph is unusual but not strictly an error — a
		// module with zero deps would produce zero edges. Fold
		// into ErrGraphUnavailable so survey treats it as
		// "nothing to bucket" rather than spurious empty buckets.
		return manifest.Graph{}, errors.New("no edges in `go mod graph` output")
	}
	return g, nil
}

// splitModulePathVersion splits "path@version" into ("path",
// true) when the @version suffix is present, or ("path", false)
// when it isn't. The path component is converted to a canonical
// URI via canonicalizeGoImportPath. Empty input returns ("", false).
func splitModulePathVersion(s string) (canonical string, hasVersion bool) {
	if s == "" {
		return "", false
	}
	if path, _, ok := strings.Cut(s, "@"); ok {
		return canonicalizeGoImportPath(path), true
	}
	return canonicalizeGoImportPath(s), false
}
