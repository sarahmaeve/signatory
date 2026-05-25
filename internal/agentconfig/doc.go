// Package agentconfig declares the AI-agent configuration-file
// taxonomy as a single source of truth. Each Locus pairs the
// in-repo file-detector shape (where to scan in a clone, which
// filenames count, what the canonical name is) with the
// runtime-path substring patterns the source-AST persistence-write
// catalog uses.
//
// # Why a separate package
//
// Two consumers ask different questions about the same taxonomy:
//
//   - internal/signal/repofiles asks "is this exact file present
//     in a cloned repo?" — used for inventory and content-injection
//     scanning. Wants the Family-shape (Dirs + regex Detector +
//     Preferred canonical names).
//   - internal/signal/source/astfeature asks "does this
//     runtime-resolved path string match an agent-config locus?" —
//     used by the AST persistence-write detector. Wants flat
//     substring patterns to match against the writer's first arg.
//
// Before this package, both consumers maintained their own list of
// AI-agent paths. The lists drifted: /.codex/ ended up in
// astfeature but had no corresponding Family in repofiles. This
// package eliminates that class of drift by making both shapes
// derive from a single Locus declaration.
//
// # Scope
//
// AI-agent configuration ONLY — files that AI coding agents
// (Claude Code, Cursor, aider, Zed AI, Continue.dev, Windsurf,
// Codex CLI, …) read as instruction or configuration input.
// Project-hygiene files (README, SECURITY, CODEOWNERS, ...) stay
// in internal/signal/repofiles, where they belong with the
// hygiene-collector logic.
//
// # External motivating incident
//
// Trapdoor (2026-05) weaponized .cursorrules and CLAUDE.md as
// zero-width-Unicode prompt-injection carriers across npm, PyPI,
// and crates.io. See design/threat-landscape/2026-05-24-trapdoor-
// crypto-stealer.md and design/anti-subversion.md.
package agentconfig
