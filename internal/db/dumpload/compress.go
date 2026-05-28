// compression.go provides transparent decompression of dump files.
// The format is sniffed from magic bytes (not the file extension)
// so a `.sql` file that happens to be gzipped still decompresses.
package dumpload

import (
	"bufio"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// Format names the compression detected on a dump file.
type Format int

const (
	FormatNone Format = iota
	FormatGzip
	FormatZstd
	FormatBzip2
	FormatXz
)

func (f Format) String() string {
	switch f {
	case FormatGzip:
		return "gzip"
	case FormatZstd:
		return "zstd"
	case FormatBzip2:
		return "bzip2"
	case FormatXz:
		return "xz"
	}
	return "none"
}

// SniffFormat peeks at the first few bytes of r to identify the
// compression format. The returned reader has the sniffed bytes
// pushed back so it can be consumed from the start.
func SniffFormat(r io.Reader) (Format, io.Reader, error) {
	br := bufio.NewReaderSize(r, 1<<16)
	head, err := br.Peek(6)
	if err != nil && err != io.EOF {
		return FormatNone, br, err
	}
	switch {
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		return FormatGzip, br, nil
	case len(head) >= 4 && head[0] == 0x28 && head[1] == 0xb5 && head[2] == 0x2f && head[3] == 0xfd:
		return FormatZstd, br, nil
	case len(head) >= 3 && head[0] == 'B' && head[1] == 'Z' && head[2] == 'h':
		return FormatBzip2, br, nil
	case len(head) >= 6 && head[0] == 0xfd && head[1] == '7' && head[2] == 'z' && head[3] == 'X' && head[4] == 'Z' && head[5] == 0x00:
		return FormatXz, br, nil
	}
	return FormatNone, br, nil
}

// Decompress wraps r in a decoder appropriate for f. The returned
// ReadCloser must always be closed by the caller, including for
// FormatNone (no-op closer).
func Decompress(r io.Reader, f Format) (io.ReadCloser, error) {
	switch f {
	case FormatNone:
		return io.NopCloser(r), nil
	case FormatGzip:
		return gzip.NewReader(r)
	case FormatZstd:
		zd, err := zstd.NewReader(r)
		if err != nil {
			return nil, err
		}
		return zd.IOReadCloser(), nil
	case FormatBzip2:
		return io.NopCloser(bzip2.NewReader(r)), nil
	case FormatXz:
		xr, err := xz.NewReader(r)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(xr), nil
	}
	return nil, fmt.Errorf("unsupported compression: %s", f)
}

// OpenDump opens path, detects compression by magic bytes, and
// returns a transparently-decompressed reader. The detected format
// is returned so callers can log it.
func OpenDump(path string) (io.ReadCloser, Format, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, FormatNone, fmt.Errorf("open dump %s: %w", path, err)
	}
	format, sniffed, err := SniffFormat(f)
	if err != nil {
		_ = f.Close()
		return nil, FormatNone, fmt.Errorf("sniff %s: %w", path, err)
	}
	dec, err := Decompress(sniffed, format)
	if err != nil {
		_ = f.Close()
		return nil, FormatNone, fmt.Errorf("decompress %s as %s: %w", path, format, err)
	}
	return &fileCloser{rc: dec, f: f}, format, nil
}

// fileCloser bundles a decompressor's ReadCloser with the
// underlying *os.File so Close shuts down both.
type fileCloser struct {
	rc io.ReadCloser
	f  *os.File
}

func (c *fileCloser) Read(p []byte) (int, error) { return c.rc.Read(p) }
func (c *fileCloser) Close() error {
	_ = c.rc.Close()
	return c.f.Close()
}
