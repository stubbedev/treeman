// Package rpc — wire types shared between `treemand` and `treeman`.
// Wire format is newline-delimited JSON over a unix domain socket with
// an SO_PEERCRED uid check. Requests and responses are tagged unions: a
// discriminator (`method` on requests, `kind` on responses) plus the
// matching args/payload object, (un)marshaled by encoding/json directly.
package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stubbedev/treeman/internal/sunpath"

	"github.com/stubbedev/treeman/pkg/safego"
)

// ErrDaemonUnreachable wraps a dial failure — the daemon socket could
// not be connected to at all. Callers use errors.Is to distinguish
// "daemon absent" (safe to fall back to in-process execution) from a
// post-dial I/O failure (encode/decode/timeout), where the daemon MAY
// already be running the plan and re-running it in-process would race
// the daemon's writer. Only the former is safe to retry locally.
var ErrDaemonUnreachable = errors.New("daemon unreachable")

// ProtocolMismatchError reports a daemon whose wire protocol predates
// (or postdates) this CLI. Method/args the CLI sends may not exist on
// the old daemon, so every stamped call fails up front — the fix is
// always the same: restart the daemon so it runs the matching binary.
// Callers match with errors.As to suppress autostart fallbacks
// (restarting behind the user's back hides the problem).
type ProtocolMismatchError struct {
	DaemonVersion  string
	DaemonProtocol uint32
}

func (e *ProtocolMismatchError) Error() string {
	ident := ""
	if e.DaemonVersion != "" {
		ident = " v" + e.DaemonVersion
	}
	return fmt.Sprintf(
		"treemand%s speaks protocol v%d but treeman expects v%d — run `treeman daemon restart`",
		ident, e.DaemonProtocol, ProtocolVersion)
}

// checkProtocol gates a decoded response on its protocol stamp. Zero
// means the daemon didn't stamp this response (a pre-stamping binary,
// or a bare Pong) — the MethodStatus probe in EnsureDaemon still
// catches those, since status always carried the fields.
func checkProtocol(resp Response) error {
	if pv := resp.ProtocolVersion; pv != 0 && pv != ProtocolVersion {
		return &ProtocolMismatchError{DaemonVersion: resp.DaemonVersion, DaemonProtocol: pv}
	}
	return nil
}

// ProtocolVersion is bumped when an incompatible RPC change ships.
// v2: nested-args envelope ({"method":m,"<m>":{...}}) replacing the old
// flat tagged union; worktree finalize/teardown folded into run_plan.
const ProtocolVersion uint32 = 2

// Default socket-path lookup order: $TREEMAN_SOCKET → $XDG_RUNTIME_DIR
// /treeman.sock → $XDG_DATA_HOME/treeman/treeman.sock.
const (
	SocketEnv      = "TREEMAN_SOCKET"
	SocketBasename = "treeman.sock"
)

// SocketPath returns the effective socket path.
func SocketPath() (string, error) {
	if p := os.Getenv(SocketEnv); p != "" {
		return checkedSocketPath(p, "set "+SocketEnv+" to a shorter path")
	}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return checkedSocketPath(filepath.Join(rt, SocketBasename),
			"point XDG_RUNTIME_DIR at a shorter directory, or set "+SocketEnv+" to a shorter path")
	}
	xdg := os.Getenv("XDG_DATA_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		xdg = filepath.Join(home, ".local", "share")
	}
	return checkedSocketPath(filepath.Join(xdg, "treeman", SocketBasename),
		"set XDG_RUNTIME_DIR to a shorter directory, or "+SocketEnv+" to a shorter path")
}

func checkedSocketPath(path, fix string) (string, error) {
	if err := sunpath.Check(path); err != nil {
		return "", fmt.Errorf("%w; %s (at most %d bytes)", err, fix, sunpath.Max)
	}
	return path, nil
}

// ─────────────────────────── Request ───────────────────────────
//
// Wire shape: {"method":"<m>","<m>":{...args...}}. `method` is the
// discriminator; the args object is nested under the method-named key.

