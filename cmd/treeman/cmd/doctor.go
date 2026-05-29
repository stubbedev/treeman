package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/migrations/framework"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/schema"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/ui"
	"github.com/stubbedev/treeman/internal/wtreg"
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
			&cli.BoolFlag{
				Name:  "fix",
				Usage: "auto-apply remediations for `schema` (install) and `registry` (repair) checks; re-runs the probe so the printed result reflects the post-fix state",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			results := RunDoctorChecks(ctx)

			if c.Bool("fix") {
				results = applyDoctorFixes(ctx, results)
			}

			if c.Bool("json") {
				return jsonStream(results)
			}
			failed := 0
			warned := 0
			fixable := 0
			for _, r := range results {
				switch r.Status {
				case "ok":
					ui.Success("%s — %s", ui.Bold(r.Name), r.Detail)
				case "warn":
					warned++
					if isFixable(r.Name) {
						fixable++
					}
					ui.Warn("%s — %s", ui.Bold(r.Name), r.Detail)
					if r.Hint != "" {
						ui.Hint("%s", r.Hint)
					}
				case "fail":
					failed++
					if isFixable(r.Name) {
						fixable++
					}
					ui.Error("%s — %s", ui.Bold(r.Name), r.Detail)
					if r.Hint != "" {
						ui.Hint("%s", r.Hint)
					}
				case "skip":
					ui.Info("%s — %s", ui.Bold(r.Name), r.Detail)
				}
			}
			// One-line summary so the user knows the verdict + next step
			// without re-reading the per-check list.
			_, _ = fmt.Fprintln(ui.Out)
			switch {
			case failed > 0:
				ui.Error("%d check(s) failed, %d warning(s)", failed, warned)
				if fixable > 0 && !c.Bool("fix") {
					ui.Hint("auto-fixable: re-run with `--fix`")
				}
				return fmt.Errorf("%d check(s) failed", failed)
			case warned > 0:
				ui.Warn("%d warning(s)", warned)
				if fixable > 0 && !c.Bool("fix") {
					ui.Hint("auto-fixable: re-run with `--fix`")
				}
			default:
				ui.Success("all checks passed")
			}
			return nil
		},
	}
}

// isFixable reports whether `applyDoctorFixes` knows how to remediate
// a check by that name. Keep the set in sync with the switch in
// applyDoctorFixes so the TLDR doesn't over-promise.
func isFixable(name string) bool {
	switch name {
	case "schema", "registry":
		return true
	}
	return false
}

// applyDoctorFixes walks the results and runs the auto-fix path for
// each warn/fail check that has one wired up. After fixing, the
// affected check is re-run so the printed result reflects the new
// state. Fixes that aren't auto-applicable (e.g. `config` warn
// requires `treeman init` against a non-empty cwd) are left as-is.
func applyDoctorFixes(ctx context.Context, results []DoctorResult) []DoctorResult {
	repoRoot, _ := resolveRepo("")
	for i, r := range results {
		if r.Status == "ok" || r.Status == "skip" {
			continue
		}
		switch r.Name {
		case "schema":
			if repoRoot == "" {
				continue
			}
			if _, _, err := schema.Install(repoRoot, schema.TargetRepo); err != nil {
				results[i].Hint = fmt.Sprintf("fix failed: %v", err)
				continue
			}
			results[i] = checkSchema(repoRoot)
		case "registry":
			if repoRoot == "" {
				continue
			}
			st, err := openDefaultStore(ctx)
			if err != nil {
				results[i].Hint = fmt.Sprintf("fix failed: %v", err)
				continue
			}
			if _, err := wtreg.Repair(ctx, st, repoRoot, detectBranchOfWorktree); err != nil {
				_ = st.Close()
				results[i].Hint = fmt.Sprintf("fix failed: %v", err)
				continue
			}
			_ = st.Close()
			results[i] = checkRegistry(ctx, repoRoot)
		}
	}
	return results
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
	ref := schema.ReadModeline(repoRoot)
	modelineDetail := ""
	if ref != "" {
		ok, detail := schema.ProbeRef(repoRoot, ref)
		if ok {
			return doctorResult{
				Name:   "schema",
				Status: "ok",
				Detail: "modeline → " + detail,
			}
		}
		modelineDetail = detail
	}

	if gp, err := schema.GlobalPath(); err == nil {
		if _, err := os.Stat(gp); err == nil {
			return doctorResult{
				Name:   "schema",
				Status: "ok",
				Detail: "global install → " + gp,
			}
		}
	}
	repoPath := filepath.Join(repoRoot, "schemas", "treeman.schema.json")
	if _, err := os.Stat(repoPath); err == nil {
		return doctorResult{
			Name:   "schema",
			Status: "ok",
			Detail: "repo file → " + repoPath,
		}
	}
	if modelineDetail != "" {
		return doctorResult{
			Name:   "schema",
			Status: "warn",
			Detail: "modeline unresolved: " + modelineDetail,
			Hint:   "regenerate with: treeman schema install [--global|--url]",
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
	cfg, _ := resolve.LoadResolved(repoRoot)
	detected := framework.RegistryFor(&cfg).DetectAll(repoRoot)
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
	defer func() { _ = st.Close() }()
	rows, err := st.DB.QueryContext(ctx, `SELECT w.path FROM worktrees w
		JOIN repos r ON r.id = w.repo_id WHERE r.path = ? COLLATE NOCASE AND w.deleted_at IS NULL`, repoRoot)
	if err != nil {
		return doctorResult{
			Name:   "registry",
			Status: "fail",
			Detail: err.Error(),
		}
	}
	defer func() { _ = rows.Close() }()
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

// gitWorktreePaths is a thin shim over wtreg.GitWorktreePaths kept
// so existing call sites in this file don't need an import churn.
func gitWorktreePaths(ctx context.Context, repoRoot string) ([]string, error) {
	return wtreg.GitWorktreePaths(ctx, repoRoot)
}
