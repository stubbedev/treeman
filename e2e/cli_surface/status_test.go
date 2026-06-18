//go:build e2e

package cli_surface_e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedStatusFixture lays down two repos whose worktrees span all four
// `treeman status` buckets — stable (2), up (1), down (1), failed (1) —
// plus a main worktree. Buckets are driven by the most-recent lifecycle
// event per worktree (ORDER BY ts DESC, id DESC), so events are inserted
// last-state-last. Returns both repo roots.
func seedStatusFixture(t *testing.T, e *env) (apiRepo, webRepo string) {
	t.Helper()
	apiRepo = newGitRepo(t)
	webRepo = newGitRepo(t)
	ctx := context.Background()
	st := openStore(t, e)

	apiID, err := st.EnsureRepo(ctx, apiRepo, "api")
	if err != nil {
		t.Fatal(err)
	}
	webID, err := st.EnsureRepo(ctx, webRepo, "web")
	if err != nil {
		t.Fatal(err)
	}

	ts := time.Now().UnixMilli()
	ev := func(wtID int64, level, etype string) {
		t.Helper()
		ts++
		if _, err := st.DB.ExecContext(ctx,
			`INSERT INTO events(ts, level, worktree_id, event_type, payload_json)
			 VALUES (?, ?, ?, ?, '{}')`, ts, level, wtID, etype); err != nil {
			t.Fatal(err)
		}
	}

	// api: main worktree, stable (last finalize succeeded).
	mID, err := st.EnsureMainWorktree(ctx, apiID, apiRepo, "api_main", "main")
	if err != nil {
		t.Fatal(err)
	}
	ev(mID, "info", "worktree:create:end")

	// api: preparing → up.
	upID, err := st.EnsureWorktree(ctx, apiID, filepath.Join(apiRepo, ".worktrees", "feat"), "api_feat", "feature/login")
	if err != nil {
		t.Fatal(err)
	}
	ev(upID, "info", "worktree:create:start")

	// api: errored → failed.
	badID, err := st.EnsureWorktree(ctx, apiID, filepath.Join(apiRepo, ".worktrees", "bug"), "api_bug", "bugfix/crash")
	if err != nil {
		t.Fatal(err)
	}
	ev(badID, "info", "worktree:create:start")
	ev(badID, "error", "worktree:create:error")

	// web: no events → stable.
	if _, err := st.EnsureWorktree(ctx, webID, filepath.Join(webRepo, ".worktrees", "x"), "web_x", "feature/x"); err != nil {
		t.Fatal(err)
	}

	// web: teardown in flight → down.
	dnID, err := st.EnsureWorktree(ctx, webID, filepath.Join(webRepo, ".worktrees", "old"), "web_old", "old/thing")
	if err != nil {
		t.Fatal(err)
	}
	ev(dnID, "info", "worktree:create:end")
	ev(dnID, "info", "worktree:delete:start")

	return apiRepo, webRepo
}

