# Dogfood GAPS

Process and integrity gaps surfaced while running signatory on signatory's
own dependencies. Distinct from per-dependency trust notes (those live in
`<name>.md`) — these are pipeline findings.

## 2026-04-21 — Version information dropped between survey, analyze, and posture

### Symptom

`signatory survey` shows go.mod pins `modernc.org/sqlite` at **v1.48.2**
and emits this action item:

```
signatory analyze pkg:go/modernc.org/sqlite --refresh --clone --path filestore/clones/sqlite
```

Running the `/analyze` skill against that target produced a posture at
`version_scope: v1.49.1` (HEAD of default branch, tagged 4 days prior).
A subsequent `signatory survey` then reported:

```
[?] modernc.org/sqlite   v1.48.2   unexamined   (other versions in store)
```

— i.e. the analysis the user was just told to run does not cover the
version the user actually ships.

### Where the version drops

Three separate hand-offs each discard version info:

1. **Survey → action item.** Survey has already parsed `go.mod` and knows
   the pinned version. The rendered action-item command omits it. A
   caller who follows the instruction verbatim analyzes a different
   version than the one they asked about.

2. **`signatory handoff … --clone-dir …` shallow-clones the default
   branch.** `git clone --depth=1` lands on HEAD. There is no tag
   checkout, no ref pin, no awareness of a pinned version. Whatever
   happens to be HEAD at collection time is what the analysts see.

3. **Posture records the observed version, not the requested version.**
   The synthesist sets `version_scope` to whatever the analysts
   inspected. If the caller asked about v1.48.2 and the clone landed
   on v1.49.1, the posture records v1.49.1 — silently moving the scope
   off the user's question.

### Why this is worse than a single bug

Each layer preserves *some* version info (survey knows it; clone has it
in refs; posture has a slot for it) but none of them carry it through
the boundary. The failure is the composition — any one of these
behaviours in isolation would be defensible; together they produce a
pipeline where version-specific questions always get version-wrong
answers, silently.

Related (same family, recorded separately): `pkg:npm/X@V` is ignored
when `--version` is unset on `posture set` (2026-04-20). Version info
is leaking at multiple boundaries.

### What the user expected

The contract survey implies: *"run this command and the [?] will
become a [✓] (or [✗])."* The contract today: *"run this command and
some version — not necessarily the one in your go.mod — will acquire
a posture, leaving your actual pin unexamined."*

### Candidate remediations (not decisions)

- Survey's action item should include the pinned version:
  `signatory analyze pkg:go/modernc.org/sqlite@v1.48.2 …`
- `signatory handoff --clone-dir` should honour a version in the
  target URI and `git checkout` the corresponding tag after shallow
  clone (or `git fetch --depth=1 <ref>`).
- The analyst handoff templates should declare the requested version
  so the analysts can refuse to proceed if the working tree is at a
  different ref.
- The posture ingest path should reject a `version_scope` that does
  not match what the caller originally asked about — or, less
  aggressively, warn the user when there's a mismatch.

Fixing this is a prerequisite for survey → analyze → posture to form
a coherent loop without version-drift surprises.
