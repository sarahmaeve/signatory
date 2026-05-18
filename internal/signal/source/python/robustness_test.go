package python

import (
	"strings"
	"testing"
	"time"
)

// TestParse_AdversarialNestedInput_TerminatesInBoundedTime is an
// adversarial-robustness regression test.
//
// signatory ingests untrusted package source by design; a malicious
// publisher controls the .py bytes the parser sees, up to the
// BlobStreamer's 10 MiB per-file cap. AST.md §4 makes the contract
// explicit: "Malformed/adversarial input must yield a best-effort
// partial result, never abort the file." A parser that hangs (or
// stack-overflows) on a crafted file blinds the analyzer for that
// version and aborts the whole collection — a successful evasion and
// a DoS.
//
// Each input here is trivial for a linear parser (a few hundred KiB,
// far under the 10 MiB cap) yet pathological for the current
// implementation: resolveFirstArg→splitCallArgs is invoked at every
// nested call site and re-scans to its matching close (O(n^2)), and
// resolveExpr recurses without a depth bound on nested path-builder
// callees (str()/os.path.join()). The deadline is enormous relative
// to a correct linear parse (microseconds), so this is not a flaky
// timing test — it only fires on a genuine super-linear blowup.
func TestParse_AdversarialNestedInput_TerminatesInBoundedTime(t *testing.T) {
	t.Parallel()

	const deadline = 5 * time.Second

	cases := []struct {
		name string
		src  string
	}{
		{
			// Generic nested calls. Not a catalog callee — exercises
			// the parser's "resume at '(' so nested-arg calls are
			// still seen" path, where splitCallArgs rescans the whole
			// remaining tail once per nested site.
			name: "nested generic calls",
			src:  strings.Repeat("f(", 20000) + "1" + strings.Repeat(")", 20000) + "\n",
		},
		{
			// Nested path-builder: str() is a pathPassthroughCallees
			// entry, so resolveExpr recurses one frame per level with
			// no depth cap.
			name: "nested path-builder calls",
			src:  "open(" + strings.Repeat("str(", 20000) + "'/tmp/x'" + strings.Repeat(")", 20000) + ")\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = Parse([]byte(tc.src))
			}()
			select {
			case <-done:
			case <-time.After(deadline):
				t.Fatalf("Parse did not terminate within %s on %d-byte "+
					"adversarial input — super-linear blowup / unbounded "+
					"recursion violates the AST.md §4 leniency contract",
					deadline, len(tc.src))
			}
		})
	}
}
