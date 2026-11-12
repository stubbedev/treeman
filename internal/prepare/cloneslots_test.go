package prepare

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/engine"
)

// Regression for #199: concurrent prepares against one server each ran
// their own fan-out limit, so eight worktrees ran 72 restores at once
// against one mysqld. Sharing CloneSlots holds them to one limit.
func TestCloneSlotsCapRestoresAcrossPrepares(t *testing.T) {
	slots := NewCloneSlots()
	key := serverKey{host: "127.0.0.1", port: 3306}
	opts := RunOptions{CloneSlots: slots}
	const override, prepares, clonesEach = 3, 4, 5

	var running, peak atomic.Int32
	track := func() {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		running.Add(-1)
	}

	var wg sync.WaitGroup
	errs := make(chan error, prepares)
	for i := range prepares {
		wg.Go(func() {
			clones := make([]string, clonesEach)
			for j := range clones {
				clones[j] = fmt.Sprintf("wt%d_t%d", i, j)
			}
			errs <- fanOutClones(context.Background(), nil, 0, 0,
				func(context.Context, string, string) error { track(); return nil },
				"tmpl", clones, "mysql", override, opts.serverBudget(0, key))
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := peak.Load(); got > override {
		t.Errorf("peak concurrent restores across prepares = %d, want <= %d", got, override)
	}
}

func TestCloneSlotsSeparateServersDoNotShare(t *testing.T) {
	slots := NewCloneSlots()
	a := slots.server(serverKey{host: "a"})
	b := slots.server(serverKey{host: "b"})
	ctx := context.Background()
	if err := a.acquire(ctx, 1, nil); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := b.acquire(waitCtx, 1, nil); err != nil {
		t.Fatalf("a full server must not block another server: %v", err)
	}
}

// Regression for #210: the key was the configured spelling, so repos
// reaching one mysqld as localhost and as 127.0.0.1 (or as a container
// published on loopback) each got their own pool and the cap did not
// hold across them.
func TestCloneSlotsLoopbackAliasesShareOnePool(t *testing.T) {
	aliases := []string{
		"localhost", "LOCALHOST", "localhost.", "127.0.0.1", "127.1.2.3",
		"::1", "0:0:0:0:0:0:0:1", "::ffff:127.0.0.1", "0.0.0.0", "::",
	}
	slots := NewCloneSlots()
	held := slots.server(newServerKey(engine.FamilyMySQL, "127.0.0.1", 3306))
	ctx := context.Background()
	if err := held.acquire(ctx, 1, nil); err != nil {
		t.Fatal(err)
	}
	for _, host := range aliases {
		t.Run(host, func(t *testing.T) {
			s := slots.server(newServerKey(engine.FamilyMySQL, host, 3306))
			waitCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
			defer cancel()
			if err := s.acquire(waitCtx, 1, nil); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s:3306 must wait on the slot held through 127.0.0.1:3306, got %v", host, err)
			}
		})
	}
}

func TestCloneSlotsSpellingsOfOneServerShareOnePool(t *testing.T) {
	cases := []struct{ a, b string }{
		{"DB.Internal", "db.internal."},
		{"10.0.0.5", "::ffff:10.0.0.5"},
		{"2001:db8::1", "2001:DB8:0:0:0:0:0:1"},
		{"/var/run/mysqld/mysqld.sock", "/var/run//mysqld/./mysqld.sock"},
	}
	for _, c := range cases {
		a := newServerKey(engine.FamilyMySQL, c.a, 3306)
		b := newServerKey(engine.FamilyMySQL, c.b, 3306)
		slots := NewCloneSlots()
		if slots.server(a) != slots.server(b) {
			t.Errorf("%q and %q name one server but got separate pools", c.a, c.b)
		}
	}
}

func TestCloneSlotsDistinctEndpointsDoNotShare(t *testing.T) {
	base := newServerKey(engine.FamilyMySQL, "localhost", 3306)
	others := []struct {
		name string
		key  serverKey
	}{
		{"another port", newServerKey(engine.FamilyMySQL, "localhost", 3307)},
		{"another family", newServerKey(engine.FamilyPostgres, "localhost", 3306)},
		{"a bridge IP", newServerKey(engine.FamilyMySQL, "172.17.0.2", 3306)},
		{"a remote host", newServerKey(engine.FamilyMySQL, "db.internal", 3306)},
		{"a unix socket", newServerKey(engine.FamilyMySQL, "/var/run/mysqld/mysqld.sock", 3306)},
		{"a scoped ipv6", newServerKey(engine.FamilyMySQL, "fe80::1%eth0", 3306)},
		{"a localhost subdomain", newServerKey(engine.FamilyMySQL, "db.localhost.example", 3306)},
	}
	slots := NewCloneSlots()
	for _, o := range others {
		if slots.server(base) == slots.server(o.key) {
			t.Errorf("%s must get its own pool, shared localhost:3306's", o.name)
		}
	}
}

func TestCloneSlotsReportQueueAndAdmission(t *testing.T) {
	s := NewCloneSlots().server(serverKey{host: "h"})
	ctx := context.Background()
	if err := s.acquire(ctx, 1, nil); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []CloneProgress
	progress := func(p CloneProgress) {
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
	}
	admitted := make(chan error, 1)
	go func() { admitted <- s.acquire(ctx, 1, progress) }()

	deadline := time.After(time.Second)
	for {
		mu.Lock()
		queued := len(got) == 1 && got[0] == CloneQueued
		mu.Unlock()
		if queued {
			break
		}
		select {
		case <-deadline:
			t.Fatal("a restore over the limit must report CloneQueued")
		case <-time.After(time.Millisecond):
		}
	}
	s.release()
	if err := <-admitted; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []CloneProgress{CloneQueued, CloneDequeued, CloneAdmitted}
	if !slices.Equal(got, want) {
		t.Errorf("progress = %v, want %v", got, want)
	}
}

func TestCloneSlotsAbandonedWaiterFreesItsPlace(t *testing.T) {
	s := NewCloneSlots().server(serverKey{host: "h"})
	ctx := context.Background()
	if err := s.acquire(ctx, 1, nil); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithCancel(ctx)
	var dequeued atomic.Bool
	done := make(chan error, 1)
	go func() {
		done <- s.acquire(waitCtx, 1, func(p CloneProgress) {
			if p == CloneDequeued {
				dequeued.Store(true)
			}
		})
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled wait must fail with the cancellation, got %v", err)
	}
	if !dequeued.Load() {
		t.Error("a cancelled wait must report CloneDequeued")
	}
	s.release()
	nextCtx, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if err := s.acquire(nextCtx, 1, nil); err != nil {
		t.Fatalf("the abandoned waiter must not keep the slot: %v", err)
	}
}
