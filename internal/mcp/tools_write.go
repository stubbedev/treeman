package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/prepare"
)

// registerWriteTools binds tools that mutate state. Gated by
// Options.AllowMutations in Serve. Tools that shell out to the
// `treeman` binary (worktree_create, worktree_delete, init,
// schema_install) are further gated by AllowShellOps.
func registerWriteTools(srv *mcpsdk.Server, opts Options) {
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "prepare_run",
		Description: "Run the prepare pipeline for a worktree (ensure → dump → migrate → snapshot → replicate). Foreground; blocks until every engine returns an outcome.",
	}, prepareTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "hook_run",
		Description: "Run one configured hook phase synchronously. Accepts precreate|postcreate|predelete|postdelete. Returns per-group exit codes and stdout/stderr tails.",
	}, hookTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "config_write",
		Description: "Replace .treeman.yaml with the supplied body. The body is parsed into config.Config first; the write only happens if parsing succeeds, so invalid YAML never lands on disk. Returns the byte count written.",
	}, configWriteTool)

	if !opts.AllowShellOps {
		return
	}

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "init_repo",
		Description: "Scaffold .treeman.yaml under cwd (or --repo) by shelling to `treeman init`. Pass force=true to overwrite an existing file. Returns the chosen path and whether it was newly created.",
	}, initRepoTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "schema_install",
		Description: "Install schemas/treeman.schema.json by shelling to `treeman schema install`. Required for editor autocompletion against .treeman.yaml.",
	}, schemaInstallTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "worktree_create",
		Description: "Create a new git worktree under .worktrees/<branch> and dispatch postcreate hooks + prepare via the daemon. Shells to `treeman wt create`. Long-running. Returns the captured stdout/stderr and exit code.",
	}, worktreeCreateTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "worktree_delete",
		Description: "Tear down a worktree: predelete hooks → DB teardown → git worktree remove. Shells to `treeman wt delete`. Returns the captured stdout/stderr and exit code.",
	}, worktreeDeleteTool)

	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "daemon_control",
		Description: "Start or stop treemand by shelling to `treeman daemon start|stop`. Action must be one of: start, stop.",
	}, daemonControlTool)
}

// ─── prepare_run ──────────────────────────────────────────────────

type prepareIn struct {
	Worktree string `json:"worktree,omitempty" jsonschema:"defaults to cwd"`
	Repo     string `json:"repo,omitempty"`
}
type prepareOut struct {
	Outcomes []prepare.Outcome `json:"outcomes"`
}

func prepareTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in prepareIn) (*mcpsdk.CallToolResult, prepareOut, error) {
	outs, err := runPrepare(ctx, in.Worktree, in.Repo)
	if err != nil {
		return nil, prepareOut{}, err
	}
	return nil, prepareOut{Outcomes: outs}, nil
}

// ─── hook_run ─────────────────────────────────────────────────────

type hookIn struct {
	Phase    string `json:"phase" jsonschema:"precreate|postcreate|predelete|postdelete"`
	Worktree string `json:"worktree,omitempty"`
}
type hookOut struct {
	Phase   string            `json:"phase"`
	Outcome hooks.RunOutcome  `json:"outcome"`
}

func hookTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in hookIn) (*mcpsdk.CallToolResult, hookOut, error) {
	out, err := runHookPhase(ctx, in.Phase, in.Worktree)
	if err != nil {
		return nil, hookOut{}, err
	}
	return nil, hookOut{Phase: in.Phase, Outcome: out}, nil
}

// ─── config_write ─────────────────────────────────────────────────

type configWriteIn struct {
	Repo string `json:"repo,omitempty"`
	Body string `json:"body" jsonschema:"the full YAML body to write to .treeman.yaml"`
}
type configWriteOut struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
}

func configWriteTool(_ context.Context, _ *mcpsdk.CallToolRequest, in configWriteIn) (*mcpsdk.CallToolResult, configWriteOut, error) {
	if in.Body == "" {
		return nil, configWriteOut{}, fmt.Errorf("body is required")
	}
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, configWriteOut{}, err
	}
	// Validate by parsing before any write so a malformed body
	// never overwrites a working config.
	var parsed config.Config
	if err := yaml.Unmarshal([]byte(in.Body), &parsed); err != nil {
		return nil, configWriteOut{}, fmt.Errorf("yaml parse: %w", err)
	}
	target := filepath.Join(repoRoot, ".treeman.yaml")
	if err := atomicWrite(target, []byte(in.Body)); err != nil {
		return nil, configWriteOut{}, err
	}
	return nil, configWriteOut{Path: target, Bytes: len(in.Body)}, nil
}

