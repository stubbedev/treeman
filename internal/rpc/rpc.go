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
const SocketEnv = "TREEMAN_SOCKET"
const SocketBasename = "treeman.sock"

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
	MethodShutdown         = "shutdown"
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
}

// MarshalJSON flattens the discriminator + the matching args struct
// onto a single object so the wire format is a flat tagged union
// keyed by `method`.
func (r Request) MarshalJSON() ([]byte, error) {
	wrapper := map[string]any{"method": r.Method}
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
	case MethodWorktreeTeardown:
		if r.WorktreeTeardown != nil {
			wrapper["repo_path"] = r.WorktreeTeardown.RepoPath
			wrapper["worktree_path"] = r.WorktreeTeardown.WorktreePath
			wrapper["force"] = r.WorktreeTeardown.Force
			wrapper["inherited_env"] = r.WorktreeTeardown.InheritedEnv
		}
	}
	return json.Marshal(wrapper)
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
	case MethodStatus, MethodPing, MethodWatcherList, MethodShutdown:
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
type WatcherStartArgs struct{ RepoPath string }
type WatcherStopArgs struct{ RepoPath string }

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
	KindError                  = "error"
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
	// Error
	Message string `json:"message,omitempty"`
}

// WatcherSummary — one row of the WatcherList response.
type WatcherSummary struct {
	Repo          string `json:"repo"`
	WorktreeCount uint32 `json:"worktree_count"`
}

// ─────────────────────────── client ───────────────────────────

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
	defer conn.Close()

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
