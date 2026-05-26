package hooks

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/config"
)

// emptyEnv returns the minimum env needed for `sh -c <cmd>` to
// resolve coreutils (`touch`, `sleep`, `printenv`, etc.). PATH is
// inherited from the test runner's own env so the tests work on any
// host — NixOS (where coreutils live under /nix/store/.../bin), Mac
// (/usr/bin), Alpine containers (/usr/sbin), etc. The
// `TREEMAN_TEST_PATH_PROBE` value is set by one specific test and
// passes through unchanged for any others.
func emptyEnv() map[string]string {
	env := map[string]string{}
	if p := os.Getenv("PATH"); p != "" {
		env["PATH"] = p
	}
	return env
}

func waitUntil(cond func() bool, maxMs int) {
	deadline := time.Now().Add(time.Duration(maxMs) * time.Millisecond)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestGroupRunsSequenceWithin(t *testing.T) {
	wt := t.TempDir()
	entries := []config.Action{
		{Run: []string{
			"touch a",
			"touch b",
		}},
	}
	out, err := RunHooks(context.Background(), "setup", entries, wt, wt, "slug", false, emptyEnv(), false)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(func() bool {
		_, err := os.Stat(filepath.Join(wt, "b"))
		return err == nil
	}, 2000)
	aInfo, err := os.Stat(filepath.Join(wt, "a"))
	if err != nil {
		t.Fatalf("a missing: %v", err)
	}
	bInfo, err := os.Stat(filepath.Join(wt, "b"))
	if err != nil {
		t.Fatalf("b missing: %v", err)
	}
	if !bInfo.ModTime().After(aInfo.ModTime()) && !bInfo.ModTime().Equal(aInfo.ModTime()) {
		t.Errorf("b should be created after a (a=%v b=%v)", aInfo.ModTime(), bInfo.ModTime())
	}
	if len(out.Groups) != 1 {
		t.Errorf("want 1 group, got %d", len(out.Groups))
	}
}

func TestGroupsRunParallelAcross(t *testing.T) {
	wt := t.TempDir()
	started := time.Now()
	entries := []config.Action{
		{Run: []string{"sleep 0.5 && touch one"}},
		{Run: []string{"sleep 0.5 && touch two"}},
	}
	_, err := RunHooks(context.Background(), "setup", entries, wt, wt, "slug", false, emptyEnv(), false)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Errorf("RunHooks blocked for %v — should be <300ms", elapsed)
	}
	waitUntil(func() bool {
		_, e1 := os.Stat(filepath.Join(wt, "one"))
		_, e2 := os.Stat(filepath.Join(wt, "two"))
		return e1 == nil && e2 == nil
	}, 1500)
	total := time.Since(started)
	if total > 900*time.Millisecond {
		t.Errorf("parallel groups took %v — should be <900ms", total)
	}
}

func TestRunHooksReturnsFastRegardlessOfCommandDuration(t *testing.T) {
	wt := t.TempDir()
	entries := []config.Action{{Run: []string{"sleep 10"}}}
	started := time.Now()
	_, err := RunHooks(context.Background(), "setup", entries, wt, wt, "slug", false, emptyEnv(), false)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Errorf("RunHooks blocked for %v", elapsed)
	}
}

