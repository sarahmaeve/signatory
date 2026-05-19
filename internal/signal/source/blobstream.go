package source

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/sarahmaeve/signatory/internal/gitenv"
	"github.com/sarahmaeve/signatory/internal/signal/source/astfeature"
)

// BlobStreamer reads source content from a local git clone via two
// primitives: a persistent `git cat-file --batch` subprocess for
// content fetches, and one-shot `git ls-tree -r <sha>` invocations
// for tree enumeration.
//
// One BlobStreamer owns one subprocess for its lifetime. ReadBlob
// is serialized via a mutex; concurrent calls would interleave the
// cat-file protocol's request/response framing.
//
// No filesystem mutation, no working tree allocation. By default no
// fetch from remote either — purely read operations against the
// existing object DB. If a requested SHA isn't present locally
// (which can happen when the proxy.golang.org pin table includes a
// version that --refresh did not fetch), ReadBlob returns
// ErrSHAMissingFromClone; the matrix-row assembler preserves the
// row with tag_sha_local_status rather than treating it as a fatal
// error.
//
// WithAllowFetch enables an opt-in `git fetch origin` retry on
// missing-SHA. The fetch is at most once per BlobStreamer lifetime
// — see ensureFetched for the rationale.
type BlobStreamer struct {
	clonePath string

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	// stderr is the cat-file subprocess's captured stderr.
	// os/exec's internal stderr-pump goroutine writes to it for the
	// subprocess's lifetime (from cmd.Start until cmd.Wait returns).
	// readBlobOnce reads it via String() when a mid-flight stdin/
	// stdout op fails (subprocess died, broken pipe, ctx cancelled)
	// to include the subprocess's last words in the error. On a
	// plain *bytes.Buffer those reads race the pump goroutine's
	// writes (surfaced by -race in
	// TestBlobStreamer_ConstructionCtxCancel_TerminatesSubprocess
	// during teardown), so we wrap with the small mutex-protected
	// safeBuffer below.
	stderr *safeBuffer

	parentCtx    context.Context
	parentCancel context.CancelFunc

	mu     sync.Mutex
	closed bool

	// allowFetch is the WithAllowFetch toggle. When true,
	// missing-SHA reads trigger a single `git fetch origin` retry
	// per BlobStreamer lifetime via ensureFetched.
	allowFetch bool

	// fetchMu protects fetched + the actual fetch operation;
	// concurrent missing-SHA reads must not multiply the fetch.
	fetchMu sync.Mutex
	fetched bool

	// maxBlobSize bounds the per-blob byte allocation in
	// readBlobOnce. The cat-file --batch protocol reports each
	// blob's size in its header line; that size is then used as
	// the make([]byte, size) capacity. Without a cap, a tampered
	// loose object header claiming a multi-GiB size would drive
	// signatory's allocator into territory that has nothing to do
	// with the actual file content. Default defaultMaxBlobSize;
	// override via WithMaxBlobSize for tests or for callers that
	// know their content fits a smaller envelope.
	maxBlobSize int

	// sourceFileFilter decides which tree paths EnumerateSourceFiles
	// yields. Defaults to isGoSourceFile; WithSourceFileFilter swaps
	// in a language-appropriate predicate (e.g. isPythonSourceFile
	// for pypi entities) so the same streamer serves any ecosystem.
	sourceFileFilter func(path string) bool
}

// safeBuffer is a bytes.Buffer with a mutex around Write and String
// so the os/exec stderr-pump goroutine and concurrent readers
// (readBlobOnce formatting a mid-flight error) don't race on the
// long-lived cat-file subprocess stderr. os/exec spawns the pump
// goroutine when cmd.Start runs and joins it when cmd.Wait returns —
// any read in between needs the lock. One-shot exec.Cmd invocations
// (ListTreeBlobs, DiffStat, fetchOrigin) collect stderr into a stack
// *bytes.Buffer and read it only after cmd.Run returns, which is
// safe; safeBuffer is only used for the persistent stderr.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// defaultMaxBlobSize is the per-blob allocation cap applied to
// readBlobOnce when WithMaxBlobSize is not supplied. 10 MiB matches
// the maxResponseSize used by the github, gopublish, npm, and pypi
// HTTP clients — generous for any realistic Go source file (typical
// .go files are <100 KiB; the largest go.mod files are ~1 MiB) while
// still bounding allocation by two orders of magnitude below where
// a forged size header would do damage.
const defaultMaxBlobSize = 10 * 1024 * 1024

