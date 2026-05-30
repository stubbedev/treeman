package daemon

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/notify"
	"github.com/stubbedev/treeman/internal/store"
)

// notifyHookID is the registry key the desktop-notification hook lives
// under. A constant so re-registration (on config reload) replaces
// rather than duplicates.
const notifyHookID = "desktop-notify"

// senderFor builds the Sender for a configured backend. A package var
// so tests can substitute a fake without a real notify-send / osascript
// on the host; production always uses notify.NewSender.
var senderFor = notify.NewSender

// RegisterNotifier wires desktop notifications into the event stream.
// Reads the global `notifications:` config; if the feature is disabled
// or no backend is available, it unregisters any prior hook and returns
// without installing one (so a reload that turns the feature off stops
// notifications immediately).
//
// Called at daemon boot and again on every global config reload, so the
// captured config + sender always reflect the current YAML.
func RegisterNotifier(st *State) {
	gcfg, err := config.LoadGlobal()
	if err != nil {
		slog.Warn("notifications: load global config failed, leaving notifier unchanged", "err", err)
		return
	}
	nc := gcfg.Notifications
	// `enabled: false` or `backend: none` both mean "don't notify" — the
	// latter is a deliberate per-host mute that keeps the rest of the
	// shared config intact, so neither warns.
	if !nc.Enabled || nc.Backend == "none" {
		st.Store.UnregisterEventHook(notifyHookID)
		return
	}
	sender := senderFor(nc.Backend)
	if !sender.Available() {
		slog.Warn("notifications: enabled but no backend available — install notify-send (Linux) or run on macOS; skipping",
			"backend", nc.Backend)
		st.Store.UnregisterEventHook(notifyHookID)
		return
	}
	st.Store.RegisterEventHook(notifyHookID, NotifyHook(st, nc, sender))
	slog.Info("notifications enabled", "events", nc.Events, "backend", nc.Backend)
}

// NotifyHook builds the store EventHook that turns lifecycle events into
// desktop notifications. The returned hook offloads to a goroutine —
// store.fireEventHooks runs hooks synchronously on the WriteEvent path,
// and the dispatch does DB lookups + a subprocess exec that must never
// block event ingestion.
func NotifyHook(st *State, nc config.NotificationsConfig, sender notify.Sender) store.EventHook {
	return func(ev store.Event) {
		go dispatchNotification(st.BgCtx, st, nc, sender, ev)
	}
}

// dispatchNotification is the synchronous core: classify the event,
// gate on the configured buckets, compose the banner from repo + branch,
// and send. Exposed (package-internal) so tests can drive it
// deterministically without the goroutine in NotifyHook.
func dispatchNotification(
	ctx context.Context,
	st *State,
	nc config.NotificationsConfig,
	sender notify.Sender,
	ev store.Event,
) {
	bucket := notify.Bucket(ev.EventType, ev.Level)
	if bucket == "" || !nc.NotifyOn(bucket) {
		return
	}
	repo, target := notificationSubject(ctx, st, ev)
	if err := sender.Send(ctx, notify.Compose(bucket, repo, target)); err != nil {
		slog.Warn("notifications: send failed", "bucket", bucket, "err", err)
	}
}

// notificationSubject resolves the repo display name and the worktree
// branch (falling back to the worktree dir name) for the banner. Best-
// effort: any lookup miss yields an empty string, which Compose handles.
func notificationSubject(ctx context.Context, st *State, ev store.Event) (repo, target string) {
	if ev.RepoID.Valid {
		if p, err := st.Store.RepoPath(ctx, ev.RepoID.Int64); err == nil && p != "" {
			repo = filepath.Base(p)
		}
	}
	if ev.WorktreeID.Valid {
		if b, err := st.Store.WorktreeBranch(ctx, ev.WorktreeID.Int64); err == nil && b != "" {
			target = b
		} else if p, err := st.Store.WorktreePathByID(ctx, ev.WorktreeID.Int64); err == nil && p != "" {
			target = filepath.Base(p)
		}
	}
	return repo, target
}
