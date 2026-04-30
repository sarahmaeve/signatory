package invariants

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Invariant 1 (design/v0.1-invariants.md): signatory's Go code must
// not depend on any LLM-client SDK, and must not contain a string
// expression that evaluates to a known LLM-provider API host.
// Violations of either check fail the test suite.
//
// Two layers of defense:
//
//  1. TestNoLLMClientInModuleGraph inspects `go list -m -json all`.
//     That catches every direct dependency and every transitive one
//     (including dependencies pulled in by a `replace` directive).
//  2. TestNoLLMAPIHostsInSource parses every .go file with
//     go/parser, walks the AST, and decodes string expressions
//     (BasicLit + `+`-chained BinaryExpr operands) before comparing
//     them — case-insensitively — to the forbidden host list. That
//     catches casual bypass attempts like Unicode/hex escapes,
//     case-shifted spellings, and string concatenation across
//     literals — the patterns a contributor would reach for to hide
//     an `api.anthropic.com` from a naive byte scan.
//
// Together they express: "no SDK, and no DIY client either." Both
// are needed — either alone leaves a gap.
//
// Documented residual gaps in the source-scan layer (caught by code
// review, not by this test):
//
//   - Constants assembled by identifier reference. e.g.
//     `const a = "f"; const b = "orbidden..."; var _ = a + b`.
//     Catching this requires constant-resolution via `go/types`.
//     The literals are still each scanned, but neither contains the
//     host as a substring on its own.
//   - `string([]byte{...})` synthesis from byte sequences.
//   - Hostnames materialized at runtime from env vars, config files,
//     `embed.FS` text resources, or `fmt.Sprintf` with computed
//     parts.
//
// The module-graph check (layer 1) remains the structural fence:
// any code that actually wants to call a forbidden host needs an
// HTTP client, and the SDK shape of that need is what layer 1
// catches independent of how the hostname is spelled. The
// source-scan layer is a low-cost guard against the casual bypass,
// not a determined-attacker fence.

// forbiddenModules enumerates LLM-client SDKs in the Go ecosystem
// that signatory must never import, directly or transitively.
//
// Extend this list when a new SDK appears. The cost of a false
// positive (someone legitimately needs the dependency for a
// non-LLM-calling purpose) is a scope-change review, which is
// cheap. The cost of a false negative (an LLM client lands and
// quietly calls out to a paid API) is a v0.1 invariant breach,
// which is expensive to unwind.
var forbiddenModules = []string{
	"github.com/anthropics/anthropic-sdk-go",
	"github.com/sashabaranov/go-openai",
	"github.com/google/generative-ai-go",
	"github.com/cohere-ai/cohere-go",
	"github.com/replicate/replicate-go",
}

// forbiddenAPIHosts enumerates LLM-provider API hostnames that must
// not appear as string literals in Go source. The list is narrow on
// purpose: it matches hosts that a human would reasonably write only
// if they were building an LLM caller. General-purpose URLs (e.g.,
// github.com, registry.npmjs.org) are not in scope.
var forbiddenAPIHosts = []string{
	"api.anthropic.com",
	"api.openai.com",
	"generativelanguage.googleapis.com",
	"api.cohere.ai",
	"api.replicate.com",
}

// TestNoLLMClientInModuleGraph asserts the effective module graph
// contains no forbidden LLM-client SDK. The graph is produced by
// `go list -m -json all`, which is the canonical source for what
// Go will actually link — it reflects both the go.mod require
// entries and any transitive pull-ins after replaces.
func TestNoLLMClientInModuleGraph(t *testing.T) {
	// CommandContext (not Command) so a slow/offline module proxy
	// can't hang the test indefinitely — the test deadline kills
	// the subprocess instead of the test waiting forever.
	cmd := exec.CommandContext(t.Context(), "go", "list", "-m", "-json", "all")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "go list -m -json all failed: %s", stderr.String())

	// The output is a concatenated stream of JSON objects, not an
	// array. Decode iteratively.
	dec := json.NewDecoder(bytes.NewReader(out))
	seen := map[string]string{} // module path -> version
	for dec.More() {
		var mod struct {
			Path    string `json:"Path"`
			Version string `json:"Version"`
		}
		require.NoError(t, dec.Decode(&mod))
		if mod.Path != "" {
			seen[mod.Path] = mod.Version
		}
	}

	var violations []string
	for _, forbidden := range forbiddenModules {
		if version, ok := seen[forbidden]; ok {
			violations = append(violations, forbidden+"@"+version)
		}
	}

	if len(violations) > 0 {
		t.Fatalf(
			"forbidden LLM-client module(s) present in module graph "+
				"— this violates v0.1 invariant 1 (no direct "+
				"Anthropic/LLM API access). See "+
				"design/v0.1-invariants.md. If the dependency is "+
				"genuinely needed, propose a scope change; do not "+
				"weaken this check silently.\n  %s",
			strings.Join(violations, "\n  "),
		)
	}
}