// RequestMethod values.
const (
	MethodStatus       = "status"
	MethodPing         = "ping"
	MethodRepoRegister = "repo_register"
	MethodWatcherStart = "watcher_start"
	MethodWatcherStop  = "watcher_stop"
	MethodWatcherList  = "watcher_list"
	MethodWorktreeList = "worktree_list"
	MethodConfigReload = "config_reload"
	MethodRepoRemove   = "repo_remove"
	MethodShutdown     = "shutdown"
	MethodSyncNow      = "sync_now"
	MethodSyncStatus   = "sync_status"
	MethodDaemonState  = "daemon_state"
	// MethodRunPlan offloads a plan to the daemon, the sole mutator of
	// repo/worktree state. A plan is a set of groups: groups run in
	// PARALLEL with each other, tasks within a group run SEQUENTIALLY
	// (a group is one ordered lane; a lane stops at its first failure).
	// The daemon either queues the plan and returns immediately
	// (RunPlanArgs.Wait false) or runs it to completion and returns
	// per-task results (Wait true). Foreground progress is observed by
	// subscribing to the plan's RunID via MethodEventSubscribe before
	// dispatch.
	MethodRunPlan = "run_plan"
	// MethodEventSubscribe opens a STREAMING subscription: one request
	// → many responses (one per matching event) over the same socket
	// connection, until ctx cancels or client closes.
	MethodEventSubscribe = "event_subscribe"
)

// Request is the envelope every CLI message sends. The wire shape is the
// method discriminator plus the matching args object, e.g.
// {"method":"run_plan","run_plan":{...}}. encoding/json (un)marshals it
// directly — omitempty drops the nil pointers, so exactly the one set for
// r.Method survives, and decode populates the matching pointer by key.
// No hand-rolled (un)marshal: add a method = add a Method* const, a
// tagged pointer field here, and an args struct.
type Request struct {
	Method         string              `json:"method"`
	RepoRegister   *RepoRegisterArgs   `json:"repo_register,omitempty"`
	WatcherStart   *WatcherStartArgs   `json:"watcher_start,omitempty"`
	WatcherStop    *WatcherStopArgs    `json:"watcher_stop,omitempty"`
	WorktreeList   *WorktreeListArgs   `json:"worktree_list,omitempty"`
	ConfigReload   *ConfigReloadArgs   `json:"config_reload,omitempty"`
	RepoRemove     *RepoRemoveArgs     `json:"repo_remove,omitempty"`
	SyncNow        *SyncNowArgs        `json:"sync_now,omitempty"`
	SyncStatus     *SyncStatusArgs     `json:"sync_status,omitempty"`
	RunPlan        *RunPlanArgs        `json:"run_plan,omitempty"`
	EventSubscribe *EventSubscribeArgs `json:"event_subscribe,omitempty"`
}

// Task type values — the concrete operation a Task performs. The daemon
// is the sole executor; every state mutation is one of these.
const (
	TaskPrepare            = "prepare"             // prepare.Run
	TaskDBReset            = "db_reset"            // drop branch-scoped + re-prepare
	TaskDBSave             = "db_save"             // capture branch-scoped → durable copy
	TaskHookRun            = "hook_run"            // run one hook phase
	TaskSnapshotsPurge     = "snapshots_purge"     // drop every cached template DB
	TaskSnapshotsPrune     = "snapshots_prune"     // delete rows whose engine template is gone
	TaskMainPurgeDBs       = "main_purge_dbs"      // drop main_<branch> DBs across branches
	TaskWorktreeRegister   = "worktree_register"   // EnsureRepo + EnsureWorktree row
	TaskWorktreeUnregister = "worktree_unregister" // mark a worktree row deleted
	TaskLogsPurge          = "logs_purge"          // delete event-log rows by filter
	TaskRegistryRepair     = "registry_repair"     // reconcile SQLite vs git worktree list
	TaskConfigWrite        = "config_write"        // snapshot + atomic-write .treeman.yaml + reload
	// Worktree lifecycle, folded into the plan model. Booleans ride in
	// Params ("force"/"no_fetch"/"skip_hooks"/"skip_prepare" == "1");
	// create's from/path overrides ride in Params["from"]/["path"].
	TaskWorktreeCreate   = "worktree_create"   // git add + register + ports (+ async finalize)
	TaskWorktreeFinalize = "worktree_finalize" // setup hooks + prepare tail
	TaskWorktreeTeardown = "worktree_teardown" // teardown hooks + DB drop + git remove
)