// BlobStreamerOption configures a BlobStreamer at construction.
// Functional-options pattern so future options (timeout, fetch
// scope, parallelism) compose cleanly without breaking callers.
type BlobStreamerOption func(*BlobStreamer)

// WithAllowFetch enables a single `git fetch origin` retry when
// ReadBlob or ListTreeBlobs encounters ErrSHAMissingFromClone. The
// retry happens at most once per BlobStreamer lifetime; if the
// fetch doesn't recover the missing SHA, subsequent missing-SHA
// failures return ErrSHAMissingFromClone immediately rather than
// re-fetching (a second fetch wouldn't change the outcome).
//
// Default is false (no fetch). With --no-fetch as the operator
// default, allowFetch is opt-in for the case where the operator
// knows the clone may be stale relative to the proxy and prefers
// completeness over the missing-SHA signal.
func WithAllowFetch(allow bool) BlobStreamerOption {
	return func(b *BlobStreamer) {
		b.allowFetch = allow
	}
}

// WithMaxBlobSize overrides the per-blob allocation cap applied by
// readBlobOnce. The cap rejects any cat-file response whose declared
// size exceeds this value before the make([]byte, size) allocation,
// returning ErrBlobSizeExceedsCap. Useful for tests (where small
// fixture blobs let the cap fire on values much smaller than the
// production default) and for callers with stricter memory budgets.
//
// A value <= 0 is treated as "use default" rather than "no limit" —
// disabling the cap is a footgun for a defensive bound, and the
// signature stays simple by collapsing both invalid inputs to the
// safe default.
func WithMaxBlobSize(n int) BlobStreamerOption {
	return func(b *BlobStreamer) {
		if n > 0 {
			b.maxBlobSize = n
		}
	}
}

// WithSourceFileFilter overrides the predicate EnumerateSourceFiles
// uses to decide which tree paths to yield. The default is
// isGoSourceFile; the source-evolution collector passes
// isPythonSourceFile for pypi entities. A nil filter is ignored
// (the Go default stays) rather than yielding every blob — an
// unfiltered stream would feed non-source into the analyzer.
func WithSourceFileFilter(filter func(path string) bool) BlobStreamerOption {
	return func(b *BlobStreamer) {
		if filter != nil {
			b.sourceFileFilter = filter
		}
	}
}

// TreeBlob is one entry from `git ls-tree -r`. Mode is the git
// file mode (e.g., "100644"); SHA is the blob's git object hash;
// Path is posix-style, relative to the tree root.
type TreeBlob struct {
	Mode string
	SHA  string
	Path string
}

// DiffStat captures the file- and line-count delta between two
// commit SHAs. Populates the matrix row's `diff_from_previous`
// block. Counts are non-negative; "file added" and "file removed"
// are mutually exclusive per file (a rename counts as one Changed
// entry, not Added+Removed).
//
// Binary files contribute to FilesChanged/Added/Removed but not
// to LinesAdded/LinesRemoved — git's --numstat marks binaries
// with "-\t-\t<path>" so the line counts are unknown.
type DiffStat struct {
	FilesAdded   int `json:"files_added"`
	FilesChanged int `json:"files_changed"`
	FilesRemoved int `json:"files_removed"`
	LinesAdded   int `json:"lines_added"`
	LinesRemoved int `json:"lines_removed"`
}

