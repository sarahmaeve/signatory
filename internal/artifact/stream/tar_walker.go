package stream

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// walkTarGzip implements Walk for FormatTarGzip. Single-pass over a
// gzip-compressed tar stream:
//
//  1. Tee the input through sha256 so ArchiveSHA256 falls out of the
//     same pass — no second read.
//  2. Wrap the input in a counting reader so the compression-ratio
//     defense knows how many compressed bytes have been consumed.
//  3. Wrap the gunzipped stream in a cap-enforcing reader so
//     MaxTotalBytes and MaxCompressionRatio fire as ErrLimitExceeded
//     anywhere in the walk.
//  4. Iterate tar headers, classifying each entry by typeflag and
//     evaluating CaptureIntents against the verbatim path. For
//     intent matches, copy entry body bytes into a bounded buffer;
//     for non-matches, advance past the body without copying.
//  5. After EOF, detect the common top-level directory and finalize
//     the manifest's hash.
func walkTarGzip(ctx context.Context, src io.Reader,
	intents []CaptureIntent, lim Limits) (*Manifest, error) {

	lim = resolveLimits(lim)

	// Compressed-side accounting: tee through sha256 (for the
	// manifest's ArchiveSHA256) AND a counting reader (for the
	// ratio defense's "compressed bytes consumed" denominator).
	hasher := sha256.New()
	compressedCounter := &countingReader{r: io.TeeReader(src, hasher)}

	gz, err := gzip.NewReader(compressedCounter)
	if err != nil {
		return nil, fmt.Errorf("stream: gunzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	// Decompressed-side accounting: cap total bytes AND check ratio
	// against the compressed counter on every Read. Cap-triggered
	// failures return ErrLimitExceeded so Walk's caller can branch
	// on cap-vs-malformed without parsing strings.
	capped := &cappedReader{
		r:                    gz,
		maxTotal:             lim.MaxTotalBytes,
		maxRatio:             lim.MaxCompressionRatio,
		compressedCounter:    compressedCounter,
		ratioMinCompressed:   ratioMinCompressedBytes,
		ratioMinDecompressed: ratioMinDecompressedBytes,
	}
	tr := tar.NewReader(capped)

	manifest := &Manifest{
		Captured:       map[string][]byte{},
		SkippedIntents: map[string]string{},
	}

	for {
		// Honour cancellation at every header boundary so an
		// operator-initiated abort surfaces promptly even if the
		// archive is otherwise within caps.
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// cappedReader produces ErrLimitExceeded for cap firings;
			// keep that path distinct from format errors so callers
			// can branch correctly.
			if errors.Is(err, ErrLimitExceeded) {
				return nil, err
			}
			return nil, fmt.Errorf("stream: read tar header: %w", err)
		}

		// Entry-count cap: enforce BEFORE appending so the cap is the
		// exact maximum the manifest can carry, not max+1.
		if int64(len(manifest.Entries)) >= int64(lim.MaxEntries) {
			return nil, fmt.Errorf("%w: entry count exceeds %d",
				ErrLimitExceeded, lim.MaxEntries)
		}

		// Per-entry size cap: reject entries whose declared size is
		// over the cap before reading the body. archive/tar enforces
		// the declared size against the actual stream, so a lying
		// header shorter than the body fails parsing on the next
		// iteration; a lying header longer than the body fails with
		// io.ErrUnexpectedEOF — both surface as malformed-archive
		// errors, not silent truncation.
		if hdr.Size > lim.MaxEntryBytes {
			return nil, fmt.Errorf("%w: entry %q declared size %d > %d",
				ErrLimitExceeded, hdr.Name, hdr.Size, lim.MaxEntryBytes)
		}

		entry := Entry{
			Path:       hdr.Name,
			Size:       hdr.Size,
			Mode:       hdr.Mode, // verbatim int64; see Entry.Mode docstring.
			Type:       classifyTarType(hdr.Typeflag),
			LinkTarget: hdr.Linkname,
		}
		manifest.Entries = append(manifest.Entries, entry)
		manifest.TotalUncompressedBytes += hdr.Size

		// Capture-intent dispatch: only files with non-zero size are
		// candidates (a zero-size match has nothing to capture and
		// would produce an empty Captured entry that's
		// indistinguishable from "not captured").
		if entry.Type != EntryFile || hdr.Size == 0 {
			continue
		}

		captured := false
		for _, intent := range intents {
			if !intent.Match(entry) {
				continue
			}
			// Already captured for this intent → record duplicate.
			// First-match-wins is documented: a tarball with two
			// files matching the same intent is itself suspicious,
			// and consumers needing more sophisticated tie-breaking
			// can match more narrowly.
			if _, exists := manifest.Captured[intent.Name]; exists {
				manifest.SkippedIntents[intent.Name] = fmt.Sprintf(
					"duplicate match: %q (first match already captured)", entry.Path)
				continue
			}
			// Already recorded as oversized for this intent → don't
			// overwrite the first-seen reason.
			if _, exists := manifest.SkippedIntents[intent.Name]; exists {
				continue
			}
			// Per-intent oversize cap: refuse to read attacker-
			// controlled bytes into memory beyond the intent's
			// declared budget. This is the security-load-bearing
			// check that lets capture intents exist at all without
			// re-introducing the memory-exhaustion risk header-only
			// walking exists to eliminate.
			if hdr.Size > intent.MaxSize {
				manifest.SkippedIntents[intent.Name] = fmt.Sprintf(
					"oversize: entry %d bytes > intent cap %d",
					hdr.Size, intent.MaxSize)
				continue
			}

			buf := make([]byte, hdr.Size)
			if _, err := io.ReadFull(tr, buf); err != nil {
				if errors.Is(err, ErrLimitExceeded) {
					return nil, err
				}
				return nil, fmt.Errorf("stream: read body for capture intent %q: %w",
					intent.Name, err)
			}
			manifest.Captured[intent.Name] = buf
			captured = true
			break
		}

		if !captured {
			// Advance past the body without copying. Bounded by
			// hdr.Size which was already validated against
			// MaxEntryBytes above, so the read length is statically
			// known to be safe — io.CopyN, not io.Copy, makes that
			// boundedness visible to readers and to gosec (G110).
			//
			// The cappedReader still applies underneath, so the body
			// also counts against MaxTotalBytes / MaxCompressionRatio
			// during the skip; a lying header (smaller than the body)
			// surfaces as a tar parse error on the next tr.Next().
			if _, err := io.CopyN(io.Discard, tr, hdr.Size); err != nil {
				if errors.Is(err, ErrLimitExceeded) {
					return nil, err
				}
				return nil, fmt.Errorf("stream: skip body for %q: %w", hdr.Name, err)
			}
		}
	}

	manifest.StrippedTopDir = detectCommonTopDir(manifest.Entries)
	manifest.ArchiveSHA256 = hex.EncodeToString(hasher.Sum(nil))
	return manifest, nil
}

// classifyTarType maps a tar typeflag byte to the package's
// archive-format-agnostic EntryType. Unknown / non-standard
// typeflags collapse to EntryOther so consumers can flag them as
// anomalous without the walker pretending they're files.
func classifyTarType(typeflag byte) EntryType {
	switch typeflag {
	case tar.TypeReg, '\x00':
		// '\x00' is the historical "v7 tar" regular-file typeflag
		// (archive/tar's deprecated TypeRegA alias). Old archives
		// still surface entries with this byte; classify as EntryFile.
		return EntryFile
	case tar.TypeDir:
		return EntryDir
	case tar.TypeSymlink:
		return EntrySymlink
	case tar.TypeLink:
		return EntryHardlink
	default:
		// TypeChar, TypeBlock, TypeFifo, TypeCont, TypeXHeader,
		// TypeXGlobalHeader, TypeGNU* — all atypical-in-release-
		// tarball; collapsed to EntryOther for consumer surfacing.
		return EntryOther
	}
}

// countingReader wraps an io.Reader and exposes the running byte
// count. Used compressed-side to feed the ratio defense.
type countingReader struct {
	r     io.Reader
	count int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.count += int64(n)
	return n, err
}

// cappedReader wraps a decompressed stream and enforces MaxTotalBytes
// + MaxCompressionRatio. Cap-triggered errors are wrapped with
// ErrLimitExceeded so Walk can distinguish them from gzip/tar parse
// errors.
//
// Once a cap fires, the reader stays in the triggered state and
// returns the same error on subsequent Reads — this prevents tar.Reader
// from attempting recovery and producing confusing secondary errors.
type cappedReader struct {
	r                    io.Reader
	maxTotal             int64
	maxRatio             float64
	compressedCounter    *countingReader
	ratioMinCompressed   int64
	ratioMinDecompressed int64

	decompressedRead int64
	triggered        bool
	triggerErr       error
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.triggered {
		return 0, c.triggerErr
	}

	// Total-bytes cap: trim the buffer so we can't over-shoot. If
	// nothing is left, fire the cap before consuming bytes from
	// the underlying reader.
	remaining := c.maxTotal - c.decompressedRead
	if remaining <= 0 {
		c.fire(fmt.Errorf("%w: total uncompressed exceeds %d bytes",
			ErrLimitExceeded, c.maxTotal))
		return 0, c.triggerErr
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}

	n, err := c.r.Read(p)
	c.decompressedRead += int64(n)

	// Compression-ratio defense: only meaningful once we've seen
	// enough compressed input AND emitted enough decompressed output
	// to make the ratio statistically interesting. Below the
	// thresholds, gzip's internal buffering can produce noisy ratios
	// that would yield false positives on legitimate small inputs.
	if c.compressedCounter.count >= c.ratioMinCompressed &&
		c.decompressedRead >= c.ratioMinDecompressed {
		ratio := float64(c.decompressedRead) / float64(c.compressedCounter.count)
		if ratio > c.maxRatio {
			c.fire(fmt.Errorf("%w: compression ratio %.0f exceeds cap %.0f "+
				"(decompressed=%d compressed=%d)",
				ErrLimitExceeded, ratio, c.maxRatio,
				c.decompressedRead, c.compressedCounter.count))
			return n, c.triggerErr
		}
	}
	return n, err
}

func (c *cappedReader) fire(err error) {
	c.triggered = true
	c.triggerErr = err
}

// Ratio-defense thresholds. Below these, gzip's internal read-ahead
// buffer can make the running ratio swing wildly. Above them, the
// cap is a meaningful statistical signal.
//
// 1 KiB compressed and 64 KiB decompressed are well below the smallest
// legitimate release tarball and will be reached early in any walk
// large enough to be worth caring about — i.e. before any real bomb
// finishes, well after any noise.
const (
	ratioMinCompressedBytes   int64 = 1024
	ratioMinDecompressedBytes int64 = 64 * 1024
)
