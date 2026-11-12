package hooks

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/progress"
)

func TestRunHooksWaitReportsProgressForEachGroup(t *testing.T) {
	wt := t.TempDir()
	var reports atomic.Int32
	ctx := progress.With(context.Background(), func() { reports.Add(1) })
	entries := []config.Action{{Run: []string{"true"}}, {Run: []string{"true"}}}
	if _, err := RunHooks(ctx, "setup", entries, wt, wt, "slug", false, emptyEnv(), true); err != nil {
		t.Fatal(err)
	}
	if n := int(reports.Load()); n < len(entries) {
		t.Errorf("progress reported %d times for %d groups, want at least one per group", n, len(entries))
	}
}

func TestLogsGrewTracksAppendedOutput(t *testing.T) {
	log := filepath.Join(t.TempDir(), "setup-0.log")
	if err := os.WriteFile(log, []byte("resolving dependencies\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	groups := []GroupOutcome{{LogPath: log}, {}}
	sizes := make([]int64, len(groups))
	if !logsGrew(groups, sizes) {
		t.Fatal("first look at a non-empty log must count as growth")
	}
	if logsGrew(groups, sizes) {
		t.Fatal("an unchanged log must not count as growth")
	}
	f, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("installing c2pa\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if !logsGrew(groups, sizes) {
		t.Fatal("appended output must count as growth")
	}
}