// NewBlobStreamer starts a `git cat-file --batch` subprocess
// against clonePath and returns a streamer ready for reads. The
// subprocess persists for the streamer's lifetime; call Close to
// terminate it cleanly.
//
// ctx bounds the subprocess: the persistent cat-file child is
// derived from ctx (via context.WithCancel), so cancelling the
// caller's context — e.g. an aborted collection run — tears the
// subprocess down rather than leaking a git process until Close.
// Close remains the explicit teardown path; ctx cancellation is the
// implicit one tied to the request lifetime.
//
// Fails with ErrNoClone if clonePath is empty. Subprocess startup
// errors are wrapped with context. Options are applied in order
// after struct initialization.
func NewBlobStreamer(ctx context.Context, clonePath string, opts ...BlobStreamerOption) (*BlobStreamer, error) {
	if clonePath == "" {
		return nil, ErrNoClone
	}

	parentCtx, parentCancel := context.WithCancel(ctx)
	cmd := gitenv.NewCmd(parentCtx, "-C", clonePath, "cat-file", "--batch")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		parentCancel()
		return nil, fmt.Errorf("cat-file stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		parentCancel()
		return nil, fmt.Errorf("cat-file stdout pipe: %w", err)
	}
	stderr := &safeBuffer{}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		parentCancel()
		return nil, fmt.Errorf("start cat-file --batch in %q: %w", clonePath, err)
	}

	bs := &BlobStreamer{
		clonePath:        clonePath,
		cmd:              cmd,
		stdin:            stdin,
		stdout:           bufio.NewReader(stdout),
		stderr:           stderr,
		parentCtx:        parentCtx,
		parentCancel:     parentCancel,
		maxBlobSize:      defaultMaxBlobSize,
		sourceFileFilter: isGoSourceFile,
	}
	// Options apply after the defaults so a caller's WithMaxBlobSize
	// (or future tunables) overrides the const baked into the struct
	// initializer.
	for _, opt := range opts {
		opt(bs)
	}
	return bs, nil
}

// ReadBlob fetches the bytes of a git blob by its object SHA.
// Returns ErrSHAMissingFromClone if the SHA isn't in the local
// object DB (the cat-file response is `<sha> missing`).
//
// With WithAllowFetch enabled, a missing-SHA failure triggers one
// `git fetch origin` retry per BlobStreamer lifetime; the read is
// then attempted once more. If the fetch doesn't recover the SHA,
// or if a subsequent missing-SHA happens after the fetch was
// already attempted, ErrSHAMissingFromClone is returned immediately
// (no further retry — see ensureFetched).
func (b *BlobStreamer) ReadBlob(ctx context.Context, sha string) ([]byte, error) {
	content, err := b.readBlobOnce(ctx, sha)
	if err == nil {
		return content, nil
	}
	if !errors.Is(err, ErrSHAMissingFromClone) || !b.allowFetch {
		return nil, err
	}
	if fetchErr := b.ensureFetched(ctx); fetchErr != nil {
		return nil, errors.Join(err, fmt.Errorf("allow-fetch retry: %w", fetchErr))
	}
	return b.readBlobOnce(ctx, sha)
}

