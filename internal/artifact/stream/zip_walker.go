package stream

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// symlinkMaxBytes bounds how many bytes the walker reads from a
// zip-encoded symlink entry to populate Entry.LinkTarget. Real
// symlinks are well under 1 KiB (filesystems cap PATH_MAX around
// 4096); 4 KiB is generous and aligns with Linux's typical
// PATH_MAX. A symlink whose body exceeds this is itself anomalous
// and gets recorded with an empty LinkTarget rather than failing
// the walk.
const symlinkMaxBytes int64 = 4 * 1024

// gpFlagEncrypted is bit 0 of zip's general-purpose flag word: an
// entry with this bit set is encrypted (per PKZIP appnote 4.4.4).
// We can't decompress encrypted entries — and their presence in a
// release artifact is itself anomalous — so the walker rejects.
const gpFlagEncrypted uint16 = 0x0001

// walkZip implements Walk for FormatZip. Buffers the compressed
// input under MaxCompressedBytes (zip's central-directory-at-end
// design forces random access), then:
//
//  1. Hashes the raw bytes for ArchiveSHA256.
//  2. Cross-checks local file headers against the central directory.
//     A count or name mismatch fails with ErrParserConfusion — the
//     known supply-chain attack class where different zip parsers
//     (Go vs. Python vs. Java) see different file lists.
//  3. Pre-flights cap checks against central-directory metadata
//     before any decompression.
//  4. Walks central-directory entries, classifying each by mode bits
//     and matching CaptureIntents. Bodies are NOT opened except for
//     symlink target capture and explicit intent matches.
//  5. Detects the common top-level directory.
func walkZip(ctx context.Context, src io.Reader,
	intents []CaptureIntent, lim Limits) (*Manifest, error) {

	lim = resolveLimits(lim)

	// Buffer compressed input under the hard ceiling. A reader that
	// keeps producing past the cap returns ErrLimitExceeded before
	// any zip parsing starts.
	raw, err := readCappedAll(src, lim.MaxCompressedBytes)
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(raw)
	archiveSHA := hex.EncodeToString(sum[:])

	// Local-header / central-directory cross-check. Best-effort —
	// scanner returning errCrossCheckUnscanable means "ZIP64 or data
	// descriptors present, can't reliably scan"; we then skip the
	// cross-check and rely on stdlib's central-only validation.
	// Other errors from the scanner indicate malformed local-header
	// structure: surface as malformed-zip via the central parse
	// (don't double-report from here).
	locals, scanErr := scanLocalHeaders(raw)
	if scanErr != nil && !errors.Is(scanErr, errCrossCheckUnscanable) {
		locals = nil
	}

	// Encryption-bit detection on local-header side. A confused
	// archive could set the bit only on the local header (or only on
	// the central entry); checking both sides catches both shapes.
	for _, l := range locals {
		if l.flags&gpFlagEncrypted != 0 {
			return nil, fmt.Errorf("%w: %q (local header)", ErrEncryptedEntry, l.name)
		}
	}

	// archive/zip requires int64 size — readCappedAll bounds raw to
	// MaxCompressedBytes, which is itself an int64 cap, so this is
	// statically safe.
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("stream: zip parse: %w", err)
	}

	if locals != nil && !errors.Is(scanErr, errCrossCheckUnscanable) {
		if err := crossCheckCounts(locals, zr.File); err != nil {
			return nil, err
		}
	}

	// Pre-flight: entry count, per-entry size, total size, ratio.
	// All four caps fire before any per-entry decompression so we
	// never spend CPU on an archive we'd reject anyway.
	if int64(len(zr.File)) > int64(lim.MaxEntries) {
		return nil, fmt.Errorf("%w: entry count %d > %d",
			ErrLimitExceeded, len(zr.File), lim.MaxEntries)
	}

	var totalUncompressed, totalCompressed int64
	for _, f := range zr.File {
		if f.Flags&gpFlagEncrypted != 0 {
			return nil, fmt.Errorf("%w: %q (central entry)", ErrEncryptedEntry, f.Name)
		}
		unc, ok := zipSizeAsInt64(f.UncompressedSize64)
		if !ok {
			// Beyond int64 — guaranteed past any reasonable cap.
			return nil, fmt.Errorf("%w: entry %q uncompressed size %d > %d (int64 ceiling)",
				ErrLimitExceeded, f.Name, f.UncompressedSize64, lim.MaxEntryBytes)
		}
		if unc > lim.MaxEntryBytes {
			return nil, fmt.Errorf("%w: entry %q size %d > %d",
				ErrLimitExceeded, f.Name, unc, lim.MaxEntryBytes)
		}
		totalUncompressed += unc
		if totalUncompressed > lim.MaxTotalBytes {
			return nil, fmt.Errorf("%w: total uncompressed exceeds %d",
				ErrLimitExceeded, lim.MaxTotalBytes)
		}
		comp, ok := zipSizeAsInt64(f.CompressedSize64)
		if !ok {
			return nil, fmt.Errorf("%w: entry %q compressed size %d > int64 ceiling",
				ErrLimitExceeded, f.Name, f.CompressedSize64)
		}
		totalCompressed += comp
	}

	// Compression-ratio defense. Only meaningful past the same
	// thresholds the tar walker uses (small inputs produce noisy
	// ratios that would yield false positives on legitimate archives).
	if totalCompressed >= ratioMinCompressedBytes &&
		totalUncompressed >= ratioMinDecompressedBytes {
		ratio := float64(totalUncompressed) / float64(totalCompressed)
		if ratio > lim.MaxCompressionRatio {
			return nil, fmt.Errorf("%w: compression ratio %.0f exceeds cap %.0f "+
				"(decompressed=%d compressed=%d)",
				ErrLimitExceeded, ratio, lim.MaxCompressionRatio,
				totalUncompressed, totalCompressed)
		}
	}

	manifest := &Manifest{
		Captured:               map[string][]byte{},
		SkippedIntents:         map[string]string{},
		ArchiveSHA256:          archiveSHA,
		TotalUncompressedBytes: totalUncompressed,
	}

	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// Already pre-flight-validated; this conversion is safe.
		size, _ := zipSizeAsInt64(f.UncompressedSize64)

		entry := Entry{
			Path: f.Name,
			Size: size,
			// External attributes are uint32; record verbatim as int64.
			// See Entry.Mode docstring for the "evidence not enforcement"
			// rationale — losing high bits would hide diagnostic data.
			Mode: int64(f.ExternalAttrs),
			Type: classifyZipEntry(f),
		}

		if entry.Type == EntrySymlink {
			target, err := readZipSymlinkTarget(f, symlinkMaxBytes)
			if err != nil {
				return nil, fmt.Errorf("stream: read symlink target for %q: %w",
					f.Name, err)
			}
			entry.LinkTarget = target
		}

		manifest.Entries = append(manifest.Entries, entry)

		// Capture-intent dispatch. Same first-match-wins / oversize-skip /
		// duplicate-skip semantics as the tar walker — see that walker's
		// inner loop for the rationale.
		if entry.Type != EntryFile || size == 0 {
			continue
		}
		for _, intent := range intents {
			if !intent.Match(entry) {
				continue
			}
			if _, exists := manifest.Captured[intent.Name]; exists {
				manifest.SkippedIntents[intent.Name] = fmt.Sprintf(
					"duplicate match: %q (first match already captured)", entry.Path)
				continue
			}
			if _, exists := manifest.SkippedIntents[intent.Name]; exists {
				continue
			}
			if size > intent.MaxSize {
				manifest.SkippedIntents[intent.Name] = fmt.Sprintf(
					"oversize: entry %d bytes > intent cap %d",
					size, intent.MaxSize)
				continue
			}
			body, err := readZipEntryBody(f, size)
			if err != nil {
				return nil, fmt.Errorf("stream: read body for capture intent %q: %w",
					intent.Name, err)
			}
			manifest.Captured[intent.Name] = body
			break
		}
	}

	manifest.StrippedTopDir = detectCommonTopDir(manifest.Entries)
	return manifest, nil
}

