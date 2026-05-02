# 2026-05-02: BufferZoneCorp Campaign — Cross-Ecosystem Typosquat Validates Install-Time-Execution and C2-Destination-Class Signals

## Source

Socket Threat Research, "Malicious Ruby Gems and Go Modules Steal
Secrets, Poison CI" (`socket.dev/blog/malicious-ruby-gems-and-go-modules-steal-secrets-poison-ci`,
fetched 2026-05-02). Reports a campaign of 16 weaponized packages
distributed across two ecosystems by a single GitHub identity,
`BufferZoneCorp`:

- **9 Go modules** under `github.com/BufferZoneCorp/*` —
  `grpc-client`, `go-metrics-sdk`, `go-retryablehttp`, `go-stdlib-ext`,
  `go-weather-sdk`, `net-helper`, `config-loader`, plus sleeper modules
  `log-core`, `go-envconfig`, `go-stdlog`.
- **7 Ruby gems** with `knot-` prefix — `knot-activesupport-logger`,
  `knot-devise-jwt-helper`, `knot-rack-session-store`,
  `knot-rails-assets-pipeline`, `knot-rspec-formatter-json`, plus
  sleepers `knot-date-utils-rb`, `knot-simple-formatter`.

This entry pairs with the existing in-store analysis of one Go module
in the campaign — `repo:github/bufferzonecorp/grpc-client`, posture
`rejected`, burn extending via publisher role from
`identity:github/bufferzonecorp` (synthesis output_id
`699f03a6-e1fc-4915-be2a-65c33a89e649`, security output_id
`541ffbf0-de87-4b66-be2d-57f32e56adbc`, provenance output_id
`881b9a6c-900f-457d-b65c-c0b93d4b38b9`, all 2026-05-01). The store
already carries the per-package facts; this entry codifies the
campaign-level lessons that update `signals-v01.md` and the methodology
registry.

## Why this entry exists

Signatory's analysis of `grpc-client` was strong on per-package facts
but blind to three structural properties of the broader campaign:

1. The campaign spanned **two ecosystems** under one operator. Our v0.1
   identity model burned `identity:github/bufferzonecorp` and fanned
   that burn to all 17 BufferZoneCorp repos via `via_role=publisher`.
   The Ruby gems half is invisible to us — we have no rubygems
   collector, no rubygems publisher minter, and no concept of an
   "operator" entity that groups identities sharing operational
   fingerprint across ecosystems.
2. The campaign relied on a **request-capture-as-a-service C2 host**
   (`webhook.site/<UUID>`) that is structurally suspicious independent
   of which operator runs it. Our signal set has no concept of a "host
   class" corpus — we caught the C2 fact via per-package analyst
   reasoning (F001), at analyst-token cost, when a literal-string scan
   against a small curated catalog would have flagged all 16 packages
   in milliseconds.
3. The campaign used the **sleeper / version-pumping tactic** —
   `v0.1.0` benign, malice added at `v0.4.0` — that defeats any cached
   posture decision tied to the entity rather than to a commit/version
   range. Our `MP-PROV-PUB-03` records the version-pumping fact but
   our posture row does not currently encode a version bound, so a
   consumer who vets `v0.1.0` and trusts the slot is structurally
   exposed.

The campaign is also a strong validation of the methodology patterns
we already had — every one of `MP-PROV-PKG-01` through `MP-PROV-PUB-03`
hit on `grpc-client`. The lessons here are about the gaps the patterns
*didn't* cover, not about retiring the patterns that worked.

## The campaign shape

Canonical operator playbook reconstructed from the article and our
in-store analysis:

1. Spin up a single GitHub account with a typosquat-shaped name
   (`BufferZoneCorp` ↔ `BufferZone Corp.`, a real endpoint-isolation
   vendor). Account age: 11 days at observation.
2. Mass-publish ~17 repositories over 11 days, each with a
   generic-infrastructure-noun module name (`grpc-client`,
   `go-retryablehttp`, `go-envconfig`, `go-metrics-sdk`). The names
   are generic-noun typosquats of canonical packages — `grpc-client`
   reads as "the gRPC client" against `google.golang.org/grpc`,
   `go-retryablehttp` against `hashicorp/go-retryablehttp`,
   `go-envconfig` against `kelseyhightower/envconfig`.
