// Package rpc — wire types shared between `treemand` and `treeman`.
// Wire format is newline-delimited JSON over a unix domain socket
// with an SO_PEERCRED uid check. Requests and responses use a
// tagged-union shape: every payload carries a discriminator field
// (`method` on requests, `kind` on responses) as the first key.
package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ProtocolVersion is bumped when an incompatible RPC change ships.
const ProtocolVersion uint32 = 1

// Default socket-path lookup order: $TREEMAN_SOCKET → $XDG_RUNTIME_DIR
// /treeman.sock → $XDG_DATA_HOME/treeman/treeman.sock.
const (
	SocketEnv      = "TREEMAN_SOCKET"
	SocketBasename = "treeman.sock"
)

// SocketPath returns the effective socket path.
func SocketPath() (string, error) {
	if p := os.Getenv(SocketEnv); p != "" {
		return p, nil
	}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, SocketBasename), nil
	}
	xdg := os.Getenv("XDG_DATA_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		xdg = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(xdg, "treeman", SocketBasename), nil
}

// ─────────────────────────── Request ───────────────────────────
//
// Wire shape: {"method": "status", ...}. `method` is the tagged-
// union discriminator and is always emitted as the first field on
// every payload.

// RequestMethod values.
const (
	MethodStatus           = "status"
	MethodPing             = "ping"
	MethodRepoRegister     = "repo_register"
	MethodWatcherStart     = "watcher_start"
	MethodWatcherStop      = "watcher_stop"
	MethodWatcherList      = "watcher_list"
	MethodWorktreeList     = "worktree_list"
	MethodWorktreeFinalize = "worktree_finalize"
	MethodWorktreeTeardown = "worktree_teardown"
	MethodConfigReload     = "config_reload"
	MethodRepoRemove       = "repo_remove"
	MethodShutdown         = "shutdown"
	MethodSyncNow          = "sync_now"
	MethodSyncStatus       = "sync_status"
	MethodDaemonState      = "daemon_state"
	// MethodEventSubscribe opens a STREAMING subscription: one request
	// → many responses (one per matching event) over the same socket
	// connection, until ctx cancels or client closes.
	MethodEventSubscribe = "event_subscribe"
)

// Request is the envelope every CLI message uses. Exactly one of the
// `*Args` pointers is non-nil for any given Method.
type Request struct {
	Method           string                `json:"method"`
	RepoRegister     *RepoRegisterArgs     `json:",omitempty"`
	WatcherStart     *WatcherStartArgs     `json:",omitempty"`
	WatcherStop      *WatcherStopArgs      `json:",omitempty"`
	WorktreeList     *WorktreeListArgs     `json:",omitempty"`
	WorktreeFinalize *WorktreeFinalizeArgs `json:",omitempty"`
	WorktreeTeardown *WorktreeTeardownArgs `json:",omitempty"`
	ConfigReload     *ConfigReloadArgs     `json:",omitempty"`
	RepoRemove       *RepoRemoveArgs       `json:",omitempty"`
	SyncNow          *SyncNowArgs          `json:",omitempty"`
	SyncStatus       *SyncStatusArgs       `json:",omitempty"`
	EventSubscribe   *EventSubscribeArgs   `json:",omitempty"`
}

// MarshalJSON flattens the discriminator + the matching args struct
// onto a single object so the wire format is a flat tagged union
// keyed by `method`.
func (r Request) MarshalJSON() ([]byte, error) {
	wrapper := map[string]any{"method": r.Method}
	if !populateWrapperA(r, wrapper) {
		populateWrapperB(r, wrapper)
	}
	return json.Marshal(wrapper)
}

// populateWrapperA flattens the args struct for the first half of the
// method discriminator. Returns true when r.Method was handled here so
// the caller can skip the second half. Split out of MarshalJSON to keep
// each discriminator switch under the complexity budget.
func populateWrapperA(r Request, wrapper map[string]any) bool {
	switch r.Method {
	case MethodRepoRegister:
		if r.RepoRegister != nil {
			wrapper["path"] = r.RepoRegister.Path
			wrapper["name"] = r.RepoRegister.Name
		}
	case MethodWatcherStart:
		if r.WatcherStart != nil {
			wrapper["repo_path"] = r.WatcherStart.RepoPath
		}
	case MethodWatcherStop:
		if r.WatcherStop != nil {
			wrapper["repo_path"] = r.WatcherStop.RepoPath
		}
	case MethodWorktreeList:
		if r.WorktreeList != nil {
			wrapper["repo_path"] = r.WorktreeList.RepoPath
		}
	case MethodWorktreeFinalize:
		if r.WorktreeFinalize != nil {
			wrapper["repo_path"] = r.WorktreeFinalize.RepoPath
			wrapper["worktree_path"] = r.WorktreeFinalize.WorktreePath
			wrapper["inherited_env"] = r.WorktreeFinalize.InheritedEnv
		}
	default:
		return false
	}
	return true
}