// Task.Params keys — the string-keyed side-channel a Task carries
// (booleans encoded as "1"). Single source of truth for this wire
// contract so the CLI producers and daemon consumers can't drift on a
// typo. snake_case, matching the RPC method/task convention.
const (
	ParamBranch       = "branch"
	ParamFrom         = "from"
	ParamPath         = "path"
	ParamPhase        = "phase"
	ParamForce        = "force"
	ParamBody         = "body"
	ParamEngineFilter = "engine_filter"
	ParamNoFetch      = "no_fetch"
	ParamSkipHooks    = "skip_hooks"
	ParamSkipPrepare  = "skip_prepare"
	ParamWait         = "wait"
	ParamFresh        = "fresh"
	ParamLevels       = "levels"
	ParamEventTypes   = "event_types"
	ParamUntilMs      = "until_ms"
	ParamRepo         = "repo"
	ParamWorktree     = "worktree"
)

// RepoRegisterArgs — Register or update a repo.
type RepoRegisterArgs struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// WatcherStartArgs / WatcherStopArgs — control the per-repo watcher.
type (
	WatcherStartArgs struct {
		RepoPath string `json:"repo_path"`
	}
	WatcherStopArgs struct {
		RepoPath string `json:"repo_path"`
	}
)

// WorktreeListArgs — list the watched linked-worktrees for a repo.
type WorktreeListArgs struct {
	RepoPath string `json:"repo_path"`
}

// ConfigReloadArgs — tells the daemon to invalidate its config cache
// and restart watchers. Empty RepoPath reloads every registered repo.
type ConfigReloadArgs struct {
	RepoPath string `json:"repo_path,omitempty"`
}

// SyncNowArgs — on-demand fetch + advance. Path scopes the work:
//
//   - empty       → every registered repo (mirrors auto-fetch sweep).
//   - repo root   → that repo + every linked worktree.
//   - wt path     → the repo the worktree belongs to + that wt only.
//
// Manual sync ignores the per-repo backoff window so a user override
// can punch through an offline-mode pause.
type SyncNowArgs struct {
	Path string `json:"path,omitempty"`
}

// SyncStatusArgs — read sync state. Empty RepoPath returns every
// registered repo; non-empty filters to one.
type SyncStatusArgs struct {
	RepoPath string `json:"repo_path,omitempty"`
}

// SyncRepoStatus is one row in a SyncStatus response.
type SyncRepoStatus struct {
	RepoPath       string               `json:"repo_path"`
	LastFetchUnix  int64                `json:"last_fetch_unix,omitempty"`
	ConsecFailures int                  `json:"consec_failures,omitempty"`
	NextRetryUnix  int64                `json:"next_retry_unix,omitempty"`
	Mode           string               `json:"mode"`
	Worktrees      []SyncWorktreeStatus `json:"worktrees,omitempty"`
}

// SyncWorktreeStatus is one worktree row inside SyncRepoStatus.
type SyncWorktreeStatus struct {
	Path           string `json:"path"`
	Branch         string `json:"branch,omitempty"`
	Ahead          int    `json:"ahead"`
	Behind         int    `json:"behind"`
	Dirty          bool   `json:"dirty"`
	LastSkipReason string `json:"last_skip_reason,omitempty"`
}

// EventSubscribeArgs — open a streaming subscription. Filters
// AND-combine; empty fields match everything. The subscription
// fires on EVERY future WriteEvent on the daemon's store; historical
// events are NOT replayed (use logs_query for backfill, then subscribe
// for live-tail).
//
// IMPORTANT: every filter field here MUST be mirrored in the MCP
// logs_subscribe push path so push and poll modes produce identical
// result sets. New fields go in three places: this struct, the
// MarshalJSON/UnmarshalJSON branches in rpc.go, and the per-event
// filter in daemon/dispatch.go buildSubscribeFilter.
type EventSubscribeArgs struct {
	RepoPath     string   `json:"repo_path,omitempty"`
	WorktreePath string   `json:"worktree_path,omitempty"`
	Levels       []string `json:"levels,omitempty"`
	EventTypes   []string `json:"event_types,omitempty"`
	Phases       []string `json:"phases,omitempty"`
	PayloadLike  string   `json:"payload_like,omitempty"`
	RunID        string   `json:"run_id,omitempty"`
}

