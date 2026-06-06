package wt

import (
	"context"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
)

// RunLocalFinalize executes the setup + prepare tail in the calling
// process. Shared by wt create's last-resort foreground fallback
// (the detached child path) and by `wt finalize --local`.
//
// The daemon has its own canonical FinalizeWorktree; this is the
// "daemon-less" mirror that runs when the daemon is unreachable.
func RunLocalFinalize(
	ctx context.Context,
	cfg *config.Config,
	repoRoot, wtPath string,
	sl slug.Slug,
	isMain bool,
	st *store.Store,
	repoID, wtID int64,
	env map[string]string,
	skipPrepare bool,
	sink Sink,
) error {
	if sink == nil {
		sink = NoopSink{}
	}
	runTrigger := func(trigger string, actions []config.Action) error {
		if len(actions) == 0 {
			return nil
		}
		started := hooks.EmitHookStart(ctx, st, repoID, wtID, trigger, len(actions))
		out, err := hooks.RunHooks(ctx, trigger, actions, repoRoot, wtPath, sl.Value, isMain, env, true)
		hooks.PersistOutcome(ctx, st, repoID, wtID, trigger, started, time.Now().UnixMilli(), out)
		if err != nil {
			return err
		}
		sink.Info("%s: %d action(s) complete (logs in %s/.treeman-hooks/)",
			trigger, len(actions), wtPath)
		return nil
	}
	// Materialize links/copies + render patches before hooks fire (a
	// before-engines hook may read the copied/patched `.env`). The daemon
	// path does this in FinalizeWorktree; this mirror covers the detached
	// child / `wt finalize --local` fallback. Skipped for the main
	// worktree (src == dst), matching the daemon.
	if !isMain {
		if err := BringInFiles(repoRoot, wtPath, cfg.Worktrees.Links, "link", sink); err != nil {
			return err
		}
		if err := BringInFiles(repoRoot, wtPath, cfg.Worktrees.Copies, "copy", sink); err != nil {
			return err
		}
		portMap, _ := st.LoadWorktreePorts(ctx, wtID)
		tplCtx := template.FromSlug(sl).WithPorts(portMap)
		if err := applyPatches(ctx, cfg.Patches, wtPath, tplCtx, sink); err != nil {
			return err
		}
	}
	if err := runTrigger("create-before-engines", cfg.Hooks.OnCreateBeforeEngines); err != nil {
		return err
	}
	if skipPrepare || len(cfg.Databases) == 0 {
		return runTrigger("create-after-engines", cfg.Hooks.OnCreateAfterEngines)
	}
	outs, err := prepare.Run(ctx, cfg, wtPath, sl, st, repoID, wtID, env)
	if err != nil {
		sink.Warn("prepare failed: %v", err)
	}
	for _, o := range outs {
		sink.Info("prepare[%s] %s template=%s clones=%d",
			o.Engine, o.SourceDB, o.TemplateName, len(o.Clones))
	}
	return runTrigger("create-after-engines", cfg.Hooks.OnCreateAfterEngines)
}
