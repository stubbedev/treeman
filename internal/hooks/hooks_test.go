package hooks

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/config"
)

func emptyEnv() map[string]string {
	return map[string]string{"PATH": "/usr/bin:/bin"}
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
	entries := []config.HookEntry{
		{Steps: []config.SingleStep{
			{Run: "touch a"},
			{Run: "touch b"},
		}},
	}
	out, err := RunHooks(context.Background(), "postcreate", entries, wt, wt, "slug", emptyEnv())
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
	entries := []config.HookEntry{
		{Steps: []config.SingleStep{{Run: "sleep 0.5 && touch one"}}},
		{Steps: []config.SingleStep{{Run: "sleep 0.5 && touch two"}}},
	}
	_, err := RunHooks(context.Background(), "postcreate", entries, wt, wt, "slug", emptyEnv())
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
	entries := []config.HookEntry{{Steps: []config.SingleStep{{Run: "sleep 10"}}}}
	started := time.Now()
	_, err := RunHooks(context.Background(), "postcreate", entries, wt, wt, "slug", emptyEnv())
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Errorf("RunHooks blocked for %v", elapsed)
	}
}

func TestInheritedEnvReachesSubprocess(t *testing.T) {
	wt := t.TempDir()
	env := emptyEnv()
	env["TREEMAN_TEST_PATH_PROBE"] = "xyz123"
	entries := []config.HookEntry{
		{Steps: []config.SingleStep{{Run: "printenv TREEMAN_TEST_PATH_PROBE > probe.out"}}},
	}
	_, err := RunHooks(context.Background(), "postcreate", entries, wt, wt, "slug", env)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(func() bool {
		_, err := os.Stat(filepath.Join(wt, "probe.out"))
		return err == nil
	}, 1500)
	b, err := os.ReadFile(filepath.Join(wt, "probe.out"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "xyz123\n" {
		t.Errorf("probe got %q", string(b))
	}
}

func TestPrecreateShortCircuitsOnFailure(t *testing.T) {
	wt := t.TempDir()
	steps := []config.SingleStep{
		{Run: "touch ran-first"},
		{Run: "false"},
		{Run: "touch ran-third"},
	}
	out, err := RunPrecreateHooks(context.Background(), steps, wt, wt, "slug", emptyEnv())
	if err != nil {
		t.Fatal(err)
	}
	if out.AggregateExitCode != 1 {
		t.Errorf("aggregate=%d want 1", out.AggregateExitCode)
	}
	if _, err := os.Stat(filepath.Join(wt, "ran-first")); err != nil {
		t.Error("ran-first missing")
	}
	if _, err := os.Stat(filepath.Join(wt, "ran-third")); err == nil {
		t.Error("ran-third should have been skipped")
	}
}

func TestRenderGroupChainsWithAnd(t *testing.T) {
	steps := []config.SingleStep{
		{Run: "echo a"},
		{Run: "echo b", Cwd: "/tmp/sub"},
	}
	got := renderGroup(steps, "/tmp/x")
	if !contains(got, " && ") {
		t.Errorf("missing &&: %s", got)
	}
	if !contains(got, "cd '/tmp/x' && echo a") {
		t.Errorf("missing first clause: %s", got)
	}
	if !contains(got, "cd '/tmp/sub' && echo b") {
		t.Errorf("missing second clause: %s", got)
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
