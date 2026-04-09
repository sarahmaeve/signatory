# Signatory: Dependency Decisions (Dogfooding)

Signatory applies its own trust model to its own dependencies. This
directory records posture decisions for each dependency we adopt, serving
as both a record and a test of the trust model.

## Dependency Roles

Not all dependencies carry the same risk. A project owner should assess
the **role** a dependency plays in their project, because role determines
blast radius:

| Role | Scope | Risk Profile |
|------|-------|-------------|
| **Runtime** | Compiled into the production binary, executes in deployment | Highest risk. Compromise = production compromise. |
| **Validation** | Runs during testing and CI. Not in the production binary, but executes in build environments with access to secrets, tokens, and CI credentials. | High risk. Compromise enables CI secret theft (cf. prt-scan), supply chain pivot, or silent test corruption. |
| **Build-only** | Code generation, linting, formatting. Runs at build time, output is checked in or compiled. | Medium risk. Compromise can inject code into build artifacts. |
| **Development** | Editor tooling, local-only utilities. Never runs in CI or production. | Lower risk, but can compromise developer workstations. |

A test framework like testify is a **validation dependency** — it never
reaches production, but it executes in every CI run with full access to
the build environment. The axios attack compromised runtime dependencies;
the prt-scan attack targeted CI environments. Both vectors matter.

## Decisions

| Dependency | Role | Decision | File |
|------------|------|----------|------|
| mousetrap | Runtime (transitive) | **Rejected** — fallow, single-maintainer | [mousetrap.md](mousetrap.md) |
| Kong | Runtime | **Trusted-for-now** — zero deps, strong author provenance | [kong.md](kong.md) |
| testify | Validation | **Trusted-for-now** — org-owned, formal governance, test-only | [testify.md](testify.md) |
| modernc.org/sqlite | Runtime | **Trusted-for-now** — active, pure Go requirement, but single-maintainer governance gap | [modernc-sqlite.md](modernc-sqlite.md) |
