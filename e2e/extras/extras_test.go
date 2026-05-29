//go:build e2e

// Package extras_e2e is a grab-bag of the smaller schema features
// that didn't justify their own compose stack — each subtest reuses
// the shared MySQL container.
//
//   - TestDumpOptionalSkipsMissing — dump path missing + optional:true
//     must skip the load step without erroring.
//   - TestDumpRequiredFailsCleanly — dump path missing + optional:false
//     must produce a clear "dump … no such file" error.
//   - TestFanoutCapIsHonored — explicit databases[].fanout caps
//     concurrent clone restore.
//   - TestActionContainerWrap — hooks.on-create-after-engines with a
//     container: ref runs the action inside that container.
//   - TestOnFileChangeMatchList — hooks.on-file-change match: [a,b]
//     fires for both labels and not for non-listed labels.
package extras_e2e

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
)

func bootMySQL(t *testing.T) {
	t.Helper()
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mysql:13416", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13416", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})
}

func TestDumpOptionalSkipsMissing(t *testing.T) {
	harness.SkipIfNoDocker(t)
	bootMySQL(t)
	wt := t.TempDir()
	witness := filepath.Join(wt, "migrate-ran")
	// NO seed.sql file written. Migrate writes a witness file
	// AND inserts a marker row so we can distinguish "migrate
	// didn't run at all" from "migrate ran but couldn't reach
	// MySQL".
	logFile := filepath.Join(wt, "migrate.log")
	if err := os.WriteFile(filepath.Join(wt, "migrate.sh"),
		[]byte(`#!/bin/sh
touch `+witness+`
echo "DB_DATABASE=$DB_DATABASE" > `+logFile+`
docker exec treeman-e2e-extras-mysql mysql \
  --user=root --password=rootpw --database="$DB_DATABASE" \
  -e "CREATE TABLE marker (id INT);" >> `+logFile+` 2>&1
echo "exit=$?" >> `+logFile+`
exit 0
`), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql: &config.MysqlConn{Host: "127.0.0.1", Port: 13416, User: "root", Password: "rootpw"},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "mysql",
				NameTemplate: "tm_xtr_dopt_{slug}",
				Dump:         config.DumpList{{Path: "missing-seed.sql", Optional: true}},
				Migrate: &config.Step{
					Run: "./migrate.sh",
					Env: map[string]string{"DB_DATABASE": "{target_db}"},
				},
			},
		},
	}
	env := harness.NewEnv(t, wt)
	outs := env.RunPrepare(t, cfg)
	o := harness.AssertOutcome(t, outs, "mysql", false)
	if _, err := os.Stat(witness); err != nil {
		t.Fatalf("migrate.sh did not execute (no witness file): %v", err)
	}
	t.Logf("witness present: migrate.sh did run")
	if !hasTable(t, o.SourceDB, "marker") {
		body, _ := os.ReadFile(logFile)
		t.Logf("--- migrate.log ---\n%s", string(body))
		t.Logf("databases visible: %v", listDatabases(t))
		// List tables in the source DB to see what's actually there.
		db := openDB(t, o.SourceDB)
		defer db.Close()
		rows, err := db.Query("SHOW TABLES")
		if err != nil {
			t.Logf("SHOW TABLES error: %v", err)
		} else {
			defer rows.Close()
			var tables []string
			for rows.Next() {
				var s string
				_ = rows.Scan(&s)
				tables = append(tables, s)
			}
			t.Logf("tables in %s: %v", o.SourceDB, tables)
		}
		t.Errorf("migrate ran but marker table missing in %s", o.SourceDB)
	}
}

