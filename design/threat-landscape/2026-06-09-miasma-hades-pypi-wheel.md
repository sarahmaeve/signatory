# 2026-06-09: Miasma / Hades Mini-Shai-Hulud — the Payload Moves Into the Wheel

## Source

Socket Threat Research, "Mini Shai-Hulud / Miasma / Hades worms target
bioinformatics and MCP developers via malicious PyPI packages"
(`socket.dev/blog/mini-shai-hulud-miasma-and-hades-worms-target-bioinformatics-and-mcp-developers-via-malicious`,
fetched 2026-06-09). Reports 60 malicious artifacts across 37 PyPI
packages (a 23-artifact newer wave expanding an earlier set), targeting
two developer communities: bioinformatics researchers (graph-learning,
phenotyping) and MCP / AI-tooling developers.

This is the **PyPI / wheel arm** of the same campaign family the
threat-landscape already tracks on the npm and cross-ecosystem sides:

- [`2026-05-12-tanstack-mini-shai-hulud.md`](2026-05-12-tanstack-mini-shai-hulud.md)
  — the npm / OIDC-federated-republish arm (TeamPCP).
- [`2026-05-24-trapdoor-crypto-stealer.md`](2026-05-24-trapdoor-crypto-stealer.md)
  — the three-ecosystem credential-stealer arm; closest structural
  analog (its §"PyPI payload" already noted a Python package shelling
  out to a JS runtime via `node -e`).

What distinguishes this arm is *where the payload lives*. The prior
PyPI coverage centered on `setup.py` / import-time execution and the
sdist. This campaign moves the payload **into the wheel (bdist)** —
`.pth` startup hooks, bundled `_index.js`, and trojanized `.abi3.so`
native extensions — which is the one PyPI artifact signatory
deliberately never opens.

## Why this entry exists

The campaign's central novelty maps almost perfectly onto a structural
blind spot in the v0.1 collection model, and the "almost" is the point.

Signatory's PyPI surface is **sdist + git-clone + registry-metadata**.
Three verified facts make the wheel invisible:

1. `recordArtifactURL`
   (`internal/signal/registry/pypi/collector.go`, ~lines 502-507)
   emits **only** the sdist URL to the artifact-vs-repo collector, with
   the in-code rationale "wheels are build outputs … so wheel-vs-repo is
   a category error." The wheel is never paired against the source tree,
   so the xz-shaped `files_extra_in_tarball` check cannot see a
   wheel-resident `.pth` / `_index.js` / `.abi3.so`.
2. `extLanguage` / `LanguageForPath`
   (`internal/signal/source/astfeature/language.go`, ~lines 87-105)
   classify `.pth` as `NotSource`, and the Python AST analyzer only
   parses `.py`. The `.pth → _index.js → Bun` chain is never
   AST-analyzed.
3. Many of these packages are typosquats / standalone malicious uploads
   with no real source repo, so the git / repofiles / exfilwatch paths
   have nothing to clone.

The payload lives only in the wheel, and the wheel is the one place
signatory does not look.

## The campaign shape

Reconstructed from the Socket writeup:

1. Publish wheels to PyPI under two naming verticals: a bioinformatics
   cluster (`embiggen`, `ensmallen`, `gpsea`,
   `phenopacket-store-toolkit`, `ppkt2synergy`, `pyphetools`) and an
   MCP / AI-tooling cluster (`langchain-core-mcp`, `instructor-mcp`,
   `openai-mcp`, `tiktoken-mcp`, `ray-mcp-server`), plus typosquats
   (`rsquests`, `tlask`, `rlask`) and miscellany (`dreamgen`, `mem8`,
   `mflux-streamlit`, `orchestr8-platform`).
2. **Three parallel delivery branches** — the campaign's stated
   evolution from the earlier single-`.pth`-plus-bundled-payload model:
   - **`.pth` startup hook.** A malicious wheel ships a `*-setup.pth`
     file. Python's `site` module executes any `.pth` line beginning
     `import` at interpreter startup; the line bootstraps a bundled
     `_index.js`.
   - **Native-extension trojanization.** The bioinformatics cluster
     embeds the malicious path inside a compiled `.abi3.so`. When Python
     imports the package and `dlopen()`s the extension, the native
     module's initializer runs `_index.js` as a side effect.
   - **Loader/payload split.** `langchain-core-mcp@1.4.2`'s `.pth`
     searches `sys.path` for `_index.js` rather than bundling it —
     staged delivery across artifacts.
