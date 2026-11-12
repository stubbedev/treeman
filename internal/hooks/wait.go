package hooks

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/stubbedev/treeman/internal/progress"
	"github.com/stubbedev/treeman/pkg/safego"
)

const logGrowthPoll = 15 * time.Second

// waitGroups blocks until every spawned group exits, recording each
// exit code on groups and calling afterExit with its index. While the
// groups run, output appended to any group's log counts as progress on
// ctx, as does each group's exit, so a long but visibly working hook
// is never mistaken for a stalled one.
func waitGroups(ctx context.Context, cmds []*exec.Cmd, groups []GroupOutcome, afterExit func(idx int)) {
	stop := reportWhileLogsGrow(ctx, groups)
	defer stop()
	for idx, c := range cmds {
		groups[idx].ExitCode = exitCodeOf(c.Wait())
		afterExit(idx)
		progress.Report(ctx)
	}
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode()
	}
	return -1
}

func reportWhileLogsGrow(ctx context.Context, groups []GroupOutcome) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	sizes := make([]int64, len(groups))
	safego.Go("hook:log-growth", "", func() {
		defer close(finished)
		ticker := time.NewTicker(logGrowthPoll)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if logsGrew(groups, sizes) {
					progress.Report(ctx)
				}
			}
		}
	})
	return func() {
		close(done)
		<-finished
	}
}

func logsGrew(groups []GroupOutcome, sizes []int64) bool {
	grew := false
	for i, g := range groups {
		if g.LogPath == "" {
			continue
		}
		info, err := os.Stat(g.LogPath)
		if err != nil {
			slog.Warn("hook log unreadable while watching for progress", "path", g.LogPath, "err", err)
			continue
		}
		if info.Size() > sizes[i] {
			sizes[i] = info.Size()
			grew = true
		}
	}
	return grew
}
