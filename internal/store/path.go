package store

import (
	"os"
	"path/filepath"
	"strings"
)

// DefaultDBPath returns the default database path (~/.signatory/signatory.db)
// with the home directory properly resolved.
func DefaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".signatory", "signatory.db"), nil
}

// ResolvePath expands ~ in a database path to the user's home directory.
func ResolvePath(path string) (string, error) {
	if path == "" {
		return DefaultDBPath()
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, path[2:]), nil
	}
	if path == "~" {
		return DefaultDBPath()
	}
	return filepath.Abs(path)
}
