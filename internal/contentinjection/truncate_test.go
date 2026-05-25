package contentinjection

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTruncate_HelpersPreserveRuneBoundary covers a class of latent
// bug across the three detail-truncate helpers: byte-position
// slicing can land inside a multi-byte UTF-8 rune, leaving a
// trailing fragment that encoding/json silently replaces with
// U+FFFD when the Finding is serialized into a signal payload.
//
// The class matters specifically for the confusable detector,
// whose detail samples are the non-ASCII tokens the primitive
// exists to surface. Garbling them at serialization time degrades
// exactly the evidence an analyst would inspect.
//
// The test asserts the function-level invariant: every truncate
// helper must produce a string that passes utf8.ValidString.
// Inputs are calibrated to exceed each helper's byte limit by
// exactly enough that the limit lands inside a 2-byte Cyrillic
// rune.
func TestTruncate_HelpersPreserveRuneBoundary(t *testing.T) {
	t.Parallel()

	// Cyrillic capital І (U+0406) — 2 bytes in UTF-8 (0xD0 0x86).
	// Looks visually identical to Latin I; ideal stand-in for the
	// confusable detector's stated target inputs.
	const cyrillicI = "І"
	require.Equal(t, 2, len(cyrillicI),
		"test premise: cyrillic capital I must be exactly 2 UTF-8 bytes")

	t.Run("truncateWordForDetail", func(t *testing.T) {
		t.Parallel()
		// 39 ASCII + cyrillicI = 41 bytes. Limit is 40. Naive [:40]
		// emits 39 'a' + 0xD0 (first byte of cyrillicI) — invalid UTF-8.
		word := strings.Repeat("a", 39) + cyrillicI
		require.Equal(t, 41, len(word))

		result := truncateWordForDetail(word)
		assert.True(t, utf8.ValidString(result),
			"truncateWordForDetail must back up to a rune boundary; got %q", result)
	})

	t.Run("truncateForDetail", func(t *testing.T) {
		t.Parallel()
		// 79 ASCII + cyrillicI = 81 bytes. Limit 80.
		body := strings.Repeat("a", 79) + cyrillicI
		require.Equal(t, 81, len(body))

		result := truncateForDetail(body)
		assert.True(t, utf8.ValidString(result),
			"truncateForDetail must back up to a rune boundary; got %q", result)
	})

	t.Run("truncateURLForDetail", func(t *testing.T) {
		t.Parallel()
		// urlExfilSampleLen-1 ASCII + cyrillicI = (limit+1) bytes.
		url := strings.Repeat("a", urlExfilSampleLen-1) + cyrillicI
		require.Equal(t, urlExfilSampleLen+1, len(url))

		result := truncateURLForDetail(url)
		assert.True(t, utf8.ValidString(result),
			"truncateURLForDetail must back up to a rune boundary; got %q", result)
	})
}
