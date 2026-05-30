package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/ident"
)

// templateStage is the once-per-template export used by SnapshotRestore
// to avoid serializing a fan-out on FLUSH TABLES … FOR EXPORT.
//
// A template is immutable for the life of a fan-out, so its InnoDB
// tablespaces are copied to `dir` exactly once (under a single export
// lock); every restore then imports plain file copies from `dir`
// without locking the template. Exactly one of skip / err / (dir set)
// is meaningful after `once` fires:
//
//   - skip != nil  → preconditions for physical staging not met
//     (no ContainerRef, engine binary absent, empty secure_file_priv,
//     non-InnoDB table, empty schema). Caller uses the logical restore.
//   - err != nil   → staging was attempted but failed. Caller logs a
//     warning and uses the logical restore.
//   - otherwise    → dir/tables/cid/engineBin/datadir are populated and
//     physicalRestoreFromStage can import from `dir`.
type templateStage struct {
	once sync.Once

	dir       string   // container path holding the exported .ibd/.cfg
	tables    []string // InnoDB base tables captured from the template
	cid       string   // resolved container id
	engineBin string   // container engine binary (docker/podman/…)
	datadir   string   // mysql @@datadir inside the container

	skip *physicalSkippedError
	err  error
}

// ensureStage returns the templateStage for `template`, running the
// one-shot export the first time it is requested. Concurrent callers
// block on sync.Once until the export completes, then all read the same
// result.
func (d *Driver) ensureStage(ctx context.Context, template string) *templateStage {
	d.stagesMu.Lock()
	if d.stages == nil {
		d.stages = make(map[string]*templateStage)
	}
	st := d.stages[template]
	if st == nil {
		st = &templateStage{}
		d.stages[template] = st
	}
	d.stagesMu.Unlock()

	st.once.Do(func() { d.stageTemplate(ctx, st, template) })
	return st
}

// resolveStageTarget runs the physical-clone preconditions for staging
// `template` and resolves the container/datadir coordinates plus its
// InnoDB table list. Mirrors the checks in tryPhysicalSnapshotCreate so
// the fallback behaviour is identical:
//
//   - skip != nil → staging not applicable; caller uses logical.
//   - err != nil  → a precondition probe itself failed.
//   - otherwise   → all returned coordinates are populated.
func (d *Driver) resolveStageTarget(ctx context.Context, template string) (
	cid, engineBin, datadir, stageDir string, tables []string,
	skip *physicalSkippedError, err error,
) {
	if d.cfg.Container == "" && d.cfg.ComposeService == "" {
		return "", "", "", "", nil, &physicalSkippedError{reason: "no ContainerRef on connections.mysql"}, nil
	}
	opts := containerip.Opts{
		Container:      d.cfg.Container,
		ComposeService: d.cfg.ComposeService,
		ComposeProject: d.cfg.ComposeProject,
		Engine:         d.cfg.ContainerEngine,
		Network:        d.cfg.Network,
	}
	cid, cerr := containerip.ContainerID(ctx, opts)
	if cerr != nil {
		return "", "", "", "", nil, &physicalSkippedError{reason: "container not resolvable: " + cerr.Error()}, nil
	}
	engineBin = opts.Engine
	if engineBin == "" {
		engineBin = "docker"
	}
	if _, lerr := exec.LookPath(engineBin); lerr != nil {
		return "", "", "", "", nil, &physicalSkippedError{reason: engineBin + " binary not on PATH"}, nil
	}

	if derr := d.DB.QueryRowContext(ctx, "SELECT @@datadir").Scan(&datadir); derr != nil {
		return "", "", "", "", nil, nil, fmt.Errorf("read @@datadir: %w", derr)
	}
	datadir = strings.TrimRight(datadir, "/")

	// secure_file_priv is a mysql-owned directory the server is willing
	// to read/write; staging the exported files there keeps them out of
	// the data dir (where a stray dir would look like an unknown schema)
	// while staying writable by the in-container mysql user. An empty /
	// NULL value (secure_file_priv disabled) means there is no safe
	// mysql-owned scratch dir → skip to logical.
	var secureDir sql.NullString
	if serr := d.DB.QueryRowContext(ctx, "SELECT @@secure_file_priv").Scan(&secureDir); serr != nil {
		return "", "", "", "", nil, nil, fmt.Errorf("read @@secure_file_priv: %w", serr)
	}
	if !secureDir.Valid || strings.TrimSpace(secureDir.String) == "" {
		return "", "", "", "", nil, &physicalSkippedError{
			reason: "secure_file_priv empty; no mysql-owned staging dir for exported tablespaces",
		}, nil
	}
	stageDir = strings.TrimRight(secureDir.String, "/") + "/_tm_stage/" + template

	tables, terr := listInnoDBTables(ctx, d.DB, template)
	if terr != nil {
		var sk *physicalSkippedError
		if errors.As(terr, &sk) {
			return "", "", "", "", nil, sk, nil
		}
		return "", "", "", "", nil, nil, terr
	}
	if len(tables) == 0 {
		return "", "", "", "", nil, &physicalSkippedError{reason: "no InnoDB tables to transfer"}, nil
	}
	return cid, engineBin, datadir, stageDir, tables, nil, nil
}

