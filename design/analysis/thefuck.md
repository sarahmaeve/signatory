# thefuck (https://github.com/nvbn/thefuck)

**Role: Development tool (shell-augment / command interception)**
**Decision: Analysis only — no posture recorded**
**Recommended posture if adopted: Trusted-for-now, with the project's fallow status as the dominant caveat**
**Date: 2026-04-14**

## Provenance of this analysis

This is signatory's first **structured-emission** dual-analyst engagement. Both
analysts produced JSON conforming to the v1 schema in
`internal/exchange/`; both files passed `signatory format-check`
without modification. The synthesis below integrates them.

| Analyst | Role | Output |
|---------|------|--------|
| Provenance | Metadata, git history, `.mailmap`, lockfile composition, signing posture, package-registry signals | [`thefuck-provenance-v1.json`](../../filestore/analysis/thefuck-provenance-v1.json) |
| Security | Source-code threat modeling, file:line citations, behavioral analysis | [`thefuck-security-v1.json`](../../filestore/analysis/thefuck-security-v1.json) |

The two analysts ran independently on the same git SHA. Provenance was
me (`signatory-provenance` analyst, model claude-opus-4-6, round 1)
running a v1-JSON-emitting variant of the `vet-dependency` skill
methodology. Security was a separate Claude Opus 4.6 instance running
under the security-focused system prompt at
`/tmp/security-review-handoff.md` (not committed; the next commit
should promote that into a checked-in handoff template — see
"Methodology" below).

Neither analyst read the other's output before emitting. Synthesis
followed.

## Framing notes

