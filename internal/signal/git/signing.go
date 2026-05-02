package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// Git log format for a single batched pass over commit signing
// metadata. The separators are ASCII control characters that git
// treats as literal bytes inside format strings:
//
//   - 0x1F (unit separator) between fields of a record;
//   - 0x1E (record separator) between records.
//
// Neither byte can appear in a commit hash, author name, email,
// verification flag, signer name, or key ID under any legitimate
// circumstance, so the parser never needs to worry about quoting.
const commitSigningFormat = "--format=%H%x1f%aN%x1f%aE%x1f%G?%x1f%GS%x1f%GK%x1e"

// webFlowKeyIDs is the set of GPG key short IDs GitHub uses to sign
// "web-flow" commits — merges via UI, suggestion accepts, squash-
// and-rebase through the web interface. GitHub has rotated this
// key; multiple IDs can legitimately appear across the history of
// a long-running project.
//
// Maintained manually. When GitHub publishes a new web-flow key,
// extend this map. Matching is case-insensitive (keys normalized
// to upper-case before lookup).
var webFlowKeyIDs = map[string]bool{
	"4AEE18F83AFDEB23": true, // older web-flow key, retired ~2022
	"B5690EEEBB952194": true, // current web-flow key as of 2026-04
}

// commitSigningRow is one parsed record from the batched git-log
// pass. Fields map 1:1 to the %-placeholders in commitSigningFormat.
type commitSigningRow struct {
	Hash            string
	AuthorName      string
	AuthorEmail     string
	SignatureStatus string // %G? single char: G, B, U, X, Y, R, E, N
	SignerName      string // %GS
	KeyID           string // %GK
}

// signingClass categorizes a commit's signature posture. The three
// cases are mutually exclusive and exhaustive: every commit
// classifies as exactly one.
type signingClass int

const (
	classUnsigned     signingClass = iota // no signature, bad, revoked, or uncheckable
	classPerDeveloper                     // valid signature from a non-web-flow key
	classWebFlow                          // valid signature from a known GitHub web-flow key
)

// parseCommitSigningLog parses the 0x1F/0x1E-delimited output of a
// git log run using commitSigningFormat. Malformed records (fewer
// than six fields) are skipped silently; this is defensive against
// unexpected git output and does not panic.
//
// Returns an empty slice for empty input rather than nil, so
// callers can len() the result without a nil check.
func parseCommitSigningLog(data []byte) []commitSigningRow {
	recs := bytes.Split(data, []byte{0x1E})
	out := make([]commitSigningRow, 0, len(recs))
	for _, rec := range recs {
		// Git emits a newline after each record terminator; trim it
		// so the first field of the next record starts clean.
		rec = bytes.TrimLeft(rec, "\n")
		if len(rec) == 0 {
			continue
		}
		fields := bytes.Split(rec, []byte{0x1F})
		if len(fields) < 6 {
			continue
		}
		out = append(out, commitSigningRow{
			Hash:            string(fields[0]),
			AuthorName:      string(fields[1]),
			AuthorEmail:     string(fields[2]),
			SignatureStatus: string(fields[3]),
			SignerName:      string(fields[4]),
			KeyID:           string(fields[5]),
		})
	}
	return out
}

// classifySigning maps one commit row into a signingClass.
//
// Git's %G? placeholder produces one of:
//
//	G = good signature
//	B = bad signature (tampered)
//	U = good signature with unknown validity
//	X = good signature, signature itself expired
//	Y = good signature, signing key expired
//	R = good signature, signing key revoked
//	E = signature cannot be checked (e.g., missing key)
//	N = no signature
//
// For trust purposes we count only cryptographically valid
// signatures the user has not rejected: G, U, X, Y. A revoked key
// (R) explicitly withdraws trust from that key's signatures — we
// honor the revocation. B (tampered) and E (uncheckable) are
// conservatively treated as unsigned; crediting them would let an
// attacker manufacture signing-ratio signals by stripping public
// keys from the host's keyring.
func classifySigning(r commitSigningRow) signingClass {
	switch r.SignatureStatus {
	case "G", "U", "X", "Y":
		if webFlowKeyIDs[strings.ToUpper(strings.TrimSpace(r.KeyID))] {
			return classWebFlow
		}
		return classPerDeveloper
	default:
		// N, B, E, R, empty, or any future flag value.
		return classUnsigned
	}
}

