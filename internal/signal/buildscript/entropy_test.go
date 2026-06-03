package buildscript

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// b64alpha is the 64 distinct base64 chars; repeating it yields a
// uniform symbol distribution → entropy log2(64) = 6 bits/char.
const b64alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

func TestShannonEntropy(t *testing.T) {
	require.InDelta(t, 0.0, shannonEntropy([]byte("aaaaaaaa")), 0.001)
	require.InDelta(t, 2.0, shannonEntropy([]byte("abcdabcd")), 0.001) // 4 symbols
	require.InDelta(t, 6.0, shannonEntropy([]byte(b64alpha)), 0.001)
}

func TestHasHighEntropyRun(t *testing.T) {
	// Too short to be a payload (a checksum-length run).
	require.False(t, hasHighEntropyRun("sha = 'YWJjZGVm'"))
	// Long but zero-entropy (padding/repeat) — not a payload.
	require.False(t, hasHighEntropyRun(strings.Repeat("a", 300)))
	// A long, high-entropy base64 run embedded in a literal — the tell.
	blob := strings.Repeat(b64alpha, 4) // 256 chars, entropy 6
	require.True(t, hasHighEntropyRun("PAYLOAD = '"+blob+"'"))
	// The quotes/'=' boundaries break the run correctly; surrounding
	// prose does not extend it.
	require.False(t, hasHighEntropyRun("this is a normal sentence with words"))
}
