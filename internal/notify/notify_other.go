//go:build !linux && !darwin

package notify

// osSender — fallback for platforms without a known native sender
// (Windows, BSDs). Notifications silently no-op there.
func osSender() Sender { return noopSender{} }
