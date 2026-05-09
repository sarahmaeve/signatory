package stream

import (
	"context"
	"errors"
	"io"
)

// ErrNotImplemented is the placeholder error returned by the
// scaffold's Walk and FetchAndWalk. Phases A2 (tar.gz) and A3 (zip)
// replace it with format-dispatch logic. Exported so consumers can
// errors.Is-test against it during the transition.
var ErrNotImplemented = errors.New("stream: not implemented")

// ErrUnknownFormat is returned by Walk when the supplied Format is
// FormatUnknown or otherwise unrecognized. Surfaced separately from
// ErrNotImplemented so callers can distinguish "the package doesn't
// do this yet" from "the call site forgot to set Format".
var ErrUnknownFormat = errors.New("stream: unknown archive format")

// ErrLimitExceeded is wrapped by every cap-triggered Walk failure
// (MaxTotalBytes, MaxEntryBytes, MaxEntries, MaxCompressionRatio,
// MaxCompressedBytes). Callers errors.Is-test against this to branch
// on cap-vs-malformed without disambiguating which specific cap
// fired — the error string carries that detail for operator
// diagnostics.
var ErrLimitExceeded = errors.New("stream: archive limit exceeded")

// ErrEncryptedEntry is returned when a zip archive contains an
// entry with the encryption flag set. Encrypted entries are useless
// to the divergence collector (we can't read their contents to
// compare against the source repo) and are themselves anomalous in
// release artifacts — failing the walk surfaces the anomaly.
var ErrEncryptedEntry = errors.New("stream: archive contains encrypted entry")

// ErrParserConfusion is returned when a zip archive's local file
// headers and central directory disagree (different counts, missing
// names, or mismatched sizes). Different zip parsers resolve such
// mismatches differently, making them a known supply-chain attack
// vector — Java JAR vs. Python vs. Go can pick different views of
// the same bytes. Failing the walk on detection is the strongest
// signal a defender can act on.
var ErrParserConfusion = errors.New("stream: zip local headers disagree with central directory")

// Walk consumes the archive bytes from src in the declared format,
// emits a manifest of headers, captures bytes for any matching
// CaptureIntent under its MaxSize cap, and enforces lim against
// decompression bombs / path traversal / entry-count floods.
//
// The walker is single-pass and never writes to disk. Zero-valued
// Limits fields fall back to DefaultLimits (see resolveLimits).
//
// Returns ErrUnknownFormat for FormatUnknown. Returns
// ErrNotImplemented for formats whose walker hasn't landed yet
// (FormatZip until A3).
func Walk(ctx context.Context, src io.Reader, format Format,
	intents []CaptureIntent, lim Limits) (*Manifest, error) {

	switch format {
	case FormatTarGzip:
		return walkTarGzip(ctx, src, intents, lim)
	case FormatZip:
		return walkZip(ctx, src, intents, lim)
	default:
		return nil, ErrUnknownFormat
	}
}

// FetchAndWalk is the package-level convenience equivalent of
// (&Fetcher{}).FetchAndWalk: HTTP-GET the URL using a default
// fetcher (DefaultFetchTimeout, default User-Agent), stream the
// body into Walk in one pass. Callers needing a custom HTTP client
// (proxy, custom UA, longer timeout for known-large artifacts) can
// instantiate Fetcher directly via NewFetcher.
func FetchAndWalk(ctx context.Context, url string, format Format,
	intents []CaptureIntent, lim Limits) (*Manifest, error) {

	return defaultFetcher.FetchAndWalk(ctx, url, format, intents, lim)
}
