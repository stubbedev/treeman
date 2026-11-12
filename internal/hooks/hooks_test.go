package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/shellenv/shellenvtest"
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
	out, err := RunHooks(context.Background(), CreateBeforeEngines, entries, wt, wt, "slug", false, emptyEnv(), false)
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
	_, err := RunHooks(context.Background(), CreateBeforeEngines, entries, wt, wt, "slug", false, emptyEnv(), false)
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
	_, err := RunHooks(context.Background(), CreateBeforeEngines, entries, wt, wt, "slug", false, emptyEnv(), false)
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
	// Each group announces itself, then waits for the other before it
	// finishes: run sequentially, the first group would never see its
	// peer and exit 1, so parallelism is proven without timing.
	rendezvous := func(self, peer string) string {
		return "touch " + self + ".started && " +
			"for i in $(seq 200); do [ -e " + peer + ".started ] && touch " + self + " && exit 0; sleep 0.05; done; exit 1"
	}
	entries := []config.Action{
		{Run: []string{rendezvous("a", "b")}},
		{Run: []string{rendezvous("b", "a")}},
	}
	out, err := RunHooks(context.Background(), CreateBeforeEngines, entries, wt, wt, "slug", false, emptyEnv(), true)
	if err != nil {
		t.Fatal(err)
	}
	for i, g := range out.Groups {
		if g.ExitCode != 0 {
			t.Errorf("group %d exit %d — groups didn't run in parallel", i, g.ExitCode)
		}
	}
	// Both files are written as each group's last step, so they must
	// exist when RunHooks returns — no waitUntil.
	for _, f := range []string{"a", "b"} {
		if _, err := os.Stat(filepath.Join(wt, f)); err != nil {
			t.Errorf("%s missing after wait=true return: %v", f, err)
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
	out, err := RunHooks(context.Background(), CreateBeforeEngines, entries, wt, wt, "slug", false, emptyEnv(), true)
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
	_, err := RunHooks(context.Background(), CreateBeforeEngines, entries, wt, wt, "slug", false, env, false)
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
	got, err := renderAction(context.Background(), a, "/tmp/x")
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
	got, err := renderAction(context.Background(), a, "/tmp/x")
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
		Container:       "myapp",
		ContainerEngine: "docker",
		Run:             []string{"composer install", "php artisan migrate"},
		Cwd:             "/var/www/html",
	}
	got, err := renderAction(context.Background(), a, "/host/wt")
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

func TestRenderActionResolvesContainerRefKeys(t *testing.T) {
	engine := filepath.Join(t.TempDir(), "fake-engine")
	script := "#!/bin/sh\necho \"$*\" > \"$0.args\"\necho resolved-id\n"
	if err := os.WriteFile(engine, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var a config.Action
	body := "run: make\ncompose_service: app\ncompose_project: proj\ncontainer_engine: " + engine + "\ncontainer_network: backend\n"
	if err := yaml.Unmarshal([]byte(body), &a); err != nil {
		t.Fatal(err)
	}
	if got := containerip.FromRef(a.ContainerRef, 0).Network; got != "backend" {
		t.Fatalf("container_network not carried into the lookup: %q", got)
	}
	got, err := renderAction(context.Background(), a, "/host/wt")
	if err != nil {
		t.Fatal(err)
	}
	if want := shellJoin([]string{engine, "exec", "resolved-id", "sh", "-c", "make"}); got != want {
		t.Fatalf("render = %s, want %s", got, want)
	}
	args, err := os.ReadFile(engine + ".args")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"label=com.docker.compose.service=app", "label=com.docker.compose.project=proj"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("engine lookup args %q missing %q", args, want)
		}
	}
}

func TestRenderActionNoContainerPassesThrough(t *testing.T) {
	entry := config.Action{Run: []string{"echo hi"}}
	got, err := renderAction(context.Background(), entry, "/host/wt")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(got, "cd '/host/wt' && echo hi") {
		t.Errorf("expected legacy render, got: %s", got)
	}
}

