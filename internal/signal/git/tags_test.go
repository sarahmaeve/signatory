package git

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/sarahmaeve/signatory/internal/gitenv"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTagsList(t *testing.T) {
	t.Parallel()

	const us = "\x1f"

	cases := []struct {
		name string
		data string
		want []tagRow
	}{
		{
			name: "empty input produces empty slice not nil",
			data: "",
			want: []tagRow{},
		},
		{
			name: "single lightweight tag",
			data: "v1.0.0" + us + "commit" + us + "" + us + "unsigned\n",
			want: []tagRow{
				{Name: "v1.0.0", ObjectType: "commit", DereferencedType: "", SignaturePresent: false},
			},
		},
		{
			name: "single annotated unsigned tag",
			data: "v2.0.0" + us + "tag" + us + "commit" + us + "unsigned\n",
			want: []tagRow{
				{Name: "v2.0.0", ObjectType: "tag", DereferencedType: "commit", SignaturePresent: false},
			},
		},
		{
			name: "single signed annotated tag",
			data: "v3.0.0" + us + "tag" + us + "commit" + us + "signed\n",
			want: []tagRow{
				{Name: "v3.0.0", ObjectType: "tag", DereferencedType: "commit", SignaturePresent: true},
			},
		},
		{
			name: "three mixed tags with final newline",
			data: "v1" + us + "commit" + us + "" + us + "unsigned\n" +
				"v2" + us + "tag" + us + "commit" + us + "unsigned\n" +
				"v3" + us + "tag" + us + "commit" + us + "signed\n",
			want: []tagRow{
				{Name: "v1", ObjectType: "commit", DereferencedType: "", SignaturePresent: false},
				{Name: "v2", ObjectType: "tag", DereferencedType: "commit", SignaturePresent: false},
				{Name: "v3", ObjectType: "tag", DereferencedType: "commit", SignaturePresent: true},
			},
		},
		{
			name: "three mixed tags without trailing newline",
			data: "v1" + us + "commit" + us + "" + us + "unsigned\n" +
				"v2" + us + "tag" + us + "commit" + us + "signed",
			want: []tagRow{
				{Name: "v1", ObjectType: "commit", DereferencedType: "", SignaturePresent: false},
				{Name: "v2", ObjectType: "tag", DereferencedType: "commit", SignaturePresent: true},
			},
		},
		{
			name: "truncated record (fewer than 4 fields) is skipped silently",
			data: "bad-line-only-two-fields" + us + "commit\n" +
				"v1" + us + "commit" + us + "" + us + "unsigned\n",
			want: []tagRow{
				{Name: "v1", ObjectType: "commit", DereferencedType: "", SignaturePresent: false},
			},
		},
		{
			name: "tag pointing at a tree object (rare but valid)",
			data: "docs-tree-ref" + us + "tree" + us + "" + us + "unsigned\n",
			want: []tagRow{
				{Name: "docs-tree-ref", ObjectType: "tree", DereferencedType: "", SignaturePresent: false},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseTagsList([]byte(tc.data))
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestClassifyTag(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		row  tagRow
		want tagClass
	}{
		{"lightweight commit ref", tagRow{ObjectType: "commit"}, tagClassLightweight},
		{"lightweight tree ref", tagRow{ObjectType: "tree"}, tagClassLightweight},
		{"lightweight blob ref", tagRow{ObjectType: "blob"}, tagClassLightweight},
		{"annotated unsigned", tagRow{ObjectType: "tag", DereferencedType: "commit", SignaturePresent: false}, tagClassAnnotatedUnsigned},
		{"annotated signed", tagRow{ObjectType: "tag", DereferencedType: "commit", SignaturePresent: true}, tagClassSignedAnnotated},
		// Defensive: if signature marker is set on a non-tag objecttype,
		// the refers-to-commit-directly rule wins (signature shouldn't
		// be possible here in practice — git would not set
		// contents:signature on a non-tag ref — but the classifier is
		// defensive and treats the objecttype as the authoritative
		// structural signal).
		{"commit ref with stray signature marker", tagRow{ObjectType: "commit", SignaturePresent: true}, tagClassLightweight},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyTag(tc.row)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestTagClass_String(t *testing.T) {
	t.Parallel()
	// Lock in the strings — signal value consumers match on these.
	assert.Equal(t, "lightweight", tagClassLightweight.String())
	assert.Equal(t, "annotated_unsigned", tagClassAnnotatedUnsigned.String())
	assert.Equal(t, "signed_annotated", tagClassSignedAnnotated.String())
}

// ---- Integration tests ----

func TestCollector_NoTags_RecordsAbsence(t *testing.T) {
	t.Parallel()

	repo := initRepo(t)
	commitEmpty(t, repo, "seed")

	c := NewCollector(repo)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "no-tags"})
	require.NoError(t, err)

	entry := indexByType(result)["tag_signing_status"]
	require.NotNil(t, entry.Absence, "no tags → absence, not failure")
	assert.Contains(t, entry.Absence.Reason, "no tags")
}

func TestCollector_LightweightTag_Classified(t *testing.T) {
	t.Parallel()

	repo := initRepo(t)
	commitEmpty(t, repo, "seed")
	mustRunGit(t, repo, "tag", "v1.0.0") // lightweight: bare ref, no -a, no -m

	c := NewCollector(repo)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "lw"})
	require.NoError(t, err)

	sig := findSignal(t, result, "tag_signing_status")
	v := unmarshalValue(t, sig)
	assert.Equal(t, float64(1), v["total_tags"])
	assert.Equal(t, float64(1), v["lightweight"])
	assert.Equal(t, float64(0), v["annotated_unsigned"])
	assert.Equal(t, float64(0), v["signed_annotated"])
	assert.Equal(t, float64(0), v["signed_ratio"])
}

