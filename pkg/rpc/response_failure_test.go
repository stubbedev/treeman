package rpc

import "testing"

func TestResponseFailure(t *testing.T) {
	tests := []struct {
		name string
		resp Response
		want string
	}{
		{"queued plan", Response{Kind: KindPlanQueued}, ""},
		{"all tasks succeeded", Response{Kind: KindPlanResult, TaskResults: []TaskResult{{Type: "a", OK: true}}}, ""},
		{"daemon error", Response{Kind: KindError, Message: "boom"}, "boom"},
		{
			"in-process run with a failed task",
			Response{Kind: KindPlanResult, TaskResults: []TaskResult{
				{Type: "a", OK: true},
				{Type: TaskWorktreeFinalize, Message: "hook exited 1"},
				{Type: "c", Message: "later"},
			}},
			"hook exited 1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.resp.Failure()
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != tt.want {
				t.Errorf("Failure() = %q, want %q", got, tt.want)
			}
		})
	}
}
