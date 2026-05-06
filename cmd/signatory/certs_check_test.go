package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/certs"
)

// TestCertsCheck_JSON_OK verifies that --json returns structured
// output when the certs environment is healthy.
func TestCertsCheck_JSON_OK(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	cmd := &CertsCheckCmd{
		JSON: true,
		Checker: func() certs.CheckResult {
			return certs.CheckResult{
				OK:      true,
				Message: "NODE_EXTRA_CA_CERTS is set",
			}
		},
		Stdout: &stdout,
	}

	require.NoError(t, cmd.Run(nil))

	var result CertsCheckResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result),
		"stdout must be valid JSON; got: %s", stdout.String())

	assert.Equal(t, "ok", result.Status)
	assert.Equal(t, "NODE_EXTRA_CA_CERTS is set", result.Message)
	assert.Empty(t, result.Fix)
}

// TestCertsCheck_JSON_Fail verifies that --json returns the failure
// message and remediation hint as structured fields, and still
// returns a non-nil error for exit code handling.
func TestCertsCheck_JSON_Fail(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	cmd := &CertsCheckCmd{
		JSON: true,
		Checker: func() certs.CheckResult {
			return certs.CheckResult{
				OK:      false,
				Message: "NODE_EXTRA_CA_CERTS is not set",
				Fix:     "run `signatory certs init --write-profile`",
			}
		},
		Stdout: &stdout,
	}

	err := cmd.Run(nil)
	require.Error(t, err, "non-zero exit on failure even in JSON mode")

	var result CertsCheckResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result),
		"JSON output must be written before error return; got: %s", stdout.String())

	assert.Equal(t, "error", result.Status)
	assert.Equal(t, "NODE_EXTRA_CA_CERTS is not set", result.Message)
	assert.Equal(t, "run `signatory certs init --write-profile`", result.Fix)
}
