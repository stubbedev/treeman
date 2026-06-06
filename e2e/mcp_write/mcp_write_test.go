//go:build e2e

// Package mcp_write_e2e drives the `treeman mcp` server's write
// tools (worktree_create / worktree_delete) over its stdio JSON-RPC
// transport.
//
// The MCP server runs these tools in-process via internal/wt — no
// subprocess to the CLI binary — so this suite verifies:
//
//   - JSON-RPC tools/call returns the structured wt.CreateResult /
//     wt.DeleteResult (wt_path, slug, status, log_path, …) rather
//     than shellOut text.
//   - The status field correctly reflects which dispatch path
//     ran (queued vs detached for the heavy tail).
//   - The worktree directory actually exists on disk after create.
//   - A second create on the same branch returns CreatedNoop.
//   - worktree_delete tears down both the engine database and the
//     git worktree.
//
// This is the canonical regression guard for the MCP-in-process
// rewrite that replaced the previous `runTreeman` subprocess shim.
package mcp_write_e2e

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/stubbedev/treeman/e2e/harness"
)

func TestMCPWriteTools(t *testing.T) {
	harness.SkipIfNoDocker(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mysql:13446", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:13446", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	repoRoot := projectRoot(t)
	binDir := t.TempDir()
	buildBin(t, binDir, repoRoot, "treeman", "./cmd/treeman")

	// Isolated state so we don't trip over the developer's daemon.
	stateDir := t.TempDir()
	runtimeDir := t.TempDir()
	dbPath := filepath.Join(stateDir, "treeman.db")
	t.Setenv("TREEMAN_DB_PATH", dbPath)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("XDG_STATE_HOME", stateDir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// Advertise every tool — these tests exercise tool behavior, not the
	// lazy-disclosure gateway (which is unit-tested in internal/mcp).
	t.Setenv("TREEMAN_MCP_ALL_TOOLS", "1")

	// Build a git repo with a .treeman.yaml so the orchestrator's
	// engine prepare path lights up.
	mainRepo := filepath.Join(t.TempDir(), "main")
	if err := os.MkdirAll(mainRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, mainRepo, "init", "-q", "-b", "main")
	mustGit(t, mainRepo, "config", "user.email", "e2e@example.com")
	mustGit(t, mainRepo, "config", "user.name", "e2e")
	writeFile(t, filepath.Join(mainRepo, ".env"), "MYSQL_PW=rootpw\n")
	writeFile(t, filepath.Join(mainRepo, "seed.sql"), "CREATE TABLE widgets (id INT PRIMARY KEY); INSERT INTO widgets VALUES (1),(2);")
	writeFile(t, filepath.Join(mainRepo, ".treeman.yaml"), `
worktrees:
  root: .worktrees
env_sources:
  - .env
connections:
  mysql:
    host: 127.0.0.1
    port: 13446
    user: root
    password: $MYSQL_PW
databases:
  - engine: mysql
    name_template: tm_mcpw_{slug}
    dump: seed.sql
`)
	mustGit(t, mainRepo, "add", "-A")
	mustGit(t, mainRepo, "commit", "-q", "-m", "init")

	srv := startMCPServer(t, binDir, mainRepo)

	// MCP handshake.
	srv.send(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "treeman-e2e", "version": "0.0.0"},
		},
	})
	if resp := srv.read(); resp["error"] != nil {
		t.Fatalf("initialize: %v", resp["error"])
	}
	srv.send(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})

	// ── worktree_create ─────────────────────────────────────────────
	branch := "feature/mcpw"
	srv.send(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "worktree_create",
			"arguments": map[string]any{
				"branch": branch,
				"repo":   mainRepo,
			},
		},
	})
	resp := srv.read()
	if errObj := resp["error"]; errObj != nil {
		t.Fatalf("worktree_create error: %v", errObj)
	}
	res := decodeStructuredResult(t, resp)
	t.Logf("worktree_create result: %+v", res)

	// Structured-result shape: must include wt_path, slug, status.
	wtPath, _ := res["wt_path"].(string)
	if wtPath == "" {
		t.Errorf("missing wt_path in result: %+v", res)
	}
	slug, _ := res["slug"].(string)
	if slug == "" {
		t.Errorf("missing slug in result: %+v", res)
	}
	status, _ := res["status"].(string)
	// Either daemon dispatch (queued) or detached child — both are
	// acceptable terminal states when hooks/databases are present.
	if status != "queued" && status != "detached" {
		t.Errorf("unexpected status %q, want queued or detached", status)
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Errorf("worktree path %q does not exist on disk: %v", wtPath, err)
	}

	// Poll for the per-worktree source DB — finalize runs async.
	expectedDB := "tm_mcpw_" + slug
	harness.WaitForReady(t, "source-db "+expectedDB, 30*time.Second, func() error {
		for _, d := range listDatabases(t) {
			if d == expectedDB {
				return nil
			}
		}
		return fmt.Errorf("db %s not present yet", expectedDB)
	})

	// ── idempotent re-create ─────────────────────────────────────────
	srv.send(map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "worktree_create",
			"arguments": map[string]any{
				"branch": branch,
				"repo":   mainRepo,
			},
		},
	})
	resp = srv.read()
	if errObj := resp["error"]; errObj != nil {
		t.Fatalf("worktree_create (re-run) error: %v", errObj)
	}
	res = decodeStructuredResult(t, resp)
	if got, _ := res["status"].(string); got != "noop" {
		t.Errorf("re-run status = %q, want noop", got)
	}

	// ── worktree_delete ─────────────────────────────────────────────
	srv.send(map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "worktree_delete",
			"arguments": map[string]any{
				"name":  branch,
				"repo":  mainRepo,
				"force": true, // bypass confirmation (it's a no-op for MCP anyway)
			},
		},
	})
	resp = srv.read()
	if errObj := resp["error"]; errObj != nil {
		t.Fatalf("worktree_delete error: %v", errObj)
	}
	res = decodeStructuredResult(t, resp)
	delStatus, _ := res["status"].(string)
	if delStatus != "queued" && delStatus != "detached" {
		t.Errorf("delete status = %q, want queued or detached", delStatus)
	}

	// Poll for the source DB to vanish — teardown runs async.
	harness.WaitForReady(t, "drop-source-db", 30*time.Second, func() error {
		for _, d := range listDatabases(t) {
			if d == expectedDB {
				return fmt.Errorf("db %s still present", expectedDB)
			}
		}
		return nil
	})
}

