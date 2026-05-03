# Example: What signatory analyze Should Produce

This document captures the format that emerged from dogfooding our own
dependency decisions. This is what both a human SWE and an LLM need to
evaluate a project.

## Output Structure

### 1. Identity and Metadata

Quick summary: what is this, who maintains it, what's the basic state.

```
Package:        github.com/alecthomas/kong
Type:           Go module
Purpose:        CLI argument parser
License:        MIT
Created:        April 2018
Last commit:    April 1, 2026
Latest release: v1.15.0
Stars:          3,023
Dependents:     ~2,000
Runtime deps:   0
```

### 2. Signal Table

Every signal grouped by category, with raw values and forgery resistance.

| Signal | Value | Assessment |
|--------|-------|------------|
| Last commit | 8 days ago | Active |
| Release cadence | Regular (v1.12–v1.15) | Disciplined |
| Contributors | 10+ meaningful | Healthy |
| Maintainer tenure | 17+ years on GitHub | Very high forgery resistance |
| Commit signing | All recent verified | Consistent |
| Runtime deps | 0 | Exemplary |
| CI present | Yes (Renovate bot active) | Good hygiene |
| ... | ... | ... |

### 3. Author/Maintainer Provenance

Who is this person, and how confident are we in their identity?

```
Primary maintainer: Alec Thomas (alecthomas)
  Account age:      17 years (Dec 2008)
  Public repos:     175
  Followers:        1,419
  Known projects:   participle, chroma (used by Hugo), kingpin
  Org affiliation:  None (individual)
  Forgery resistance: Very high (long tenure, cross-ecosystem presence)
```

### 4. Trust Model Assessment

One-line-per-group summary with a clear positive/negative/neutral signal.

```
Vitality:     ✓ Active (commits this week, PRs being reviewed)
Governance:   ~ Single maintainer, but active community contributions
Publication:  ✓ Tags match verified commits, regular releases
Hygiene:      ✓ CI, Renovate bot, signed commits
Criticality:  ~ Moderate (3K stars, 2K dependents)
```

### 5. Gaps and Concerns

What's missing or worrying, stated plainly.

```
Gaps:
  - No institutional affiliation (individual project)
  - Single primary maintainer — bus factor risk
```

### 6. Posture Recommendation

A brief recommendation with rationale, for the human to approve or reject.

```
Recommended posture: trusted-for-now

Rationale: Strong vitality, strong author provenance, zero runtime
dependencies, verified commits. The zero-dependency property is
particularly important for a supply chain security tool. Main gap
is individual maintainership with no institutional backing.
```

## Design Notes

This format emerged from manually evaluating `mousetrap` and `kong`
during signatory's own dependency selection. Key properties:

1. **Scannable.** A human or LLM can read the top-level summary and
   stop, or drill into the signal table for detail.

2. **Honest about gaps.** Gaps are stated alongside positives, not
   buried or omitted.

3. **Recommendation is separate from data.** The signals are presented
   first. The recommendation comes last and includes rationale. The
   human decides.

4. **Forgery resistance is visible.** Not all signals are equal. The
   format surfaces which signals are hard to fake (tenure, signatures)
   vs. easy to fake (code style, descriptions).

5. **Actionable.** Ends with a clear posture recommendation that can
   be accepted, rejected, or modified.

This should be the target format for `signatory analyze` CLI output
(human-readable) and the structure of the MCP `signatory_analyze`
response (JSON equivalent).
