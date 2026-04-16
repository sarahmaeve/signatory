package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ConfigFileName is the conventional filename signatory looks for at
// the project root. Exposed as a constant so both the discovery
// helper and `signatory init` (which writes the file) stay in sync.
const ConfigFileName = "signatory.config.toml"

// Config holds the parsed contents of a signatory.config.toml file.
// All fields are optional. A zero Config is valid and equivalent to
// "no config file present" — the resolver falls through to built-in
// defaults.
type Config struct {
	// Source is the absolute path the config was loaded from. Empty
	// when the Config was synthesized because no file was present.
	Source string

	// Templates lists extra template directories to consult before
	// the ./templates/ default. Entries that do not exist or are not
	// readable are silently skipped at resolution time — a config is
	// allowed to reference optional locations.
	Templates []string

	// Filestores lists preferred output directories for filestore
	// artifacts (e.g., analyst-output JSONs written by tools that
	// choose not to default to stdout). Tried in order; the first
	// writable directory wins.
	Filestores []string
}

// LoadConfig reads and parses a config file at path. A missing file
// returns (nil, error) with a wrapped fs.ErrNotExist — callers that
// want "absent is fine" semantics should use DiscoverAndLoad instead.
func LoadConfig(path string) (*Config, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path %s: %w", path, err)
	}
	// path is the user's explicit --config argument (kong's type:"path"
	// validates it is a real filesystem path); LoadConfig's purpose is
	// exactly to open that path. The G304 threat model — traversal into
	// an unintended file — doesn't apply when the caller IS asking us
	// to open exactly this file.
	f, err := os.Open(absPath) //nolint:gosec // G304: path is the user's explicit --config argument (kong's type:"path"); LoadConfig's purpose IS to open that file
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only config file close; errors here are not actionable after the decode below

	raw, err := decodeTOML(f)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", absPath, err)
	}

	cfg := &Config{Source: absPath}
	if err := cfg.populateFromRaw(absPath, raw); err != nil {
		return nil, err
	}
	return cfg, nil
}

// DiscoverAndLoad looks for ConfigFileName in dir and returns the
// parsed config, or an empty *Config when the file is not present.
// Any error other than "not exist" (permission denied, parse errors,
// I/O errors) propagates.
//
// This is the right entry point for command implementations: they
// want "use the config if the user has one, otherwise proceed with
// defaults." Missing files are the common case.
func DiscoverAndLoad(dir string) (*Config, error) {
	candidate := filepath.Join(dir, ConfigFileName)
	_, err := os.Stat(candidate)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat config: %w", err)
	}
	return LoadConfig(candidate)
}

// populateFromRaw validates the top-level key set and materializes
// the fields onto cfg. Unknown keys are a hard error: signatory's
// config schema is small and typoed keys should surface immediately
// rather than silently degrade to defaults.
func (cfg *Config) populateFromRaw(path string, raw map[string]rawValue) error {
	for k, v := range raw {
		switch k {
		case "templates":
			if !v.IsArray {
				return fmt.Errorf("%s line %d: %q must be an array of strings (e.g., templates = [\"/path/one\", \"/path/two\"])", path, v.Line, k)
			}
			cfg.Templates = v.Array
		case "filestores":
			if !v.IsArray {
				return fmt.Errorf("%s line %d: %q must be an array of strings (e.g., filestores = [\"/path/one\"])", path, v.Line, k)
			}
			cfg.Filestores = v.Array
		default:
			return fmt.Errorf("%s line %d: unknown key %q (valid keys: templates, filestores)", path, v.Line, k)
		}
	}
	return nil
}
