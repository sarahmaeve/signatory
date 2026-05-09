// Package stream provides centralized, defensive walking of release
// artifacts (tarballs, zip archives) for signatory's signal collectors.
//
// # Threat model
//
// Release artifacts are untrusted inputs from adversaries. The package
// enforces three load-bearing rules:
//
//  1. Never write to disk. Walking is in-memory and single-pass; the
//     bytes never land in the filestore. This eliminates the
//     "expand-then-execute" install-vector attack surface that
//     CVE-2024-3094 (xz-utils) exploited via build-to-host.m4 — see
//     design/threat-landscape/example-xz-utils-cve-2024-3094.md.
//
//  2. Never read entry contents into memory unless a consumer has
//     registered a named CaptureIntent with a hard size cap. The
//     walker emits headers (the manifest) by default; bytes flow only
//     when a consumer has declared what it wants and how large the
//     declaration permits.
//
//  3. Apply defensive caps to every walk: total uncompressed size,
//     per-entry size, entry count, and compression ratio. These
//     caps protect against decompression bombs, oversized payloads,
//     and entry-count flood DoS. Defaults in DefaultLimits.
//
// # Suspicious paths are evidence, not failures
//
// Path-traversal entries (../../etc/passwd), absolute paths,
// NUL-bytes-in-name, backslashes, drive letters, and symlinks whose
// LinkTarget escapes the archive root are RECORDED verbatim in
// Entry.Path / Entry.LinkTarget — they are NOT rejected. The
// reasoning is structural: with rule 1 (no disk writes) in force,
// these paths cannot actually escape anywhere; surfacing them in the
// manifest is exactly the value-add for the artifact-vs-repo
// divergence collector. A walker that sanitized suspicious paths
// upstream would hide the most diagnostic evidence the consumer has.
// Classification ("this looks like tar-slip") is a consumer concern
// applied to the manifest, not a walker concern that drops data.
//
// # Consumers
//
// The package is the substrate for the artifact-vs-repo divergence
// collector (internal/signal/artifact/) and is positioned to host
// future consumers — build-script content scrutiny per
// design/threat-landscape/example-xz-utils-cve-2024-3094.md (Signal
// #2 in that doc's "Signals That Would Close the Gap" section) is the
// canonical next consumer.
//
// # Format dispatch
//
// Callers declare the format explicitly (FormatTarGzip, FormatZip)
// rather than the walker sniffing magic bytes. Every signatory
// consumer knows what container its registry serves; explicit
// declaration prevents the "we walked a zip as a tarball because the
// magic bytes happened to align" failure mode and makes the dispatch
// auditable in the call site.
package stream
