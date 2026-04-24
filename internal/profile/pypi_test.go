package profile

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestNormalizePyPIName covers PEP 503's normalization: lowercase +
// runs of `.`/`-`/`_` collapsed to a single `-`. Edge cases at the
// boundary of PEP 508 grammar (leading/trailing separators, empty
// input) pass through unchanged — grammar enforcement is a separate
// concern (Layer 4).
func TestNormalizePyPIName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		// Identity: already in canonical form.
		{"already canonical", "requests", "requests"},
		{"already canonical with dash", "python-dotenv", "python-dotenv"},

		// Pure case folding.
		{"initial cap", "Requests", "requests"},
		{"all caps", "REQUESTS", "requests"},
		{"mixed case", "PyYAML", "pyyaml"},

		// Separator conversion.
		{"underscore to dash", "python_dotenv", "python-dotenv"},
		{"dot to dash", "python.dotenv", "python-dotenv"},
		{"underscores and dots", "python_some.module_name", "python-some-module-name"},

		// Run collapse.
		{"double dash", "python--dotenv", "python-dotenv"},
		{"mixed run", "python-_.-dotenv", "python-dotenv"},
		{"long run of dots", "python...dotenv", "python-dotenv"},
		{"long run of underscores", "python___dotenv", "python-dotenv"},

		// Case + separator combined.
		{"case and underscore", "Python_Dotenv", "python-dotenv"},
		{"case and dot", "Python.Dotenv", "python-dotenv"},
		{"case and mixed run", "Python_-.Dotenv", "python-dotenv"},

		// Edge cases at PEP 508 grammar boundary. Per §NormalizePyPIName
		// doc, grammar violations pass through — this function just
		// normalizes.
		{"empty", "", ""},
		{"leading separator preserved", "_foo", "-foo"},
		{"trailing separator preserved", "foo_", "foo-"},
		{"both ends separator preserved", "_foo_", "-foo-"},
		{"only separators", "_-.", "-"},

		// Digits and other grammar-legal characters pass through.
		{"digits", "foo123", "foo123"},
		{"digits and separators", "foo_1_2_3", "foo-1-2-3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := NormalizePyPIName(tc.in)
			assert.Equal(t, tc.want, got,
				"NormalizePyPIName(%q) = %q; want %q", tc.in, got, tc.want)
		})
	}
}

// TestNormalizePyPIName_Idempotent pins the idempotence property —
// normalizing a normalized name is a no-op. Callers rely on this:
// when the input is already canonical, the output must be the same
// object-value so storage writes stay stable across re-resolutions.
func TestNormalizePyPIName_Idempotent(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"requests",
		"Python_Dotenv",
		"PyYAML",
		"python---___...dotenv",
		"",
		"_foo_",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			once := NormalizePyPIName(in)
			twice := NormalizePyPIName(once)
			assert.Equal(t, once, twice,
				"NormalizePyPIName must be idempotent; %q → %q → %q", in, once, twice)
		})
	}
}