// TestNoLLMAPIHostsInSource walks all Go source files under the
// module root and fails if any contains a string expression that
// evaluates to a forbidden LLM API hostname. The evaluation handles
// Unicode/hex escapes, case-shifted spellings, and `+`-chained
// concatenation across string literals — see scanGoSourceForHosts
// for the precise scope and the package comment for documented
// residual gaps.
//
// Scope:
//   - *.go files only (documentation, design docs, and skill
//     markdown files are excluded — they can legitimately reference
//     provider hostnames in prose)
//   - excludes this package (it holds the hostnames as test data)
//   - excludes vendor/, filestore/, node_modules/, and dotted dirs
func TestNoLLMAPIHostsInSource(t *testing.T) {
	moduleRoot := findModuleRoot(t)
	selfRel := filepath.Join("internal", "invariants")

	var violations []string
	err := filepath.WalkDir(moduleRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			name := d.Name()
			// Don't recurse into generated/vendored/IDE/build
			// directories, or into this package.
			if path != moduleRoot {
				rel, err := filepath.Rel(moduleRoot, path)
				if err == nil && rel == selfRel {
					return fs.SkipDir
				}
			}
			if name != "." && (strings.HasPrefix(name, ".") ||
				name == "vendor" ||
				name == "filestore" ||
				name == "node_modules") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // G304: path is under module root, walked by go test
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(moduleRoot, path)
		hits, scanErr := scanGoSourceForHosts(rel, data, forbiddenAPIHosts)
		if scanErr != nil {
			return scanErr
		}
		violations = append(violations, hits...)
		return nil
	})
	require.NoError(t, err)

	if len(violations) > 0 {
		t.Fatalf(
			"forbidden LLM-provider API hostname(s) present in Go "+
				"source — this violates v0.1 invariant 1. See "+
				"design/v0.1-invariants.md. If a test fixture "+
				"legitimately needs to reference a provider "+
				"hostname, put it in this package alongside the "+
				"check itself so the scope is auditable.\n  %s",
			strings.Join(violations, "\n  "),
		)
	}
}

// scanGoSourceForHosts parses src as a Go source file named filename
// (the name is used for error messages and the returned hit strings;
// the source need not exist on disk) and returns one entry per AST
// string expression that decodes to a value containing a forbidden
// host substring. Comparison is case-insensitive — DNS hostnames are
// case-insensitive at runtime, so a case-shifted spelling of the
// same host counts as the same host here.
//
// What it evaluates:
//
//   - *ast.BasicLit of kind STRING (interpreted "..." with all
//     standard escape forms, and raw `...` strings) via
//     strconv.Unquote.
//   - *ast.ParenExpr wrapping an evaluable subtree.
//   - *ast.BinaryExpr with token.ADD whose operands both evaluate
//     to strings (transitively — chains like `"a" + "b" + "c"`
//     evaluate fully).
//
// Anything else (identifiers, calls, composite literals,
// conversions) falls through and is not evaluated. The walk still
// descends into those nodes, so a literal nested inside an
// unevaluable parent is still seen as a standalone literal — but
// concatenations whose operands are identifiers (`a + b`) are not
// resolved across the identifier. See the package comment for the
// documented residual gaps.
//
// Returned strings are formatted "<filename>: <host>" matching the
// caller's pre-existing violation list shape.
func scanGoSourceForHosts(filename string, src []byte, hosts []string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", filename, err)
	}

	lowerHosts := make([]string, len(hosts))
	for i, h := range hosts {
		lowerHosts[i] = strings.ToLower(h)
	}

	var hits []string
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		val, ok := evalString(n)
		if !ok {
			return true // descend into children
		}
		lower := strings.ToLower(val)
		for i, lh := range lowerHosts {
			if strings.Contains(lower, lh) {
				hits = append(hits, filename+": "+hosts[i])
			}
		}
		// We evaluated the whole subtree — don't re-walk children.
		return false
	})
	return hits, nil
}

