package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// formatBurnLine is the shared renderer behind signatory analyze,
// summary, and show-analyses. These tests pin the exact output
// shape — both the direct form and the cascade form — so a
// future change that reformats the BURNED line affects every
// command at once and is detected here, not in three separate
// integration tests scattered across surfaces.

func TestFormatBurnLine_Direct(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 5, 2, 2, 0, 14, 0, time.UTC)
	got := formatBurnLine(burnDisplayInput{
		Reason:   "campaign-shaped account, 17 throwaway repos",
		BurnedBy: "team:sarah+unassisted",
		BurnedAt: at,
	})

	want := "*** BURNED: campaign-shaped account, 17 throwaway repos " +
		"(by team:sarah+unassisted, 2026-05-02T02:00:14Z) ***"
	assert.Equal(t, want, got,
		"direct burn must produce the no-via form, exact format pinned across all three commands")
}

func TestFormatBurnLine_Cascade(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 5, 2, 2, 0, 14, 0, time.UTC)
	got := formatBurnLine(burnDisplayInput{
		Reason:      "adversarial exploit campaign documented and verified",
		BurnedBy:    "team:sarah+unassisted",
		BurnedAt:    at,
		ViaOwnerURI: "identity:github/bufferzonecorp",
		ViaRole:     "publisher",
	})

	want := "*** BURNED: adversarial exploit campaign documented and verified " +
		"(via publisher identity:github/bufferzonecorp, " +
		"by team:sarah+unassisted, 2026-05-02T02:00:14Z) ***"
	assert.Equal(t, want, got,
		"cascade burn must produce the via-form with role + owner-URI inline; this is the load-bearing user-visible surface for Path B")
}

func TestFormatBurnLine_PartialCascadeContext_FallsBackToDirect(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 5, 2, 2, 0, 14, 0, time.UTC)

	// Defensive: ViaOwnerURI set without ViaRole (or vice versa)
	// is a programming error from a caller that didn't populate
	// the cascade context as a unit. Format falls back to the
	// direct form rather than producing "via  identity:..." or
	// "via publisher " — both of which would render as garbled
	// output in the user's terminal.
	cases := []struct {
		name string
		in   burnDisplayInput
	}{
		{
			name: "URI without role",
			in: burnDisplayInput{
				Reason:      "test",
				BurnedBy:    "team:test",
				BurnedAt:    at,
				ViaOwnerURI: "identity:github/foo",
				ViaRole:     "",
			},
		},
		{
			name: "role without URI",
			in: burnDisplayInput{
				Reason:      "test",
				BurnedBy:    "team:test",
				BurnedAt:    at,
				ViaOwnerURI: "",
				ViaRole:     "publisher",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatBurnLine(tc.in)
			assert.Contains(t, got, "BURNED:")
			assert.NotContains(t, got, "via",
				"partial cascade context must NOT produce a malformed via clause; got: %s", got)
			assert.Contains(t, got, "(by team:test,",
				"direct-form fallback structure must be intact")
		})
	}
}
