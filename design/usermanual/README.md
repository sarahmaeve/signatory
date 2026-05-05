# Signatory User Manual (WIP)

Task-oriented documentation for signatory users. Each page covers a coherent workflow rather than exhaustive command references.

Contents will grow as features stabilize; the v0.1 target is enough to onboard a new user to the primary workflows without reading the design docs.

## Index

- [The analysis flow](analysis-flow.md) — mental model (Layer 1 / Layer 2), routing across CLI / MCP / `/analyze`, the full `/analyze` pipeline step-by-step, which steps the CLI can replicate by hand, MCP tool reference, verification recipes, known v0.1 interface gaps
- [Recording trust decisions](recording-trust-decisions.md) — posture set / unset / get / burn add / remove / list, target grammar, dry-run, exit codes, audit trail

## Status

- **Format**: each page is a single markdown file, task-oriented, with copy-pasteable commands. Design rationale stays in `../` (the `design/` root); the user manual shows *what to type* and the narrowest *why* needed to pick the right form.
- **Drift**: when the CLI changes, the corresponding user manual page changes with it. A manual page that doesn't match `signatory <cmd> --help` is a bug.
- **Assumed reader**: a software engineer comfortable with CLI tools, new to signatory, with a running daemon (`signatory serve start`) and binary on `PATH`.
