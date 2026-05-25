package mcp

import (
	"context"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerPrompts binds the canned multi-step workflows treeman
// exposes as MCP prompts. Prompts encode the *order* of tool calls
// for tasks where there's a known-good recipe — diagnosing a failed
// prepare, scaffolding a .treeman.yaml, hunting stale cache entries.
// Without them every agent client reinvents the chain.
//
// Each handler returns a single user-role message that briefs the
// model on the goal, lists which tools to call in which order, and
// names the artifacts to inspect. The agent then drives the tool
// calls; treeman does not pre-execute them.
func registerPrompts(srv *mcpsdk.Server) {
	srv.AddPrompt(&mcpsdk.Prompt{
		Name:        "diagnose-prepare-failure",
		Title:       "Diagnose a failed prepare",
		Description: "Walks through the chain of tool calls needed to localize a failing prepare: pull recent error events, identify the engine that failed, check engine reachability, inspect the cached snapshot, and surface the actual root-cause line. Pass worktree to scope to one worktree, or run_id to scope to one prepare invocation.",
		Arguments: []*mcpsdk.PromptArgument{
			{Name: "worktree", Description: "slug, branch, or basename to scope the investigation", Required: false},
			{Name: "run_id", Description: "8-char correlation id; preferred when known (much narrower scope than worktree)", Required: false},
		},
	}, diagnosePreparePrompt)

	srv.AddPrompt(&mcpsdk.Prompt{
		Name:        "scaffold-from-framework",
		Title:       "Scaffold .treeman.yaml for the detected framework",
		Description: "Detects the framework in the current repo, drafts a .treeman.yaml from the matching scaffold template, validates it, and writes it after the user reviews the diff. Stops short of executing prepare so the user controls the first cold build.",
		Arguments: []*mcpsdk.PromptArgument{
			{Name: "repo", Description: "absolute path to the repo root; defaults to cwd's repo", Required: false},
		},
	}, scaffoldFromFrameworkPrompt)

	srv.AddPrompt(&mcpsdk.Prompt{
		Name:        "cache-cleanup",
		Title:       "Hunt orphan snapshots and drop them",
		Description: "Lists every cached snapshot for the current repo, probes each one to see whether the engine-side template still exists, and drops the orphans (SQLite rows whose template was deleted out-of-band on the engine). The agent confirms before each drop.",
		Arguments: []*mcpsdk.PromptArgument{
			{Name: "repo", Description: "absolute path to the repo root; defaults to cwd's repo", Required: false},
		},
	}, cacheCleanupPrompt)
}

// userMsg wraps a string in the one-user-message-result shape every
// handler in this file returns. Keeps the per-prompt bodies short
// and focused on instructions.
func userMsg(text string) *mcpsdk.GetPromptResult {
	return &mcpsdk.GetPromptResult{
		Messages: []*mcpsdk.PromptMessage{{
			Role:    "user",
			Content: &mcpsdk.TextContent{Text: text},
		}},
	}
}

func diagnosePreparePrompt(_ context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
	wt := req.Params.Arguments["worktree"]
	runID := req.Params.Arguments["run_id"]

	scope := "scope: most-recent prepare across every worktree"
	switch {
	case runID != "":
		scope = fmt.Sprintf("scope: run_id=%s", runID)
	case wt != "":
		scope = fmt.Sprintf("scope: worktree=%s", wt)
	}

	text := fmt.Sprintf(`Diagnose the most recent failed prepare. %s

Execute these tool calls in order. Stop as soon as you can identify the failing step + root cause.

1. logs_query — fetch recent errors.
   • levels=["error"]
   • event_types=["prepare_done","wt_finalize","clone_restore_fail","fanout_done","prepare_unsupported_engine"]
   %s
   • limit=20
   Identify the most-recent failure row's repo, worktree, engine, and event_type.

2. If a run_id is visible in the failure payload but wasn't supplied as an argument, RE-RUN logs_query with that run_id and event_types=["prepare_start","prepare_phase","prepare_done","fanout_start","fanout_done","clone_restore_fail","snapshot_cache_hit"] to reconstruct the full timeline of that prepare invocation.

3. engine_status — confirm the affected engine is reachable and responsive. If unreachable, surface that as the root cause and stop.

4. snapshot_inspect — when the failure involved a snapshot template (look for "snapshot" in the message or fingerprint in the payload). Verify the engine-side template_exists matches the SQLite row. A row whose template_exists=false is an orphan — recommend snapshot_drop or cache-cleanup.

5. If the failure was inside the user's migrate/seed command, fetch the run_id and call logs_hooks for the worktree, plus hook_log_read for the specific phase/group_idx referenced in the failing hook_run.

Report: failing step (phase), engine, the actual error line, and one concrete next-action.`, scope, runIDOrWorktreeLine(runID, wt))

	return userMsg(text), nil
}

// runIDOrWorktreeLine emits the right logs_query argument depending
// on which scoping field the user supplied. Keeps the prompt body
// from carrying conditional logic the model has to read past.
func runIDOrWorktreeLine(runID, worktree string) string {
	switch {
	case runID != "":
		return "• run_id=\"" + runID + "\""
	case worktree != "":
		return "• worktree=\"" + worktree + "\""
	default:
		return "• (no scope filter — pull cross-worktree errors)"
	}
}

func scaffoldFromFrameworkPrompt(_ context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
	repo := req.Params.Arguments["repo"]
	repoArg := ""
	if repo != "" {
		repoArg = "repo=\"" + repo + "\", "
	}

	text := fmt.Sprintf(`Scaffold a .treeman.yaml for this repo from the detected framework template. Do NOT run prepare — the user should approve the file first.

Execute these tool calls in order:

1. fw_detect (%s) — list every framework treeman recognises in the repo. If none detected, STOP and tell the user there's no template to scaffold from.

2. init_repo (%sforce=false) — generate the scaffold. If the call errors with "file exists", call config_get first, then init_repo with force=true ONLY after the user confirms they want to overwrite.

3. config_validate — verify the scaffolded file parses. If validation fails, surface the parse error verbatim and stop.

4. config_get (resolved=true) — show the user what treeman will actually execute against (after env-var substitution and defaults).

Report: the detected framework(s), the path written, the resolved databases[] block, and one short paragraph telling the user what to verify before running 'treeman prepare'.`, repoArg, repoArg)

	return userMsg(text), nil
}

func cacheCleanupPrompt(_ context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
	repo := req.Params.Arguments["repo"]
	repoArg := ""
	if repo != "" {
		repoArg = "repo=\"" + repo + "\""
	}

	text := fmt.Sprintf(`Find and drop orphan snapshots for this repo. An orphan is a SQLite snapshot row whose engine-side template was deleted out-of-band (e.g. someone ran 'DROP DATABASE' directly). They're invisible to snapshots_purge because purge drops every snapshot, not just stale ones.

Execute these tool calls in order:

1. snapshots_list (%s) — list every cached snapshot. Record each fingerprint.

2. For EACH fingerprint, call snapshot_inspect with that fingerprint. Collect the ones where template_exists=false. Those are the orphans.

3. If no orphans, report "0 orphans found" and stop.

4. Show the user the orphan list (fingerprint, engine, template_name, source_db, created_at) and ASK FOR CONFIRMATION before dropping anything.

5. After confirmation, for each orphan call snapshot_drop with that fingerprint. Report per-engine success/failure counts.

NEVER call snapshots_purge from this flow — that nukes valid templates too.`, repoArg)

	return userMsg(text), nil
}