// readBlobOnce is the single-shot blob fetch — write SHA, parse
// response. Public ReadBlob wraps this with the allow-fetch retry.
//
// The cat-file --batch protocol is line-then-bytes: write the SHA
// to stdin, read a header line (`<sha> <type> <size>` on success or
// `<sha> missing` on failure), then for success read exactly <size>
// bytes followed by a trailing newline. Concurrent ReadBlob calls
// would corrupt this framing, so the method is serialized via a
// mutex.
func (b *BlobStreamer) readBlobOnce(ctx context.Context, sha string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil, ErrBlobStreamerClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if _, err := fmt.Fprintln(b.stdin, sha); err != nil {
		return nil, fmt.Errorf("write to cat-file stdin: %w (stderr: %s)", err, b.stderr.String())
	}

	headerLine, err := b.stdout.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read cat-file header: %w (stderr: %s)", err, b.stderr.String())
	}
	headerLine = strings.TrimRight(headerLine, "\n")

	parts := strings.Fields(headerLine)
	if len(parts) == 2 && parts[1] == "missing" {
		return nil, fmt.Errorf("%w: %s", ErrSHAMissingFromClone, parts[0])
	}
	if len(parts) != 3 {
		return nil, fmt.Errorf("unexpected cat-file response: %q", headerLine)
	}

	size, err := strconv.Atoi(parts[2])
	if err != nil {
		return nil, fmt.Errorf("parse cat-file size %q: %w", parts[2], err)
	}
	if size < 0 {
		return nil, fmt.Errorf("negative cat-file size %d", size)
	}
	// Cap before allocation: a tampered loose object could claim
	// any size in its header. Without this check, make([]byte, size)
	// is the only thing standing between signatory and a multi-GiB
	// allocation driven by attacker-controlled bytes. The cat-file
	// pipe is now in an indeterminate framing state — the reported
	// size bytes are still queued upstream — so we close the stream
	// rather than risk reading them on a subsequent ReadBlob (the
	// trailing-newline parser at the bottom of this function would
	// hit one of the queued bytes instead). Close is idempotent and
	// callers already handle ReadBlob errors as terminal for their
	// loop; the assembler treats this as a partial-row case.
	if size > b.maxBlobSize {
		_ = b.closeLocked()
		return nil, fmt.Errorf("%w: %d > %d", ErrBlobSizeExceedsCap, size, b.maxBlobSize)
	}

	content := make([]byte, size)
	if _, err := io.ReadFull(b.stdout, content); err != nil {
		return nil, fmt.Errorf("read cat-file content (size=%d): %w", size, err)
	}

	// Trailing newline that terminates each batch response.
	nl, err := b.stdout.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("read cat-file trailer: %w", err)
	}
	if nl != '\n' {
		return nil, fmt.Errorf("expected newline trailer after cat-file content, got %q", string(nl))
	}

	return content, nil
}

// ListTreeBlobs runs `git ls-tree -r <sha>` and returns every blob
// entry in the resolved tree (commits are dereferenced to their
// trees automatically). Tree-type entries are skipped; only blob
// entries are returned.
//
// Returns ErrSHAMissingFromClone if the SHA isn't a known object.
// git's stderr is inspected for the canonical phrases that indicate
// a missing object — slightly brittle (git wording can shift across
// versions) but the alternative requires a separate rev-parse call.
//
// With WithAllowFetch enabled, behaves the same as ReadBlob: one
// fetch retry per BlobStreamer lifetime on missing-SHA.
func (b *BlobStreamer) ListTreeBlobs(ctx context.Context, sha string) ([]TreeBlob, error) {
	blobs, err := b.listTreeBlobsOnce(ctx, sha)
	if err == nil {
		return blobs, nil
	}
	if !errors.Is(err, ErrSHAMissingFromClone) || !b.allowFetch {
		return nil, err
	}
	if fetchErr := b.ensureFetched(ctx); fetchErr != nil {
		return nil, errors.Join(err, fmt.Errorf("allow-fetch retry: %w", fetchErr))
	}
	return b.listTreeBlobsOnce(ctx, sha)
}

// listTreeBlobsOnce is the single-shot ls-tree call. Public
// ListTreeBlobs wraps this with the allow-fetch retry.
func (b *BlobStreamer) listTreeBlobsOnce(ctx context.Context, sha string) ([]TreeBlob, error) {
	if b == nil {
		return nil, ErrBlobStreamerClosed
	}

	cmd := gitenv.NewCmd(ctx, "-C", b.clonePath, "ls-tree", "-r", sha)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		stderrStr := stderr.String()
		if isMissingObjectMessage(stderrStr) {
			return nil, fmt.Errorf("%w: %s", ErrSHAMissingFromClone, sha)
		}
		return nil, fmt.Errorf("git ls-tree %s: %w (stderr: %s)", sha, err, stderrStr)
	}

	var blobs []TreeBlob
	scanner := bufio.NewScanner(&stdout)
	// ls-tree lines can be long for deep paths; raise the buffer
	// from the default 64KiB to 1MiB defensively.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		// Format: <mode> <type> <sha>\t<path>
		// E.g.:   "100644 blob abc123\tpath/to/file.go"
		meta, path, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}

		fields := strings.Fields(meta)
		if len(fields) != 3 {
			continue
		}
		if fields[1] != "blob" {
			continue
		}

		blobs = append(blobs, TreeBlob{
			Mode: fields[0],
			SHA:  fields[2],
			Path: path,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan ls-tree output for %s: %w", sha, err)
	}
	return blobs, nil
}

