//go:build e2e

// Package matrixmongo_e2e fills the mongodb cells of the option×engine
// matrix: compose_service resolution, test_clones:auto detection, and
// password $ENV-ref resolution (which, being the bare URI scalar form,
// also exercises the URI-scalar connection form). Auth-enabled server so
// the password-ref test is meaningful. A successful RunPrepare proves
// the connection resolved + authenticated.
package matrixmongo_e2e

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

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	dbmongo "github.com/stubbedev/treeman/internal/db/mongo"
	"github.com/stubbedev/treeman/internal/migrations/testfw"
	"github.com/stubbedev/treeman/internal/resolve"
)

const authURI = "mongodb://root:mongopw@127.0.0.1:27130/?authSource=admin"

func up(t *testing.T) {
	t.Helper()
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "mongo:27130", 60*time.Second, func() error {
		out, err := exec.Command("docker", "exec", "treeman-e2e-mxmongo",
			"mongosh", "-u", "root", "-p", "mongopw", "--authenticationDatabase", "admin",
			"--quiet", "--eval", "db.runCommand({ping:1}).ok").CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "1" {
			return fmt.Errorf("mongo not ready: %s", out)
		}
		return nil
	})
}

func TestMongoComposeService(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)
	project := composeProject(t, "treeman-e2e-mxmongo")

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Mongodb: &config.MongoConn{
				URI:          "mongodb://root:mongopw@placeholder:27017/?authSource=admin",
				ContainerRef: config.ContainerRef{ComposeService: "mongodb", ComposeProject: project},
			},
		},
		Databases: []config.DatabaseConfig{{Engine: "mongodb", NameTemplate: "mxmongo_svc_{slug}"}},
	}
	// A clean RunPrepare proves compose_service resolved the container +
	// the driver connected (with auth) — a bad resolve would error.
	harness.AssertOutcome(t, harness.NewEnv(t, t.TempDir()).RunPrepare(t, cfg), "mongodb", false)
}

func TestMongoClonesAuto(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)

	wt := t.TempDir()
	write(t, wt, "package.json", `{"devDependencies":{"jest":"^29"}}`)
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{Mongodb: &config.MongoConn{URI: authURI}},
		Databases: []config.DatabaseConfig{{
			Engine: "mongodb", NameTemplate: "mxmongo_ca_{slug}",
			TestClones: &config.TestClonesSpec{Clones: config.ClonesSetting{Auto: true}, NameTemplate: "mxmongo_ca_{slug}_w{n}"},
		}},
	}
	o := harness.AssertOutcome(t, harness.NewEnv(t, wt).RunPrepare(t, cfg), "mongodb", false)
	want := int(testfw.DetectedCloneCount(wt))
	if want == 0 {
		want = int(testfw.NumCPUs())
	}
	if len(o.Clones) != want {
		t.Fatalf("clones:auto produced %d clones, want %d (detector)", len(o.Clones), want)
	}
}

// TestMongoConnFromEnvFile covers the URI engines' secret-from-env path
// (the analog of mysql/postgres `password: $ENV`): the YAML omits the
// mongodb connection and the resolver derives it from MONGODB_URI in the
// env file. The derived URI carries the auth creds → prepare connects.
func TestMongoConnFromEnvFile(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	wt := t.TempDir()
	write(t, wt, ".env", "MONGODB_URI=mongodb://root:mongopw@127.0.0.1:27130/?authSource=admin\n")
	write(t, wt, ".treeman.yaml", `env_sources: [.env]
databases:
  - engine: mongodb
    name_template: mxmongo_env_{slug}
`)
	cfg, err := resolve.LoadResolved(wt)
	if err != nil {
		t.Fatalf("LoadResolved: %v", err)
	}
	if cfg.Connections.Mongodb == nil || !strings.Contains(cfg.Connections.Mongodb.URI, "mongopw") {
		t.Fatalf("mongodb connection not derived from MONGODB_URI env: %+v", cfg.Connections.Mongodb)
	}
	harness.AssertOutcome(t, harness.NewEnv(t, wt).RunPrepare(t, &cfg), "mongodb", false)
}

// TestMongoURIScalar covers the bare-URI scalar connection form for
// mongo (conn_forms covers mysql/postgres/redis; mongo + es were the
// remaining scalar cells).
func TestMongoURIScalar(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	wt := t.TempDir()
	write(t, wt, ".treeman.yaml", `connections:
  mongodb: "mongodb://root:mongopw@127.0.0.1:27130/?authSource=admin"
databases:
  - engine: mongodb
    name_template: mxmongo_uri_{slug}
`)
	cfg, err := config.LoadLayered(wt)
	if err != nil {
		t.Fatalf("LoadLayered (URI scalar didn't parse?): %v", err)
	}
	harness.AssertOutcome(t, harness.NewEnv(t, wt).RunPrepare(t, &cfg), "mongodb", false)
}

// TestMongoMigrateAndSeedRun proves the migrate AND seed steps actually
// run for mongodb. migrate was previously wired only for mysql/postgres —
// a mongo `migrate:` was silently ignored; this pins the fix. Each step
// writes $TREEMAN_TARGET_DB to a witness so we can confirm it ran with
// the right per-run target.
func TestMongoMigrateAndSeedRun(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)

	wt := t.TempDir()
	mwit := filepath.Join(wt, "migrate.out")
	swit := filepath.Join(wt, "seed.out")
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{Mongodb: &config.MongoConn{URI: authURI}},
		Databases: []config.DatabaseConfig{{
			Engine: "mongodb", NameTemplate: "mxmongo_ms_{slug}",
			Migrate: &config.Step{Run: `echo "$TREEMAN_TARGET_DB" > ` + mwit},
			Seed:    &config.Step{Run: `echo "$TREEMAN_TARGET_DB" > ` + swit},
		}},
	}
	o := harness.AssertOutcome(t, harness.NewEnv(t, wt).RunPrepare(t, cfg), "mongodb", false)
	assertFileHas(t, mwit, o.SourceDB) // migrate ran (newly wired for mongo)
	assertFileHas(t, swit, o.SourceDB) // seed ran
}

// TestMongoPoolMaxCaps proves pool_max caps concurrency: with
// maxPoolSize=2, eight concurrent server-side {sleep} ops (~400ms each,
// lock:none so they don't serialize on a lock) can only run 2 at a time
// → ~4 batches ≈ 1.6s. Uncapped they'd all run at once (~0.4s). The
// floor is the mongod-side sleep, so it's machine-independent.
func TestMongoPoolMaxCaps(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)

	const poolMax = 2
	const ops = 8
	const sleepMillis = 400

	drv, err := dbmongo.Connect(context.Background(), config.MongoConn{URI: authURI, PoolMax: poolMax})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer drv.Close(context.Background())

	// Confirm the server supports the {sleep} testing command before
	// timing — otherwise skip rather than flake.
	probe := drv.Client.Database("admin").RunCommand(context.Background(),
		bson.D{{Key: "sleep", Value: 1}, {Key: "millis", Value: 1}, {Key: "lock", Value: "none"}})
	if err := probe.Err(); err != nil {
		t.Skipf("mongod {sleep} command unavailable: %v", err)
	}

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < ops; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_ = drv.Client.Database("admin").RunCommand(ctx,
				bson.D{{Key: "sleep", Value: 1}, {Key: "millis", Value: sleepMillis}, {Key: "lock", Value: "none"}}).Err()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	if elapsed < time.Second {
		t.Errorf(
			"pool_max=2 did not cap concurrency: %d×%dms sleeps finished in %s (want ≥1s — they ran in parallel)",
			ops,
			sleepMillis,
			elapsed,
		)
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
