package mcp

import (
	"context"
	"sort"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// completionHandler dispatches MCP `completion/complete` requests to
// the right value source. Triggered when an interactive client tabs
// through prompt-argument fields or fills in a resource-template
// variable (`treeman://worktrees/{slug}/…`). The MCP spec only
// supports completion for prompt args + resource-template URI
// variables — not tool arguments — so this surface is narrower than
// it sounds.
//
// All sources are READ-ONLY against the SQLite store + the resolved
// config; no engine I/O happens here (latency matters — completions
// can fire on every keystroke).
func completionHandler(ctx context.Context, req *mcpsdk.CompleteRequest) (*mcpsdk.CompleteResult, error) {
	if req == nil || req.Params == nil || req.Params.Ref == nil {
		return emptyCompletion(), nil
	}
	ref := req.Params.Ref
	argName := req.Params.Argument.Name
	prefix := req.Params.Argument.Value

	var values []string
	switch ref.Type {
	case "ref/prompt":
		values = completeForPromptArg(ctx, ref.Name, argName)
	case "ref/resource":
		// All our templated resources currently carry one variable:
		// {slug} → worktree slug. argName matches the template's
		// variable name.
		if strings.HasPrefix(ref.URI, "treeman://worktrees/") && argName == "slug" {
			values = worktreeSlugCompletions(ctx)
		}
	}

	values = filterByPrefix(values, prefix)
	sort.Strings(values)

	// MCP caps the per-response page at 100 values; truncate so
	// HasMore reflects whether more would have been available.
	hasMore := false
	if len(values) > 100 {
		hasMore = true
		values = values[:100]
	}
	return &mcpsdk.CompleteResult{
		Completion: mcpsdk.CompletionResultDetails{
			Values:  values,
			HasMore: hasMore,
			Total:   len(values),
		},
	}, nil
}

func emptyCompletion() *mcpsdk.CompleteResult {
	return &mcpsdk.CompleteResult{
		Completion: mcpsdk.CompletionResultDetails{Values: []string{}},
	}
}

// completeForPromptArg maps (prompt name, arg name) onto a value
// source. Unknown combinations return nil so the client falls back
// to free-form input.
func completeForPromptArg(ctx context.Context, promptName, argName string) []string {
	switch argName {
	case "worktree":
		return worktreeSlugCompletions(ctx)
	case "branch":
		return branchCompletions(ctx)
	case "run_id":
		return recentRunIDCompletions(ctx)
	case "fingerprint":
		return snapshotFingerprintCompletions(ctx)
	}
	// Per-prompt fallbacks — prompts with arg names not in the
	// generic set above. Currently none, but the switch lets us add
	// e.g. "phase" suggestions for hook-flavoured prompts later.
	_ = promptName
	return nil
}

// worktreeSlugCompletions pulls every live worktree slug from the
// SQLite registry. The store handle is opened+closed per call —
// completions don't run often enough to justify a long-lived handle.
func worktreeSlugCompletions(ctx context.Context) []string {
	st, err := openStore(ctx)
	if err != nil {
		return nil
	}
	defer func() { _ = st.Close() }()
	rows, err := st.DB.QueryContext(ctx, `
		SELECT slug FROM worktrees WHERE deleted_at IS NULL AND slug IS NOT NULL
		ORDER BY slug`)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			continue
		}
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// branchCompletions returns every distinct branch known to the
// registry across every repo. Cheaper than shelling out to git and
// good enough for completion — users typing a branch name almost
// always want one that already exists in the registry.
func branchCompletions(ctx context.Context) []string {
	st, err := openStore(ctx)
	if err != nil {
		return nil
	}
	defer func() { _ = st.Close() }()
	rows, err := st.DB.QueryContext(ctx, `
		SELECT DISTINCT branch FROM worktrees
		WHERE deleted_at IS NULL AND branch IS NOT NULL AND branch != ''
		ORDER BY branch`)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var b string
		_ = rows.Scan(&b)
		if b != "" {
			out = append(out, b)
		}
	}
	return out
}

// recentRunIDCompletions returns the 50 most recent distinct run_ids
// from the events table. Useful for the diagnose-prepare-failure
// prompt's run_id arg — the user usually wants the LAST run.
func recentRunIDCompletions(ctx context.Context) []string {
	st, err := openStore(ctx)
	if err != nil {
		return nil
	}
	defer func() { _ = st.Close() }()
	// run_id lives inside payload_json; pull it out via a string-scan
	// pattern. Cheap because payload_json is small (kilobytes) and
	// we limit to the 200 most recent rows.
	rows, err := st.DB.QueryContext(ctx, `
		SELECT payload_json FROM events
		WHERE payload_json LIKE '%"run_id":"%'
		ORDER BY id DESC LIMIT 200`)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]struct{}{}
	var out []string
	for rows.Next() {
		var p string
		_ = rows.Scan(&p)
		if id := extractRunID(p); id != "" {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
			if len(out) >= 50 {
				break
			}
		}
	}
	return out
}

// extractRunID reaches into a payload JSON string and pulls the
// run_id value out without a full decode. Returns "" when the
// pattern doesn't match.
func extractRunID(payload string) string {
	const needle = `"run_id":"`
	i := strings.Index(payload, needle)
	if i < 0 {
		return ""
	}
	start := i + len(needle)
	end := strings.Index(payload[start:], `"`)
	if end < 0 {
		return ""
	}
	return payload[start : start+end]
}

func snapshotFingerprintCompletions(ctx context.Context) []string {
	st, err := openStore(ctx)
	if err != nil {
		return nil
	}
	defer func() { _ = st.Close() }()
	rows, err := st.DB.QueryContext(ctx, `
		SELECT fingerprint FROM snapshots ORDER BY last_used_at DESC LIMIT 100`)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var f string
		_ = rows.Scan(&f)
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func filterByPrefix(values []string, prefix string) []string {
	if prefix == "" {
		return values
	}
	prefix = strings.ToLower(prefix)
	out := values[:0]
	for _, v := range values {
		if strings.HasPrefix(strings.ToLower(v), prefix) {
			out = append(out, v)
		}
	}
	return out
}
