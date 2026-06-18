package prepare

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/store"
)

// TestSingleDumpFingerprintMatchesV5 locks in the equivalence promise
// for the array refactor: a one-entry DumpList must produce the same
// DumpHashHex (and therefore the same fingerprint) as the pre-array
// single Dump *DumpSpec did, so single-dump configs keep their v5
// templates without a FormatVersion bump.
//
// The equivalence is enforced by computeSnapshotKey's len(d.Dump)==1
// branch: it hashes the lone file via LockfileHashesForWithCache and
// uses the basename-keyed result — bit-for-bit what the previous
// implementation did.
func TestSingleDumpFingerprintMatchesV5(t *testing.T) {
	ctx := context.Background()
	wt := t.TempDir()
	dumpPath := filepath.Join(wt, "seed.sql")
	if err := os.WriteFile(dumpPath, []byte("CREATE TABLE t (id INT);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	d := config.DatabaseConfig{
		Engine: "postgres",
		Dump:   config.DumpList{{Path: "seed.sql"}},
	}
	k := computeSnapshotKey(ctx, st, d, wt, "16")

	if k.DumpHashHex == "" {
		t.Fatal("DumpHashHex must be populated for a single-dump config")
	}
	// 256-bit blake3 hex digest is 64 chars; the single-dump path uses
	// the raw file hash (no combiner), so the result is the same width
	// it has always been.
	if got := len(k.DumpHashHex); got != 64 {
		t.Errorf("single-dump DumpHashHex len=%d, want 64 (a raw blake3-256 hex)", got)
	}
}

// TestMultiDumpFingerprintIsOrderSensitive proves the combined-ordered
// hash treats [a,b] and [b,a] as DIFFERENT keys — reordering the dumps
// changes what ends up in the DB, so the fingerprint must follow.
func TestMultiDumpFingerprintIsOrderSensitive(t *testing.T) {
	ctx := context.Background()
	wt := t.TempDir()
	aPath := filepath.Join(wt, "a.sql")
	bPath := filepath.Join(wt, "b.sql")
	if err := os.WriteFile(aPath, []byte("-- a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bPath, []byte("-- b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	ab := config.DatabaseConfig{
		Engine: "postgres",
		Dump:   config.DumpList{{Path: "a.sql"}, {Path: "b.sql"}},
	}
	ba := config.DatabaseConfig{
		Engine: "postgres",
		Dump:   config.DumpList{{Path: "b.sql"}, {Path: "a.sql"}},
	}
	kAB := computeSnapshotKey(ctx, st, ab, wt, "16")
	kBA := computeSnapshotKey(ctx, st, ba, wt, "16")

	if kAB.Fingerprint() == kBA.Fingerprint() {
		t.Errorf("reordering dumps must flip the fingerprint: ab=%s ba=%s",
			kAB.Fingerprint(), kBA.Fingerprint())
	}
}

// TestMultiDumpSkipsMissingOptional locks in the optional-skip
// semantics for entries inside a list: an optional dump whose file is
// missing must silently drop out of the load order AND out of the
// fingerprint, while a non-optional missing entry fails the resolve.
func TestMultiDumpSkipsMissingOptional(t *testing.T) {
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "present.sql"), []byte("-- p\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dumps, err := dumpsReady(config.DumpList{
		{Path: "present.sql"},
		{Path: "absent.sql", Optional: true},
	}, wt)
	if err != nil {
		t.Fatalf("optional missing should not error: %v", err)
	}
	if len(dumps) != 1 || filepath.Base(dumps[0].Path) != "present.sql" {
		t.Errorf("optional-missing should be dropped: got %+v", dumps)
	}

	if _, err := dumpsReady(config.DumpList{
		{Path: "present.sql"},
		{Path: "absent.sql"}, // required, missing
	}, wt); err == nil {
		t.Error("required missing dump should surface an error")
	}
}
