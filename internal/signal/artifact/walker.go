// Package artifact compares the contents of a registry-published
// release artifact (npm tarball, PyPI sdist, Cargo crate, GitHub
// release tarball, ...) against the git tree at the corresponding
// commit, and emits a single artifact_repo_divergence signal
// summarising files present in one source but not the other.
//
// Threat model anchor: CVE-2024-3094 (xz-utils, March 2024). The
// backdoored release tarballs contained a malicious build-to-host.m4
// macro that was absent from the git tree at the same tag. Comparing
// "what was published" against "what's in the source repo" is the
// single highest-leverage missing signal in v0.1's surface area
// (see design/threat-landscape/example-xz-utils-cve-2024-3094.md §1).
//
// Design choices the package commits to:
//
//   - Header-only iteration. We open the tarball stream, read each
//     archive header, record (path, size, mode, type), and let
//     archive/tar advance past the body to the next header. We
//     NEVER io.Copy the entry body to disk. This removes the entire
//     class of tar-slip / zip-slip / symlink-escape vulnerabilities
//     by construction: there is no extraction, so attacker-controlled
//     paths and links are records, not filesystem operations.
//
//   - Decompression cap. The tar format interleaves headers with
//     content; finding header N+1 requires streaming through file
//     N's body. A LimitReader on the gunzipped stream caps total
//     bytes consumed and surfaces ErrArchiveTooLarge as a defined
//     sentinel. CPU is not unbounded; memory is not unbounded.
//
//   - No persistent cache. Tarballs land in os.MkdirTemp and are
//     removed via the orchestrator's opts.Cleanups drain. Re-runs
//     re-download. Tarballs are small (low MB); cache invalidation
//     is more cost than the bandwidth saved.
package artifact

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
)

// ErrArchiveTooLarge indicates the gunzipped stream exceeded the
// caller's WalkOptions.MaxBytes cap. Returned wrapped via fmt.Errorf
// so callers compare with errors.Is rather than equality.
var ErrArchiveTooLarge = errors.New("archive exceeded decompression cap")

// Entry is a header-only record of one tarball entry. No body bytes
// are captured — the walker advances past content without copying it
// anywhere.
//
// Path is the entry name as it appears in the tar header, NOT cleaned
// or filesystem-resolved. Callers that need a normalised path do that
// downstream; here we keep the raw form so suspicious paths (".."
// components, absolute paths, NUL bytes) are visible to the classifier
// rather than silently rewritten.
type Entry struct {
	Path       string
	Size       int64
	Mode       int64
	Type       byte   // tar typeflag: '0' regular, '5' dir, '2' symlink, ...
	LinkTarget string // populated for symlinks and hardlinks; empty otherwise
}

// WalkOptions controls the safety caps applied during iteration.
type WalkOptions struct {
	// MaxBytes caps the total number of GUNZIPPED bytes the walker
	// will read from r. The cap is enforced before any single header
	// or body chunk is consumed, so an over-cap archive returns
	// ErrArchiveTooLarge rather than processing entries up to the
	// cap and silently truncating.
	//
	// Zero means unlimited — useful for tests that want to exercise
	// the walker against a fixture whose size is known and trusted.
	// Production call sites MUST set a positive value.
	MaxBytes int64
}

// Walk reads gzip-compressed tar headers from r and returns the
// entry list. Body bytes are streamed past (counted against the cap)
// but never written anywhere — see the package doc for the rationale.
//
// Returns ErrArchiveTooLarge (wrapped) when MaxBytes is exceeded.
// Returns the wrapped underlying error from gzip / tar on malformed
// input. The returned entries slice contains every header successfully
// read BEFORE the failure; callers that need atomic semantics should
// discard the partial slice on error.
func Walk(r io.Reader, opts WalkOptions) ([]Entry, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("gunzip archive: %w", err)
	}
	defer gz.Close()

	// Wrap the gunzipped stream in a counting reader so we can
	// detect cap exhaustion at any point — header read or body
	// skip — and convert it to ErrArchiveTooLarge regardless of
	// where the over-read happened.
	var src io.Reader = gz
	var counter *cappedReader
	if opts.MaxBytes > 0 {
		counter = &cappedReader{r: gz, cap: opts.MaxBytes}
		src = counter
	}

	tr := tar.NewReader(src)
	var entries []Entry
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return entries, nil
		}
		if err != nil {
			if counter != nil && counter.exceeded {
				return entries, fmt.Errorf("walk archive: %w (cap=%d bytes)",
					ErrArchiveTooLarge, opts.MaxBytes)
			}
			return entries, fmt.Errorf("read tar header: %w", err)
		}
		entries = append(entries, Entry{
			Path:       hdr.Name,
			Size:       hdr.Size,
			Mode:       hdr.Mode,
			Type:       hdr.Typeflag,
			LinkTarget: hdr.Linkname,
		})
	}
}

// cappedReader wraps an io.Reader with a hard byte cap. Reads past
// the cap return ErrArchiveTooLarge so the tar reader's next call
// surfaces a typed failure that Walk converts into the sentinel.
//
// Sets exceeded=true when the cap fires so Walk can distinguish a
// cap-triggered error from a malformed-tar error at the recovery
// site (both come back through tar.Reader.Next()).
type cappedReader struct {
	r        io.Reader
	cap      int64
	read     int64
	exceeded bool
}

func (c *cappedReader) Read(p []byte) (int, error) {
	remaining := c.cap - c.read
	if remaining <= 0 {
		c.exceeded = true
		return 0, ErrArchiveTooLarge
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}
