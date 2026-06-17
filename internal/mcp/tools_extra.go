package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/engineconn"
	"github.com/stubbedev/treeman/internal/engine"
	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
)

// ─── logs_wait ──────────────────────────────────────────────────────

type logsWaitIn struct {
	Repo           string   `json:"repo,omitempty"`
	Worktree       string   `json:"worktree,omitempty"        jsonschema:"slug, branch, or basename"`
	Levels         []string `json:"levels,omitempty"`
	EventTypes     []string `json:"event_types,omitempty"`
	Phases         []string `json:"phases,omitempty"`
	PayloadLike    string   `json:"payload_like,omitempty"`
	RunID          string   `json:"run_id,omitempty"          jsonschema:"correlation id; the common case — wait for events from one prepare/finalize run"`
	MinCount       int      `json:"min_count,omitempty"       jsonschema:"return after this many new events arrive (default 1)"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" jsonschema:"give up after this many seconds (default 30, max 300)"`
}

type logsWaitOut struct {
	Events   []store.EventJSON `json:"events"`
	TimedOut bool              `json:"timed_out"`
	Anchor   int64             `json:"anchor_id" jsonschema:"highest event id at the moment the wait started; only events newer than this count"`
}

// logsWaitTool blocks until min_count new events match the filter, or
// timeout fires. Anchors at the current max event id so historical
// matches don't satisfy the call instantly. Polls every 500ms — cheap
// enough that the chosen polling interval doesn't matter relative to
// the time the agent saved by not polling itself.
func logsWaitTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in logsWaitIn) (*mcpsdk.CallToolResult, logsWaitOut, error) {
	minCount := in.MinCount
	if minCount <= 0 {
		minCount = 1
	}
	timeout := time.Duration(in.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if timeout > 5*time.Minute {
		timeout = 5 * time.Minute
	}

	st, err := openStore(ctx)
	if err != nil {
		return nil, logsWaitOut{}, err
	}
	defer func() { _ = st.Close() }()

	// Build the filter once. WorktreeID resolution mirrors logs_query.
	f := store.EventFilter{
		Levels:      validateLevels(in.Levels),
		EventTypes:  in.EventTypes,
		Phases:      in.Phases,
		PayloadLike: in.PayloadLike,
		RunID:       in.RunID,
		HydrateWT:   true,
		Limit:       200,
		OldestFirst: true,
	}
	if err := applyRepoWorktreeFilter(ctx, st, &f, in.Repo, in.Worktree); err != nil {
		return nil, logsWaitOut{}, err
	}

	anchor := newestMatchingID(ctx, st, f)
	f.AfterID = anchor
	deadline := time.Now().Add(timeout)

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	var collected []store.Event
	for {
		// Try a query immediately so a fast event is returned without
		// an unnecessary tick wait.
		evs, err := st.QueryEvents(ctx, f)
		if err != nil {
			return nil, logsWaitOut{Anchor: anchor}, err
		}
		for _, e := range evs {
			e.Message = redactSecrets(e.Message)
			e.PayloadJSON = redactSecrets(e.PayloadJSON)
			collected = append(collected, e)
			if e.ID > f.AfterID {
				f.AfterID = e.ID
			}
		}
		if len(collected) >= minCount {
			return nil, logsWaitOut{Events: store.EventsJSON(collected), Anchor: anchor}, nil
		}
		if time.Now().After(deadline) {
			return nil, logsWaitOut{Events: store.EventsJSON(collected), Anchor: anchor, TimedOut: true}, nil
		}
		select {
		case <-ctx.Done():
			return nil, logsWaitOut{Events: store.EventsJSON(collected), Anchor: anchor, TimedOut: true}, ctx.Err()
		case <-tick.C:
		}
	}
}

// newestMatchingID returns the highest event id matching the filter
// at the moment of the call — the anchor every subsequent poll
// compares against. Errors collapse to 0 (treat as "no anchor"); the
// downstream loop then surfaces the real error on its next query.
func newestMatchingID(ctx context.Context, st *store.Store, f store.EventFilter) int64 {
	f.Limit = 1
	f.OldestFirst = false
	evs, err := st.QueryEvents(ctx, f)
	if err != nil || len(evs) == 0 {
		return 0
	}
	return evs[0].ID
}

// ─── logs_subscribe ─────────────────────────────────────────────────

type logsSubscribeIn struct {
	Repo           string   `json:"repo,omitempty"`
	Worktree       string   `json:"worktree,omitempty"        jsonschema:"slug, branch, or basename"`
	Levels         []string `json:"levels,omitempty"`
	EventTypes     []string `json:"event_types,omitempty"`
	Phases         []string `json:"phases,omitempty"`
	PayloadLike    string   `json:"payload_like,omitempty"`
	RunID          string   `json:"run_id,omitempty"          jsonschema:"correlation id — usually the right scope for live streaming"`
	MinCount       int      `json:"min_count,omitempty"       jsonschema:"return after this many new events arrive (default 1)"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" jsonschema:"give up after this many seconds (default 60, max 600)"`
}

// logsSubscribeOut mirrors logsWaitOut. Streaming is a side-effect via
// notifications; the final return carries the collected batch so an
// agent that ignored notifications still sees what happened. Mode
// records whether events came via daemon push or a polling fallback —
// useful for debugging "did my streaming actually stream".
type logsSubscribeOut struct {
	Events        []store.EventJSON `json:"events"`
	TimedOut      bool              `json:"timed_out"`
	Anchor        int64             `json:"anchor_id"          jsonschema:"highest event id at the moment the subscription started"`
	Notifications int               `json:"notifications_sent" jsonschema:"how many notifications were dispatched to the session"`
	Mode          string            `json:"mode"               jsonschema:"push (daemon streaming) | poll (sqlite fallback when daemon unreachable)"`
}

// logsSubscribeTool blocks until min_count new events match the filter
// (or timeout). For each matching event it fires a progress + logging
// notification on the ServerSession so an interactive client can
// surface events as they happen.
//
// Two delivery paths:
//   - push (preferred): open a streaming RPC subscription to the
//     daemon. Each WriteEvent fires a store hook that fans the event
//     down the socket — zero polling.
//   - poll (fallback): when the daemon is unreachable, fall back to
//     500ms SQLite polling. Same notification shape; agents can't tell
//     the difference except via the Mode field in the result.
func logsSubscribeTool(
	ctx context.Context,
	req *mcpsdk.CallToolRequest,
	in logsSubscribeIn,
) (*mcpsdk.CallToolResult, logsSubscribeOut, error) {
	minCount := in.MinCount
	if minCount <= 0 {
		minCount = 1
	}
	timeout := time.Duration(in.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if timeout > 10*time.Minute {
		timeout = 10 * time.Minute
	}

	// progressToken is supplied by the client in CallToolParams._meta.
	var progressToken any
	if req != nil && req.Params != nil {
		progressToken = req.Params.GetProgressToken()
	}

	// Try the daemon-push path first.
	out, ok := subscribePushPath(ctx, req, in, minCount, timeout, progressToken)
	if ok {
		return nil, out, nil
	}
	// Fallback: poll SQLite. Same shape; Mode=poll lets agents tell.
	return nil, subscribePollPath(ctx, req, in, minCount, timeout, progressToken), nil
}

// subscribePushPath opens an rpc.SubscribeEvents stream and consumes
// events as they arrive. Returns ok=false (and the caller falls back
// to polling) on dial failure — the daemon may not be running.
//
// Pristine push: every filter field in subscribePollPath is also
// passed to rpc.SubscribeEvents below, and the Anchor (= max event
// id at subscription time) is computed from the local store before
// the stream opens so the output shape matches poll exactly.
func subscribePushPath(
	ctx context.Context,
	req *mcpsdk.CallToolRequest,
	in logsSubscribeIn,
	minCount int,
	timeout time.Duration,
	progressToken any,
) (logsSubscribeOut, bool) {
	resolvedRepo, _ := resolveRepo(in.Repo)
	resolvedWT, _ := resolveWorktreePath(ctx, resolvedRepo, in.Worktree)

	// Compute the same Anchor poll mode reports. Required so agents
	// can correlate the subscription start with logs_query results
	// regardless of mode. Best-effort: an unopenable store leaves
	// Anchor=0 (subscription still works, no historical anchor).
	anchor := pushAnchor(ctx, in)

	stream, cancel, err := rpc.SubscribeEvents(ctx, rpc.EventSubscribeArgs{
		RepoPath:     resolvedRepo,
		WorktreePath: resolvedWT,
		Levels:       validateLevels(in.Levels),
		EventTypes:   in.EventTypes,
		Phases:       in.Phases,
		PayloadLike:  in.PayloadLike,
		RunID:        in.RunID,
	})
	if err != nil {
		return logsSubscribeOut{Mode: "poll"}, false
	}
	defer cancel()

	deadlineCh := time.After(timeout)
	var collected []store.Event
	notifications := 0
	for {
		select {
		case env, open := <-stream:
			if !open {
				// Stream closed by daemon (or ctx). Treat as timeout
				// if we haven't met min_count.
				timedOut := len(collected) < minCount
				return logsSubscribeOut{
					Events: store.EventsJSON(collected), Anchor: anchor, TimedOut: timedOut,
					Notifications: notifications, Mode: "push",
				}, true
			}
			ev := envelopeToEvent(env)
			ev.Message = redactSecrets(ev.Message)
			ev.PayloadJSON = redactSecrets(ev.PayloadJSON)
			collected = append(collected, ev)
			notifications += dispatchEventNotification(ctx, req, progressToken, len(collected), minCount, ev)
			if len(collected) >= minCount {
				return logsSubscribeOut{
					Events: store.EventsJSON(collected), Anchor: anchor,
					Notifications: notifications, Mode: "push",
				}, true
			}
		case <-deadlineCh:
			return logsSubscribeOut{
				Events: store.EventsJSON(collected), Anchor: anchor, TimedOut: true,
				Notifications: notifications, Mode: "push",
			}, true
		case <-ctx.Done():
			return logsSubscribeOut{
				Events: store.EventsJSON(collected), Anchor: anchor, TimedOut: true,
				Notifications: notifications, Mode: "push",
			}, true
		}
	}
}

// pushAnchor returns the highest event id matching the filter at
// subscription open time. Symmetric with poll mode's newestMatchingID
// call so the Anchor field has the same meaning across both paths.
// Errors collapse to 0 — push mode still works, the agent just sees
// "no anchor" in the result.
func pushAnchor(ctx context.Context, in logsSubscribeIn) int64 {
	st, err := openStore(ctx)
	if err != nil {
		return 0
	}
	defer func() { _ = st.Close() }()
	f := store.EventFilter{
		Levels:      validateLevels(in.Levels),
		EventTypes:  in.EventTypes,
		Phases:      in.Phases,
		PayloadLike: in.PayloadLike,
		RunID:       in.RunID,
	}
	if err := applyRepoWorktreeFilter(ctx, st, &f, in.Repo, in.Worktree); err != nil {
		return 0
	}
	return newestMatchingID(ctx, st, f)
}

// subscribePollPath is the original SQLite-polling implementation,
// retained as the daemon-down fallback.
func subscribePollPath(
	ctx context.Context,
	req *mcpsdk.CallToolRequest,
	in logsSubscribeIn,
	minCount int,
	timeout time.Duration,
	progressToken any,
) logsSubscribeOut {
	st, err := openStore(ctx)
	if err != nil {
		return logsSubscribeOut{Mode: "poll"}
	}
	defer func() { _ = st.Close() }()

	f := store.EventFilter{
		Levels:      validateLevels(in.Levels),
		EventTypes:  in.EventTypes,
		Phases:      in.Phases,
		PayloadLike: in.PayloadLike,
		RunID:       in.RunID,
		HydrateWT:   true,
		Limit:       200,
		OldestFirst: true,
	}
	if err := applyRepoWorktreeFilter(ctx, st, &f, in.Repo, in.Worktree); err != nil {
		return logsSubscribeOut{Mode: "poll"}
	}

	anchor := newestMatchingID(ctx, st, f)
	f.AfterID = anchor
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	var collected []store.Event
	notifications := 0
	for {
		evs, err := st.QueryEvents(ctx, f)
		if err != nil {
			return logsSubscribeOut{Anchor: anchor, Notifications: notifications, Mode: "poll"}
		}
		for _, e := range evs {
			e.Message = redactSecrets(e.Message)
			e.PayloadJSON = redactSecrets(e.PayloadJSON)
			collected = append(collected, e)
			if e.ID > f.AfterID {
				f.AfterID = e.ID
			}
			notifications += dispatchEventNotification(ctx, req, progressToken, len(collected), minCount, e)
		}
		if len(collected) >= minCount {
			return logsSubscribeOut{Events: store.EventsJSON(collected), Anchor: anchor, Notifications: notifications, Mode: "poll"}
		}
		if time.Now().After(deadline) {
			return logsSubscribeOut{
				Events:        store.EventsJSON(collected),
				Anchor:        anchor,
				TimedOut:      true,
				Notifications: notifications,
				Mode:          "poll",
			}
		}
		select {
		case <-ctx.Done():
			return logsSubscribeOut{
				Events:        store.EventsJSON(collected),
				Anchor:        anchor,
				TimedOut:      true,
				Notifications: notifications,
				Mode:          "poll",
			}
		case <-tick.C:
		}
	}
}

// envelopeToEvent flattens an rpc.EventEnvelope back onto a store.Event
// so notification dispatch + the final return shape match the polling
// path exactly.
func envelopeToEvent(env rpc.EventEnvelope) store.Event {
	ev := store.Event{
		ID:          env.ID,
		Ts:          env.Ts,
		Level:       env.Level,
		EventType:   env.EventType,
		Phase:       env.Phase,
		Message:     env.Message,
		PayloadJSON: env.PayloadJSON,
	}
	if env.RepoID != 0 {
		ev.RepoID.Valid = true
		ev.RepoID.Int64 = env.RepoID
	}
	if env.WorktreeID != 0 {
		ev.WorktreeID.Valid = true
		ev.WorktreeID.Int64 = env.WorktreeID
	}
	if env.DurationMs != 0 {
		ev.DurationMs.Valid = true
		ev.DurationMs.Int64 = env.DurationMs
	}
	return ev
}

// resolveWorktreePath resolves a worktree slug/branch/basename to its
// absolute path via the registry. Returns empty when nothing matches
// — the streaming subscribe args treat empty as "no worktree scope".
func resolveWorktreePath(ctx context.Context, repoRoot, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	st, err := openStore(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = st.Close() }()
	repoID, _ := st.LookupRepoID(ctx, repoRoot)
	wtID, err := st.LookupWorktreeID(ctx, repoID, name)
	if err != nil || wtID == 0 {
		return "", err
	}
	var p string
	_ = st.DB.QueryRowContext(ctx, `SELECT path FROM worktrees WHERE id = ?`, wtID).Scan(&p)
	return p, nil
}

// dispatchEventNotification fires a progress-notification (always) and
// a logging-message (best-effort) for one event. Returns the number of
// notifications successfully sent (0–2). Errors are swallowed — losing
// a notification shouldn't fail the tool call.
func dispatchEventNotification(
	ctx context.Context,
	req *mcpsdk.CallToolRequest,
	progressToken any,
	progress, total int,
	e store.Event,
) int {
	if req == nil || req.Session == nil {
		return 0
	}
	sent := 0
	msg := fmt.Sprintf("[%s] %s %s", e.Level, e.EventType, e.Message)
	if err := req.Session.NotifyProgress(ctx, &mcpsdk.ProgressNotificationParams{
		ProgressToken: progressToken,
		Message:       msg,
		Progress:      float64(progress),
		Total:         float64(total),
	}); err == nil {
		sent++
	}
	// LoggingMessage gets ignored unless the client called SetLevel —
	// best-effort.
	if err := req.Session.Log(ctx, &mcpsdk.LoggingMessageParams{
		Level:  mapEventLevel(e.Level),
		Logger: "treeman",
		Data: map[string]any{
			"event_id":   e.ID,
			"event_type": e.EventType,
			"message":    e.Message,
			"phase":      e.Phase,
			"level":      e.Level,
		},
	}); err == nil {
		sent++
	}
	return sent
}

// mapEventLevel maps treeman's level strings (debug|info|warn|error) to
// the MCP LoggingLevel enum (RFC 5424 syslog levels).
func mapEventLevel(lvl string) mcpsdk.LoggingLevel {
	switch strings.ToLower(lvl) {
	case "debug":
		return mcpsdk.LoggingLevel("debug")
	case "warn", "warning":
		return mcpsdk.LoggingLevel("warning")
	case "error":
		return mcpsdk.LoggingLevel("error")
	}
	return mcpsdk.LoggingLevel("info")
}

// ─── branches_list ──────────────────────────────────────────────────

type branchesListIn struct {
	Repo  string `json:"repo,omitempty"`
	Limit int    `json:"limit,omitempty" jsonschema:"max branches returned (default 200, max 1000)"`
}

type branchesListEntry struct {
	Name        string `json:"name"`
	HasLocal    bool   `json:"has_local"`
	HasRemote   bool   `json:"has_remote"`
	WorktreeDir string `json:"worktree_dir,omitempty" jsonschema:"path of the worktree currently checked out on this branch (if any)"`
	IsCurrent   bool   `json:"is_current,omitempty"`
}

type branchesListOut struct {
	Repo     string              `json:"repo"`
	Branches []branchesListEntry `json:"branches"`
}

// branchesListTool enumerates every git branch (local + origin-only)
// and joins against the SQLite registry so callers can see which
// branches already occupy a worktree. Mirrors the CLI's `treeman
// branches` minus the column-formatting glue.
func branchesListTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in branchesListIn) (*mcpsdk.CallToolResult, branchesListOut, error) {
	out, err := collectRepoBranches(ctx, in.Repo, in.Limit)
	if err != nil {
		return nil, branchesListOut{}, err
	}
	return nil, out, nil
}

// collectRepoBranches is the shared implementation for the
// branches_list tool and the treeman://repos/{repo}/branches
// resource. limit≤0 → default 200, capped at 1000.
func collectRepoBranches(ctx context.Context, repo string, limit int) (branchesListOut, error) {
	repoRoot, err := resolveRepo(repo)
	if err != nil {
		return branchesListOut{}, err
	}
	local, err := gitcmd.String(ctx, repoRoot, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return branchesListOut{}, fmt.Errorf("list local branches: %w", err)
	}
	remote, err := gitcmd.String(ctx, repoRoot, "for-each-ref", "--format=%(refname:short)", "refs/remotes/origin")
	if err != nil {
		return branchesListOut{}, fmt.Errorf("list remote branches: %w", err)
	}
	current, _ := gitcmd.String(ctx, repoRoot, "symbolic-ref", "--short", "HEAD")
	current = strings.TrimSpace(current)

	branchToWtDir, _ := branchOccupancyFromStore(ctx, repoRoot)

	entries := map[string]*branchesListEntry{}
	for b := range strings.SplitSeq(local, "\n") {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		entries[b] = &branchesListEntry{Name: b, HasLocal: true}
	}
	for b := range strings.SplitSeq(remote, "\n") {
		b = strings.TrimSpace(b)
		if b == "" || b == "origin/HEAD" {
			continue
		}
		name := strings.TrimPrefix(b, "origin/")
		if e, ok := entries[name]; ok {
			e.HasRemote = true
		} else {
			entries[name] = &branchesListEntry{Name: name, HasRemote: true}
		}
	}
	for name, dir := range branchToWtDir {
		if e, ok := entries[name]; ok {
			e.WorktreeDir = dir
		} else {
			entries[name] = &branchesListEntry{Name: name, WorktreeDir: dir, HasLocal: true}
		}
	}
	if current != "" {
		if e, ok := entries[current]; ok {
			e.IsCurrent = true
		}
	}

	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	if len(names) > limit {
		names = names[:limit]
	}
	out := branchesListOut{Repo: repoRoot, Branches: make([]branchesListEntry, 0, len(names))}
	for _, n := range names {
		out.Branches = append(out.Branches, *entries[n])
	}
	return out, nil
}

// ─── config_diff ────────────────────────────────────────────────────

type configDiffIn struct {
	Repo  string `json:"repo,omitempty"`
	Body  string `json:"body"            jsonschema:"proposed config body"`
	Scope string `json:"scope,omitempty" jsonschema:"repo (default — diff against .treeman.yaml) | global (diff against ~/.config/treeman/config.yaml)"`
}

type configDiffChange struct {
	Path     string `json:"path"`
	Op       string `json:"op"            jsonschema:"add|remove|change"`
	OldValue any    `json:"old,omitempty"`
	NewValue any    `json:"new,omitempty"`
}

type configDiffOut struct {
	Repo    string             `json:"repo"`
	Parsed  bool               `json:"parsed"`
	Changes []configDiffChange `json:"changes"`
	Summary string             `json:"summary"`
}

// configDiffTool parses the supplied body as a .treeman.yaml, loads
// the current resolved config from disk, and emits a flat list of
// changed dotted paths. The body is validated before diffing so a
// broken YAML produces a parse error rather than confusing diff output.
func configDiffTool(ctx context.Context, _ *mcpsdk.CallToolRequest, in configDiffIn) (*mcpsdk.CallToolResult, configDiffOut, error) {
	if strings.TrimSpace(in.Body) == "" {
		return nil, configDiffOut{}, errors.New("body is required")
	}

	var proposed config.Config
	if err := yaml.Unmarshal([]byte(in.Body), &proposed); err != nil {
		return nil, configDiffOut{}, fmt.Errorf("parse proposed config: %w", err)
	}

	// Global scope diffs the proposed body against the user-global config
	// alone; repo scope diffs against the resolved layered view.
	var current config.Config
	var baseLabel string
	if strings.EqualFold(in.Scope, "global") {
		baseLabel, _ = config.GlobalConfigPath()
		c, err := config.LoadGlobal()
		if err != nil {
			c = config.Config{}
		}
		current = c
	} else {
		if in.Scope != "" && !strings.EqualFold(in.Scope, "repo") {
			return nil, configDiffOut{}, fmt.Errorf("invalid scope %q (want repo|global)", in.Scope)
		}
		repoRoot, err := resolveRepo(in.Repo)
		if err != nil {
			return nil, configDiffOut{}, err
		}
		baseLabel = repoRoot
		c, err := resolve.LoadResolved(repoRoot)
		if err != nil {
			// Treat missing config as an empty baseline so the diff shows
			// every proposed field as an add — useful for previewing a
			// scaffold against a virgin repo.
			c = config.Config{}
		}
		current = c
	}

	curJSON, err := json.Marshal(current)
	if err != nil {
		return nil, configDiffOut{Repo: baseLabel}, fmt.Errorf("marshal current config: %w", err)
	}
	newJSON, err := json.Marshal(proposed)
	if err != nil {
		return nil, configDiffOut{Repo: baseLabel}, fmt.Errorf("marshal proposed config: %w", err)
	}
	var curMap, newMap map[string]any
	_ = json.Unmarshal(curJSON, &curMap)
	_ = json.Unmarshal(newJSON, &newMap)

	changes := diffMaps("", curMap, newMap)
	out := configDiffOut{Repo: baseLabel, Parsed: true, Changes: changes}
	out.Summary = summarizeChanges(changes)
	return nil, out, nil
}

// diffMaps walks two JSON-shaped trees and records every leaf path
// that differs. Slices are compared by serialised value (no per-index
// recursion) so a reordering shows as a single change at the slice's
// path, not 100 add/remove pairs. Good enough for "did this YAML edit
// touch databases[0].migrate.env?"-style questions.
func diffMaps(prefix string, a, b map[string]any) []configDiffChange {
	var out []configDiffChange
	seen := map[string]struct{}{}
	for k, av := range a {
		seen[k] = struct{}{}
		p := joinPath(prefix, k)
		bv, ok := b[k]
		if !ok {
			out = append(out, configDiffChange{Path: p, Op: "remove", OldValue: av})
			continue
		}
		out = append(out, diffValues(p, av, bv)...)
	}
	for k, bv := range b {
		if _, ok := seen[k]; ok {
			continue
		}
		p := joinPath(prefix, k)
		out = append(out, configDiffChange{Path: p, Op: "add", NewValue: bv})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func diffValues(path string, a, b any) []configDiffChange {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		return diffMaps(path, am, bm)
	}
	if reflect.DeepEqual(a, b) {
		return nil
	}
	return []configDiffChange{{Path: path, Op: "change", OldValue: a, NewValue: b}}
}

func joinPath(prefix, k string) string {
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}

func summarizeChanges(c []configDiffChange) string {
	counts := map[string]int{}
	for _, ch := range c {
		counts[ch.Op]++
	}
	var b bytes.Buffer
	if counts["add"]+counts["remove"]+counts["change"] == 0 {
		return "no changes"
	}
	for _, op := range []string{"add", "change", "remove"} {
		if counts[op] > 0 {
			if b.Len() > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%d %s", counts[op], op)
		}
	}
	return b.String()
}

// ─── inputs_fingerprint ─────────────────────────────────────────────

type inputsFingerprintIn struct {
	Repo         string `json:"repo,omitempty"`
	WorktreePath string `json:"worktree_path,omitempty" jsonschema:"absolute worktree path; defaults to the cwd-discovered repo root"`
	DBIndex      *int   `json:"db_index,omitempty"      jsonschema:"index into databases[]; omit to return one report per configured database"`
	ProbeEngine  bool   `json:"probe_engine,omitempty"  jsonschema:"connect to the engine to fetch the real engine_version (slower but produces a fingerprint that can match the cached one); default false"`
}

type inputsFingerprintEntry struct {
	DBIndex int                       `json:"db_index"`
	Report  prepare.FingerprintReport `json:"report"`
	Note    string                    `json:"note,omitempty" jsonschema:"e.g. \"engine_version skipped — pass probe_engine=true to match the cached fingerprint\""`
}

type inputsFingerprintOut struct {
	Repo         string                   `json:"repo"`
	WorktreePath string                   `json:"worktree_path"`
	Entries      []inputsFingerprintEntry `json:"entries"`
}

func inputsFingerprintTool(
	ctx context.Context,
	_ *mcpsdk.CallToolRequest,
	in inputsFingerprintIn,
) (*mcpsdk.CallToolResult, inputsFingerprintOut, error) {
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, inputsFingerprintOut{}, err
	}
	wt := in.WorktreePath
	if wt == "" {
		wt = repoRoot
	}
	cfg, err := resolve.LoadResolvedForWorktree(repoRoot, wt)
	if err != nil {
		return nil, inputsFingerprintOut{}, fmt.Errorf("load resolved config: %w", err)
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, inputsFingerprintOut{}, err
	}
	defer func() { _ = st.Close() }()

	sl := slug.For(wt, "")
	tplCtx := template.FromSlug(sl)

	out := inputsFingerprintOut{Repo: repoRoot, WorktreePath: wt}
	indices := []int{}
	if in.DBIndex != nil {
		if *in.DBIndex < 0 || *in.DBIndex >= len(cfg.Databases) {
			return nil, out, fmt.Errorf("db_index %d out of range (0..%d)", *in.DBIndex, len(cfg.Databases)-1)
		}
		indices = []int{*in.DBIndex}
	} else {
		for i := range cfg.Databases {
			indices = append(indices, i)
		}
	}

	for _, i := range indices {
		d := cfg.Databases[i]
		sourceDB, _ := template.Render(d.NameTemplate, tplCtx)
		engineVersion := ""
		note := "engine_version skipped — pass probe_engine=true for a cache-comparable fingerprint"
		if in.ProbeEngine {
			engineVersion = probeEngineVersion(ctx, &cfg, d.Engine)
			note = ""
		}
		rep := prepare.InspectFingerprint(ctx, st, d, wt, sourceDB, engineVersion)
		out.Entries = append(out.Entries, inputsFingerprintEntry{
			DBIndex: i,
			Report:  rep,
			Note:    note,
		})
	}
	return nil, out, nil
}

// probeEngineVersion calls each engine driver's EngineVersion. Errors
// are swallowed (empty string) so a half-up environment doesn't
// crash the introspection call — the caller will see an empty
// engine_version field and can investigate via engine_status.
func probeEngineVersion(ctx context.Context, cfg *config.Config, eng string) string {
	fam, _ := engine.Canonical(eng)
	conn, configured, err := engineconn.Connect(ctx, cfg, fam)
	if !configured || err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	v, _ := conn.EngineVersion(ctx)
	return v
}

// ─── prepare_dry_run ────────────────────────────────────────────────

type prepareDryRunIn struct {
	Worktree string `json:"worktree,omitempty" jsonschema:"defaults to cwd's worktree"`
	Repo     string `json:"repo,omitempty"`
}

type prepareDryRunDB struct {
	DBIndex      int                       `json:"db_index"`
	Engine       string                    `json:"engine"`
	SourceDB     string                    `json:"source_db,omitempty"`
	KeyPrefix    string                    `json:"key_prefix,omitempty"`
	BranchScoped bool                      `json:"branch_scoped,omitempty"`
	Dumps        []prepareDryRunDump       `json:"dumps,omitempty"`
	Migrate      *prepareDryRunStep        `json:"migrate,omitempty"`
	Seed         *prepareDryRunStep        `json:"seed,omitempty"`
	FanoutTarget int                       `json:"fanout_target,omitempty"`
	Fingerprint  prepare.FingerprintReport `json:"fingerprint"`
	Notes        []string                  `json:"notes,omitempty"`
}

type prepareDryRunDump struct {
	Path     string `json:"path"`
	Exists   bool   `json:"exists"`
	Optional bool   `json:"optional,omitempty"`
}

type prepareDryRunStep struct {
	Run string            `json:"run"`
	Env map[string]string `json:"env,omitempty"`
}

type prepareDryRunOut struct {
	WorktreePath string            `json:"worktree_path"`
	Repo         string            `json:"repo"`
	Slug         string            `json:"slug"`
	Databases    []prepareDryRunDB `json:"databases"`
}

// prepareDryRunTool walks cfg.Databases and produces a per-DB plan
// describing exactly what prepare_run would do without executing
// anything. Useful as a sanity check before a destructive cold build
// and as a debugging surface for "what command did treeman actually
// run?"-style questions.
func prepareDryRunTool(
	ctx context.Context,
	_ *mcpsdk.CallToolRequest,
	in prepareDryRunIn,
) (*mcpsdk.CallToolResult, prepareDryRunOut, error) {
	wt, _ := resolveWorktree(in.Worktree)
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, prepareDryRunOut{}, err
	}
	cfg, err := resolve.LoadResolvedForWorktree(repoRoot, wt)
	if err != nil {
		return nil, prepareDryRunOut{}, fmt.Errorf("load resolved config: %w", err)
	}
	st, err := openStore(ctx)
	if err != nil {
		return nil, prepareDryRunOut{}, err
	}
	defer func() { _ = st.Close() }()
	sl := slug.For(wt, "")
	tplCtx := template.FromSlug(sl)
	out := prepareDryRunOut{WorktreePath: wt, Repo: repoRoot, Slug: sl.Value}
	for i, d := range cfg.Databases {
		out.Databases = append(out.Databases, planOneDatabase(ctx, st, d, i, wt, tplCtx))
	}
	return nil, out, nil
}

