package source

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"iter"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/sarahmaeve/signatory/internal/gitenv"
	"github.com/sarahmaeve/signatory/internal/signal/source/golang"
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
// No filesystem mutation, no working tree allocation, no fetch from
// remote — purely read operations against the existing object DB.
// If a requested SHA isn't present locally (which can happen when
// the proxy.golang.org pin table includes a version that --refresh
// did not fetch), ReadBlob returns ErrSHAMissingFromClone; the
// matrix-row assembler preserves the row with tag_sha_local_status
// rather than treating it as a fatal error.
type BlobStreamer struct {
	clonePath string

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bytes.Buffer

	parentCtx    context.Context
	parentCancel context.CancelFunc

	mu     sync.Mutex
	closed bool
}

// TreeBlob is one entry from `git ls-tree -r`. Mode is the git
// file mode (e.g., "100644"); SHA is the blob's git object hash;
// Path is posix-style, relative to the tree root.
type TreeBlob struct {
	Mode string
	SHA  string
	Path string
}

// NewBlobStreamer starts a `git cat-file --batch` subprocess
// against clonePath and returns a streamer ready for reads. The
// subprocess persists for the streamer's lifetime; call Close to
// terminate it cleanly.
//
// Fails with ErrNoClone if clonePath is empty. Subprocess startup
// errors are wrapped with context.
func NewBlobStreamer(clonePath string) (*BlobStreamer, error) {
	if clonePath == "" {
		return nil, ErrNoClone
	}

	parentCtx, parentCancel := context.WithCancel(context.Background())
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
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		parentCancel()
		return nil, fmt.Errorf("start cat-file --batch in %q: %w", clonePath, err)
	}

	return &BlobStreamer{
		clonePath:    clonePath,
		cmd:          cmd,
		stdin:        stdin,
		stdout:       bufio.NewReader(stdout),
		stderr:       &stderr,
		parentCtx:    parentCtx,
		parentCancel: parentCancel,
	}, nil
}

// ReadBlob fetches the bytes of a git blob by its object SHA.
// Returns ErrSHAMissingFromClone if the SHA isn't in the local
// object DB (the cat-file response is `<sha> missing`).
//
// The cat-file --batch protocol is line-then-bytes: write the SHA
// to stdin, read a header line (`<sha> <type> <size>` on success or
// `<sha> missing` on failure), then for success read exactly <size>
// bytes followed by a trailing newline. Concurrent ReadBlob calls
// would corrupt this framing, so the method is serialized via a
// mutex.
func (b *BlobStreamer) ReadBlob(ctx context.Context, sha string) ([]byte, error) {
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
func (b *BlobStreamer) ListTreeBlobs(ctx context.Context, sha string) ([]TreeBlob, error) {
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

// EnumerateGoFiles iterates Go source files at the commit SHA,
// excluding _test.go files and vendored code. Each file's content
// is fetched on demand from the persistent cat-file subprocess;
// stopping iteration partway through (yield returns false) is safe
// and skips the unread blobs.
//
// Errors are yielded in-band:
//   - If ListTreeBlobs fails (e.g., ErrSHAMissingFromClone) the
//     iterator yields one (zero SourceFile, error) pair and stops.
//   - If a per-blob ReadBlob fails, the error is yielded with an
//     empty Content; iteration continues.
//   - If ctx is cancelled mid-iteration, ctx.Err() is yielded and
//     iteration stops.
func (b *BlobStreamer) EnumerateGoFiles(ctx context.Context, sha string) iter.Seq2[golang.SourceFile, error] {
	return func(yield func(golang.SourceFile, error) bool) {
		blobs, err := b.ListTreeBlobs(ctx, sha)
		if err != nil {
			yield(golang.SourceFile{}, err)
			return
		}
		for _, blob := range blobs {
			if !isGoSourceFile(blob.Path) {
				continue
			}
			if err := ctx.Err(); err != nil {
				yield(golang.SourceFile{}, err)
				return
			}
			content, err := b.ReadBlob(ctx, blob.SHA)
			if err != nil {
				if !yield(golang.SourceFile{Path: blob.Path}, err) {
					return
				}
				continue
			}
			if !yield(golang.SourceFile{Path: blob.Path, Content: content}, nil) {
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
// most of the time, but parent-context cancellation can race and
// produce "signal: killed" — both are correct shutdowns from our
// perspective.
func isExpectedShutdownErr(err error) bool {
	if err == nil {
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
	}
	return false
}
