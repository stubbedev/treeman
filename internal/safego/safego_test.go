package safego

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestGoRecoversPanic is the package's load-bearing guarantee: a
// panicking goroutine must neither crash the process nor die silently
// — the panic lands in the log with its label + detail.
func TestGoRecoversPanic(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, nil)))
	defer slog.SetDefault(old)

	done := make(chan struct{})
	Go("test:panicker", "wt-1", func() {
		defer close(done)
		panic("boom")
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("goroutine did not finish")
	}
	// The deferred Recover runs AFTER our deferred close(done) — poll
	// briefly for the log line instead of racing it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		out := buf.String()
		mu.Unlock()
		if strings.Contains(out, "goroutine panic") {
			for _, want := range []string{"test:panicker", "wt-1", "boom"} {
				if !strings.Contains(out, want) {
					t.Errorf("panic log missing %q: %s", want, out)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("panic was not logged: %s", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestGoRunsFunction: the happy path actually executes.
func TestGoRunsFunction(t *testing.T) {
	done := make(chan int, 1)
	Go("test:worker", "", func() { done <- 42 })
	select {
	case v := <-done:
		if v != 42 {
			t.Fatalf("got %d", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fn never ran")
	}
}

// TestRecoverInline covers the exported Recover for WaitGroup lanes
// that own their own defer ordering.
func TestRecoverInline(t *testing.T) {
	var wg sync.WaitGroup
	wg.Go(func() {
		defer Recover("test:inline", "")
		panic("inline boom")
	})
	waited := make(chan struct{})
	go func() { wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("wg.Done was skipped — Recover must not swallow the defer chain")
	}
}

// lockedWriter serializes concurrent handler writes during the test.
type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