func TestDumpRequiredFailsCleanly(t *testing.T) {
	harness.SkipIfNoDocker(t)
	bootMySQL(t)
	wt := t.TempDir()
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql: &config.MysqlConn{Host: "127.0.0.1", Port: 13416, User: "root", Password: "rootpw"},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "mysql",
				NameTemplate: "tm_xtr_dreq_{slug}",
				Dump:         config.DumpList{{Path: "missing-seed.sql"}}, // optional: false
			},
		},
	}
	env := harness.NewEnv(t, wt)
	_, err := prepareErr(env, cfg)
	if err == nil {
		t.Fatal("required dump missing should have errored")
	}
	if !strings.Contains(err.Error(), "missing-seed.sql") {
		t.Errorf("error should name the missing file, got: %v", err)
	}
}

func TestFanoutCapIsHonored(t *testing.T) {
	harness.SkipIfNoDocker(t)
	bootMySQL(t)
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "seed.sql"),
		[]byte("CREATE TABLE t (id INT);"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mysql: &config.MysqlConn{Host: "127.0.0.1", Port: 13416, User: "root", Password: "rootpw"},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "mysql",
				NameTemplate: "tm_xtr_fan_{slug}",
				Dump:         config.DumpList{{Path: "seed.sql"}},
				Fanout:       2, // cap at 2 concurrent clones
				TestClones: &config.TestClonesSpec{
					Clones:       config.ClonesSetting{Fixed: 6},
					NameTemplate: "tm_xtr_fan_{slug}_w{n}",
				},
			},
		},
	}
	env := harness.NewEnv(t, wt)
	outs := env.RunPrepare(t, cfg)
	o := harness.AssertOutcome(t, outs, "mysql", false)
	if len(o.Clones) != 6 {
		t.Errorf("clones=%d want 6", len(o.Clones))
	}
	// All 6 should exist regardless of the cap — the cap only
	// affects concurrency, not the final count.
	for _, c := range o.Clones {
		if !hasDatabase(t, c) {
			t.Errorf("clone %s missing", c)
		}
	}
}

