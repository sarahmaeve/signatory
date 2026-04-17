package git

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/sarahmaeve/signatory/internal/signal"
)

// tagsFormat is the git for-each-ref format for the batched tag-
// classification pass. One record per tag, four fields separated
// by 0x1F:
//
//	refname          — tag name, short-form
//	objecttype       — type of the object the ref directly points
//	                   at: "tag" for annotated tags, "commit" (or
//	                   rarely "tree" / "blob") for lightweight
//	dereferenced     — type of the target after tag-dereferencing;
//	                   populated only when objecttype=="tag",
//	                   empty for lightweight
//	signed_marker    — the literal string "signed" if the tag
//	                   object carries a signature block,
//	                   "unsigned" otherwise. For lightweight tags
//	                   this is always "unsigned" because there's
//	                   no tag object to inspect.
//
// git-check-ref-format(1) forbids ASCII control characters in ref
// names, so 0x1F cannot collide with a tag name. Records are
// newline-separated (for-each-ref's default); no field in this
// schema can contain a newline, so the parser is line-based.
//
// The %(if) / %(then) / %(else) / %(end) conditional (git >= 2.8)
// emits the fixed "signed" / "unsigned" string, which keeps the
// parser trivially pattern-matchable without pulling the signature
// body into our output.
const tagsFormat = "--format=%(refname:short)\x1f%(objecttype)\x1f%(*objecttype)\x1f%(if)%(contents:signature)%(then)signed%(else)unsigned%(end)"

// tagSampleCap bounds how many tag names are included in each
// per-class sample. The sample is a human-audit affordance ("give
// me the first few signed tag names so I can sanity-check") rather
// than an exhaustive enumeration. Exhaustive listing at large tag
// counts would bloat every signal row without adding analysis
// value.
const tagSampleCap = 10

// tagRow is one parsed record from the for-each-ref pass.
type tagRow struct {
	Name             string
	ObjectType       string // "tag", "commit", "tree", "blob"
	DereferencedType string // populated only when ObjectType == "tag"
	SignaturePresent bool
}

// tagClass is the three-way classification surfaced by the
// tag_signing_status signal (see internal/signal/types.go).
type tagClass int

const (
	tagClassLightweight       tagClass = iota // bare ref, no tag object
	tagClassAnnotatedUnsigned                 // tag object, no signature block
	tagClassSignedAnnotated                   // tag object with signature block
)

// String gives each class a stable, consumer-facing name matching
// the registry entry's description. The signal value's per-class
// counts use these same labels as map keys.
func (t tagClass) String() string {
	switch t {
	case tagClassLightweight:
		return "lightweight"
	case tagClassAnnotatedUnsigned:
		return "annotated_unsigned"
	case tagClassSignedAnnotated:
		return "signed_annotated"
	default:
		return "unknown"
	}
}

// parseTagsList parses the newline-separated, 0x1F-field-separated
// output of `git for-each-ref` in tagsFormat. Malformed records
// (fewer than four fields) are skipped silently; returns an empty
// slice for empty input rather than nil so callers can len() the
// result without a nil check.
func parseTagsList(data []byte) []tagRow {
	lines := bytes.Split(data, []byte{'\n'})
	out := make([]tagRow, 0, len(lines))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		fields := bytes.Split(line, []byte{0x1F})
		if len(fields) < 4 {
			continue
		}
		out = append(out, tagRow{
			Name:             string(fields[0]),
			ObjectType:       string(fields[1]),
			DereferencedType: string(fields[2]),
			SignaturePresent: string(fields[3]) == "signed",
		})
	}
	return out
}

// classifyTag maps one tag row into a tagClass.
//
//   - objecttype != "tag" → lightweight (the ref points directly
//     at the underlying object; there is no tag object to sign).
//   - objecttype == "tag" and signature present → signed_annotated.
//   - objecttype == "tag" and no signature → annotated_unsigned.
//
// Note: "signature present" here is structural — a PGP/SSH
// signature block exists in the tag object's body. It is NOT a
// cryptographic verification. A downstream caller that wants
// verification runs `git verify-tag` per tag; we intentionally
// don't because it would be a per-tag subprocess call, which the
// v0.1 plan explicitly avoids.
func classifyTag(t tagRow) tagClass {
	if t.ObjectType != "tag" {
		return tagClassLightweight
	}
	if t.SignaturePresent {
		return tagClassSignedAnnotated
	}
	return tagClassAnnotatedUnsigned
}

// collectTags runs the for-each-ref pass and emits the
// tag_signing_status signal.
//
// Failure / absence discipline mirrors collectCommitSigning:
//
//   - No tags in the repo → absence ("repo has no tags").
//   - Context cancellation / deadline → failure with
//     retryable=true.
//   - Any other git error → failure with retryable=false and
//     the clone path sanitized out of the reason.
func (c *Collector) collectTags(
	ctx context.Context,
	result *signal.CollectionResult,
	entityID string,
	now time.Time,
	ttl time.Duration,
) {
	out, err := runGit(ctx, c.path, "for-each-ref", tagsFormat, "refs/tags/")
	if err != nil {
		retryable := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		reason := c.sanitize(err.Error())
		result.RecordFailure(entityID, "tag_signing_status", sourceName, reason, retryable, now)
		return
	}

	rows := parseTagsList(out)
	if len(rows) == 0 {
		result.RecordAbsence(entityID, "tag_signing_status", sourceName,
			"repo has no tags", false, now)
		return
	}

	var (
		lightweight       int
		annotatedUnsigned int
		signedAnnotated   int
		sampleLight       = make([]string, 0, tagSampleCap)
		sampleUnsigned    = make([]string, 0, tagSampleCap)
		sampleSigned      = make([]string, 0, tagSampleCap)
	)
	for _, r := range rows {
		switch classifyTag(r) {
		case tagClassSignedAnnotated:
			signedAnnotated++
			if len(sampleSigned) < tagSampleCap {
				sampleSigned = append(sampleSigned, r.Name)
			}
		case tagClassAnnotatedUnsigned:
			annotatedUnsigned++
			if len(sampleUnsigned) < tagSampleCap {
				sampleUnsigned = append(sampleUnsigned, r.Name)
			}
		default:
			lightweight++
			if len(sampleLight) < tagSampleCap {
				sampleLight = append(sampleLight, r.Name)
			}
		}
	}
	total := len(rows)

	result.RecordSignal(entityID, "tag_signing_status", sourceName, now, ttl,
		map[string]any{
			"total_tags":         total,
			"signed_annotated":   signedAnnotated,
			"annotated_unsigned": annotatedUnsigned,
			"lightweight":        lightweight,
			"signed_ratio":       float64(signedAnnotated) / float64(total),
			"sample_signed":      sampleSigned,
			"sample_annotated":   sampleUnsigned,
			"sample_lightweight": sampleLight,
		})
}
