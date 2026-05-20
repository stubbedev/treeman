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

func TestRequestRoundtripWorktreeFinalize(t *testing.T) {
	req := Request{
		Method: MethodWorktreeFinalize,
		WorktreeFinalize: &WorktreeFinalizeArgs{
			RepoPath:     "/repos/foo",
			WorktreePath: "/repos/foo/.worktrees/x",
			InheritedEnv: map[string]string{"PATH": "/usr/bin:/bin"},
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
	if got.Method != MethodWorktreeFinalize {
		t.Errorf("method: %s", got.Method)
	}
	if got.WorktreeFinalize == nil {
		t.Fatal("args nil")
	}
	if got.WorktreeFinalize.WorktreePath != req.WorktreeFinalize.WorktreePath {
		t.Errorf("worktree_path: %s", got.WorktreeFinalize.WorktreePath)
	}
	if got.WorktreeFinalize.InheritedEnv["PATH"] != "/usr/bin:/bin" {
		t.Errorf("env: %v", got.WorktreeFinalize.InheritedEnv)
	}
}

func TestRequestRoundtripWorktreeTeardown(t *testing.T) {
	req := Request{
		Method: MethodWorktreeTeardown,
		WorktreeTeardown: &WorktreeTeardownArgs{
			RepoPath:     "/repos/foo",
			WorktreePath: "/repos/foo/.worktrees/x",
			Force:        true,
			InheritedEnv: map[string]string{},
		},
	}
	b, _ := json.Marshal(&req)
	var got Request
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !got.WorktreeTeardown.Force {
		t.Error("force should be true")
	}
}

func TestUnknownMethodErrors(t *testing.T) {
	var got Request
	err := json.Unmarshal([]byte(`{"method":"nope"}`), &got)
	if err == nil {
		t.Fatal("want error for unknown method")
	}
}
