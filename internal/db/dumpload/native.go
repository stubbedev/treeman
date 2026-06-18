package dumpload

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
)

// LoadStrategy names the dump-load fast-path actually used by
// LoadMySQL/LoadPostgres. Surfaced in the return value so callers can
// log which path ran (and how much faster the cold build got).
type LoadStrategy string

const (
	// StrategyDockerExec ran the engine's native CLI inside the engine
	// container via `<container-engine> exec -i ...`. Fastest because
	// the CLI talks to the engine over a unix socket inside the same
	// container — no network layer between host and engine.
	StrategyDockerExec LoadStrategy = "docker-exec"
	// StrategyNativeCLI ran the engine's CLI from $PATH on the host,
	// piping the dump over stdin. Skips per-statement round-trips that
	// the Go wire-protocol loader pays — typically several times
	// faster on large dumps.
	StrategyNativeCLI LoadStrategy = "native-cli"
	// StrategyWire used the in-process statement-streaming loader over
	// the existing database/sql connection. Always available — picked
	// when neither fast path is usable.
	StrategyWire LoadStrategy = "wire"
)

// mysqlSessionPrefix is the same pre-loop SET statements the wire
// loader runs on its connection. Prepended to the CLI's stdin so the
// fast paths apply dumps under identical session flags — without
// these mysql refuses to apply mysqldump output that depends on
// FK/uniqueness deferral, and a binlog-enabled server logs every
// inserted row needlessly during a cold build.
var mysqlSessionPrefix = []byte("" +
	"SET SESSION foreign_key_checks=0;\n" +
	"SET SESSION unique_checks=0;\n" +
	"SET SESSION sql_log_bin=0;\n")

// postgresSessionPrefix is the psql-side equivalent: -v ON_ERROR_STOP
// is set on the command line, but a SET that the wire loader doesn't
// otherwise apply belongs here so callers can extend later.
var postgresSessionPrefix = []byte("")

// tryDockerExecMySQL pipes the dump through `<engine> exec -i CID mysql …`.
// Returns (true, nil) on success (caller can stop), (false, nil) when
// the container ref isn't set / can't be resolved / engine binary
// missing (caller should fall through to the next strategy), and
// (false, err) when the exec ran but FAILED — the caller logs the
// warning and falls back to the wire loader.
func tryDockerExecMySQL(ctx context.Context, conn *config.MysqlConn, targetDB string, stdin io.Reader) (bool, error) {
	if conn == nil || (conn.Container == "" && conn.ComposeService == "") {
		return false, nil
	}
	opts := containerOpts(conn.ContainerRef)
	cid, cerr := containerip.ContainerID(ctx, opts)
	if cerr != nil {
		return false, nil //nolint:nilerr // container not resolvable; fall through to the next strategy
	}
	engineBin := opts.Engine
	if engineBin == "" {
		engineBin = "docker"
	}
	if _, err := exec.LookPath(engineBin); err != nil {
		return false, nil //nolint:nilerr // CLI not on PATH; soft fall-through to the next strategy
	}
	user := conn.User
	if user == "" {
		user = "root"
	}
	args := []string{
		"exec", "-i", "-e", "MYSQL_PWD=" + conn.Password, cid,
		"mysql", "-u", user, targetDB,
	}
	cmd := exec.CommandContext(ctx, engineBin, args...)
	cmd.Stdin = io.MultiReader(bytes.NewReader(mysqlSessionPrefix), stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("%s exec mysql: %w (stderr: %s)", engineBin, err, stderr.String())
	}
	return true, nil
}

// tryNativeCLIMySQL pipes the dump through the host's `mysql` CLI when
// it's on PATH. Same fall-through semantics as tryDockerExecMySQL.
func tryNativeCLIMySQL(ctx context.Context, conn *config.MysqlConn, targetDB string, stdin io.Reader) (bool, error) {
	if conn == nil {
		return false, nil
	}
	if _, err := exec.LookPath("mysql"); err != nil {
		return false, nil //nolint:nilerr // CLI not on PATH; soft fall-through to the next strategy
	}
	host := conn.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := conn.Port
	if port == 0 {
		port = 3306
	}
	user := conn.User
	if user == "" {
		user = "root"
	}
	cmd := exec.CommandContext(ctx, "mysql",
		"--protocol=tcp",
		"--host="+host,
		"--port="+strconv.Itoa(int(port)),
		"-u", user,
		targetDB,
	)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+conn.Password)
	cmd.Stdin = io.MultiReader(bytes.NewReader(mysqlSessionPrefix), stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("native mysql: %w (stderr: %s)", err, stderr.String())
	}
	return true, nil
}

// tryDockerExecPostgres mirrors tryDockerExecMySQL for psql.
func tryDockerExecPostgres(ctx context.Context, conn *config.PostgresConn, targetDB string, stdin io.Reader) (bool, error) {
	if conn == nil || (conn.Container == "" && conn.ComposeService == "") {
		return false, nil
	}
	opts := containerOpts(conn.ContainerRef)
	cid, cerr := containerip.ContainerID(ctx, opts)
	if cerr != nil {
		return false, nil //nolint:nilerr // container not resolvable; fall through to the next strategy
	}
	engineBin := opts.Engine
	if engineBin == "" {
		engineBin = "docker"
	}
	if _, err := exec.LookPath(engineBin); err != nil {
		return false, nil //nolint:nilerr // CLI not on PATH; soft fall-through to the next strategy
	}
	user := conn.User
	if user == "" {
		user = "postgres"
	}
	args := []string{
		"exec", "-i", "-e", "PGPASSWORD=" + conn.Password, cid,
		"psql", "-U", user, "-d", targetDB, "-v", "ON_ERROR_STOP=1",
	}
	cmd := exec.CommandContext(ctx, engineBin, args...)
	cmd.Stdin = io.MultiReader(bytes.NewReader(postgresSessionPrefix), stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("%s exec psql: %w (stderr: %s)", engineBin, err, stderr.String())
	}
	return true, nil
}

// tryNativeCLIPostgres pipes the dump through the host's `psql` CLI.
func tryNativeCLIPostgres(ctx context.Context, conn *config.PostgresConn, targetDB string, stdin io.Reader) (bool, error) {
	if conn == nil {
		return false, nil
	}
	if _, err := exec.LookPath("psql"); err != nil {
		return false, nil //nolint:nilerr // CLI not on PATH; soft fall-through to the next strategy
	}
	host := conn.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := conn.Port
	if port == 0 {
		port = 5432
	}
	user := conn.User
	if user == "" {
		user = "postgres"
	}
	cmd := exec.CommandContext(ctx, "psql",
		"--host="+host,
		"--port="+strconv.Itoa(int(port)),
		"--username="+user,
		"--dbname="+targetDB,
		"-v", "ON_ERROR_STOP=1",
	)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+conn.Password)
	cmd.Stdin = io.MultiReader(bytes.NewReader(postgresSessionPrefix), stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("native psql: %w (stderr: %s)", err, stderr.String())
	}
	return true, nil
}

// containerOpts adapts a config.ContainerRef into the containerip.Opts
// the resolver expects. Keeps the dispatch helpers free of any
// containerip imports beyond ContainerID.
func containerOpts(ref config.ContainerRef) containerip.Opts {
	return containerip.Opts{
		Container:      ref.Container,
		ComposeService: ref.ComposeService,
		ComposeProject: ref.ComposeProject,
		Engine:         ref.ContainerEngine,
		Network:        ref.Network,
	}
}
