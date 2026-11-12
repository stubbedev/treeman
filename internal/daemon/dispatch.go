package daemon

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/runid"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
	"github.com/stubbedev/treeman/internal/version"
	"github.com/stubbedev/treeman/internal/watcher"
	"github.com/stubbedev/treeman/internal/wt"
	"github.com/stubbedev/treeman/pkg/rpc"
)

// Dispatch executes one RPC request against the live state, returning
// the response to send back over the socket. Every response is stamped
// with the daemon's protocol + version (stampIdentity) so the CLI can
// flag a stale daemon instead of failing on a decode/unknown-method
// error it can't act on.
//
// The shutdown channel is closed when a `Shutdown` request fires,
// signalling the main loop to bail.
func Dispatch(ctx context.Context, st *State, shutdown chan<- struct{}, req rpc.Request) (resp rpc.Response) {
	defer func() { stampIdentity(&resp) }()
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
		return handleRepoRegister(ctx, st, req)

	case rpc.MethodWatcherList:
		return handleWatcherList(st)

	case rpc.MethodWatcherStart:
		return handleWatcherStart(ctx, st, req)

	case rpc.MethodWatcherStop:
		return handleWatcherStop(st, req)

	case rpc.MethodConfigReload:
		return handleConfigReload(st, req)

	case rpc.MethodRepoRemove:
		return handleRepoRemove(ctx, st, req)

	case rpc.MethodWorktreeList:
		return handleWorktreeList(ctx, st, req)

	case rpc.MethodSyncNow:
		return handleSyncNow(ctx, st, req)

	case rpc.MethodSyncStatus:
		return handleSyncStatus(ctx, st, req)

	case rpc.MethodDaemonState:
		return handleDaemonState(st)

	case rpc.MethodRunPlan:
		return handleRunPlan(ctx, st, req)

	default:
		return errResp("unknown method: " + req.Method)
	}
}

// stampIdentity fills the compatibility fields the CLI's protocol
// gate checks on every response. Applied after Dispatch so even an
// unknown-method error response announces the daemon's protocol —
// that's the exact situation where the user needs the restart hint
// instead of a bare error.
func stampIdentity(resp *rpc.Response) {
	resp.ProtocolVersion = rpc.ProtocolVersion
	resp.DaemonVersion = version.Version
}

// handleRepoRegister ensures the repo row exists and returns its ID.
func handleRepoRegister(ctx context.Context, st *State, req rpc.Request) rpc.Response {
	if req.RepoRegister == nil {
		return errResp("repo_register: missing args")
	}
	id, err := st.Store.EnsureRepo(ctx, req.RepoRegister.Path, req.RepoRegister.Name)
	if err != nil {
		return errResp(err.Error())
	}
	return rpc.Response{Kind: rpc.KindRepoRegistered, RepoID: id}
}

// handleWatcherList snapshots the running watcher set into the RPC
// summary shape.
func handleWatcherList(st *State) rpc.Response {
	s := st.ListWatchers()
	out := make([]rpc.WatcherSummary, len(s))
	for i, e := range s {
		out[i] = rpc.WatcherSummary{Repo: e.Repo, WorktreeCount: e.WorktreeCount}
	}
	return rpc.Response{Kind: rpc.KindWatcherList, Repos: out}
}

// handleWatcherStart validates args and spins up the per-repo watcher.
func handleWatcherStart(ctx context.Context, st *State, req rpc.Request) rpc.Response {
	if req.WatcherStart == nil {
		return errResp("watcher_start: missing args")
	}
	if err := startRepoWatcher(ctx, st, req.WatcherStart.RepoPath); err != nil {
		return errResp(err.Error())
	}
	return rpc.Response{Kind: rpc.KindOk}
}

// handleWatcherStop validates args and unregisters the per-repo watcher.
func handleWatcherStop(st *State, req rpc.Request) rpc.Response {
	if req.WatcherStop == nil {
		return errResp("watcher_stop: missing args")
	}
	st.UnregisterWatcher(req.WatcherStop.RepoPath)
	return rpc.Response{Kind: rpc.KindOk}
}

// handleConfigReload triggers a global or per-repo config reload.
func handleConfigReload(st *State, req rpc.Request) rpc.Response {
	repoPath := ""
	if req.ConfigReload != nil {
		repoPath = req.ConfigReload.RepoPath
	}
	if st.ConfigReloader == nil {
		return errResp("config_reload: reloader not initialised")
	}
	if repoPath == "" {
		st.ConfigReloader.ReloadAll(st.BgCtx)
	} else {
		st.ConfigReloader.ReloadRepo(st.BgCtx, repoPath)
	}
	return rpc.Response{Kind: rpc.KindOk}
}

