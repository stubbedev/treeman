//go:build e2e

// Package matrixes_e2e fills the elasticsearch cells of the option×engine
// matrix: compose_service resolution, test_clones:auto detection,
// connection-from-env (ELASTICSEARCH_URL), and the bare-URL scalar form.
package matrixes_e2e

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	dbes "github.com/stubbedev/treeman/internal/db/es"
	"github.com/stubbedev/treeman/internal/migrations/testfw"
	"github.com/stubbedev/treeman/internal/resolve"
)

const esURL = "http://127.0.0.1:19210"

func up(t *testing.T) {
	t.Helper()
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "es:19210", 120*time.Second, func() error {
		resp, err := http.Get(esURL + "/_cluster/health?wait_for_status=yellow&timeout=5s")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return http.ErrServerClosed
		}
		return nil
	})
}

func TestESComposeService(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)
	project := composeProject(t, "treeman-e2e-mxes")

	cfg := &config.Config{
		Connections: config.ConnectionsConfig{
			Elasticsearch: &config.EsConn{
				URL:          "http://placeholder:9200",
				ContainerRef: config.ContainerRef{ComposeService: "elasticsearch", ComposeProject: project},
			},
		},
		Databases: []config.DatabaseConfig{{Engine: "elasticsearch", KeyPrefix: "mxes_svc_{slug}_"}},
	}
	harness.AssertOutcome(t, harness.NewEnv(t, t.TempDir()).RunPrepare(t, cfg), "elasticsearch", false)
}

func TestESClonesAuto(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)

	wt := t.TempDir()
	write(t, wt, "package.json", `{"devDependencies":{"jest":"^29"}}`)
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{Elasticsearch: &config.EsConn{URL: esURL}},
		Databases: []config.DatabaseConfig{{
			Engine: "elasticsearch", KeyPrefix: "mxes_ca_{slug}_",
			TestClones: &config.TestClonesSpec{Clones: config.ClonesSetting{Auto: true}, NameTemplate: "mxes_ca_{slug}_w{n}_"},
		}},
	}
	o := harness.AssertOutcome(t, harness.NewEnv(t, wt).RunPrepare(t, cfg), "elasticsearch", false)
	want := int(testfw.DetectedCloneCount(wt))
	if want == 0 {
		want = int(testfw.NumCPUs())
	}
	if len(o.Clones) != want {
		t.Fatalf("clones:auto produced %d clones, want %d (detector)", len(o.Clones), want)
	}
}

// TestESConnFromEnvFile — secret/connection-from-env path: YAML omits the
// es connection; resolver derives it from ELASTICSEARCH_URL in the env file.
func TestESConnFromEnvFile(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	wt := t.TempDir()
	write(t, wt, ".env", "ELASTICSEARCH_URL="+esURL+"\n")
	write(t, wt, ".treeman.yaml", `env_sources: [.env]
databases:
  - engine: elasticsearch
    key_prefix: "mxes_env_{slug}_"
`)
	cfg, err := resolve.LoadResolved(wt)
	if err != nil {
		t.Fatalf("LoadResolved: %v", err)
	}
	if cfg.Connections.Elasticsearch == nil || !strings.Contains(cfg.Connections.Elasticsearch.URL, "19210") {
		t.Fatalf("es connection not derived from ELASTICSEARCH_URL env: %+v", cfg.Connections.Elasticsearch)
	}
	harness.AssertOutcome(t, harness.NewEnv(t, wt).RunPrepare(t, &cfg), "elasticsearch", false)
}

// TestESURLScalar — bare-URL scalar connection form for es.
func TestESURLScalar(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	wt := t.TempDir()
	write(t, wt, ".treeman.yaml", `connections:
  elasticsearch: "`+esURL+`"
databases:
  - engine: elasticsearch
    key_prefix: "mxes_url_{slug}_"
`)
	cfg, err := config.LoadLayered(wt)
	if err != nil {
		t.Fatalf("LoadLayered (URL scalar didn't parse?): %v", err)
	}
	harness.AssertOutcome(t, harness.NewEnv(t, wt).RunPrepare(t, &cfg), "elasticsearch", false)
}

// TestESMigrateAndSeedRun proves migrate AND seed run for elasticsearch
// (migrate was previously mysql/postgres-only and silently ignored here).
func TestESMigrateAndSeedRun(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)

	wt := t.TempDir()
	mwit := filepath.Join(wt, "migrate.out")
	swit := filepath.Join(wt, "seed.out")
	cfg := &config.Config{
		Connections: config.ConnectionsConfig{Elasticsearch: &config.EsConn{URL: esURL}},
		Databases: []config.DatabaseConfig{{
			Engine: "elasticsearch", KeyPrefix: "mxes_ms_{slug}_",
			Migrate: &config.Step{Run: `echo "$TREEMAN_TARGET_DB" > ` + mwit},
			Seed:    &config.Step{Run: `echo "$TREEMAN_TARGET_DB" > ` + swit},
		}},
	}
	o := harness.AssertOutcome(t, harness.NewEnv(t, wt).RunPrepare(t, cfg), "elasticsearch", false)
	assertFileHas(t, mwit, o.SourceDB)
	assertFileHas(t, swit, o.SourceDB)
}

// TestESPoolMaxApplied verifies pool_max is wired for elasticsearch:
// the driver's HTTP transport caps connections per host. ES has no
// server-side sleep to time a real cap observation against, so this is
// a white-box check that the knob reaches the transport (the cap itself
// is Go's http.Transport, which is well-tested upstream).
func TestESPoolMaxApplied(t *testing.T) {
	harness.SkipIfNoDocker(t)
	up(t)

	const poolMax = 3
	drv, err := dbes.Connect(context.Background(), config.EsConn{URL: esURL, PoolMax: poolMax})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	tr, ok := drv.HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("pool_max set but HTTP.Transport is %T, want *http.Transport", drv.HTTP.Transport)
	}
	if tr.MaxConnsPerHost != poolMax {
		t.Errorf("MaxConnsPerHost = %d, want %d (pool_max not applied)", tr.MaxConnsPerHost, poolMax)
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
