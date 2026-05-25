package prepare

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/store"
)

// TestInspectFingerprint_DerivesPerInputHashes builds a tiny synthetic
// worktree with one migration file + one lockfile and asserts the
// report carries both as per-input hashes and computes a stable
// fingerprint. Regression guard: if computeSnapshotKey starts using
// keys other than the input glob, this test catches it.
func TestInspectFingerprint_DerivesPerInputHashes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// migration file
	mig := filepath.Join(dir, "db", "migrations", "0001_init.sql")
	_ = os.MkdirAll(filepath.Dir(mig), 0o755)
	_ = os.WriteFile(mig, []byte("CREATE TABLE x(id INT);"), 0o644)
	// lockfile
	lock := filepath.Join(dir, "composer.lock")
	_ = os.WriteFile(lock, []byte("{\"version\":\"1\"}"), 0o644)

	d := config.DatabaseConfig{
		Engine:       "mysql",
		NameTemplate: "app_{slug}",
		Inputs: []config.Input{
			{Glob: "db/migrations/*.sql", Hash: "filename"},
			{Glob: "composer.lock"}, // default checksum
		},
		Migrate: &config.Step{Run: "echo migrate"},
	}

	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	rep := InspectFingerprint(ctx, s, d, dir, "app_test", "8.0.36")

	if rep.Fingerprint == "" {
		t.Fatal("fingerprint not set")
	}
	if rep.TemplateName == "" {
		t.Errorf("template_name not derived")
	}
	if _, ok := rep.InputHashes["db/migrations/*.sql"]; !ok {
		t.Errorf("missing migration input hash: %#v", rep.InputHashes)
	}
	if _, ok := rep.InputHashes["composer.lock"]; !ok {
		t.Errorf("missing lockfile hash: %#v", rep.InputHashes)
	}
	if rep.CommandsHash == "" {
		t.Errorf("commands_hash not surfaced (migrate.run set but hash empty)")
	}
	if rep.CacheHitAvailable {
		t.Errorf("no snapshot recorded yet — cache_hit_available should be false")
	}
}

// TestInspectFingerprint_DetectsCachedSnapshot — record a snapshot
// against the same fingerprint, then verify InspectFingerprint
// reports CacheHitAvailable=true on a second call with the same
// inputs.
func TestInspectFingerprint_DetectsCachedSnapshot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d := config.DatabaseConfig{
		Engine:       "mysql",
		NameTemplate: "x",
		Migrate:      &config.Step{Run: "echo m"},
	}
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	r1 := InspectFingerprint(ctx, s, d, dir, "x", "8.0")
	if r1.CacheHitAvailable {
		t.Fatal("no snapshot recorded yet")
	}

	// Insert a snapshot with the same fingerprint we just computed.
	if err := s.RecordSnapshot(ctx, store.SnapshotRecord{
		Fingerprint:    r1.Fingerprint,
		Engine:         "mysql",
		EngineVersion:  "8.0",
		SourceDB:       "x",
		TemplateName:   r1.TemplateName,
		MigrationsHash: "n/a",
	}); err != nil {
		t.Fatal(err)
	}

	r2 := InspectFingerprint(ctx, s, d, dir, "x", "8.0")
	if !r2.CacheHitAvailable {
		t.Errorf("expected cache_hit_available=true after RecordSnapshot")
	}
	if r2.CachedSnapshot == nil || r2.CachedSnapshot.Fingerprint != r1.Fingerprint {
		t.Errorf("cached_snapshot not surfaced: %#v", r2.CachedSnapshot)
	}
}

// TestInspectFingerprint_VersionChangesFlipFingerprint — same inputs
// but different engine version → different fingerprint. The version
// is mixed into the hash; this verifies that contract.
func TestInspectFingerprint_VersionChangesFlipFingerprint(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d := config.DatabaseConfig{Engine: "mysql", NameTemplate: "x"}
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	a := InspectFingerprint(ctx, s, d, dir, "x", "8.0.0")
	b := InspectFingerprint(ctx, s, d, dir, "x", "8.0.1")
	if a.Fingerprint == b.Fingerprint {
		t.Errorf("fingerprint should differ between engine versions; both = %q", a.Fingerprint)
	}
}
