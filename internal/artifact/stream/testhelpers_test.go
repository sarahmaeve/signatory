package stream

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// targzBuilder helps tests construct tar.gz fixtures inline. Every
// production walker test in this package builds its archive from
// known-shape entries rather than checking a binary blob into the
// repo — fixtures stay legible at the test site, and the failure
// mode of "the helper is wrong" is the same as "the test is wrong"
// (both fixable in one place).
type targzBuilder struct {
	t  *testing.T
	gz *gzip.Writer
	tw *tar.Writer
	bb *bytes.Buffer
}

// newTarGz returns a builder writing into a fresh bytes.Buffer.
func newTarGz(t *testing.T) *targzBuilder {
	t.Helper()
	bb := &bytes.Buffer{}
	gz := gzip.NewWriter(bb)
	tw := tar.NewWriter(gz)
	return &targzBuilder{t: t, gz: gz, tw: tw, bb: bb}
}

// addFile writes a regular-file entry with the given path and body.
func (b *targzBuilder) addFile(path string, body []byte) *targzBuilder {
	b.t.Helper()
	require.NoError(b.t, b.tw.WriteHeader(&tar.Header{
		Name:     path,
		Size:     int64(len(body)),
		Mode:     0o644,
		Typeflag: tar.TypeReg,
	}))
	if len(body) > 0 {
		_, err := b.tw.Write(body)
		require.NoError(b.t, err)
	}
	return b
}

// addSymlink writes a symbolic link entry.
func (b *targzBuilder) addSymlink(path, target string) *targzBuilder {
	b.t.Helper()
	require.NoError(b.t, b.tw.WriteHeader(&tar.Header{
		Name:     path,
		Mode:     0o777,
		Typeflag: tar.TypeSymlink,
		Linkname: target,
	}))
	return b
}

// addHardlink writes a hard link entry.
func (b *targzBuilder) addHardlink(path, target string) *targzBuilder {
	b.t.Helper()
	require.NoError(b.t, b.tw.WriteHeader(&tar.Header{
		Name:     path,
		Mode:     0o644,
		Typeflag: tar.TypeLink,
		Linkname: target,
	}))
	return b
}

// addOther writes an entry with a non-standard typeflag (fifo, char
// device, etc.). Used to verify the walker classifies these as
// EntryOther rather than panicking or dropping them.
func (b *targzBuilder) addOther(path string, typeflag byte) *targzBuilder {
	b.t.Helper()
	require.NoError(b.t, b.tw.WriteHeader(&tar.Header{
		Name:     path,
		Mode:     0o644,
		Typeflag: typeflag,
	}))
	return b
}

// bytes finalizes the archive and returns its compressed bytes.
// Closes the inner tar.Writer and outer gzip.Writer in order.
func (b *targzBuilder) bytes() []byte {
	b.t.Helper()
	require.NoError(b.t, b.tw.Close())
	require.NoError(b.t, b.gz.Close())
	return b.bb.Bytes()
}

// reader returns the finalized archive as a fresh bytes.Reader, the
// shape Walk wants. Multiple calls reuse the same buffer since the
// builder is a one-shot.
func (b *targzBuilder) reader() *bytes.Reader {
	return bytes.NewReader(b.bytes())
}

// ----- zip builder ---------------------------------------------------

// zipBuilder mirrors targzBuilder for zip archives. Same one-shot
// semantics: build entries, then call bytes()/reader() to finalize.
type zipBuilder struct {
	t  *testing.T
	zw *zip.Writer
	bb *bytes.Buffer
}

// newZip returns a builder writing into a fresh bytes.Buffer.
func newZip(t *testing.T) *zipBuilder {
	t.Helper()
	bb := &bytes.Buffer{}
	return &zipBuilder{t: t, zw: zip.NewWriter(bb), bb: bb}
}

// addFile writes a regular-file zip entry.
func (b *zipBuilder) addFile(path string, body []byte) *zipBuilder {
	b.t.Helper()
	w, err := b.zw.Create(path)
	require.NoError(b.t, err)
	if len(body) > 0 {
		_, err := w.Write(body)
		require.NoError(b.t, err)
	}
	return b
}

// addSymlink writes a symbolic link entry. Zip encodes symlinks via
// Unix mode bits (S_IFLNK) on the entry header; the link target is
// the file body. We construct one manually because stdlib's
// zip.Writer.Create doesn't expose a "make this a symlink" knob.
//
// SetMode takes Go's portable os.FileMode, not raw Unix mode bits
// — stdlib zip translates os.ModeSymlink into S_IFLNK in the
// ExternalAttrs field on the way out, and back again on the way in
// via FileHeader.Mode().
func (b *zipBuilder) addSymlink(path, target string) *zipBuilder {
	b.t.Helper()
	hdr := &zip.FileHeader{
		Name:   path,
		Method: zip.Deflate,
	}
	hdr.SetMode(os.ModeSymlink | 0o777)

	w, err := b.zw.CreateHeader(hdr)
	require.NoError(b.t, err)
	_, err = w.Write([]byte(target))
	require.NoError(b.t, err)
	return b
}