// TestRunHooksWaitBlocksUntilGroupsExit proves the wait=true contract
// the daemon's FinalizeWorktree depends on: when a downstream phase
// (prepare) needs the setup work done first, RunHooks must not
// return until every group has actually completed.
func TestRunHooksWaitBlocksUntilGroupsExit(t *testing.T) {
	wt := t.TempDir()
	entries := []config.Action{
		{Run: []string{"sleep 0.3 && touch a"}},
		{Run: []string{"sleep 0.3 && touch b"}},
	}
	started := time.Now()
	out, err := RunHooks(context.Background(), "setup", entries, wt, wt, "slug", false, emptyEnv(), true)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	// Both files must exist when RunHooks returns — no waitUntil.
	if _, err := os.Stat(filepath.Join(wt, "a")); err != nil {
		t.Errorf("a missing after wait=true return: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "b")); err != nil {
		t.Errorf("b missing after wait=true return: %v", err)
	}
	// Groups run in parallel — wall time should track one sleep, not
	// the sum. Allow generous slack for slow runners.
	if elapsed < 250*time.Millisecond {
		t.Errorf("returned before group sleep finished: %v", elapsed)
	}
	if elapsed > 700*time.Millisecond {
		t.Errorf("groups didn't parallelise (took %v)", elapsed)
	}
	for i, g := range out.Groups {
		if g.ExitCode != 0 {
			t.Errorf("group %d exit %d", i, g.ExitCode)
		}
	}
}

// TestRunHooksWaitSurfacesNonZeroExit confirms a failing group's exit
// code lands on the returned outcome instead of aborting the whole
// phase. The downstream code (prepare, db teardown) can then decide
// whether to proceed.
func TestRunHooksWaitSurfacesNonZeroExit(t *testing.T) {
	wt := t.TempDir()
	entries := []config.Action{
		{Run: []string{"true"}},
		{Run: []string{"exit 7"}},
	}
	out, err := RunHooks(context.Background(), "setup", entries, wt, wt, "slug", false, emptyEnv(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Groups) != 2 {
		t.Fatalf("groups=%d", len(out.Groups))
	}
	if out.Groups[0].ExitCode != 0 {
		t.Errorf("group 0 exit %d, want 0", out.Groups[0].ExitCode)
	}
	if out.Groups[1].ExitCode != 7 {
		t.Errorf("group 1 exit %d, want 7", out.Groups[1].ExitCode)
	}
}

func TestInheritedEnvReachesSubprocess(t *testing.T) {
	wt := t.TempDir()
	env := emptyEnv()
	env["TREEMAN_TEST_PATH_PROBE"] = "xyz123"
	entries := []config.Action{
		{Run: []string{"printenv TREEMAN_TEST_PATH_PROBE > probe.out"}},
	}
	_, err := RunHooks(context.Background(), "setup", entries, wt, wt, "slug", false, env, false)
	if err != nil {
		t.Fatal(err)
	}
	// Wait for non-empty content. The shell creates probe.out
	// before printenv writes to it, so an existence-only poll
	// races the write on slow runners (observed on macos GHA).
	waitUntil(func() bool {
		fi, err := os.Stat(filepath.Join(wt, "probe.out"))
		return err == nil && fi.Size() > 0
	}, 3000)
	b, err := os.ReadFile(filepath.Join(wt, "probe.out"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "xyz123\n" {
		t.Errorf("probe got %q", string(b))
	}
}

func TestRenderActionChainsWithAnd(t *testing.T) {
	a := config.Action{Run: []string{"echo a", "echo b"}}
	got, err := renderAction(a, "/tmp/x")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(got, " && ") {
		t.Errorf("missing &&: %s", got)
	}
	// Both steps share the action's cwd (defaulted to /tmp/x here).
	if !contains(got, "cd '/tmp/x' && echo a") {
		t.Errorf("missing first clause: %s", got)
	}
	if !contains(got, "cd '/tmp/x' && echo b") {
		t.Errorf("missing second clause: %s", got)
	}
}

func TestRenderActionGroupLevelCwd(t *testing.T) {
	// Explicit cwd on the Action applies to every step.
	a := config.Action{Run: []string{"yarn install", "yarn build"}, Cwd: "frontend"}
	got, err := renderAction(a, "/tmp/x")
	if err != nil {
		t.Fatal(err)
	}
	if contains(got, "cd '/tmp/x'") {
		t.Errorf("default cwd leaked when action.Cwd was set: %s", got)
	}
	if !contains(got, "cd 'frontend' && yarn install") {
		t.Errorf("missing first clause: %s", got)
	}
	if !contains(got, "cd 'frontend' && yarn build") {
		t.Errorf("missing second clause: %s", got)
	}
}

func TestRenderActionWrapsInDockerExec(t *testing.T) {
	a := config.Action{
		Container: "myapp",
		Engine:    "docker",
		Run:       []string{"composer install", "php artisan migrate"},
		Cwd:       "/var/www/html",
	}
	got, err := renderAction(a, "/host/wt")
	if err != nil {
		t.Fatal(err)
	}
	// No host-side cd: container WORKDIR / `-w` control cwd.
	if contains(got, "cd '/host/wt'") {
		t.Errorf("host cd leaked into container exec: %s", got)
	}
	if !contains(got, "'docker' 'exec' '-w' '/var/www/html' 'myapp' 'sh' '-c' 'composer install'") {
		t.Errorf("first wrap wrong: %s", got)
	}
	if !contains(got, "'docker' 'exec' '-w' '/var/www/html' 'myapp' 'sh' '-c' 'php artisan migrate'") {
		t.Errorf("second wrap wrong: %s", got)
	}
	if !contains(got, " && ") {
		t.Errorf("steps not chained: %s", got)
	}
}

func TestRenderActionNoContainerPassesThrough(t *testing.T) {
	entry := config.Action{Run: []string{"echo hi"}}
	got, err := renderAction(entry, "/host/wt")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(got, "cd '/host/wt' && echo hi") {
		t.Errorf("expected legacy render, got: %s", got)
	}
}

// TestBuildEnvUserPathWins asserts the user's PATH from
// inheritedEnv is preserved verbatim — no merge with the daemon's
// PATH, no override. The user's PATH is authoritative because it
// carries their version-manager shims (asdf, nvm, mise, …).
func TestBuildEnvUserPathWins(t *testing.T) {
	env := buildEnv(map[string]string{"PATH": "/user/shims:/user/bin"}, "/repo", "/wt", "slug", false)
	pathLine := ""
	for _, kv := range env {
		if len(kv) > 5 && kv[:5] == "PATH=" {
			pathLine = kv
		}
	}
	if pathLine != "PATH=/user/shims:/user/bin" {
		t.Errorf("user PATH not preserved: %q", pathLine)
	}
}

// TestBuildEnvFallsBackToDaemonPath asserts that when the user's
// cached env has no PATH (lifecycle watcher firing for a first-time
// external `git worktree add`, before any wt finalize), we fall back
// to the daemon process's own PATH so coreutils still resolve.
func TestBuildEnvFallsBackToDaemonPath(t *testing.T) {
	// Sanity: this test only makes sense when the daemon-process
	// env (the test runner's env) actually has PATH set.
	if os.Getenv("PATH") == "" {
		t.Skip("test runner has no PATH set; skipping fallback assertion")
	}
	env := buildEnv(nil, "/repo", "/wt", "slug", false) // no inheritedEnv, no PATH
	foundPath := false
	for _, kv := range env {
		if len(kv) > 5 && kv[:5] == "PATH=" {
			foundPath = true
		}
	}
	if !foundPath {
		t.Error("buildEnv should fall back to os.Getenv(PATH) when inheritedEnv has none")
	}
}

func TestShellSingleQuoteEscapesApostrophes(t *testing.T) {
	if shellSingleQuote("plain") != "'plain'" {
		t.Errorf("plain: %s", shellSingleQuote("plain"))
	}
	if shellSingleQuote("a'b") != `'a'\''b'` {
		t.Errorf("apostrophe: %s", shellSingleQuote("a'b"))
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(s) > 0 && (s[0:1] == sub[0:1] || true) && indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
