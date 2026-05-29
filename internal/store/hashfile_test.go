package store

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"lukechampine.com/blake3"
)

// TestHashFileBLAKE3_AcrossBufferBoundary hashes a file several times
// larger than the 1 MiB io.CopyBuffer window and asserts the digest
// equals an independent single-shot BLAKE3 over the same bytes. This
// pins the buffered-copy change: a wrong buffer size or a partial-read
// bug would surface as a digest mismatch here.
func TestHashFileBLAKE3_AcrossBufferBoundary(t *testing.T) {
	dir := t.TempDir()
	// 3 MiB + a non-aligned tail so the final Read is a partial buffer.
	data := make([]byte, 3*(1<<20)+1237)
	for i := range data {
		data[i] = byte(i*131 + 7) //nolint:gosec // intentional truncation to fill a test buffer
	}
	p := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := hashFileBLAKE3(p)
	if err != nil {
		t.Fatalf("hashFileBLAKE3: %v", err)
	}

	h := blake3.New(32, nil)
	if _, err := h.Write(data); err != nil {
		t.Fatal(err)
	}
	want := hex.EncodeToString(h.Sum(nil))

	if got != want {
		t.Fatalf("digest mismatch across buffer boundary:\n got  %s\n want %s", got, want)
	}
}

// TestHashFileBLAKE3_Empty guards the zero-length edge: an empty file
// must hash to BLAKE3 of no bytes, not error.
func TestHashFileBLAKE3_Empty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.bin")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := hashFileBLAKE3(p)
	if err != nil {
		t.Fatalf("hashFileBLAKE3: %v", err)
	}
	h := blake3.New(32, nil)
	want := hex.EncodeToString(h.Sum(nil))
	if got != want {
		t.Fatalf("empty-file digest mismatch:\n got  %s\n want %s", got, want)
	}
}
