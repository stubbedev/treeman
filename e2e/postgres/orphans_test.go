//go:build e2e

package postgres_e2e

import (
	"database/sql"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/snapshot"
)

// TestOrphanTemplateDetection covers the doctor's snapshots probe
// end-to-end: an engine-side `_tm_*` database with no snapshots row
// (crash between SnapshotCreate and RecordSnapshot) is detected as an
// orphan — together with its spare family — and DropOrphans reclaims
// it, while the LIVE template (row present) is never flagged.
func TestOrphanTemplateDetection(t *testing.T) {
	harness.SkipIfNoDocker(t)
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))

	harness.WaitForReady(t, "postgres:15432", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:15432", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	wt := t.TempDir()
	copyTree(t, "fixtures", filepath.Join(wt, "fixtures"))
	cfg := buildConfig()
	env := harness.NewEnv(t, wt)

	// Build a LIVE template (engine DB + SQLite row).
	outs := env.RunPrepare(t, cfg)
	o1 := harness.AssertOutcome(t, outs, "postgres", false)

	// Fake a crash leftover: engine-side template + a spare, no row.
	const orphan = "_tm_deadbeefdeadbeef"
	mustExecPG(t, fmt.Sprintf("CREATE DATABASE %q", orphan))
	mustExecPG(t, fmt.Sprintf("CREATE DATABASE %q", orphan+"_spare1"))
	t.Cleanup(func() {
		mustExecPG(t, fmt.Sprintf("DROP DATABASE IF EXISTS %q", orphan))
		mustExecPG(t, fmt.Sprintf("DROP DATABASE IF EXISTS %q", orphan+"_spare1"))
	})

	orphans, err := snapshot.FindOrphans(env.Ctx, cfg, env.Store)
	if err != nil {
		t.Fatalf("FindOrphans: %v", err)
	}
	if len(orphans) != 1 || orphans[0].Name != orphan {
		t.Fatalf("orphans = %+v, want exactly [%s] (live template %s must NOT be flagged)",
			orphans, orphan, o1.TemplateName)
	}

	dropped, errs := snapshot.DropOrphans(env.Ctx, cfg, orphans)
	if len(errs) > 0 || dropped != 1 {
		t.Fatalf("DropOrphans: dropped=%d errs=%v", dropped, errs)
	}
	for _, name := range []string{orphan, orphan + "_spare1"} {
		if pgDBExists(t, "127.0.0.1:15432", name) {
			t.Errorf("%s survived DropOrphans", name)
		}
	}
	if !pgDBExists(t, "127.0.0.1:15432", o1.TemplateName) {
		t.Errorf("live template %s was wrongly dropped", o1.TemplateName)
	}
}

// mustExecPG runs one statement against the e2e postgres as superuser.
func mustExecPG(t *testing.T, stmt string) {
	t.Helper()
	db, err := sql.Open("pgx", "postgres://postgres:pgpw@127.0.0.1:15432/postgres")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("exec %q: %v", stmt, err)
	}
}