// stageTemplate exports `template`'s InnoDB tablespaces to a staging dir
// under secure_file_priv. Runs once per template via ensureStage.
func (d *Driver) stageTemplate(ctx context.Context, st *templateStage, template string) {
	cid, engineBin, datadir, stageDir, tables, skip, err := d.resolveStageTarget(ctx, template)
	if skip != nil {
		st.skip = skip
		return
	}
	if err != nil {
		st.err = err
		return
	}

	// Fresh staging dir, owned by the in-container mysql user so the
	// later `cp` lands files mysqld can read for IMPORT.
	_ = dockerExecRoot(ctx, engineBin, cid, "rm", "-rf", stageDir)
	if err := dockerExecMysql(ctx, engineBin, cid, "mkdir", "-p", stageDir); err != nil {
		st.err = fmt.Errorf("mkdir staging dir: %w", err)
		return
	}

	flushList, err := exportFlushList(template, tables)
	if err != nil {
		st.err = err
		return
	}

	// FLUSH … FOR EXPORT on a held connection: it freezes the template
	// tables read-only and writes a .cfg next to each .ibd describing
	// the tablespace layout. The .cfg only exists while the lock is
	// held (UNLOCK deletes it), so the copy MUST run before UNLOCK.
	lockConn, err := d.DB.Conn(ctx)
	if err != nil {
		st.err = err
		return
	}
	defer func() { _ = lockConn.Close() }()
	if _, err := lockConn.ExecContext(ctx, "FLUSH TABLES "+strings.Join(flushList, ", ")+" FOR EXPORT"); err != nil {
		st.err = fmt.Errorf("FLUSH TABLES FOR EXPORT: %w", err)
		return
	}
	copyErr := copyTablespaces(ctx, engineBin, cid, datadir+"/"+template, stageDir, tables)
	if _, err := lockConn.ExecContext(ctx, "UNLOCK TABLES"); err != nil && copyErr == nil {
		copyErr = fmt.Errorf("UNLOCK TABLES: %w", err)
	}
	if copyErr != nil {
		_ = dockerExecRoot(ctx, engineBin, cid, "rm", "-rf", stageDir)
		st.err = copyErr
		return
	}

	st.cid = cid
	st.engineBin = engineBin
	st.datadir = datadir
	st.dir = stageDir
	st.tables = tables
}

// exportFlushList renders the `schema`.`table` list for a FLUSH TABLES …
// FOR EXPORT statement.
func exportFlushList(schema string, tables []string) ([]string, error) {
	qschema, err := ident.QuoteMySQL(schema)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(tables))
	for _, t := range tables {
		qt, qerr := ident.QuoteMySQL(t)
		if qerr != nil {
			return nil, qerr
		}
		out = append(out, qschema+"."+qt)
	}
	return out, nil
}

// copyTablespaces copies each table's .cfg + .ibd from srcDir to dstDir
// inside the container as the mysql user. .cfg is copied first so a
// reader never sees an .ibd without its layout descriptor.
func copyTablespaces(ctx context.Context, engineBin, cid, srcDir, dstDir string, tables []string) error {
	for _, t := range tables {
		for _, ext := range []string{".cfg", ".ibd"} {
			if err := dockerExecMysql(ctx, engineBin, cid,
				"cp", srcDir+"/"+t+ext, dstDir+"/"+t+ext); err != nil {
				return fmt.Errorf("cp %s%s: %w", t, ext, err)
			}
		}
	}
	return nil
}

