package htmlreport

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sarahmaeve/signatory/internal/exchange"
)

// WriteReportTreeInput bundles every parameter the directory writer
// needs. The writer is the seam where the pure renderers meet the
// filesystem; all I/O happens here.
type WriteReportTreeInput struct {
	// ParentDir is the existing parent directory the auto-named
	// report subdirectory will be created inside. Must exist and be
	// writeable. The writer never creates parents (no MkdirAll on
	// ParentDir itself); operators choose where reports land and
	// signatory honours that.
	ParentDir string

	// Synth is the synthesis AnalystOutput. Must carry a non-nil
	// SynthesisSupplement.
	Synth *exchange.AnalystOutput

	// SynthesisOutputID is the synthesis row id. Used to disambiguate
	// the auto-named subdir (`<short>-<id-short>`). Required.
	SynthesisOutputID string

	// ShortName is the human-friendly identifier the index renderer
	// puts in the title and the directory writer uses as the subdir
	// slug prefix.
	ShortName string

	// Loaded is the analyst-output set discovered from
	// distinct(KeyConclusionRefs[].OutputID), keyed by output id.
	Loaded map[string]*exchange.AnalystOutput

	// TargetURL is the resolved external registry URL for Synth.Target,
	// passed through to the index renderer. Empty disables the link.
	TargetURL string

	// RecordedPosture is the entity's current posture from signatory's
	// store, if any. Optional — the index renders its block only when
	// non-nil.
	RecordedPosture *RecordedPosture

	// GeneratedAt and Version flow into every page footer.
	GeneratedAt string
	Version     string
}

// WriteReportTree materializes the report directory under ParentDir
// and returns the absolute path to the generated index.html.
//
// Refusal conditions (no --force in v0.1):
//   - ParentDir does not exist or is not a directory.
//   - The auto-named subdir already exists inside ParentDir.
//   - ParentDir is not writeable (surfaces as a Mkdir error on the
//     subdir creation).
//
// Success contract:
//   - Returns an absolute path to <ParentDir>/<subdir>/index.html.
//   - <subdir> contains conclusions/, analysts/, and assets/ trees
//     plus index.html, all as documented in design §4.
//   - assets/style.css byte-equals the embedded source.
func WriteReportTree(in WriteReportTreeInput) (string, error) {
	if in.Synth == nil || in.Synth.SynthesisSupplement == nil {
		return "", fmt.Errorf("WriteReportTree: input has no synthesis_supplement")
	}

	// Refuse-if-parent-missing: stat first so the error is
	// distinguishable from a permissions failure.
	pi, err := os.Stat(in.ParentDir)
	if err != nil {
		return "", fmt.Errorf("parent directory %q: %w", in.ParentDir, err)
	}
	if !pi.IsDir() {
		return "", fmt.Errorf("parent path %q is not a directory", in.ParentDir)
	}

	subdirName := buildSubdirName(in.ShortName, in.SynthesisOutputID)
	subdir := filepath.Join(in.ParentDir, subdirName)

	// Refuse-if-subdir-exists. We check before creating so the error
	// names the colliding path explicitly. Mkdir would also fail with
	// EEXIST, but the wrapped message is less specific.
	if _, err := os.Stat(subdir); err == nil {
		return "", fmt.Errorf("report subdirectory %q already exists; remove it or choose a different parent", subdirName)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat subdir %q: %w", subdir, err)
	}

	// Create the tree. Mkdir (not MkdirAll) on the subdir surfaces
	// EEXIST and EACCES distinctly and prevents accidental creation
	// of intermediate dirs the operator didn't ask for.
	if err := os.Mkdir(subdir, 0o755); err != nil {
		return "", fmt.Errorf("create subdir: %w", err)
	}
	for _, child := range []string{"conclusions", "analysts", "assets"} {
		if err := os.Mkdir(filepath.Join(subdir, child), 0o755); err != nil {
			return "", fmt.Errorf("create %s/: %w", child, err)
		}
	}

	// Build the link plan once; every renderer reads from it.
	plan := BuildLinkPlan(in.Synth, in.Loaded)

	page := PageContext{
		// Index sits at the report root, so its RootPrefix is empty.
		// Conclusion and analyst pages live one directory deep, so
		// their prefix is "../" — applied per-renderer below.
		GeneratedAt: in.GeneratedAt,
		Version:     in.Version,
	}

	// 1. Index page.
	if err := writeFile(filepath.Join(subdir, "index.html"), func(f *os.File) error {
		return RenderSynthesisIndex(f, SynthesisIndexInput{
			Synth:           in.Synth,
			ShortName:       in.ShortName,
			TargetURL:       in.TargetURL,
			RecordedPosture: in.RecordedPosture,
			Plan:            plan,
			GeneratedAt:     page.GeneratedAt,
			Version:         page.Version,
		})
	}); err != nil {
		return "", fmt.Errorf("write index.html: %w", err)
	}

	// 2. One analyst page per loaded output.
	for outputID, ao := range in.Loaded {
		if ao == nil {
			continue
		}
		analystPath := plan.AnalystPagePath(ao.Attribution.AnalystID)
		if analystPath == "" {
			// Defensive: BuildLinkPlan populates AnalystPages for
			// every loaded output, so this should never trigger.
			continue
		}
		if err := writeFile(filepath.Join(subdir, analystPath), func(f *os.File) error {
			return RenderAnalystPage(f, AnalystPageInput{
				Output:   ao,
				OutputID: outputID,
				Plan:     plan,
				Page: PageContext{
					RootPrefix:  "../",
					GeneratedAt: page.GeneratedAt,
					Version:     page.Version,
				},
			})
		}); err != nil {
			return "", fmt.Errorf("write analyst page %s: %w", analystPath, err)
		}
	}

	// 3. One conclusion page per resolvable reference.
	for key, relPath := range plan.ConclusionPages {
		ao, ok := in.Loaded[key.OutputID]
		if !ok || ao == nil {
			// Plan invariant: keys appear iff the conclusion was
			// found in a loaded output. This is defensive belt-and-
			// braces against a future BuildLinkPlan bug.
			continue
		}
		c := findConclusion(ao, key.LocalID)
		if c == nil {
			continue
		}
		if err := writeFile(filepath.Join(subdir, relPath), func(f *os.File) error {
			return RenderConclusionPage(f, ConclusionPageInput{
				Conclusion:      c,
				OutputID:        key.OutputID,
				Analyst:         ao.Attribution,
				AnalystPagePath: plan.AnalystPagePath(ao.Attribution.AnalystID),
				Plan:            plan,
				Page: PageContext{
					RootPrefix:  "../",
					GeneratedAt: page.GeneratedAt,
					Version:     page.Version,
				},
			})
		}); err != nil {
			return "", fmt.Errorf("write conclusion page %s: %w", relPath, err)
		}
	}

	// 4. One stub page per Dangling reference. Slug mirrors the
	// resolvable case so the synthesis index's plain-text fallback
	// could in theory be replaced with a stub link in a later
	// iteration without slug churn.
	for _, d := range plan.Dangling {
		stubRel := conclusionPageSlug(d.OutputID, d.LocalID)
		// Try to find the parent analyst page for the back-link;
		// skip when the dangling output wasn't loaded at all.
		var analystBack string
		if ao, ok := in.Loaded[d.OutputID]; ok && ao != nil {
			analystBack = plan.AnalystPagePath(ao.Attribution.AnalystID)
		}
		if err := writeFile(filepath.Join(subdir, stubRel), func(f *os.File) error {
			return RenderConclusionStub(f, ConclusionStubInput{
				Ref:             d,
				AnalystPagePath: analystBack,
				Page: PageContext{
					RootPrefix:  "../",
					GeneratedAt: page.GeneratedAt,
					Version:     page.Version,
				},
			})
		}); err != nil {
			return "", fmt.Errorf("write stub page %s: %w", stubRel, err)
		}
	}

	// 5. Copy embedded assets verbatim.
	if err := copyAssets(filepath.Join(subdir, "assets")); err != nil {
		return "", fmt.Errorf("copy assets: %w", err)
	}

	indexPath, err := filepath.Abs(filepath.Join(subdir, "index.html"))
	if err != nil {
		return "", fmt.Errorf("resolve index path: %w", err)
	}
	return indexPath, nil
}