// EnumerateSourceFiles iterates source files at the commit SHA. This
// is the Go-flavored implementation of the language-neutral
// SourceProvider.EnumerateSourceFiles contract: it filters to Go
// sources (isGoSourceFile), excluding _test.go files and vendored
// code. A Python provider would implement the same interface with a
// .py filter. Each file's content is fetched on demand from the
// persistent cat-file subprocess; stopping iteration partway through
// (yield returns false) is safe and skips the unread blobs.
//
// Errors are yielded in-band:
//   - If ListTreeBlobs fails (e.g., ErrSHAMissingFromClone) the
//     iterator yields one (zero SourceFile, error) pair and stops.
//   - If a per-blob ReadBlob fails, the error is yielded with an
//     empty Content; iteration continues.
//   - If ctx is cancelled mid-iteration, ctx.Err() is yielded and
//     iteration stops.
func (b *BlobStreamer) EnumerateSourceFiles(ctx context.Context, sha string) iter.Seq2[astfeature.SourceFile, error] {
	return func(yield func(astfeature.SourceFile, error) bool) {
		blobs, err := b.ListTreeBlobs(ctx, sha)
		if err != nil {
			yield(astfeature.SourceFile{}, err)
			return
		}
		for _, blob := range blobs {
			if !b.sourceFileFilter(blob.Path) {
				continue
			}
			if err := ctx.Err(); err != nil {
				yield(astfeature.SourceFile{}, err)
				return
			}
			content, err := b.ReadBlob(ctx, blob.SHA)
			if err != nil {
				if !yield(astfeature.SourceFile{Path: blob.Path}, err) {
					return
				}
				continue
			}
			if !yield(astfeature.SourceFile{Path: blob.Path, Content: content}, nil) {
				return
			}
		}
	}
}

// Close terminates the cat-file subprocess by closing its stdin and
// waiting for exit. Safe to call multiple times; subsequent
// Close-after-close returns nil. After Close, ReadBlob returns
// ErrBlobStreamerClosed.
//
// Close cancels the parent context as a fallback in case the
// subprocess doesn't exit cleanly on stdin close. Common shutdown
// errors ("signal: killed", "broken pipe") are treated as expected
// and not surfaced.
func (b *BlobStreamer) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closeLocked()
}

// closeLocked is the no-mutex-acquire path used both by Close (which
// takes b.mu before delegating) and by callers that already hold
// b.mu and need to abort the subprocess mid-operation — readBlobOnce
// uses this when the cat-file header reports a size over the cap and
// the pipe framing is no longer trustworthy. Pre-condition: b.mu is
// held. Idempotent under repeat calls.
func (b *BlobStreamer) closeLocked() error {
	if b.closed {
		return nil
	}
	b.closed = true

	if b.stdin != nil {
		_ = b.stdin.Close()
	}
	b.parentCancel()

	if err := b.cmd.Wait(); err != nil {
		if !isExpectedShutdownErr(err) {
			return fmt.Errorf("cat-file subprocess wait: %w (stderr: %s)", err, b.stderr.String())
		}
	}
	return nil
}

