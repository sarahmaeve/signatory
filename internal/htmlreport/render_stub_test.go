package htmlreport

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderConclusionStub_HappyPath(t *testing.T) {
	in := ConclusionStubInput{
		Ref: DanglingRef{
			OutputID: "out-missing-9999",
			LocalID:  "F999",
			Reason:   "local id absent in output",
		},
		AnalystPagePath: "analysts/signatory-security-v1-r1.html",
		Page: PageContext{
			RootPrefix:  "../",
			GeneratedAt: "2026-05-06T13:00:00Z",
			Version:     "v0.1.0-test",
		},
	}

	var buf bytes.Buffer
	require.NoError(t, RenderConclusionStub(&buf, in))
	out := buf.String()

	t.Run("HTML shell + stylesheet via root prefix", func(t *testing.T) {
		assert.Contains(t, out, "<!DOCTYPE html>")
		assert.Contains(t, out, `<link rel="stylesheet" href="../assets/style.css">`)
	})

	t.Run("heading and missing-reference banner", func(t *testing.T) {
		assert.Contains(t, out, "Missing reference: F999")
		assert.Contains(t, out, "missing-reference-banner")
		// Reason surfaces inside the banner so the operator knows what
		// to investigate.
		assert.Contains(t, out, "local id absent in output")
	})

	t.Run("output id and local id surfaced for investigation", func(t *testing.T) {
		assert.Contains(t, out, "F999")
		assert.Contains(t, out, "out-missing-9999")
	})

	t.Run("back-links to index and analyst page", func(t *testing.T) {
		assert.Contains(t, out, `href="../index.html"`)
		assert.Contains(t, out, `href="../analysts/signatory-security-v1-r1.html"`)
	})

	t.Run("footer carries generated-at and version", func(t *testing.T) {
		assert.Contains(t, out, "2026-05-06T13:00:00Z")
		assert.Contains(t, out, "v0.1.0-test")
	})
}

func TestRenderConclusionStub_NoAnalystBackLink(t *testing.T) {
	// When AnalystPagePath is empty, only the index back-link renders.
	in := ConclusionStubInput{
		Ref: DanglingRef{
			OutputID: "out-x",
			LocalID:  "F001",
			Reason:   "output not loaded",
		},
		Page: PageContext{
			RootPrefix:  "../",
			GeneratedAt: "2026-05-06T13:00:00Z",
			Version:     "v0.1.0-test",
		},
	}

	var buf bytes.Buffer
	require.NoError(t, RenderConclusionStub(&buf, in))
	out := buf.String()

	assert.Contains(t, out, `href="../index.html"`)
	assert.NotContains(t, out, "Back to analyst",
		"no analyst back-link should render when AnalystPagePath is empty")
}

func TestRenderConclusionStub_EscapesAllFields(t *testing.T) {
	// LocalID and Reason are LLM/store data; escape them on the way
	// in even though they're not expected to contain markup.
	in := ConclusionStubInput{
		Ref: DanglingRef{
			OutputID: `<bad>id`,
			LocalID:  `F<script>`,
			Reason:   `reason "with" <markup>`,
		},
		Page: PageContext{RootPrefix: "../"},
	}

	var buf bytes.Buffer
	require.NoError(t, RenderConclusionStub(&buf, in))
	out := buf.String()

	assert.NotContains(t, out, "<script>")
	assert.NotContains(t, out, "<bad>")
	assert.Contains(t, out, "F&lt;script&gt;")
	assert.Contains(t, out, "&lt;bad&gt;id")
	assert.Contains(t, out, "&lt;markup&gt;")
}
