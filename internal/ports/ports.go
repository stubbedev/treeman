// Package ports allocates per-worktree TCP-port assignments declared
// in the top-level `ports:` block of `.treeman.yaml`.
//
// Allocation is two-phase per slot:
//
//  1. Skip every port already recorded in `worktree_ports` for a live
//     worktree in the same repo + slot (DB-level reservation).
//  2. Probe the remaining candidates by attempting a brief TCP listen
//     on 127.0.0.1 — only ports that no other process currently holds
//     are returned (OS-level liveness check).
//
// The DB step prevents allocation collisions across worktrees the
// daemon manages; the listen probe also rejects ports that a
// non-treeman process happens to be using. The latter is a best-
// effort safety net — between the probe and the actual app server
// binding the port, a third process could swoop in. The allocator
// reports success once the row is written; a subsequent bind failure
// is the user's to retry.
//
// Probe time is bounded by ProbeTimeout (default 100 ms) so a single
// stuck firewall rule can't stall `wt create` indefinitely.
package ports

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/store"
)

// ProbeTimeout caps the per-port TCP-listen probe. 100 ms is plenty
// on a healthy local stack; longer waits indicate the kernel is
// refusing the bind (firewall, SELinux, port < 1024 without privs)
// which we treat as "this port is unusable" rather than the more
// optimistic "give it more time".
var ProbeTimeout = 100 * time.Millisecond

// Allocation pairs a slot name with the port treeman picked for it.
type Allocation struct {
	Name string
	Port uint16
}

// Allocator owns the policy for choosing ports. The default
// implementation walks each slot's range sequentially; callers can
// substitute a deterministic-test allocator (or a hash-based one)
// without re-implementing the SQLite plumbing.
type Allocator interface {
	// Allocate returns one port per declared slot, in slot-name
	// alphabetical order. Each returned port is reserved in the
	// `worktree_ports` table against (repoID, worktreeID, name).
	// On any per-slot failure, every port allocated earlier in the
	// same call is released so the worktree doesn't end up with a
	// half-populated port map.
	Allocate(ctx context.Context, st *store.Store, cfg *config.Config, repoID, worktreeID int64) ([]Allocation, error)
}

// New returns the default sequential-scan Allocator.
func New() Allocator { return defaultAllocator{} }

type defaultAllocator struct{}

// Allocate walks every declared port slot in alphabetical order and
// reserves a free port per slot.
func (defaultAllocator) Allocate(
	ctx context.Context,
	st *store.Store,
	cfg *config.Config,
	repoID, worktreeID int64,
) ([]Allocation, error) {
	slots := cfg.PortSlotNames()
	if len(slots) == 0 {
		return nil, nil
	}
	out := make([]Allocation, 0, len(slots))
	for _, name := range slots {
		spec := cfg.Ports[name]
		port, err := allocateOne(ctx, st, repoID, worktreeID, name, spec)
		if err != nil {
			// Best-effort cleanup so a mid-allocation failure doesn't
			// leak rows.
			_ = st.ReleaseWorktreePorts(ctx, worktreeID)
			return nil, fmt.Errorf("allocate port %q: %w", name, err)
		}
		out = append(out, Allocation{Name: name, Port: port})
	}
	return out, nil
}

// allocateOne reserves one free port for (repoID, worktreeID, name)
// from spec.Range. Returns ErrExhausted when every port in the range
// is taken by another live worktree or by a foreign process.
func allocateOne(
	ctx context.Context,
	st *store.Store,
	repoID, worktreeID int64,
	name string,
	spec config.PortSpec,
) (uint16, error) {
	used, err := st.ListUsedPorts(ctx, repoID, name)
	if err != nil {
		return 0, err
	}
	// Iterate with an int so candidate == 65535 + 1 doesn't wrap a
	// uint16 back to 0 and loop forever.
	for p := int(spec.Range.Min); p <= int(spec.Range.Max); p++ {
		candidate := uint16(p)
		if _, taken := used[candidate]; taken {
			continue
		}
		if !portFree(candidate) {
			continue
		}
		switch err := st.AllocateWorktreePort(ctx, repoID, worktreeID, name, candidate); {
		case err == nil:
			return candidate, nil
		case errors.Is(err, store.ErrPortInUse):
			// Race: another worktree create grabbed this port between
			// ListUsedPorts and AllocateWorktreePort. Try the next.
			continue
		default:
			return 0, err
		}
	}
	return 0, fmt.Errorf("%w: slot %q range [%d, %d]", ErrExhausted, name, spec.Range.Min, spec.Range.Max)
}

// ErrExhausted reports that no free port remains in a slot's range.
var ErrExhausted = errors.New("port range exhausted")

// portFree reports whether 127.0.0.1:port is currently bindable.
// Uses a single tcp4 listen attempt + immediate close so the probe
// is brief (<= ProbeTimeout under a sane kernel).
func portFree(port uint16) bool {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	deadline := time.Now().Add(ProbeTimeout)
	for {
		l, err := net.Listen("tcp4", addr)
		if err == nil {
			_ = l.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		// Brief backoff so a transient TIME_WAIT clears.
		time.Sleep(5 * time.Millisecond)
	}
}

// FormatSummary returns a single-line "ports: name=port name=port"
// summary, ordered by slot name. Empty string when no allocations.
// Used by `wt create` stdout + `wt show`.
func FormatSummary(allocs []Allocation) string {
	if len(allocs) == 0 {
		return ""
	}
	out := "ports:"
	for _, a := range allocs {
		out += " " + a.Name + "=" + strconv.Itoa(int(a.Port))
	}
	return out
}