func TestCollector_AnnotatedUnsignedTag_Classified(t *testing.T) {
	t.Parallel()

	repo := initRepo(t)
	commitEmpty(t, repo, "seed")
	mustRunGit(t, repo, "tag", "-a", "v1.0.0", "-m", "Release 1.0.0")

	c := NewCollector(repo)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "au"})
	require.NoError(t, err)

	sig := findSignal(t, result, "tag_signing_status")
	v := unmarshalValue(t, sig)
	assert.Equal(t, float64(1), v["total_tags"])
	assert.Equal(t, float64(0), v["lightweight"])
	assert.Equal(t, float64(1), v["annotated_unsigned"])
	assert.Equal(t, float64(0), v["signed_annotated"])
	assert.Equal(t, float64(0), v["signed_ratio"])
}

func TestCollector_MixedTags_Classified(t *testing.T) {
	// Create three tags of different shapes in one repo: a
	// lightweight, an annotated unsigned, and a manually-
	// constructed signed annotated tag. Assert that all three
	// land in their respective buckets.
	t.Parallel()

	repo := initRepo(t)
	commitEmpty(t, repo, "seed")

	mustRunGit(t, repo, "tag", "v1.0.0-lightweight")
	mustRunGit(t, repo, "tag", "-a", "v1.0.0-annotated", "-m", "annotated release")
	writeFakeSignedTag(t, repo, "v1.0.0-signed")

	c := NewCollector(repo)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "mixed"})
	require.NoError(t, err)

	sig := findSignal(t, result, "tag_signing_status")
	v := unmarshalValue(t, sig)
	assert.Equal(t, float64(3), v["total_tags"])
	assert.Equal(t, float64(1), v["lightweight"])
	assert.Equal(t, float64(1), v["annotated_unsigned"])
	assert.Equal(t, float64(1), v["signed_annotated"])
	assert.InDelta(t, 1.0/3.0, v["signed_ratio"], 1e-9)

	// Sample lists should carry the names in their proper bucket.
	sampleLight := toStringSlice(t, v["sample_lightweight"])
	sampleUnsigned := toStringSlice(t, v["sample_annotated"])
	sampleSigned := toStringSlice(t, v["sample_signed"])
	assert.Equal(t, []string{"v1.0.0-lightweight"}, sampleLight)
	assert.Equal(t, []string{"v1.0.0-annotated"}, sampleUnsigned)
	assert.Equal(t, []string{"v1.0.0-signed"}, sampleSigned)
}

func TestCollector_TagSampleCap_BoundsSampleSize(t *testing.T) {
	// A repo with more tags than tagSampleCap in one class must
	// still emit only tagSampleCap names in the sample list.
	// Counts are unaffected by the cap.
	t.Parallel()

	repo := initRepo(t)
	commitEmpty(t, repo, "seed")
	extra := tagSampleCap + 3
	for i := 0; i < extra; i++ {
		mustRunGit(t, repo, "tag", fmt.Sprintf("lw-%02d", i))
	}

	c := NewCollector(repo)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "capped"})
	require.NoError(t, err)

	sig := findSignal(t, result, "tag_signing_status")
	v := unmarshalValue(t, sig)
	assert.Equal(t, float64(extra), v["lightweight"], "count reflects all tags")
	assert.Len(t, toStringSlice(t, v["sample_lightweight"]), tagSampleCap,
		"sample is bounded by tagSampleCap")
}

// writeFakeSignedTag manually constructs a tag object whose body
// includes a PGP signature block, then points refs/tags/<name> at
// that object. The signature is NOT cryptographically valid — it's
// a literal string that makes %(contents:signature) non-empty,
// which is all our classifier needs to mark the tag as signed.
//
// This avoids requiring a real GPG key setup in the test
// environment. If signatory ever needs to distinguish "signature
// block present" from "signature cryptographically valid" —
// currently it doesn't — that would require real verification and
// a different test strategy.
func writeFakeSignedTag(t *testing.T, repo, name string) {
	t.Helper()

	//nolint:gosec // G204: test helper
	headCmd := exec.Command("git", "-C", repo, "rev-parse", "HEAD")
	headCmd.Env = gitenv.SafeEnv()
	headOut, err := headCmd.Output()
	require.NoError(t, err)
	headSha := strings.TrimSpace(string(headOut))

	tagBody := fmt.Sprintf(`object %s
type commit
tag %s
tagger Test User <test@example.invalid> 0 +0000

Release %s
-----BEGIN PGP SIGNATURE-----
Version: signatory-test-fixture

ZmFrZXNpZ25hdHVyZWJhc2U2NGRhdGE=
-----END PGP SIGNATURE-----
`, headSha, name, name)

	hashCmd := exec.Command("git", "-C", repo, "hash-object", "-w", "-t", "tag", "--stdin") //nolint:gosec // G204: test helper
	hashCmd.Env = gitenv.SafeEnv()
	hashCmd.Stdin = strings.NewReader(tagBody)
	hashOut, err := hashCmd.Output()
	require.NoError(t, err, "hash-object failed")
	tagSha := strings.TrimSpace(string(hashOut))

	mustRunGit(t, repo, "update-ref", "refs/tags/"+name, tagSha)
}

// toStringSlice converts an []any (what json.Unmarshal produces
// for JSON arrays of strings) into a typed []string for assertion.
func toStringSlice(t *testing.T, v any) []string {
	t.Helper()
	raw, ok := v.([]any)
	require.True(t, ok, "expected []any, got %T", v)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		require.True(t, ok, "expected string in slice, got %T", item)
		out = append(out, s)
	}
	return out
}
