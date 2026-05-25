package wt

import (
	"context"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
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
		out, err := hooks.RunHooks(ctx, trigger, actions, repoRoot, wtPath, sl.Value, env, true)
		hooks.PersistOutcome(ctx, st, repoID, wtID, trigger, started, time.Now().UnixMilli(), out)
		if err != nil {
			return err
		}
		sink.Info("%s: %d action(s) complete (logs in %s/.treeman-hooks/)",
			trigger, len(actions), wtPath)
		return nil
	}
	if err := runTrigger("on-create-before-engines", cfg.Hooks.OnCreateBeforeEngines); err != nil {
		return err
	}
	if skipPrepare || len(cfg.Databases) == 0 {
		return runTrigger("on-create-after-engines", cfg.Hooks.OnCreateAfterEngines)
	}
	outs, err := prepare.Run(ctx, cfg, wtPath, sl, st, repoID, wtID, env)
	if err != nil {
		sink.Warn("prepare failed: %v", err)
	}
	for _, o := range outs {
		sink.Info("prepare[%s] %s template=%s clones=%d",
			o.Engine, o.SourceDB, o.TemplateName, len(o.Clones))
	}
	return runTrigger("on-create-after-engines", cfg.Hooks.OnCreateAfterEngines)
}