// atomicWrite writes data to <path>.tmp then renames over <path> so
// readers never see a partial config.
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ─── shell-out helpers ────────────────────────────────────────────

type shellOut struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// runTreeman shells out to the treeman binary (same one currently
// executing when possible) with the given arg list. cwd controls
// the worktree the CLI sees; pass "" for the current process cwd.
func runTreeman(ctx context.Context, cwd string, args ...string) (shellOut, error) {
	bin, err := treemanBinary()
	if err != nil {
		return shellOut{}, err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return shellOut{}, err
	}
	outBytes, _ := readAll(stdout)
	errBytes, _ := readAll(stderr)
	waitErr := cmd.Wait()
	code := 0
	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if waitErr != nil {
		code = -1
	}
	return shellOut{ExitCode: code, Stdout: string(outBytes), Stderr: string(errBytes)}, nil
}

func treemanBinary() (string, error) {
	if p, err := os.Executable(); err == nil {
		return p, nil
	}
	return exec.LookPath("treeman")
}

func readAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	const max = 1 << 20 // 1 MiB cap so a runaway hook can't OOM the mcp server
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) >= max {
				buf = buf[:max]
				break
			}
		}
		if err != nil {
			break
		}
	}
	return buf, nil
}

// ─── init_repo / schema_install ───────────────────────────────────

type initIn struct {
	Repo  string `json:"repo,omitempty"`
	Force bool   `json:"force,omitempty"`
}

func initRepoTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in initIn) (*mcpsdk.CallToolResult, shellOut, error) {
	args := []string{"init", "--json"}
	if in.Force {
		args = append(args, "--force")
	}
	cwd, _ := resolveRepo(in.Repo)
	out, err := runTreeman(ctx, cwd, args...)
	return nil, out, err
}

type schemaInstallIn struct {
	Repo string `json:"repo,omitempty"`
}

func schemaInstallTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in schemaInstallIn) (*mcpsdk.CallToolResult, shellOut, error) {
	cwd, _ := resolveRepo(in.Repo)
	out, err := runTreeman(ctx, cwd, "schema", "install")
	return nil, out, err
}

// ─── worktree_create / delete ─────────────────────────────────────

type worktreeCreateIn struct {
	Branch  string `json:"branch" jsonschema:"branch name for the new worktree"`
	From    string `json:"from,omitempty" jsonschema:"base branch"`
	Path    string `json:"path,omitempty" jsonschema:"explicit worktree path"`
	Repo    string `json:"repo,omitempty"`
	NoFetch bool   `json:"no_fetch,omitempty"`
}

func worktreeCreateTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in worktreeCreateIn) (*mcpsdk.CallToolResult, shellOut, error) {
	if in.Branch == "" {
		return nil, shellOut{}, fmt.Errorf("branch is required")
	}
	args := []string{"wt", "create", in.Branch}
	if in.From != "" {
		args = append(args, "--from", in.From)
	}
	if in.Path != "" {
		args = append(args, "--path", in.Path)
	}
	if in.NoFetch {
		args = append(args, "--no-fetch")
	}
	cwd, _ := resolveRepo(in.Repo)
	out, err := runTreeman(ctx, cwd, args...)
	return nil, out, err
}

type worktreeDeleteIn struct {
	Name  string `json:"name" jsonschema:"slug, branch, or basename of the worktree to delete"`
	Repo  string `json:"repo,omitempty"`
	Force bool   `json:"force,omitempty"`
}

func worktreeDeleteTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in worktreeDeleteIn) (*mcpsdk.CallToolResult, shellOut, error) {
	if in.Name == "" {
		return nil, shellOut{}, fmt.Errorf("name is required")
	}
	args := []string{"wt", "delete", in.Name}
	if in.Force {
		args = append(args, "--force")
	}
	cwd, _ := resolveRepo(in.Repo)
	out, err := runTreeman(ctx, cwd, args...)
	return nil, out, err
}

// ─── daemon_control ───────────────────────────────────────────────

type daemonControlIn struct {
	Action string `json:"action" jsonschema:"start|stop"`
}

func daemonControlTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in daemonControlIn) (*mcpsdk.CallToolResult, shellOut, error) {
	if in.Action != "start" && in.Action != "stop" {
		return nil, shellOut{}, fmt.Errorf("action must be start or stop, got %q", in.Action)
	}
	out, err := runTreeman(ctx, "", "daemon", in.Action)
	return nil, out, err
}
