// Package buildscript is a heuristic content scanner for build /
// install lifecycle scripts in a published release artifact — the
// "content scrutiny" follow-on to the presence-only build_script_present
// / postinstall_present signals, and item #2 ("highest remaining
// leverage") of the CVE-2024-3094 gap analysis in
// design/threat-landscape/example-xz-utils-cve-2024-3094.md.
//
// # What it looks at
//
// Author-written build inputs only — setup.py, build.rs, extconf.rb,
// configure.ac/.in, Makefile.am, and hand-written *.m4 macros.
// Generated/vendored autotools output (configure, config.status,
// aclocal.m4, libtool's lt*.m4, …) is deliberately excluded: it is
// huge and legitimately full of eval/base64-shaped content, so scanning
// it is pure false-positive noise. The xz backdoor lived in a
// hand-written m4 macro, which this DOES scan.
//
// # What it looks for
//
// Line-based token catalogs (case-insensitive substring, like
// exfilwatch) for three behaviour classes — decode primitives, eval /
// exec / shell-out, and build-time network fetch — plus a high-entropy
// embedded-literal detector (long base64-charset runs). It is a
// heuristic, NOT a parser: deliberately language-neutral so it covers
// m4 / shell / Ruby that have no analyzer.
//
// # Severity
//
// Findings are informational by default and escalate to strong only on
// the malware-shaped combinations, mirroring the source_evolution
// concern's rare-on-benign discipline:
//
//   - a high-entropy embedded literal — always strong;
//   - any two distinct behaviour classes co-occurring in one file
//     (decode + exec, fetch + exec, decode + fetch) — strong;
//   - a single class alone (a configure.ac that merely shells out) —
//     informational. The mechanistic layer is descriptive; a Layer-2
//     analyst reads the snippet to judge intent.
//
// The scanner reads bytes as data only — it never executes a script.
package buildscript
