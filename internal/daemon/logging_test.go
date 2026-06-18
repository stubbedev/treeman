package daemon

import (
	"log/slog"
	"testing"
)

func TestSlogLevel(t *testing.T) {
	cases := []struct {
		raw    string
		want   slog.Level
		wantOK bool
	}{
		{"debug", slog.LevelDebug, true},
		{"info", slog.LevelInfo, true},
		{"", slog.LevelInfo, true},
		{"warn", slog.LevelWarn, true},
		{"warning", slog.LevelWarn, true},
		{"error", slog.LevelError, true},
		{"bogus", slog.LevelInfo, false},
		{"DEBUG", slog.LevelInfo, false}, // case-sensitive by design
	}
	for _, c := range cases {
		got, ok := SlogLevel(c.raw)
		if got != c.want || ok != c.wantOK {
			t.Errorf("SlogLevel(%q) = (%v, %v), want (%v, %v)", c.raw, got, ok, c.want, c.wantOK)
		}
	}
}
