package stream

// Format identifies the archive container the walker should dispatch
// on. Callers declare this explicitly rather than sniff bytes — see
// the package doc comment for why.
type Format int

const (
	// FormatUnknown is the zero value; passing this to Walk fails
	// fast rather than guessing.
	FormatUnknown Format = iota

	// FormatTarGzip is the gzip-compressed tar container used by npm
	// (.tgz), cargo (.crate), pypi sdists (.tar.gz), gem inner data
	// streams, and most autotools dist tarballs.
	FormatTarGzip

	// FormatZip is the PKZIP container used by pypi wheels (.whl),
	// java jars (.jar), nupkg (.nupkg), and GitHub release zips.
	FormatZip

	// FormatTar is the plain (uncompressed) tar container. Currently
	// used only for `.gem` outer wrappers — RubyGems' .gem format is
	// a tar holding `data.tar.gz`, `metadata.gz`, and
	// `checksums.yaml.gz` as siblings. Header-only walking dispatches
	// here; the gem collector then re-walks the captured data.tar.gz
	// bytes via FormatTarGzip in a second pass.
	//
	// Compression-ratio defenses don't apply (input is uncompressed,
	// ratio is 1:1 by definition); MaxTotalBytes still enforces the
	// process-resource ceiling on the same terms as the other formats.
	FormatTar
)

// String renders the format for logging and error messages.
func (f Format) String() string {
	switch f {
	case FormatTarGzip:
		return "tar.gz"
	case FormatZip:
		return "zip"
	case FormatTar:
		return "tar"
	default:
		return "unknown"
	}
}

// EntryType classifies an archive entry by what it represents on a
// real filesystem. The walker emits headers only; even for EntryFile
// entries the bytes are NOT copied unless a CaptureIntent matches.
type EntryType int

const (
	EntryUnknown EntryType = iota
	EntryFile
	EntryDir
	EntrySymlink
	EntryHardlink
	// EntryOther covers device nodes, fifos, sockets, and any
	// header type that has no meaningful filesystem-extraction
	// semantics in a release artifact. Their presence is itself
	// suspicious; the walker records them in the manifest but
	// surfaces them as a hygiene observation to consumers.
	EntryOther
)

// String renders the entry type for logging and test assertions.
func (t EntryType) String() string {
	switch t {
	case EntryFile:
		return "file"
	case EntryDir:
		return "dir"
	case EntrySymlink:
		return "symlink"
	case EntryHardlink:
		return "hardlink"
	case EntryOther:
		return "other"
	default:
		return "unknown"
	}
}

// Entry is a single header from the archive's manifest. Bytes are
// deliberately absent: see CaptureIntent for the only way to obtain
// file contents.
type Entry struct {
	// Path is the entry's path within the archive, preserved verbatim
	// from the archive header. The walker performs no normalization,
	// no top-dir stripping, and no rejection of suspicious patterns
	// (path traversal, absolute paths, NUL bytes, backslashes, drive
	// letters). See the package doc's "Suspicious paths are evidence"
	// section for why: the divergence collector NEEDS to see these
	// paths verbatim to surface them as evidence; with no disk
	// writes, the paths cannot actually cause filesystem escapes.
	//
	// Manifest.StrippedTopDir reports the auto-detected wrapping
	// directory ("package/" for npm, "<name>-<version>/" for cargo
	// and autotools); consumers compute their own post-strip view by
	// trimming that prefix. This keeps the walker's output a faithful
	// record of the archive's actual contents.
	Path string

	// Size is the uncompressed size in bytes as declared by the
	// archive header. Untrusted: the walker's per-entry cap is
	// enforced against the actual stream length, not this value.
	Size int64

	// Mode is the POSIX mode bits from the entry header, recorded
	// verbatim as int64 (the native tar header field type). Zero for
	// archive formats that don't carry mode information (e.g. some
	// zip producers).
	//
	// Using int64 rather than os.FileMode is deliberate: the manifest
	// records evidence, and a malformed or anomalous mode value is
	// itself diagnostic. Coercing into a stricter type would lose the
	// out-of-spec bits that a defender might want to flag.
	Mode int64

	// Type classifies the entry.
	Type EntryType

	// LinkTarget is set for EntrySymlink and EntryHardlink entries;
	// empty otherwise. Preserved verbatim like Path — targets that
	// escape the archive root are recorded, not rejected. Symlinks
	// are NEVER followed by the walker (no extraction means no
	// filesystem traversal); classification is a consumer concern.
	LinkTarget string
}

