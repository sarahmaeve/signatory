package resources_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// mustMarshal JSON-encodes v and returns the bytes. Fails the test on error.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// unmarshal JSON-decodes src into dst. Returns any error.
func unmarshal(src []byte, dst any) error {
	return json.Unmarshal(src, dst)
}