// writeGlobalConfig drops a global config.yaml into the env's
// XDG_CONFIG_HOME/treeman/ — the file `treeman status` reads for its
// `status:` block.
func writeGlobalConfig(t *testing.T, e *env, body string) {
	t.Helper()
	dir := filepath.Join(e.configDir, "treeman")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// statusJSON mirrors the `--format json` shape we assert against.
type statusJSON struct {
	Total  int    `json:"total"`
	Stable int    `json:"stable"`
	Up     int    `json:"up"`
	Down   int    `json:"down"`
	Failed int    `json:"failed"`
	Class  string `json:"class"`
	Repos  []struct {
		Repo      string `json:"repo"`
		Total     int    `json:"total"`
		Worktrees []struct {
			Branch string `json:"branch"`
			State  string `json:"state"`
			Bucket string `json:"bucket"`
			IsMain bool   `json:"is_main"`
		} `json:"worktrees"`
	} `json:"repos"`
}

func TestStatusJSON(t *testing.T) {
	e := newEnv(t)
	apiRepo, webRepo := seedStatusFixture(t, e)

	res := e.run(t, apiRepo, "status", "--format", "json")
	if res.err != nil {
		t.Fatalf("status --format json: %v\nstderr:\n%s", res.err, res.stderr)
	}
	var got statusJSON
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &got); err != nil {
		t.Fatalf("decode status JSON %q: %v", res.stdout, err)
	}
	if got.Total != 5 || got.Stable != 2 || got.Up != 1 || got.Down != 1 || got.Failed != 1 {
		t.Errorf("counts: got total=%d stable=%d up=%d down=%d failed=%d; want 5/2/1/1/1",
			got.Total, got.Stable, got.Up, got.Down, got.Failed)
	}
	if got.Class != "failed" {
		t.Errorf("class: got %q want failed", got.Class)
	}
	if len(got.Repos) != 2 {
		t.Fatalf("repos: got %d want 2", len(got.Repos))
	}
	// Both repo labels should be the basenames of the two temp repos.
	wantLabels := map[string]bool{filepath.Base(apiRepo): true, filepath.Base(webRepo): true}
	var sawMain, sawFailed bool
	for _, r := range got.Repos {
		if !wantLabels[r.Repo] {
			t.Errorf("unexpected repo label %q", r.Repo)
		}
		for _, w := range r.Worktrees {
			if w.IsMain {
				sawMain = true
				if w.Bucket != "stable" {
					t.Errorf("main worktree bucket: got %q want stable", w.Bucket)
				}
			}
			if w.Bucket == "failed" {
				sawFailed = true
				if w.State != "error" {
					t.Errorf("failed worktree state: got %q want error", w.State)
				}
			}
		}
	}
	if !sawMain {
		t.Error("expected a worktree with is_main=true")
	}
	if !sawFailed {
		t.Error("expected a worktree in the failed bucket")
	}
}

func TestStatusIconDefault(t *testing.T) {
	e := newEnv(t)
	apiRepo, _ := seedStatusFixture(t, e)

	res := e.run(t, apiRepo, "status")
	if res.err != nil {
		t.Fatalf("status: %v\nstderr:\n%s", res.err, res.stderr)
	}
	out := strings.TrimSpace(res.stdout)
	for _, want := range []string{"stable: 2", "up: 1", "down: 1", "failed: 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("icon line %q missing %q", out, want)
		}
	}
}

func TestStatusHover(t *testing.T) {
	e := newEnv(t)
	apiRepo, webRepo := seedStatusFixture(t, e)

	res := e.run(t, apiRepo, "status", "--format", "hover")
	if res.err != nil {
		t.Fatalf("status --format hover: %v\nstderr:\n%s", res.err, res.stderr)
	}
	out := res.stdout
	for _, want := range []string{
		filepath.Base(apiRepo), filepath.Base(webRepo),
		"★ ",          // default main marker
		"(error)",     // failed worktree state suffix
		"(preparing)", // up worktree state suffix
		"(teardown)",  // down worktree state suffix
	} {
		if !strings.Contains(out, want) {
			t.Errorf("hover output missing %q:\n%s", want, out)
		}
	}
}