// EventEnvelope is one streamed event row. Mirrors store.Event (minus
// the ID nullables) so the daemon can flatten in-memory Event values
// onto the wire without an extra serialisation hop. ID is best-effort
// (0 on the batched WriteEvent path).
type EventEnvelope struct {
	ID          int64  `json:"id,omitempty"`
	Ts          int64  `json:"ts"`
	Level       string `json:"level"`
	EventType   string `json:"event_type"`
	Phase       string `json:"phase,omitempty"`
	Message     string `json:"message,omitempty"`
	PayloadJSON string `json:"payload_json,omitempty"`
	RepoID      int64  `json:"repo_id,omitempty"`
	WorktreeID  int64  `json:"worktree_id,omitempty"`
	DurationMs  int64  `json:"duration_ms,omitempty"`
}

// RepoRemoveArgs — drop a repo from the SQLite registry. Daemon stops
// every watcher attached to the repo first. Force=false refuses the
// removal when active (`deleted_at IS NULL`) worktrees still exist.
// External resources (databases, on-disk worktree dirs) are never
// touched — callers wanting a destructive purge run `treeman worktree delete`
// first.
type RepoRemoveArgs struct {
	RepoPath string `json:"repo_path"`
	Force    bool   `json:"force,omitempty"`
}

// Task is one unit of work the daemon executes. Convention: Type +
// RepoPath/WorktreePath (the routing fields) + InheritedEnv are the only
// explicit fields; every task-specific argument rides in Params (e.g.
// branch, phase, engine_filter, force/no_fetch/skip_hooks=="1", the
// logs-purge filter, the config-write body). WorktreePath is empty for
// repo-scoped tasks (snapshots_purge / main_purge_dbs). The CLI resolves
// all paths to absolute before dispatch — the daemon's cwd is not the
// user's.
type Task struct {
	Type         string            `json:"type"`
	RepoPath     string            `json:"repo_path,omitempty"`
	WorktreePath string            `json:"worktree_path,omitempty"`
	Params       map[string]string `json:"params,omitempty"`
	InheritedEnv map[string]string `json:"inherited_env,omitempty"`
}

// One wraps a single task as a one-task group — the common plan shape.
func One(t Task) []Task { return []Task{t} }

// Plan builds a run_plan Request. Groups run in parallel; tasks within a
// group run sequentially. wait=false queues the plan (KindPlanQueued);
// wait=true runs it to completion and returns results (KindPlanResult).
func Plan(wait bool, groups ...[]Task) Request {
	return Request{Method: MethodRunPlan, RunPlan: &RunPlanArgs{Groups: groups, Wait: wait}}
}

// WorktreeCreateArgs is the typed form of a TaskWorktreeCreate's
// Params. Task encodes it and WorktreeCreateArgsOf decodes it, so every
// producer (CLI, MCP) and the daemon consumer share one mapping onto the
// string-keyed wire params instead of each spelling it out.
type WorktreeCreateArgs struct {
	Branch      string
	From        string // base branch; empty = repo default
	Path        string // explicit worktree path; empty = derived
	NoFetch     bool
	SkipHooks   bool
	SkipPrepare bool
	// Wait runs the hooks + prepare tail before the task returns and
	// fails the task when it fails, instead of detaching it.
	Wait bool
}

// Task builds the TaskWorktreeCreate for a in repoPath, carrying the
// caller's inherited env.
func (a WorktreeCreateArgs) Task(repoPath string, inheritedEnv map[string]string) Task {
	params := map[string]string{ParamBranch: a.Branch}
	setParam(params, ParamFrom, a.From)
	setParam(params, ParamPath, a.Path)
	setFlagParam(params, ParamNoFetch, a.NoFetch)
	setFlagParam(params, ParamSkipHooks, a.SkipHooks)
	setFlagParam(params, ParamSkipPrepare, a.SkipPrepare)
	setFlagParam(params, ParamWait, a.Wait)
	return Task{Type: TaskWorktreeCreate, RepoPath: repoPath, Params: params, InheritedEnv: inheritedEnv}
}

// WorktreeCreateArgsOf decodes a TaskWorktreeCreate's Params.
func WorktreeCreateArgsOf(t Task) WorktreeCreateArgs {
	return WorktreeCreateArgs{
		Branch:      t.Params[ParamBranch],
		From:        t.Params[ParamFrom],
		Path:        t.Params[ParamPath],
		NoFetch:     FlagParam(t, ParamNoFetch),
		SkipHooks:   FlagParam(t, ParamSkipHooks),
		SkipPrepare: FlagParam(t, ParamSkipPrepare),
		Wait:        FlagParam(t, ParamWait),
	}
}

