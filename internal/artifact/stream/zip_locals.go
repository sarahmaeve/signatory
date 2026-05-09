package stream

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// localEntry is a single record from a manual local-file-header
// scan. Used to cross-check stdlib's central-directory view for
// parser-confusion attacks (CVE-style mismatches where local
// headers and central directory disagree).
type localEntry struct {
	name  string
	flags uint16 // general-purpose bit flag; bit 0 = encrypted
}

// scanLocalHeaders walks raw bytes from the start, parsing local
// file headers (signature 0x04034b50) until it hits the central
// directory signature (0x02014b50). Returns the per-entry name +
// general-purpose flags so the walker can both cross-check counts
// against the central directory AND detect encryption bits that
// might be set only on the local-header side.
//
// Two body-length cases:
//
//  1. Sizes in the local header (the simple case): compressed size
//     is non-zero and not the ZIP64 marker; advance past the body
//     by the declared length.
//
//  2. Data descriptors (general-purpose flag bit 3): compressed
//     size in the local header is zero, real sizes live in a
//     trailing data-descriptor record (12 or 16 bytes after body).
//     Modern writers — including Go's stdlib zip — include the
//     optional descriptor signature 0x08074b50 which makes
//     scan-forward reliable. We scan from the body start for that
//     signature, then advance 16 bytes past it (sig + crc + 2
//     sizes). Encountering a local or central signature first
//     means the descriptor lacked its optional signature, which
//     we can't safely disambiguate from "the body happened to
//     contain those bytes" — return errCrossCheckUnscanable in
//     that case so the walker skips cross-check rather than
//     misreport.
//
// ZIP64 limitation (deferred): entries with compressed size
// 0xFFFFFFFF carry the real size in an "extra" field. We surface
// errCrossCheckUnscanable and the walker skips the check.
func scanLocalHeaders(raw []byte) ([]localEntry, error) {
	const (
		sigLocal             = uint32(0x04034b50)
		sigCentral           = uint32(0x02014b50)
		sigDataDescriptor    = uint32(0x08074b50)
		zip64SizeMarker      = uint32(0xFFFFFFFF)
		gpFlagDataDescriptor = uint16(0x0008)
		// Data descriptor with optional signature: sig (4) + crc (4)
		// + compressed (4) + uncompressed (4) = 16 bytes. The 12-byte
		// signature-less variant is unhandled here — see doc comment.
		dataDescriptorWithSig = 16
	)

	var entries []localEntry
	i := 0
	for i+30 <= len(raw) {
		sig := binary.LittleEndian.Uint32(raw[i : i+4])
		if sig == sigCentral {
			return entries, nil
		}
		if sig != sigLocal {
			return nil, fmt.Errorf("stream: expected local-header signature at offset %d, got 0x%08x", i, sig)
		}

		flags := binary.LittleEndian.Uint16(raw[i+6 : i+8])
		compressedSize := binary.LittleEndian.Uint32(raw[i+18 : i+22])
		nameLen := binary.LittleEndian.Uint16(raw[i+26 : i+28])
		extraLen := binary.LittleEndian.Uint16(raw[i+28 : i+30])

		nameStart := i + 30
		nameEnd := nameStart + int(nameLen)
		if nameEnd > len(raw) {
			return nil, fmt.Errorf("stream: local header at %d: filename extends past buffer", i)
		}
		entries = append(entries, localEntry{
			name:  string(raw[nameStart:nameEnd]),
			flags: flags,
		})

		bodyStart := nameEnd + int(extraLen)

		if compressedSize == zip64SizeMarker {
			return entries, errCrossCheckUnscanable
		}

		if flags&gpFlagDataDescriptor != 0 {
			next, ok := advancePastDataDescriptor(raw, bodyStart, sigDataDescriptor, sigLocal, sigCentral)
			if !ok {
				return entries, errCrossCheckUnscanable
			}
			i = next + dataDescriptorWithSig
			continue
		}

		i = bodyStart + int(compressedSize)
	}
	return entries, nil
}

// advancePastDataDescriptor scans raw forward from bodyStart for the
// optional data-descriptor signature. Returns (offset of descriptor
// signature, true) on success. If a local or central signature is
// hit first, returns (_, false) — the descriptor is signature-less
// and we can't disambiguate from body bytes that happen to match a
// header signature.
func advancePastDataDescriptor(raw []byte, bodyStart int, sigDD, sigL, sigC uint32) (int, bool) {
	for j := bodyStart; j+4 <= len(raw); j++ {
		sig := binary.LittleEndian.Uint32(raw[j : j+4])
		switch sig {
		case sigDD:
			return j, true
		case sigL, sigC:
			return 0, false
		}
	}
	return 0, false
}

// errCrossCheckUnscanable signals that scanLocalHeaders cannot
// reliably advance through the archive (ZIP64 or data-descriptor
// territory). Sentinel-only — the caller responds by skipping the
// local-vs-central cross-check, not by failing the walk.
var errCrossCheckUnscanable = errors.New("stream: local-header scanner cannot advance (zip64 or data descriptor)")
