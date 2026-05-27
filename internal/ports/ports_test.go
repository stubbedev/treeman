package ports

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/store"
)

func openStore(t *testing.T) (*store.Store, int64, int64) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	repoID, err := st.EnsureRepo(ctx, "/repos/test", "test")
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := st.EnsureWorktree(ctx, repoID, "/repos/test/.worktrees/x", "x", "feature/x")
	if err != nil {
		t.Fatal(err)
	}
	return st, repoID, wtID
}

// pickFreePort asks the kernel for a usable port, then closes the
// listener so the port returns to the free pool. Used to seed test
// ranges with values that won't fight a real service for the bind.
func pickFreePort(t *testing.T) uint16 {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, p, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return uint16(n)
}

func TestAllocateAssignsPortsInOrder(t *testing.T) {
	ctx := context.Background()
	st, repoID, wtID := openStore(t)
	a := pickFreePort(t)
	b := pickFreePort(t)
	cfg := &config.Config{
		Ports: map[string]config.PortSpec{
			"alpha": {Range: config.PortRange{Min: a, Max: a}},
			"beta":  {Range: config.PortRange{Min: b, Max: b}},
		},
	}
	allocs, err := New().Allocate(ctx, st, cfg, repoID, wtID)
	if err != nil {
		t.Fatal(err)
	}
	if len(allocs) != 2 {
		t.Fatalf("expected 2 allocs, got %d", len(allocs))
	}
	if allocs[0].Name != "alpha" || allocs[1].Name != "beta" {
		t.Errorf("expected alphabetical order, got %+v", allocs)
	}
	if allocs[0].Port != a || allocs[1].Port != b {
		t.Errorf("unexpected port assignment: %+v (want alpha=%d beta=%d)", allocs, a, b)
	}
}

func TestAllocateSkipsHeldPortAndPicksNext(t *testing.T) {
	ctx := context.Background()
	st, repoID, wtID := openStore(t)

	// Bind a port for the lifetime of the test so the listen probe
	// rejects it; the allocator should fall through to the next slot.
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, p, _ := net.SplitHostPort(l.Addr().String())
	pn, _ := strconv.Atoi(p)
	held := uint16(pn)
	next := pickFreePort(t)
	cfg := &config.Config{
		Ports: map[string]config.PortSpec{
			"octane": {Range: config.PortRange{Min: held, Max: max16(held, next)}},
		},
	}
	if next <= held {
		t.Skipf("test prerequisite: free probe (%d) must be above held (%d)", next, held)
	}
	allocs, err := New().Allocate(ctx, st, cfg, repoID, wtID)
	if err != nil {
		t.Fatal(err)
	}
	if len(allocs) != 1 || allocs[0].Port == held {
		t.Fatalf("expected allocator to skip held port %d, got %+v", held, allocs)
	}
}

func TestAllocateExhaustsRange(t *testing.T) {
	ctx := context.Background()
	st, repoID, wtID := openStore(t)

	// Pick a free port, then bind it for the duration so no port in
	// the single-port range is bindable. allocateOne should report
	// ErrExhausted.
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, p, _ := net.SplitHostPort(l.Addr().String())
	pn, _ := strconv.Atoi(p)
	port := uint16(pn)
	cfg := &config.Config{
		Ports: map[string]config.PortSpec{
			"only": {Range: config.PortRange{Min: port, Max: port}},
		},
	}
	_, err = New().Allocate(ctx, st, cfg, repoID, wtID)
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("want ErrExhausted, got %v", err)
	}
}

// TestAllocateReleasesEarlierSlotsOnLaterFailure locks the cleanup
// contract: when allocation succeeds for one slot but a later slot
// exhausts its range, the earlier slot's row MUST be released so the
// worktree doesn't end up half-populated. Without this rollback, a
// `wt create` that fails midway leaves stale rows that permanently
// shrink the pool of allocatable ports.
func TestAllocateReleasesEarlierSlotsOnLaterFailure(t *testing.T) {
	ctx := context.Background()
	st, repoID, wtID := openStore(t)

	// alpha: a single free port — succeeds.
	// beta:  a single port we hold for the test duration — exhausts.
	a := pickFreePort(t)
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, p, _ := net.SplitHostPort(l.Addr().String())
	pn, _ := strconv.Atoi(p)
	held := uint16(pn)
	if held == a {
		t.Skipf("test prerequisite: free port (%d) collided with held port (%d)", a, held)
	}

	cfg := &config.Config{
		Ports: map[string]config.PortSpec{
			"alpha": {Range: config.PortRange{Min: a, Max: a}},
			"beta":  {Range: config.PortRange{Min: held, Max: held}},
		},
	}
	if _, err := New().Allocate(ctx, st, cfg, repoID, wtID); !errors.Is(err, ErrExhausted) {
		t.Fatalf("want ErrExhausted from beta, got %v", err)
	}

	// alpha's row must NOT remain — otherwise the next retry leaks a
	// permanently-allocated port.
	leftover, err := st.LoadWorktreePorts(ctx, wtID)
	if err != nil {
		t.Fatal(err)
	}
	if len(leftover) != 0 {
		t.Fatalf("mid-allocation failure must release earlier slots; leftover=%v", leftover)
	}
}

func max16(a, b uint16) uint16 {
	if a > b {
		return a
	}
	return b
}