// CaptureIntent registers a consumer's interest in receiving the
// bytes of one or more specific archive entries. The walker enforces
// MaxSize: oversized matches are not captured and are reported in
// Manifest.SkippedIntents with a reason.
//
// CaptureIntent is the only way to obtain entry contents — there is
// no "give me everything" mode by design. A bulk-extract API would
// re-introduce the "fully expand a tarball" risk this package
// centralizes against.
//
// Match is called with each Entry post-strip and post-normalization;
// consumers should match on Path and Type only (Size is untrusted
// header data). When Match returns true for multiple entries, only
// the first match is captured — additional matches are recorded in
// SkippedIntents with reason "duplicate match".
//
// Use cases:
//   - cargo divergence: Match Path == ".cargo_vcs_info.json" &&
//     Type == EntryFile, MaxSize 64 KiB.
//   - npm divergence: no intents needed (gitHead comes from
//     registry metadata, not the tarball).
//   - future build-script scrutiny: configure.ac, *.m4, setup.py,
//     build.rs, package.json scripts field.
type CaptureIntent struct {
	Name    string
	Match   func(Entry) bool
	MaxSize int64
}

// Manifest is the result of walking an archive.
type Manifest struct {
	// Entries is the verbatim header listing of every entry observed
	// in the archive, in archive order, with paths and link targets
	// preserved exactly as they appeared. Suspicious patterns are NOT
	// filtered out — see Entry.Path for why.
	Entries []Entry

	// StrippedTopDir is the wrapping directory the walker
	// auto-detected (e.g. "package/" for npm, "<name>-<version>/" for
	// cargo and autotools). Empty if no single common top-level
	// directory exists across every entry.
	//
	// The walker does NOT rewrite Entry.Path values; consumers that
	// want a post-strip view trim this prefix themselves. This keeps
	// Entries a faithful record while still saving every consumer
	// from re-implementing the same prefix-detection heuristic.
	StrippedTopDir string

	// Captured maps CaptureIntent.Name → file bytes for intents that
	// matched at least one entry under their MaxSize cap. Absent if
	// the intent matched nothing.
	Captured map[string][]byte

	// SkippedIntents maps CaptureIntent.Name → human-readable reason
	// for intents that matched an entry but did NOT capture it. The
	// most common reason is oversize; another is "duplicate match"
	// (intent matched a second entry after the first was captured).
	SkippedIntents map[string]string

	// ArchiveSHA256 is the hex-encoded sha256 of the raw archive
	// bytes as fed to Walk. Computed via tee-reader during the same
	// pass as the walk — no extra read.
	ArchiveSHA256 string

	// TotalUncompressedBytes is the sum of accepted entry sizes,
	// checked against Limits.MaxTotalBytes during the walk.
	TotalUncompressedBytes int64
}

// Limits caps the resources a single Walk call may consume. The
// defaults (DefaultLimits) protect against decompression bombs, tar
// slips, zip bombs, and entry-count floods. Callers may relax limits
// when they know the source is trusted; tightening is also fine.
//
// Exceeding any limit fails the walk — partial walks are not
// returned. A truncated manifest hides exactly the kind of evidence
// (the entry past the cap) that would explain why the walk stopped.
type Limits struct {
	// MaxTotalBytes caps the total uncompressed bytes the walker
	// will accept across all accepted entries.
	MaxTotalBytes int64

	// MaxEntryBytes caps any single entry's uncompressed size. An
	// entry exceeding this fails the walk: an archive containing a
	// giant entry is itself the threat signal.
	MaxEntryBytes int64

	// MaxEntries caps the entry count. Defends against
	// millions-of-tiny-files DoS.
	MaxEntries int

	// MaxCompressionRatio is the ratio of total uncompressed bytes
	// to compressed input bytes the walker tolerates. 100.0 means a
	// 1 MiB compressed stream may not exceed 100 MiB uncompressed.
	// Standard zip-bomb defense.
	MaxCompressionRatio float64

	// MaxCompressedBytes caps the size of the COMPRESSED input the
	// walker will buffer in memory. Used by formats that require
	// random access (zip — central directory at end of stream); not
	// applicable to streaming formats (tar.gz reads the cap on the
	// uncompressed side via MaxTotalBytes alone).
	//
	// A compressed input exceeding this fails the walk before any
	// archive parsing starts; an archive whose on-the-wire size is
	// >256 MiB is well outside legitimate package-distribution
	// territory and any cap-relaxation should come with documented
	// justification.
	MaxCompressedBytes int64
}

// DefaultLimits is the conservative baseline applied when callers
// pass a zero-valued Limits to Walk. Sized for npm / cargo /
// pypi-sdist / gem / GitHub-release-zip artifacts; the largest
// legitimate npm tarball in npm registry history is well under
// 256 MiB and the largest single-entry payload is well under 64 MiB.
//
// The walk-time default-fallback helper that fills zero-valued
// fields from this baseline lives alongside the format walkers in
// A2 — see resolveLimits in tar_walker.go (added in phase A2).
var DefaultLimits = Limits{
	MaxTotalBytes:       256 << 20, // 256 MiB
	MaxEntryBytes:       64 << 20,  // 64 MiB
	MaxEntries:          100_000,
	MaxCompressionRatio: 100.0,
	MaxCompressedBytes:  256 << 20, // 256 MiB (zip / random-access formats)
}
