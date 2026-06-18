//go:build e2e

// Package matrixredis_e2e fills the redis cells of the option×engine
// matrix: compose_service resolution, test_clones:auto detection, and
// password $ENV-ref resolution (bare URL scalar form). Auth-enabled via
// requirepass so the password-ref test is meaningful — redis errors on
// AUTH when no password is set, so a successful prepare proves the
// resolved password was used.
package matrixredis_e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	dbredis "github.com/stubbedev/treeman/internal/db/redis"
	"github.com/stubbedev/treeman/internal/migrations/testfw"
	"github.com/stubbedev/treeman/internal/resolve"
)

const authURL = "redis://:redispw@127.0.0.1:16385"

func up(t *testing.T) {
	t.Helper()
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "redis:16385", 60*time.Second, func() error {
		out, err := exec.Command("docker", "exec", "treeman-e2e-mxredis",
			"redis-cli", "-a", "redispw", "ping").CombinedOutput()
		if err != nil || !strings.Contains(string(out), "PONG") {
			return fmt.Errorf("redis not ready: %s", out)
		}
		return nil
	})
}

func TestRedisComposeService(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)
	project := composeProject(t, "treeman-e2e-mxredis")

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Redis: &config.RedisConn{
				URL:          "redis://:redispw@placeholder:6379",
				ContainerRef: config.ContainerRef{ComposeService: "redis", ComposeProject: project},
			},
		},
		Databases: []config.DatabaseConfig{{Engine: "redis", KeyPrefix: "mxr_svc_{slug}:"}},
	}
	harness.AssertOutcome(t, harness.NewEnv(t, t.TempDir()).RunPrepare(t, cfg), "redis", false)
}

func TestRedisClonesAuto(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)

	wt := t.TempDir()
	write(t, wt, "package.json", `{"devDependencies":{"jest":"^29"}}`)
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{Redis: &config.RedisConn{URL: authURL}},
		Databases: []config.DatabaseConfig{{
			Engine: "redis", KeyPrefix: "mxr_ca_{slug}:",
			TestClones: &config.TestClonesSpec{Clones: config.ClonesSetting{Auto: true}, NameTemplate: "mxr_ca_{slug}:w{n}:"},
		}},
	}
	o := harness.AssertOutcome(t, harness.NewEnv(t, wt).RunPrepare(t, cfg), "redis", false)
	want := int(testfw.DetectedCloneCount(wt))
	if want == 0 {
		want = int(testfw.NumCPUs())
	}
	if len(o.Clones) != want {
		t.Fatalf("clones:auto produced %d clones, want %d (detector)", len(o.Clones), want)
	}
}

// TestRedisConnFromEnvFile covers redis' secret-from-env path: the YAML
// omits the redis connection and the resolver derives it from REDIS_URL
// in the env file (carrying the requirepass credential) → prepare auths.
func TestRedisConnFromEnvFile(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	wt := t.TempDir()
	write(t, wt, ".env", "REDIS_URL=redis://:redispw@127.0.0.1:16385\n")
	write(t, wt, ".treeman.yaml", `env_sources: [.env]
databases:
  - engine: redis
    key_prefix: "mxr_env_{slug}:"
`)
	cfg, err := resolve.LoadResolved(wt)
	if err != nil {
		t.Fatalf("LoadResolved: %v", err)
	}
	if cfg.Connections.Redis == nil || !strings.Contains(cfg.Connections.Redis.URL, "redispw") {
		t.Fatalf("redis connection not derived from REDIS_URL env: %+v", cfg.Connections.Redis)
	}
	harness.AssertOutcome(t, harness.NewEnv(t, wt).RunPrepare(t, &cfg), "redis", false)
}

// TestRedisMigrateAndSeedRun proves migrate AND seed run for redis
// (migrate was previously mysql/postgres-only and silently ignored here).
func TestRedisMigrateAndSeedRun(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)

	wt := t.TempDir()
	mwit := filepath.Join(wt, "migrate.out")
	swit := filepath.Join(wt, "seed.out")
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{Redis: &config.RedisConn{URL: authURL}},
		Databases: []config.DatabaseConfig{{
			Engine: "redis", KeyPrefix: "mxr_ms_{slug}:",
			Migrate: &config.Step{Run: `echo "$TREEMAN_TARGET_DB" > ` + mwit},
			Seed:    &config.Step{Run: `echo "$TREEMAN_TARGET_DB" > ` + swit},
		}},
	}
	o := harness.AssertOutcome(t, harness.NewEnv(t, wt).RunPrepare(t, cfg), "redis", false)
	assertFileHas(t, mwit, o.SourceDB)
	assertFileHas(t, swit, o.SourceDB)
}

// TestRedisPoolMaxCaps proves pool_max actually limits concurrency: with
// PoolSize=2, eight blocking BLPOPs (each ~400ms) can only run 2 at a
// time → ~4 serial batches ≈ 1.6s. Uncapped they'd all run at once
// (~0.4s). Timing is machine-independent: the floor is set by redis-side
// block timeouts, not client speed.
func TestRedisPoolMaxCaps(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)

	const poolMax = 2
	const ops = 8
	const block = 400 * time.Millisecond

	drv, err := dbredis.Connect(context.Background(), config.RedisConn{URL: authURL, PoolMax: poolMax})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer drv.Close()
	cl := drv.Client()

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < ops; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			// No value is ever pushed → each BLPOP holds its pooled
			// connection for the full block timeout.
			_, _ = cl.BLPop(ctx, block, fmt.Sprintf("mxr_poolcap_%d", i)).Result()
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	// 8 ops / pool 2 = 4 batches × 400ms ≈ 1.6s. Uncapped ≈ 0.4s.
	if elapsed < time.Second {
		t.Errorf("pool_max=2 did not cap concurrency: %d×%s BLPOPs finished in %s (want ≥1s — they ran in parallel)", ops, block, elapsed)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────

func assertFileHas(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("step did not run (no witness %s): %v", filepath.Base(path), err)
	}
	if !strings.Contains(string(b), want) {
		t.Errorf("%s = %q, want it to contain target_db %q", filepath.Base(path), b, want)
	}
}

func composeProject(t *testing.T, container string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "--format",
		`{{index .Config.Labels "com.docker.compose.project"}}`, container).CombinedOutput()
	if err != nil {
		t.Fatalf("read compose project: %v\n%s", err, out)
	}
	p := strings.TrimSpace(string(out))
	if p == "" {
		t.Fatal("empty compose project label")
	}
	return p
}

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
