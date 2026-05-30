package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestNotifyOnGating(t *testing.T) {
	// Disabled never fires, even for a listed bucket.
	off := NotificationsConfig{Enabled: false, Events: []string{"stable", "failed"}}
	if off.NotifyOn("stable") {
		t.Error("disabled config fired on stable")
	}

	on := NotificationsConfig{Enabled: true, Events: []string{"stable", "failed"}}
	if !on.NotifyOn("stable") || !on.NotifyOn("failed") {
		t.Error("enabled config did not fire on its listed buckets")
	}
	if on.NotifyOn("up") || on.NotifyOn("down") {
		t.Error("enabled config fired on an unlisted bucket")
	}
}

func TestNotificationsDefaultEvents(t *testing.T) {
	// Key absent → default to [stable, failed].
	var cfg Config
	applyDefaults(&cfg)
	if got := cfg.Notifications.Events; len(got) != 2 || got[0] != "stable" || got[1] != "failed" {
		t.Errorf("default events = %v, want [stable failed]", got)
	}

	// Explicit empty list survives (notify on nothing).
	cfg2 := Config{Notifications: NotificationsConfig{Enabled: true, Events: []string{}}}
	applyDefaults(&cfg2)
	if len(cfg2.Notifications.Events) != 0 {
		t.Errorf("explicit empty events overwritten by default: %v", cfg2.Notifications.Events)
	}
}

func TestNotificationsParseFromYAML(t *testing.T) {
	body := "notifications:\n  enabled: true\n  events: [stable, up, failed]\n  backend: notify-send\n"
	var cfg Config
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	applyDefaults(&cfg)
	if !cfg.Notifications.Enabled {
		t.Error("enabled not parsed")
	}
	if !cfg.Notifications.NotifyOn("up") {
		t.Error("up bucket not parsed from events list")
	}
	if cfg.Notifications.Backend != "notify-send" {
		t.Errorf("backend = %q", cfg.Notifications.Backend)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func TestNotificationsValidateRejectsBadBucketAndBackend(t *testing.T) {
	cfg := Config{Notifications: NotificationsConfig{
		Enabled: true,
		Events:  []string{"stable", "stabel"},
		Backend: "notifysend",
	}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for bad bucket + backend")
	}
	msg := err.Error()
	if !strings.Contains(msg, "stabel") {
		t.Errorf("error should name the bad bucket: %v", msg)
	}
	if !strings.Contains(msg, "notifysend") {
		t.Errorf("error should name the bad backend: %v", msg)
	}
}
