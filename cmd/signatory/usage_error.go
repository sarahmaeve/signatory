package main

import (
	"errors"
	"fmt"
)

// ErrUsage is the sentinel wrapped by NewUsageError. exitCodeFor
// detects it via errors.Is to return EX_USAGE (64) instead of the
// generic runtime code (1). Scripts branching on signatory's exit
// codes can distinguish "I passed bad flags" from "signatory had
// a runtime problem" without parsing stderr.
var ErrUsage = errors.New("usage error")

// NewUsageError wraps err so exitCodeFor sees it as a user-input
// failure. Use for flag conflicts, malformed inputs, and similar
// "the invocation itself is wrong" cases. Runtime failures (DB
// errors, network errors) should NOT be wrapped — they exit 1.
//
// Wrapping is no-op-safe on nil: NewUsageError(nil) returns nil.
func NewUsageError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrUsage, err)
}
