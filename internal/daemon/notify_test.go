package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/notify"
	"github.com/stubbedev/treeman/internal/store"
)

// fakeSender captures notifications on a buffered channel so a test can
// assert what fired (and, via a timeout, what didn't).
type fakeSender struct{ ch chan notify.Notification }

func newFakeSender() *fakeSender { return &fakeSender{ch: make(chan notify.Notification, 8)} }

func (f *fakeSender) Available() bool { return true }
func (f *fakeSender) Send(_ context.Context, n notify.Notification) error {
	f.ch <- n
	return nil
}

func (f *fakeSender) recv(t *testing.T) notify.Notification {
	t.Helper()
	select {
	case n := <-f.ch:
		return n
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a notification")
		return notify.Notification{}
	}
}

func (f *fakeSender) expectSilence(t *testing.T) {
	t.Helper()
	select {
	case n := <-f.ch:
		t.Fatalf("expected no notification, got %q / %q", n.Title, n.Body)
	case <-time.After(300 * time.Millisecond):
	}
}

func newTestState(t *testing.T) *State {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewState(ctx, s)
}

func writeGlobalConfig(t *testing.T, cfgDir, body string) {
	t.Helper()
	dir := filepath.Join(cfgDir, "treeman")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// TestNotifierLiveReload proves a global-config edit toggles
// notifications at runtime through ReloadAll — the same seam the
// fsnotify watcher, SIGHUP, and the config_reload RPC all reach — with
// no daemon restart.
func TestNotifierLiveReload(t *testing.T) {
	fake := newFakeSender()
	prev := senderFor
	senderFor = func(string) notify.Sender { return fake }
	t.Cleanup(func() { senderFor = prev })

	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)

	st := newTestState(t)
	ctx := context.Background()
	repoID, err := st.Store.EnsureRepo(ctx, "/repos/myrepo", "myrepo")
	if err != nil {
		t.Fatalf("ensure repo: %v", err)
	}
	wtID, err := st.Store.EnsureWorktree(ctx, repoID, "/repos/myrepo/.worktrees/feat", "feat", "feature/x")
	if err != nil {
		t.Fatalf("ensure worktree: %v", err)
	}

	// Start disabled: boot registration installs no hook.
	writeGlobalConfig(t, cfgDir, "notifications:\n  enabled: false\n")
	RegisterNotifier(st)
	if err := st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtWorktreeCreateEnd, "", repoID, wtID, "", 0, nil); err != nil {
		t.Fatalf("write event: %v", err)
	}
	fake.expectSilence(t)

	// Edit the global config to enable, then reload through the live
	// seam — no restart, no re-call of RegisterNotifier by the test.
	writeGlobalConfig(t, cfgDir, "notifications:\n  enabled: true\n  events: [stable]\n")
	cr, err := NewConfigReloader(st)
	if err != nil {
		t.Fatalf("new reloader: %v", err)
	}
	cr.ReloadAll(ctx)

	if err := st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtWorktreeCreateEnd, "", repoID, wtID, "", 0, nil); err != nil {
		t.Fatalf("write event: %v", err)
	}
	n := fake.recv(t)
	if n.Title != "treeman: ready" {
		t.Errorf("after live-enable, title = %q, want 'treeman: ready'", n.Title)
	}

	// Edit again to disable, reload, and confirm the hook is gone.
	writeGlobalConfig(t, cfgDir, "notifications:\n  enabled: false\n")
	cr.ReloadAll(ctx)
	if err := st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtWorktreeCreateEnd, "", repoID, wtID, "", 0, nil); err != nil {
		t.Fatalf("write event: %v", err)
	}
	fake.expectSilence(t)
}

// TestNotifierBackendNoneMutes proves `backend: none` is an intentional
// mute: enabled, but no hook is registered (and no send happens), even
// though a real backend would be available.
func TestNotifierBackendNoneMutes(t *testing.T) {
	fake := newFakeSender()
	prev := senderFor
	senderFor = func(string) notify.Sender { return fake }
	t.Cleanup(func() { senderFor = prev })

	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	writeGlobalConfig(t, cfgDir, "notifications:\n  enabled: true\n  backend: none\n  events: [stable]\n")

	st := newTestState(t)
	ctx := context.Background()
	repoID, err := st.Store.EnsureRepo(ctx, "/repos/myrepo", "myrepo")
	if err != nil {
		t.Fatalf("ensure repo: %v", err)
	}
	wtID, err := st.Store.EnsureWorktree(ctx, repoID, "/repos/myrepo/.worktrees/feat", "feat", "feature/x")
	if err != nil {
		t.Fatalf("ensure worktree: %v", err)
	}

	RegisterNotifier(st)
	if err := st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtWorktreeCreateEnd, "", repoID, wtID, "", 0, nil); err != nil {
		t.Fatalf("write event: %v", err)
	}
	fake.expectSilence(t)
}
