package rpc

import (
	"encoding/json"
	"testing"
)

func TestRequestRoundtripStatus(t *testing.T) {
	req := Request{Method: MethodStatus}
	b, err := json.Marshal(&req)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"method":"status"}` {
		t.Errorf("status payload: %s", string(b))
	}
	var got Request
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Method != MethodStatus {
		t.Errorf("method: %s", got.Method)
	}
}

func TestRequestRoundtripRunPlan(t *testing.T) {
	req := Request{
		Method: MethodRunPlan,
		RunPlan: &RunPlanArgs{
			RunID: "abcd1234",
			Wait:  true,
			Groups: [][]Task{
				{{
					Type:         TaskWorktreeFinalize,
					RepoPath:     "/repos/foo",
					WorktreePath: "/repos/foo/.worktrees/x",
					InheritedEnv: map[string]string{"PATH": "/usr/bin:/bin"},
				}},
				{
					{Type: TaskPrepare, WorktreePath: "/repos/foo/.worktrees/x"},
					{
						Type:         TaskWorktreeTeardown,
						WorktreePath: "/repos/foo/.worktrees/y",
						Params:       map[string]string{"force": "1"},
					},
				},
			},
		},
	}
	b, err := json.Marshal(&req)
	if err != nil {
		t.Fatal(err)
	}
	var got Request
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Method != MethodRunPlan || got.RunPlan == nil {
		t.Fatalf("method/args: %s %+v", got.Method, got.RunPlan)
	}
	if !got.RunPlan.Wait || got.RunPlan.RunID != "abcd1234" {
		t.Errorf("wait/run_id: %v %s", got.RunPlan.Wait, got.RunPlan.RunID)
	}
	if len(got.RunPlan.Groups) != 2 || len(got.RunPlan.Groups[1]) != 2 {
		t.Fatalf("groups shape: %+v", got.RunPlan.Groups)
	}
	g0 := got.RunPlan.Groups[0][0]
	if g0.Type != TaskWorktreeFinalize || g0.InheritedEnv["PATH"] != "/usr/bin:/bin" {
		t.Errorf("group0 task: %+v", g0)
	}
	if got.RunPlan.Groups[1][1].Params["force"] != "1" {
		t.Errorf("force param lost: %+v", got.RunPlan.Groups[1][1])
	}
}

// TestUnknownMethodDecodes confirms decode no longer rejects unknown
// methods — that validation moved to the daemon's Dispatch switch (which
// returns an "unknown method" error response). Decode just sets Method
// and leaves every args pointer nil.
func TestUnknownMethodDecodes(t *testing.T) {
	var got Request
	if err := json.Unmarshal([]byte(`{"method":"nope"}`), &got); err != nil {
		t.Fatalf("decode should not error on unknown method: %v", err)
	}
	if got.Method != "nope" {
		t.Errorf("method: %q", got.Method)
	}
	if got.RunPlan != nil || got.RepoRegister != nil {
		t.Errorf("no args pointer should be set for an unknown method")
	}
}
