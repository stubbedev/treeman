package mcp

import (
	"context"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stubbedev/treeman/internal/rpc"
)

// registerSyncTools binds sync_now (write) + sync_status (read) onto
// the server. Wired from both registerReadTools and registerWriteTools
// in tools_read / tools_write so the surface stays one-line-per-tool
// in those files; the implementations and types live here.
func registerSyncReadTools(srv *mcpsdk.Server) {
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "sync_status",
		Description: "Report per-repo + per-worktree git sync state: last-fetch time, fetch failures, next-retry, ahead/behind, dirty flag, last skip reason (dirty|no_upstream|non_ff|detached_head|rebase_conflict). Use to answer \"why isn't my branch up to date?\".",
		Annotations: readOnlyAnno("Report sync status", true),
	}, syncStatusTool)
}

func registerSyncWriteTools(srv *mcpsdk.Server) {
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name:        "sync_now",
		Description: "Force an immediate `git fetch --all --prune` + advance (ff or rebase per config) without waiting for the auto-fetch tick. Bypasses the offline-mode backoff. path scopes the work: empty → all repos, repo root → that repo + worktrees, worktree path → that wt only. Returns same shape as sync_status.",
		Annotations: writeAnno("Sync now", false, true, true),
	}, syncNowTool)
}

// ─── sync_now ──────────────────────────────────────────────────────

type syncNowIn struct {
	Path string `json:"path,omitempty" jsonschema:"empty = all repos; repo root or worktree path narrows the scope"`
}

type syncNowOut struct {
	Repos  []rpc.SyncRepoStatus `json:"repos"`
	Errors []string             `json:"errors,omitempty"`
}

// syncNowTool relays to the daemon's MethodSyncNow handler. Treating
// the daemon as the source of truth keeps backoff bookkeeping and
// last-fetch state in one place — running fetch in-process from the
// MCP server would skip the State updates the auto-fetch loop depends on.
func syncNowTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in syncNowIn) (*mcpsdk.CallToolResult, syncNowOut, error) {
	resp, err := rpc.Call(ctx, rpc.Request{
		Method:  rpc.MethodSyncNow,
		SyncNow: &rpc.SyncNowArgs{Path: in.Path},
	})
	if err != nil {
		return nil, syncNowOut{}, fmt.Errorf("daemon: %w", err)
	}
	if resp.Kind == rpc.KindError {
		return nil, syncNowOut{}, fmt.Errorf("daemon: %s", resp.Message)
	}
	return nil, syncNowOut{Repos: resp.SyncedRepos, Errors: resp.SyncErrors}, nil
}

// ─── sync_status ───────────────────────────────────────────────────

type syncStatusIn struct {
	Repo string `json:"repo,omitempty" jsonschema:"empty = every registered repo; repo path narrows to one"`
}

type syncStatusOut struct {
	Repos []rpc.SyncRepoStatus `json:"repos"`
}

func syncStatusTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in syncStatusIn) (*mcpsdk.CallToolResult, syncStatusOut, error) {
	repoRoot := ""
	if in.Repo != "" {
		root, err := resolveRepo(in.Repo)
		if err != nil {
			return nil, syncStatusOut{}, err
		}
		repoRoot = root
	}
	resp, err := rpc.Call(ctx, rpc.Request{
		Method:     rpc.MethodSyncStatus,
		SyncStatus: &rpc.SyncStatusArgs{RepoPath: repoRoot},
	})
	if err != nil {
		return nil, syncStatusOut{}, fmt.Errorf("daemon: %w", err)
	}
	if resp.Kind == rpc.KindError {
		return nil, syncStatusOut{}, fmt.Errorf("daemon: %s", resp.Message)
	}
	return nil, syncStatusOut{Repos: resp.SyncedRepos}, nil
}
