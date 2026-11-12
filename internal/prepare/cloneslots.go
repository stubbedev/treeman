package prepare

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sync"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/engine"
)

// CloneSlots caps the clone restores running at once against each
// engine server across every prepare that shares it. The per-prepare
// fan-out limit is derived from the server's connection budget, which
// every worktree draws on: eight concurrent creates with a limit of
// eight each ran 72 restores against one mysqld (#199). A restore waits
// in FIFO order until fewer restores than its own limit are running on
// its server.
//
// The daemon owns one CloneSlots and hands it to every prepare it runs
// through RunOptions. A prepare without one (inline `finalize --local`,
// the MCP server) is bounded by its own fan-out limit only: it runs in
// another process, so no in-process cap could coordinate with the
// daemon's restores anyway.
type CloneSlots struct {
	mu      sync.Mutex
	servers map[serverKey]*serverSlots
}

// NewCloneSlots returns an empty CloneSlots.
func NewCloneSlots() *CloneSlots {
	return &CloneSlots{servers: map[serverKey]*serverSlots{}}
}

// serverKey identifies one engine server as a connection block
// addresses it. Two spellings of the same server (localhost vs
// 127.0.0.1) count as two servers.
type serverKey struct {
	family                                    engine.Family
	host                                      string
	port                                      uint16
	container, composeService, composeProject string
}

func mysqlServerKey(c config.MysqlConn) serverKey {
	return serverKey{
		family: engine.FamilyMySQL, host: c.Host, port: c.Port,
		container: c.Container, composeService: c.ComposeService, composeProject: c.ComposeProject,
	}
}

func postgresServerKey(c config.PostgresConn) serverKey {
	return serverKey{
		family: engine.FamilyPostgres, host: c.Host, port: c.Port,
		container: c.Container, composeService: c.ComposeService, composeProject: c.ComposeProject,
	}
}

// server returns the slots of the server key names; nil when c is nil.
func (c *CloneSlots) server(key serverKey) *serverSlots {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.servers[key]
	if s == nil {
		s = &serverSlots{}
		c.servers[key] = s
	}
	return s
}

// CloneProgress is one step of a clone restore's way through the
// server slots, reported through RunOptions.Progress.
type CloneProgress int

const (
	// CloneQueued: the restore waits for a slot on its server.
	CloneQueued CloneProgress = iota + 1
	// CloneDequeued: a queued restore left the queue, admitted or
	// abandoned because its context ended. Pairs with CloneQueued.
	CloneDequeued
	// CloneAdmitted: the restore got its slot and starts.
	CloneAdmitted
	// CloneFinished: an admitted restore returned, ok or not.
	CloneFinished
)

// ProgressFunc receives the clone progress of a prepare. A nil
// ProgressFunc discards it.
type ProgressFunc func(CloneProgress)

func (f ProgressFunc) report(p CloneProgress) {
	if f != nil {
		f(p)
	}
}

// serverSlots is the FIFO gate of one server.
type serverSlots struct {
	mu      sync.Mutex
	active  int
	waiters []*slotWaiter
}

type slotWaiter struct {
	limit int
	ready chan struct{}
}

// acquire blocks until fewer than limit restores run on the server and
// every earlier waiter was admitted, or ctx ends. A nil receiver admits
// at once.
func (s *serverSlots) acquire(ctx context.Context, limit int, progress ProgressFunc) error {
	if s == nil {
		progress.report(CloneAdmitted)
		return nil
	}
	s.mu.Lock()
	if len(s.waiters) == 0 && s.active < limit {
		s.active++
		s.mu.Unlock()
		progress.report(CloneAdmitted)
		return nil
	}
	w := &slotWaiter{limit: limit, ready: make(chan struct{})}
	s.waiters = append(s.waiters, w)
	s.mu.Unlock()
	progress.report(CloneQueued)

	select {
	case <-w.ready:
		progress.report(CloneDequeued)
		progress.report(CloneAdmitted)
		return nil
	case <-ctx.Done():
	}
	s.mu.Lock()
	if i := slices.Index(s.waiters, w); i >= 0 {
		s.waiters = slices.Delete(s.waiters, i, i+1)
		s.grantLocked()
		s.mu.Unlock()
	} else {
		s.mu.Unlock()
		s.release()
	}
	progress.report(CloneDequeued)
	return fmt.Errorf("wait for a clone slot: %w", context.Cause(ctx))
}

// release frees one slot and admits the waiters it unblocks. A nil
// receiver does nothing.
func (s *serverSlots) release() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active--
	s.grantLocked()
}

func (s *serverSlots) grantLocked() {
	for len(s.waiters) > 0 && s.active < s.waiters[0].limit {
		w := s.waiters[0]
		s.waiters = s.waiters[1:]
		s.active++
		close(w.ready)
	}
}

// fanoutBudget is what bounds a clone fan-out against one engine
// server: the server's probed connection ceiling (0 when unknown or
// not applicable), the slots it shares with every other prepare
// targeting it (nil: no cross-prepare cap), and where clone progress
// is reported.
type fanoutBudget struct {
	maxConns int
	server   *serverSlots
	progress ProgressFunc
}

// serverBudget is the fan-out budget against the server key names.
func (o RunOptions) serverBudget(maxConns int, key serverKey) fanoutBudget {
	return fanoutBudget{maxConns: maxConns, server: o.CloneSlots.server(key), progress: o.Progress}
}

// localBudget is the fan-out budget of an engine whose restores are
// bounded by the per-prepare limit only.
func (o RunOptions) localBudget() fanoutBudget {
	return fanoutBudget{progress: o.Progress}
}

// runRestore runs one clone restore inside a server slot. The server's
// limit is the fan-out limit the database would get with unlimited
// clones, so a single prepare is never throttled below its own limit.
func (b fanoutBudget) runRestore(ctx context.Context, override uint32, eng string, restore func() error) error {
	serverLimit, _ := fanOutLimit(override, b.maxConns, math.MaxInt, eng)
	if err := b.server.acquire(ctx, serverLimit, b.progress); err != nil {
		return err
	}
	defer func() {
		b.server.release()
		b.progress.report(CloneFinished)
	}()
	return restore()
}
