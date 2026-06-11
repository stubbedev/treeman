//go:build e2e

package postgres_e2e

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/snapshot"
	"github.com/stubbedev/treeman/internal/store"
)

// TestPrewarmPoolClaimReplenishAndPurge exercises `databases[].prewarm`
// end-to-end against a real Postgres:
//
//  1. cold build with prewarm: 2 → the detached replenisher creates
//     `<template>_spare1` + `_spare2`
//  2. worktree teardown drops source + clones but leaves the template
//     AND its spares (they belong to the cache, not the worktree)
//  3. the next prepare is a cache hit that CLAIMS spares via rename —
//     observable as snapshots:prewarm:claim events — and the pool is
//     replenished afterwards
//  4. snapshot purge reaps the spare family alongside the template so
//     no anonymous spare DBs survive their template
func TestPrewarmPoolClaimReplenishAndPurge(t *testing.T) {
	harness.SkipIfNoDocker(t)
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))

	harness.WaitForReady(t, "postgres:15432", 60*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:15432", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	wt := t.TempDir()
	copyTree(t, "fixtures", filepath.Join(wt, "fixtures"))
	cfg := buildConfig()
	cfg.Databases[0].Prewarm = 2
	env := harness.NewEnv(t, wt)

	// ── pass 1: cold build, then the detached replenisher fills the pool ──
	outs := env.RunPrepare(t, cfg)
	o1 := harness.AssertOutcome(t, outs, "postgres", false)
	spare1 := snapshot.SpareName(o1.TemplateName, 1)
	spare2 := snapshot.SpareName(o1.TemplateName, 2)
	waitForSpares(t, spare1, spare2)

	// ── teardown: spares survive with the template ──
	if err := prepare.TeardownDatabases(env.Ctx, cfg, env.Slug.Value, env.RepoID, env.WTID, env.Store); err != nil {
		t.Fatalf("TeardownDatabases: %v", err)
	}
	if pgDBExists(t, "127.0.0.1:15432", o1.SourceDB) {
		t.Errorf("source DB %s still exists after teardown", o1.SourceDB)
	}
	for _, s := range []string{spare1, spare2} {
		if !pgDBExists(t, "127.0.0.1:15432", s) {
			t.Errorf("spare %s was dropped by worktree teardown (must survive with the template)", s)
		}
	}

	// ── pass 2: cache hit claims spares instead of full restores ──
	outs = env.RunPrepare(t, cfg)
	o2 := harness.AssertOutcome(t, outs, "postgres", true)
	if o2.Fingerprint != o1.Fingerprint {
		t.Fatalf("fingerprint drift: %s vs %s", o1.Fingerprint, o2.Fingerprint)
	}
	if !pgDBExists(t, "127.0.0.1:15432", o2.SourceDB) {
		t.Fatalf("source DB %s missing after cache-hit prepare", o2.SourceDB)
	}
	assertTables(t, "127.0.0.1:15432", o2.SourceDB, []string{"products", "orders"})
	claims, err := env.Store.QueryEvents(env.Ctx, store.EventFilter{
		RepoID:     env.RepoID,
		EventTypes: []string{store.EvtSnapshotsPrewarmClaim},
	})
	if err != nil {
		t.Fatalf("query claim events: %v", err)
	}
	// Pool of 2 vs three restores (source + 2 clones): both spares must
	// have been claimed, the third restore fell back to a full clone.
	if len(claims) != 2 {
		t.Errorf("prewarm claims = %d, want 2 (pool size)", len(claims))
	}
	// Replenisher tops the pool back up after the claims.
	waitForSpares(t, spare1, spare2)

	// ── purge: spare family dies with the template ──
	if dropped, errs := snapshot.PurgeRepo(env.Ctx, cfg, env.Store, env.RepoID); len(errs) > 0 {
		t.Fatalf("PurgeRepo (dropped=%d): %v", dropped, errs)
	}
	for _, name := range []string{o1.TemplateName, spare1, spare2} {
		if pgDBExists(t, "127.0.0.1:15432", name) {
			t.Errorf("%s still exists after purge", name)
		}
	}
}

// waitForSpares polls pg_database until every named spare exists —
// the replenisher is a detached goroutine, so the pool fills shortly
// after RunPrepare returns, not synchronously.
func waitForSpares(t *testing.T, names ...string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		all := true
		for _, n := range names {
			if !pgDBExists(t, "127.0.0.1:15432", n) {
				all = false
				break
			}
		}
		if all {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("spares %v not pre-warmed within 60s", names)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