// handleRepoRemove validates args and removes a repo from the registry.
func handleRepoRemove(ctx context.Context, st *State, req rpc.Request) rpc.Response {
	if req.RepoRemove == nil || req.RepoRemove.RepoPath == "" {
		return errResp("repo_remove: missing repo_path")
	}
	if err := removeRepoFromRegistry(ctx, st, req.RepoRemove.RepoPath, req.RepoRemove.Force); err != nil {
		return errResp(err.Error())
	}
	return rpc.Response{Kind: rpc.KindOk}
}

// handleWorktreeList validates args and lists the repo's active worktrees.
func handleWorktreeList(ctx context.Context, st *State, req rpc.Request) rpc.Response {
	if req.WorktreeList == nil {
		return errResp("worktree_list: missing args")
	}
	paths, err := listWorktreePaths(ctx, st, req.WorktreeList.RepoPath)
	if err != nil {
		return errResp(err.Error())
	}
	return rpc.Response{
		Kind:      rpc.KindWorktreeList,
		RepoPath:  req.WorktreeList.RepoPath,
		Worktrees: paths,
	}
}

// handleSyncNow runs an on-demand fetch/merge sweep for the target.
func handleSyncNow(ctx context.Context, st *State, req rpc.Request) rpc.Response {
	target := ""
	if req.SyncNow != nil {
		target = req.SyncNow.Path
	}
	statuses, errs := SyncNow(ctx, st, target)
	return rpc.Response{
		Kind:        rpc.KindSyncResult,
		SyncedRepos: statuses,
		SyncErrors:  errs,
	}
}

// IsStreamingMethod reports whether req.Method is a one-request →
// many-response streaming subscription. The socket accept loop checks
// this before dispatching so streaming methods skip the one-shot
// Dispatch path and route through DispatchStreaming instead.
func IsStreamingMethod(method string) bool {
	return method == rpc.MethodEventSubscribe
}

// DispatchStreaming handles a streaming RPC method. Writes Response
// envelopes to conn as events arrive until ctx cancels, the client
// closes (next write fails), or the underlying subscription ends.
// Returns when the connection should be torn down. Errors are logged
// + swallowed; the connection close terminates the stream.
func DispatchStreaming(ctx context.Context, st *State, conn net.Conn, req rpc.Request) {
	switch req.Method {
	case rpc.MethodEventSubscribe:
		streamEvents(ctx, st, conn, req)
	default:
		// Best-effort error envelope; a failed write means the client
		// has likely already closed.
		if err := json.NewEncoder(conn).
			Encode(&rpc.Response{Kind: rpc.KindError, Message: "not a streaming method: " + req.Method}); err != nil {
			slog.Debug("streaming error envelope dropped", "method", req.Method, "err", err)
		}
	}
}

// streamEvents registers a hook on st.Store that filters events
// against args, acknowledges the live subscription with one
// KindSubscribed frame, and writes matching events to conn as KindEvent
// responses.
// Blocks until ctx cancels, the client closes, or a write fails.
// Filter semantics: every non-empty field is AND-combined; empty
// fields match everything (matches logs_query exactly).
//
// Writes are batched: whenever an event arrives, every envelope
// already queued behind it is coalesced into a single conn.Write, so
// a burst of events costs one syscall instead of one per event. The
// wire bytes are identical to one-Encode-per-event — newline-
// delimited JSON frames.
func streamEvents(ctx context.Context, st *State, conn net.Conn, req rpc.Request) {
	// The client never writes after its subscribe request, so a read
	// returning means it hung up. Without this, a subscription whose
	// filter stops matching (a foreground command's run ended) never
	// writes again, never sees the closed socket, and leaks its
	// goroutine and event hook for the daemon's lifetime.
	ctx, hangup := context.WithCancel(ctx)
	defer hangup()
	safeGo(lblSubscribeHangup, "", func() {
		_, _ = io.Copy(io.Discard, conn)
		hangup()
	})

	args := rpc.EventSubscribeArgs{}
	if req.EventSubscribe != nil {
		args = *req.EventSubscribe
	}
	repoID, worktreeID, err := resolveSubscribeIDs(ctx, st, args)
	if err != nil {
		refusal := rpc.Response{Kind: rpc.KindError, Message: err.Error()}
		stampIdentity(&refusal)
		if werr := json.NewEncoder(conn).Encode(&refusal); werr != nil {
			slog.Warn("event subscribe: send refusal", "err", werr, "refusal", err)
		}
		return
	}

	// Buffered channel keeps the hook fast even when the socket peer
	// is slow. On overflow the oldest queued event is dropped, so the
	// newest — a run's terminal plan:end, which `worktree wait` blocks
	// on — always gets through (the agent sees the gap, not a stall).
	const bufSize = 256
	ch := make(chan rpc.EventEnvelope, bufSize)
	// Hook id combines a channel-pointer fingerprint (already unique
	// per allocation) with random bytes so two subscriptions whose
	// channel addresses happen to alias after GC can't share or
	// clobber registrations. crypto/rand is overkill but cheap once.
	var salt [4]byte
	_, _ = cryptorand.Read(salt[:])
	hookID := fmt.Sprintf("sub:%p:%x", ch, salt[:])
	filter := buildSubscribeFilter(args, repoID, worktreeID)

	st.Store.RegisterEventHook(hookID, func(ev store.Event) {
		if !filter(ev) {
			return
		}
		offerNewest(ch, toEnvelope(ev))
	})
	defer st.Store.UnregisterEventHook(hookID)

	// Handshake: the hook is live, so tell the client. SubscribeEvents
	// blocks on this frame — a caller that dispatches work right after
	// subscribing can never race the registration above and miss the
	// run's terminal event.
	ack := rpc.Response{Kind: rpc.KindSubscribed}
	stampIdentity(&ack)
	if err := json.NewEncoder(conn).Encode(&ack); err != nil {
		return
	}

	// One reusable frame buffer per subscription: batches encode into
	// it and land on the conn in a single write. json.Encoder reuses
	// its internal scratch across Encode calls, so steady-state per-
	// event encoding allocates nothing.
	buf := new(bytes.Buffer)
	enc := json.NewEncoder(buf)

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			buf.Reset()
			encodeEventBatch(enc, ev, ch)
			if _, err := conn.Write(buf.Bytes()); err != nil {
				return
			}
		}
	}
}

