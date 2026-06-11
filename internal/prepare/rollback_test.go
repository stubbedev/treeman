package prepare

import (
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/snapshot"
	"github.com/stubbedev/treeman/internal/store"
)

// TestCommandsHashIgnoresRollback pins the decision that adding/changing
// a `rollback:` block does NOT alter the template fingerprint: rollback
// affects only the build transformation, not the built content, and
// folding it in would break rollback-ancestor matching the first time a
// user adds the block (every prior template would carry a different
// commands hash).
func TestCommandsHashIgnoresRollback(t *testing.T) {
	base := config.DatabaseConfig{
		Engine:  "mysql",
		Migrate: &config.Step{Run: "php artisan migrate --force"},
		Seed:    &config.Step{Run: "php artisan db:seed"},
	}
	withRollback := base
	withRollback.Rollback = &config.Step{Run: "php artisan migrate:rollback --step=$TREEMAN_ROLLBACK_STEPS"}

	if commandsHash(base) != commandsHash(withRollback) {
		t.Error("adding a rollback block must not change commandsHash")
	}

	// Sanity: a different migrate command MUST change the hash.
	diffMigrate := base
	diffMigrate.Migrate = &config.Step{Run: "php artisan migrate"}
	if commandsHash(base) == commandsHash(diffMigrate) {
		t.Error("different migrate command should change commandsHash")
	}
}

// TestDumpOnlySnapshotKey verifies the dump-only key is stable, varies
// with dumpHash, carries the marker, and is distinct from a real
// (commands-bearing) template key for the same dump.
func TestDumpOnlySnapshotKey(t *testing.T) {
	k1 := dumpOnlySnapshotKey("mysql", "8.0", "dumpA")
	k2 := dumpOnlySnapshotKey("mysql", "8.0", "dumpA")
	if k1.Fingerprint() != k2.Fingerprint() {
		t.Error("dump-only key not stable for identical inputs")
	}
	if k1.LockfileHashes[store.DumpOnlyMarkerKey] != "1" {
		t.Error("dump-only key missing marker")
	}
	if k1.Fingerprint() == dumpOnlySnapshotKey("mysql", "8.0", "dumpB").Fingerprint() {
		t.Error("dump-only key must vary with dumpHash")
	}
	// A real template for the same dump but with a commands hash must
	// not collide with the dump-only template.
	realKey := snapshot.New("mysql", "8.0", "", "", "dumpA",
		map[string]string{store.CommandsHashKey: "ch"})
	if k1.Fingerprint() == realKey.Fingerprint() {
		t.Error("dump-only key collides with a real template key")
	}
}
