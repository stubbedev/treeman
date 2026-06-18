//go:build e2e

package redis_e2e

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/store"
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

// TestIncrementalAncestorBuild proves task #4 wired to redis: adding a
// NEW file under an `inputs:` glob (an extending vector, never an edit)
// makes the next prep build from the cached ancestor template — clone
// every seeded key via COPY (or DUMP+RESTORE on Redis <6.2), skip seed,
// just register the new snapshot — instead of dropping the source
// prefix and re-running the seed script.
func TestIncrementalAncestorBuild(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
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
	extrasDir := filepath.Join(wt, "extras")
	if err := os.MkdirAll(extrasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extrasDir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := buildConfig()
	cfg.Databases[0].Inputs = append(cfg.Databases[0].Inputs, config.Input{
		Glob:  "extras/*.txt",
		Label: "extras",
	})

	env := harness.NewEnv(t, wt)
	o1 := harness.AssertOutcome(t, env.RunPrepare(t, cfg), "redis", false)
	if o1.IncrementalBase != "" {
		t.Fatalf("pass 1 should be cold; got base=%s", o1.IncrementalBase)
	}
	assertKeyCount(t, o1.SourceDB, 3)
	t.Logf("pass1 cold: fp=%s tmpl=%s", o1.Fingerprint[:12], o1.TemplateName)

	if err := os.WriteFile(filepath.Join(extrasDir, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	o2 := harness.AssertOutcome(t, env.RunPrepare(t, cfg), "redis", false)
	if o2.IncrementalBase != o1.Fingerprint {
		t.Errorf("pass 2 should build incrementally from pass 1; got IncrementalBase=%q want %q",
			o2.IncrementalBase, o1.Fingerprint)
	}
	if o2.Fingerprint == o1.Fingerprint {
		t.Errorf("pass 2 must produce a NEW fingerprint: still %s", o1.Fingerprint[:12])
	}
	t.Logf("pass2 incremental: fp=%s base=%s", o2.Fingerprint[:12], o2.IncrementalBase[:12])
	// Source prefix carries the seeded keys from the ancestor template
	// (the seed script did NOT re-run — we skipped it via incremental).
	assertKeyCount(t, o2.SourceDB, 3)
}

// TestRedisDumpLoadViaDockerExec proves the redis dump-load dispatcher
// picks the docker-exec fast path when ContainerRef is set: `docker
// exec -i CID redis-cli --pipe` from inside the container. Asserts
// strategy=docker-exec on the dump-load phase event AND that the keys
// landed.
func TestRedisDumpLoadViaDockerExec(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "redis:16379", 30*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:16379", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	wt := t.TempDir()
	resp := []byte("" +
		"*3\r\n$3\r\nSET\r\n$9\r\nwte2ex:k1\r\n$2\r\nv1\r\n" +
		"*3\r\n$3\r\nSET\r\n$9\r\nwte2ex:k2\r\n$2\r\nv2\r\n" +
		"*3\r\n$3\r\nSET\r\n$9\r\nwte2ex:k3\r\n$2\r\nv3\r\n",
	)
	dumpPath := filepath.Join(wt, "seed.resp")
	if err := os.WriteFile(dumpPath, resp, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Redis: &config.RedisConn{
				// In-container URL; containerip rewrites host to the
				// bridge IP; 6379 is what redis listens on inside.
				URL: "redis://127.0.0.1:6379",
				ContainerRef: config.ContainerRef{
					Container: "treeman-e2e-redis",
				},
			},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "redis",
			NameTemplate: "treeman_e2e_{slug}",
			KeyPrefix:    "wte2ex:",
			Dump:         config.DumpList{{Path: "seed.resp"}},
		}},
	}
	env := harness.NewEnv(t, wt)
	o := harness.AssertOutcome(t, env.RunPrepare(t, cfg), "redis", false)

	evs, err := env.Store.QueryEvents(env.Ctx, store.EventFilter{
		WorktreeID: env.WTID,
		EventTypes: []string{"prepare:phase"},
		Phases:     []string{"dump-load"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawDockerExec bool
	for _, e := range evs {
		t.Logf("phase event: %s", e.Message)
		if strings.Contains(e.Message, "strategy=docker-exec") {
			sawDockerExec = true
		}
	}
	if !sawDockerExec {
		t.Errorf("expected strategy=docker-exec — ContainerRef should have selected redis-cli-in-container")
	}
	assertKeyCount(t, o.SourceDB, 3)
}

// TestRedisDumpWireFallback proves the Go-native RESP parser + go-redis
// Pipeline fallback completes a cold build when redis-cli is NOT on
// PATH and ContainerRef isn't set. Hand-builds a small RESP-encoded
// dump file (the format `redis-cli --pipe` accepts) so we don't need
// the CLI even to GENERATE the fixture.
func TestRedisDumpWireFallback(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "redis:16379", 30*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:16379", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	wt := t.TempDir()
	// Hand-built RESP stream: 3 SET commands with LITERAL keys. RESP
	// is length-prefixed binary, so per-worktree templating happens at
	// dump-generation time — not at load (a byte-level substitution
	// would desync the `$<N>` headers). Hardcoding the prefix here
	// matches what a real user-supplied RESP dump would carry.
	resp := []byte("" +
		"*3\r\n$3\r\nSET\r\n$9\r\nwte2ew:k1\r\n$2\r\nv1\r\n" +
		"*3\r\n$3\r\nSET\r\n$9\r\nwte2ew:k2\r\n$2\r\nv2\r\n" +
		"*3\r\n$3\r\nSET\r\n$9\r\nwte2ew:k3\r\n$2\r\nv3\r\n",
	)
	dumpPath := filepath.Join(wt, "seed.resp")
	if err := os.WriteFile(dumpPath, resp, 0o644); err != nil {
		t.Fatal(err)
	}

	// Narrow PATH so the dispatcher's native-CLI + docker-exec paths
	// both skip (no redis-cli) and the wire fallback runs. docker still
	// needed for compose teardown.
	sandbox := t.TempDir()
	for _, bin := range []string{"docker", "sh"} {
		full, perr := exec.LookPath(bin)
		if perr != nil {
			continue
		}
		_ = os.Symlink(full, filepath.Join(sandbox, bin))
	}
	t.Setenv("PATH", sandbox)
	if _, err := exec.LookPath("redis-cli"); err == nil {
		t.Fatal("PATH sandbox leak: redis-cli still resolvable")
	}

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Redis: &config.RedisConn{URL: "redis://127.0.0.1:16379"},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "redis",
			NameTemplate: "treeman_e2e_{slug}",
			// Literal KeyPrefix (no {slug}) so the run's sourcePrefix
			// matches the keys baked into the RESP fixture exactly.
			KeyPrefix: "wte2ew:",
			Dump:      config.DumpList{{Path: "seed.resp"}},
		}},
	}
	env := harness.NewEnv(t, wt)
	o := harness.AssertOutcome(t, env.RunPrepare(t, cfg), "redis", false)
	t.Logf("wire fallback: source=%s template=%s", o.SourceDB, o.TemplateName)

	evs, err := env.Store.QueryEvents(env.Ctx, store.EventFilter{
		WorktreeID: env.WTID,
		EventTypes: []string{"prepare:phase"},
		Phases:     []string{"dump-load"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 {
		t.Fatal("no dump-load prepare_phase events recorded")
	}
	var sawWire bool
	for _, e := range evs {
		t.Logf("phase event: %s", e.Message)
		if strings.Contains(e.Message, "strategy=wire") {
			sawWire = true
		}
	}
	if !sawWire {
		t.Errorf("expected strategy=wire — fast path leaked through")
	}
	// All 3 keys present under the worktree's source prefix.
	assertKeyCount(t, o.SourceDB, 3)
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

// TestBeefyRedisCopyFanout proves the server-side COPY clone stays fast
// and correct at scale: 50k keys under the source prefix, fanned out to
// 4 clone prefixes, each verified to hold every key. Guards against a
// regression replacing pipelined COPY with a slow per-key round trip.
func TestBeefyRedisCopyFanout(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "redis:16379", 30*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:16379", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		return nil
	})

	const (
		nKeys   = 50000
		nClones = 4
	)
	wt := t.TempDir()
	// seq | sed builds nKeys `SET <prefix>k<n> <n>` lines piped into
	// redis-cli --pipe (one bulk load), so the seed itself isn't the
	// bottleneck the clone is being measured against.
	seed := `#!/usr/bin/env sh
set -eu
: "${REDIS_PREFIX:?REDIS_PREFIX not set}"
container="${REDIS_CONTAINER:-treeman-e2e-redis}"
seq 1 50000 | sed "s/.*/SET ${REDIS_PREFIX}k& &/" | docker exec -i "$container" redis-cli --pipe
`
	if err := os.WriteFile(filepath.Join(wt, "seed.sh"), []byte(seed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "schema.txt"), []byte("beefy-v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Redis: &config.RedisConn{URL: "redis://127.0.0.1:16379"},
		},
		Databases: []config.DatabaseConfig{{
			Engine:       "redis",
			NameTemplate: "treeman_beefy_{slug}",
			KeyPrefix:    "beefy:{slug}:",
			Seed: &config.Step{
				Run: "./seed.sh",
				Env: map[string]string{"REDIS_PREFIX": "{target_db}", "REDIS_CONTAINER": "treeman-e2e-redis"},
			},
			Inputs: []config.Input{{Glob: "schema.txt", Label: "keyset"}},
			TestClones: &config.TestClonesSpec{
				Clones:       config.ClonesSetting{Fixed: nClones},
				NameTemplate: "beefy:{slug}:w{n}:",
			},
		}},
	}

	env := harness.NewEnv(t, wt)
	start := time.Now()
	o := harness.AssertOutcome(t, env.RunPrepare(t, cfg), "redis", false)
	t.Logf("beefy redis: %d keys → %d clones in %s (source=%s)",
		nKeys, nClones, time.Since(start).Round(time.Millisecond), o.SourceDB)
	if len(o.Clones) != nClones {
		t.Fatalf("clone count = %d, want %d", len(o.Clones), nClones)
	}
	for _, clone := range o.Clones {
		assertKeyCount(t, clone, nKeys)
	}
}
