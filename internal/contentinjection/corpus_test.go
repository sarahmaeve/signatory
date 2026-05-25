package contentinjection

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCorpus_CognitiveHazards is the corpus-driven verification of
// the primitive scanners. It walks testdata/cognitivehazards/, runs
// Scan against each fixture, and asserts the result matches the
// expectation encoded by the fixture's directory + filename suffix.
//
// Layout and convention are documented in
// testdata/cognitivehazards/README.md. The short version:
//
//   - <primitive>/<file>.malicious.* must fire that primitive.
//   - <primitive>/<file>.benign.* must NOT fire that primitive.
//   - composite/<file>.malicious.* must fire at least one primitive
//     (optionally an .expected.json sidecar lists the exact set).
//   - benign_baseline/<file>.benign.* must fire ZERO primitives.
//
// A failure here either reveals a real detection gap (the wild
// shape eludes our model) or means the fixture isn't representative
// (the corpus needs a stronger example or the annotation is wrong).
// Either is information.
func TestCorpus_CognitiveHazards(t *testing.T) {
	root := filepath.Join("testdata", "cognitivehazards")

	// Map of subdirectory name -> expected primitive when the
	// fixture's outcome suffix is .malicious.
	primitiveByDir := map[string]Primitive{
		"invisible_unicode":      PrimitiveInvisibleUnicode,
		"bidi_control":           PrimitiveBidiControl,
		"tag_block":              PrimitiveTagBlock,
		"markdown_comment":       PrimitiveMarkdownComment,
		"markdown_image":         PrimitiveMarkdownImage,
		"lexical_injection":      PrimitiveLexicalInjection,
		"encoded_blob":           PrimitiveEncodedBlob,
		"confusable_mixedscript": PrimitiveConfusableMixedScript,
	}

	entries, err := os.ReadDir(root)
	require.NoError(t, err, "corpus root must be readable")

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := entry.Name()
		t.Run(dir, func(t *testing.T) {
			t.Parallel()
			fixtures, err := corpusFixtures(filepath.Join(root, dir))
			require.NoError(t, err)
			for _, fx := range fixtures {
				t.Run(fx.relname, func(t *testing.T) {
					t.Parallel()
					verifyFixture(t, fx, dir, primitiveByDir)
				})
			}
		})
	}
}

// fixture is one corpus file plus the outcome encoded in its name.
type fixture struct {
	path     string // absolute or relative path usable with ScanFile
	relname  string // basename for test naming
	outcome  outcome
	expected []Primitive // composite/-only: from sidecar .expected.json
}

type outcome int

const (
	outcomeUnknown outcome = iota
	outcomeMalicious
	outcomeBenign
)

// corpusFixtures lists the fixture files in a corpus subdirectory.
// README.md and .expected.json files are skipped; everything else
// is interpreted via its filename suffix.
func corpusFixtures(dir string) ([]fixture, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []fixture
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "README.md" || strings.HasSuffix(name, ".expected.json") {
			continue
		}
		fx := fixture{
			path:    filepath.Join(dir, name),
			relname: name,
			outcome: outcomeFromName(name),
		}
		if fx.outcome == outcomeUnknown {
			return nil, fmt.Errorf("fixture %q in %q has no .malicious. or .benign. suffix",
				name, dir)
		}
		if fx.outcome == outcomeMalicious {
			expected, err := loadExpectedSidecar(fx.path)
			if err != nil {
				return nil, err
			}
			fx.expected = expected
		}
		out = append(out, fx)
	}
	return out, nil
}

func outcomeFromName(name string) outcome {
	// Match the *.malicious.* / *.benign.* infix. Last-dot is
	// the file extension, second-to-last is the outcome marker.
	parts := strings.Split(name, ".")
	for _, p := range parts[:len(parts)-1] {
		switch p {
		case "malicious":
			return outcomeMalicious
		case "benign":
			return outcomeBenign
		}
	}
	return outcomeUnknown
}

// loadExpectedSidecar reads <fixture>.expected.json if present.
// Used by composite/ fixtures to specify exactly which primitives
// the malicious fixture must fire.
func loadExpectedSidecar(fixturePath string) ([]Primitive, error) {
	sidecar := fixturePath + ".expected.json"
	data, err := os.ReadFile(sidecar) //nolint:gosec // G304: corpus-controlled paths
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var spec struct {
		Primitives []Primitive `json:"primitives"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse %s: %w", sidecar, err)
	}
	return spec.Primitives, nil
}

// verifyFixture runs Scan against the fixture and asserts the
// directory/outcome expectation.
func verifyFixture(t *testing.T, fx fixture, dir string, primitiveByDir map[string]Primitive) {
	t.Helper()
	res, err := ScanFile(fx.path)
	require.NoError(t, err, "ScanFile must succeed on fixture %s", fx.path)

	switch dir {
	case "benign_baseline":
		assert.Empty(t, res.Findings,
			"benign_baseline fixture %s must produce zero findings; got: %v",
			fx.relname, summarize(res.Findings))
	case "composite":
		verifyComposite(t, fx, res)
	default:
		expectedPrimitive, ok := primitiveByDir[dir]
		require.True(t, ok,
			"corpus subdirectory %q has no expected-primitive mapping in the harness", dir)
		verifyPrimitiveScoped(t, fx, expectedPrimitive, res)
	}
}

// verifyPrimitiveScoped enforces the per-primitive subdirectory
// contract: malicious fixtures must fire the directory's primitive;
// benign fixtures must NOT fire it (other primitives are free to
// fire or not — this directory's job is only that one primitive).
func verifyPrimitiveScoped(t *testing.T, fx fixture, expected Primitive, res ScanResult) {
	t.Helper()
	fired := firedPrimitives(res)
	_, present := fired[expected]
	switch fx.outcome {
	case outcomeMalicious:
		assert.True(t, present,
			"fixture %s must fire primitive %s; got findings: %v",
			fx.relname, expected, summarize(res.Findings))
	case outcomeBenign:
		assert.False(t, present,
			"fixture %s must NOT fire primitive %s; got findings: %v",
			fx.relname, expected, summarize(res.Findings))
	}
}

// verifyComposite enforces the composite/ subdirectory contract:
// malicious fixtures must fire at least one primitive (or, if a
// .expected.json sidecar is present, the exact primitives listed
// in it). Benign fixtures must fire zero primitives.
func verifyComposite(t *testing.T, fx fixture, res ScanResult) {
	t.Helper()
	if fx.outcome == outcomeBenign {
		assert.Empty(t, res.Findings,
			"composite benign fixture %s must produce zero findings; got: %v",
			fx.relname, summarize(res.Findings))
		return
	}
	if len(fx.expected) > 0 {
		fired := firedPrimitives(res)
		for _, p := range fx.expected {
			_, present := fired[p]
			assert.True(t, present,
				"composite fixture %s expected primitive %s per sidecar; got findings: %v",
				fx.relname, p, summarize(res.Findings))
		}
		return
	}
	assert.NotEmpty(t, res.Findings,
		"composite malicious fixture %s must fire at least one primitive", fx.relname)
}

func firedPrimitives(res ScanResult) map[Primitive]struct{} {
	out := make(map[Primitive]struct{}, len(res.Findings))
	for _, f := range res.Findings {
		out[f.Primitive] = struct{}{}
	}
	return out
}

func summarize(findings []Finding) string {
	if len(findings) == 0 {
		return "(empty)"
	}
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		parts = append(parts, fmt.Sprintf("%s×%d", f.Primitive, f.Count))
	}
	return strings.Join(parts, ", ")
}
