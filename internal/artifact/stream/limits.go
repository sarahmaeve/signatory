package stream

// resolveLimits returns DefaultLimits values for any zero-valued
// field of the input Limits. Lets callers override one cap without
// having to re-state all four. Used by every format walker.
//
// The walker MUST call this before enforcing any cap — otherwise a
// caller passing Limits{} would get a zero MaxTotalBytes and either
// fail immediately on the first byte or (worse, depending on
// implementation) get no protection at all.
func resolveLimits(lim Limits) Limits {
	out := lim
	if out.MaxTotalBytes == 0 {
		out.MaxTotalBytes = DefaultLimits.MaxTotalBytes
	}
	if out.MaxEntryBytes == 0 {
		out.MaxEntryBytes = DefaultLimits.MaxEntryBytes
	}
	if out.MaxEntries == 0 {
		out.MaxEntries = DefaultLimits.MaxEntries
	}
	if out.MaxCompressionRatio == 0 {
		out.MaxCompressionRatio = DefaultLimits.MaxCompressionRatio
	}
	if out.MaxCompressedBytes == 0 {
		out.MaxCompressedBytes = DefaultLimits.MaxCompressedBytes
	}
	return out
}
