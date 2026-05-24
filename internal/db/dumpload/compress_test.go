package dumpload

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// TestSniffFormatRecognisesAllSupported writes a tiny dump through
// each compressor, then asserts SniffFormat picks the right Format
// AND that Decompress recovers the original bytes verbatim. The
// payload includes binary bytes so we'd catch a decoder that
// truncates at NULs.
func TestSniffFormatRecognisesAllSupported(t *testing.T) {
	payload := []byte("INSERT INTO t VALUES (1, 'two', 3);\n\x00\xffend\n")

	cases := []struct {
		name    string
		want    Format
		compress func(t *testing.T, in []byte) []byte
	}{
		{"none", FormatNone, func(t *testing.T, in []byte) []byte { return in }},
		{"gzip", FormatGzip, func(t *testing.T, in []byte) []byte {
			var b bytes.Buffer
			w := gzip.NewWriter(&b)
			if _, err := w.Write(in); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			return b.Bytes()
		}},
		{"zstd", FormatZstd, func(t *testing.T, in []byte) []byte {
			var b bytes.Buffer
			w, err := zstd.NewWriter(&b)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(in); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			return b.Bytes()
		}},
		{"xz", FormatXz, func(t *testing.T, in []byte) []byte {
			var b bytes.Buffer
			w, err := xz.NewWriter(&b)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(in); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			return b.Bytes()
		}},
		// bzip2 is stdlib-decompress-only; produce a fixture via a
		// pre-known short header instead.
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := c.compress(t, payload)
			got, r, err := SniffFormat(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("sniff: %v", err)
			}
			if got != c.want {
				t.Errorf("Format=%v want %v", got, c.want)
			}
			dec, err := Decompress(r, got)
			if err != nil {
				t.Fatalf("decompress: %v", err)
			}
			out, err := io.ReadAll(dec)
			dec.Close()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !bytes.Equal(out, payload) {
				t.Errorf("roundtrip mismatch:\nwant %q\ngot  %q", payload, out)
			}
		})
	}
}

// TestSniffFormatBzip2 round-trips a pre-baked bzip2 fixture (stdlib
// has no bzip2 writer; fixture is `printf 'hi' | bzip2 -c`).
func TestSniffFormatBzip2(t *testing.T) {
	fixture := []byte{
		0x42, 0x5a, 0x68, 0x39, 0x31, 0x41, 0x59, 0x26, 0x53, 0x59,
		0x9a, 0x89, 0xb4, 0x22, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00,
		0x60, 0x20, 0x00, 0x21, 0x00, 0x82, 0xb1, 0x77, 0x24, 0x53,
		0x85, 0x09, 0x09, 0xa8, 0x9b, 0x42, 0x20,
	}
	got, r, err := SniffFormat(bytes.NewReader(fixture))
	if err != nil {
		t.Fatalf("sniff: %v", err)
	}
	if got != FormatBzip2 {
		t.Errorf("Format=%v want %v", got, FormatBzip2)
	}
	dec, err := Decompress(r, got)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	defer dec.Close()
	out, err := io.ReadAll(dec)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "hi" {
		t.Errorf("bzip2 roundtrip: %q want %q", out, "hi")
	}
}

// TestOpenDumpEndToEnd writes a gzipped fixture to disk and confirms
// OpenDump returns the decompressed content.
func TestOpenDumpEndToEnd(t *testing.T) {
	dir := t.TempDir()
	body := []byte("SELECT 1;\nSELECT 2;\n")
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(body)
	_ = w.Close()
	p := filepath.Join(dir, "fixture.sql.gz")
	if err := os.WriteFile(p, gz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	rc, f, err := OpenDump(p)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if f != FormatGzip {
		t.Errorf("format=%v want gzip", f)
	}
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, body) {
		t.Errorf("roundtrip mismatch: %q vs %q", out, body)
	}
}