// offerNewest queues env without ever blocking the event writer: on a
// full buffer it drops the oldest queued envelope and retries, so the
// newest event always lands.
func offerNewest(ch chan rpc.EventEnvelope, env rpc.EventEnvelope) {
	for {
		select {
		case ch <- env:
			return
		default:
		}
		select {
		case <-ch:
		default:
		}
	}
}

// encodeEventBatch frames `first` plus every envelope already queued
// on ch into enc (bound to the caller's reusable buffer) as newline-
// delimited KindEvent responses. Non-blocking drains only — an idle
// stream returns after one frame. The caller resets the buffer between
// batches, so per-event cost is a buffered encode with no fresh
// output allocation.
func encodeEventBatch(enc *json.Encoder, first rpc.EventEnvelope, ch <-chan rpc.EventEnvelope) {
	ev := first
	for {
		resp := rpc.Response{Kind: rpc.KindEvent, Event: &ev}
		if err := enc.Encode(&resp); err != nil {
			// json.Encoder into a bytes.Buffer cannot fail on this
			// concrete type; stop rather than desync the frame stream.
			return
		}
		if len(ch) == 0 {
			return
		}
		select {
		case ev = <-ch:
		default:
			return
		}
	}
}

// resolveSubscribeIDs resolves the repo/worktree path filters to ids
// once before the hot path, so every WriteEvent doesn't re-look-up.
func resolveSubscribeIDs(ctx context.Context, st *State, args rpc.EventSubscribeArgs) (repoID, worktreeID int64, err error) {
	if args.RepoPath != "" {
		repoID, err = st.Store.LookupRepoID(ctx, args.RepoPath)
		if err != nil {
			return 0, 0, fmt.Errorf("resolve the subscription's repo %s: %w", args.RepoPath, err)
		}
		if repoID == 0 {
			return 0, 0, fmt.Errorf("subscribe to repo %s: not registered", args.RepoPath)
		}
	}
	if args.WorktreePath != "" {
		row, err := st.Store.LookupActiveWorktreeByPath(ctx, args.WorktreePath)
		if err != nil {
			return 0, 0, fmt.Errorf("resolve the subscription's worktree %s: %w", args.WorktreePath, err)
		}
		if row.ID == 0 {
			return 0, 0, fmt.Errorf("subscribe to worktree %s: not registered", args.WorktreePath)
		}
		worktreeID = row.ID
	}
	return repoID, worktreeID, nil
}

// buildSubscribeFilter closes over the resolved args + ids and returns
// the per-event predicate. Extracted from streamEvents to keep the
// outer function under the cyclomatic-complexity budget.
func buildSubscribeFilter(args rpc.EventSubscribeArgs, repoID, worktreeID int64) func(store.Event) bool {
	// Match SQLite LIKE semantics for PayloadLike: wrap a bare needle in
	// `%…%` so callers can pass either an LIKE-style pattern (`%key%`)
	// or a plain substring. Mirrors store.EventFilter.PayloadLike's
	// behaviour so push and poll mode produce identical results.
	payloadNeedle := args.PayloadLike
	if payloadNeedle != "" && !strings.ContainsAny(payloadNeedle, "%_") {
		payloadNeedle = "%" + payloadNeedle + "%"
	}
	return func(ev store.Event) bool {
		if repoID != 0 && (!ev.RepoID.Valid || ev.RepoID.Int64 != repoID) {
			return false
		}
		if worktreeID != 0 && (!ev.WorktreeID.Valid || ev.WorktreeID.Int64 != worktreeID) {
			return false
		}
		if len(args.Levels) > 0 && !containsCI(args.Levels, ev.Level) {
			return false
		}
		if len(args.EventTypes) > 0 && !containsCI(args.EventTypes, ev.EventType) {
			return false
		}
		if len(args.Phases) > 0 && !containsCI(args.Phases, ev.Phase) {
			return false
		}
		if payloadNeedle != "" && !payloadLikeMatch(ev.PayloadJSON, payloadNeedle) {
			return false
		}
		if args.RunID != "" && !payloadHasRunID(ev.PayloadJSON, args.RunID) {
			return false
		}
		return true
	}
}