// evalString attempts to evaluate an AST node as a string constant
// using only literal sources and `+` concatenation. It returns
// (value, true) for *ast.BasicLit (kind STRING), *ast.ParenExpr
// wrapping an evaluable subtree, and *ast.BinaryExpr (op ADD) whose
// operands both evaluate. Anything else returns ("", false) — we
// don't follow identifiers, calls, or conversions.
func evalString(n ast.Node) (string, bool) {
	switch node := n.(type) {
	case *ast.BasicLit:
		if node.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(node.Value)
		if err != nil {
			return "", false
		}
		return s, true
	case *ast.ParenExpr:
		return evalString(node.X)
	case *ast.BinaryExpr:
		if node.Op != token.ADD {
			return "", false
		}
		l, lok := evalString(node.X)
		if !lok {
			return "", false
		}
		r, rok := evalString(node.Y)
		if !rok {
			return "", false
		}
		return l + r, true
	}
	return "", false
}

// TestScanGoSourceForHosts_CatchesEvasion exercises the AST-based
// host scanner against synthetic Go source containing patterns that
// a naive `strings.Contains(rawBytes, host)` scan would miss. The
// scanner is meant to deliver what TestNoLLMAPIHostsInSource's name
// promises — including Unicode/hex escape decoding, case-shifted
// spellings, and `+`-chain concatenation across string literals.
//
// The host list passed to the scanner is a synthetic
// "forbidden.example.com" rather than the real LLM-provider hosts:
// this tests the *rule* (decoded-string-contains-host) rather than
// the enumeration. If the scanner is reverted to a raw-byte scan,
// the unicode_escape, hex_escape, and concat cases below fail
// because the bypass form does not contain the host as a raw byte
// sequence.
//
// The "documented_gap_*" cases pin the boundaries of the current
// implementation. If a future tightening starts catching them, flip
// wantHit:true and note the upgrade in the package comment.
//
// Revert proof: replace scanGoSourceForHosts's body with a
// `bytes.Contains(src, []byte(host))` byte scan; the unicode_escape,
// hex_escape, two_piece_concat, three_piece_concat, concat_in_call,
// and case_shifted_literal cases fail.
func TestScanGoSourceForHosts_CatchesEvasion(t *testing.T) {
	t.Parallel()

	const host = "forbidden.example.com"
	hosts := []string{host}

	cases := []struct {
		name    string
		src     string
		wantHit bool
	}{
		{
			name: "plain_literal",
			src: `package x
var _ = "forbidden.example.com"
`,
			wantHit: true,
		},
		{
			name: "unicode_escape",
			// \u0066 is 'f' — source bytes contain the literal
			// 6-char sequence "\u0066", NOT "forbidden". A raw-byte
			// scan looking for "forbidden" misses this; the AST
			// scanner decodes the Go string literal via
			// strconv.Unquote and finds "forbidden.example.com".
			src: `package x
var _ = "\u0066orbidden.example.com"
`,
			wantHit: true,
		},
		{
			name: "hex_escape",
			// \x66 is 'f' — same idea, different escape form.
			src: `package x
var _ = "\x66orbidden.example.com"
`,
			wantHit: true,
		},
		{
			name: "two_piece_concat",
			src: `package x
var _ = "forbidden" + ".example.com"
`,
			wantHit: true,
		},
		{
			name: "three_piece_concat",
			src: `package x
var _ = "forbidden" + "." + "example.com"
`,
			wantHit: true,
		},
		{
			name: "concat_in_call",
			src: `package x
import "fmt"
func _f() string { return fmt.Sprintf("https://" + "forbidden.example.com" + "/v1") }
`,
			wantHit: true,
		},
		{
			name: "case_shifted_literal",
			// DNS is case-insensitive at runtime; case-shifted
			// spellings of the same host should still match.
			src: `package x
var _ = "FORBIDDEN.EXAMPLE.COM"
`,
			wantHit: true,
		},
		{
			name:    "raw_string_literal",
			src:     "package x\nvar _ = `forbidden.example.com`\n",
			wantHit: true,
		},
		{
			name: "in_struct_field",
			src: `package x
type T struct{ URL string }
var _ = T{URL: "forbidden" + ".example.com"}
`,
			wantHit: true,
		},
		{
			name: "parenthesized",
			src: `package x
var _ = ("forbidden" + ".example.com")
`,
			wantHit: true,
		},
		{
			name: "const_declaration_rhs",
			// The const's RHS is a literal expression, so it's
			// caught at the declaration site even without resolving
			// uses by identifier.
			src: `package x
const host = "forbidden.example.com"
var _ = host
`,
			wantHit: true,
		},
		{
			name: "comment_only",
			src: `package x
// forbidden.example.com is referenced in prose, not in a literal
var _ = "harmless"
`,
			wantHit: false,
		},
		{
			name: "identifier_name_only",
			// Variable name resembles the host; no string literal
			// holds it, so no hit.
			src: `package x
var forbiddenExampleCom = "innocuous"
`,
			wantHit: false,
		},
		{
			name: "different_host",
			src: `package x
var _ = "permitted.example.org"
`,
			wantHit: false,
		},
		{
			name: "documented_gap_const_split_then_concat",
			// CURRENTLY NOT CAUGHT: declaring host parts as
			// separate constants and concatenating via identifiers
			// (not literals) sidesteps the AST literal walk. The
			// individual literals "forbidden" and ".example.com"
			// don't each contain the full host substring. Catching
			// this requires constant-resolution via go/types.
			src: `package x
const a = "forbidden"
const b = ".example.com"
var _ = a + b
`,
			wantHit: false,
		},
		{
			name: "documented_gap_byte_slice_synthesis",
			// CURRENTLY NOT CAUGHT: synthesizing the host from a
			// byte slice is not evaluated by the scanner. The
			// module-graph check (layer 1) is the structural fence
			// for this class.
			src: `package x
var _ = string([]byte{
	'f','o','r','b','i','d','d','e','n','.',
	'e','x','a','m','p','l','e','.','c','o','m',
})
`,
			wantHit: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hits, err := scanGoSourceForHosts(tc.name+".go", []byte(tc.src), hosts)
			require.NoError(t, err, "scanner returned an unexpected error")
			if tc.wantHit {
				assert.NotEmptyf(t, hits,
					"expected scanner to flag %s as containing %q; source:\n%s",
					tc.name, host, tc.src)
			} else {
				assert.Emptyf(t, hits,
					"expected scanner to NOT flag %s; got %v; source:\n%s",
					tc.name, hits, tc.src)
			}
		})
	}
}

