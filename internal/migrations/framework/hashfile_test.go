package framework

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"lukechampine.com/blake3"
)

// TestHashFileBLAKE3_AcrossBufferBoundary mirrors the store-package
// check for framework's no-cache fallback hasher: a file larger than
// the 1 MiB io.CopyBuffer window must hash identically to a single-shot
// BLAKE3 over the same bytes.
func TestHashFileBLAKE3_AcrossBufferBoundary(t *testing.T) {
	dir := t.TempDir()
	data := make([]byte, 2*(1<<20)+999) // > 1 MiB, non-aligned tail
	for i := range data {
		data[i] = byte((i*97 + 13) & 0xff)
	}
	p := filepath.Join(dir, "migration.sql")
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