3. Within each repo, ship four lightweight tags `v0.1.0` through
   `v0.4.0` over ~72 hours. Backdate the LICENSE to a year that
   predates the repo's first commit. Omit CI, CHANGELOG, CONTRIBUTING,
   SECURITY, and CODEOWNERS — every standard governance file.
4. Pre-stage benign initial versions; weaponize the latest version
   with the actual payload, so a consumer sampling `v0.1.0` sees a
   clean repo and a consumer doing `go get @latest` retrieves the
   malicious zip.
5. Publish parallel Ruby gems under a `knot-` prefix to expand
   ecosystem coverage. Different distribution surface, same operator
   intent.
6. The malicious payload fires from the language's import-time
   execution hook (Go `init()`, Ruby `extconf.rb`), reads the host's
   credential set (`~/.ssh/`, `~/.aws/`, `~/.kube/`, `~/.docker/`,
   `~/.npmrc`, `~/.netrc`, `~/.config/gh/hosts.yml`, AWS IMDS,
   Kubernetes pod-projected SA tokens), and POSTs an XOR-encoded
   payload to `webhook.site/<UUID>`. CI runners get extra: env writes
   that disable Go module integrity (`GONOSUMCHECK=*`, `GONOSUMDB=*`),
   `GOPROXY` redirect, a build-tool shim prepended to `$GITHUB_PATH`
   (`kubectl` in `grpc-client`; a fake `go` in `go-retryablehttp`),
   and `go.sum` line removal. Persistence is a post-commit git hook
   walking up to six parent directories, *and* in `go-stdlib-ext`, an
   append of a hardcoded `deploy@buildserver` SSH key to
   `~/.ssh/authorized_keys`.

The exported public API of each package is decorative — `grpc-client`'s
`Connection.Ping` shells out to `nc` and the package does not import
`google.golang.org/grpc`. The surface exists to make a downstream
consumer who runs `godoc` or skims `client.go` see something
plausible-looking before importing.

## What this validates in our existing model

Every methodology pattern we registered hit on `grpc-client`:

- `MP-PROV-IDG-01` (young owner + single-author) — HIT.
- `MP-PROV-IDG-02` (LICENSE backdating) — HIT.
- `MP-PROV-GOV-01` (bus-factor-1 with anonymized noreply email) — HIT.
- `MP-PROV-HYG-01` (governance scaffolding empty) — HIT.
- `MP-PROV-PUB-01` (lightweight tags, signed_ratio=0) — HIT.
- `MP-PROV-PUB-02` (no CI + multiple releases) — HIT.
- `MP-PROV-PUB-03` (version pumping over short window with no tests) — HIT.
- `MP-PROV-PKG-01` (init() egress + credential file reads) — HIT.
- `MP-PROV-PKG-02` (`$GITHUB_ENV` / `$GITHUB_PATH` mutation,
  `GONOSUMCHECK` / `GONOSUMDB` writes) — HIT.
- `MP-PROV-PKG-03` (decorative public API not importing the library
  it claims to wrap) — HIT.

The forgery-resistance ladder collapsed cleanly: F001-F003 and PROV-002
cite the artifact directly, which is the strongest evidence class
available. The two analysts converged independently on the same
verdict from orthogonal evidence streams. The synthesis correctly
assigned `rejected` and the burn correctly fanned out via the
publisher role.

## What this exposes as a gap

### Cross-ecosystem operator correlation

Our v0.1 entity model knows `identity:github/bufferzonecorp` and the
17 GitHub repos it owns. It does not know:

- The rubygems publisher of `knot-*` is operationally the same actor.
- The XOR key, exfil endpoint shape, install-hook silhouette, and
  version-pumping cadence are shared across both ecosystem halves
  of the campaign.

The collector matrix has shipped `git` (Path F, GPG signers) and
`pypi` (Path E, publishers) identity minters. **A `rubygems`
publisher minter is the next entry on that path.** Beyond the
collector, the campaign motivates a `campaign:` or `operator:` entity
URI that groups identities sharing operational fingerprint. Today the
burn at `identity:github/bufferzonecorp` is correct but incomplete —
the rubygems half of the operator's surface is uncovered. Drafting
the campaign-entity shape is a v0.2 candidate; the rubygems collector
is a v0.1 candidate.

### Install-time execution methodology pattern is Go-specific

`MP-PROV-PKG-01` cites Go `init()` and `~/.ssh`. The same shape lives
in:

| Ecosystem | Hook |
|-----------|------|
| Go | `init()` |
| Python | `setup.py`, top-level `__init__.py` |
| Ruby | `extconf.rb`, `gemspec.post_install_message` |
| npm | `preinstall` / `install` / `postinstall` |
| Cargo | `build.rs` |

The pattern should be **ecosystem-abstracted** at the parent level
("code that runs as a side-effect of install or first-import"), with
ecosystem-specific child patterns under it. Without the abstraction
we will rebuild the parallel methodology tree once per ecosystem,
which is exactly the duplication the methodology registry was
designed to prevent.

### Credential-target list is incomplete

`MP-PROV-PKG-01` enumerates `~/.aws`, `~/.ssh`, `~/.kube`, `~/.npmrc`,
`~/.netrc`, `~/.docker`, `~/.config/gh`. The article confirms three
additions:

- **AWS IMDS / GCP / Azure metadata** — `169.254.169.254/latest/meta-data`
  and the IMDSv2 token dance, `metadata.google.internal`,
  `169.254.169.254/metadata/identity/oauth2/token`.
- **Kubernetes pod-projected SA tokens** at
  `/var/run/secrets/kubernetes.io/serviceaccount/`.
- **`~/.ssh/authorized_keys` *append*** (not just *read*) — distinct
  persistence vector from the post-commit hook in F003. Worth a
  separate conclusion category (`persistence/ssh-authorized-key`).

### Build-tool PATH shim generalization

F002 caught a `kubectl` shim on `$GITHUB_PATH`. The article shows
`go-retryablehttp` shipped a fake `go` executable. Shim impersonating
the build/runtime tool's *own* name (`go`, `python`, `ruby`, `node`,
`npm`, `cargo`) prepended to a write-controllable PATH is a strict
specialization of the kubectl-shim pattern and is materially worse,
because it intercepts every subsequent build step in the same
workflow. Worth a dedicated sub-pattern.

### Sandbox-evading delayed-fire goroutine

`MP-PROV-PKG-01` says "init() egress within 200ms." A package using
`go func(){ time.Sleep(N); exfil() }()` from init() looks quiescent
in any naive sandbox-then-snapshot scan but is functionally identical.
Add a sub-pattern: "delayed-fire goroutine spawned from init()" with
explicit acknowledgement that the 200ms threshold misses it.

### Module-name typosquat (not just publisher-name typosquat)

`PROV-001` caught the publisher-name typosquat (BufferZoneCorp ↔
BufferZone Corp.). We did **not** flag that `grpc-client` is itself
a generic-noun typosquat. The pattern: a module path uses a generic
infrastructure noun without an owner-namespace differentiator, while
a high-blast-radius package with the same noun exists under a known
maintainer namespace. Concrete pairs in this campaign:

- `BufferZoneCorp/grpc-client` ↔ `google.golang.org/grpc`
- `BufferZoneCorp/go-retryablehttp` ↔ `hashicorp/go-retryablehttp`
- `BufferZoneCorp/go-envconfig` ↔ `kelseyhightower/envconfig`
- `BufferZoneCorp/go-stdlib-ext` ↔ Go standard library

Cheap to compute against a curated allowlist of canonical
high-blast-radius package roots per ecosystem.

### C2-destination-class as a corpus signal

The single biggest cheap-shot win the campaign reveals. Every package
exfiltrated to `webhook.site/<UUID>`. No legitimate package's
import-time code should POST to a free, account-less, ephemeral
HTTP-capture-as-a-service host. A literal-string scan against a small
curated catalog of host classes (`webhook.site`, `requestbin.com`,
`beeceptor.com`, `pipedream.com/v1/sources/*`, `interact.sh`,
`oast.live`, `oast.fun`, `requestcatcher.com`, `postb.in`, `smee.io`)
would have flagged every package in the campaign in milliseconds, at
~$0 cost, with VERY HIGH forgery resistance for the literal match.

The architectural care this signal needs — staying a *typology* of
host classes rather than degrading into a vendor IOC list — is the
subject of a dedicated design document
([`host-class-corpus-signal.md`](../host-class-corpus-signal.md)).

### Sleeper / version-pumping defeats entity-bound posture

