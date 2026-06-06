package mcp

import (
	"context"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// promptSpec pairs a prompt definition with its handler. Defined as
// a slice rather than inline AddPrompt calls so the prompts_list tool
// can iterate the same source-of-truth registry.
type promptSpec struct {
	def     *mcpsdk.Prompt
	handler mcpsdk.PromptHandler
	// WhenToUse is the trigger phrase agents should match on. Surfaced
	// only via the prompts_list tool (the MCP Prompt protocol type
	// has no equivalent field) — keeps clients with weak prompt UI
	// from missing the natural-language activation cue.
	WhenToUse string
}

// allPrompts is the single source of truth for prompts. registerPrompts
// iterates it to wire AddPrompt calls; promptsListTool returns it as
// JSON for discoverability on clients that hide MCP prompts.
var allPrompts = []promptSpec{
	{
		def: &mcpsdk.Prompt{
			Name:        "diagnose-prepare-failure",
			Title:       "Diagnose a failed prepare",
			Description: "Walk through the chain of tool calls needed to localize a failing prepare: pull recent error events, identify the engine that failed, check engine reachability, inspect the cached snapshot, surface the actual root-cause line. Pass worktree to scope to one worktree, or run_id to scope to one prepare invocation.",
			Arguments: []*mcpsdk.PromptArgument{
				{Name: "worktree", Description: "slug, branch, or basename to scope the investigation", Required: false},
				{
					Name:        "run_id",
					Description: "8-char correlation id; preferred when known (much narrower scope than worktree)",
					Required:    false,
				},
			},
		},
		handler:   diagnosePreparePrompt,
		WhenToUse: `user says "why did prepare fail" / "what went wrong" / "the cold build is broken"`,
	},
	{
		def: &mcpsdk.Prompt{
			Name:        "scaffold-from-framework",
			Title:       "Scaffold .treeman.yaml for the detected framework",
			Description: "Detect the framework in the current repo, draft a .treeman.yaml from the matching scaffold template, validate it, and write it after the user reviews the diff. Stop short of executing prepare so the user controls the first cold build.",
			Arguments: []*mcpsdk.PromptArgument{
				{Name: "repo", Description: "absolute path to the repo root; defaults to cwd's repo", Required: false},
			},
		},
		handler:   scaffoldFromFrameworkPrompt,
		WhenToUse: `user wants a .treeman.yaml scaffolded but is willing to review before running prepare`,
	},
	{
		def: &mcpsdk.Prompt{
			Name:        "cache-cleanup",
			Title:       "Hunt orphan snapshots and drop them",
			Description: "List every cached snapshot for the current repo, probe each one to see whether the engine-side template still exists, and drop the orphans (SQLite rows whose template was deleted out-of-band on the engine). The agent confirms before each drop.",
			Arguments: []*mcpsdk.PromptArgument{
				{Name: "repo", Description: "absolute path to the repo root; defaults to cwd's repo", Required: false},
			},
		},
		handler:   cacheCleanupPrompt,
		WhenToUse: `prepare keeps cold-building / cache seems wrong / snapshot rows look stale; safer than snapshots_purge`,
	},
	{
		def: &mcpsdk.Prompt{
			Name:        "worktree-setup",
			Title:       "Create a worktree end-to-end",
			Description: "Walk through picking an unoccupied branch, computing the slug, creating the worktree (which triggers prepare + setup hooks), waiting for finalize, and reporting the result. Use when the user says \"set me up a worktree for branch X\" but hasn't decided how to verify success.",
			Arguments: []*mcpsdk.PromptArgument{
				{
					Name:        "branch",
					Description: "branch name to base the worktree on; omit to let the agent recommend one from branches_list",
					Required:    false,
				},
				{Name: "repo", Description: "absolute path to the repo root; defaults to cwd's repo", Required: false},
			},
		},
		handler:   worktreeSetupPrompt,
		WhenToUse: `user says "set me up a worktree" / "make me a worktree for X" / "start working on branch Y"`,
	},
	{
		def: &mcpsdk.Prompt{
			Name:        "migration-trial",
			Title:       "Trial a migration in an ephemeral worktree",
			Description: "Create a throw-away worktree, run the user's migrate step against it, report the outcome (plus any schema deltas via db_schema_dump), and tear the worktree down. Use to validate a migration change BEFORE merging — without polluting any existing worktree's database state.",
			Arguments: []*mcpsdk.PromptArgument{
				{Name: "branch", Description: "branch carrying the migration to trial", Required: true},
				{
					Name:        "db_index",
					Description: "index into databases[] to focus the schema diff on; omit to skip the diff step",
					Required:    false,
				},
				{Name: "repo", Description: "absolute path to the repo root; defaults to cwd's repo", Required: false},
			},
		},
		handler:   migrationTrialPrompt,
		WhenToUse: `user wants to validate a migration without committing to it / "is this migration safe?"`,
	},
	{
		def: &mcpsdk.Prompt{
			Name:        "edit-config",
			Title:       "Edit treeman config (global or repo) safely",
			Description: "Pick the right config file for a change (user-global ~/.config/treeman/config.yaml vs per-repo .treeman.yaml), preview the edit, apply it scope-checked, and validate. Covers create/update/remove/delete + rollback. Use whenever the user wants to change a treeman setting and it isn't obvious which file it belongs in.",
			Arguments: []*mcpsdk.PromptArgument{
				{
					Name:        "setting",
					Description: "the setting or key the user wants to change (free text, e.g. 'daemon log level', 'databases[0].engine')",
					Required:    false,
				},
				{Name: "repo", Description: "absolute path to the repo root; defaults to cwd's repo", Required: false},
			},
		},
		handler:   editConfigPrompt,
		WhenToUse: `user wants to change a treeman setting / "set the daemon log level" / "where does X config go, global or repo?"`,
	},
	{
		def: &mcpsdk.Prompt{
			Name:        "bootstrap-new-repo",
			Title:       "Set up treeman in a fresh repo end-to-end",
			Description: "Walk through first-time enrollment: framework detect → engine connection probe per engine → init_repo → schema_install → daemon ensure → registry_register → first prepare → verify. Use when the user wants treeman wired into a repo that has no .treeman.yaml yet.",
			Arguments: []*mcpsdk.PromptArgument{
				{Name: "repo", Description: "absolute path to the repo root; defaults to cwd's repo", Required: false},
			},
		},
		handler:   bootstrapNewRepoPrompt,
		WhenToUse: `repo has no .treeman.yaml yet; user wants treeman fully wired up from scratch`,
	},
}

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
// registerPrompts records each prompt into the build catalog as a
// deferred entry (category "prompt") rather than registering it
// immediately — so prompts, like non-core tools, stay out of the
// up-front context and are revealed on demand through the `tools`
// gateway (action=list shows them, action=enable / category=prompt
// loads them). Under TREEMAN_MCP_ALL_TOOLS=1 activateTools registers
// them all eagerly, matching the pre-disclosure behavior. The summary
// is the WhenToUse trigger phrase so the catalog listing is actionable.
func registerPrompts(srv *mcpsdk.Server) {
	for _, p := range allPrompts {
		def, handler := p.def, p.handler
		pendingTools = append(pendingTools, &toolEntry{
			name:     def.Name,
			category: "prompt",
			summary:  p.WhenToUse,
			register: func() { srv.AddPrompt(def, handler) },
		})
	}
}

// ─── prompts_list (discoverability tool) ────────────────────────────

type promptsListIn struct{}

type promptsListArgEntry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

type promptsListEntry struct {
	Name        string                `json:"name"`
	Title       string                `json:"title"`
	Description string                `json:"description"`
	WhenToUse   string                `json:"when_to_use,omitempty"`
	Arguments   []promptsListArgEntry `json:"arguments,omitempty"`
}

type promptsListOut struct {
	Prompts []promptsListEntry `json:"prompts"`
}

// promptsListTool returns every registered prompt as a flat list with
// the when-to-use trigger phrases. Backup discovery surface for clients
// whose MCP-prompts UI is hidden or hard to find.
func promptsListTool(_ context.Context, _ *mcpsdk.CallToolRequest, _ promptsListIn) (*mcpsdk.CallToolResult, promptsListOut, error) {
	out := promptsListOut{Prompts: make([]promptsListEntry, 0, len(allPrompts))}
	for _, p := range allPrompts {
		entry := promptsListEntry{
			Name:        p.def.Name,
			Title:       p.def.Title,
			Description: p.def.Description,
			WhenToUse:   p.WhenToUse,
		}
		for _, a := range p.def.Arguments {
			entry.Arguments = append(entry.Arguments, promptsListArgEntry{
				Name:        a.Name,
				Description: a.Description,
				Required:    a.Required,
			})
		}
		out.Prompts = append(out.Prompts, entry)
	}
	return nil, out, nil
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
		scope = "scope: run_id=" + runID
	case wt != "":
		scope = "scope: worktree=" + wt
	}

	text := fmt.Sprintf(`Diagnose the most recent failed prepare. %s

Execute these tool calls in order. Stop as soon as you can identify the failing step + root cause.

1. logs_query — fetch recent errors.
   • levels=["error"]
   • event_types=["prepare:end","worktree:create:error","clones:restore:error","clones:end","prepare:unsupported"]
   %s
   • limit=20
   Identify the most-recent failure row's repo, worktree, engine, and event_type.

2. If a run_id is visible in the failure payload but wasn't supplied as an argument, RE-RUN logs_query with that run_id and event_types=["prepare:start","prepare:phase","prepare:end","clones:start","clones:end","clones:restore:error","snapshots:cache:hit"] to reconstruct the full timeline of that prepare invocation.

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

	text := fmt.Sprintf(
		`Scaffold a .treeman.yaml for this repo from the detected framework template. Do NOT run prepare — the user should approve the file first.

Execute these tool calls in order:

1. fw_detect (%s) — list every framework treeman recognises in the repo. If none detected, STOP and tell the user there's no template to scaffold from.

2. init_repo (%sforce=false) — generate the scaffold. If the call errors with "file exists", call config_get first, then init_repo with force=true ONLY after the user confirms they want to overwrite.

3. config_validate — verify the scaffolded file parses. If validation fails, surface the parse error verbatim and stop.

4. config_get (resolved=true) — show the user what treeman will actually execute against (after env-var substitution and defaults).

Report: the detected framework(s), the path written, the resolved databases[] block, and one short paragraph telling the user what to verify before running 'treeman prepare'.`,
		repoArg,
		repoArg,
	)

	return userMsg(text), nil
}

func worktreeSetupPrompt(_ context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
	branch := req.Params.Arguments["branch"]
	repo := req.Params.Arguments["repo"]
	repoArg := ""
	if repo != "" {
		repoArg = "repo=\"" + repo + "\""
	}

	var branchStep string
	if branch == "" {
		branchStep = `1. branches_list (` + repoArg + `) — list local + origin-only branches. RECOMMEND one branch to the user (prefer: has_local=true AND worktree_dir empty; otherwise has_remote=true AND worktree_dir empty). ASK the user to confirm or pick a different branch before continuing.`
	} else {
		branchStep = fmt.Sprintf(
			`1. branches_list (%s) — verify branch=%q is not already occupying a worktree (worktree_dir empty). If it is, STOP and tell the user which worktree currently has it.`,
			repoArg,
			branch,
		)
	}

	text := fmt.Sprintf(
		`Set up a fresh worktree end-to-end. Do NOT skip the wait step — without it, the user can't tell whether prepare actually succeeded.

%s

2. daemon_status — confirm treemand is running. If status=not-running, call daemon_control(action="start") and verify status flips to running before proceeding.

3. slug_compute (path=".worktrees/<branch>") — preview the slug so you can tell the user which database/redis prefix/index suffix the new worktree will get.

4. worktree_create (branch=%q, %s) — this BLOCKS until the cold-build finishes. May take minutes on first runs. Capture stdout/stderr.

5. If worktree_create returned a non-zero exit code, IMMEDIATELY chain into the diagnose-prepare-failure prompt (pass run_id if visible in the output). Do NOT proceed to verify if create failed.

6. worktree_show — confirm the new worktree is registered, with the expected slug and branch.

Report: the slug, the absolute worktree path, total wall-clock time, and one short verification command the user can run inside the worktree to sanity-check the app sees the new database.`,
		branchStep,
		ifEmpty(branch, "<chosen-branch>"),
		repoArg,
	)

	return userMsg(text), nil
}

func migrationTrialPrompt(_ context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
	branch := req.Params.Arguments["branch"]
	dbIdx := req.Params.Arguments["db_index"]
	repo := req.Params.Arguments["repo"]
	repoArg := ""
	if repo != "" {
		repoArg = "repo=\"" + repo + "\", "
	}
	schemaDiffStep := ""
	if dbIdx != "" {
		schemaDiffStep = fmt.Sprintf(`
6. db_schema_dump (engine=<from cfg.databases[%s].engine>, db=<from worktree_show>) — capture the post-migration schema. Compare against the pre-migration schema (run the same call against an unmodified worktree's db if one exists, or against the source DB before migration ran).`, dbIdx)
	}

	text := fmt.Sprintf(
		`Trial the migration on branch %q in a throw-away worktree. The worktree MUST be torn down at the end regardless of outcome — otherwise we leak DB state.

Execute these tool calls in order:

1. branches_list (%s) — confirm branch=%q exists. If not, STOP.

2. daemon_status — confirm treemand is up. Start it if not.

3. worktree_create (%sbranch=%q) — blocks until cold-build finishes. The migrate step runs as part of create.

4. CAPTURE the run_id from worktree_create's output for later log queries.

5. If create returned non-zero, fetch logs_query (run_id=..., levels=["error"]) and report the failing step. Then SKIP to step 7 (always teardown).
%s
7. worktree_delete (branch=%q) — ALWAYS run this, even on failure. The trial is throw-away by design.

Report: did the migration succeed? If yes, the schema delta (table-level summary). If no, the failing step and the exact error line.`,
		branch,
		repoArg,
		branch,
		repoArg,
		branch,
		schemaDiffStep,
		branch,
	)

	return userMsg(text), nil
}

// ifEmpty returns dflt when s is empty, otherwise s. Used to keep
// the prompt body parameterised when the user didn't supply a value.
func ifEmpty(s, dflt string) string {
	if s == "" {
		return dflt
	}
	return s
}

func editConfigPrompt(_ context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
	setting := req.Params.Arguments["setting"]
	repo := req.Params.Arguments["repo"]
	repoArg := ""
	if repo != "" {
		repoArg = "repo=\"" + repo + "\""
	}
	repoArgComma := ""
	if repoArg != "" {
		repoArgComma = repoArg + ", "
	}
	settingLine := "the user's requested change"
	if setting != "" {
		settingLine = fmt.Sprintf("%q", setting)
	}

	text := fmt.Sprintf(
		`Apply a treeman config change for %s. Treeman has TWO config layers; your first job is to put the change in the right one, then edit it safely.

SCOPE RULES (which file?):
• GLOBAL (~/.config/treeman/config.yaml, scope="global"): daemon, snapshots, logs, status, notifications — machine-wide, shared by every repo.
• REPO (.treeman.yaml, scope="repo"): databases, patches, hooks, main_worktree, env_sources — project-specific.
• BOTH (connections, worktrees, ports, frameworks, auto_fetch): allowed in either; prefer repo unless the user wants a machine-wide default.
A key in the wrong layer is REJECTED at write time — don't guess, use config_schema if unsure.

Execute these tool calls in order:

1. config_locate (%s) — show which config files exist and where. Decide the target scope from the SCOPE RULES above.

2. config_schema (scope=<global|repo>) — confirm the key is valid for that scope and learn its type/shape. If the key only exists in the OTHER scope, switch targets.

3. config_get (scope=<target>%s) — read the current value so you can show a before/after. If the target file doesn't exist yet and you're adding the first key, that's fine — config_set creates it (or run init_repo with global=true / for the repo to scaffold a commented starter).

4. Apply the edit:
   • single field → config_set (scope=<target>, path="<dotted.path>", value=<new>). Creates the file if missing, preserves comments.
   • remove a key entirely → config_unset (scope=<target>, path="<dotted.path>").
   • full rewrite → config_diff (scope=<target>, body=...) to preview, then config_write (scope=<target>, body=...).
   • delete the whole file → config_delete (scope=<target>, dry_run=true first, then ack=true). DESTRUCTIVE — confirm with the user.

5. config_validate (scope=<target>) — confirm the result still parses + validates. config_set/write/unset validate inline, but run this after any out-of-band edit.

6. If anything looks wrong, config_history (scope=<target>) → config_restore (scope=<target>, generation=N) rolls back — every mutation is snapshotted to SQLite first.

Report: which scope/file you chose and WHY, the before→after value, and the validation result.`,
		settingLine,
		repoArg,
		repoArgComma,
	)
	return userMsg(text), nil
}

func bootstrapNewRepoPrompt(_ context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
	repo := req.Params.Arguments["repo"]
	repoArg := ""
	if repo != "" {
		repoArg = "repo=\"" + repo + "\""
	}
	repoArgComma := ""
	if repoArg != "" {
		repoArgComma = repoArg + ", "
	}

	text := fmt.Sprintf(
		`Set up treeman in this repo end-to-end. The repo has (or you suspect has) no .treeman.yaml — your job is to drive it from zero to a working first prepare. ASK before running anything destructive or anything that hits a live engine the user hasn't explicitly OK'd.

Execute these tool calls in order:

1. config_get (%s) — confirm there is no .treeman.yaml yet (or that the user wants to start over). If one exists and the user does NOT want to overwrite, STOP and tell them to use the worktree-setup prompt instead.

2. fw_detect (%s) — list detected migration + test frameworks. If none, STOP and tell the user treeman has no scaffold template for this stack — they'll need to write .treeman.yaml manually.

3. init_repo (%sforce=false) — generate the scaffold. On "file exists", ASK before passing force=true.

4. config_get (%sresolved=true) — show the resolved config. For EACH engine in cfg.connections, in parallel:
   • connection_probe (engine="<name>", repo="<repo>") — confirm reachable. If unreachable, tell the user the exact engine + error and ASK whether to (a) edit the connection via config_set, (b) skip prepare and let them fix it, or (c) abort.

5. schema_install (%starget=repo) — wire up editor autocomplete for .treeman.yaml.

6. daemon_status — confirm treemand is up. If not, call daemon_control(action="start") and verify status flips to running.

7. registry_register (%spath="<repo>") — enroll the repo so the daemon's watcher attaches.

8. prepare_run (%s) — run the FIRST prepare. This is the longest step; pair with logs_wait if you want to surface progress. If it fails, IMMEDIATELY chain into the diagnose-prepare-failure prompt.

9. engine_status — verify every configured engine has the expected per-worktree database after prepare.

Report: which frameworks were detected, which engines were probed (reachable vs not), the scaffolded .treeman.yaml path, the first prepare's duration + outcome, and one short paragraph telling the user what to verify before creating their first worktree.`,
		repoArg,
		repoArg,
		repoArgComma,
		repoArgComma,
		repoArgComma,
		repoArgComma,
		repoArg,
	)
	return userMsg(text), nil
}

func cacheCleanupPrompt(_ context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
	repo := req.Params.Arguments["repo"]
	repoArg := ""
	if repo != "" {
		repoArg = "repo=\"" + repo + "\""
	}

	text := fmt.Sprintf(
		`Find and drop orphan snapshots for this repo. An orphan is a SQLite snapshot row whose engine-side template was deleted out-of-band (e.g. someone ran 'DROP DATABASE' directly). They're invisible to snapshots_purge because purge drops every snapshot, not just stale ones.

Execute these tool calls in order:

1. snapshots_list (%s) — list every cached snapshot. Record each fingerprint.

2. For EACH fingerprint, call snapshot_inspect with that fingerprint. Collect the ones where template_exists=false. Those are the orphans.

3. If no orphans, report "0 orphans found" and stop.

4. Show the user the orphan list (fingerprint, engine, template_name, source_db, created_at) and ASK FOR CONFIRMATION before dropping anything.

5. After confirmation, for each orphan call snapshot_drop with that fingerprint. Report per-engine success/failure counts.

NEVER call snapshots_purge from this flow — that nukes valid templates too.`,
		repoArg,
	)

	return userMsg(text), nil
}
