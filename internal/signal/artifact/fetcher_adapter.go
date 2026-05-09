package artifact

import (
	"context"
	"io"
	"time"

	"github.com/sarahmaeve/signatory/internal/artifact/stream"
)

// StreamFetcherOptions configures a stream-backed ArtifactFetcher.
// Mirror of the legacy HTTPFetcherOptions API so existing call
// sites don't need shape changes — only the constructor name and
// the package the fetcher comes from. Production wiring uses this;
// tests typically supply their own ArtifactFetcher stubs without
// instantiating one of these.
type StreamFetcherOptions struct {
	// MaxBytes caps the HTTP body read (passed through to
	// stream.Fetcher.Fetch as the per-request limit). Required (>0).
	MaxBytes int64

	// Timeout is the per-request total budget. Zero falls back to
	// stream.DefaultFetchTimeout via stream.NewFetcher.
	Timeout time.Duration

	// UserAgent overrides the default identification string. Empty
	// falls back to stream's default. Useful for fork builds that
	// want their own attribution in registry logs.
	UserAgent string
}

// NewStreamArtifactFetcher returns an ArtifactFetcher backed by
// stream.Fetcher. The collector's Fetcher slot accepts any
// ArtifactFetcher implementation, so production wiring uses this and
// tests inject lighter-weight stubs.
//
// Panics on MaxBytes <= 0 — production-required value, surfaced
// loudly at construction rather than at first request. (Timeout
// resolves to a default; only MaxBytes is non-recoverable to
// misconfigure.)
func NewStreamArtifactFetcher(opts StreamFetcherOptions) ArtifactFetcher {
	if opts.MaxBytes <= 0 {
		panic("artifact.NewStreamArtifactFetcher: MaxBytes must be positive")
	}
	return &streamArtifactFetcher{
		f: stream.NewFetcher(stream.FetcherOptions{
			Timeout:   opts.Timeout,
			UserAgent: opts.UserAgent,
		}),
		limit: opts.MaxBytes,
	}
}

// streamArtifactFetcher adapts a *stream.Fetcher to the
// ArtifactFetcher interface. Single-method indirection — Fetch
// delegates straight through.
type streamArtifactFetcher struct {
	f     *stream.Fetcher
	limit int64
}

func (s *streamArtifactFetcher) Fetch(ctx context.Context, url string) (io.ReadCloser, error) {
	return s.f.Fetch(ctx, url, s.limit)
}
