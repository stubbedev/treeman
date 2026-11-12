package daemon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/pkg/rpc"
)

type triggerSource string

const (
	triggerRPC              triggerSource = "rpc"
	triggerHeadChange       triggerSource = "head_change"
	triggerWorktreeAppeared triggerSource = "worktree_appeared"
	triggerFileChange       triggerSource = "file_change"
	triggerDaemonRestart    triggerSource = "daemon_restart"
	triggerCreateRetry      triggerSource = "create_retry"
)

type trigger struct {
	Source      triggerSource  `json:"source"`
	RequestedBy *rpc.Requester `json:"requested_by,omitempty"`
	// Attempt numbers an automatic create re-run (triggerCreateRetry):
	// 1 for the first. 0 for every other run.
	Attempt int `json:"attempt,omitempty"`
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

// triggerAttempt returns the automatic re-run number of the run ctx
// belongs to; 0 when it is not one.
func triggerAttempt(ctx context.Context) int {
	t, _ := ctx.Value(triggerKey{}).(trigger)
	return t.Attempt
}

// triggerOf returns the trigger recorded in the payload of e, an event
// written with triggerPayload; the zero trigger when e carries none.
func triggerOf(e store.Event) (trigger, error) {
	if e.PayloadJSON == "" {
		return trigger{}, nil
	}
	var p struct {
		Trigger trigger `json:"trigger"`
	}
	if err := json.Unmarshal([]byte(e.PayloadJSON), &p); err != nil {
		return trigger{}, fmt.Errorf("decode the trigger of event %d: %w", e.ID, err)
	}
	return p.Trigger, nil
}

func triggerPayload(ctx context.Context) map[string]any {
	t, ok := ctx.Value(triggerKey{}).(trigger)
	if !ok {
		return nil
	}
	return map[string]any{"trigger": t}
}