// DiffStat returns the file- and line-count delta between sha1
// and sha2 (in that order: sha1 is "before", sha2 is "after").
// Both SHAs must be present in the local clone — there's no
// fetch-retry path here because diff between two SHAs requires
// both to be in the object DB simultaneously, and the missing-SHA
// case for a row is handled at matrix-row assembly time.
//
// Implementation runs two git invocations:
//   - `git diff --numstat sha1 sha2` for line counts per file
//   - `git diff --name-status sha1 sha2` for per-file add/modify/
//     delete classification
//
// Both are local porcelain (NewCmd, no WaitDelay). One subprocess
// per invocation; the cost is two forks but keeps parsing simple
// and avoids parsing unified-diff format. Renames are counted as
// FilesChanged (a single semantic change), not Added+Removed.
func (b *BlobStreamer) DiffStat(ctx context.Context, sha1, sha2 string) (DiffStat, error) {
	var stat DiffStat

	// Pass 1: numstat → line counts.
	numstatCmd := gitenv.NewCmd(ctx, "-C", b.clonePath, "diff", "--numstat", sha1, sha2)
	var numstatOut, numstatErr bytes.Buffer
	numstatCmd.Stdout = &numstatOut
	numstatCmd.Stderr = &numstatErr
	if err := numstatCmd.Run(); err != nil {
		stderrStr := numstatErr.String()
		if isMissingObjectMessage(stderrStr) {
			return DiffStat{}, fmt.Errorf("%w: diff %s..%s", ErrSHAMissingFromClone, sha1, sha2)
		}
		return DiffStat{}, fmt.Errorf("git diff --numstat %s..%s: %w (stderr: %s)", sha1, sha2, err, stderrStr)
	}

	numstatScanner := bufio.NewScanner(&numstatOut)
	numstatScanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for numstatScanner.Scan() {
		line := numstatScanner.Text()
		// Format: <added>\t<removed>\t<path>
		// Binary files: "-\t-\t<path>"
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[0] == "-" || parts[1] == "-" {
			// Binary file — counted as a file at the name-status
			// pass below, but no line count.
			continue
		}
		added, err1 := strconv.Atoi(parts[0])
		removed, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			continue
		}
		stat.LinesAdded += added
		stat.LinesRemoved += removed
	}
	if err := numstatScanner.Err(); err != nil {
		return DiffStat{}, fmt.Errorf("scan numstat output for %s..%s: %w", sha1, sha2, err)
	}

	// Pass 2: name-status → file add/modify/delete classification.
	nsCmd := gitenv.NewCmd(ctx, "-C", b.clonePath, "diff", "--name-status", sha1, sha2)
	var nsOut, nsErr bytes.Buffer
	nsCmd.Stdout = &nsOut
	nsCmd.Stderr = &nsErr
	if err := nsCmd.Run(); err != nil {
		stderrStr := nsErr.String()
		if isMissingObjectMessage(stderrStr) {
			return DiffStat{}, fmt.Errorf("%w: diff %s..%s", ErrSHAMissingFromClone, sha1, sha2)
		}
		return DiffStat{}, fmt.Errorf("git diff --name-status %s..%s: %w (stderr: %s)", sha1, sha2, err, stderrStr)
	}

	nsScanner := bufio.NewScanner(&nsOut)
	nsScanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for nsScanner.Scan() {
		line := nsScanner.Text()
		if line == "" {
			continue
		}
		// Format: <status>\t<path>[\t<new-path>]
		// Status codes:
		//   A = added
		//   D = deleted
		//   M = modified (content changed)
		//   T = typechange (e.g., file → symlink)
		//   R<num> = renamed (with similarity %)
		//   C<num> = copied (with similarity %)
		switch line[0] {
		case 'A':
			stat.FilesAdded++
		case 'D':
			stat.FilesRemoved++
		case 'M', 'T', 'R', 'C':
			stat.FilesChanged++
		}
	}
	if err := nsScanner.Err(); err != nil {
		return DiffStat{}, fmt.Errorf("scan name-status output for %s..%s: %w", sha1, sha2, err)
	}

	return stat, nil
}