// bytes finalizes and returns the compressed archive.
func (b *zipBuilder) bytes() []byte {
	b.t.Helper()
	require.NoError(b.t, b.zw.Close())
	return b.bb.Bytes()
}

// reader returns the finalized archive as a *bytes.Reader.
func (b *zipBuilder) reader() *bytes.Reader {
	return bytes.NewReader(b.bytes())
}

// markFirstLocalHeaderEncrypted toggles bit 0 of the general-purpose
// flag in the FIRST local file header of the supplied zip bytes,
// returning a new slice. We can't construct an actual encrypted
// entry via stdlib (no API), but flipping the encryption-flag bit
// is exactly what the walker checks against: by spec, an entry with
// bit 0 set IS encrypted, and the walker must reject regardless of
// whether it could decrypt.
//
// Bit 0 of the general-purpose flag lives at byte offset 6 in the
// 30-byte fixed local-header preamble (see PKZIP appnote). Local
// header signature is 0x04034b50 little-endian.
func markFirstLocalHeaderEncrypted(t *testing.T, raw []byte) []byte {
	t.Helper()
	const sigLocal = uint32(0x04034b50)
	const flagOffset = 6
	out := append([]byte(nil), raw...)
	for i := 0; i+30 <= len(out); i++ {
		if binary.LittleEndian.Uint32(out[i:i+4]) == sigLocal {
			out[i+flagOffset] |= 0x01
			return out
		}
	}
	t.Fatalf("no local file header signature found in zip; fixture broken")
	return nil
}

// injectFakeLocalHeader inserts a synthetic local file header at the
// position immediately before the central directory, then rewrites
// the End-of-Central-Directory record's central-dir offset to skip
// the injected bytes. The resulting zip parses cleanly via stdlib
// (central directory still describes the original N entries) but a
// local-header scan finds N+1 entries — exactly the parser-confusion
// shape the walker must detect.
//
// EOCD record format (last 22 bytes of any zip without comment):
//
//	0x06054b50 signature, 4 + 4 + 4 + 4 + 4 bytes of disk/entry/size
//	fields, then 4 bytes "offset of central directory", then 2 bytes
//	"comment length".
//
// Local file header (synthetic, for a zero-byte file named fakeName):
//
//	0x04034b50 signature, 26 fixed bytes (version, flags, method,
//	time/date, CRC=0, compressed=0, uncompressed=0), uint16 filename
//	length, uint16 extra length, then filename bytes.
func injectFakeLocalHeader(t *testing.T, raw []byte, fakeName string) []byte {
	t.Helper()
	const sigEOCD = uint32(0x06054b50)

	eocdAt := -1
	for i := len(raw) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(raw[i:i+4]) == sigEOCD {
			eocdAt = i
			break
		}
	}
	if eocdAt < 0 {
		t.Fatalf("no EOCD signature found in zip fixture")
	}
	cdOffset := binary.LittleEndian.Uint32(raw[eocdAt+16 : eocdAt+20])

	nameBytes := []byte(fakeName)
	require.LessOrEqual(t, len(nameBytes), 0xFFFF,
		"fakeName length must fit in uint16 zip filename-length field")

	fake := make([]byte, 30+len(nameBytes))
	binary.LittleEndian.PutUint32(fake[0:4], 0x04034b50) // local header sig
	binary.LittleEndian.PutUint16(fake[4:6], 20)         // version needed
	// flags, method, time, date, crc, compressed-size, uncompressed-size
	// are all already zero from make().
	binary.LittleEndian.PutUint16(fake[26:28], uint16(len(nameBytes)))
	binary.LittleEndian.PutUint16(fake[28:30], 0) // extra field length
	copy(fake[30:], nameBytes)

	out := make([]byte, 0, len(raw)+len(fake))
	out = append(out, raw[:cdOffset]...)
	out = append(out, fake...)
	out = append(out, raw[cdOffset:]...)

	// Both cdOffset (already uint32) and len(fake) (bounded by the
	// require above + the fixed 30-byte preamble) fit in uint32; the
	// sum cannot overflow for any reasonable test fixture.
	require.LessOrEqual(t, int64(cdOffset)+int64(len(fake)), int64(0xFFFFFFFF),
		"injected zip exceeds uint32 central-dir offset range")
	newCdOffset := cdOffset + uint32(len(fake))
	newEocdAt := eocdAt + len(fake)
	binary.LittleEndian.PutUint32(out[newEocdAt+16:newEocdAt+20], newCdOffset)
	return out
}
