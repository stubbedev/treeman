package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/migrations/framework"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/ui"
)

// DoctorCmd — `treeman doctor` runs a short health probe over the
// pieces the CLI depends on and prints a fixable diff.
//
// Probes (each prints one line, colored by outcome):
//
//   - daemon reachability + version
//   - .treeman.yaml presence + parse-ability in the discovered repo
//   - schemas/treeman.schema.json presence (yaml-language-server hint)
//   - migration framework detection (non-empty)
//   - git worktree list ↔ SQLite registry consistency
//
// Non-zero exit when any blocking probe fails so CI can gate.
func DoctorCmd() *cli.Command {
	return &cli.Command{
		Name:  "doctor",
		Usage: "health-check the local treeman setup",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "json", Usage: "emit one JSON line per check"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			results := RunDoctorChecks(ctx)

			if c.Bool("json") {
				return jsonStream(results)
			}
			failed := 0
			for _, r := range results {
				switch r.Status {
				case "ok":
					ui.Success("%s — %s", ui.Bold(r.Name), r.Detail)
				case "warn":
					ui.Warn("%s — %s", ui.Bold(r.Name), r.Detail)
					if r.Hint != "" {
						ui.Hint("%s", r.Hint)
					}
				case "fail":
					failed++
					ui.Error("%s — %s", ui.Bold(r.Name), r.Detail)
					if r.Hint != "" {
						ui.Hint("%s", r.Hint)
					}
				case "skip":
					ui.Info("%s — %s", ui.Bold(r.Name), r.Detail)
				}
			}
			if failed > 0 {
				return fmt.Errorf("%d check(s) failed", failed)
			}
			return nil
		},
	}
}

// doctorResult is the shape both the human renderer and the JSON
// emitter consume. Status ∈ {ok, warn, fail, skip}; Hint is a
// remediation line shown beneath warn/fail rows.
type doctorResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

// DoctorResult mirrors the internal doctorResult for callers outside
// the cmd package (e.g. the MCP server).
type DoctorResult = doctorResult

// RunDoctorChecks executes all health probes and returns the result
// slice. Shared by the CLI renderer and the MCP `doctor_run` tool.
func RunDoctorChecks(ctx context.Context) []DoctorResult {
	repoRoot, _ := resolveRepo("")
	results := []doctorResult{}
	results = append(results, checkDaemon(ctx))
	if repoRoot != "" {
		results = append(results, checkConfig(repoRoot))
		results = append(results, checkSchema(repoRoot))
		results = append(results, checkFrameworks(repoRoot))
		results = append(results, checkRegistry(ctx, repoRoot))
	} else {
		results = append(results, doctorResult{
			Name:   "repo",
			Status: "skip",
			Detail: "not inside a git repo — repo-scoped checks skipped",
		})
	}
	return results
}

func checkDaemon(ctx context.Context) doctorResult {
	resp, err := rpc.Call(ctx, rpc.Request{Method: rpc.MethodStatus})
	if err != nil {
		return doctorResult{
			Name:   "daemon",
			Status: "warn",
			Detail: "not reachable",
			Hint:   "start it with: treeman daemon start (or `treeman daemon install` to auto-launch)",
		}
	}
	if resp.Kind == rpc.KindError {
		return doctorResult{
			Name:   "daemon",
			Status: "fail",
			Detail: resp.Message,
		}
	}
	return doctorResult{
		Name:   "daemon",
		Status: "ok",
		Detail: fmt.Sprintf("treemand %s pid=%d watchers=%d", resp.DaemonVersion, resp.Pid, resp.WatcherCount),
	}
}

func checkConfig(repoRoot string) doctorResult {
	p := filepath.Join(repoRoot, ".treeman.yaml")
	if _, err := os.Stat(p); err != nil {
		return doctorResult{
			Name:   "config",
			Status: "warn",
			Detail: ".treeman.yaml not found in " + repoRoot,
			Hint:   "scaffold one with: treeman init",
		}
	}
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return doctorResult{
			Name:   "config",
			Status: "fail",
			Detail: err.Error(),
		}
	}
	return doctorResult{
		Name:   "config",
		Status: "ok",
		Detail: fmt.Sprintf("loaded %s (%d databases)", p, len(cfg.Databases)),
	}
}