// TestBuildEnvUserPathWins asserts the user's PATH from inheritedEnv
// keeps precedence — its directories lead the merged PATH, so their
// version-manager shims (asdf, nvm, mise, …) resolve first. The
// login-shell PATH is merged in after (see shellenv.CommandEnv), so the
// user's dirs stay at the front while the user's profile bin dirs are
// still guaranteed present.
func TestBuildEnvUserPathWins(t *testing.T) {
	env, err := buildEnv(
		context.Background(),
		map[string]string{"PATH": "/user/shims:/user/bin"},
		t.TempDir(),
		"/repo",
		"/wt",
		"slug",
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	pathLine := ""
	for _, kv := range env {
		if len(kv) > 5 && kv[:5] == "PATH=" {
			pathLine = kv
		}
	}
	if !strings.HasPrefix(pathLine, "PATH=/user/shims:/user/bin") {
		t.Errorf("user PATH should lead the merged PATH, got: %q", pathLine)
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
	env, err := buildEnv(context.Background(), nil, t.TempDir(), "/repo", "/wt", "slug", false)
	if err != nil {
		t.Fatal(err)
	}
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

func TestFirstFailureErrorNilWhenAllZero(t *testing.T) {
	out := RunOutcome{Groups: []GroupOutcome{
		{Command: "composer install", ExitCode: 0},
		{Command: "yarn install", ExitCode: 0},
	}}
	if err := FirstFailureError(CreateBeforeEngines, out, []int64{1, 2}); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

func TestFirstFailureErrorReportsFirstFailureWithIDAndTail(t *testing.T) {
	out := RunOutcome{Groups: []GroupOutcome{
		{Command: "composer install", ExitCode: 0},
		{Command: "yarn install", ExitCode: 1, StderrTail: "ENOENT: clean-css missing"},
	}}
	err := FirstFailureError(CreateBeforeEngines, out, []int64{10, 11})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"exited 1", "yarn install", "ENOENT: clean-css missing", "--show 11"} {
		if indexOf(msg, want) < 0 {
			t.Errorf("error missing %q:\n%s", want, msg)
		}
	}
}

func TestFirstFailureErrorFallsBackWhenIDUnknown(t *testing.T) {
	out := RunOutcome{Groups: []GroupOutcome{{Command: "composer install", ExitCode: 2}}}
	err := FirstFailureError(CreateBeforeEngines, out, nil)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if indexOf(err.Error(), "logs hooks --all") < 0 {
		t.Errorf("want generic pointer, got:\n%s", err.Error())
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

// TestRunHooksLoadDirenvEnvironment asserts a hook resolves a tool that
// only the worktree's direnv environment provides, as `use flake` does
// for composer/php (#182).
func TestRunHooksLoadDirenvEnvironment(t *testing.T) {
	wt := t.TempDir()
	tools := shellenvtest.FakeTool(t, "composer", "echo composer-from-flake\n")
	env := emptyEnv()
	env["PATH"] = shellenvtest.FlakeDirenv(t, tools) + ":" + env["PATH"]
	entries := []config.Action{{Run: []string{"composer install"}}}
	out, err := RunHooks(context.Background(), CreateBeforeEngines, entries, wt, wt, "slug", false, env, true)
	if err != nil {
		t.Fatal(err)
	}
	if g := out.Groups[0]; g.ExitCode != 0 || !strings.Contains(string(g.LogBody), "composer-from-flake") {
		t.Errorf("hook did not run under the direnv environment: exit=%d log=%q", g.ExitCode, g.LogBody)
	}
}

// TestRunHooksFailsWhenDirenvCannotLoad asserts a blocked `.envrc`
// fails the phase with direnv's own message instead of running the
// hooks without the environment they were written for.
func TestRunHooksFailsWhenDirenvCannotLoad(t *testing.T) {
	wt := t.TempDir()
	env := emptyEnv()
	env["PATH"] = shellenvtest.FakeDirenv(t, "echo '.envrc is blocked' >&2; exit 1\n") + ":" + env["PATH"]
	entries := []config.Action{{Run: []string{"true"}}}
	_, err := RunHooks(context.Background(), CreateBeforeEngines, entries, wt, wt, "slug", false, env, true)
	if err == nil || !strings.Contains(err.Error(), ".envrc is blocked") {
		t.Fatalf("err = %v, want the direnv failure", err)
	}
}