// PrepareArgs is the typed form of a TaskPrepare's Params, shared by
// the CLI producer and the daemon consumer like WorktreeCreateArgs.
type PrepareArgs struct {
	Fresh bool
}

// Task builds the TaskPrepare for worktreePath in repoPath, carrying
// the caller's inherited env.
func (a PrepareArgs) Task(repoPath, worktreePath string, inheritedEnv map[string]string) Task {
	params := map[string]string{}
	setFlagParam(params, ParamFresh, a.Fresh)
	return Task{Type: TaskPrepare, RepoPath: repoPath, WorktreePath: worktreePath, Params: params, InheritedEnv: inheritedEnv}
}

// PrepareArgsOf decodes a TaskPrepare's Params into PrepareArgs.
func PrepareArgsOf(t Task) PrepareArgs {
	return PrepareArgs{Fresh: FlagParam(t, ParamFresh)}
}

// FlagParam reports whether boolean param key is set on t (encoded "1").
func FlagParam(t Task, key string) bool { return t.Params[key] == "1" }

// setParam stores a string param, omitting the empty value.
func setParam(params map[string]string, key, val string) {
	if val != "" {
		params[key] = val
	}
}

// setFlagParam stores a boolean param in its "1" wire encoding,
// omitting false.
func setFlagParam(params map[string]string, key string, on bool) {
	if on {
		params[key] = "1"
	}
}

// RunPlanArgs — submit a plan to the daemon. Groups run in parallel; the
// tasks inside a group run sequentially (one ordered lane, stopping at
// its first failure). RunID, when set, becomes the executor's run-id so
// a foreground caller can subscribe to the plan's events before dispatch
// (no missed-event race); empty → daemon mints one. Wait=false queues
// the plan and returns immediately (KindPlanQueued); Wait=true runs it to
// completion and returns per-task results (KindPlanResult).
type RunPlanArgs struct {
	Groups    [][]Task   `json:"groups"`
	RunID     string     `json:"run_id,omitempty"`
	Wait      bool       `json:"wait,omitempty"`
	Requester *Requester `json:"requester,omitempty"`
}

type Requester struct {
	PID     int    `json:"pid"`
	PPID    int    `json:"ppid"`
	Command string `json:"command"`
}

func CurrentRequester() *Requester {
	return &Requester{PID: os.Getpid(), PPID: os.Getppid(), Command: strings.Join(os.Args, " ")}
}

// TaskResult is one task's outcome, returned for a Wait=true plan.
// PayloadJSON carries the task-specific structured result (e.g. prepare
// outcomes) for callers that asked for --json.
type TaskResult struct {
	Type        string `json:"type"`
	OK          bool   `json:"ok"`
	Message     string `json:"message,omitempty"`
	PayloadJSON string `json:"payload_json,omitempty"`
}

// ─────────────────────────── Response ───────────────────────────

// Failure is the error a run_plan response reports: the daemon's error,
// or the message of the first failed task of a plan that ran to
// completion. Nil for a queued plan and for one whose tasks all
// succeeded.
func (r Response) Failure() error {
	if r.Kind == KindError {
		return errors.New(r.Message)
	}
	for _, t := range r.TaskResults {
		if !t.OK {
			return errors.New(t.Message)
		}
	}
	return nil
}

// Response kinds. Wire shape: {"kind": "ok", ...}.
const (
	KindOk             = "ok"
	KindPong           = "pong"
	KindStatus         = "status"
	KindRepoRegistered = "repo_registered"
	KindWatcherStarted = "watcher_started"
	KindWatcherStopped = "watcher_stopped"
	KindWatcherList    = "watcher_list"
	KindWorktreeList   = "worktree_list"
	KindPlanQueued     = "plan_queued"
	KindPlanResult     = "plan_result"
	KindSyncResult     = "sync_result"
	KindSyncStatus     = "sync_status"
	KindDaemonState    = "daemon_state"
	// KindSubscribed is the first frame of a MethodEventSubscribe
	// stream: the daemon sends it once the subscription's filter is
	// live, so every event written after the client reads it is
	// delivered. Stamped with ProtocolVersion/DaemonVersion.
	KindSubscribed = "subscribed"
	// KindEvent is the per-event envelope emitted on a streaming
	// MethodEventSubscribe response. One emitted per matching event
	// after the KindSubscribed frame; the subscription has no terminal
	// "done" envelope — it ends when the connection closes.
	KindEvent = "event"
	KindError = "error"
)