// checkSchema honors any of three install modes:
//   - repo-local file       (treeman schema install)
//   - user-global file      (treeman schema install --global)
//   - upstream URL modeline (treeman schema install --url)
//
// The yaml-language-server modeline at the top of `.treeman.yaml`
// is the source of truth; doctor resolves whatever it points at.
// When no modeline is set, doctor falls back to probing the
// repo-local and global file locations so editors still get
// hinting even if the modeline was hand-removed.
func checkSchema(repoRoot string) doctorResult {
	ref := ReadSchemaModeline(repoRoot)
	if ref != "" {
		if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
			return doctorResult{
				Name:   "schema",
				Status: "ok",
				Detail: "modeline → " + ref,
			}
		}
		p := ref
		if !filepath.IsAbs(p) {
			p = filepath.Join(repoRoot, p)
		}
		if _, err := os.Stat(p); err == nil {
			return doctorResult{
				Name:   "schema",
				Status: "ok",
				Detail: "modeline → " + p,
			}
		}
		return doctorResult{
			Name:   "schema",
			Status: "warn",
			Detail: "modeline points to missing file: " + p,
			Hint:   "regenerate with: treeman schema install [--global|--url]",
		}
	}

	repoPath := filepath.Join(repoRoot, "schemas", "treeman.schema.json")
	if _, err := os.Stat(repoPath); err == nil {
		return doctorResult{
			Name:   "schema",
			Status: "warn",
			Detail: repoPath + " (no modeline in .treeman.yaml)",
			Hint:   "wire it up: treeman schema install",
		}
	}
	if gp, err := GlobalSchemaPath(); err == nil {
		if _, err := os.Stat(gp); err == nil {
			return doctorResult{
				Name:   "schema",
				Status: "warn",
				Detail: gp + " (no modeline in .treeman.yaml)",
				Hint:   "wire it up: treeman schema install --global",
			}
		}
	}
	return doctorResult{
		Name:   "schema",
		Status: "warn",
		Detail: "no schema installed",
		Hint:   "install for editor autocompletion: treeman schema install [--global|--url]",
	}
}

func checkFrameworks(repoRoot string) doctorResult {
	detected := framework.DefaultRegistry().DetectAll(repoRoot)
	if len(detected) == 0 {
		return doctorResult{
			Name:   "framework",
			Status: "warn",
			Detail: "no migration framework detected",
			Hint:   "supported frameworks: laravel, rails, django, golang-migrate, goose, sqlx-cli, ... (see `treeman fw detect`)",
		}
	}
	names := make([]string, 0, len(detected))
	for _, s := range detected {
		names = append(names, s.Name)
	}
	return doctorResult{
		Name:   "framework",
		Status: "ok",
		Detail: strings.Join(names, ", "),
	}
}

// checkRegistry compares `git worktree list --porcelain` against the
// SQLite registry's active worktrees for this repo. Drifts surface
// as a warn so the user can run `wt register` (or `unregister`) to
// reconcile.
func checkRegistry(ctx context.Context, repoRoot string) doctorResult {
	gitPaths, err := gitWorktreePaths(ctx, repoRoot)
	if err != nil {
		return doctorResult{
			Name:   "registry",
			Status: "fail",
			Detail: err.Error(),
		}
	}
	dbPath, _ := store.DefaultDBPath()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return doctorResult{
			Name:   "registry",
			Status: "fail",
			Detail: err.Error(),
		}
	}
	defer st.Close()
	rows, err := st.DB.QueryContext(ctx, `SELECT w.path FROM worktrees w
		JOIN repos r ON r.id = w.repo_id WHERE r.path = ? AND w.deleted_at IS NULL`, repoRoot)
	if err != nil {
		return doctorResult{
			Name:   "registry",
			Status: "fail",
			Detail: err.Error(),
		}
	}
	defer rows.Close()
	dbPaths := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err == nil {
			dbPaths[p] = true
		}
	}

	var onlyInGit, onlyInDB []string
	gitSet := map[string]bool{}
	for _, p := range gitPaths {
		gitSet[p] = true
		if !dbPaths[p] {
			onlyInGit = append(onlyInGit, p)
		}
	}
	for p := range dbPaths {
		if !gitSet[p] {
			onlyInDB = append(onlyInDB, p)
		}
	}

	if len(onlyInGit) == 0 && len(onlyInDB) == 0 {
		return doctorResult{
			Name:   "registry",
			Status: "ok",
			Detail: fmt.Sprintf("%d worktree(s) synced with git", len(dbPaths)),
		}
	}
	parts := []string{}
	if len(onlyInGit) > 0 {
		parts = append(parts, fmt.Sprintf("%d in git but not registered: %s", len(onlyInGit), strings.Join(onlyInGit, ", ")))
	}
	if len(onlyInDB) > 0 {
		parts = append(parts, fmt.Sprintf("%d registered but missing from git: %s", len(onlyInDB), strings.Join(onlyInDB, ", ")))
	}
	return doctorResult{
		Name:   "registry",
		Status: "warn",
		Detail: strings.Join(parts, "; "),
		Hint:   "reconcile via `treeman wt register <path>` or `treeman wt unregister <path>`",
	}
}

// gitWorktreePaths returns the absolute paths reported by `git
// worktree list --porcelain`. The MAIN repo itself is excluded —
// only linked worktrees are returned, matching what the registry
// tracks.
func gitWorktreePaths(ctx context.Context, repoRoot string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w", err)
	}
	var paths []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		p := strings.TrimPrefix(line, "worktree ")
		if p == repoRoot {
			continue
		}
		paths = append(paths, p)
	}
	return paths, nil
}