// TestScanGoSourceForHosts_NoFalsePositiveOnUnrelated is a sanity
// probe: a non-trivial Go file that contains realistic-looking
// strings (URLs, log messages, error text) but no forbidden hosts
// must produce zero hits. Catches a regression where the scanner
// over-matches — e.g., if the case-folding turned into a
// substring-of-substring match that flagged any "api." prefix.
func TestScanGoSourceForHosts_NoFalsePositiveOnUnrelated(t *testing.T) {
	t.Parallel()

	src := `package x

import (
	"fmt"
	"net/http"
)

const userAgent = "signatory/0.1 (+https://github.com/sarah/signatory)"

func fetch(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", "https://api.github.com"+url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	return http.DefaultClient.Do(req)
}
`
	// Deliberately use the real forbidden-host list — none of these
	// should appear in the synthetic source above.
	hits, err := scanGoSourceForHosts("unrelated.go", []byte(src), forbiddenAPIHosts)
	require.NoError(t, err)
	assert.Empty(t, hits, "unrelated source should produce no hits; got %v", hits)
}

// findModuleRoot returns the absolute path of the directory
// containing the enclosing go.mod. Fails the test if not inside a
// module (which shouldn't happen under `go test`, but we check
// anyway so the failure mode is readable).
func findModuleRoot(t *testing.T) string {
	t.Helper()
	// CommandContext bounds the subprocess by the test deadline;
	// go env GOMOD is fast in practice but the discipline is
	// uniform across the test file's exec.* call sites.
	out, err := exec.CommandContext(t.Context(), "go", "env", "GOMOD").Output()
	require.NoError(t, err, "go env GOMOD failed")
	goMod := strings.TrimSpace(string(out))
	require.NotEmpty(t, goMod, "not inside a Go module")
	require.NotEqual(t, "/dev/null", goMod, "GOMOD is /dev/null — not inside a module")
	return filepath.Dir(goMod)
}