// ensureFetched runs `git fetch origin` at most once per
// BlobStreamer lifetime, recording success in b.fetched. Multiple
// goroutines / sequential failed reads see one actual fetch.
//
// Why "at most once": the production case for allow-fetch is "the
// proxy emitted a SHA the local --refresh missed; bring the local
// up to date." A full `git fetch origin` covers that. If the SHA
// is STILL missing after fetch, it's likely a force-push case
// (proxy has a SHA the origin no longer has at any ref) — fetching
// again can't recover it, so subsequent missing-SHA failures
// should fail fast rather than re-fetching uselessly.
func (b *BlobStreamer) ensureFetched(ctx context.Context) error {
	b.fetchMu.Lock()
	defer b.fetchMu.Unlock()
	if b.fetched {
		return nil
	}
	if err := b.fetchOrigin(ctx); err != nil {
		// Mark as attempted even on failure so we don't retry the
		// failed fetch on every subsequent missing-SHA. The fetch
		// fails fast on second-and-later attempts via this short-
		// circuit.
		b.fetched = true
		return err
	}
	b.fetched = true
	return nil
}

// fetchOrigin runs a network-spawning `git fetch origin --quiet`
// against the local clone. Uses gitenv.NewCloneCmd because fetch
// may fork ssh/credential-helper grandchildren that need
// WaitDelay-based subprocess discipline.
//
// Doesn't target a specific SHA: a full fetch is more robust than
// `fetch origin <sha>` (which requires server config to allow
// fetching by SHA). The cost is a slightly bigger fetch payload —
// acceptable because allow-fetch is opt-in for known-stale clones.
func (b *BlobStreamer) fetchOrigin(ctx context.Context) error {
	cmd := gitenv.NewCloneCmd(ctx, "-C", b.clonePath, "fetch", "origin", "--quiet")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git fetch origin in %q: %w (stderr: %s)", b.clonePath, err, stderr.String())
	}
	return nil
}

// isGoSourceFile reports whether the given posix-style path is a
// Go source file the source-evolution analyzer wants to consume.
// Excludes:
//   - non-.go files
//   - test files (_test.go) — they don't run on import
//   - vendored code (vendor/ at root or any nested vendor/ dir) —
//     not the package's own code
func isGoSourceFile(path string) bool {
	if !strings.HasSuffix(path, ".go") {
		return false
	}
	if strings.HasSuffix(path, "_test.go") {
		return false
	}
	if path == "vendor" {
		return false
	}
	if strings.HasPrefix(path, "vendor/") {
		return false
	}
	if strings.Contains(path, "/vendor/") {
		return false
	}
	return true
}

// isPythonSourceFile reports whether the given posix-style path is a
// Python source file the source-evolution analyzer wants to consume.
// The Python analog of isGoSourceFile — same intent: the package's
// own importable runtime source, not tests or bundled third-party
// code. Excludes:
//   - non-.py files (incl. .pyc bytecode and .pyi type stubs, which
//     are not importable runtime source)
//   - test files: test_*.py, *_test.py, conftest.py — they don't run
//     on import
//   - tests/ and test/ directories at any depth
//   - vendored / virtual-env trees: vendor/, _vendor/, site-packages/,
//     .venv/, venv/ at any depth
//
// Heuristic, deliberately conservative. Python lacks Go's single
// vendor/ convention, so the exclude set is a best guess refined
// when the Python analyzer (and real-data comparability) lands.
func isPythonSourceFile(path string) bool {
	if !strings.HasSuffix(path, ".py") {
		return false
	}
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if base == "conftest.py" ||
		strings.HasPrefix(base, "test_") ||
		strings.HasSuffix(base, "_test.py") {
		return false
	}
	for seg := range strings.SplitSeq(path, "/") {
		switch seg {
		case "tests", "test", "vendor", "_vendor", "site-packages", ".venv", "venv":
			return false
		}
	}
	return true
}

