package invariants

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Invariant 1 (design/v0.1-invariants.md): signatory's Go code must
// not depend on any LLM-client SDK, and must not contain a string
// literal pointing at a known LLM-provider API host. Violations of
// either check fail the test suite.
//
// Two layers of defense:
//
//  1. TestNoLLMClientInModuleGraph inspects `go list -m -json all`.
//     That catches every direct dependency and every transitive one
//     (including dependencies pulled in by a `replace` directive).
//  2. TestNoLLMAPIHostsInSource walks all .go files and scans for
//     literal provider hostnames. That catches the bypass case of an
//     ad-hoc `net/http` call that skips the SDK entirely.
//
// Together they express: "no SDK, and no DIY client either." Both
// are needed — either alone leaves a gap.

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
	cmd := exec.Command("go", "list", "-m", "-json", "all")
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
// module root and fails if any contains a literal forbidden LLM
// API hostname. This catches the case where someone bypasses the
// SDK by calling net/http directly against, e.g., api.anthropic.com.
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
		text := string(data)
		rel, _ := filepath.Rel(moduleRoot, path)
		for _, host := range forbiddenAPIHosts {
			if strings.Contains(text, host) {
				violations = append(violations, rel+": "+host)
			}
		}
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

// findModuleRoot returns the absolute path of the directory
// containing the enclosing go.mod. Fails the test if not inside a
// module (which shouldn't happen under `go test`, but we check
// anyway so the failure mode is readable).
func findModuleRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	require.NoError(t, err, "go env GOMOD failed")
	goMod := strings.TrimSpace(string(out))
	require.NotEmpty(t, goMod, "not inside a Go module")
	require.NotEqual(t, "/dev/null", goMod, "GOMOD is /dev/null — not inside a module")
	return filepath.Dir(goMod)
}
