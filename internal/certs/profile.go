package certs

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// WriteProfileOptions drives WriteProfile. ProfilePath defaults to
// DefaultShellProfile (~/.zshrc) when empty. CAPath is the value
// NODE_EXTRA_CA_CERTS should be set to — typically the stable path
// returned by InitResult.CAPath.
type WriteProfileOptions struct {
	ProfilePath string
	CAPath      string
}

// WriteProfileResult reports what WriteProfile did. Action is one
// of the ProfileAction constants; callers that want to be quiet on
// the unchanged path can skip logging when Action == ProfileUnchanged.
type WriteProfileResult struct {
	ProfilePath string
	Action      ProfileAction
}

// WriteProfile appends or updates signatory's managed block in the
// user's shell profile. The block is bracketed with ProfileMarker-
// Begin / End so re-running replaces the block atomically — no
// duplicate exports, no stale values left behind.
//
// Writes via temp-file + rename for crash safety. An interrupted
// write leaves the previous profile content intact rather than a
// half-written dotfile that would break shell startup.
//
// Idempotency contract:
//
//   - First run on a profile without the block: ProfileAppended.
//   - Profile doesn't exist yet: ProfileCreated.
//   - Subsequent run, same CAPath: ProfileUnchanged (no disk write).
//   - Subsequent run, different CAPath: ProfileReplaced.
//
// WriteProfile does NOT create parent directories. The home dir is
// assumed to exist; if it doesn't, something much more broken is
// going on than a cert env var.
func WriteProfile(opts WriteProfileOptions) (*WriteProfileResult, error) {
	if opts.CAPath == "" {
		return nil, errors.New("WriteProfile: CAPath must be non-empty")
	}
	profilePath := opts.ProfilePath
	if profilePath == "" {
		profilePath = DefaultShellProfile
	}
	resolved, err := expandHome(profilePath)
	if err != nil {
		return nil, fmt.Errorf("resolve profile path %q: %w", profilePath, err)
	}

	result := &WriteProfileResult{ProfilePath: resolved}

	existing, existed, err := readIfExists(resolved)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", resolved, err)
	}

	newBlock := renderManagedBlock(opts.CAPath)

	updated, action := updateProfileContent(existing, newBlock, existed)

	// Skip the disk write entirely on the unchanged path so mtime
	// doesn't drift every `certs init` invocation. Users notice
	// when a "no-op" command touches their dotfiles.
	if action == ProfileUnchanged {
		result.Action = ProfileUnchanged
		return result, nil
	}

	if err := writeFileAtomic(resolved, []byte(updated), 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", resolved, err)
	}
	result.Action = action
	return result, nil
}

// renderManagedBlock returns the exact text (including trailing
// newline) that should live between the begin/end markers.
func renderManagedBlock(caPath string) string {
	return fmt.Sprintf("%s\nexport %s=%q\n%s\n",
		ProfileMarkerBegin, EnvVar, caPath, ProfileMarkerEnd)
}

// updateProfileContent computes the new profile contents and the
// action taken. Pure function — no I/O — so it's cheap to unit-
// test and easy to reason about.
func updateProfileContent(existing, newBlock string, fileExisted bool) (string, ProfileAction) {
	begin := strings.Index(existing, ProfileMarkerBegin)
	if begin < 0 {
		// No managed block present. Decide Created vs Appended.
		if !fileExisted {
			return newBlock, ProfileCreated
		}
		if existing == "" {
			return newBlock, ProfileAppended
		}
		// Ensure at least one separating newline between user
		// content and the managed block — avoids visual collision
		// if the user's last line has no trailing newline.
		sep := ""
		if !strings.HasSuffix(existing, "\n") {
			sep = "\n"
		}
		return existing + sep + newBlock, ProfileAppended
	}

	// Block present. Find end marker on or after Begin.
	endIdx := strings.Index(existing[begin:], ProfileMarkerEnd)
	if endIdx < 0 {
		// Malformed — begin present but no end. Treat as a stale
		// marker; overwrite from begin to EOF with the new block.
		// This is the graceful recovery path for a user who edited
		// the file by hand and accidentally broke the closing
		// marker.
		return existing[:begin] + newBlock, ProfileReplaced
	}
	endIdx += begin + len(ProfileMarkerEnd)

	// Consume one trailing newline after the end marker so
	// replacement doesn't accumulate blank lines across
	// regenerations.
	consumeEnd := endIdx
	if consumeEnd < len(existing) && existing[consumeEnd] == '\n' {
		consumeEnd++
	}

	before := existing[:begin]
	after := existing[consumeEnd:]
	replaced := before + newBlock + after

	if replaced == existing {
		return existing, ProfileUnchanged
	}
	return replaced, ProfileReplaced
}

// readIfExists returns the contents of path along with a flag
// indicating whether the file existed. Missing-file is not an
// error — it's the "create" path.
func readIfExists(path string) (content string, existed bool, err error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: profile path is either default (~/.zshrc) or user-supplied via --shell-profile-path; scope is their own shell config
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	return string(data), true, nil
}

// writeFileAtomic writes via temp-file + rename so a crash mid-
// write can't leave the user with a truncated dotfile. Perm
// argument is applied to the temp file pre-rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".signatory.tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