// Response is the envelope. Tagged-union shape with `kind` as the
// discriminator (always emitted as the first field).
type Response struct {
	Kind string `json:"kind"`
	// Status
	ProtocolVersion uint32 `json:"protocol_version,omitempty"`
	DaemonVersion   string `json:"daemon_version,omitempty"`
	Pid             uint32 `json:"pid,omitempty"`
	StartedAtUnix   int64  `json:"started_at_unix,omitempty"`
	WatcherCount    uint32 `json:"watcher_count,omitempty"`
	// RepoRegistered
	RepoID int64 `json:"repo_id,omitempty"`
	// Watcher
	RepoPath  string           `json:"repo_path,omitempty"`
	Repos     []WatcherSummary `json:"repos,omitempty"`
	Worktrees []string         `json:"worktrees,omitempty"`
	// Finalize / Teardown / Plan queued
	WorktreePath string `json:"worktree_path,omitempty"`
	// PlanResult (Wait=true)
	TaskResults []TaskResult `json:"task_results,omitempty"`
	// SyncResult / SyncStatus
	SyncedRepos []SyncRepoStatus `json:"synced_repos,omitempty"`
	SyncErrors  []string         `json:"sync_errors,omitempty"`
	// DaemonState
	State *DaemonStateSnapshot `json:"state,omitempty"`
	// Streaming event subscription — one Response with Kind=KindEvent
	// per matching event.
	Event *EventEnvelope `json:"event,omitempty"`
	// Error
	Message string `json:"message,omitempty"`
}

// DaemonStateSnapshot is the rich runtime view returned by
// MethodDaemonState — what treemand is doing right now. Used by MCP's
// daemon_state tool / treeman://daemon/state resource so the agent can
// reason about "is the daemon already finalising this worktree, or do
// I dispatch one?". Everything is snapshotted at call time; nothing
// remains live after the call returns.
type DaemonStateSnapshot struct {
	WatcherCount      uint32             `json:"watcher_count"`
	Watchers          []WatcherSummary   `json:"watchers,omitempty"`
	WorktreeWatchers  []string           `json:"worktree_watchers,omitempty"`
	LifecycleWatchers []string           `json:"lifecycle_watchers,omitempty"`
	InFlightFinalizes []InFlightWork     `json:"in_flight_finalizes,omitempty"`
	InFlightTeardowns []string           `json:"in_flight_teardowns,omitempty"`
	SyncBackoffs      []SyncBackoffEntry `json:"sync_backoffs,omitempty"`
	SyncLastSkips     map[string]string  `json:"sync_last_skips,omitempty"`
}

// InFlightWork pairs a worktree path with when the in-flight goroutine
// started — surfaces hung finalizes the agent can wait on (or escalate).
type InFlightWork struct {
	WorktreePath  string `json:"worktree_path"`
	StartedAtUnix int64  `json:"started_at_unix"`
	AgeSeconds    int64  `json:"age_seconds"`
}

// SyncBackoffEntry surfaces the per-repo auto-fetch backoff state so
// the agent can answer "why isn't this repo fetching?". NextRetryUnix
// = 0 means eligible now.
type SyncBackoffEntry struct {
	RepoPath       string `json:"repo_path"`
	ConsecFailures int    `json:"consec_failures"`
	NextRetryUnix  int64  `json:"next_retry_unix"`
}

// WatcherSummary — one row of the WatcherList response.
type WatcherSummary struct {
	Repo          string `json:"repo"`
	WorktreeCount uint32 `json:"worktree_count"`
}

// ─────────────────────────── client ───────────────────────────

