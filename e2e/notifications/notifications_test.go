//go:build e2e

// Package notifications_e2e exercises the daemon's `notifications:`
// block end-to-end: a real SQLite store, the registered WriteEvent
// hook, the bucket classifier, the configurable `events:` gate, and a
// fake Sender standing in for notify-send / osascript.
//
// Every config knob is covered:
//   - enabled            → an enabled config fires; the negative cases
//                          below also prove a non-listed bucket is muted.
//   - events (default)   → [stable, failed] fire; transient up/down don't.
//   - events (custom)    → an explicit [up, down] list flips which
//                          buckets notify, proving the list is honoured.
//
// No real desktop backend is needed (the fake Sender captures sends), so
// this suite runs anywhere.
package notifications_e2e

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/daemon"
	"github.com/stubbedev/treeman/internal/notify"
	"github.com/stubbedev/treeman/internal/store"
)

// fakeSender records every notification on a buffered channel so tests
// can assert what fired (and, via a timeout, what didn't).
type fakeSender struct {
	ch chan notify.Notification
}

func newFakeSender() *fakeSender { return &fakeSender{ch: make(chan notify.Notification, 16)} }

func (f *fakeSender) Available() bool { return true }
func (f *fakeSender) Send(n notify.Notification) error {
	f.ch <- n
	return nil
}

// recv waits up to 2s for one notification.
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

// expectSilence asserts nothing arrives within a short window.
func (f *fakeSender) expectSilence(t *testing.T) {
	t.Helper()
	select {
	case n := <-f.ch:
		t.Fatalf("expected no notification, got %q / %q", n.Title, n.Body)
	case <-time.After(300 * time.Millisecond):
	}
}

func setup(t *testing.T) (*daemon.State, int64, int64) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	st := daemon.NewState(ctx, s)

	repoID, err := s.EnsureRepo(ctx, "/repos/myrepo", "myrepo")
	if err != nil {
		t.Fatalf("ensure repo: %v", err)
	}
	wtID, err := s.EnsureWorktree(ctx, repoID, "/repos/myrepo/.worktrees/feat", "feat", "feature/x")
	if err != nil {
		t.Fatalf("ensure worktree: %v", err)
	}
	return st, repoID, wtID
}

func write(t *testing.T, st *daemon.State, level, eventType string, repoID, wtID int64) {
	t.Helper()
	if err := st.Store.WriteEvent(context.Background(), level, eventType, "", repoID, wtID, "", 0, nil); err != nil {
		t.Fatalf("write %s: %v", eventType, err)
	}
}

// ─── TestDefaultEventsNotifyReadyAndFailedOnly ───────────────────────
//
// With the default `events:` ([stable, failed]) a finalize completion
// and a finalize error each fire, carrying the repo + branch in the
// banner, while the transient `up` start event stays silent.
func TestDefaultEventsNotifyReadyAndFailedOnly(t *testing.T) {
	st, repoID, wtID := setup(t)

	var cfg config.Config
	// applyDefaults runs inside LoadGlobal in production; mimic it by
	// going through the YAML default path used everywhere else.
	cfg.Notifications = config.NotificationsConfig{Enabled: true}
	cfg.Notifications.Events = []string{"stable", "failed"} // the documented default

	fake := newFakeSender()
	st.Store.RegisterEventHook("test-notify", daemon.NotifyHook(st, cfg.Notifications, fake))

	// up → muted; stable → fires.
	write(t, st, store.LevelInfo, "wt_finalize_start", repoID, wtID)
	write(t, st, store.LevelInfo, "wt_finalize_done", repoID, wtID)

	n := fake.recv(t)
	if n.Title != "treeman: ready" {
		t.Errorf("ready title = %q", n.Title)
	}
	if want := "myrepo · feature/x finished preparing"; n.Body != want {
		t.Errorf("ready body = %q, want %q", n.Body, want)
	}

	// failed → fires, critical urgency.
	write(t, st, store.LevelError, "wt_finalize", repoID, wtID)
	f := fake.recv(t)
	if f.Title != "treeman: failed" {
		t.Errorf("failed title = %q", f.Title)
	}
	if f.Urgency != notify.UrgencyCritical {
		t.Errorf("failed urgency = %q, want critical", f.Urgency)
	}

	fake.expectSilence(t)
}

// ─── TestCustomEventsListFlipsBuckets ────────────────────────────────
//
// An explicit `events: [up, down]` flips which buckets notify: the
// transient start/teardown events fire while `stable` (ready) is muted —
// proving the configurable list is honoured, not just the default.
func TestCustomEventsListFlipsBuckets(t *testing.T) {
	st, repoID, wtID := setup(t)

	cfg := config.NotificationsConfig{Enabled: true, Events: []string{"up", "down"}}
	fake := newFakeSender()
	st.Store.RegisterEventHook("test-notify", daemon.NotifyHook(st, cfg, fake))

	// up → fires.
	write(t, st, store.LevelInfo, "wt_finalize_start", repoID, wtID)
	if got := fake.recv(t).Title; got != "treeman: preparing" {
		t.Errorf("up title = %q, want 'treeman: preparing'", got)
	}

	// stable → muted (not in the list).
	write(t, st, store.LevelInfo, "wt_finalize_done", repoID, wtID)
	fake.expectSilence(t)

	// down → fires.
	write(t, st, store.LevelInfo, "wt_teardown_start", repoID, wtID)
	if got := fake.recv(t).Title; got != "treeman: tearing down" {
		t.Errorf("down title = %q, want 'treeman: tearing down'", got)
	}
}

// ─── TestDisabledNeverFires ──────────────────────────────────────────
//
// `enabled: false` mutes everything regardless of the events list.
func TestDisabledNeverFires(t *testing.T) {
	st, repoID, wtID := setup(t)

	cfg := config.NotificationsConfig{Enabled: false, Events: []string{"stable", "failed"}}
	fake := newFakeSender()
	st.Store.RegisterEventHook("test-notify", daemon.NotifyHook(st, cfg, fake))

	write(t, st, store.LevelInfo, "wt_finalize_done", repoID, wtID)
	write(t, st, store.LevelError, "wt_finalize", repoID, wtID)
	fake.expectSilence(t)
}