- **thefuck is not a signatory dependency.** This file lives in
  `design/analysis/` (external projects analyzed for trust-model
  validation) rather than `design/dogfood/` (signatory's own deps).
- **The user's intake question** was "likelihood of leaking
  credentials or code?" That question maps directly onto the dual
  analysis: code-side leakage is the security analyst's beat;
  artifact-side leakage (could a hostile release reach users?) is
  provenance's beat. Both surfaces are addressed below.
- **No posture is recorded.** Per signatory's memory rule
  ("Always record trust analyses"), this analysis is persisted; the
  posture decision belongs to a consuming organization, not to
  signatory itself.

## TL;DR

For the question "will adopting `thefuck` leak my credentials or
code?" the integrated answer is:

- **Code-running risk is moderate but bounded by opt-in modes.**
  Three on-disk leak vectors (instant-mode TTY log, `--debug` env
  dump, forced `GIT_TRACE=1`) and one runtime amplification (re-run
  of the failing command). All have explicit triggers — none fire on
  a default install where the user just types `fuck`.
- **Artifact-supply risk is structural and unaddressed.** PyPI
  publish goes through `release.py` with `twine upload` from the
  maintainer's local environment. No CI publishing. No PyPI Trusted
  Publishing. No release signing. No tag signing. No commit signing
  beyond GitHub's web-flow merges. If a hostile release is ever
  published, there's no chain-of-custody mechanism to detect it.
- **The dominant trust signal is dormancy.** Last PyPI release
  2022-01-02. Last commit 2024-01-25. 5+ open PRs from March 2026
  unreviewed. Open issue #1566 (2026-04-05) literally asks if the
  project is maintained. Whatever the security review surfaces —
  even low-severity conclusions — has no remediation path beyond
  forking.
- **Counterbalancing positives exist.** No telemetry, no phone-home
  endpoints, no credential storage, no install-time stealth
  execution (security's positive absences). Clean PyPI-only
  dependency tree, no git pins, no alternative registries
  (provenance's positive absences). Default `require_confirmation =
  True` and the catastrophic rules (`rm_root`, `git_push_force`)
  ship `enabled_by_default = False` (security F010 + positive
  absence). Maintainer identity is strong (15-year GitHub tenure,
  cross-platform consistency, real name).

## Convergence: where both analyses reinforced each other

### The "credentials or code" question has two surfaces

The user's intake question maps neatly onto the dual-analyst split:

| Surface | Provenance's view | Security's view |
|---------|-------------------|-----------------|
| Does the **running code** leak? | Out of scope by methodology | F003 (instant-mode log), F006 (debug env), F007 (GIT_TRACE) — three specific vectors, all conditional on opt-in modes |
| Could a **hostile published artifact** leak? | F002 (local-laptop publish + no signing) is the unguarded path; F003+F004 (no tag/commit signing) eliminate detection | Doesn't address (out of methodology scope) |

Each analyst answered exactly half the question. Neither could have
answered both alone. The synthesist's job here is not to merge
overlapping conclusions but to lay the two halves side-by-side and
note that the user's question requires both.

### Action amplification compounds with maintenance gap

- Security **F005**: re-run via `Popen(shell=True)` duplicates any
  side-effecting command (HTTP POST, kubectl delete, partial git
  push, migrations) when the user invokes `fuck`. Severity: medium.
- Provenance **F001**: no commits in 2025-2026, no PyPI release
  since 2022. Any "could be fixed in a future release" mitigation
  is unavailable.

A maintained project's medium-severity conclusion has a remediation
path (file an issue, contribute a PR, wait for next release). For
thefuck, the path doesn't exist — the medium becomes effectively
permanent. This matters across all of security's conclusions: F003
through F009 are all "could be tightened upstream" issues that won't
be tightened, because there's no upstream to tighten them.

### Plugin discovery surface

- Security **F001**: any `thefuck_contrib_*` package on `sys.path`
  is auto-imported and its `rules/*.py` files are executed.
  Documented plugin model, but no allowlist mechanism.
- Provenance: no conclusions directly here, but **positive absence**
  confirmed there are no git-pinned deps and no alternative
  registries — so the typosquatting surface is exactly "PyPI itself"
  rather than "PyPI + git URLs + private indexes."

The combined picture: the typosquat-into-`thefuck_contrib_*`
attack requires PyPI-side malice (publishing a malicious
`thefuck_contrib_<typo>` package) plus a victim with both `thefuck`
installed and that typosquat resolved into their interpreter. PyPI's
organizational scrutiny narrows the threat but doesn't eliminate it.

## Divergence and complementary conclusions

### Security F010 [positive] strengthens provenance's silent positive

Security explicitly searched for telemetry, phone-home, hardcoded
URLs, and Sentry/posthog/segment/rollbar imports — found none. Marked
as a positive conclusion (`severity: positive`).

Provenance noted no hardcoded callbacks but didn't actively look for
telemetry libraries (out of methodology scope — that's a code-reading
pattern). The dual conclusion produces: *high confidence that thefuck
itself doesn't ship telemetry, even though it could plausibly have
been added at any point in its 11-year history.*

This is a worked example of the dual-analyst architecture's
self-confirmation property: when both analysts arrive at the same
absence-of-vector conclusion through different methods, confidence
compounds.

### Provenance F004 (no commit signing) doesn't change the security picture

Security side doesn't touch commit signing — it's not relevant to
"what can the running code do." But it matters for the runtime risk
analysis because it answers a different question: if a malicious
commit appears in the repo, can we cryptographically attribute it?
Answer: no. Combined with the dormant-maintainer profile, this means
a hypothetical future "thefuck v3.33" release would have no
git-level chain-of-custody beyond GitHub web-flow merge signing.

Not a synthesis "win" exactly — more a clean handoff: each analyst
covered the part of the question they should cover, and the answers
compose without contradiction.

## Cross-cutting insight: dormancy is the risk multiplier

The most useful cross-conclusion observation is one neither analyst
could have produced alone. Looking at the security severity
distribution:

- 1 medium (F001 plugin discovery, design intent)
- 1 medium (F005 re-run amplification, design intent)
- 7 low (F002, F003, F004, F006, F007, F008, F009)
- 1 positive (F010 no telemetry)

Compare to atuin's distribution from the prior engagement: similar
shape — most conclusions are low-severity hygiene gaps, with a couple of
medium architectural surfaces. For atuin, those are reasonable
"watch this on each release" items because atuin ships releases. For
thefuck, **provenance F001's high-severity vitality conclusion
multiplies all the security conclusions**: each one is permanent, not
provisional. The signatory trust model already encodes "criticality
as multiplier" (per `design/trust-model.md`); this engagement
demonstrates *fallow status as multiplier* — a related but distinct
amplifier.

Worth registering as a signal type (see "Signals Surfaced That Didn't
Fit" below).

## Direct answer to Sarah's intake question

> "Likelihood of leaking credentials or code?"

**Credential leakage in current code: low-to-moderate, opt-in
gated.**

Three concrete paths, each requiring an explicit user action:

1. **Instant mode (F003)** — if enabled via
   `--enable-experimental-instant-mode`, every TTY-rendered line of
   the user's shell session is written to a `/tmp/thefuck-script-log-*.log`
   file with umask-inherited permissions (typically world-readable on
   default Linux). Includes `cat ~/.aws/credentials`, `aws sts
   get-caller-identity` output, `gh auth status` output, anything
   else echoed to the terminal. Random uuid4 filename prevents
   pre-creation but not local enumeration. Strongest leak vector.
2. **`--debug` mode (F006)** — formats `os.environ` (including all
   `*_TOKEN`, `*_KEY`, `AWS_*`, etc.) into stderr. Trivially leaks
   when users paste debug output into bug reports. Not enabled by
   default.
3. **Forced `GIT_TRACE=1` on every git subprocess (F007)** —
   captures URL-embedded git credentials (`https://user:token@host/repo`)
   into `Command.output`, which is then reachable via debug logs and
   instant-mode logs. Composes with #1 and #2 above; the credential
   exposure surface widens when multiple of these are active.

Mitigations: don't enable instant mode; don't share `--debug`
output; if you must use `--debug`, redact environment variables
before sharing.

**Code leakage: not a current-version risk.** thefuck does not
exfiltrate source code or stored data — there are no network
endpoints, no telemetry, no credential storage. Security F010 +
the four positive absences confirm this thoroughly.

**Future-version risk: structurally elevated.** If a hostile
`thefuck` release is ever published to PyPI, the publisher chain
provides no detection mechanism (provenance F002, F003, F004). For
adopters, this means: the safest posture is "pin to v3.32, never
auto-update."

## Adoption guidance

If your organization is considering adopting thefuck:

**Acceptable use cases:**
- Personal/individual developer convenience tool, single-user
  workstation, with the v3.32 pinning above.
- One-off `pip install --user thefuck` followed by a known config.

**Use cases requiring active mitigation:**
- Multi-user shared host (Linux server, devcontainer cluster) — if
  enabled, F003 (instant-mode log perms) and F002 (shelve cache
  perms) become medium-severity rather than low.
- CI runners — where re-run amplification (F005) interacts badly
  with destructive commands, and where the ssh_known_hosts MITM
  bypass (F004) and git hook bypass (F008) UX patterns can train
  bad reflexes.

**Hard-stops (don't adopt):**
- Anywhere the abandoned-publisher-token risk is unacceptable. If
  your security model can't tolerate "the next release of this
  package, if one ever ships, may not be from the legitimate
  maintainer," don't pull thefuck into your dependency tree.
- Production environments. Not designed for non-interactive use;
  re-run amplification is unsafe.

## Action items, prioritized

For an adopter:

1. **Pin to v3.32 explicitly.** Disable auto-update.
2. **Do not enable instant mode** (`--enable-experimental-instant-mode`)
   unless you understand the F003 leakage profile and have set the
   right umask.
3. **Configure `~/.config/thefuck/settings.py`** to disable the
   default-on rules with weak security UX — at minimum
   `ssh_known_hosts` (F004) and `git_hook_bypass` (F008).
4. **Don't paste `--debug` output into bug reports** without
   redacting environment variables.
5. **For shared-host or container deployments**, additionally:
   verify `~/.cache/thefuck` perms are 0o600; consider mounting
   `/tmp` per-user; pin the `thefuck_contrib_*` package set if any
   contrib packages are used.

For upstream (if anyone ever picks up maintenance):

- Highest-leverage: migrate to PyPI Trusted Publishing (F002).
- Second: restrict `thefuck_contrib_*` to an opt-in allowlist (F001).
- Third: explicit `mode=0o600` on the instant-mode log (F003).
- Lower priority but cheap: `commonpath` containment check in
  `dirty_untar`/`dirty_unzip` (F009); credential redaction in
  debug output (F006); scope `GIT_TRACE` to alias-detection only
  (F007).
- Hygiene: `.github/dependabot.yml` (provenance F005); `pip-audit`
  in CI (provenance F005); `.mailmap` to consolidate dual-name
  identities (provenance F006).

## Signals Surfaced That Didn't Fit Current Schema

The synthesis exercise itself surfaced two new candidate signal
types beyond what each individual analyst found:

- `fallow_status_amplifier` — when a project's vitality score
  drops below an "actively maintained" threshold, every other
  conclusion's effective severity should be amplified (because there's
  no remediation path). This is a meta-signal that operates on the
  rest of the signal set, similar to how `criticality` amplifies
  per the existing trust model.
- `dual_analyst_self_confirmation` — when both analysts independently
  report the absence of the same pattern (e.g., security F010 "no
  telemetry" + provenance positive absence on hardcoded callbacks),
  the resulting positive-absence confidence is higher than either
  alone. Worth representing as a derivable signal in the synthesist
  output rather than just inferring it ad-hoc.

Both should be added to `design/signal-storage-evolution.md` in a
follow-up commit.

## Methodology notes

This engagement validated several architectural claims:

1. **Schema generalizes across ecosystems.** The v1 schema was
   designed against atuin (Rust). It accepted thefuck (Python) with
   zero modification — both analysts emitted valid v1 JSON that
   passed `signatory format-check`.
2. **Dual-analyst structured handoff works at the format level.** No
   re-shaping needed at synthesis time; the structured fields
   composed cleanly. The two `methodology_trace` catalogs (14 +
   14 = 28 patterns) are independent enough to be a useful joint
   reference for Layer 1 collector design.
3. **Two real tooling gaps were found and closed during the
   engagement.** Both analysts reached for ad-hoc validation
   (security agent did its own JSON checks; provenance side started
   `cat`-ing into a temp test file). Result: `signatory format-check`
   command shipped (commit `9016d73`). Both agents would have
   benefited from a structural overview without prose; result:
   `signatory format-check --summary` shipped (commit `f1706f4`).
4. **The security analyst's TL;DR directly addressed the user's
   intake question.** Worth noting as a methodology pattern for the
   security handoff template: include the user's question verbatim
   in the prompt and require the analyst's `round_notes` to address
   it.

The security handoff template at `/tmp/security-review-handoff.md`
should be promoted into the repo (probably at
`templates/handoffs/security-review-v1.md`) so future engagements can
reuse it. Provenance handoff (Option A from the prior session) is
the natural follow-up.

## Related artifacts

- Raw analyst outputs: [`thefuck-security-v1.json`](../../filestore/analysis/thefuck-security-v1.json),
  [`thefuck-provenance-v1.json`](../../filestore/analysis/thefuck-provenance-v1.json)
- v1 schema: `internal/exchange/types.go`
- Trial validation that drove the v1 schema:
  [`atuin-schema-trial-feedback.md`](atuin-schema-trial-feedback.md)
- Architecture: `design/mcp-dual-analyst-architecture.md`
- Prior engagement on a Rust target: [`atuin.md`](atuin.md)
