# Signal Type Registry Design

## Status

Draft — open questions pending. Tracked as issue (TBD).

## Motivation

Signatory collects signals with two existing pieces of metadata per
observation: `Source` (where the signal came from) and
`ForgeryResistance` (categorical: very-high, high, medium-declining,
low-declining). This is enough for storage but not enough for:

- Letting users (and LLMs) understand *why* a signal has its rating
- Applying configurable weighting in scoring systems
- Surfacing the limitations of a signal in display
- Distinguishing "this signal is missing for known good reason" from
  "we couldn't get it"

The signal type registry adds **type-level metadata** that complements
the per-observation data we already store.

## Proposed Structure

```go
type SignalTypeInfo struct {
    Type              string            // canonical signal name
    Group             SignalGroup       // existing question grouping
    Realm             SignalRealm       // see open question 1
    ForgeryResistance ForgeryResistance // existing categorical
    Weight            int               // 1-10 tiered scale
    Polarity          string            // positive, negative, contextual, amplifier
    Description       string
    Caveats           []string          // known limitations
}
```

The registry is a code constant for v0.1, with a config file override
path planned for v0.2 (and a slider UI further out).

## Tiered Weight Scale

A 10-point scale with documented anchors. CVSS-inspired but simpler.

| Tier | Meaning | Example signal |
|------|---------|---------------|
| 10 | Cryptographically verifiable, hard to fake | tag SHA stability, signed commits, trusted publishing |
| 8-9 | Strong evidence, requires significant effort to forge | account tenure, org affiliation, total commit count |
| 6-7 | Useful evidence, some manipulation possible | release cadence, license presence, CI/CD config |
| 4-5 | Soft signal, moderate noise | open issues, contributor count |
| 2-3 | Manipulable, useful as broad indicator only | stars, forks, followers |
| 1 | Almost noise, useful only in combination | watcher count, single-snapshot metrics |

A future UI could expose these as sliders, allowing users to customize
the weights for their threat model.

## Polarity Values

- `positive` — having this signal increases trust (signed commits,
  long maintainer tenure)
- `negative` — having this signal decreases trust (recent maintainer
  change, license absent, fallow code)
- `contextual` — neither positive nor negative on its own (stars,
  forks, followers — they're broad indicators, not trust evidence)
- `amplifier` — multiplies the weight of other signals (criticality,
  blast radius — these don't add trust, they amplify the importance
  of whatever else is true)

## Caveats Field

A list of known issues with the signal. Self-documenting reasons for
the weight and forgery resistance ratings. Surfaced in `--verbose`
output and MCP responses.

Example:
```go
"stars": {
    Caveats: []string{
        "silently mutable — GitHub does not expose historical star counts",
        "vulnerable to mass star/unstar manipulation campaigns",
        "no way to distinguish organic growth from manipulation in a single observation",
    },
},
```

## Example Registry Entries

```go
var signalTypeRegistry = map[string]SignalTypeInfo{
    "stars": {
        Type:              "stars",
        Group:             SignalGroupCriticality,
        ForgeryResistance: ForgeryLowDeclining,
        Weight:            3,
        Polarity:          "contextual",
        Description:       "GitHub star count",
        Caveats: []string{
            "silently mutable — no historical data via API",
            "vulnerable to mass star/unstar manipulation",
        },
    },
    "tag_sha_stability": {
        Type:              "tag_sha_stability",
        Group:             SignalGroupPublication,
        ForgeryResistance: ForgeryVeryHigh,
        Weight:            10,
        Polarity:          "positive",
        Description:       "SHA stability of git tags over time",
        Caveats: []string{
            "requires multiple observations to detect change",
        },
    },
    "commit_signing": {
        Type:              "commit_signing",
        Group:             SignalGroupGovernance,
        ForgeryResistance: ForgeryHigh,
        Weight:            8,
        Polarity:          "positive",
        Description:       "Ratio of recent commits with verified signatures",
        Caveats: []string{
            "verification status depends on key validity at observation time",
            "key revocation invalidates previously-verified commits",
        },
    },
}
```

## Open Questions

### 1. What does `Realm` mean?

Three possible interpretations, each gives a different axis:

**Interpretation A: Source platform.**
The platform the signal came from. Already partially captured per-
observation as `Source` (github, npm-registry). As type metadata it
would constrain "this signal type only applies to GitHub-hosted
projects."

**Interpretation B: Trust domain.**
The kind of trust concern the signal addresses, distinct from the
question-based `Group`:
- `provenance` — who made it
- `identity` — is the maintainer who they claim to be
- `integrity` — has the code been tampered with
- `vitality` — is the project alive
- `quality` — is the code well-maintained
- `operational` — does it ship cleanly

This would enable filtering like "show me only the integrity signals."

**Interpretation C: Enterprise realm** (from v0.2 design).
Whether the signal applies to internal entities, external entities,
or both. Aligns with the realm concept in the entity model v2 design
for internal identity registries.

Pending decision.

### 2. How should weights compose for derived metrics?

If a user wants a single composite score, how do polarity and weight
combine? Options:
- Simple weighted sum (positive contributes positively, negative
  negatively, contextual zero, amplifier multiplies)
- Bayesian update (weights are confidence in evidence)
- Configurable formula (let users define their own)

This is downstream of v0.1 — signatory exposes raw signals and
metadata, scoring is the user's choice. But the metadata structure
should be sufficient to support reasonable composition.

### 3. Should weights be adjustable per entity type?

A "stars" signal might be weight 3 for an open source library but
weight 1 for an internal tool. Should the registry support per-
entity-type overrides, or is one global weight enough?

### 4. How does the registry handle absence signals?

When we record `absence:contributors`, does the registry have an
entry for the absence type, or does it inherit from the parent
signal type? Implementation choice.

## Versioning Concerns

The registry will evolve over time as we add new signal types,
adjust weights based on operational experience, and respond to
new attack patterns. This raises versioning questions:

- If a user has a config override for weights, how do schema changes
  in the registry interact with their overrides?
- When the registry adds new signal types, are existing scores
  affected?
- Should the registry itself be versioned alongside the schema
  migration system?

These need a separate design pass before implementation.

## Implementation Notes

For v0.1:
- Code constant in `internal/signal/types.go`
- Function `GetSignalTypeInfo(signalType string) (*SignalTypeInfo, bool)`
- Exposed via MCP as a resource (`signatory://signal-types`)
- Surfaced in CLI `--verbose` output and `signatory analyze` JSON output

For v0.2:
- Config file override (`~/.signatory/signal-weights.yaml`)
- `signatory weights` command to view and edit
- Per-entity-type overrides if open question 3 is answered yes

For later:
- Slider UI in dashboard
- Bayesian or learned weights based on feedback loops
