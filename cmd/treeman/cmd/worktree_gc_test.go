package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/store"
)

func TestParseDaysDuration(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"30d", 30 * 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"0d", 0, false},
		{"72h", 72 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"xxd", 0, true},
		{"-5d", 0, true},
		{"nope", 0, true},
	}
	for _, c := range cases {
		got, err := parseDaysDuration(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseDaysDuration(%q): want error, got %v", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("parseDaysDuration(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
}

func TestGcSelectorsDefaults(t *testing.T) {
	sel := gcSelectors

	stale, merged, ref, err := sel("", "", false)
	if err != nil || stale != 30*24*time.Hour || merged || ref != "" {
		t.Errorf("no selectors: stale=%v merged=%v ref=%q err=%v, want 30d/false/empty", stale, merged, ref, err)
	}
	stale, merged, ref, _ = sel("", "", true)
	if !merged || ref != "" {
		t.Errorf("--merged: merged=%v ref=%q stale=%v, want true/empty/0", merged, ref, stale)
	}
	if stale != 0 {
		t.Errorf("--merged alone must disable the stale arm, got %v", stale)
	}
	_, merged, ref, _ = sel("", "origin/master", true)
	if !merged || ref != "origin/master" {
		t.Errorf("--merged=origin/master: merged=%v ref=%q", merged, ref)
	}
	if _, _, _, err := sel("bogus", "", false); err == nil {
		t.Error("--stale bogus must error")
	}
}

func TestStaleAge(t *testing.T) {
	now := time.Now()
	// A nonexistent path keeps headCommitTs out of the picture (and,
	// in tests, away from the cwd's own git repo).
	mk := func(lastEventMs int64) gcCandidate {
		return gcCandidate{row: store.WorktreeRow{Path: "/nonexistent"}, lastEventMs: lastEventMs}
	}
	old := mk(now.Add(-45 * 24 * time.Hour).UnixMilli())
	fresh := mk(now.Add(-2 * time.Hour).UnixMilli())
	if !gcStale(old, now, 30*24*time.Hour) {
		t.Error("45d-old worktree must be stale at 30d")
	}
	if gcStale(fresh, now, 30*24*time.Hour) {
		t.Error("2h-old worktree must not be stale at 30d")
	}
	never := mk(0) // no events, no HEAD, no visits
	if !gcStale(never, now, 24*time.Hour) {
		t.Error("never-seen worktree must count as stale")
	}
}

func TestCacheDirsBytesAndDrop(t *testing.T) {
	wt := t.TempDir()
	for _, d := range []string{"node_modules/pkg/a", "vendor/lib", "storage/dumps", "app/models"} {
		if err := os.MkdirAll(filepath.Join(wt, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"node_modules/pkg/a/index.js", "vendor/lib/lib.go", "storage/dumps/x.sql", "app/models/user.go", "README"} {
		if err := os.WriteFile(filepath.Join(wt, f), []byte(strings.Repeat("x", 100)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	size := cacheDirsBytes(wt)
	if size != 300 { // three 100-byte files in cache dirs; app + README don't count
		t.Errorf("cacheDirsBytes = %d, want 300", size)
	}

	reclaimed, err := dropCacheDirs(wt)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed != 300 {
		t.Errorf("dropCacheDirs reclaimed = %d, want 300", reclaimed)
	}
	for _, gone := range []string{"node_modules", "vendor", "storage"} {
		if _, err := os.Stat(filepath.Join(wt, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should be removed", gone)
		}
	}
	// The checkout itself stays.
	if _, err := os.Stat(filepath.Join(wt, "app/models/user.go")); err != nil {
		t.Errorf("checkout files must survive: %v", err)
	}
	if cacheDirsBytes(wt) != 0 {
		t.Error("second pass must find nothing to reclaim")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		512:            "512 B",
		2 << 10:        "2.0 KB",
		3 * (1 << 20):  "3.0 MB",
		15 * (1 << 30): "15.0 GB",
	}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