3. **Execution via a foreign runtime.** The loader downloads the Bun
   runtime into a temp directory if absent, then
   `subprocess.run([bun_path, "run", payload])`. A run-once marker
   `/tmp/.bun_ran` bounds re-execution; the observed upload User-Agent
   was `Bun/1.3.14`.
4. **The "Hades" stealer.** Heavily obfuscated JavaScript (char-code
   arrays + ROT-style substitution). Credential targets: GitHub / npm /
   PyPI / RubyGems / JFrog tokens, cloud tokens, Kubernetes
   service-account material, SSH keys, Docker configs, shell history,
   `.env` files, AI-developer-tool configs.
5. **Lateral movement and escape.** SSH propagation via
   `/tmp/.sshu-setup.js`; abuse of `docker.sock` for container escape
   when accessible.
6. **Defense evasion.** The malware blackholes StepSecurity's telemetry
   endpoints — `agent.stepsecurity.io`, `api.stepsecurity.io`,
   `app.stepsecurity.io` — blinding the monitoring tool that would
   otherwise catch it.
7. **Anti-LLM-analysis.** `_index.js` opens with a large block comment
   of fake system instructions / policy-triggering content, engineered
   to trigger AI-safety refusals so an LLM scanner stops before reaching
   the real payload. This is an attack on AI code scanners — including
   signatory's own analyst pipeline.
8. **C2.** GitHub-based dead-drop with fallback discovery strings
   (`thebeautifulmarchoftime`, `thebeautifulsnadsoftime`).

## What this validates in our existing model

The cross-ecosystem-operator framing holds: this is the fourth campaign
of the template (after bufferzonecorp, TanStack/TeamPCP, and Trapdoor),
and the Python→foreign-JS-runtime primitive
[Trapdoor §"PyPI payload"](2026-05-24-trapdoor-crypto-stealer.md) named
(`node -e`) recurs here as `bun run`. The shell-out-to-a-JS-runtime
shape is now a campaign-family constant, not a one-off — and it is
correctly priced by the existing `ExecCalls` spike (a Python module that
spawns a subprocess at import time), *when the code is reachable by the
analyzer*. Here it usually is not, because the exec lives in the
wheel-resident loader.

