package gitcmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandPathOverride(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	selfDir := filepath.Dir(exe)
	sep := string(os.PathListSeparator)

	t.Run("no override pins self dir onto inherited PATH", func(t *testing.T) {
		cmd := command(context.Background(), "", false, "status")
		got := pathValue(cmd.Env)
		if !strings.HasPrefix(got, selfDir+sep) && got != selfDir {
			t.Fatalf("PATH %q does not start with self dir %q", got, selfDir)
		}
	})

	t.Run("WithPath replaces PATH then self dir is prepended", func(t *testing.T) {
		ctx := WithPath(context.Background(), "/shell/bin"+sep+"/usr/bin")
		cmd := command(ctx, "", false, "status")
		got := pathValue(cmd.Env)
		want := selfDir + sep + "/shell/bin" + sep + "/usr/bin"
		if got != want {
			t.Fatalf("PATH = %q, want %q", got, want)
		}
	})

	t.Run("WithPath empty is a no-op", func(t *testing.T) {
		a := WithPath(context.Background(), "")
		if a != context.Background() {
			t.Fatal("WithPath(\"\") should return ctx unchanged")
		}
	})

	t.Run("self dir already first is not duplicated", func(t *testing.T) {
		ctx := WithPath(context.Background(), selfDir+sep+"/usr/bin")
		cmd := command(ctx, "", false, "status")
		got := pathValue(cmd.Env)
		if got != selfDir+sep+"/usr/bin" {
			t.Fatalf("PATH = %q, want no duplicate self dir", got)
		}
	})
}

func TestSetEnvKey(t *testing.T) {
	t.Run("replaces existing", func(t *testing.T) {
		env := []string{"A=1", "PATH=old", "B=2"}
		env = setEnvKey(env, "PATH", "new")
		if got := pathValue(env); got != "new" {
			t.Fatalf("PATH = %q, want new", got)
		}
		if len(env) != 3 {
			t.Fatalf("len = %d, want 3 (replace, not append)", len(env))
		}
	})
	t.Run("appends when absent", func(t *testing.T) {
		env := setEnvKey([]string{"A=1"}, "PATH", "p")
		if got := pathValue(env); got != "p" {
			t.Fatalf("PATH = %q, want p", got)
		}
	})
}

// pathValue returns the PATH= value in a name=value env slice, or "".
func pathValue(env []string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			return v
		}
	}
	return ""
}
