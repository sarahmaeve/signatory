# mousetrap (github.com/inconshreveable/mousetrap)

**Role: Runtime (transitive dependency of Cobra)**
**Decision: Rejected**
**Date: 2026-04-09**

## Signal Table

| Signal | Value | Assessment |
|--------|-------|------------|
| Purpose | Detect Windows Explorer launch | Windows-only, no-op elsewhere |
| Owner | Alan Shreve (inconshreveable, ngrok creator) | Known identity, 2055 followers, account since 2011 |
| Created | April 2014 | 12 years old |
| Last commit | November 2022 | 3.5 years fallow |
| Contributors | 3 (16 commits from owner) | Bus factor = 1 |
| Stars | 269 | Low; mostly pulled via Cobra |
| Commit signing | Mixed | Recent verified, older unsigned |
| Temporal era | Pre-LLM (last activity Nov 2022) | Never AI-reviewed |
| Org affiliation | None | Individual maintainer |

## Risk Assessment

Fallow, single-maintainer, Windows-only code pulled into every Cobra-based
project regardless of target platform. The module is in go.mod
unconditionally even though the code is behind a Windows build tag. If this
account were compromised, the blast radius would include every Cobra-based
CLI tool — kubectl, Docker CLI, Hugo, and thousands of others.

## Decision

**Rejected.** This dependency led us to evaluate CLI frameworks with zero
or minimal runtime dependencies. We selected Kong instead of Cobra,
eliminating mousetrap and three other transitive dependencies entirely.

### CLI Framework Comparison

| Framework | Runtime deps | Mousetrap? | Notes |
|-----------|-------------|------------|-------|
| Cobra | 4 (mousetrap, pflag, go-md2man, yaml) | Yes | Industry standard, but heavy dep tree |
| Kong | 0 | No | Struct-based, well-maintained |
| urfave/cli v3 | 0 | No | Functional style, widely used |
| stdlib flag | 0 | No | No subcommand support without manual work |