// populateWrapperB handles the remaining methods not matched by
// populateWrapperA.
func populateWrapperB(r Request, wrapper map[string]any) {
	switch r.Method {
	case MethodWorktreeTeardown:
		if r.WorktreeTeardown != nil {
			wrapper["repo_path"] = r.WorktreeTeardown.RepoPath
			wrapper["worktree_path"] = r.WorktreeTeardown.WorktreePath
			wrapper["force"] = r.WorktreeTeardown.Force
			wrapper["inherited_env"] = r.WorktreeTeardown.InheritedEnv
		}
	case MethodConfigReload:
		if r.ConfigReload != nil {
			wrapper["repo_path"] = r.ConfigReload.RepoPath
		}
	case MethodRepoRemove:
		if r.RepoRemove != nil {
			wrapper["repo_path"] = r.RepoRemove.RepoPath
			wrapper["force"] = r.RepoRemove.Force
		}
	case MethodSyncNow:
		if r.SyncNow != nil {
			wrapper["path"] = r.SyncNow.Path
		}
	case MethodSyncStatus:
		if r.SyncStatus != nil {
			wrapper["repo_path"] = r.SyncStatus.RepoPath
		}
	case MethodEventSubscribe:
		if r.EventSubscribe != nil {
			wrapper["repo_path"] = r.EventSubscribe.RepoPath
			wrapper["worktree_path"] = r.EventSubscribe.WorktreePath
			wrapper["levels"] = r.EventSubscribe.Levels
			wrapper["event_types"] = r.EventSubscribe.EventTypes
			wrapper["phases"] = r.EventSubscribe.Phases
			wrapper["run_id"] = r.EventSubscribe.RunID
		}
	}
}

// UnmarshalJSON parses the flat tagged shape back into Request +
// the matching args struct.
func (r *Request) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m, ok := raw["method"]
	if !ok {
		return errors.New("rpc: missing `method`")
	}
	if err := json.Unmarshal(m, &r.Method); err != nil {
		return err
	}
	switch r.Method {
	case MethodStatus, MethodPing, MethodWatcherList, MethodShutdown, MethodDaemonState:
		return nil
	case MethodRepoRegister:
		r.RepoRegister = &RepoRegisterArgs{}
		return decodeFields(raw, map[string]any{
			"path": &r.RepoRegister.Path,
			"name": &r.RepoRegister.Name,
		})
	case MethodWatcherStart:
		r.WatcherStart = &WatcherStartArgs{}
		return decodeFields(raw, map[string]any{"repo_path": &r.WatcherStart.RepoPath})
	case MethodWatcherStop:
		r.WatcherStop = &WatcherStopArgs{}
		return decodeFields(raw, map[string]any{"repo_path": &r.WatcherStop.RepoPath})
	case MethodWorktreeList:
		r.WorktreeList = &WorktreeListArgs{}
		return decodeFields(raw, map[string]any{"repo_path": &r.WorktreeList.RepoPath})
	case MethodWorktreeFinalize:
		r.WorktreeFinalize = &WorktreeFinalizeArgs{}
		return decodeFields(raw, map[string]any{
			"repo_path":     &r.WorktreeFinalize.RepoPath,
			"worktree_path": &r.WorktreeFinalize.WorktreePath,
			"inherited_env": &r.WorktreeFinalize.InheritedEnv,
		})
	case MethodWorktreeTeardown:
		r.WorktreeTeardown = &WorktreeTeardownArgs{}
		return decodeFields(raw, map[string]any{
			"repo_path":     &r.WorktreeTeardown.RepoPath,
			"worktree_path": &r.WorktreeTeardown.WorktreePath,
			"force":         &r.WorktreeTeardown.Force,
			"inherited_env": &r.WorktreeTeardown.InheritedEnv,
		})
	case MethodConfigReload:
		r.ConfigReload = &ConfigReloadArgs{}
		return decodeFields(raw, map[string]any{"repo_path": &r.ConfigReload.RepoPath})
	case MethodRepoRemove:
		r.RepoRemove = &RepoRemoveArgs{}
		return decodeFields(raw, map[string]any{
			"repo_path": &r.RepoRemove.RepoPath,
			"force":     &r.RepoRemove.Force,
		})
	case MethodSyncNow:
		r.SyncNow = &SyncNowArgs{}
		return decodeFields(raw, map[string]any{"path": &r.SyncNow.Path})
	case MethodSyncStatus:
		r.SyncStatus = &SyncStatusArgs{}
		return decodeFields(raw, map[string]any{"repo_path": &r.SyncStatus.RepoPath})
	case MethodEventSubscribe:
		r.EventSubscribe = &EventSubscribeArgs{}
		return decodeFields(raw, map[string]any{
			"repo_path":     &r.EventSubscribe.RepoPath,
			"worktree_path": &r.EventSubscribe.WorktreePath,
			"levels":        &r.EventSubscribe.Levels,
			"event_types":   &r.EventSubscribe.EventTypes,
			"phases":        &r.EventSubscribe.Phases,
			"run_id":        &r.EventSubscribe.RunID,
		})
	default:
		return fmt.Errorf("rpc: unknown method %q", r.Method)
	}
}

