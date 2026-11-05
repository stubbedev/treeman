package daemon

import (
	"context"

	"github.com/stubbedev/treeman/pkg/rpc"
)

type triggerSource string

const (
	triggerRPC              triggerSource = "rpc"
	triggerHeadChange       triggerSource = "head_change"
	triggerWorktreeAppeared triggerSource = "worktree_appeared"
	triggerFileChange       triggerSource = "file_change"
	triggerDaemonRestart    triggerSource = "daemon_restart"
)

type trigger struct {
	Source      triggerSource  `json:"source"`
	RequestedBy *rpc.Requester `json:"requested_by,omitempty"`
}

type triggerKey struct{}

func withTrigger(ctx context.Context, t trigger) context.Context {
	return context.WithValue(ctx, triggerKey{}, t)
}

func inheritTrigger(dst, src context.Context) context.Context {
	if t, ok := src.Value(triggerKey{}).(trigger); ok {
		return withTrigger(dst, t)
	}
	return dst
}

func triggerPayload(ctx context.Context) map[string]any {
	t, ok := ctx.Value(triggerKey{}).(trigger)
	if !ok {
		return nil
	}
	return map[string]any{"trigger": t}
}
