# Photon (https://github.com/komoot/photon)

**Role: Service / self-hosted geocoder**
**Decision: Analysis only — no posture recorded**
**Recommended posture if adopted: Trusted-for-now, with single-maintainer dominance as the primary caveat**
**Date: 2026-04-15**

## Framing notes

- **Photon is not a signatory dependency.** This file lives in
  `design/analysis/` (external projects analyzed for trust-model
  validation) rather than `design/dogfood/` (signatory's own deps).
- **Photon is a Java project**, which is outside signatory's Go
  ecosystem. The trust model is ecosystem-agnostic; this exercises the
  Java signal collection path.
- **Target kind: self-hosted service.** Photon is an OSM geocoder
  backed by OpenSearch. It runs as a long-lived HTTP server processing
  geographic queries. Deployment involves downloading a ~95 GB
  pre-built database dump from a third party (GraphHopper at
  `download1.graphhopper.com`). This is neither a library nor a CLI
  tool — the blast radius includes the host's network surface and any
  data flowing through the geocoding API.

## Dependency Role

| Aspect | Value |
|--------|-------|
| Role | Service (self-hosted geocoder) |
| Blast radius | Network-exposed HTTP service; compromise means attacker controls geocoding responses and has a foothold on the host. Database dump fetched from third-party infrastructure adds a second trust surface. |
| Runtime requirements | Java 21+, OpenSearch 3.x (embedded or external), 64 GB RAM recommended for planet-wide data |
| Data pipeline trust chain | OSM data → Nominatim import → photon extract → OpenSearch index. The Nominatim import step is maintained by the same lead developer (lonvia). |

## Signal Table

| Signal | Value | Assessment |
|--------|-------|------------|
| **Vitality** | | |
| Last commit | 2026-04-15 (today) | Active development. Era 3 (Modern AI). |
| Total commits | 1,691 over 12.7 years | Moderate. Not write-once, not hyperactive. |
| Release cadence | 5 releases in ~9 months (0.7.0 through 1.0.1); 1.0.0 on 2026-02-11 | Accelerating. The project recently hit a maturity milestone. |
| Open issues | 41 | Moderate backlog; not runaway. |
| Commit activity shape | Accelerating — see distribution below | Positive vitality signal. |
| **Governance** | | |
| Contributor count | 15 with ≥7 commits | Healthy breadth for a niche tool. |
| Top contributor dominance | lonvia: 772/1,691 = 45.7% | High single-maintainer concentration. |
| Top-3 concentration | 1,244/1,691 = 73.6% (lonvia 772, christophlingg 350, yohanboniface 122) | Steep drop-off after top 2. |
| Owner type | Organization (komoot) — commercial outdoor navigation company, est. 2012 | Institutional backing. |
| lonvia org membership | Not listed in public komoot org members | May be outside collaborator or private member. Identity is well-established in OSM ecosystem regardless. |
| Maintainer account ages | lonvia: 2012 (14 yr), christophlingg: 2012 (14 yr), yohanboniface: pre-2013 | All long-tenured. No young-account anomaly. |
| Commit signing | 7/30 recent commits verified (23%), all by lonvia, all merge commits only | Partial signing. Lead signs merges via GitHub web UI; direct pushes and contributor commits are unsigned. |
| **Publication** | | |
| Tags vs. releases | Tags include legacy names (`export-by-country-county-wise`, `v0.1`); recent tags map cleanly to releases | Mixed historical discipline, clean recent practice. |
| Release artifacts | JAR files on GitHub Releases | No registry publication (not on Maven Central). Distribution is GitHub-only. |
| Trusted publishing | N/A — no registry involvement | No OIDC, no signing. GitHub Release is the publish vector. |
| **Hygiene** | | |
| CI present | Yes — `ci.yml` with build (Java 21+25 matrix), integration import test, backwards-compat test | Strong CI for a project this size. |
| Action pinning | All third-party actions pinned to full SHA | Best-practice supply-chain hygiene for CI. |
| CVE remediation | `build.gradle` forces `commons-io ≥ 2.20.0` (CVE-2024-47554) and `io.netty = 4.2.10.Final` (CVE-2025-67735) | Proactive. Maintainers track and remediate known vulns. |
| SECURITY.md | Absent | No formal vulnerability disclosure policy. |
| Dependabot/Renovate | Absent | No automated dependency update pipeline. dependabot[bot] appears in contributors (15 commits) suggesting past use, now disabled. |
| CONTRIBUTING.md | Present, with AI-content disclosure policy | Thoughtful. Requires AI-generated PR content to be labeled and proven against a real installation. |
| License | Apache-2.0 | Permissive. No concern. |
| Community health score | 50% (GitHub metric) | Missing: code of conduct, security policy. Present: contributing guide, license. |
| **Criticality** | | |
| Stars | 2,718 | Moderate for a niche geocoder. |
| Forks | 348 | Healthy fork count. |
| Public demo | photon.komoot.io | Commercial operator (komoot) dogfoods this in production. |

## Author/Org Provenance

### komoot (organization)

- GitHub org since 2012-03-20 (14 years)
- 7 public repos, 127 followers
- Commercial outdoor navigation company (komoot.com)
- The org having a real commercial product that depends on photon is a
  strong positive: compromise of photon would affect komoot's own service

### lonvia (Sarah Hoffmann) — dominant contributor

- GitHub account since 2012-02-17 (14 years), 321 followers, 48 public repos
- Lead developer of Nominatim (the OSM geocoder that photon imports from)
- This dual-maintainership means lonvia controls both the upstream data
  pipeline (Nominatim) and the downstream consumer (photon). Positive
  for coherence; negative for bus factor — a single account compromise
  would affect both systems.
- No listed company affiliation on GitHub profile
- Identity is well-established in OSM community (forgery-resistant:
  long tenure, cross-project consistency, community recognition)

### christophlingg (Christoph Lingg) — second contributor

- GitHub since 2012-07-24 (14 years), 58 followers, 15 public repos
- Low public footprint outside this project
- 350 commits = 20.7% of total — substantial contributor

### henrik242 (Henrik Brautaset Aronsen) — active recent contributor

- Appears affiliated with entur (Norwegian public transport company) based on PR branch prefixes (`entur/henrik/...`)
- Contributing performance improvements and dependency upgrades
- Another institutional user, which reinforces the "real users with skin in the game" signal

## Commit Activity Distribution

| Period | Cumulative commits | Delta | Interpretation |
|--------|-------------------|-------|----------------|
| By 2015-01-01 | 500 | 500 (first ~1.5 yr) | Strong initial burst |
| By 2017-01-01 | 688 | 188 (2 yr) | Slowed significantly |
| By 2019-01-01 | 761 | 73 (2 yr) | Near-fallow |
| By 2021-01-01 | 949 | 188 (2 yr) | Recovered |
| By 2023-01-01 | 1,143 | 194 (2 yr) | Sustained |
| By 2025-01-01 | 1,324 | 181 (2 yr) | Sustained |
| By 2026-04-15 | 1,691 | 367 (~1.3 yr) | **Accelerating** |

**Shape: Recovery then acceleration.** The 2017–2019 dip was near-fallow
(73 commits in 2 years), but the project recovered and the last 15
months show the highest activity rate in its history (367 commits,
annualized ~280/yr vs. historical ~130/yr). The 1.0.0 release in
February 2026 likely catalyzed this.

**Nature of recent commits:** Feature work (postcode search, structured
search), performance optimization (allocation reduction), dependency
upgrades, backwards-compatibility infrastructure. This is real
development, not cosmetic maintenance.

## Publication Integrity

- **No registry publication.** Photon is not on Maven Central. Users
  download JARs from GitHub Releases or build from source. This
  sidesteps registry-level supply chain attacks (no malicious
  version injection possible via Maven) but also means no registry-level
  integrity checks (no PGP signatures on artifacts, no reproducible
  builds).
- **GitHub Releases are the sole distribution vector.** Compromise of
  the GitHub repo or a maintainer's GitHub credentials would allow
  publishing a malicious JAR.
- **Database dumps are hosted by GraphHopper** at
  `download1.graphhopper.com`. This is a separate trust surface — users
  must trust both the photon code and the GraphHopper-provided data.
  No integrity verification mechanism for the dumps is documented.
- **Tag discipline:** Recent tags (0.7.0 through 1.0.1) map cleanly to
  releases. Legacy tags include non-semver names
  (`export-by-country-county-wise`). No evidence of tag force-pushing.

## Supply Chain Hygiene

### CI (strong)

Three-job CI pipeline:
1. **Build** — Gradle build on Java 21 and 25 matrix, artifact upload
   with git SHA in filename
2. **Import** — Full integration test: installs Nominatim, downloads
   real OSM data (Monaco), imports, runs replication update
3. **Backwards compatibility** — Compiles against older OpenSearch,
   verifies old JARs still start

All third-party actions pinned to full commit SHAs — this is
best-practice and uncommon even in well-maintained projects.

### Dependency management (mixed)

- CVE remediation is explicit and documented in `build.gradle` with
  named CVEs and version floors/pins
- Dependabot was used historically (15 commits from `dependabot[bot]`)
  but no current configuration exists
- No Renovate
- One non-standard Maven repository: `datanucleus.org/downloads/maven2/`
  — trust transitive to DataNucleus project for whatever artifact is
  pulled from there

### Security policy (absent)

No `SECURITY.md`. No documented vulnerability disclosure process. The
contributing guide focuses on code quality, not security.

## Transitive Dependency Health

Photon's `build.gradle` declares these runtime dependencies:

| Dependency | Version | Notes |
|------------|---------|-------|
| opensearch-java | 3.8.0 | Client library for OpenSearch |
| opensearch-runner | 3.6.0.0 (default, configurable) | Embedded server. Large dependency tree. |
| httpclient5 | 5.6 | Apache HTTP client |
| log4j-core/api/slf4j2 | 2.25.4 | Logging. log4j has a history (Log4Shell). Current version is post-remediation. |
| postgresql | 42.7.10 | JDBC driver (for Nominatim DB connection) |
| spring-jdbc | 7.0.6 | Database access layer |
| commons-dbcp2 | 2.14.0 | Connection pooling |
| jts-core | 1.20.0 | Geometry library (LocationTech) |
| javalin | 7.1.0 | HTTP server framework |
| micrometer-registry-prometheus | 1.16.4 | Metrics export |
| jackson-databind | 2.21.2 | JSON processing |
| jcommander | 3.0 | CLI argument parsing |

**Assessment:** Mainstream, well-maintained dependencies. The
opensearch-runner embedded server is the largest trust surface — it
pulls in a substantial transitive tree. The log4j dependency is
post-Log4Shell and pinned to a recent version. The explicit CVE
remediation (`commons-io` floor, `netty` pin) shows the maintainers
are tracking transitive vulnerabilities.

No `commons-io` or `netty` appear as direct dependencies — they're
forced via Gradle constraints, meaning they're transitive deps that
the maintainers are actively managing.

## Trust Model Assessment

### Vitality: Strong

Active development with accelerating commit rate. Recent 1.0.0
milestone. Real feature work, not just maintenance. Last commit is
today.

### Governance: Moderate with caveats

Strong institutional backing (komoot org, commercial product). Lead
maintainer (lonvia) has 14-year identity, deep OSM ecosystem roots,
high forgery resistance. **However:** single-person dominance (45.7%
of all commits) and dual-maintainership of both photon and its
upstream (Nominatim) creates correlated risk. Bus factor is
effectively 1 for the critical path. Second contributor
(christophlingg) provides some coverage but hasn't been as active
recently. Third-party contributors from institutional users (entur)
are a positive sign of broadening governance.

### Publication: Weak

No registry. No artifact signing. No reproducible builds. GitHub
Release JARs are the distribution mechanism — a single GitHub
credential compromise is the attack surface. Database dumps from a
third party add an unverified trust surface.

### Hygiene: Strong

SHA-pinned CI actions, multi-job CI with real integration tests,
explicit CVE remediation in build config, AI-content disclosure
policy in contributing guide. The absent SECURITY.md and disabled
Dependabot are gaps but don't negate the overall posture.

### Criticality: Moderate

2,718 stars, 348 forks. Not a foundational infrastructure component
like a logging library. The blast radius is bounded to organizations
that deploy photon as a geocoding service. Commercial use by komoot
and entur indicates real-world reliance.

### Forgery Resistance

- **Very high:** lonvia's 14-year identity, institutional org ownership
- **High:** cross-project consistency (Nominatim + photon), cross-platform
  OSM community recognition
- **Medium:** commit signing (23%, merges only)
- **Absent:** no artifact signing, no trusted publishing

## Gaps and Concerns

1. **Single-maintainer dominance across two coupled systems.** lonvia
   maintains both Nominatim and photon. Account compromise would give
   an attacker control of the data pipeline end-to-end. This is the
   most structurally significant concern.

2. **No artifact integrity verification.** GitHub Release JARs are
   unsigned. Database dumps from GraphHopper are unverified. A user
   deploying photon trusts two unsigned download surfaces.

3. **No SECURITY.md or disclosure policy.** If a vulnerability is found,
   there's no documented path to report it responsibly.

4. **Database dump trust chain.** The README directs users to download
   ~95 GB data files from `download1.graphhopper.com`. No checksums,
   no signatures, no documentation of how these dumps are produced.
   GraphHopper is a separate organization with its own trust profile.

5. **Non-standard Maven repository.** `datanucleus.org/downloads/maven2/`
   is a non-Maven-Central repo in the build config. Whatever artifact
   it serves is outside Maven Central's (limited) integrity guarantees.

6. **Dependabot disabled.** Past use (15 bot commits) but no current
   config. Dependency staleness is manually managed.

7. **lonvia not publicly listed in komoot org.** May be private
   membership or outside collaborator. The relationship between the
   lead maintainer and the owning org is not transparent on GitHub.

## Risk Assessment

For a **service deployment** (the typical photon use case):

- **Network exposure risk:** Photon runs as an HTTP server. A
  compromised build could serve poisoned geocoding results (subtle:
  redirect users to wrong locations) or open a backdoor on the host.
  Mitigated by: the service is typically behind a reverse proxy and
  serves read-only geographic data.

- **Data pipeline risk:** The Nominatim → photon pipeline means the
  geocoding index inherits whatever is in the Nominatim database.
  A compromise of Nominatim (same maintainer) would propagate through
  to photon's search results.

- **Supply chain attack surface:** The most likely vector is a
  compromised GitHub Release JAR or a poisoned database dump. Both
  are unsigned. For the JAR: building from source and auditing the
  diff against the previous release mitigates this. For the dump:
  no mitigation exists short of running your own Nominatim import
  (which is operationally expensive).

## Decision

**Analysis only — no posture recorded.**

Photon is not a signatory dependency. If it were being adopted as a
geocoding service, the recommended posture would be **Trusted-for-now**
based on:

- Strong vitality and acceleration toward maturity
- Institutional backing from a commercial company that dogfoods it
- Long-tenured, identity-strong lead maintainer
- Strong CI hygiene (SHA-pinned actions, integration tests, CVE
  remediation)

The primary caveats would be:

- Single-maintainer bus factor and coupled Nominatim dependency
- No artifact signing or database dump verification
- No security disclosure policy
- Build from source rather than using pre-built JARs if operating
  in a security-sensitive context

Why not **Vetted-frozen:** The project is actively developing and
accelerating. Freezing would be inappropriate.

Why not **Rejected:** The fundamentals (identity, vitality, hygiene,
institutional backing) are strong. The gaps are real but addressable,
and none indicate active malice or structural unsoundness.

## Action Items

*For an adopting organization:*

1. Build from source rather than downloading pre-built JARs
2. Run your own Nominatim import if data integrity is critical, rather
   than relying on GraphHopper dumps
3. Monitor the GitHub repo for maintainer changes (especially lonvia's
   access)
4. Consider opening an issue requesting SECURITY.md and a vulnerability
   disclosure policy
5. Pin to a specific release tag and audit diffs between releases
6. Verify what artifact is pulled from datanucleus.org and whether it
   can be sourced from Maven Central instead

## Signals Surfaced That Didn't Fit Current Schema

1. **Third-party data dump trust surface.** The trust model's
   publication integrity signals focus on code artifacts (tarballs,
   JARs, wheels). Photon's operational trust depends equally on a ~95 GB
   data file from a third party. This is a **data-supply-chain** signal
   that doesn't map to any existing signal type. Relevant for any
   project where the artifact is a code+data combination.

2. **Cross-project maintainer coupling.** lonvia maintaining both
   Nominatim and photon is a signal that doesn't fit the per-project
   model cleanly. The existing "dependency author concentration" signal
   (in Governance) covers multiple deps from the same author within a
   consumer's manifest, but not "the dep and its upstream share a
   maintainer." This is a **pipeline-coupling** signal.

3. **AI-content disclosure policy.** The contributing guide's
   requirement to label AI-generated content and prove it against a
   real installation is a governance signal that didn't exist when
   `signals-v01.md` was written. It's a positive hygiene signal
   specific to Era 3 code review practices.

4. **Non-standard repository in build config.** The datanucleus.org
   Maven repo is a trust surface that's analogous to npm's alternative
   registries but in the Java ecosystem. The current signal set doesn't
   have a "non-default registry" signal for Java/Gradle builds.

5. **Disabled automation (Dependabot ghost).** The contributor list
   shows past Dependabot activity (15 commits) but no current config.
   "Previously automated, now manual" is a different signal from
   "never automated." The current schema doesn't distinguish these.
