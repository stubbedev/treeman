package mcp

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/store"
)

// ─── logs_wait ──────────────────────────────────────────────────────

type logsWaitIn struct {
	Repo           string   `json:"repo,omitempty"`
	Worktree       string   `json:"worktree,omitempty" jsonschema:"slug, branch, or basename"`
	Levels         []string `json:"levels,omitempty"`
	EventTypes     []string `json:"event_types,omitempty"`
	Phases         []string `json:"phases,omitempty"`
	PayloadLike    string   `json:"payload_like,omitempty"`
	RunID          string   `json:"run_id,omitempty" jsonschema:"correlation id; the common case — wait for events from one prepare/finalize run"`
	MinCount       int      `json:"min_count,omitempty" jsonschema:"return after this many new events arrive (default 1)"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" jsonschema:"give up after this many seconds (default 30, max 300)"`
}

type logsWaitOut struct {
	Events   []store.Event `json:"events"`
	TimedOut bool          `json:"timed_out"`
	Anchor   int64         `json:"anchor_id" jsonschema:"highest event id at the moment the wait started; only events newer than this count"`
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
	defer st.Close()

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
	if in.Repo != "" || in.Worktree != "" {
		repoRoot, _ := resolveRepo(in.Repo)
		if repoRoot != "" {
			if rid, _ := lookupRepoID(ctx, st, repoRoot); rid > 0 {
				f.RepoID = rid
			}
		}
		if in.Worktree != "" {
			wid, _ := st.LookupWorktreeID(ctx, f.RepoID, in.Worktree)
			if wid == 0 {
				return nil, logsWaitOut{}, fmt.Errorf("no worktree matches %q", in.Worktree)
			}
			f.WorktreeID = wid
		}
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
			return nil, logsWaitOut{Events: collected, Anchor: anchor}, nil
		}
		if time.Now().After(deadline) {
			return nil, logsWaitOut{Events: collected, Anchor: anchor, TimedOut: true}, nil
		}
		select {
		case <-ctx.Done():
			return nil, logsWaitOut{Events: collected, Anchor: anchor, TimedOut: true}, ctx.Err()
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

// ─── branches_list ──────────────────────────────────────────────────

type branchesListIn struct {
	Repo string `json:"repo,omitempty"`
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
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, branchesListOut{}, err
	}
	local, err := gitcmd.String(ctx, repoRoot, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return nil, branchesListOut{}, fmt.Errorf("list local branches: %w", err)
	}
	remote, err := gitcmd.String(ctx, repoRoot, "for-each-ref", "--format=%(refname:short)", "refs/remotes/origin")
	if err != nil {
		return nil, branchesListOut{}, fmt.Errorf("list remote branches: %w", err)
	}
	current, _ := gitcmd.String(ctx, repoRoot, "symbolic-ref", "--short", "HEAD")
	current = strings.TrimSpace(current)

	branchToWtDir, _ := branchOccupancyFromStore(ctx, repoRoot)

	entries := map[string]*branchesListEntry{}
	for _, b := range strings.Split(local, "\n") {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		entries[b] = &branchesListEntry{Name: b, HasLocal: true}
	}
	for _, b := range strings.Split(remote, "\n") {
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
	out := branchesListOut{Repo: repoRoot, Branches: make([]branchesListEntry, 0, len(names))}
	for _, n := range names {
		out.Branches = append(out.Branches, *entries[n])
	}
	return nil, out, nil
}

// ─── config_diff ────────────────────────────────────────────────────

type configDiffIn struct {
	Repo string `json:"repo,omitempty"`
	Body string `json:"body" jsonschema:"proposed .treeman.yaml body"`
}

type configDiffChange struct {
	Path     string `json:"path"`
	Op       string `json:"op" jsonschema:"add|remove|change"`
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
		return nil, configDiffOut{}, fmt.Errorf("body is required")
	}
	repoRoot, err := resolveRepo(in.Repo)
	if err != nil {
		return nil, configDiffOut{}, err
	}

	var proposed config.Config
	if err := yaml.Unmarshal([]byte(in.Body), &proposed); err != nil {
		return nil, configDiffOut{Repo: repoRoot}, fmt.Errorf("parse proposed config: %w", err)
	}

	current, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		// Treat missing config as an empty baseline so the diff shows
		// every proposed field as an add — useful for previewing a
		// scaffold against a virgin repo.
		current = config.Config{}
	}

	curJSON, _ := json.Marshal(current)
	newJSON, _ := json.Marshal(proposed)
	var curMap, newMap map[string]any
	_ = json.Unmarshal(curJSON, &curMap)
	_ = json.Unmarshal(newJSON, &newMap)

	changes := diffMaps("", curMap, newMap)
	out := configDiffOut{Repo: repoRoot, Parsed: true, Changes: changes}
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

// resolvePathSafe is a small helper for resource handlers that build
// file paths from URI templates — guards against path traversal so a
// malicious {slug} can't escape the worktree root.
func resolvePathSafe(base, untrusted string) (string, error) {
	full := filepath.Join(base, untrusted)
	abs, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	baseAbs, _ := filepath.Abs(base)
	if !strings.HasPrefix(abs, baseAbs) {
		return "", os.ErrPermission
	}
	return abs, nil
}

// nowMs is a tiny shim so tests can fake the wall clock without
// dragging in a time-mock package.
var nowMs = func() int64 { return time.Now().UnixMilli() }

// branchOccupancyFromStore mirrors the CLI's branchOccupancy helper:
// branch → worktree path for every live worktree of repoRoot, sourced
// from SQLite (cheaper than parsing `git worktree list --porcelain`
// per call).
func branchOccupancyFromStore(ctx context.Context, repoRoot string) (map[string]string, error) {
	st, err := openStore(ctx)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	rows, err := st.DB.QueryContext(ctx, `
		SELECT COALESCE(w.branch, ''), w.path
		FROM worktrees w JOIN repos r ON r.id = w.repo_id
		WHERE r.path = ? AND w.deleted_at IS NULL AND w.branch IS NOT NULL`, repoRoot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
	return out, nil
}