The campaign exploits the gap between "entity vetted" and "version
vetted." A consumer who vets `v0.1.0` of `grpc-client` and stores a
posture against `repo:github/bufferzonecorp/grpc-client` has no
mechanism in v0.1 to invalidate that posture when `v0.4.0` is
published with a payload. `target_commit` is recorded on conclusions
but the posture row itself is entity-scoped. A posture should bind to
a commit/version range, with explicit invalidation on new
publications under the slot. The `takeover-bait` composite already
implies this as a *monitoring commitment*; lift it from
composite-only to the posture record so consumers can't unknowingly
cache an old-version posture against a new fetch.

### MITRE ATT&CK technique tags

The Socket writeup maps the campaign to T1195.001, T1552.001/.004/.005,
T1098.004, T1574.007, T1036.005. Our Conclusion records have a
`category` field (`malware`, `ci-subversion`, `persistence`,
`obfuscation`, `typosquat-camouflage`, `command-injection`,
`filesystem-arbitrary-read`) but no MITRE technique. A
schema-additive `mitre_techniques[]` field on Conclusion lets
downstream tools cross-reference threat-intel platforms without
breaking existing consumers. Cheap.

### External advisory cross-reference

The synthesis explicitly listed "No OSV/GHSA cross-check" as a gap
(synthesis output_id `699f03a6-e1fc-4915-be2a-65c33a89e649`,
gap[3]). Add `external_advisory` as a Layer-1 signal type:
`{source: socket|osv|ghsa|proxy.golang.org-takedown, url, advisory_id, ingested_at}`.
Then `signatory_summary` can show "burned internally + N external
advisories concur" — exactly the kind of forgery-resistance
reinforcement the trust model values, with cheap collection. Both
Socket and OSV are GET-only queryable, fitting the WebFetch
architectural constraint.

## Impact on `signals-v01.md` and the methodology registry

This entry motivates the following updates. Each is a separate piece
of work; this list is the index, not the work itself.

1. **New signal type**: `c2_destination_class` under Publication
   Integrity, sourced from package source / sdist scan against a
   host-class corpus. See `host-class-corpus-signal.md` for the
   detailed design.
2. **New signal type**: `external_advisory` under Publication
   Integrity, sourced from Socket / OSV / GHSA / proxy.golang.org
   feeds.
3. **New methodology pattern** `MP-PROV-PKG-04`: package source
   contains hostname literal in request-capture-as-a-service host
   class. Composes with `MP-PROV-PKG-01` as compounding confirmation.
4. **Generalize `MP-PROV-PKG-01`** to ecosystem-abstracted
   "side-effect of install or first-import" with Go / Python / Ruby /
   npm / Cargo child patterns. Existing Go-specific text becomes the
   Go child.
5. **Extend `MP-PROV-PKG-01` credential-target list** to include AWS
   IMDS / GCP / Azure metadata endpoints, Kubernetes pod-projected SA
   tokens, and `~/.ssh/authorized_keys` *writes* (separate from
   *reads*).
6. **Extend `MP-PROV-PKG-02` CI-poisoning sub-patterns** to include
   `go.sum` line removal and `GOPROXY` env override, alongside the
   existing `GONOSUMCHECK` / `GONOSUMDB` writes.
7. **Add sub-pattern under `MP-PROV-PKG-02`**: build-tool PATH shim
   impersonating the toolchain executable name (`go`, `python`,
   `ruby`, `node`, `npm`, `cargo`).
8. **Add sub-pattern under `MP-PROV-PKG-01`**: delayed-fire goroutine
   spawned from init() — defeats naive sandbox-then-snapshot scans.
9. **Promote OBS-002 to first-class pattern** `MP-PROV-IDG-03`:
   ≥N repos published in ≤M days from an account younger than P days
   → coordinated-typosquat-campaign prior. Wire as a pre-emptive
   corpus search.
10. **New methodology pattern** `MP-PROV-IDG-04`: module path uses
    generic infrastructure noun without owner-namespace differentiator
    while a high-blast-radius package with the same noun exists under
    a known maintainer namespace.
11. **Schema-additive field** `mitre_techniques[]` on Conclusion.
12. **Posture record gains a `version_scope` bound** so a posture
    decision attaches to a commit/version range, with explicit
    invalidation on new publications under the slot.

## Cross-ecosystem rubygems collector

Following the path established by Path E (PyPI publisher minter,
commit `5880821`) and Path F (git GPG signer minter, commit
`648f6bd`), a rubygems publisher minter is the next collector. The
campaign is the motivating instance: without it, the rubygems half
of `BufferZoneCorp` is uncovered by any burn extension.