// isNodeSourceFile reports whether the given posix-style path is a
// JS/TS source file the source-evolution analyzer wants to consume.
// The Node analog of isGoSourceFile / isPythonSourceFile — same
// intent: the package's own authored runtime source, not tests, type
// declarations, vendored deps, or build output. Excludes:
//   - non-source extensions (only .js/.mjs/.cjs/.ts/.tsx/.jsx)
//   - .d.ts TypeScript declaration files — types, not runtime source
//   - .min.js — minified bundle output, not authored source
//   - test/spec files: *.test.* and *.spec.* (any of the above exts)
//   - __tests__/, test/, tests/ directories at any depth
//   - node_modules/ (vendored), and dist/ build/ out/ build output
//     trees at any depth
//
// Heuristic and deliberately conservative — JS has no single vendor
// convention, so the exclude set is a best effort refined against
// real-data comparability (mirrors the isPythonSourceFile note).
func isNodeSourceFile(path string) bool {
	// .d.ts is a declaration file even though it ends in .ts — check
	// before the extension allowlist so it isn't admitted as TS.
	if strings.HasSuffix(path, ".d.ts") {
		return false
	}
	ext := false
	for _, suf := range []string{".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx"} {
		if strings.HasSuffix(path, suf) {
			ext = true
			break
		}
	}
	if !ext {
		return false
	}

	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	// Minified bundle: any `.min.` segment in the BASENAME means the
	// file is build output, not authored source. The intent of `.min.`
	// is language-side-agnostic — bundlers emit `.min.mjs` and
	// `.min.cjs` for ESM/CJS targets and (rarely) `.min.ts` for TS
	// minifiers — so the suffix-on-the-whole-path `.min.js` check that
	// shipped previously missed every variant other than plain `.js`.
	// Basename-scoped to avoid matching `.min.` in directory components
	// like a hypothetical `vendor/.min.dir/foo.js`.
	if strings.Contains(base, ".min.") {
		return false
	}
	// test/spec file: a ".test." or ".spec." segment in the basename
	// (foo.test.js, bar.spec.tsx).
	if strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return false
	}
	for seg := range strings.SplitSeq(path, "/") {
		switch seg {
		case "__tests__", "test", "tests", "node_modules", "dist", "build", "out":
			return false
		}
	}
	return true
}

// isMissingObjectMessage reports whether the stderr text from a
// failing git command contains a phrasing that indicates the
// requested SHA is unknown or unresolvable. Conservative: matches
// all common git versions but will require an update if a future
// git release renames any of these.
//
// "not a tree object" covers ls-tree's response when the SHA can't
// be dereferenced to a tree — which happens both when the object
// is missing and when the SHA names a different object type. Both
// are "we can't enumerate blobs at this SHA" from our perspective.
func isMissingObjectMessage(stderr string) bool {
	switch {
	case strings.Contains(stderr, "Not a valid object name"):
		return true
	case strings.Contains(stderr, "bad object"):
		return true
	case strings.Contains(stderr, "unknown revision"):
		return true
	case strings.Contains(stderr, "invalid object name"):
		return true
	case strings.Contains(stderr, "not a tree object"):
		return true
	}
	return false
}

// isExpectedShutdownErr reports whether err is one of the
// "subprocess was killed by us" errors that Close should treat as
// success. The cat-file subprocess exits cleanly on stdin close
// most of the time, but parent-context cancellation can race in
// several ways — `cmd.Wait()` may return "signal: killed",
// "signal: terminated", a "broken pipe" wrap, or — when the
// context cancellation propagates faster than the OS-level signal
// — `context.Canceled` / `context.DeadlineExceeded` directly.
// All are correct shutdowns from our perspective.
//
// The errors.Is check covers the context-sentinel cases robustly
// (works whether `cmd.Wait` returns the sentinel verbatim or
// wrapped); the string-match cases handle the os/exec ExitError
// paths that don't unwrap cleanly. Both shapes appeared under
// `go test -race` load where parallel execution shifts timing
// enough to surface every race window.
func isExpectedShutdownErr(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "signal: killed"):
		return true
	case strings.Contains(msg, "signal: terminated"):
		return true
	case strings.Contains(msg, "broken pipe"):
		return true
	case strings.Contains(msg, "file already closed"):
		return true
	case strings.Contains(msg, "context canceled"):
		return true
	case strings.Contains(msg, "context deadline exceeded"):
		return true
	}
	return false
}
