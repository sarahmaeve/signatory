package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Detect searches dir for a manifest file signatory knows how to
// parse. Returns the absolute path and the ecosystem slug of the
// first match. Errors when no recognized manifest exists in dir.
//
// Only go.mod is recognized in v0.1. As additional parsers land
// (package.json, requirements.txt, Cargo.toml, ...), the
// recognized-filenames list below grows — the intent is that
// survey "just works" when run from any project root regardless
// of ecosystem.
//
// Order matters: if a directory somehow contains multiple
// manifests (a polyglot monorepo root), the first match wins.
// That's rare enough in v0.1 that it doesn't justify a more
// elaborate picker; callers with multi-manifest projects can
// pass --manifest explicitly.
func Detect(dir string) (path, ecosystem string, err error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", "", fmt.Errorf("resolve dir %q: %w", dir, err)
	}

	// Each entry: (filename, ecosystem slug).
	// Extend this list as new parsers land.
	candidates := []struct {
		file      string
		ecosystem string
	}{
		{"go.mod", "go"},
	}

	for _, c := range candidates {
		p := filepath.Join(absDir, c.file)
		info, err := os.Stat(p)
		if err == nil && !info.IsDir() {
			return p, c.ecosystem, nil
		}
	}

	return "", "", fmt.Errorf("no recognized manifest in %s (looked for: %s)",
		absDir, candidateNames(candidates))
}

// candidateNames produces a human-readable list of the filenames
// Detect checks, used in the "no manifest found" error. Kept
// separate so the error message stays accurate when the list
// grows.
func candidateNames(candidates []struct {
	file      string
	ecosystem string
}) string {
	names := make([]string, 0, len(candidates))
	for _, c := range candidates {
		names = append(names, c.file)
	}
	return strings.Join(names, ", ")
}
