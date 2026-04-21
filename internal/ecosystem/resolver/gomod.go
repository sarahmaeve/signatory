package resolver

import (
	"context"
	"fmt"
	"strings"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// GoModResolver implements Resolver for pkg:go/<module-path> using
// offline path-prefix rules for the common hosting patterns:
//
//   - github.com/<owner>/<name>[/...] → https://github.com/<owner>/<name>
//   - golang.org/x/<name>             → https://github.com/golang/<name>
//   - gopkg.in/<name>.v<N>            → https://github.com/go-<name>/<name>
//   - gopkg.in/<user>/<name>.v<N>     → https://github.com/<user>/<name>
//
// These four patterns cover the modules in signatory's own go.mod
// and the overwhelming majority of Go modules we'd analyze. Other
// paths (custom vanity URLs like k8s.io/client-go, bitbucket.org/...,
// gitlab.com/..., etc.) return an empty DeclaredSource with nil
// error — "we know the ecosystem but don't know this module's
// source." Callers treat that exactly like npm's no-source case.
//
// go-get meta-import lookup (HTTP GET <module>?go-get=1 → parse
// <meta name="go-import"> tag) is a follow-up when the offline
// rules aren't enough. Offline resolution is faster, deterministic,
// and doesn't require --network-precheck opt-in; the meta lookup
// would gate behind network-precheck the same way npm's registry
// lookup does.
type GoModResolver struct{}

// NewGoModResolver returns a resolver with no configuration — the
// path-prefix rules are hardcoded. Tests can construct a resolver
// directly; there's no network surface to stub.
func NewGoModResolver() *GoModResolver {
	return &GoModResolver{}
}

// ResolveSource maps a Go module path to its declared source. See
// the type-level doc for the patterns recognized.
func (r *GoModResolver) ResolveSource(_ context.Context, modulePath string) (DeclaredSource, error) {
	if modulePath == "" {
		return DeclaredSource{}, fmt.Errorf("empty module path")
	}

	// github.com/<owner>/<name>[/subpath...]
	if rest, ok := strings.CutPrefix(modulePath, "github.com/"); ok {
		owner, name, ok := takeTwoSegments(rest)
		if !ok {
			return DeclaredSource{SelfReported: true}, nil
		}
		return githubSource(owner, name)
	}

	// golang.org/x/<name> — first-party extension modules, all
	// mirrored at github.com/golang/<name>. The "/x/" path element
	// is fixed; anything else under golang.org is a vanity URL we
	// don't special-case.
	if rest, ok := strings.CutPrefix(modulePath, "golang.org/x/"); ok {
		name := firstSegment(rest)
		if name == "" {
			return DeclaredSource{SelfReported: true}, nil
		}
		return githubSource("golang", name)
	}

	// gopkg.in/<user>/<name>.v<N> or gopkg.in/<name>.v<N>.
	// See https://labix.org/gopkg.in for the path-to-github mapping:
	// the single-segment form maps to github.com/go-<name>/<name>;
	// the two-segment form maps to github.com/<user>/<name>.
	if rest, ok := strings.CutPrefix(modulePath, "gopkg.in/"); ok {
		return gopkgInSource(rest)
	}

	// Unknown vanity URL — we don't guess. Caller sees "no source
	// declared for this ecosystem" which is the right signal: the
	// target is valid, just not automatically clone-able.
	return DeclaredSource{SelfReported: true}, nil
}

// githubSource constructs a DeclaredSource for a github-hosted repo,
// routing through ResolveTarget so the canonical URI and clone URL
// stay consistent with every other pkg → repo mapping in the tree.
func githubSource(owner, name string) (DeclaredSource, error) {
	url := "https://github.com/" + owner + "/" + name
	resolved, err := profile.ResolveTarget(url)
	if err != nil {
		return DeclaredSource{}, fmt.Errorf("construct github source for %s/%s: %w", owner, name, err)
	}
	return DeclaredSource{
		URI:          resolved.CanonicalURI,
		URL:          resolved.CloneURL,
		SelfReported: true,
	}, nil
}

// gopkgInSource handles both `gopkg.in/yaml.v3` and
// `gopkg.in/user/pkg.v3` forms per the gopkg.in URL scheme.
func gopkgInSource(rest string) (DeclaredSource, error) {
	// Strip the `.v<N>` suffix first so downstream logic doesn't
	// have to care about the version marker.
	base := stripGopkgInVersion(rest)
	if base == "" {
		return DeclaredSource{SelfReported: true}, nil
	}

	// Two-segment form: user/pkg.
	if idx := strings.IndexByte(base, '/'); idx >= 0 {
		user := base[:idx]
		pkg := base[idx+1:]
		if user == "" || pkg == "" || strings.Contains(pkg, "/") {
			return DeclaredSource{SelfReported: true}, nil
		}
		return githubSource(user, pkg)
	}

	// One-segment form: maps to github.com/go-<name>/<name>.
	return githubSource("go-"+base, base)
}

// stripGopkgInVersion removes a trailing `.v<digits>` suffix. Returns
// empty when the input has no `.v` marker — gopkg.in requires one, so
// such an input is malformed.
func stripGopkgInVersion(path string) string {
	idx := strings.LastIndex(path, ".v")
	if idx < 0 {
		return ""
	}
	suffix := path[idx+2:]
	if suffix == "" {
		return ""
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return path[:idx]
}

// takeTwoSegments splits "a/b/c/d" into ("a", "b", true). Returns
// ("", "", false) when fewer than two non-empty segments are present.
func takeTwoSegments(s string) (first, second string, ok bool) {
	i := strings.IndexByte(s, '/')
	if i <= 0 {
		return "", "", false
	}
	first = s[:i]
	rest := s[i+1:]
	j := strings.IndexByte(rest, '/')
	if j < 0 {
		if rest == "" {
			return "", "", false
		}
		return first, rest, true
	}
	if j == 0 {
		return "", "", false
	}
	return first, rest[:j], true
}

// firstSegment returns the first "/"-delimited segment, or the whole
// string if there's no slash. Empty string maps to empty.
func firstSegment(s string) string {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i]
	}
	return s
}

func init() {
	Default.Register("go", NewGoModResolver())
}