// SubscribeEvents opens a streaming event subscription. It returns
// only once the daemon has acknowledged that the subscription is live
// (the KindSubscribed frame), so an event written after SubscribeEvents
// returns is never missed — callers may dispatch work and then wait
// for its events without racing the daemon's hook registration. The
// returned channel is fed by an already-running goroutine until ctx
// cancels, the daemon disconnects, or the caller calls cancel, and is
// closed when the subscription ends.
//
// Unlike Call, the stream itself runs with NO deadline — the
// subscription is intentionally long-lived and ctx-cancel is the only
// supported shutdown. Only the handshake is bounded (subscribeAckTimeout).
//
// Returns (nil, nil, err) on dial or handshake failure. A dial failure
// wraps ErrDaemonUnreachable, exactly like Call, so callers can tell
// "no daemon" (fall back) from "daemon misbehaved" (surface it).
func SubscribeEvents(ctx context.Context, args EventSubscribeArgs) (<-chan EventEnvelope, func(), error) {
	path, err := SocketPath()
	if err != nil {
		return nil, nil, err
	}
	d := net.Dialer{Timeout: 1500 * time.Millisecond}
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, nil, fmt.Errorf("dial %s — is treemand running? %w: %w", path, ErrDaemonUnreachable, err)
	}
	req := Request{Method: MethodEventSubscribe, EventSubscribe: &args}
	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("encode subscribe: %w", err)
	}
	// One decoder for the whole stream: it buffers past the handshake
	// frame, so a second decoder would lose the events behind it.
	dec := json.NewDecoder(conn)
	if err := awaitSubscribed(ctx, conn, dec); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}

	out := make(chan EventEnvelope, 64)
	subCtx, cancel := context.WithCancel(ctx)

	// Closer fired by both the caller (via the returned cancel) and the
	// context-watch goroutine (when parent ctx ends). Closing the
	// underlying conn unblocks the read loop.
	stop := func() {
		cancel()
		_ = conn.Close()
	}

	safego.Go("rpc:subscribe:closer", "", func() {
		<-subCtx.Done()
		_ = conn.Close()
	})

	safego.Go("rpc:subscribe:read", "", func() {
		defer close(out)
		defer cancel()
		for {
			var resp Response
			if err := dec.Decode(&resp); err != nil {
				return
			}
			if resp.Kind == KindError {
				return
			}
			if resp.Kind == KindEvent && resp.Event != nil {
				select {
				case out <- *resp.Event:
				case <-subCtx.Done():
					return
				}
			}
		}
	})

	return out, stop, nil
}

// subscribeAckTimeout bounds the wait for the KindSubscribed frame. The
// daemon sends it right after registering the subscription (one local
// ID lookup), so the bound only ever trips on a daemon that predates
// the handshake or is wedged.
const subscribeAckTimeout = 5 * time.Second

// awaitSubscribed reads the handshake frame of an event subscription:
// KindSubscribed (protocol-checked) means the filter is live; an error
// frame, EOF, or no frame within subscribeAckTimeout (or ctx's earlier
// deadline) fails the subscription instead of leaving the caller
// waiting on events the daemon never registered for.
func awaitSubscribed(ctx context.Context, conn net.Conn, dec *json.Decoder) error {
	deadline := time.Now().Add(subscribeAckTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetReadDeadline(deadline)
	var resp Response
	if err := dec.Decode(&resp); err != nil {
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			return errors.New(
				"event subscribe: daemon did not acknowledge the subscription — treemand is older than this CLI or wedged; restart it (`treeman daemon stop`, then `treeman daemon start`)",
			)
		}
		return fmt.Errorf("event subscribe: read handshake: %w", err)
	}
	if err := checkProtocol(resp); err != nil {
		return err
	}
	switch resp.Kind {
	case KindSubscribed:
		return conn.SetReadDeadline(time.Time{})
	case KindError:
		return fmt.Errorf("event subscribe: %s", resp.Message)
	default:
		return fmt.Errorf("event subscribe: unexpected handshake frame %q", resp.Kind)
	}
}

// Call dials the daemon, sends one Request, reads one Response,
// closes. Default timeout 5s for I/O.
func Call(ctx context.Context, req Request) (Response, error) {
	path, err := SocketPath()
	if err != nil {
		return Response{}, err
	}
	d := net.Dialer{Timeout: 1500 * time.Millisecond}
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return Response{}, fmt.Errorf("dial %s — is treemand running? %w: %w", path, ErrDaemonUnreachable, err)
	}
	defer func() { _ = conn.Close() }()

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	if req.RunPlan != nil && req.RunPlan.Requester == nil {
		stamped := *req.RunPlan
		stamped.Requester = CurrentRequester()
		req.RunPlan = &stamped
	}
	enc := json.NewEncoder(conn)
	if err := enc.Encode(&req); err != nil {
		return Response{}, fmt.Errorf("encode: %w", err)
	}
	dec := json.NewDecoder(conn)
	var resp Response
	if err := dec.Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("decode: %w", err)
	}
	if err := checkProtocol(resp); err != nil {
		return resp, err
	}
	return resp, nil
}
