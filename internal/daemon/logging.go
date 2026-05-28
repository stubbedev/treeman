package daemon

import "log/slog"

// SlogLevel maps a `daemon.log_level` config value to a slog.Level.
// Recognised: "debug", "info" (and ""), "warn"/"warning", "error".
// ok=false for anything else (the caller decides how to warn); the
// returned level is then LevelInfo so an unknown value degrades to the
// default rather than silencing everything.
func SlogLevel(raw string) (slog.Level, bool) {
	switch raw {
	case "debug":
		return slog.LevelDebug, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	case "", "info":
		return slog.LevelInfo, true
	}
	return slog.LevelInfo, false
}