// collectCommitSigning runs the batched git-log pass and emits the
// per_developer_commit_signing_ratio and web_flow_signing_ratio
// signals for the caller-provided entity.
//
// A single git invocation produces all the data needed for both
// signals; this avoids N subprocess invocations and keeps the
// collector's git-CLI footprint to exactly one call per analysis.
//
// On an upstream git failure, both signals are recorded as
// failures (not absences), with the git stderr sanitized to remove
// the absolute clone path before storage.
//
// On an empty window (no commits in the last `window` duration),
// both signals are recorded as absences with a reason naming the
// window — the distinction matters for downstream consumers that
// treat absence differently from failure.
func (c *Collector) collectCommitSigning(
	ctx context.Context,
	result *signal.CollectionResult,
	entityID string,
	now time.Time,
	ttl time.Duration,
) {
	// Distinguish "repo has zero commits" from "git command failed"
	// up front. A repo with no HEAD ref is a definitive absence
	// (not a failure); git log against such a repo returns exit 128
	// with "ambiguous argument 'HEAD'", which our callers would
	// otherwise have to string-match against.
	//
	// If the pre-check fails because of context cancellation (or
	// any transient), treat it as a failure on both signals rather
	// than masquerading as a definitive absence. Transients are
	// retryable; definitive absences are not.
	if _, err := runGit(ctx, c.path, "rev-parse", "--verify", "HEAD"); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			reason := c.sanitize(err.Error())
			result.RecordFailure(entityID, "per_developer_commit_signing_ratio", sourceName, reason, true, now)
			result.RecordFailure(entityID, "web_flow_signing_ratio", sourceName, reason, true, now)
			return
		}
		reason := "repo has no commits on HEAD"
		result.RecordAbsence(entityID, "per_developer_commit_signing_ratio", sourceName, reason, false, now)
		result.RecordAbsence(entityID, "web_flow_signing_ratio", sourceName, reason, false, now)
		return
	}

	since := fmt.Sprintf("--since=%s", now.Add(-c.window).Format(time.RFC3339))
	maxCount := fmt.Sprintf("-n%d", c.commitCap)

	out, err := runGit(ctx, c.path, "log", since, commitSigningFormat, maxCount, "HEAD")
	if err != nil {
		reason := c.sanitize(err.Error())
		result.RecordFailure(entityID, "per_developer_commit_signing_ratio", sourceName, reason, false, now)
		result.RecordFailure(entityID, "web_flow_signing_ratio", sourceName, reason, false, now)
		return
	}

	rows := parseCommitSigningLog(out)
	if len(rows) == 0 {
		reason := fmt.Sprintf("no commits within the %s window", c.window)
		result.RecordAbsence(entityID, "per_developer_commit_signing_ratio", sourceName, reason, false, now)
		result.RecordAbsence(entityID, "web_flow_signing_ratio", sourceName, reason, false, now)
		return
	}

	var perDev, webFlow, unsigned int
	for _, r := range rows {
		switch classifySigning(r) {
		case classPerDeveloper:
			perDev++
		case classWebFlow:
			webFlow++
		default:
			unsigned++
		}
	}
	total := len(rows)

	result.RecordSignal(entityID, "per_developer_commit_signing_ratio", sourceName, now, ttl,
		map[string]any{
			"per_developer_signed": perDev,
			"web_flow_signed":      webFlow,
			"unsigned":             unsigned,
			"total_commits":        total,
			"ratio":                float64(perDev) / float64(total),
			"window":               c.window.String(),
		})
	result.RecordSignal(entityID, "web_flow_signing_ratio", sourceName, now, ttl,
		map[string]any{
			"web_flow_signed":      webFlow,
			"per_developer_signed": perDev,
			"unsigned":             unsigned,
			"total_commits":        total,
			"ratio":                float64(webFlow) / float64(total),
			"window":               c.window.String(),
		})

	// ----- commit_signing_keys + signer-entity minting (Path F) -----
	//
	// Walk the same already-parsed rows to extract the distinct
	// per-developer GPG key IDs (lowercased, web-flow excluded,
	// deduped, lexicographically sorted for deterministic output).
	// Mint identity:gpg/<keyid> for each, then emit
	// commit_signing_keys carrying the list so the cascade resolver
	// (internal/store/effective_burn.go's "commit_signing_keys"
	// case) can walk them at read time.
	//
	// When zero per-developer keys are present (all-unsigned or
	// all-web-flow window), record absence rather than emitting an
	// empty signal — keeps consumers from confusing "no
	// cryptographic signers" with "we haven't checked yet."
	keyIDs := extractPerDeveloperKeyIDs(rows)
	if len(keyIDs) == 0 {
		reason := "no per-developer GPG-signed commits in window (web-flow excluded)"
		result.RecordAbsence(entityID, "commit_signing_keys", sourceName,
			reason, false, now)
		return
	}
	c.ensureSignerEntities(ctx, keyIDs)
	result.RecordSignal(entityID, "commit_signing_keys", sourceName, now, ttl,
		map[string]any{
			"count":   len(keyIDs),
			"key_ids": keyIDs,
			"window":  c.window.String(),
		})
}