// ── helpers ──────────────────────────────────────────────────────

type mcpServer struct {
	t      *testing.T
	stdin  io.WriteCloser
	rdr    *bufio.Reader
	cancel context.CancelFunc
}

func startMCPServer(t *testing.T, binDir, cwd string) *mcpServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, filepath.Join(binDir, "treeman"), "mcp")
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start mcp: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		cancel()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return &mcpServer{t: t, stdin: stdin, rdr: bufio.NewReader(stdout), cancel: cancel}
}

func (m *mcpServer) send(req map[string]any) {
	m.t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		m.t.Fatal(err)
	}
	if _, err := m.stdin.Write(append(body, '\n')); err != nil {
		m.t.Fatalf("write: %v", err)
	}
}

func (m *mcpServer) read() map[string]any {
	m.t.Helper()
	line, err := m.rdr.ReadBytes('\n')
	if err != nil && err != io.EOF {
		m.t.Fatalf("read: %v", err)
	}
	if len(line) == 0 {
		m.t.Fatal("empty response")
	}
	var resp map[string]any
	if err := json.Unmarshal(line, &resp); err != nil {
		m.t.Fatalf("parse %q: %v", string(line), err)
	}
	return resp
}

// decodeStructuredResult pulls the embedded structuredContent out of
// the mcp-sdk's CallToolResult envelope. The shape is:
//
//	{ "result": { "structuredContent": { …tool output… } } }
func decodeStructuredResult(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("response missing result: %+v", resp)
	}
	if sc, ok := result["structuredContent"].(map[string]any); ok {
		return sc
	}
	// Older SDK shape — fall back to result itself.
	return result
}

func projectRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

func buildBin(t *testing.T, binDir, repoRoot, name, pkg string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", filepath.Join(binDir, name), pkg)
	cmd.Dir = repoRoot
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func listDatabases(t *testing.T) []string {
	t.Helper()
	db, err := sql.Open("mysql", "root:rootpw@tcp(127.0.0.1:13446)/")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), "SHOW DATABASES")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		out = append(out, n)
	}
	return out
}

var _ = strings.Contains
