//go:build e2e

// Package redisvariants_e2e proves the `valkey` and `dragonfly` engine
// aliases drive the redis-family code path end-to-end against the real
// servers — not just the unit-level Canonical() mapping. Both alias
// strings canonicalise to engine.FamilyRedis, so the same prepare →
// cache-hit → teardown lifecycle the redis suite exercises must hold
// when the configured engine is `valkey` or `dragonfly` and the server
// on the wire is Valkey / DragonflyDB rather than Redis.
package redisvariants_e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/prepare"
)

// variant pins one alias to its host port + seeding strategy. Valkey
// seeds in-container (it ships redis-cli); DragonflyDB has no CLI, so
// it's seeded from the valkey container over the compose network.
type variant struct {
	engine        string // the `engine:` config value under test
	addr          string // host-mapped RESP endpoint
	execContainer string // container holding a redis-cli for the seed
	targetHost    string // compose-network host for the seed (-h); "" = local
}

func variants() []variant {
	return []variant{
		{engine: "valkey", addr: "127.0.0.1:16380", execContainer: "treeman-e2e-valkey"},
		{engine: "dragonfly", addr: "127.0.0.1:16381", execContainer: "treeman-e2e-valkey", targetHost: "dragonfly"},
	}
}

func TestRedisVariantsEndToEnd(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))

	for _, v := range variants() {
		t.Run(v.engine, func(t *testing.T) {
			// Gate on a real RESP round-trip, not just an open port —
			// DragonflyDB accepts the TCP connection before it's ready
			// to serve commands.
			harness.WaitForReady(t, v.engine+" "+v.addr, 60*time.Second, func() error {
				return pingRESP(v.addr)
			})

			wt := t.TempDir()
			copyTree(t, "fixtures", filepath.Join(wt, "fixtures"))
			cfg := buildConfig(v)
			env := harness.NewEnv(t, wt)

			// Pass 1: cold build seeds the source prefix.
			o1 := harness.AssertOutcome(t, env.RunPrepare(t, cfg), v.engine, false)
			t.Logf("pass1: source=%s template=%s", o1.SourceDB, o1.TemplateName)
			assertKeyCount(t, v.addr, o1.SourceDB, 3)

			// Pass 2: identical inputs → cache hit, stable fingerprint.
			o2 := harness.AssertOutcome(t, env.RunPrepare(t, cfg), v.engine, true)
			if o2.Fingerprint != o1.Fingerprint {
				t.Errorf("fingerprint drift: %s vs %s", o1.Fingerprint, o2.Fingerprint)
			}

			// Editing an input invalidates the cache → cold rebuild.
			if err := os.WriteFile(filepath.Join(wt, "fixtures/schema.txt"),
				[]byte("redis keyset v2\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			o3 := harness.AssertOutcome(t, env.RunPrepare(t, cfg), v.engine, false)
			if o3.Fingerprint == o1.Fingerprint {
				t.Errorf("fingerprint unchanged after schema.txt edit")
			}

			// Teardown drops the per-worktree keys but keeps the
			// fingerprint-keyed template so the next prepare is a hit.
			if err := prepare.TeardownDatabases(env.Ctx, cfg, env.Slug.Value, env.RepoID, env.WTID, env.Store); err != nil {
				t.Fatalf("TeardownDatabases: %v", err)
			}
			assertKeyCount(t, v.addr, o3.SourceDB, 0)
			assertKeyCount(t, v.addr, o3.TemplateName, 3)
		})
	}
}

func buildConfig(v variant) *config.Config {
	seedEnv := map[string]string{
		"REDIS_PREFIX":   "{target_db}",
		"EXEC_CONTAINER": v.execContainer,
	}
	if v.targetHost != "" {
		seedEnv["TARGET_HOST"] = v.targetHost
	}
	return &config.Config{
		Connections: config.ConnectionsConfig{
			Redis: &config.RedisConn{URL: "redis://" + v.addr},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       v.engine,
				NameTemplate: "treeman_e2e_{slug}",
				KeyPrefix:    "wtvar:{slug}:",
				Seed: &config.Step{
					Run: "./fixtures/seed.sh",
					Env: seedEnv,
				},
				Inputs: []config.Input{
					{Glob: "fixtures/schema.txt", Label: "keyset"},
				},
			},
		},
	}
}

func pingRESP(addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c := redis.NewClient(&redis.Options{Addr: addr})
	defer c.Close()
	return c.Ping(ctx).Err()
}

func assertKeyCount(t *testing.T, addr, prefix string, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := redis.NewClient(&redis.Options{Addr: addr})
	defer c.Close()
	var (
		cursor uint64
		count  int
	)
	for {
		keys, next, err := c.Scan(ctx, cursor, prefix+"*", 100).Result()
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		count += len(keys)
		cursor = next
		if cursor == 0 {
			break
		}
	}
	if count != want {
		t.Errorf("key count under %s = %d, want %d", prefix, count, want)
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			copyTree(t, s, d)
			continue
		}
		body, _ := os.ReadFile(s)
		info, _ := e.Info()
		mode := os.FileMode(0o644)
		if info != nil {
			mode = info.Mode().Perm()
		}
		_ = os.WriteFile(d, body, mode)
	}
}
