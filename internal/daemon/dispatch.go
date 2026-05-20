package daemon

import (
	"context"

	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/version"
)

// Dispatch executes one RPC request against the live state, returning
// the response to send back over the socket. Mirrors the `dispatch`
// fn in `crates/treeman-daemon/src/main.rs`.
//
// The shutdown channel is closed when a `Shutdown` request fires,
// signalling the main loop to bail.
func Dispatch(ctx context.Context, st *State, shutdown chan<- struct{}, req rpc.Request) rpc.Response {
	switch req.Method {
	case rpc.MethodPing:
		return rpc.Response{Kind: rpc.KindPong}

	case rpc.MethodStatus:
		return rpc.Response{
			Kind:            rpc.KindStatus,
			ProtocolVersion: rpc.ProtocolVersion,
			DaemonVersion:   version.Version,
			Pid:             st.PID,
			StartedAtUnix:   st.StartedAtUnix,
			WatcherCount:    st.WatcherCount(),
		}

	case rpc.MethodShutdown:
		select {
		case shutdown <- struct{}{}:
		default:
		}
		return rpc.Response{Kind: rpc.KindOk}

	case rpc.MethodRepoRegister:
		if req.RepoRegister == nil {
			return errResp("repo_register: missing args")
		}
		id, err := st.Store.EnsureRepo(ctx, req.RepoRegister.Path, req.RepoRegister.Name)
		if err != nil {
			return errResp(err.Error())
		}
		return rpc.Response{Kind: rpc.KindRepoRegistered, RepoID: id}

	case rpc.MethodWatcherList:
		s := st.ListWatchers()
		out := make([]rpc.WatcherSummary, len(s))
		for i, e := range s {
			out[i] = rpc.WatcherSummary{Repo: e.Repo, WorktreeCount: e.WorktreeCount}
		}
		return rpc.Response{Kind: rpc.KindWatcherList, Repos: out}

	case rpc.MethodWatcherStart, rpc.MethodWatcherStop, rpc.MethodWorktreeList,
		rpc.MethodWorktreeFinalize, rpc.MethodWorktreeTeardown:
		// Phases 8/10 wire these up. For now report a stub error
		// the CLI can surface clearly during the bring-up window.
		return errResp("not implemented yet (Go port phase " + phaseFor(req.Method) + ")")

	default:
		return errResp("unknown method: " + req.Method)
	}
}

func phaseFor(method string) string {
	switch method {
	case rpc.MethodWatcherStart, rpc.MethodWatcherStop, rpc.MethodWorktreeList:
		return "10 (watcher)"
	case rpc.MethodWorktreeFinalize, rpc.MethodWorktreeTeardown:
		return "8/10 (prepare + daemon supervisor)"
	}
	return "TBD"
}

func errResp(msg string) rpc.Response {
	return rpc.Response{Kind: rpc.KindError, Message: msg}
}