// physicalRestoreFromStage imports the staged tablespaces into `target`.
// No lock is taken on the template: every input is a plain file copy out
// of the staging dir, so an arbitrary number of restores run in
// parallel. The schema is recreated from the template's live table
// definitions (a lock-free metadata read), each tablespace discarded,
// the staged .ibd/.cfg copied in, then IMPORT TABLESPACE attaches them.
//
// On any failure past the target CREATE the partially-built target is
// dropped so the caller's logical fallback gets a clean slate.
func (d *Driver) physicalRestoreFromStage(ctx context.Context, st *templateStage, template, target string) (err error) {
	qtarget, err := ident.QuoteMySQL(target)
	if err != nil {
		return err
	}
	qtemplate, err := ident.QuoteMySQL(template)
	if err != nil {
		return err
	}
	if _, err := d.DB.ExecContext(ctx, "DROP DATABASE IF EXISTS "+qtarget); err != nil {
		return err
	}
	if _, err := d.DB.ExecContext(ctx,
		"CREATE DATABASE "+qtarget+" DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		return err
	}

	var ok bool
	defer func() {
		if ok {
			return
		}
		dropCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = d.DB.ExecContext(dropCtx, "DROP DATABASE IF EXISTS "+qtarget)
		_ = exec.CommandContext(dropCtx, st.engineBin,
			"exec", st.cid, "rm", "-rf", st.datadir+"/"+target).Run()
	}()

	prepConn, err := d.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = prepConn.Close() }()
	if _, err := prepConn.ExecContext(ctx, "SET SESSION foreign_key_checks=0"); err != nil {
		return fmt.Errorf("disable foreign_key_checks: %w", err)
	}
	if _, err := prepConn.ExecContext(ctx, "SET SESSION unique_checks=0"); err != nil {
		return fmt.Errorf("disable unique_checks: %w", err)
	}
	if _, err := prepConn.ExecContext(ctx, "USE "+qtarget); err != nil {
		return fmt.Errorf("USE %s: %w", qtarget, err)
	}
	for _, t := range st.tables {
		if err := recreateAndDiscard(ctx, prepConn, qtemplate, t); err != nil {
			return err
		}
	}

	if err := copyTablespaces(ctx, st.engineBin, st.cid, st.dir, st.datadir+"/"+target, st.tables); err != nil {
		return fmt.Errorf("copy staged tablespaces → %s: %w", target, err)
	}

	for _, t := range st.tables {
		qt, qerr := ident.QuoteMySQL(t)
		if qerr != nil {
			return qerr
		}
		if _, err := prepConn.ExecContext(ctx, "ALTER TABLE "+qt+" IMPORT TABLESPACE"); err != nil {
			return fmt.Errorf("IMPORT TABLESPACE %s: %w", t, err)
		}
	}
	ok = true
	return nil
}

// dropStage best-effort removes a template's staging dir. Resolves the
// path from secure_file_priv rather than the in-memory map so a daemon
// that never staged this template (e.g. after a restart) still cleans
// up. Silent on every failure: staging is a cache, not state.
func (d *Driver) dropStage(ctx context.Context, template string) {
	d.stagesMu.Lock()
	if d.stages != nil {
		delete(d.stages, template)
	}
	d.stagesMu.Unlock()

	if d.cfg.Container == "" && d.cfg.ComposeService == "" {
		return
	}
	opts := containerip.Opts{
		Container:      d.cfg.Container,
		ComposeService: d.cfg.ComposeService,
		ComposeProject: d.cfg.ComposeProject,
		Engine:         d.cfg.ContainerEngine,
		Network:        d.cfg.Network,
	}
	cid, err := containerip.ContainerID(ctx, opts)
	if err != nil {
		return
	}
	engineBin := opts.Engine
	if engineBin == "" {
		engineBin = "docker"
	}
	if _, err := exec.LookPath(engineBin); err != nil {
		return
	}
	var secureDir sql.NullString
	if err := d.DB.QueryRowContext(ctx, "SELECT @@secure_file_priv").Scan(&secureDir); err != nil {
		return
	}
	if !secureDir.Valid || strings.TrimSpace(secureDir.String) == "" {
		return
	}
	stageDir := strings.TrimRight(secureDir.String, "/") + "/_tm_stage/" + template
	_ = dockerExecRoot(ctx, engineBin, cid, "rm", "-rf", stageDir)
}

// dockerExecMysql runs `<engine> exec --user mysql <cid> <args…>`.
// `--user mysql` is load-bearing for file ops: the default exec user is
// root, so files created by cp/mkdir would be root-owned and opaque to
// the non-root mysqld for IMPORT TABLESPACE.
func dockerExecMysql(ctx context.Context, engineBin, cid string, args ...string) error {
	full := append([]string{"exec", "--user", "mysql", cid}, args...)
	if out, err := exec.CommandContext(ctx, engineBin, full...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s exec --user mysql %v: %w (%s)", engineBin, args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dockerExecRoot runs `<engine> exec <cid> <args…>` as the default
// (root) user. Used only for cleanup rm -rf, which must succeed
// regardless of who owns the staged files.
func dockerExecRoot(ctx context.Context, engineBin, cid string, args ...string) error {
	full := append([]string{"exec", cid}, args...)
	if out, err := exec.CommandContext(ctx, engineBin, full...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s exec %v: %w (%s)", engineBin, args, err, strings.TrimSpace(string(out)))
	}
	return nil
}
