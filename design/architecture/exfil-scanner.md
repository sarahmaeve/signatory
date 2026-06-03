# Exfil / Content Scanners — Orientation & Lessons Learned

Status: written after the spadata PyPI-stealer follow-up (2026-06-03),
which added Discord-webhook exfil detection and a non-retaining
artifact-content scan composed into `artifact_repo_divergence`.
Audience: whoever adds the next **mechanistic scanner** — any check
that reads file *bytes* (not just paths/metadata) looking for a literal
or pattern across a source tree or a published artifact.

The single most important thing to internalize first:

> **There are two scan surfaces — the source clone and the published
> artifact — and they catch different attacks. A content scanner is a
> leaf that plugs into one or both. The substrate (walking, fetching,
> caps, no-disk-write) already exists; do not rebuild it.**

`exfilwatch` is the first worked example: one literal host-list
scanner, wired to *both* surfaces. Copy its shape.
`internal/signal/buildscript` (emitted as `build_script_concern`) is
the second — a heuristic build-script content scanner on the artifact
surface, built straight from §6's recipe — so the pattern is proven
twice. It adds two reusable wrinkles worth stealing: **severity by
co-occurrence** (a lone behaviour class is informational; two
co-occurring — decode+exec, fetch+exec — escalate to strong, mirroring
`source_evolution`'s rare-on-benign discipline) and a **conservative
high-entropy literal detector** (long base64-charset runs only, never
bare hex, so checksums don't false-positive).

---

## 1. The two surfaces (and why both)

| Surface | Reads | Wired at | Catches |
|---|---|---|---|
| **Source clone** | the git checkout of the declared source repo | `exfilwatch.Collector`, registered in `cmd/signatory/collectors.go` (clone-required collector set) → emits `exfil_capture_host` | a sink committed to source |
| **Published artifact** | the sdist/tarball actually uploaded to the registry | `internal/signal/artifact` collector, via the `stream.Scanner` hook → composed into `artifact_repo_divergence` | a sink in what was *published* but not in source — the CVE-2024-3094 / xz shape |

Scanning only the clone is a structural blind spot: a typosquat with a
clean (or absent) repo but a weaponized uploaded sdist passes. spadata
was exactly this. **A mechanistic scanner that only reads the clone is
half-built.**

---

## 2. `exfilwatch` — the matcher (surface-agnostic core)

`internal/signal/exfilwatch`. Pure, no I/O policy:

- `Hosts` — the curated literal list. Bar for inclusion: *no legitimate
  reason for this literal to appear in published library code.*
  webhook.site et al. meet it outright. `discord.com/api/webhooks`
  meets it as a dual-use exception (the webhook path embeds an id+token
  secret; a hardcoded one is a sink, and the path is specific enough
  that an API client on `/api/v10/` does not match). **Telegram is
  deliberately excluded** — every legit bot library hardcodes
  `api.telegram.org/bot`, so it would break the "a hit is a strong
  signal" contract. When in doubt, leave it out and note why in the
  `Hosts` doc comment.
- `scanReader` (core) → `ScanBytes(rel, []byte)` and
  `ScanReader(rel, io.Reader)` (the streaming variant the artifact
  walker uses). Line-based, case-insensitive, substring; an over-long
  minified line never halts the scan (an attacker would hide the sink
  past it otherwise).

The matcher knows nothing about clones vs artifacts. Keep new matchers
this way — a `[]Hit`-returning function over a reader — so both
surfaces can share it.

---

## 3. The artifact hook — `stream.Scanner`

`internal/artifact/stream` is the security-reviewed archive substrate:
header-only by default, never writes to disk, caps everything
(`DefaultLimits`). Read its `doc.go` before touching it.

Contents come out via two channels — know the difference:

| | `CaptureIntent` | `Scanner` |
|---|---|---|
| Fires for | **first** matching entry only | **every** matching entry |
| Retains | yes — bytes in `Manifest.Captured` for the result's life | **no** — body streamed to `Scan`, discarded after |
| For | named files (`.cargo_vcs_info.json`, future `setup.py`) | "examine all source files" (exfil scan) |
| Oversize | `SkippedIntents` | `SkippedScans` (keyed by path, since many entries match) |

A first-match `CaptureIntent` is a bypass for an all-files scan: a
second weaponized `__init__.py` would be invisible. That is why
`Scanner` exists. Use `CaptureIntent` only when you genuinely want one
named file.

API: `WalkWithScanners(ctx, src, format, intents, scanners, lim)`;
`Walk` is the zero-scanner shorthand (delegates with `nil`), so
existing callers are untouched. Threaded through tar **and** zip
walkers; the shared dispatch helpers live in `scan.go`
(`matchingScanners` / `anyScannerAccepts` / `runScanners` /
`recordScanSkips`). Body is read at most once: reused from a capture
buffer when an intent already claimed it, else into a transient
per-entry buffer freed at iteration end, else (nothing needs it)
advanced past unread. `MaxSize` per scanner; oversize → recorded, never
silently dropped, never buffered.

**Non-retaining is the contract.** Findings survive the walk; bytes do
not. This is a deliberate, bounded relaxation of the substrate's
"contents only via a named CaptureIntent" rule — it does **not**
re-introduce bulk-extract because nothing lands in the `Manifest` or on
disk.

---

## 4. Composition — don't just duplicate the clone signal

The artifact collector registers `exfilScanner` (2 MiB/file cap) in the
**same single walk** that builds the divergence manifest — no second
fetch. Hits flow through `CompareOptions.ExfilHits` →
`classifyExfilHits` (`exfil.go`) → `Comparison.ExfilHostsInArtifact`,
each tagged `path_in_repo`:

- `path_in_repo == false` → the strongest read: a sink present only in
  what was published (xz shape). Reuses the divergence `gitSet` and
  `StrippedTopDir` that were already being computed.
- `path_in_repo == true` → weaker alone (the source file exists too;
  we only know the *published* copy carries the literal), but a
  no-legitimate-purpose host is notable wherever it sits.

Lesson: a content scan on the artifact surface should **compose with
the divergence signal that already models artifact-vs-repo**, not emit
a parallel near-duplicate of the clone-side signal. The
clone-vs-artifact distinction *is* the signal.

---

## 5. Orientation lessons (how to not waste the first hour)

- **Don't reconnoiter the wiring with a qualified grep.** A grep for
  `exfilwatch.Scan` missed the in-package `Collector` (it calls `Scan`
  unqualified). Read the package's own files, then trace
  `NewCollector` callers. A symbol can be wired in a way your pattern
  doesn't see.
- **Scan tests for `internal/<pkg>` are usually `package <pkg>`**, not
  `<pkg>_test` — the fixture builders (`newTarGz`/`newZip`) are
  unexported. Match the existing test files' package clause or you
  can't reach the helpers.
- **PyPI `artifact_url` is the sdist (`.tar.gz`), not the wheel** — by
  design (`pypi/collector.go`: "wheel-vs-repo is a category error").
  So the **tar** walker is the load-bearing path for PyPI; the source
  payload lives in the sdist. Confirm the artifact *format* per
  ecosystem before assuming which walker matters.
- **Confirm the gate before celebrating the signal.** A new feature is
  only as useful as the gate that consumes it. The source-evolution
  *concern* gate (`source/concern.go`) deliberately **excludes**
  import-time / network / base64 counts (naturally non-zero on benign
  code). spadata's only observable AST features were exactly those
  three — so even a perfect AST read would not have fired the concern
  boolean. Always check: does my new signal reach a gate that acts on
  it, or does it die in a field nobody thresholds?
- **The substrate already solved the hard parts** (fetch, decompress,
  caps, no-disk-write, sha, top-dir strip). New scanner work is a leaf:
  a matcher + a `Scanner` registration + a composition into an existing
  signal. If you find yourself writing archive-walking code, stop.

---

## 6. Recipe — adding the next mechanistic scanner

1. Write the matcher as a pure `func(rel string, r io.Reader) []Hit`
   (or `[]byte`), surface-agnostic, in its own package. TDD the matcher
   alone first.
2. **Clone surface:** wrap it in a `signal.Collector` over the clone
   path (mirror `exfilwatch.Collector`); register in
   `cmd/signatory/collectors.go`.
3. **Artifact surface:** register a `stream.Scanner` (set `MaxSize`,
   `Match`, `Scan → matcher`) in the artifact collector's
   `WalkWithScanners` call; compose results into an existing signal
   (prefer enriching `artifact_repo_divergence` over a new type).
4. Register/extend the signal-type metadata in `internal/signal/types.go`.
5. Decide both surfaces deliberately. Clone-only is a known blind spot.

---

## 7. Known gaps (as of 2026-06-03)

- **gem** ecosystem: the two-pass outer/inner `.gem` walk does not yet
  thread scanners (`artifact/collector.go` gem branch is a no-op for
  scanning). Tar/zip are covered.
- **Per-file cap** 2 MiB: larger entries are recorded in `SkippedScans`,
  not scanned.
- **Telegram** and other genuinely dual-use channels are unhandled by
  design — they need a discriminating mechanism, not a literal host
  list.
- Obfuscated literals (XOR/base64/runtime concat) defeat a literal
  scan by design — that is the AST analyzer's job, not this one.
