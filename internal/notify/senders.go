package notify

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// sendTimeout bounds a single notification subprocess so a wedged
// notify-send / osascript can't leak a goroutine on the event path.
const sendTimeout = 5 * time.Second

// notifySendSender shells out to `notify-send` (libnotify), the de-facto
// Linux desktop notification CLI. Available on any host where the binary
// is on PATH, regardless of GOOS — so forcing `backend: notify-send`
// works even on non-Linux as long as the tool exists.
type notifySendSender struct{}

func (notifySendSender) Available() bool {
	_, err := exec.LookPath("notify-send")
	return err == nil
}

func (notifySendSender) Send(n Notification) error {
	path, err := exec.LookPath("notify-send")
	if err != nil {
		return fmt.Errorf("notify-send not found: %w", err)
	}
	urgency := string(n.Urgency)
	if urgency == "" {
		urgency = string(UrgencyNormal)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	// notify-send <summary> <body>. App name keeps the banner grouped
	// under "treeman" in notification centres that support grouping.
	// Args are internally composed (no shell), so title/body can't inject.
	cmd := exec.CommandContext(ctx, path, "--app-name=treeman", "--urgency="+urgency, n.Title, n.Body)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("notify-send: %w: %s", err, out)
	}
	return nil
}

// osascriptSender shells out to macOS `osascript` to post a native
// Notification Center banner. Available wherever the binary is on PATH
// (i.e. macOS).
type osascriptSender struct{}

func (osascriptSender) Available() bool {
	_, err := exec.LookPath("osascript")
	return err == nil
}

func (osascriptSender) Send(n Notification) error {
	path, err := exec.LookPath("osascript")
	if err != nil {
		return fmt.Errorf("osascript not found: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	// Build the AppleScript with -e and pass title/body as separate
	// arguments referenced positionally, so a quote or backslash in the
	// branch name can't break out of the string literal or inject
	// AppleScript. `item N of argv` reads the trailing args verbatim.
	script := `on run argv
	display notification (item 1 of argv) with title (item 2 of argv)
end run`
	cmd := exec.CommandContext(ctx, path, "-e", script, n.Body, n.Title)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("osascript: %w: %s", err, out)
	}
	return nil
}