func TestStatusWaybar(t *testing.T) {
	e := newEnv(t)
	apiRepo, _ := seedStatusFixture(t, e)

	res := e.run(t, apiRepo, "status", "--format", "waybar")
	if res.err != nil {
		t.Fatalf("status --format waybar: %v\nstderr:\n%s", res.err, res.stderr)
	}
	var obj struct {
		Text    string `json:"text"`
		Tooltip string `json:"tooltip"`
		Class   string `json:"class"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &obj); err != nil {
		t.Fatalf("decode waybar JSON %q: %v", res.stdout, err)
	}
	if obj.Class != "failed" {
		t.Errorf("waybar class: got %q want failed", obj.Class)
	}
	if !strings.Contains(obj.Text, "failed: 1") {
		t.Errorf("waybar text missing counter: %q", obj.Text)
	}
	if !strings.Contains(obj.Tooltip, filepath.Base(apiRepo)) {
		t.Errorf("waybar tooltip missing repo grouping: %q", obj.Tooltip)
	}
}

func TestStatusEmptyRegistry(t *testing.T) {
	e := newEnv(t)
	repo := newGitRepo(t)
	res := e.run(t, repo, "status")
	if res.err != nil {
		t.Fatalf("status (empty): %v\nstderr:\n%s", res.err, res.stderr)
	}
	if !strings.Contains(res.stdout, "stable: 0") {
		t.Errorf("empty registry icon line unexpected: %q", res.stdout)
	}
}

// TestStatusConfigOverrides exercises every customization knob in the
// status: block — icons, labels, separator, hover header/row, and the
// main marker — to make sure each flows from YAML into the rendered
// output.
func TestStatusConfigOverrides(t *testing.T) {
	e := newEnv(t)
	apiRepo, _ := seedStatusFixture(t, e)
	writeGlobalConfig(t, e, `status:
  labels:
    stable: ok
    up: prep
    down: rm
    failed: ERR
  separator: " :: "
  header: "{repo} <{failed} failed / {total}>"
  row: "  [{bucket}] {main}{branch}{state_suffix}"
  main_marker: "@@ "
`)

	icon := e.run(t, apiRepo, "status")
	if icon.err != nil {
		t.Fatalf("status icon: %v\nstderr:\n%s", icon.err, icon.stderr)
	}
	for _, want := range []string{"ok: 2", "prep: 1", "rm: 1", "ERR: 1", " :: "} {
		if !strings.Contains(icon.stdout, want) {
			t.Errorf("custom icon line missing %q:\n%s", want, icon.stdout)
		}
	}

	hover := e.run(t, apiRepo, "status", "--format", "hover")
	if hover.err != nil {
		t.Fatalf("status hover: %v\nstderr:\n%s", hover.err, hover.stderr)
	}
	for _, want := range []string{
		"<1 failed / 3>", // custom header rendered against api repo (3 wts)
		"[failed]",       // custom row exposes {bucket}
		"@@ ",            // custom main marker
	} {
		if !strings.Contains(hover.stdout, want) {
			t.Errorf("custom hover missing %q:\n%s", want, hover.stdout)
		}
	}
}

// TestStatusCustomFormat covers a user-declared single-line format and
// an override of the built-in `icon` format, both via status.formats.
func TestStatusCustomFormat(t *testing.T) {
	e := newEnv(t)
	apiRepo, _ := seedStatusFixture(t, e)
	writeGlobalConfig(t, e, `status:
  formats:
    polybar: "{failed}!/{total}"
    icon: "F={failed} U={up} T={total}"
`)

	custom := e.run(t, apiRepo, "status", "--format", "polybar")
	if custom.err != nil {
		t.Fatalf("status --format polybar: %v\nstderr:\n%s", custom.err, custom.stderr)
	}
	if got := strings.TrimSpace(custom.stdout); got != "1!/5" {
		t.Errorf("polybar format: got %q want %q", got, "1!/5")
	}

	// A formats.icon entry overrides the built-in icon line, and the
	// default --format (icon) must pick it up.
	overridden := e.run(t, apiRepo, "status")
	if got := strings.TrimSpace(overridden.stdout); got != "F=1 U=1 T=5" {
		t.Errorf("overridden icon: got %q want %q", got, "F=1 U=1 T=5")
	}
}

func TestStatusBadFormatAndToken(t *testing.T) {
	t.Run("unknown --format errors", func(t *testing.T) {
		e := newEnv(t)
		apiRepo, _ := seedStatusFixture(t, e)
		res := e.run(t, apiRepo, "status", "--format", "bogus")
		if res.err == nil {
			t.Fatalf("expected unknown --format to exit non-zero; stdout:\n%s", res.stdout)
		}
		if !strings.Contains(res.stderr, "unknown --format") {
			t.Errorf("stderr missing 'unknown --format': %q", res.stderr)
		}
	})

	t.Run("typo'd template token fails loudly", func(t *testing.T) {
		e := newEnv(t)
		apiRepo, _ := seedStatusFixture(t, e)
		writeGlobalConfig(t, e, `status:
  formats:
    broken: "{failed}/{totl}"
`)
		res := e.run(t, apiRepo, "status", "--format", "broken")
		if res.err == nil {
			t.Fatalf("expected typo'd token to exit non-zero; stdout:\n%s", res.stdout)
		}
		if !strings.Contains(res.stderr, "unknown template key") {
			t.Errorf("stderr missing 'unknown template key': %q", res.stderr)
		}
	})
}