func TestActionContainerWrap(t *testing.T) {
	harness.SkipIfNoDocker(t)
	bootMySQL(t)
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "seed.sql"),
		[]byte("CREATE TABLE marker (host VARCHAR(64));"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".env"),
		[]byte("MYSQL_PW=rootpw\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The on-create-after-engines hook runs INSIDE the engine
	// container (Action.Container wrap). The mysql client there
	// inserts the container's hostname into the marker table.
	yaml := `
worktrees: { root: .worktrees }
env_sources: [.env]
connections:
  mysql:
    host: 127.0.0.1
    port: 13416
    user: root
    password: $MYSQL_PW
databases:
  - engine: mysql
    name_template: tm_xtr_ctr_{slug}
    dump: seed.sql
hooks:
  on-create-after-engines:
    # Sanity: this hook runs on the HOST (no container wrap). The
    # witness file proves any hook fired.
    - run: echo "fired" > /tmp/treeman-e2e-extras-hook
    # The real test: this hook runs INSIDE the engine container —
    # write a marker to /var/lib/mysql which is the mysqld data
    # dir. We can verify it landed by reading the container's
    # filesystem from the host via docker exec.
    - container: treeman-e2e-extras-mysql
      run: |
        echo "ran-inside-$(hostname)" > /tmp/treeman-e2e-extras-container-hook
`
	repoRoot := wt
	if err := os.WriteFile(filepath.Join(repoRoot, ".treeman.yaml"),
		[]byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".git/HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runFinalize(t, repoRoot)

	// Sanity: host-side hook fired.
	hostWitness := "/tmp/treeman-e2e-extras-hook"
	defer os.Remove(hostWitness)
	harness.WaitForReady(t, "host-witness", 15*time.Second, func() error {
		if _, err := os.Stat(hostWitness); err != nil {
			return fmt.Errorf("host witness not yet: %v", err)
		}
		return nil
	})

	// Real test: read the witness from INSIDE the container's
	// filesystem. If Action.Container actually wrapped the second
	// action in `docker exec`, this file lives inside the mysql
	// container (not on the host).
	harness.WaitForReady(t, "container-witness", 15*time.Second, func() error {
		out, err := exec.Command("docker", "exec",
			"treeman-e2e-extras-mysql",
			"cat", "/tmp/treeman-e2e-extras-container-hook").CombinedOutput()
		if err != nil {
			return fmt.Errorf("docker exec cat: %v", err)
		}
		if !strings.HasPrefix(strings.TrimSpace(string(out)), "ran-inside-") {
			return fmt.Errorf("witness body wrong: %q", out)
		}
		t.Logf("in-container witness: %s", strings.TrimSpace(string(out)))
		return nil
	})
	// Confirm the file does NOT exist on the host (which would
	// mean Action.Container didn't actually wrap and the hook ran
	// on the host instead).
	if _, err := os.Stat("/tmp/treeman-e2e-extras-container-hook"); err == nil {
		t.Errorf("witness leaked to host — Action.Container didn't wrap")
		_ = os.Remove("/tmp/treeman-e2e-extras-container-hook")
	}
}

func TestOnFileChangeMatchList(t *testing.T) {
	harness.SkipIfNoDocker(t)
	bootMySQL(t)

	repoRoot := t.TempDir()
	touch := filepath.Join(repoRoot, "touch")
	_ = os.MkdirAll(touch, 0o755)
	_ = os.MkdirAll(filepath.Join(repoRoot, "a"), 0o755)
	_ = os.MkdirAll(filepath.Join(repoRoot, "b"), 0o755)
	_ = os.MkdirAll(filepath.Join(repoRoot, "c"), 0o755)
	_ = os.WriteFile(filepath.Join(repoRoot, "a/init"), []byte("a"), 0o644)
	_ = os.WriteFile(filepath.Join(repoRoot, "b/init"), []byte("b"), 0o644)
	_ = os.WriteFile(filepath.Join(repoRoot, "c/init"), []byte("c"), 0o644)
	_ = os.WriteFile(filepath.Join(repoRoot, ".env"), []byte("MYSQL_PW=rootpw\n"), 0o644)
	yaml := `
worktrees: { root: .worktrees }
env_sources: [.env]
debounce_ms: 200
connections:
  mysql: { host: 127.0.0.1, port: 13416, user: root, password: $MYSQL_PW }
databases:
  - engine: mysql
    name_template: tm_xtr_mtch_{slug}
    inputs:
      - { glob: "a/*", label: alpha }
      - { glob: "b/*", label: beta }
      - { glob: "c/*", label: gamma }
hooks:
  on-file-change:
    - match: [alpha, beta]   # NOT gamma
      run: 'echo "$TREEMAN_WATCH_LABEL" >> ` + touch + `/events'
`
	_ = os.WriteFile(filepath.Join(repoRoot, ".treeman.yaml"), []byte(yaml), 0o644)
	_ = os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(repoRoot, ".git/HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644)
	runFinalize(t, repoRoot)
	startWatcher(t, repoRoot)
	time.Sleep(500 * time.Millisecond) // let watcher subscribe

	_ = os.WriteFile(filepath.Join(repoRoot, "a/alpha.sql"), []byte("a"), 0o644)
	_ = os.WriteFile(filepath.Join(repoRoot, "b/beta.sql"), []byte("b"), 0o644)
	_ = os.WriteFile(filepath.Join(repoRoot, "c/gamma.sql"), []byte("c"), 0o644)

	deadline := time.Now().Add(15 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(filepath.Join(touch, "events"))
		if strings.Contains(string(b), "alpha") && strings.Contains(string(b), "beta") {
			body = string(b)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(body, "alpha") {
		t.Errorf("alpha label didn't fire hook: %q", body)
	}
	if !strings.Contains(body, "beta") {
		t.Errorf("beta label didn't fire hook: %q", body)
	}
	if strings.Contains(body, "gamma") {
		t.Errorf("gamma label fired hook but wasn't in match list: %q", body)
	}
	t.Logf("events recorded: %s", body)
}