// planOneDatabase renders the dry-run plan for one configured database.
// Extracted from prepareDryRunTool so the outer function stays under
// the cyclomatic-complexity budget; the per-DB block has its own
// branching for templates, dump entries, migrate/seed, and fanout.
func planOneDatabase(
	ctx context.Context,
	st *store.Store,
	d config.DatabaseConfig,
	idx int,
	wt string,
	tplCtx template.Context,
) prepareDryRunDB {
	entry := prepareDryRunDB{DBIndex: idx, Engine: d.Engine, BranchScoped: d.BranchScoped}
	if d.NameTemplate != "" {
		if n, rErr := template.Render(d.NameTemplate, tplCtx); rErr == nil {
			entry.SourceDB = n
		}
	}
	if d.KeyPrefix != "" {
		if p, rErr := template.Render(d.KeyPrefix, tplCtx); rErr == nil {
			entry.KeyPrefix = p
		}
	}
	for _, ds := range d.Dump {
		abs := ds.Path
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(wt, ds.Path)
		}
		ok := false
		if _, sErr := os.Stat(abs); sErr == nil {
			ok = true
		}
		entry.Dumps = append(entry.Dumps, prepareDryRunDump{Path: abs, Exists: ok, Optional: ds.Optional})
	}
	if d.Migrate != nil {
		entry.Migrate = &prepareDryRunStep{Run: d.Migrate.Run, Env: d.Migrate.Env}
	}
	if d.Seed != nil {
		entry.Seed = &prepareDryRunStep{Run: d.Seed.Run, Env: d.Seed.Env}
	}
	if d.TestClones != nil {
		if d.TestClones.Clones.Auto {
			entry.FanoutTarget = -1 // -1 → "auto: derived from runner config at prepare time"
		} else {
			entry.FanoutTarget = int(d.TestClones.Clones.Fixed)
		}
	}
	entry.Fingerprint = prepare.InspectFingerprint(ctx, st, d, wt, entry.SourceDB, "")
	if d.Migrate != nil && d.Migrate.Run == "" {
		entry.Notes = append(entry.Notes, "migrate.run is empty — prepare will skip migrate")
	}
	if len(d.Dump) == 0 && d.Engine != "redis" && !d.BranchScoped {
		entry.Notes = append(entry.Notes, "no dump configured — source DB will start empty before migrate")
	}
	return entry
}

// branchOccupancyFromStore mirrors the CLI's branchOccupancy helper:
// branch → worktree path for every live worktree of repoRoot, sourced
// from SQLite (cheaper than parsing `git worktree list --porcelain`
// per call).
func branchOccupancyFromStore(ctx context.Context, repoRoot string) (map[string]string, error) {
	st, err := openStore(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	rows, err := st.DB.QueryContext(ctx, `
		SELECT COALESCE(w.branch, ''), w.path
		FROM worktrees w JOIN repos r ON r.id = w.repo_id
		WHERE r.path = ? AND w.deleted_at IS NULL AND w.branch IS NOT NULL`, repoRoot)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var branch, path string
		if err := rows.Scan(&branch, &path); err != nil {
			continue
		}
		if branch == "" {
			continue
		}
		out[branch] = path
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