// buildSubdirName produces "<slug-of-shortname>-<id-short>". Slugs
// substitute "/", ":", "@", "+", " " with "-" so PURL-shaped short
// names round-trip into safe filesystem paths. The id-short suffix
// disambiguates two reports for the same entity.
func buildSubdirName(shortName, outputID string) string {
	slug := slugify(shortName)
	if slug == "" {
		slug = "report"
	}
	return slug + "-" + shortOutputID(outputID)
}

// slugify replaces every non-alphanumeric, non-dot, non-hyphen
// character with "-". Keeps the rule simple and predictable; the
// output is always safe on macOS, Linux, and (for v0.1's narrow
// scope) Windows.
func slugify(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// findConclusion returns a pointer into ao.Conclusions matching
// localID, or nil. Linear scan; conclusion lists are small.
func findConclusion(ao *exchange.AnalystOutput, localID string) *exchange.Conclusion {
	for i := range ao.Conclusions {
		if ao.Conclusions[i].ID == localID {
			return &ao.Conclusions[i]
		}
	}
	return nil
}

// writeFile wraps the open / defer-close / write / sync pattern so
// the renderers stay one-call simple. The fn callback receives the
// open file; if it returns an error, the partial file is left in
// place — the directory writer's error path returns immediately and
// the operator removes the subdir on retry.
func writeFile(path string, fn func(*os.File) error) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // close error not actionable when the write succeeded
	return fn(f)
}

// copyAssets walks the embedded assets FS and writes every entry
// into dst. Phase A's assets is just style.css, but the walk shape
// stays correct if a later phase adds e.g. a logo or a print.css.
func copyAssets(dst string) error {
	return fs.WalkDir(AssetsFS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		out := filepath.Join(dst, p)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := fs.ReadFile(AssetsFS(), p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
}
