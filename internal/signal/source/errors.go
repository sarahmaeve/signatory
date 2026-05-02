package source

import "errors"

// ErrPinTableNotAvailable is returned by VersionPinSource when neither
// the in-run CollectionResult nor the signal store contains a
// version_pin_table for the requested entity. The source-evolution
// collector treats this as a graceful absence (records reason and
// returns) rather than a hard failure.
var ErrPinTableNotAvailable = errors.New("version_pin_table signal not available in run or store")

// ErrNoClone is returned by NewBlobStreamer when the clonePath
// argument is empty or refers to a directory that isn't a git
// working tree. The source-evolution collector treats this as a
// configuration issue (record absence with reason "local clone
// required").
var ErrNoClone = errors.New("local git clone path required")

// ErrSHAMissingFromClone is returned by BlobStreamer when the
// requested SHA isn't present in the local clone's object DB. With
// --no-fetch (the default), this is the signal that the proxy has
// recorded a SHA that --refresh did not fetch — itself diagnostic.
// With --allow-fetch, callers may catch this and retry after a
// targeted git fetch.
var ErrSHAMissingFromClone = errors.New("sha missing from local clone")

// ErrBlobStreamerClosed is returned by BlobStreamer methods called
// after Close. Re-using a closed streamer is a programming error;
// the sentinel makes the failure mode explicit rather than yielding
// a generic "broken pipe" from the closed stdin.
var ErrBlobStreamerClosed = errors.New("blob streamer closed")

// ErrBlobSizeExceedsCap is returned by BlobStreamer.ReadBlob when
// the size field in cat-file's response header exceeds the configured
// per-blob cap (default 10 MiB; override via WithMaxBlobSize). The
// cap is a defensive bound on allocation: signatory does not own
// git's object DB, and a tampered or corrupt loose object claiming
// e.g. size = MaxInt32 in its header would otherwise drive an
// unbounded make([]byte, size) before any content was read. Other
// HTTP clients in this codebase (github, gopublish, npm, pypi) all
// apply a 10 MiB io.LimitReader at their fetch boundary; this
// sentinel is the cat-file-pipe equivalent.
var ErrBlobSizeExceedsCap = errors.New("blob size exceeds configured cap")