// payloadLikeMatch implements the subset of SQL LIKE semantics that
// store.EventFilter.PayloadLike uses: `%` matches any run, `_` matches
// one char. Sufficient for matching against payload_json substrings;
// the daemon never sees user-controlled LIKE patterns outside this
// path so a hand-rolled matcher beats spinning up a regex.
func payloadLikeMatch(payload, pattern string) bool {
	return likeMatch(payload, pattern)
}

// likeMatch is the iterative LIKE matcher. Greedy on `%`, falling
// back via a recorded-start index — the standard trick that gives
// O(n*m) worst case for the rare nested-wildcard pattern without
// recursion overhead.
func likeMatch(s, p string) bool {
	si, pi := 0, 0
	starPi, matchSi := -1, 0
	for si < len(s) {
		switch {
		case pi < len(p) && p[pi] == '%':
			starPi = pi
			matchSi = si
			pi++
		case pi < len(p) && (p[pi] == '_' || p[pi] == s[si]):
			si++
			pi++
		case starPi != -1:
			pi = starPi + 1
			matchSi++
			si = matchSi
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '%' {
		pi++
	}
	return pi == len(p)
}

// containsCI reports whether needle is in haystack, case-insensitive.
// Cheap because the filter slices are typically 1-3 entries.
func containsCI(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.EqualFold(h, needle) {
			return true
		}
	}
	return false
}

// payloadHasRunID reports whether the payload_json string carries
// `"run_id":"<id>"`. Substring check is good enough — run_ids are
// 8-char hex so collisions with other fields are vanishingly rare.
func payloadHasRunID(payload, runID string) bool {
	return strings.Contains(payload, `"run_id":"`+runID+`"`)
}

// toEnvelope flattens a store.Event onto the wire EventEnvelope shape.
func toEnvelope(ev store.Event) rpc.EventEnvelope {
	out := rpc.EventEnvelope{
		ID:          ev.ID,
		Ts:          ev.Ts,
		Level:       ev.Level,
		EventType:   ev.EventType,
		Phase:       ev.Phase,
		Message:     ev.Message,
		PayloadJSON: ev.PayloadJSON,
	}
	if ev.RepoID.Valid {
		out.RepoID = ev.RepoID.Int64
	}
	if ev.WorktreeID.Valid {
		out.WorktreeID = ev.WorktreeID.Int64
	}
	if ev.DurationMs.Valid {
		out.DurationMs = ev.DurationMs.Int64
	}
	return out
}

// handleDaemonState assembles a rich runtime view of the daemon's
// in-memory state — watcher set, in-flight finalize/teardown work,
// per-repo sync backoff timers. Used by MCP's daemon_state tool so an
// agent can reason about "is the daemon already busy with X?" without
// reading event logs.
func handleDaemonState(st *State) rpc.Response {
	now := time.Now()
	watchers := st.ListWatchers()
	repos := make([]rpc.WatcherSummary, len(watchers))
	for i, w := range watchers {
		repos[i] = rpc.WatcherSummary{Repo: w.Repo, WorktreeCount: w.WorktreeCount}
	}
	finalizes := st.SnapshotInFlightFinalizes()
	inFlightFinalizes := make([]rpc.InFlightWork, 0, len(finalizes))
	for p, startedAt := range finalizes {
		inFlightFinalizes = append(inFlightFinalizes, rpc.InFlightWork{
			WorktreePath:  p,
			StartedAtUnix: startedAt.Unix(),
			AgeSeconds:    int64(now.Sub(startedAt).Seconds()),
		})
	}
	backoffs := st.SnapshotSyncBackoffs()
	backoffList := make([]rpc.SyncBackoffEntry, 0, len(backoffs))
	for repo, e := range backoffs {
		backoffList = append(backoffList, rpc.SyncBackoffEntry{
			RepoPath:       repo,
			ConsecFailures: e.Failures,
			NextRetryUnix:  e.NextRetryUnix,
		})
	}
	return rpc.Response{
		Kind: rpc.KindDaemonState,
		State: &rpc.DaemonStateSnapshot{
			WatcherCount:      st.WatcherCount(),
			Watchers:          repos,
			WorktreeWatchers:  st.ListWtWatcherPaths(),
			LifecycleWatchers: st.ListLifecycleWatcherPaths(),
			InFlightFinalizes: inFlightFinalizes,
			InFlightTeardowns: st.SnapshotInFlightTeardowns(),
			SyncBackoffs:      backoffList,
			SyncLastSkips:     st.SnapshotSyncLastSkips(),
		},
	}
}

// handleSyncStatus returns a snapshot of per-repo sync state.
func handleSyncStatus(ctx context.Context, st *State, req rpc.Request) rpc.Response {
	filter := ""
	if req.SyncStatus != nil {
		filter = req.SyncStatus.RepoPath
	}
	statuses, err := SyncStatusSnapshot(ctx, st, filter)
	if err != nil {
		return errResp(err.Error())
	}
	return rpc.Response{
		Kind:        rpc.KindSyncStatus,
		SyncedRepos: statuses,
	}
}

func errResp(msg string) rpc.Response {
	return rpc.Response{Kind: rpc.KindError, Message: msg}
}

// listWorktreePaths returns the active worktree paths for a repo,
// either filtered by repoPath (when supplied) or every active one.
// Reads directly from SQLite — the daemon's source of truth for the
// worktree registry.
func listWorktreePaths(ctx context.Context, st *State, repoPath string) ([]string, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if repoPath == "" {
		rows, err = st.Store.DB.QueryContext(ctx,
			`SELECT path FROM worktrees WHERE deleted_at IS NULL ORDER BY id`)
	} else {
		rows, err = st.Store.DB.QueryContext(ctx, `
			SELECT w.path FROM worktrees w
			JOIN repos r ON r.id = w.repo_id
			WHERE w.deleted_at IS NULL AND r.path = ? COLLATE NOCASE
			ORDER BY w.id`, repoPath)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ResumeRepoWatcher is the public boot-time entrypoint; daemon main
// loops over `ListRepoPaths` and calls this. Delegates to the same
// path the WatcherStart RPC uses.
func ResumeRepoWatcher(ctx context.Context, st *State, repoPath string) error {
	return startRepoWatcher(ctx, st, repoPath)
}

// ResumeWorktreeWatcher (re)spawns the per-worktree fsnotify watcher
// for a live worktree on daemon boot. No-op when already registered.
func ResumeWorktreeWatcher(ctx context.Context, st *State, repoPath, wtPath string) error {
	return startWorktreeWatcher(ctx, st, repoPath, wtPath)
}

// startRepoWatcher attaches the per-repo background services that
// don't live per-worktree: the config reloader subscription and the
// lifecycle watcher (which observes `git worktree add/remove` events
// fired outside the treeman CLI). Filesystem-event watching lives
// per-worktree in `startWorktreeWatcher` because migrations and
// dumps follow each worktree's branch checkout.
//
// Runs until the WatcherEntry's cancel is invoked or the daemon
// shuts down. State tracking lets `watcher_list` / `status` report
// the running set.
func startRepoWatcher(ctx context.Context, st *State, repoPath string) error {
	if repoPath == "" {
		return errors.New("watcher_start: empty repo_path")
	}
	if _, err := resolve.LoadResolved(repoPath); err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	repoID, err := st.Store.EnsureRepo(ctx, repoPath, filepath.Base(repoPath))
	if err != nil {
		return fmt.Errorf("ensure repo: %w", err)
	}
	// Subscribe the config reloader to this repo's YAML files so a
	// live edit will trigger a watcher restart for this repo.
	st.ConfigReloader.AddRepo(repoPath)

	_, cancel := context.WithCancel(st.BgCtx)
	entry := &WatcherEntry{
		RepoPath:      repoPath,
		WorktreeCount: 0,
		Cancel:        cancel,
	}
	st.RegisterWatcher(repoPath, entry)

	// Lifecycle watcher: always on. Worktree add/remove events outside
	// of the treeman CLI need the daemon to react regardless of any
	// opt-in toggle.
	if err := EnsureLifecycleWatcher(ctx, st, repoID, repoPath); err != nil {
		slog.Warn("lifecycle watcher start failed", "repo", repoPath, "err", err)
	}

	slog.Info("repo watcher started", "repo", repoPath)
	return nil
}

// startWorktreeWatcher spawns the per-worktree watchers:
//
//   - HEAD watcher (always-on): tails .git/HEAD and re-runs finalize
//     when the user switches branches inside an existing worktree, so
//     patches re-evaluate against the new branch's slug and prepare
//     picks up any new migrations / dump.
//   - FS watcher (when `watcher.paths` is non-empty): watches the
//     configured globs and re-runs finalize on edits.
//
// Idempotent — a second call for the same wtPath is a no-op.
func startWorktreeWatcher(ctx context.Context, st *State, repoPath, wtPath string) error {
	if wtPath == "" {
		return errors.New("watcher_start: empty worktree_path")
	}
	if st.HasWtWatcher(wtPath) {
		return nil
	}
	// A teardown in flight will rm -rf this checkout shortly; starting
	// a watcher now just feeds the dispatcher REMOVE events that
	// re-spawn FinalizeWorktree against a dying tree.
	if st.IsTeardownInFlight(wtPath) {
		return nil
	}
	cfg, err := resolve.LoadResolvedForWorktree(repoPath, wtPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	repoID, err := st.Store.EnsureRepo(ctx, repoPath, filepath.Base(repoPath))
	if err != nil {
		return fmt.Errorf("ensure repo: %w", err)
	}

	wctx, cancel := context.WithCancel(st.BgCtx)
	// Compound stop: closes both watchers when the entry is unregistered.
	stoppers := []func(){cancel}

	// HEAD watcher — always-on. 100ms debounce is plenty: git
	// rewrites HEAD atomically (tmp file + rename = one fsnotify
	// event), so we only need to coalesce against the rare ref-pack
	// race. A tight window means `git checkout` → prefetch starts
	// within ~100ms instead of ~500ms perceived latency.
	hw, err := NewHeadWatcher(wtPath, 100*time.Millisecond, func(_ context.Context, newRef string) {
		rid := runid.New()
		evCtx := runid.With(st.BgCtx, rid)
		_ = st.Store.WriteEvent(evCtx, store.LevelInfo, store.EvtWatchHead,
			"HEAD → "+newRef,
			repoID, 0, "", 0, map[string]string{
				"wt":  wtPath,
				"ref": newRef,
			})
		env, ok := inheritedEnvOrSkip(evCtx, st, repoID, wtPath, "HEAD-triggered finalize")
		if !ok {
			return
		}
		// Fire user-defined checkout actions in parallel with
		// the regular finalize re-run (no ordering — both react to
		// the same event independently).
		safeGo(lblHeadActions, wtPath, func() {
			fireTriggerActions(runid.With(st.BgCtx, rid), st, repoPath, wtPath, hooks.Checkout, env)
		})
		safeGo(lblHeadFinalize, wtPath, func() {
			if err := FinalizeWorktreeForHead(runid.With(st.BgCtx, rid), st, repoPath, wtPath, env); err != nil {
				slog.Warn("head-triggered finalize", "wt", wtPath, "err", err)
			}
		})
		// Sync the new branch against upstream — switching to a
		// stale branch otherwise leaves it stale until the next
		// auto-fetch tick (up to 15 min). Respects per-repo opt-out
		// the same way the periodic sweep does.
		safeGo(lblHeadSync, wtPath, func() {
			cfg, err := resolve.LoadResolved(repoPath)
			if err != nil || !cfg.AutoFetch.IsEnabled() {
				return
			}
			_ = SyncWorktree(runid.With(st.BgCtx, rid), st, repoID, wtPath, cfg.AutoFetch.ResolvedMode())
		})
	})
	if err != nil {
		slog.Warn("head watcher init failed", "wt", wtPath, "err", err)
	} else {
		stoppers = append(stoppers, hw.Stop)
		safeGo(lblWatcherHead, wtPath, func() {
			if err := hw.Start(wctx); err != nil {
				slog.Warn("head watcher exit", "wt", wtPath, "err", err)
			}
		})
	}

	// FS watcher — aggregate every per-DB `databases[i].watch[]`
	// entry (tagged with i = DBIndex) into a single list the fsnotify
	// driver subscribes to. Only spawn when at least one glob is
	// declared across the whole config.
	aggregatedPaths := aggregateWatches(&cfg)
	if len(aggregatedPaths) > 0 {
		dispatch := makeWtFSDispatcher(st, repoPath, repoID, wtPath)
		w, err := watcher.New(wtPath, aggregatedPaths, cfg.DebounceMs, dispatch)
		if err != nil {
			cancel()
			return fmt.Errorf("fsnotify watcher init: %w", err)
		}
		stoppers = append(stoppers, w.Stop)
		safeGo(lblWatcherFS, wtPath, func() {
			if err := w.Start(wctx); err != nil {
				slog.Warn("fsnotify watcher exit", "wt", wtPath, "err", err)
			}
		})
	}

	entry := &WatcherEntry{
		RepoPath: repoPath,
		Cancel: func() {
			// Cancel context first so any running dispatch sees ctx.Done()
			// before the underlying watchers tear down their channels.
			for _, fn := range stoppers {
				fn()
			}
		},
	}
	st.RegisterWtWatcher(wtPath, entry)
	slog.Info("worktree watcher started", "repo", repoPath, "wt", wtPath)
	return nil
}

// aggregateWatches walks every `databases[i].inputs[]` block and
// returns the flat list of paths the fsnotify driver subscribes
// to. Each entry is tagged with its DBIndex so the dispatcher can
// route the event back to its owning database. The optional
// `label:` on each input passes through for filtered hook dispatch.
func aggregateWatches(cfg *config.Config) []config.WatcherPath {
	total := 0
	for _, db := range cfg.Databases {
		total += len(db.Inputs)
	}
	out := make([]config.WatcherPath, 0, total)
	for i, db := range cfg.Databases {
		for _, in := range db.Inputs {
			out = append(out, config.WatcherPath{
				Glob:    in.Glob,
				Label:   in.Label,
				DBIndex: i,
			})
		}
	}
	return out
}

// makeWtFSDispatcher builds a watcher.Dispatcher bound to a single
// worktree. Each event re-runs finalize once per database whose inputs
// matched, scoped to that database. File-change actions fire once per
// path, with the first matching database's context. The cache-hit /
// cold-build decision is derived purely from the input fingerprint —
// no force-rebuild override.
func makeWtFSDispatcher(st *State, repoPath string, repoID int64, wtPath string) watcher.Dispatcher {
	return func(ctx context.Context, ev watcher.Event) error {
		// Drop events while a teardown is in flight — finalising a
		// worktree the user just asked to delete is a feedback loop
		// (DB writes against a dying tree, watcher re-registration,
		// etc.).
		if st.IsTeardownInFlight(wtPath) {
			return nil
		}
		// One run_id covers the watch:input event + the actions and
		// finalize fan-out that descend from it. The two safeGos use
		// st.BgCtx (so they outlive the dispatcher), so we explicitly
		// re-attach the id there too.
		rid := runid.New()
		evCtx := runid.With(ctx, rid)
		idxs := make([]string, len(ev.Matches))
		for i, m := range ev.Matches {
			idxs[i] = strconv.Itoa(m.DBIndex)
		}
		first := ev.Matches[0]
		_ = st.Store.WriteEvent(evCtx, store.LevelInfo, store.EvtWatchInput,
			fmt.Sprintf("%s (db_idx=%s label=%s)", ev.Path, strings.Join(idxs, ","), first.Label),
			repoID, 0, "", 0, map[string]string{
				"path":   ev.Path,
				"db_idx": strings.Join(idxs, ","),
				"label":  first.Label,
				"wt":     wtPath,
			})
		env, ok := inheritedEnvOrSkip(evCtx, st, repoID, wtPath, "watch re-prepare")
		if !ok {
			return nil
		}
		// Fire the file-change actions in parallel with the
		// re-prep — both react to the same event independently.
		safeGo(lblWatchActions, wtPath, func() {
			fireOnFileChange(runid.With(st.BgCtx, rid), st, repoPath, wtPath, first.DBIndex, ev.Path, first.Label, env)
		})
		safeGo(lblWatchFinalize, wtPath, func() {
			for _, m := range ev.Matches {
				if err := FinalizeWorktreeForWatch(runid.With(st.BgCtx, rid), st, repoPath, wtPath, m.DBIndex, env); err != nil {
					slog.Warn("watcher-triggered finalize", "wt", wtPath, "db_idx", m.DBIndex, "err", err)
				}
			}
		})
		return nil
	}
}

// fireOnFileChange runs the global `hooks.file-change` actions,
// filtered by the watch event's label. Actions with an empty
// `match:` fire for any watch event (any engine, any label);
// actions with `match: <label>` only fire when the event's label
// matches.
//
// The subprocess receives watch context as env vars so user scripts
// can branch on the trigger details: TREEMAN_WATCH_PATH,
// TREEMAN_WATCH_LABEL, TREEMAN_WATCH_ENGINE, TREEMAN_WATCH_DB_NAME.
//
// Main-wt aware: wt.ResolveIdentity must run BEFORE we read the
// owning database's NameTemplate, otherwise the overlay's
// main-wt-specific template wouldn't be in cfg.Databases yet and
// TREEMAN_WATCH_DB_NAME would resolve to the linked-wt template's
// rendering. Same call also forces slug.ForMain + EnsureMainWorktree
// for the repo-root path so $TREEMAN_SLUG matches the finalize path
// and the main row's slug column isn't overwritten with a path hash.
func fireOnFileChange(
	ctx context.Context,
	st *State,
	repoPath, wtPath string,
	dbIdx int,
	eventPath, label string,
	inheritedEnv map[string]string,
) {
	cfg, err := resolve.LoadResolvedForWorktree(repoPath, wtPath)
	if err != nil {
		slog.Warn("file-change: load config", "wt", wtPath, "err", err)
		return
	}
	matched := config.HooksConfig{OnFileChange: make([]config.FilteredAction, 0, len(cfg.Hooks.OnFileChange))}
	for _, fa := range cfg.Hooks.OnFileChange {
		if fa.Matches(label) {
			matched.OnFileChange = append(matched.OnFileChange, fa)
		}
	}
	if len(matched.OnFileChange) == 0 {
		return
	}
	repoID, err := st.Store.EnsureRepo(ctx, repoPath, filepath.Base(repoPath))
	if err != nil {
		slog.Warn("file-change: ensure repo", "wt", wtPath, "repo", repoPath, "err", err)
		return
	}
	id, err := wt.ResolveIdentity(ctx, st.Store, &cfg, repoPath, wtPath, detectBranch(wtPath), repoID)
	if err != nil {
		slog.Warn("file-change: resolve identity", "wt", wtPath, "err", err)
		return
	}
	sl, wtID, isMain := id.Slug, id.WtID, id.IsMain
	// Resolve the owning database's engine + rendered name so the
	// hook can branch on engine without re-parsing config. cfg has
	// already been overlay-merged by wt.ResolveIdentity, so
	// d.NameTemplate is the main-wt-specific template when relevant.
	var engine, dbName string
	if dbIdx >= 0 && dbIdx < len(cfg.Databases) {
		d := cfg.Databases[dbIdx]
		engine = d.Engine
		// Prefix-scoped engines (redis/elasticsearch) have no
		// name_template — their per-worktree namespace is the
		// key_prefix. Fall back to it so TREEMAN_WATCH_DB_NAME isn't
		// empty for those engines (and so a main_worktree key_prefix
		// overlay, already merged into cfg, reaches the hook).
		tmpl := d.NameTemplate
		if tmpl == "" {
			tmpl = d.KeyPrefix
		}
		rendered, err := template.Render(tmpl, template.FromSlug(sl))
		if err != nil {
			slog.Warn("file-change: render the database name for TREEMAN_WATCH_DB_NAME",
				"wt", wtPath, "db_idx", dbIdx, "template", tmpl, "err", err)
			return
		}
		dbName = rendered
	}
	// Layer the watch-context env on top of the user's cached env.
	env := make(map[string]string, len(inheritedEnv)+4)
	maps.Copy(env, inheritedEnv)
	env["TREEMAN_WATCH_PATH"] = eventPath
	env["TREEMAN_WATCH_LABEL"] = label
	env["TREEMAN_WATCH_ENGINE"] = engine
	env["TREEMAN_WATCH_DB_NAME"] = dbName
	if err := runTriggerActions(ctx, st, hooks.FileChange, &matched, repoPath, wtPath, sl.Value, isMain, repoID, wtID, env); err != nil {
		slog.Warn("file-change: run actions", "wt", wtPath, "err", err)
	}
}

// fireTriggerActions loads the resolved config for a worktree and
// runs the selected hooks trigger's actions. Used by the on-head-
// change and on-watch dispatch paths so user-defined hooks can
// react to those events without sharing the regular finalize logic.
// Errors are swallowed + logged; this path is best-effort.
//
// Main-wt aware via wt.ResolveIdentity: when wtPath is the repo
// root and main-wt is enabled, the hook subprocess receives
// $TREEMAN_SLUG=main_<branch> matching the finalize-path slug. A
// stale slug.For here would corrupt the main row's slug column on
// every HEAD switch (EnsureWorktree overwrites slug keyed by path).
func fireTriggerActions(
	ctx context.Context,
	st *State,
	repoPath, wtPath string,
	trigger hooks.Trigger,
	inheritedEnv map[string]string,
) {
	cfg, err := resolve.LoadResolvedForWorktree(repoPath, wtPath)
	if err != nil {
		slog.Warn("trigger actions: load config", "trigger", trigger, "wt", wtPath, "err", err)
		return
	}
	if len(trigger.Actions(&cfg.Hooks)) == 0 {
		return
	}
	repoID, err := st.Store.EnsureRepo(ctx, repoPath, filepath.Base(repoPath))
	if err != nil {
		slog.Warn("trigger actions: ensure repo",
			"trigger", trigger, "wt", wtPath, "repo", repoPath, "err", err)
		return
	}
	id, err := wt.ResolveIdentity(ctx, st.Store, &cfg, repoPath, wtPath, detectBranch(wtPath), repoID)
	if err != nil {
		slog.Warn("trigger actions: resolve identity",
			"trigger", trigger, "wt", wtPath, "err", err)
		return
	}
	if err := runTriggerActions(
		ctx,
		st,
		trigger,
		&cfg.Hooks,
		repoPath,
		wtPath,
		id.Slug.Value,
		id.IsMain,
		repoID,
		id.WtID,
		inheritedEnv,
	); err != nil {
		slog.Warn("trigger actions: run",
			"trigger", trigger, "wt", wtPath, "err", err)
	}
}

func inheritedEnvOrSkip(ctx context.Context, st *State, repoID int64, wtPath, run string) (map[string]string, bool) {
	env, err := st.Store.LoadInheritedEnvByPath(ctx, wtPath)
	if err == nil {
		return env, true
	}
	msg := fmt.Sprintf(
		"%s skipped for %s: load the environment captured at its last create/finalize: %v; run `treeman worktree finalize` in it",
		run,
		wtPath,
		err,
	)
	slog.Error(msg)
	_ = st.Store.WriteEvent(ctx, store.LevelError, store.EvtWatchSkip, msg, repoID, 0, "", 0,
		map[string]string{"wt": wtPath, "error": err.Error()})
	return nil, false
}
