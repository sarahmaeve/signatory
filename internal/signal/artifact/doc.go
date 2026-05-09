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
// # Architecture
//
// The package is a consumer of internal/artifact/stream — the
// centralized archive-walking + HTTP-fetching service that owns:
//
//   - Header-only iteration over tar.gz / zip archives. We never
//     io.Copy entry bodies to disk; never call os.OpenFile / os.Create.
//     Tar-slip / zip-slip / symlink-escape become RECORDS in the
//     manifest (preserving evidence for divergence analysis), not
//     filesystem operations.
//
//   - Defensive caps (MaxTotalBytes, MaxEntryBytes, MaxEntries,
//     MaxCompressionRatio, MaxCompressedBytes) applied uniformly
//     across both archive formats. Cap-triggered failures wrap
//     stream.ErrLimitExceeded so the collector can branch on
//     cap-vs-malformed without parsing strings.
//
//   - Named-file capture intents (the only way to obtain entry
//     contents) with hard size caps — used for cargo's
//     .cargo_vcs_info.json post-fetch SHA recovery and a future
//     home for build-script content scrutiny.
//
// This package's job is the trust-model layer ON TOP of that
// substrate: pair the artifact to a git commit (pair.go), classify
// the path differences (categorize.go), and emit the divergence
// signal. The stream layer is "what's in the archive"; this layer
// is "what does that mean."
//
// # No persistent cache
//
// Tarballs are streamed fetch → walk → diff in a single pass. No
// tempdir, no on-disk artifact, no filestore caching. Re-runs
// re-download — tarballs are small (low MB) and cache invalidation
// is more cost than the bandwidth saved.
package artifact
