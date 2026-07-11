//go:build e2e

// Package fanout_e2e exercises test_clones: building N parallel
// clone DBs from the cached template. Asserts:
//
//   - Each declared clone DB exists.
//   - Each clone has the same schema as the source.
//   - Clones are populated from the template (no fresh migrate).
package fanout_e2e

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
)

func TestTestClonesFanout(t *testing.T) {
	harness.SkipIfNoDocker(t)
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))

	harness.WaitForReady(t, "mysql:13356", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13356", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "seed.sql"),
		[]byte(`CREATE TABLE widgets (id INT PRIMARY KEY, name VARCHAR(64));
CREATE TABLE widget_audit (id INT PRIMARY KEY AUTO_INCREMENT, widget_id INT NOT NULL);
CREATE TRIGGER widgets_ai AFTER INSERT ON widgets FOR EACH ROW INSERT INTO widget_audit (widget_id) VALUES (NEW.id);
INSERT INTO widgets VALUES (1, 'one'), (2, 'two'), (3, 'three');`), 0o644); err != nil {
		t.Fatal(err)
	}

	const nClones = 4
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql: &config.MysqlConn{
				Host: "127.0.0.1", Port: 13356,
				User: "root", Password: "rootpw",
			},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "mysql",
				NameTemplate: "tm_fan_{slug}",
				Dump:         config.DumpList{{Path: "seed.sql"}},
				TestClones: &config.TestClonesSpec{
					Clones:       config.ClonesSetting{Fixed: nClones},
					NameTemplate: "tm_fan_{slug}_w{n}",
				},
			},
		},
	}
	env := harness.NewEnv(t, wt)
	outs := env.RunPrepare(t, cfg)
	o := harness.AssertOutcome(t, outs, "mysql", false)
	if len(o.Clones) != nClones {
		t.Fatalf("clone count = %d, want %d (clones=%v)", len(o.Clones), nClones, o.Clones)
	}
	t.Logf("source=%s template=%s clones=%v", o.SourceDB, o.TemplateName, o.Clones)

	sort.Strings(o.Clones)
	// Confirm each clone exists in MySQL and has the expected rows.
	for i, name := range o.Clones {
		db := openMySQL(t, name)
		defer db.Close()
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM widgets").Scan(&n); err != nil {
			t.Fatalf("clone %s: count widgets: %v", name, err)
		}
		if n != 3 {
			t.Errorf("clone %s: widgets rows = %d, want 3", name, n)
		}
		// Issue #20: the clone must carry the source's triggers, not
		// just its tables — trigger-dependent tests pass serially and
		// fail under --parallel when the worker clones drop them.
		// Assert both presence and that the trigger actually fires.
		var trg int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = DATABASE()").Scan(&trg); err != nil {
			t.Fatalf("clone %s: count triggers: %v", name, err)
		}
		if trg != 1 {
			t.Errorf("clone %s: trigger count = %d, want 1 (clone dropped triggers)", name, trg)
		}
		probeID := 100 + i
		if _, err := db.Exec("INSERT INTO widgets VALUES (?, 'probe')", probeID); err != nil {
			t.Fatalf("clone %s: insert probe row: %v", name, err)
		}
		var fired int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM widget_audit WHERE widget_id = ?", probeID).Scan(&fired); err != nil {
			t.Fatalf("clone %s: count audit rows: %v", name, err)
		}
		if fired != 1 {
			t.Errorf("clone %s: trigger did not fire (audit rows = %d, want 1)", name, fired)
		}
	}

	// Sanity: second pass — should be a cache hit with the same
	// clones already in place, fanout is idempotent.
	outs = env.RunPrepare(t, cfg)
	o2 := harness.AssertOutcome(t, outs, "mysql", true)
	if len(o2.Clones) != nClones {
		t.Errorf("pass2 clone count = %d, want %d", len(o2.Clones), nClones)
	}
}

func openMySQL(t *testing.T, dbName string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("root:rootpw@tcp(127.0.0.1:13356)/%s", dbName)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping clone %s: %v", dbName, err)
	}
	return db
}

var _ = context.Background
