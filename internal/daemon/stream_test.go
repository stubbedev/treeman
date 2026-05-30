package daemon

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/store"
)

// TestStreamingSubscribe_EndToEnd spins up a unix-socket listener,
// opens an rpc.SubscribeEvents stream against it, writes events to
// the daemon's store, and asserts that matching events arrive on the
// client channel. Covers the full hook-driven push path end-to-end —
// the foundation for logs_subscribe's "mode=push".
func TestStreamingSubscribe_EndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	sockPath := filepath.Join(t.TempDir(), "treeman.sock")
	t.Setenv("TREEMAN_SOCKET", sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	st := NewState(ctx, s)

	// Minimal accept loop — mirrors cmd/treemand/main.go's handleConn
	// but inline to avoid importing the cmd package.
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		dec := json.NewDecoder(conn)
		enc := json.NewEncoder(conn)
		var req rpc.Request
		if err := dec.Decode(&req); err != nil {
			return
		}
		if IsStreamingMethod(req.Method) {
			DispatchStreaming(ctx, st, enc, req)
		}
	}()

	stream, stop, err := rpc.SubscribeEvents(ctx, rpc.EventSubscribeArgs{
		EventTypes: []string{"unit_test_match"},
	})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	defer stop()

	// Give the subscriber time to register its hook.
	time.Sleep(50 * time.Millisecond)

	// Write one matching + one non-matching event. Only the matching
	// one should arrive on the channel.
	_ = s.WriteEvent(ctx, store.LevelInfo, "unit_test_match", "yes", 0, 0, "", 0, nil)
	_ = s.WriteEvent(ctx, store.LevelInfo, "unit_test_skip", "no", 0, 0, "", 0, nil)

	select {
	case ev := <-stream:
		if ev.EventType != "unit_test_match" {
			t.Errorf("got event_type %q, want unit_test_match", ev.EventType)
		}
		if ev.Message != "yes" {
			t.Errorf("got message %q, want 'yes'", ev.Message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for matched event")
	}

	// Drain one more tick — the skipped event must NOT arrive.
	select {
	case ev := <-stream:
		t.Errorf("unexpected non-matching event delivered: %+v", ev)
	case <-time.After(200 * time.Millisecond):
		// OK
	}
}

// TestStreamingSubscribe_LevelFilter — confirms the daemon-side
// filter respects Levels (case-insensitive). Sends two events, only
// the matching level should come through.
func TestStreamingSubscribe_LevelFilter(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sockPath := filepath.Join(t.TempDir(), "treeman.sock")
	t.Setenv("TREEMAN_SOCKET", sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	st := NewState(ctx, s)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		dec := json.NewDecoder(conn)
		enc := json.NewEncoder(conn)
		var req rpc.Request
		if err := dec.Decode(&req); err != nil {
			return
		}
		DispatchStreaming(ctx, st, enc, req)
	}()

	stream, stop, err := rpc.SubscribeEvents(ctx, rpc.EventSubscribeArgs{
		Levels: []string{"error"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	time.Sleep(50 * time.Millisecond)

	_ = s.WriteEvent(ctx, store.LevelInfo, "noise", "info-level", 0, 0, "", 0, nil)
	_ = s.WriteEvent(ctx, store.LevelError, "boom", "error-level", 0, 0, "", 0, nil)

	select {
	case ev := <-stream:
		if ev.Level != "error" {
			t.Errorf("got level %q, want error", ev.Level)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
	select {
	case ev := <-stream:
		t.Errorf("unexpected extra event: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
}
