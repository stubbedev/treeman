package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/notify"
)

// NotifyCmd — `treeman notify …`. Houses helpers for the opt-in desktop
// notification feature. Today just `test`, which fires a sample banner
// so users can confirm notify-send / osascript actually works on their
// host without waiting for a real worktree lifecycle event.
func NotifyCmd() *cli.Command {
	return &cli.Command{
		Name:  "notify",
		Usage: "desktop notification helpers",
		Commands: []*cli.Command{
			notifyTestCmd(),
		},
	}
}

// notifyTestCmd — `treeman notify test`. Resolves the backend (the
// --backend flag, else the global config's `notifications.backend`, else
// auto-detect), checks it's available, and sends one test banner.
// Deliberately does NOT gate on `notifications.enabled` — the whole
// point is to verify the backend before turning the feature on.
func notifyTestCmd() *cli.Command {
	return &cli.Command{
		Name:  "test",
		Usage: "send a sample desktop notification to verify the backend",
		Description: `Fires a single test banner through the configured (or
auto-detected) notification backend, so you can confirm notify-send
(Linux) / osascript (macOS) actually shows a notification before
enabling notifications: in your config.

Works regardless of notifications.enabled — it tests the transport, not
the opt-in. Use --backend to probe a specific sender.`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "backend",
				Aliases: []string{"b"},
				Usage:   "auto | notify-send | osascript | none (default: notifications.backend, else auto)",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			backend := c.String("backend")
			if backend == "" {
				// Fall back to the global config's choice; LoadGlobal
				// already applies defaults, so an unset key is "auto".
				if gcfg, err := config.LoadGlobal(); err == nil {
					backend = gcfg.Notifications.Backend
				}
			}
			if backend == "none" {
				return errors.New(
					"backend is \"none\" — nothing to test (it mutes all notifications); pass --backend auto|notify-send|osascript to probe a real sender",
				)
			}

			sender := notify.NewSender(backend)
			if !sender.Available() {
				return fmt.Errorf(
					"notification backend %q is not available on this host — install notify-send (Linux) or run on macOS",
					displayBackend(backend),
				)
			}

			n := notify.Notification{
				Title:   "treeman: test",
				Body:    "Desktop notifications are working.",
				Urgency: notify.UrgencyNormal,
			}
			if err := sender.Send(n); err != nil {
				return fmt.Errorf("send test notification: %w", err)
			}
			fmt.Printf("sent a test notification via %s\n", displayBackend(backend))
			return nil
		},
	}
}

// displayBackend renders the backend selector for user output, naming
// the auto / empty case explicitly.
func displayBackend(backend string) string {
	if backend == "" || backend == "auto" {
		return "auto (OS default)"
	}
	return backend
}