// extractPerDeveloperKeyIDs walks classified signing rows and
// returns the lowercased, lexicographically-sorted, deduplicated
// set of GPG key IDs that signed commits in the per-developer
// classification (G/U/X/Y status, key not in webFlowKeyIDs).
// Empty input returns nil so callers can branch on len().
//
// Lexicographic sort is the deterministic-output choice: the
// commit_signing_keys signal's key_ids field is consumed by the
// cascade resolver (which iterates in slice order, "first burned
// wins") and by tests (which assert on exact slice contents). The
// alternative — sort-by-frequency-desc — would surface the
// dominant signer first, but adds a counting pass for marginal
// value over alphabetical determinism.
//
// Web-flow keys are dropped at the classifySigning gate (see
// classifySigning); this helper trusts that filter rather than
// re-checking, keeping the responsibility split clean.
func extractPerDeveloperKeyIDs(rows []commitSigningRow) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, r := range rows {
		if classifySigning(r) != classPerDeveloper {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(r.KeyID))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}

// ensureSignerEntities mints identity:gpg/<keyid> rows for each
// extracted key. Failures are logged-and-continued: a transient
// store error on one key doesn't abort the whole sweep, because
// each entity row is independent and the next analyze run re-
// attempts. Skipped silently when no EntityStore was wired
// (pre-Path-F tests construct collectors without one and continue
// to work). Mirrors the github / npm / pypi mint-helper policy.
func (c *Collector) ensureSignerEntities(ctx context.Context, keyIDs []string) {
	if c.entityStore == nil {
		return
	}
	for _, keyID := range keyIDs {
		uri := profile.CanonicalIdentityURI("gpg", keyID)
		if _, _, err := c.entityStore.EnsureEntityByCanonicalURI(ctx, uri, keyID); err != nil {
			// Don't propagate — the signal emission is independent
			// of entity-row minting, and the next analyze run re-
			// attempts. Surface to stderr so systemic store
			// failures are visible. Matches the policy of the
			// other ecosystem collectors.
			fmt.Fprintf(os.Stderr, "warning: failed to ensure gpg signer entity %s: %v\n", uri, err)
		}
	}
}
