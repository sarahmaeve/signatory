package main

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// failingWriter returns an error on writes after threshold has been
// reached. Mirrors the real-world shape of a broken pipe: the first
// N writes succeed, then every subsequent write errors. Callers set
// limit=0 to fail on the first write.
type failingWriter struct {
	inner     bytes.Buffer
	limit     int // succeed for this many writes before erroring
	calls     int
	failWith  error // error returned once limit is reached
	writeLens []int // per-call write lengths, for assertion
}

func (f *failingWriter) Write(p []byte) (int, error) {
	f.calls++
	f.writeLens = append(f.writeLens, len(p))
	if f.calls > f.limit {
		return 0, f.failWith
	}
	return f.inner.Write(p)
}

// errBrokenPipe is a stand-in for the real-world syscall.EPIPE we'd
// see from a pipe consumer that closed its read end. Using a named
// error keeps errors.Is assertions readable.
var errBrokenPipe = errors.New("write: broken pipe (test)")

// ===== stickyWriter primitive =====

// TestStickyWriter_HappyPath confirms the wrapper is transparent
// when nothing fails: all writes reach the underlying writer, Err
// returns nil, and content arrives in order.
func TestStickyWriter_HappyPath(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	sw := &stickyWriter{w: &buf}

	sw.Writef("first %s\n", "line")
	sw.Writeln("second line")
	sw.Writef("third %d\n", 3)

	require.NoError(t, sw.Err())
	assert.Equal(t, "first line\nsecond line\nthird 3\n", buf.String())
}

// TestStickyWriter_ShortCircuitsAfterFirstError is the core
// contract: once the underlying writer errors, subsequent Writef/
// Writeln calls MUST be no-ops and the first error is preserved.
// This is the behavior that matters for broken-pipe on `analyze |
// head` — we don't waste CPU formatting lines for a closed stream.
func TestStickyWriter_ShortCircuitsAfterFirstError(t *testing.T) {
	t.Parallel()

	fw := &failingWriter{limit: 1, failWith: errBrokenPipe}
	sw := &stickyWriter{w: fw}

	sw.Writef("succeeds\n")
	sw.Writef("fails here\n") // triggers errBrokenPipe on Write
	sw.Writef("must be a no-op\n")
	sw.Writeln("also no-op")
	sw.Writef("still no-op %d\n", 99)

	// Underlying writer saw exactly two calls: the first Writef
	// (succeeded) and the second Writef (errored). The third,
	// fourth, and fifth calls MUST NOT have reached Write.
	assert.Equal(t, 2, fw.calls,
		"expected exactly 2 writes to the underlying writer; got %d — a no-op leak means the sticky gate is broken", fw.calls)

	// Err reports the first failure, and errors.Is sees through
	// to the underlying brokenPipe sentinel.
	require.Error(t, sw.Err())
	assert.ErrorIs(t, sw.Err(), errBrokenPipe)

	// Only the first successful write is in the buffer. The
	// aborted second write MAY have partially-written some bytes
	// to the underlying writer before returning the error (that
	// depends on the writer's semantics); failingWriter here
	// returns 0 on error so the buffer stays at the first line.
	assert.Equal(t, "succeeds\n", fw.inner.String())
}

// TestStickyWriter_ErrStickyAfterFirstError: calling Err multiple
// times after a failure returns the SAME error, not the zero value
// of whatever happened on the last no-op call. Important because
// callers may check Err in multiple places and expect a stable
// answer.
func TestStickyWriter_ErrStickyAfterFirstError(t *testing.T) {
	t.Parallel()

	fw := &failingWriter{limit: 0, failWith: errBrokenPipe}
	sw := &stickyWriter{w: fw}

	sw.Writef("errors immediately\n")
	err1 := sw.Err()
	sw.Writef("no-op\n")
	err2 := sw.Err()
	sw.Writeln("no-op")
	err3 := sw.Err()

	assert.ErrorIs(t, err1, errBrokenPipe)
	assert.Same(t, err1, err2,
		"Err must return the SAME error across calls, not overwrite with later no-op state")
	assert.Same(t, err2, err3)
}