// crossCheckCounts compares local-header records against central-
// directory entries and returns ErrParserConfusion on any disagreement
// in count or name set. Detection-only — does not enrich the manifest;
// any mismatch means we don't trust the archive enough to walk it.
func crossCheckCounts(locals []localEntry, central []*zip.File) error {
	if len(locals) != len(central) {
		return fmt.Errorf("%w: %d local headers vs %d central entries",
			ErrParserConfusion, len(locals), len(central))
	}
	centralNames := make(map[string]int, len(central))
	for _, f := range central {
		centralNames[f.Name]++
	}
	for _, l := range locals {
		if centralNames[l.name] == 0 {
			return fmt.Errorf("%w: local header %q has no matching central entry",
				ErrParserConfusion, l.name)
		}
		centralNames[l.name]--
	}
	return nil
}

// classifyZipEntry maps a zip file's mode bits + name convention to
// the package's archive-format-agnostic EntryType. Zip stores type
// information differently from tar — symlinks via Unix mode bits in
// ExternalAttrs, directories via name-ends-with-"/" convention.
func classifyZipEntry(f *zip.File) EntryType {
	mode := f.Mode()
	if mode&os.ModeSymlink != 0 {
		return EntrySymlink
	}
	if strings.HasSuffix(f.Name, "/") || mode.IsDir() {
		return EntryDir
	}
	if mode.IsRegular() {
		return EntryFile
	}
	return EntryOther
}

// readZipSymlinkTarget reads the body of a zip-encoded symlink entry
// (target text lives in the file content, not the header) up to a
// hard cap. Symlinks larger than the cap return an empty target with
// no error — the entry is still recorded as EntrySymlink so consumers
// see the anomaly, just without the dangling-too-big target string.
func readZipSymlinkTarget(f *zip.File, cap_ int64) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()

	body, err := io.ReadAll(io.LimitReader(rc, cap_))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// readZipEntryBody reads exactly size bytes from a zip entry. Used
// for capture-intent matches; size has already been validated against
// the intent's MaxSize so the read is bounded.
func readZipEntryBody(f *zip.File, size int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()

	buf := make([]byte, size)
	_, err = io.ReadFull(rc, buf)
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// readCappedAll buffers the input into memory, capped at limit
// bytes. A reader producing more than limit bytes returns
// ErrLimitExceeded — checked by reading limit+1 bytes and asserting
// the result fits.
func readCappedAll(r io.Reader, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("stream: read compressed input: %w", err)
	}
	if n > limit {
		return nil, fmt.Errorf("%w: compressed input exceeds %d bytes",
			ErrLimitExceeded, limit)
	}
	return buf.Bytes(), nil
}

// zipSizeAsInt64 converts zip's uint64 size to int64, returning
// (0, false) if the value would overflow. The caller fails the walk
// with ErrLimitExceeded — past int64 means past any cap we'd ever
// realistically configure.
func zipSizeAsInt64(u uint64) (int64, bool) {
	const maxInt64 = uint64(1<<63 - 1)
	if u > maxInt64 {
		return 0, false
	}
	return int64(u), true
}