func decodeFields(raw map[string]json.RawMessage, into map[string]any) error {
	for k, dst := range into {
		if v, ok := raw[k]; ok {
			if err := json.Unmarshal(v, dst); err != nil {
				return fmt.Errorf("field %s: %w", k, err)
			}
		}
	}
	return nil
}

// RepoRegisterArgs — Register or update a repo.
type RepoRegisterArgs struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// WatcherStartArgs / WatcherStopArgs — control the per-repo watcher.
type (
	WatcherStartArgs struct{ RepoPath string }
	WatcherStopArgs  struct{ RepoPath string }
)

// WorktreeListArgs — list the watched linked-worktrees for a repo.
type WorktreeListArgs struct{ RepoPath string }

// WorktreeFinalizeArgs — daemon runs postcreate hooks + prepare in
// its tokio-equivalent so the CLI can return immediately.
type WorktreeFinalizeArgs struct {
	RepoPath     string
	WorktreePath string
	InheritedEnv map[string]string
}

// WorktreeTeardownArgs — daemon runs predelete hooks + DB teardown
// + git worktree remove asynchronously.
type WorktreeTeardownArgs struct {
	RepoPath     string
	WorktreePath string
	Force        bool
	InheritedEnv map[string]string
}

// ConfigReloadArgs — tells the daemon to invalidate its config cache
// and restart watchers. Empty RepoPath reloads every registered repo.
type ConfigReloadArgs struct {
	RepoPath string
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
type EventSubscribeArgs struct {
	RepoPath     string   `json:"repo_path,omitempty"`
	WorktreePath string   `json:"worktree_path,omitempty"`
	Levels       []string `json:"levels,omitempty"`
	EventTypes   []string `json:"event_types,omitempty"`
	Phases       []string `json:"phases,omitempty"`
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
// touched — callers wanting a destructive purge run `treeman wt delete`
// first.
type RepoRemoveArgs struct {
	RepoPath string
	Force    bool
}

// ─────────────────────────── Response ───────────────────────────

// Response kinds. Wire shape: {"kind": "ok", ...}.
const (
	KindOk                     = "ok"
	KindPong                   = "pong"
	KindStatus                 = "status"
	KindRepoRegistered         = "repo_registered"
	KindWatcherStarted         = "watcher_started"
	KindWatcherStopped         = "watcher_stopped"
	KindWatcherList            = "watcher_list"
	KindWorktreeList           = "worktree_list"
	KindWorktreeFinalizeQueued = "worktree_finalize_queued"
	KindWorktreeTeardownQueued = "worktree_teardown_queued"
	KindSyncResult             = "sync_result"
	KindSyncStatus             = "sync_status"
	KindDaemonState            = "daemon_state"
	// KindEvent is the per-event envelope emitted on a streaming
	// MethodEventSubscribe response. One emitted per matching event;
	// the subscription has no terminal "done" envelope — it ends when
	// the connection closes.
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
	// Finalize / Teardown queued
	WorktreePath string `json:"worktree_path,omitempty"`
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

// SubscribeEvents opens a streaming event subscription. Returns an
// already-running goroutine that feeds matching events down the
// returned channel until ctx cancels, the daemon disconnects, or the
// caller calls cancel. The channel is closed when the subscription
// ends.
//
// Unlike Call, this dials with NO deadline on the connection — the
// subscription is intentionally long-lived. ctx-cancel is the only
// supported shutdown.
//
// Returns (nil, nil, err) on dial / initial-handshake failure so
// callers can fall back to a polling path.
func SubscribeEvents(ctx context.Context, args EventSubscribeArgs) (<-chan EventEnvelope, func(), error) {
	path, err := SocketPath()
	if err != nil {
		return nil, nil, err
	}
	d := net.Dialer{Timeout: 1500 * time.Millisecond}
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, nil, fmt.Errorf("dial %s: %w", path, err)
	}
	req := Request{Method: MethodEventSubscribe, EventSubscribe: &args}
	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("encode subscribe: %w", err)
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

	go func() {
		<-subCtx.Done()
		_ = conn.Close()
	}()

	go func() {
		defer close(out)
		defer cancel()
		dec := json.NewDecoder(conn)
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
	}()

	return out, stop, nil
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
		return Response{}, fmt.Errorf("dial %s — is treemand running? %w", path, err)
	}
	defer func() { _ = conn.Close() }()

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
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
	return resp, nil
}
