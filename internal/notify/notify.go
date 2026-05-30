// Package notify sends opt-in desktop notifications when a worktree
// crosses a lifecycle status boundary. The OS-specific senders live in
// notify_linux.go / notify_darwin.go / notify_other.go (build-tagged),
// mirroring the daemon's socket_linux.go / socket_other.go split.
//
// The package is deliberately split into a pure classification layer
// (Bucket) and a side-effecting transport (Sender). The daemon wires
// the two together; tests drive the classification + a fake Sender
// without ever shelling out to notify-send / osascript.
package notify

import "context"

// Urgency is the notification priority hint. notify-send maps it to
// `--urgency`; macOS has no per-banner urgency so it's ignored there.
type Urgency string

const (
	UrgencyNormal   Urgency = "normal"
	UrgencyCritical Urgency = "critical"
)

// Notification is one desktop banner to display.
type Notification struct {
	Title   string
	Body    string
	Urgency Urgency
}

// Sender delivers a Notification to the desktop. Implementations are
// best-effort: a missing backend binary returns an error the caller
// logs and swallows rather than propagating.
type Sender interface {
	// Send delivers the notification. Returns an error when the backend
	// is unavailable or the send failed.
	Send(ctx context.Context, n Notification) error
	// Available reports whether the backend can actually deliver (the
	// binary exists / the platform is supported). Lets the daemon skip
	// registering the notifier — and log once — instead of failing on
	// every event.
	Available() bool
}

// NewSender returns the Sender for the given backend selector:
//
//   - "auto" / "": the OS-native sender (notify-send / osascript), or a
//     no-op on unsupported platforms.
//   - "notify-send": force the Linux notify-send sender.
//   - "osascript": force the macOS osascript sender.
//   - "none": a sender that always no-ops (Available reports false).
//
// Unknown selectors fall back to "none" — config validation already
// rejects them, so this is just defence in depth.
func NewSender(backend string) Sender {
	switch backend {
	case "none":
		return noopSender{}
	case "notify-send":
		return notifySendSender{}
	case "osascript":
		return osascriptSender{}
	default: // "auto", "" and anything else
		return osSender()
	}
}

// noopSender drops every notification. Used for the "none" backend and
// as the platform fallback where no native sender exists.
type noopSender struct{}

func (noopSender) Send(context.Context, Notification) error { return nil }
func (noopSender) Available() bool                          { return false }
