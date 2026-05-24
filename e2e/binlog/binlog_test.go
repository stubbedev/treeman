//go:build e2e

// Package binlog_e2e drives the MySQL binlog tailer:
//   1. Boot MySQL with binlog enabled (ROW format).
//   2. Connect the tailer as a fake replica.
//   3. Apply DDL on the source DB (CREATE TABLE).
//   4. Confirm the tailer dispatches the DDL into the replicator's
//      target databases (the cached template, in production).
//
// The replicator's actual application of DDL to cached templates
// is exercised here against a stand-in template DB we register
// manually — that's enough to prove the read+dispatch path works
// against real binlog events.
package binlog_e2e

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/binlog"
	"github.com/stubbedev/treeman/internal/store"
)

func TestBinlogTailerAppliesDDL(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mysql:13446", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13446", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	// Open a host-side connection to set up source + a stand-in
	// "template" DB.
	hostDB, err := sql.Open("mysql", "root:rootpw@tcp(127.0.0.1:13446)/")
	if err != nil {
		t.Fatal(err)
	}
	defer hostDB.Close()
	const sourceDB = "bl_source"
	const templateDB = "bl_template"
	for _, dbn := range []string{sourceDB, templateDB} {
		if _, err := hostDB.Exec(fmt.Sprintf("CREATE DATABASE `%s`", dbn)); err != nil {
			t.Fatalf("create %s: %v", dbn, err)
		}
	}
	// Match initial state: source has one table; template mirrors it.
	for _, dbn := range []string{sourceDB, templateDB} {
		if _, err := hostDB.Exec(
			fmt.Sprintf("CREATE TABLE `%s`.widgets (id INT PRIMARY KEY, name VARCHAR(64))", dbn),
		); err != nil {
			t.Fatalf("seed table in %s: %v", dbn, err)
		}
	}

	// Stand up SQLite store and register the template so the
	// replicator's "apply to all matching templates" logic finds it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	storePath := filepath.Join(t.TempDir(), "tm.db")
	st, err := store.Open(ctx, storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	repoID, err := st.EnsureRepo(ctx, "/tmp/binlog-repo", "binlog-repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSnapshot(ctx, store.SnapshotRecord{
		Fingerprint:   "binlog-test-fp",
		Engine:        "mysql",
		EngineVersion: "8.4",
		SourceDB:      sourceDB,
		TemplateName:  templateDB,
		RepoID:        repoID,
	}); err != nil {
		t.Fatalf("RecordSnapshot: %v", err)
	}

	// Build a Config with binlog enabled.
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql: &config.MysqlConn{
				Host:     "127.0.0.1",
				Port:     13446,
				User:     "root",
				Password: "rootpw",
				Binlog:   &config.BinlogConfig{Enabled: true, ServerID: 4242},
			},
		},
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	// applyDefaults via LoadGlobal would fill in flavor + ApplyDDL;
	// do it manually here for the unit-style flow.
	tval := true
	fval := false
	cfg.Connections.Mysql.Binlog.Flavor = "mysql"
	cfg.Connections.Mysql.Binlog.ApplyDDL = &tval
	cfg.Connections.Mysql.Binlog.ApplyDML = &fval

	rep, err := binlog.New(cfg, st, repoID, sourceDB)
	if err != nil {
		t.Fatalf("binlog.New: %v", err)
	}
	defer rep.Stop()
	go func() {
		if err := rep.Start(ctx); err != nil {
			t.Logf("replicator Start exit: %v", err)
		}
	}()
	time.Sleep(2 * time.Second) // let the replicator connect + handshake

	// DDL on source — replicator should replay it onto template.
	// IMPORTANT: issue UNQUALIFIED `ALTER TABLE widgets` so the
	// binlog records the query without a schema prefix. Treeman
	// then USEs each target schema before re-running the statement.
	// Fully-qualified DDL (e.g. `ALTER TABLE source.widgets`)
	// would bypass USE and stay pinned to source — a limitation
	// users should avoid in production hooks too.
	scopedDB, err := sql.Open("mysql",
		fmt.Sprintf("root:rootpw@tcp(127.0.0.1:13446)/%s", sourceDB))
	if err != nil {
		t.Fatal(err)
	}
	defer scopedDB.Close()
	if _, err := scopedDB.Exec("ALTER TABLE widgets ADD COLUMN price DECIMAL(10,2)"); err != nil {
		t.Fatalf("DDL on source: %v", err)
	}

	// Poll for template to have the new column.
	harness.WaitForReady(t, "ddl-replay", 30*time.Second, func() error {
		row := hostDB.QueryRow(
			"SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=? AND table_name='widgets' AND column_name='price'",
			templateDB,
		)
		var n int
		if err := row.Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("price column not on template yet")
		}
		return nil
	})
	t.Logf("binlog tailer replayed ALTER TABLE onto template %s", templateDB)
}

var _ = os.Getenv // keep import used if test grows
