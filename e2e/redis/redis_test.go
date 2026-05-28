//go:build e2e

package redis_e2e

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/prepare"
)

func TestRedisEndToEnd(t *testing.T) {
	harness.SkipIfNoDocker(t)
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))

	harness.WaitForReady(t, "redis:16379", 30*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:16379", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	wt := t.TempDir()
	copyTree(t, "fixtures", filepath.Join(wt, "fixtures"))
	cfg := buildConfig()
	env := harness.NewEnv(t, wt)

	outs := env.RunPrepare(t, cfg)
	o1 := harness.AssertOutcome(t, outs, "redis", false)
	t.Logf("pass1: sourcePrefix=%s template=%s", o1.SourceDB, o1.TemplateName)
	assertKeyCount(t, o1.SourceDB, 3)

	outs = env.RunPrepare(t, cfg)
	o2 := harness.AssertOutcome(t, outs, "redis", true)
	if o2.Fingerprint != o1.Fingerprint {
		t.Errorf("fingerprint drift: %s vs %s", o1.Fingerprint, o2.Fingerprint)
	}

	if err := os.WriteFile(filepath.Join(wt, "fixtures/schema.txt"),
		[]byte("redis keyset v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outs = env.RunPrepare(t, cfg)
	o3 := harness.AssertOutcome(t, outs, "redis", false)
	if o3.Fingerprint == o1.Fingerprint {
		t.Errorf("fingerprint unchanged after schema.txt edit")
	}

	// ── teardown: `wt delete` drops the per-worktree keys, keeps the cache ──
	// TeardownDatabases is the DB layer of `treeman wt delete`: for redis it
	// DROPs every key under the worktree's prefix while leaving the
	// fingerprint-keyed template prefix intact, so the next prepare with the
	// same inputs is a cache hit.
	if err := prepare.TeardownDatabases(env.Ctx, cfg, env.Slug.Value, env.RepoID, env.WTID, env.Store); err != nil {
		t.Fatalf("TeardownDatabases: %v", err)
	}
	assertKeyCount(t, o3.SourceDB, 0) // per-worktree keys dropped
	// The fingerprint-keyed template prefix must SURVIVE teardown so the
	// next worktree with the same inputs still hits the cache.
	assertKeyCount(t, o3.TemplateName, 3)

	// ── fanout + teardown permutation ──
	// Pre-warm clones (paratest workers). Clone prefixes nest under the
	// source prefix, so the single source-prefix DropPrefix on teardown
	// reaps source + clones together. A distinct slug (fresh worktree)
	// keeps these prefixes from overlapping the assertions above.
	t.Run("fanout_teardown", func(t *testing.T) {
		wt2 := t.TempDir()
		copyTree(t, "fixtures", filepath.Join(wt2, "fixtures"))
		fcfg := buildConfig()
		fcfg.Databases[0].KeyPrefix = "wtfan:{slug}:"
		fcfg.Databases[0].TestClones = &config.TestClonesSpec{
			Clones:       config.ClonesSetting{Fixed: 2},
			NameTemplate: "wtfan:{slug}:w{n}:",
		}
		env2 := harness.NewEnv(t, wt2)
		fo := harness.AssertOutcome(t, env2.RunPrepare(t, fcfg), "redis", false)
		if len(fo.Clones) != 2 {
			t.Fatalf("fanout: got %d clones, want 2 (%v)", len(fo.Clones), fo.Clones)
		}
		for _, c := range fo.Clones {
			assertKeyCount(t, c, 3) // each clone pre-warmed from the template
		}

		if err := prepare.TeardownDatabases(env2.Ctx, fcfg, env2.Slug.Value, env2.RepoID, env2.WTID, env2.Store); err != nil {
			t.Fatalf("TeardownDatabases: %v", err)
		}
		assertKeyCount(t, fo.SourceDB, 0) // source + nested clones all dropped
		for _, c := range fo.Clones {
			assertKeyCount(t, c, 0)
		}
		assertKeyCount(t, fo.TemplateName, 3) // cache survives
	})
}

func buildConfig() *config.Config {
	return &config.Config{
		Connections: config.ConnectionsConfig{
			Redis: &config.RedisConn{URL: "redis://127.0.0.1:16379"},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "redis",
				NameTemplate: "treeman_e2e_{slug}",
				KeyPrefix:    "wte2e:{slug}:",
				Seed: &config.Step{
					Run: "./fixtures/seed.sh",
					Env: map[string]string{
						"REDIS_PREFIX":    "{target_db}",
						"REDIS_CONTAINER": "treeman-e2e-redis",
					},
				},
				Inputs: []config.Input{
					{Glob: "fixtures/schema.txt", Label: "keyset"},
				},
			},
		},
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

func assertKeyCount(t *testing.T, prefix string, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:16379"})
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