The **High-tier** forgery-resistance placement
([`../trust-model.md`](../trust-model.md) §"Signals must be weighted by
forgery resistance") is reaffirmed: a `.pth`/`.so` install-time hook is
arbitrary code execution at the same tier as npm `postinstall`, gem
`extconf.rb`, and cargo `build.rs`. No key compromise required.

## What this exposes as a gap

### PyPI has no native-extension signal — the gem parity gap (LANDED)

The gem collector emits `native_extension_present` /
`native_extension_introduced`
(`internal/signal/registry/gem/collector.go`, ~437-487; registered
`internal/signal/types.go`, ~1000-1021), keyed on `platform != "ruby"`.
**PyPI had no equivalent**, even though the bioinformatics cluster's
whole vector is a compiled `.abi3.so` in a wheel.

The data needed is already in the parsed registry response.
`Distribution` (`internal/signal/registry/pypi/wire.go`, ~27-35) carries
`Filename`, and a PEP 425 wheel filename's final hyphen-delimited
component is the platform tag: `py3-none-any` (and `py2.py3-none-any`)
is pure-Python; a concrete tag (`manylinux…`, `macosx…`, `win_amd64`,
`musllinux…`) means a compiled extension ships. No wheel is opened, no
extra HTTP, GET-only.

Landed this session (branch `pypi-sandrider`) as two signals parallel to
the existing `sdist_only_present` / `sdist_only_introduced` pair
(`internal/signal/types.go`, ~911-933) — the inverse window over the
same sorted version records:

- `native_extension_present` (PyPI): latest version ships ≥1 non-`any`
  wheel. Payload carries `present`, `version_checked`, `versions_checked`,
  `native_wheel_count`, and a capped `platform_tags` sample.
- `native_extension_introduced` (PyPI): a native wheel appears where
  prior versions in the window were pure-Python — the masquerade shape.

Implementation: `wheelPlatformTag` / `nativeWheelSummary` parse the
filename; a `nativeExt` field on `versionRecord` feeds
`recordNativeExtensionPresent` / `recordNativeExtensionIntroduced`,
wired into `recordReleaseSignals` beside `recordSdistOnlyIntroduced`.
The shared signal-type registry entries were broadened from gem-only to
cover both ecosystems.

**Honest boundary, written into the caveats.** This catches the
**masquerade** shape — a "pure-Python" typosquat (the MCP-themed
cluster: `tiktoken-mcp`, `langchain-core-mcp`) that suddenly compiles a
`.so`. It does **not** catch trojanization of an **already-native**
package — the `embiggen` / `ensmallen` bioinformatics cluster
legitimately ships native wheels every version, so a poisoned version
shows no transition. Native presence alone is not negative; the entire
scientific-Python stack (numpy, scipy, cryptography, pydantic) is
native. The *introduced* transition is the signal-bearing axis;
presence is context.

### `.pth` and native-extension *content* in the wheel — the deferred bigger step

The native-extension *presence/introduced* signal above is the
metadata-level win that needs no wheel extraction. The
`.abi3.so`-trojanization-of-an-already-native-package case, and the
`.pth`-hook case generally, need *inside-the-wheel* inspection, which
the current "wheel-vs-repo is a category error" stance excludes
wholesale.

That stance is right for *diffing compiled outputs* against source — a
regenerated `.pyc` is not signal. But it is too broad: a narrow class of
wheel contents are **not** build artifacts and are signal-bearing
regardless of the source tree —

- a `.pth` file whose body is `import …; exec(…)` rather than a bare
  path addition (legitimate `.pth` files only extend `sys.path`);
- a bundled non-Python executable payload alongside the package
  (`_index.js`, a vendored Bun binary);
- a native `.so` present in the wheel with no corresponding source.

A wheel-content scanner scoped to *these shapes only* (not a full
wheel↔source diff) is a precise carve-out from the category-error rule,
not a reversal of it. Recorded as the next step; not built here.

### `docker.sock` is a container-escape primitive the catalogs miss

`sensitivePathPatterns`
(`internal/signal/source/astfeature/catalogs.go`, ~38-57) carries
`.docker/config.json` — the credential *file* — but not the daemon
*socket* `/var/run/docker.sock`. The socket is a categorically different
thing: connecting to it from a dependency is a container-escape /
host-takeover primitive, not a credential read. Library code never
contacts the Docker socket at import/build time, so this is a
near-zero-FP catalog addition; once added, every analyzer wiring
`SensitivePathReads` inherits it and it joins the
`source_evolution_concern` rare-on-benign subset
(`internal/signal/types.go`, ~613). Same shape as the Trapdoor
wallet-keystore and spadata Roblox entries already in that file. Not
built here.

### Defense evasion — blinding security tooling — is a new axis

No current signal models *code that disables defensive tooling*. The
StepSecurity blackhole has two surfaces:

- A write to `/etc/hosts` — absent from `persistencePathPatterns`
  (`catalogs.go`, ~103-121, which has `/etc/cron`, `/etc/systemd`, shell
  rc files, agent loci, git hooks). Writing `/etc/hosts` from a
  dependency is a tamper primitive with no benign import-time use; clean
  low-FP catalog add.
- A reference to a security-vendor's telemetry domain. This is the
  structural **inverse** of `exfilwatch.Hosts`
  (`internal/signal/exfilwatch/exfilwatch.go`, ~48-67): not exfil-*to*
  hosts but block-*this-defense* hosts. It belongs to the same
  bounded-host-class-corpus discipline (bufferzonecorp / Trapdoor), not
  to a vendor IOC list. A new host-class entry; not a blocklist.

### Prompt injection aimed at signatory's own analysts

The fake-system-instruction header in `_index.js` is an attack on AI
code scanners — and signatory's analysis pipeline dispatches LLM
analyst agents over fetched source. The detector already exists
(`internal/contentinjection/lexical.go` — lexical phrases + role
markers) and already runs over PR-changed source in
`internal/prdefense/scan.go`. But the **dependency-analysis path** runs
content-injection scanning **only over agent-config files**
(`internal/signal/repofiles/collector.go` `detectAgentConfig`, ~145-205),
never over a dependency's own source / payload comments. The
analyst-pipeline-self-defense surface is the gap.

Scope it carefully. "Scan all dependency source for lexical injection"
would flood on legitimate LLM libraries — the lexical primitive is
admittedly noisy on AI-topic prose
(`internal/signal/types.go`, ~984 caveat). The defensible version leans
on the **structural** primitives: invisible-Unicode / bidi / tag-block
in *source-file comments* is near-zero-FP (there is no legitimate reason
for zero-width characters in a code comment), and is exactly the carrier
a refusal-bait header would use to hide from human review. Treat the
lexical refusal-bait half as an analyst-layer composition gated on
payload shape (obfuscation, exec spike, wheel-residence), not a
standalone Layer-1 flag. Recorded as an axis; not built here.

## What this does *not* do

### Does not add IOCs to a burn list

`thebeautifulmarchoftime` / `thebeautifulsnadsoftime`, the
`*.stepsecurity.io` domains, the `Bun/1.3.14` User-Agent, the specific
package names, and any file hashes are facts of this incident, not
durable signals. Per
[bufferzonecorp §"Does not maintain a per-vendor IOC list"](2026-05-02-bufferzonecorp-campaign.md)
and reinforced in TanStack and Trapdoor, signatory does not keep
per-incident blocklists.

### Does not add `/tmp/.bun_ran` or `/tmp/.sshu-setup.js` as persistence loci

These are ephemeral run-once / staging markers in `/tmp`, not durable
persistence destinations. A `/tmp` marker is an implementation detail of
this payload, not a signal axis.

### Does not add a paste / dead-drop host blocklist

The GitHub dead-drop C2 is a legitimate-platform-as-exfil host class
(`api.github.com`), already reasoned in
[Trapdoor §"GitHub Gist as a legitimate-platform-as-exfil host class"](2026-05-24-trapdoor-crypto-stealer.md).
The signal-bearing shape is host-class + timing + verb, not a vendor
domain list.

### Does not weaken any existing positive signal

`sdist_only_present` / `sdist_only_introduced` remain correctly named —
this campaign is the *opposite* registry-shape (it adds wheels, it does
not drop them), and the native-extension pair is their inverse-window
sibling, not a replacement.

## Open questions

- Should the wheel-content scanner (the deferred `.pth`-with-code /
  bundled-executable / native-`.so` carve-out) live in the
  artifact-vs-repo collector (it already fetches sdists) or as a
  dedicated wheel-inspection collector? The former reuses the fetch
  machinery; the latter keeps the "we do open wheels, narrowly" policy
  visibly separate from the "we diff sdists against source" policy.
- Where does the defense-evasion host-class corpus belong — extending
  `exfilwatch.Hosts` with a typed "intent" (exfil vs block-defense), or
  a sibling collector? The match mechanics are identical (literal host
  substring scan over a source tree); only the semantic differs.
- `native_extension_introduced` is bounded by the recent-version window
  (`crossVersionWindow = 10`). A native extension introduced farther
  back than the window reads as always-present. Is the window the right
  bound for this signal, or should the native-transition scan walk the
  full release history (cheap — it is filename parsing, no fetch)?

## Cross-references

- [`2026-05-12-tanstack-mini-shai-hulud.md`](2026-05-12-tanstack-mini-shai-hulud.md)
  — the npm / OIDC arm of this campaign family; establishes the
  publication-integrity depth argument this entry extends to the wheel
  surface.
- [`2026-05-24-trapdoor-crypto-stealer.md`](2026-05-24-trapdoor-crypto-stealer.md)
  — the three-ecosystem arm; Python→foreign-JS-runtime primitive,
  legitimate-platform-as-exfil host class, install-time-hook tier.
- [`2026-05-02-bufferzonecorp-campaign.md`](2026-05-02-bufferzonecorp-campaign.md)
  — campaign-entry template; host-class-corpus and IOC-discipline
  precedents; the gem `native_extension_introduced` shape this entry
  ports to PyPI.
- `internal/signal/registry/pypi/collector.go` — `wheelPlatformTag`,
  `nativeWheelSummary`, `recordNativeExtensionPresent`,
  `recordNativeExtensionIntroduced`; `recordArtifactURL` (~502-507) sdist-only
  rationale; `recordReleaseSignals` wiring site.
- `internal/signal/registry/pypi/wire.go` — `Distribution.Filename` /
  `PackageType`, the metadata the native-extension signals parse.
- `internal/signal/registry/gem/collector.go` (~437-487) /
  `internal/signal/types.go` (~1000-1021) — the gem signals this entry
  reaches parity with; registry descriptions broadened to both
  ecosystems.
- `internal/signal/source/astfeature/catalogs.go` (~38-57, ~103-121) —
  `sensitivePathPatterns` (missing `docker.sock`) and
  `persistencePathPatterns` (missing `/etc/hosts`).
- `internal/signal/exfilwatch/exfilwatch.go` (~48-67) — `Hosts`; the
  defense-evasion host class is its structural inverse.
- `internal/signal/repofiles/collector.go` (~145-205) /
  `internal/contentinjection/lexical.go` /
  `internal/prdefense/scan.go` — the content-injection surface; the
  dependency-analysis path scans only agent-config files, the
  analyst-self-defense gap.
- [`../trust-model.md`](../trust-model.md) §"Signals must be weighted by
  forgery resistance" — `.pth` / `.so` install-time execution sits at
  the High tier alongside `postinstall`, `extconf.rb`, and `build.rs`.