// TestStickyWriter_WritefAndWritelnEquivalence: Writef with a
// trailing newline behaves equivalently to Writeln for error-
// propagation purposes (both go through the underlying writer's
// Write method in the happy case and short-circuit on error).
func TestStickyWriter_WritefWritelnParity(t *testing.T) {
	t.Parallel()

	fw := &failingWriter{limit: 0, failWith: errBrokenPipe}
	sw := &stickyWriter{w: fw}

	sw.Writeln("first") // errors
	sw.Writef("second") // must be no-op
	sw.Writeln("third") // must be no-op

	assert.Equal(t, 1, fw.calls)
	require.Error(t, sw.Err())
	assert.ErrorIs(t, sw.Err(), errBrokenPipe)
}

// ===== displayHuman integration =====

// TestDisplayHuman_PropagatesWriteError confirms the full render-
// path integration: when the writer fails partway through, the
// function returns the error AND short-circuits remaining writes.
// The absolute call count under the limit matters less than the
// contract — "returns error, doesn't spin forever."
func TestDisplayHuman_PropagatesWriteError(t *testing.T) {
	t.Parallel()

	// Fail on the 2nd write — after the first line lands, every
	// subsequent Writef/Writeln in displayHuman is a no-op.
	fw := &failingWriter{limit: 1, failWith: errBrokenPipe}

	display := &AnalysisDisplay{
		Profile: &profile.Profile{
			Entity: profile.Entity{
				ShortName:    "kong",
				CanonicalURI: "repo:github/alecthomas/kong",
				Type:         profile.EntityProject,
			},
			Signals: []profile.Signal{
				// Non-empty signals ensure the render would
				// otherwise make many more writes — amplifying the
				// no-op benefit when broken-pipe short-circuits.
				{Type: "stars", Group: profile.SignalGroupCriticality, Value: []byte(`{"count":1000}`)},
				{Type: "last_commit", Group: profile.SignalGroupVitality, Value: []byte(`{"days_ago":5}`)},
			},
		},
	}

	err := displayHuman(fw, display, 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, errBrokenPipe,
		"broken-pipe from the underlying writer must propagate up through displayHuman")

	// Exactly two writes attempted: the first (succeeded) and the
	// second (failed). Without short-circuit, displayHuman would
	// fire ~20-30 writes on this fixture.
	assert.Equal(t, 2, fw.calls,
		"stickyWriter must short-circuit after the first error; got %d writes (expected 2)",
		fw.calls)
}

// TestDisplayHuman_Success confirms the happy path: a writer that
// never fails receives the full rendered output and displayHuman
// returns nil. Pairs with the error-propagation test to lock in
// the two-sided contract.
func TestDisplayHuman_Success(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	display := &AnalysisDisplay{
		Profile: &profile.Profile{
			Entity: profile.Entity{
				ShortName:    "kong",
				CanonicalURI: "repo:github/alecthomas/kong",
				Type:         profile.EntityProject,
			},
		},
	}

	err := displayHuman(&buf, display, 0)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "kong")
	assert.Contains(t, buf.String(), "repo:github/alecthomas/kong")
	assert.Contains(t, buf.String(), "Total signals:")
}

// TestPrintCompactValue_ShortCircuits confirms that printCompactValue
// participates in the sticky-error chain. A pre-errored writer
// causes zero new writes from printCompactValue, not one-per-key.
func TestPrintCompactValue_ShortCircuits(t *testing.T) {
	t.Parallel()

	fw := &failingWriter{limit: 0, failWith: errBrokenPipe}
	sw := &stickyWriter{w: fw}

	// Prime the error state.
	sw.Writef("fails\n")
	calls := fw.calls
	require.Error(t, sw.Err())

	// Now have printCompactValue try to write a multi-key map.
	// It MUST NOT write anything further.
	printCompactValue(sw, map[string]any{
		"count":    1000,
		"days_ago": 5,
		"ratio":    0.8,
	})

	assert.Equal(t, calls, fw.calls,
		"printCompactValue must not write after the stickyWriter has errored; saw %d additional writes",
		fw.calls-calls)
}

// ===== helper to satisfy Writer interface in fixtures =====

// Ensure failingWriter implements io.Writer. Compile-time check.
var _ io.Writer = (*failingWriter)(nil)
