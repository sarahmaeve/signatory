# Possible: Homebrew ecosystem provider — feasibility and shape

Status: **speculative / future-version concern** (2026-05-04). Not on
the v0.1 or v0.2 roadmap. This document is a scoping note answering
the question "can we add brew, and does it make sense to vet brew
packaging this way" — not a green-lit implementation plan. The
expected disposition is "park this until a forcing function appears";
see "What would change this" below.

Cross-references:
- [`ROADMAP.md`](ROADMAP.md) — brew is **not** listed in the v0.2
  lane (GitLab + federation + visualization); inserting brew would
  be a roadmap addition, not a continuation
- [`rust.md`](rust.md), [`ruby.md`](ruby.md),
  [`potential-pypi.md`](potential-pypi.md) — structural templates
  for "add an ecosystem provider" plans; this document deliberately
  diverges from that template because brew is structurally a
  redistribution layer, not a registry
- `internal/signal/registry/gopublish/` — the closest existing
  precedent: an ecosystem where the "registry" mines provenance
  over an upstream repo rather than acting as the publication
  surface. The graceful-degradation pattern there
  (registry-only signals when source resolution fails) transfers
  directly to brew.
- [`signal-type-registry.md`](signal-type-registry.md) — every
  emitted signal must be pre-registered; new brew-specific signals
  would land here
- [`trust-model.md`](trust-model.md), `feedback_threat_economics.md`
  in user memory — the forgery-resistance lens used below to
  discriminate which brew signals are worth collecting

## TL;DR

1. **Mechanically feasible, low-risk to add.** The `Collector`
   interface is a one-method contract; dispatch is one switch case;
   `formulae.brew.sh` exposes a clean JSON API per formula and per
   cask. Including TDD fixtures, this is roughly a `gopublish`-sized
   piece of work.
2. **But the trust model is structurally different from
   npm/cargo/pypi/gem/maven.** Brew is a redistribution layer, not
   a publication layer. `pkg:brew/<name>` should be modelled
   like `pkg:golang/<module>` — emit redistribution-provenance
   signals and resolve to a separate upstream entity that the
   github / openssf / repofiles collectors vet independently.
3. **Forcing full cross-ecosystem signal parity is the wrong
   move.** Several of the canonical signals (`version_count`,
   `version_publish_burst`, `maintainer_count`, `owner_count`)
   either don't map or map to weakly-meaningful quantities for
   brew. Adopt what fits, skip what doesn't, add brew-specific
   forgery-resistant signals where the ecosystem actually exposes
   them.
4. **Formulae and casks deserve separate treatment.** Their threat
   models diverge sharply: formulae are bottle-built and
   source-pinned by Homebrew CI; casks frequently pull
   vendor-direct binaries with `:no_check` SHAs and
   `auto_updates: true`, which delegates trust off-platform.

## Purl type and target identification