The rubygems API exposes publisher metadata at
`rubygems.org/api/v1/owners/<gem>`, which is GET-only and fits the
WebFetch architectural constraint. The minted entity is
`identity:rubygems/<publisher>` with the same `via_role=publisher`
edge that Path E built for PyPI.

## What this does *not* do

### Does not maintain a per-vendor IOC list

The C2-destination-class corpus is a *typology* of host classes
(request-capture-as-a-service, OOB-interaction, tunnel-as-a-service)
with explicit class definitions and bounded membership. It is not
"a list of bad domains we've seen in malware." The class-definition
gate is what keeps the corpus from drifting into a vendor IOC list —
see `ANTIPATTERNS.md` and `host-class-corpus-signal.md` §"Staying a
typology, not a blocklist."

### Does not promote any ecosystem to a first-class collector ahead of design

The rubygems collector path is named because the campaign motivates
it. The actual collector design follows the same template as Path E
/ Path F and is not specified in this entry.

### Does not retroactively update `grpc-client`'s posture

The synthesis's posture decision (`rejected`) and burn
(`identity:github/bufferzonecorp` via publisher) stand. The lessons
here propagate to *future* analyses, not back to a closed one. The
synthesis output already records the correct verdict at the right
forgery-resistance level for the evidence available; new methodology
patterns improve detection on the next campaign, not on this one.

### Does not add `webhook.site` to a burn list

Per the signal-vs-IOC distinction, `webhook.site` as a host is not
"burned" — it is a member of a host class whose presence in package
init code is a structurally suspicious signal. The class membership
is durable and reviewed; no individual host is being treated as
malicious in itself.

## Open questions added to `design/open-questions.md`

- Should the `campaign:` / `operator:` entity URI be a v0.2 design or
  deferred further? The two-ecosystem case is concrete and small;
  the abstraction is straightforward; the cost is a new entity-type
  invariant and a fingerprint-matching collector. Worth scoping.
- Should the host-class corpus ship as a hard-coded Go file, an
  embedded YAML asset, or a separate file pulled at run-time? See
  `host-class-corpus-signal.md` §"Catalog distribution" for the
  trade-offs.
- How does posture `version_scope` interact with the existing
  `target_commit` field on conclusions? Are they redundant, layered,
  or do they answer different questions (analysis-was-of vs.
  posture-applies-to)?
- Should the synthesist's `gaps` field be wired as input to a
  collection-task generator, so emitted gaps become queued
  follow-ups rather than free-text suggestions? This was named in
  the in-store synthesis but not yet acted on.

## Cross-references

- [`../../design/host-class-corpus-signal.md`](../host-class-corpus-signal.md)
  — design document for the C2-destination-class corpus signal,
  motivated by this entry.
- [`../signals-v01.md`](../signals-v01.md) — signal-set updates
  derived from this entry: `c2_destination_class`,
  `external_advisory`, expanded credential-target list, build-tool
  PATH shim sub-pattern.
- [`../signal-type-registry.md`](../signal-type-registry.md) —
  registration of new signal types.
- [`../entity-model-v2.md`](../entity-model-v2.md) — the entity model
  that the rubygems publisher minter and a future `campaign:` /
  `operator:` URI extend.
- [`../trust-model.md`](../trust-model.md) §"Signals must be weighted
  by forgery resistance" — corpus signal lands at HIGH for the
  literal-match layer, MEDIUM at the intent layer (obfuscation
  defeats it).
- [`../trust-policy-v1.md`](../trust-policy-v1.md) — posture
  evaluator that needs to know about `version_scope`.
- [`../ANTIPATTERNS.md`](../ANTIPATTERNS.md) §"No per-vendor IOC
  list" — the discipline that keeps the host-class corpus a typology
  rather than a blocklist.
- [`2026-04-21-vercel-contextai-incident.md`](2026-04-21-vercel-contextai-incident.md)
  — parallel "named external incident motivates a new signal axis";
  this entry is the supply-chain-malware analogue.
- In-store synthesis output `699f03a6-e1fc-4915-be2a-65c33a89e649`
  (`signatory_show_synthesis target=repo:github/bufferzonecorp/grpc-client`)
  — per-package facts. This entry is the campaign-level abstraction
  layered above that synthesis.
