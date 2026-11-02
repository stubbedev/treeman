package shellenv

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/shellenv/shellenvtest"
)

func TestParseProbedPATH(t *testing.T) {
	frame := func(p string) string { return "\n" + pathProbeBegin + p + pathProbeEnd + "\n" }
	cases := []struct {
		name, out, want string
		wantErr         bool
	}{
		{"clean", frame("/a:/b"), "/a:/b", false},
		{
			"rc noise before the frame is ignored (#133)",
			"\x1b[33mfzftab module needs to be rebuild, rebuild now?[Y/n]\n\x1b[39m" + frame("/nix-profile/bin:/usr/bin"),
			"/nix-profile/bin:/usr/bin", false,
		},
		{"noise after the frame is ignored", frame("/a") + "bye\n", "/a", false},
		{"no sentinels", "/a:/b", "", true},
		{"missing end sentinel", "\n" + pathProbeBegin + "/a", "", true},
		{"empty PATH", frame(""), "", true},
		{"relative entry", frame("bin:/usr/bin"), "", true},
		{"control character entry", frame("/usr/bin\x1b[39m:/bin"), "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseProbedPATH([]byte(c.out))
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestPathProbeCommandSurvivesRcNoise(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	rc := filepath.Join(t.TempDir(), "rc")
	if err := os.WriteFile(rc, []byte("echo 'rebuild now?[Y/n]'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, "-c", ". \"$RC\"; "+pathProbeCommand)
	cmd.Env = []string{"RC=" + rc, "PATH=/probe/bin:/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseProbedPATH(out)
	if err != nil {
		t.Fatalf("parse %q: %v", out, err)
	}
	if got != "/probe/bin:/usr/bin:/bin" {
		t.Errorf("got %q", got)
	}
}

func TestMergePaths(t *testing.T) {
	cases := []struct {
		name, base, extra, want string
	}{
		{"empty extra returns base", "/a:/b", "", "/a:/b"},
		{"empty base returns extra", "", "/x:/y", "/x:/y"},
		{"both empty", "", "", ""},
		{"appends only new dirs", "/a:/b", "/b:/c", "/a:/b:/c"},
		{"base precedence preserved", "/user/bin", "/nix/bin:/user/bin", "/user/bin:/nix/bin"},
		{"all new appended in order", "/a", "/b:/c", "/a:/b:/c"},
		{"skips empty segments in extra", "/a", ":/b:", "/a:/b"},
		{"fully overlapping extra is no-op", "/a:/b", "/a:/b", "/a:/b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MergePaths(c.base, c.extra); got != c.want {
				t.Errorf("MergePaths(%q, %q) = %q, want %q", c.base, c.extra, got, c.want)
			}
		})
	}
}

// TestBaseEnvMergesLoginPath asserts the inheritedEnv PATH keeps
// precedence (leads) while the login-shell PATH is folded in after.
func TestBaseEnvMergesLoginPath(t *testing.T) {
	stubLoginShellPATH(t, "/nix-profile/bin:/user/shims")

	env := baseEnv(map[string]string{"PATH": "/user/shims:/user/bin"})
	got := env["PATH"]
	const wantPrefix = "/user/shims:/user/bin"
	if got[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("inheritedEnv PATH should lead, got %q", got)
	}
	if got != "/user/shims:/user/bin:/nix-profile/bin" {
		t.Errorf("login-shell dirs not merged correctly: %q", got)
	}
}

// TestBaseEnvLoginPathLeadsWithoutInherited asserts that with no
// inherited PATH (externally-created worktree) the login-shell dirs
// lead the daemon floor, so a stale /usr/local/bin tool can't shadow
// the user's profile bin (#121).
func TestBaseEnvLoginPathLeadsWithoutInherited(t *testing.T) {
	stubLoginShellPATH(t, "/nix-profile/bin:/usr/bin")
	t.Setenv("PATH", "/usr/local/bin:/usr/bin")

	for name, inherited := range map[string]map[string]string{
		"nil":        nil,
		"empty PATH": {"PATH": ""},
		"no PATH":    {"FOO": "bar"},
	} {
		t.Run(name, func(t *testing.T) {
			got := baseEnv(inherited)["PATH"]
			if want := "/nix-profile/bin:/usr/bin:/usr/local/bin"; got != want {
				t.Errorf("PATH = %q, want %q", got, want)
			}
		})
	}
}

// TestLoginShellPATHDoesNotCacheFailure asserts a failed probe is
// retried (after the throttle window) instead of being cached for the
// daemon's lifetime, and that a success is cached (#121).
func TestLoginShellPATHDoesNotCacheFailure(t *testing.T) {
	origProbe := loginProbe
	defer func() {
		loginProbe = origProbe
		loginPATH.value, loginPATH.lastFailed = "", time.Time{}
	}()
	loginPATH.value, loginPATH.lastFailed = "", time.Time{}

	calls := 0
	result := ""
	loginProbe = func() string { calls++; return result }

	if got := cachedLoginShellPATH(); got != "" || calls != 1 {
		t.Fatalf("first probe: got %q calls=%d", got, calls)
	}
	// Inside the throttle window: no re-probe.
	if got := cachedLoginShellPATH(); got != "" || calls != 1 {
		t.Fatalf("throttled probe: got %q calls=%d", got, calls)
	}
	// Window elapsed and the profile is ready now.
	loginPATH.lastFailed = time.Now().Add(-loginProbeRetry - time.Second)
	result = "/nix-profile/bin"
	if got := cachedLoginShellPATH(); got != "/nix-profile/bin" || calls != 2 {
		t.Fatalf("retry probe: got %q calls=%d", got, calls)
	}
	// Success is cached.
	result = ""
	if got := cachedLoginShellPATH(); got != "/nix-profile/bin" || calls != 2 {
		t.Fatalf("cached probe: got %q calls=%d", got, calls)
	}
}

func stubLoginShellPATH(t *testing.T, p string) {
	t.Helper()
	orig := LoginShellPATH
	t.Cleanup(func() { LoginShellPATH = orig })
	LoginShellPATH = func() string { return p }
}

func TestCommandEnvAppliesDirenvDiff(t *testing.T) {
	stubLoginShellPATH(t, "")
	bin := shellenvtest.FakeDirenv(t, `[ "$1 $2" = "export json" ] || exit 2
printf '{"PATH":"/flake/bin:%s","FLAKE_DIR":"%s","STALE":null}' "$PATH" "$(pwd)"
`)
	dir := t.TempDir()
	env, err := CommandEnv(context.Background(), map[string]string{"PATH": bin, "STALE": "1"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := env["PATH"], "/flake/bin:"+bin; got != want {
		t.Errorf("PATH = %q, want %q", got, want)
	}
	if got := env["FLAKE_DIR"]; got != dir {
		t.Errorf("FLAKE_DIR = %q, want direnv run in %q", got, dir)
	}
	if _, ok := env["STALE"]; ok {
		t.Error("STALE should be unset by a null in the direnv diff")
	}
}

func TestCommandEnvNoDirenvDiff(t *testing.T) {
	stubLoginShellPATH(t, "")
	bin := shellenvtest.FakeDirenv(t, "exit 0\n")
	env, err := CommandEnv(context.Background(), map[string]string{"PATH": bin, "KEEP": "1"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if env["KEEP"] != "1" || env["PATH"] != bin {
		t.Errorf("env changed without a direnv diff: KEEP=%q PATH=%q", env["KEEP"], env["PATH"])
	}
}

func TestCommandEnvWithoutDirenvOnPath(t *testing.T) {
	stubLoginShellPATH(t, "")
	empty := t.TempDir()
	env, err := CommandEnv(context.Background(), map[string]string{"PATH": empty, "KEEP": "1"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if env["KEEP"] != "1" || env["PATH"] != empty {
		t.Errorf("env changed without direnv: KEEP=%q PATH=%q", env["KEEP"], env["PATH"])
	}
}

func TestCommandEnvFailsOnBlockedEnvrc(t *testing.T) {
	stubLoginShellPATH(t, "")
	bin := shellenvtest.FakeDirenv(t, "echo 'direnv: error .envrc is blocked. Run `direnv allow` to approve its content' >&2\nexit 1\n")
	dir := t.TempDir()
	_, err := CommandEnv(context.Background(), map[string]string{"PATH": bin}, dir)
	if err == nil {
		t.Fatal("expected an error for a blocked .envrc")
	}
	for _, want := range []string{dir, "is blocked", "direnv allow"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestBaseEnvDropsDaemonDirenvState(t *testing.T) {
	stubLoginShellPATH(t, "")
	t.Setenv("DIRENV_DIFF", "daemon")
	t.Setenv("DIRENV_DIR", "-/daemon")
	if env := baseEnv(nil); env["DIRENV_DIFF"] != "" || env["DIRENV_DIR"] != "" {
		t.Errorf("daemon direnv state leaked: DIRENV_DIFF=%q DIRENV_DIR=%q", env["DIRENV_DIFF"], env["DIRENV_DIR"])
	}
	if env := baseEnv(map[string]string{"DIRENV_DIFF": "user"}); env["DIRENV_DIFF"] != "user" {
		t.Errorf("user direnv state dropped: DIRENV_DIFF=%q", env["DIRENV_DIFF"])
	}
}

func TestCommandEnvSetsPWD(t *testing.T) {
	stubLoginShellPATH(t, "")
	t.Setenv("PWD", "/daemon/cwd")
	dir := t.TempDir()
	env, err := CommandEnv(context.Background(), map[string]string{"PATH": t.TempDir(), "PWD": "/cli/cwd"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if env["PWD"] != dir {
		t.Errorf("PWD = %q, want %q", env["PWD"], dir)
	}
}