The [purl spec](https://github.com/package-url/purl-spec) does not
yet define a stable Homebrew type at the time of writing
(2026-05-04); proposals have circulated for `pkg:brew/<name>`,
`pkg:homebrew/<name>`, and qualifier-based forms. For internal use
the natural canonical URIs are:

- `pkg:brew/<formula-name>` — homebrew/core formula
- `pkg:brew/cask/<cask-name>` — homebrew/cask
- `pkg:brew/<tap-owner>/<tap-name>/<formula-name>` — third-party tap

The ecosystem slug would be `"brew"`. None of the existing test
fixtures use brew as the example unwired ecosystem (that role is
filled by `gem` historically, now by whatever is next-unwired
after this lands).

## Why brew is structurally different

When a user runs `brew install jq`, the chain of trust is:

1. **Homebrew Core maintainers** reviewed and merged the formula
2. **Homebrew CI** built and bottled the binary, hosted on
   `ghcr.io/homebrew/core`
3. **The formula's pinned upstream URL + SHA-256** vouches for the
   source the bottle was built from
4. **The upstream project** (jqlang/jq) authored that source

The user is not directly trusting jq's author. They are trusting
Homebrew's redistribution infrastructure to faithfully package
what jq's author released — at one remove.

This differs from npm / cargo / pypi / gem / maven, where the
publisher of record on the registry is (modulo team transfer) the
software's author. Those collectors emit signals about *the
publisher*. A brew collector would emit signals about *the
redistribution channel*, then defer the upstream-author question
to a separate linked entity.

The closest existing precedent in the codebase is `gopublish`,
which mines `proxy.golang.org` and `sum.golang.org` for
publish-provenance over modules whose authoritative source lives
elsewhere. `gopublish/doc.go` notes the collector "mines
publish-provenance evidence from the two public Go-module data
planes"; it resolves the upstream source repo only to enable the
github / openssf / repofiles collectors to fire downstream, and
**falls back to registry-only signals** when source resolution
fails. That is the model brew should follow.

## Two-entity model

A `pkg:brew/jq` analysis should produce two linked outputs:

- **Distribution-layer entity** — `pkg:brew/jq`. Trust signals
  about the formula, the bottle, the tap, the Homebrew review
  history, and the install-time surface.
- **Upstream-project entity** — e.g., `github.com/jqlang/jq`,
  resolved from the formula's `homepage` and `urls.stable.url`.
  Vetted by the existing github / openssf / repofiles collectors,
  exactly as it would be if the user had asked about that repo
  directly.

These are linked but separately reasoned. An analysis conclusion
of the form "brew distribution is clean, upstream project is
unmaintained" is meaningful and currently hard to express in the
v0.1 model; brew adoption would be a forcing function for making
that link explicit. (See "Open questions" below.)

## Signal mapping

### Cross-ecosystem signals (from `c08e6be`)

| Canonical signal           | Brew analog                                                | Verdict |
|----------------------------|------------------------------------------------------------|---------|
| `last_publish`             | Last commit timestamp on the formula file                  | Clean   |
| `version_count`            | Distinct upstream versions in formula git history          | Ambiguous — measures upstream churn surfaced by Homebrew, not "versions of the formula" in the registry sense |
| `recent_downloads`         | `analytics.install.30d/90d/365d` from `formulae.brew.sh`   | Weak — opt-in client telemetry, less reliable than registry counters; document the caveat in the signal description |
| `yanked_release_count`     | Closest analog: `disabled` / `deprecated` count over time  | Weak — different semantics; probably skip |
| `version_publish_burst`    | Bursts of formula updates                                  | Skip — Homebrew's PR review and merge gating largely prevents this pattern; signal would essentially never fire |
| `maintainer_count`         | Homebrew Core has thousands of maintainers globally        | Not informative as a count; the meaningful question is "who reviewed and merged the most recent bump" |
| `owner_count`              | One (the Homebrew Project) for `homebrew/core`             | Not informative |

The shape that emerges: `last_publish` and `recent_downloads` map
cleanly enough to be worth wiring; the rest either don't apply or
need a brew-native reformulation. This is consistent with how
`gopublish` chose its own canonical set rather than mechanically
mirroring the npm signal list.

### Brew-specific forgery-resistant signals

These are the signals worth collecting because they are hard to
forge — they are verifiable against Homebrew's CI infrastructure
or against the formula's content directly.

**Formulae:**

- `bottle_present` + bottle SHA-256. Verifiable against
  `ghcr.io/homebrew/core`. Absent or rebuilt bottle is signal.
- `tap_is_homebrew_core` (boolean). Third-party taps have weaker
  review and bottle infrastructure; this is a tier indicator,
  derivable from the JSON `tap` field.
- `head_only_install` (boolean). The formula has no stable pin,
  only a HEAD git URL. High-risk, trivially detectable.
- `disabled`, `disable_date`, `disable_reason`,
  `deprecated`, `deprecation_date`, `deprecation_reason`. The
  yank-equivalents.
- `formula_pinned_source_sha`. The formula hard-pins the upstream
  source SHA-256; tampering with the source URL breaks the pin.
- `formula_last_reviewer_login` / `formula_last_merge_pr`. Derived
  from `homebrew/homebrew-core` git history; identifies which
  human (or bot) approved the most recent bump. Far more useful
  than `maintainer_count`.

**Casks (additional, often more concerning):**

- `cask_no_check_sha` (boolean — `sha256 :no_check`). Brew
  performs no integrity check on the download. Trust delegates
  entirely to the vendor's HTTPS endpoint.
- `cask_auto_updates` (boolean). The app self-updates after
  install; brew's checksum verification is moot from that point
  on. Trust delegates to the vendor's update mechanism.
- `cask_installer_script_present` (boolean). Arbitrary shell at
  install time. Rare, but worth surfacing.
- `cask_url_host`. The hostname the binary is fetched from. A
  vendor-direct domain (e.g., `dl.google.com`) and a
  github-release domain are both legitimate but materially
  different trust profiles.
- `cask_uninstall_surface`. The set of `uninstall.{launchctl,
  quit, signal, login_item, kext, pkgutil, ...}` keys reveals
  the install-time surface (a cask that needs `kext` uninstall
  installed a kernel extension; one that needs `launchctl`
  installed a daemon).

These are forgery-resistant in the sense that they are properties
of the formula/cask file itself, which is content-addressable in
the homebrew-core/cask git history. A malicious tap could lie
about them, but `tap_is_homebrew_core` already captures that
distinction.

### Forgery-prone signals to deprioritise

- `homepage`, `license` — easily wrong, no enforcement
- `caveats` text — informational, not enforced
- Self-reported install analytics — opt-in, noisy

## Why two collectors, not one

Formulae and casks share a JSON envelope but model fundamentally
different things:

| Dimension                    | Formula                                | Cask                                  |
|------------------------------|----------------------------------------|---------------------------------------|
| Build provenance             | Homebrew CI builds bottle              | Vendor-direct download (typically)    |
| Integrity check              | SHA-256 of source + bottle             | Often `:no_check`                     |
| Post-install trust           | brew owns the install                  | App often self-updates                |
| Install-time code            | Ruby formula `install` block           | Optional `installer.script` shell     |
| Typical content              | CLI tools, libraries, daemons          | GUI macOS apps                        |

Conflating them produces a signal set that is incoherent for both.
The recommended shape is two collectors:

- `internal/signal/registry/brew/` — `pkg:brew/<name>` formulae.
  Models after `gopublish`.
- `internal/signal/registry/brewcask/` — `pkg:brew/cask/<name>`.
  Different signal set centered on `cask_no_check_sha`,
  `cask_auto_updates`, `cask_installer_script_present`, and
  `cask_url_host`.

(Or one package with internal branching, if the type system
favours that. The signal sets diverge enough that two packages
read more cleanly.)

## Architectural fit (mechanical)

Following the existing pattern, in line with `rust.md`/`ruby.md`
plans:

1. Register brew-specific signal types in
   `internal/signal/types.go`. The cross-ecosystem signals already
   exist; only brew-specific additions are new rows.
2. `internal/signal/registry/brew/{client,wire,collector}.go`
   against `formulae.brew.sh/api/formula/<name>.json`. Includes
   `User-Agent`, body cap, status-code-to-sentinel mapping, name
   validation — same shape as `gem/client.go` or `cargo/client.go`.
3. `internal/signal/registry/brewcask/...` — same shape against
   `formulae.brew.sh/api/cask/<name>.json`.
4. Switch case in `cmd/signatory/collectors.go` `collectorsFor()`,
   matching `entity.Ecosystem == "brew"` and branching on whether
   the canonical URI carries a `cask/` segment. Graceful no-op
   when upstream resolution fails — same fallback pattern
   `gopublish` already uses.
5. Survey integration — analogous to PyPI's Layer 9. Out of
   scope until a manifest format demands it; brew Brewfiles are
   not commonly checked into source repos the way `Cargo.toml`
   or `package.json` are.
6. TDD with `httptest` fixtures, matching the gem/cargo patterns.
   No recorded cassettes.

Estimated work, by analogy with `cargo` (the simplest shipped
collector): 1500–2500 LoC for one ecosystem (formulae) including
tests, plus ~70% of that again for casks if they are split out.

## Open questions

These are the hard questions that need answers before this is a
real plan, not just a feasibility note.

1. **Does the v0.1/v0.2 entity model express "distribution
   entity links to upstream entity"?** `gopublish` resolves
   upstream URLs but the distribution-vs-upstream link is
   implicit (collectors fire on whichever entity the user
   targeted). For brew the link is more material, because the
   user almost always typed `brew install <thing>` rather than
   the upstream URL. If `pkg:brew/jq` analysis silently produces
   conclusions about jq the upstream project, users will
   conflate the two; if it doesn't, the analysis is too thin to
   be useful. Probably needs a "linked upstream entity" concept
   in the analysis output. See `entity-model-v2.md`.
2. **Do casks merit a separate ecosystem slug** (`"brew-cask"`)
   or a qualifier on `"brew"`? The dispatch decision in
   `collectorsFor()` is easier with separate slugs; the purl
   normalisation question is murkier.
3. **How are third-party taps modelled?** They have lower
   baseline trust than `homebrew/core`. The `tap_is_homebrew_core`
   signal captures this binary, but vetting a third-party tap
   meaningfully requires assessing the tap repo as a separate
   entity (it's just a GitHub repo of formulae). This is
   analogous to the npm scoped-registry / private-registry
   question that v0.1 also defers.
4. **Is there a manifest equivalent worth parsing?** `Brewfile`
   exists (`brew bundle`) and is occasionally checked into
   repos. Lower priority than `package.json` / `Cargo.toml`
   / `pyproject.toml` because adoption is much thinner. Probably
   skip until a forcing function arrives.
5. **Analytics signal weight.** Homebrew's install analytics are
   opt-in client telemetry. They are the only "downloads"
   number available, but they are weaker than the registry
   counters npm/pypi expose. The signal type registry would need
   a clear caveat string, and posture rules probably should not
   weigh `recent_downloads` from brew the same as from npm.

## Why not now

- **Not on the roadmap.** The committed and working-tree
  `ROADMAP.md` for v0.2 prioritise GitLab support, federation,
  visualization, cross-ecosystem correlation, and repository
  scanning. Brew is not listed.
- **Six ecosystems already shipped or stubbed.** v0.1's
  ecosystem coverage is broad (cargo, gem, gopublish, maven,
  npm, pypi). Adding a seventh before deepening the existing
  six (PyPI Layer 5 still deferred; identity tracking still
  evolving) trades depth for breadth.
- **The two-entity model question is unresolved.** Shipping
  brew before that's resolved bakes a particular interpretation
  into the store; resolving the entity-model question first
  makes brew a much smaller addition.
- **The forgery-resistance analysis above is theoretical.**
  It identifies signals worth collecting in principle, but no
  dogfood pass has been done against real brew formulae /
  casks to confirm the JSON shapes carry the fields claimed
  here at the volumes claimed.

## What would change this

Concrete forcing functions that would justify pulling brew
forward:

1. **A real brew-distributed supply-chain incident** — a
   compromised cask, a malicious tap, a bottle-build pipeline
   attack — that signatory's existing collectors cannot reason
   about because the user-facing entity is `pkg:brew/X`, not
   the upstream repo.
2. **A v0.2 dogfood target for which brew is the primary install
   path** — likely candidates are CLI tools the v0.1 target user
   (solo dev / LLM team on macOS) installs daily: `gh`,
   `kubectl`, `jq`, `ollama`, `uv`, `claude`, `signatory`
   itself once it ships a formula.
3. **Resolution of the two-entity (distribution-vs-upstream)
   model in `entity-model-v2.md`** — once that lands, brew
   becomes the natural first ecosystem to exercise it
   end-to-end, since the model is least ambiguous there.
4. **A user request via the MCP surface** for `pkg:brew/X` that
   cleanly soft-fails (the same way `signatory_analyze` now
   soft-fails on missing PyPI Layer 1). Volume here is the
   forcing function.

Until at least one of those triggers, this stays parked.
