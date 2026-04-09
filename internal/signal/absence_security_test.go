package signal

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSecurity_ZeroValueToSignalDoesNotPanic verifies that calling
// ToSignal on a zero-value SignalOrAbsence does not panic (issue #30).
func TestSecurity_ZeroValueToSignalDoesNotPanic(t *testing.T) {
	var soa SignalOrAbsence // Both Signal and Absence are nil.

	// This should not panic.
	assert.NotPanics(t, func() {
		sig := soa.ToSignal()
		// Should return a zero-value Signal, not crash.
		assert.Empty(t, sig.ID)
	})
}
