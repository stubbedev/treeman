//go:build e2e

package es_e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/prepare"
)

func TestElasticsearchEndToEnd(t *testing.T) {
	harness.SkipIfNoDocker(t)
	composeDir := harness.MustAbs(".")
	t.Cleanup(harness.ComposeUp(t, composeDir))

	harness.WaitForReady(t, "es:19200", 120*time.Second, func() error {
		c, err := net.DialTimeout("tcp", "127.0.0.1:19200", 1*time.Second)
		if err != nil {
			return err
		}
		_ = c.Close()
		// also wait for the cluster to accept requests
		resp, err := http.Get("http://127.0.0.1:19200/_cluster/health?wait_for_status=yellow&timeout=5s")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("cluster not ready: %s", resp.Status)
		}
		return nil
	})

	wt := t.TempDir()
	copyTree(t, "fixtures", filepath.Join(wt, "fixtures"))
	cfg := buildConfig()
	env := harness.NewEnv(t, wt)

	outs := env.RunPrepare(t, cfg)
	o1 := harness.AssertOutcome(t, outs, "elasticsearch", false)
	t.Logf("pass1: sourcePrefix=%s template=%s", o1.SourceDB, o1.TemplateName)
	assertDocCount(t, o1.SourceDB+"products", 2)
	assertDocCount(t, o1.SourceDB+"orders", 1)

	outs = env.RunPrepare(t, cfg)
	o2 := harness.AssertOutcome(t, outs, "elasticsearch", true)
	if o2.Fingerprint != o1.Fingerprint {
		t.Errorf("fingerprint drift: %s vs %s", o1.Fingerprint, o2.Fingerprint)
	}

	// Edit the NDJSON dump → fingerprint changes (dump file is hashed).
	if err := os.WriteFile(filepath.Join(wt, "fixtures/dump.ndjson"), []byte(`{"index":{"_index":"{target_db}products","_id":"1"}}
{"name":"WidgetV2","price":11.99}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	outs = env.RunPrepare(t, cfg)
	o3 := harness.AssertOutcome(t, outs, "elasticsearch", false)
	if o3.Fingerprint == o1.Fingerprint {
		t.Errorf("fingerprint unchanged after dump edit")
	}

	// ── teardown: `wt delete` drops the per-worktree indices, keeps the cache ──
	// TeardownDatabases is the DB layer of `treeman wt delete`: for ES it
	// DELETEs every index under the worktree's prefix while leaving the
	// fingerprint-keyed template indices intact, so the next prepare with the
	// same inputs is a cache hit.
	if err := prepare.TeardownDatabases(env.Ctx, cfg, env.Slug.Value, env.RepoID, env.WTID, env.Store); err != nil {
		t.Fatalf("TeardownDatabases: %v", err)
	}
	// Wildcard _count over the (now-empty) worktree prefix returns 0 when no
	// index matches — the clean "namespace is gone" signal.
	assertDocCount(t, o3.SourceDB+"*", 0)
	// The fingerprint-keyed template indices must SURVIVE teardown so the
	// next worktree with the same inputs still hits the cache. After the
	// dump edit the template holds a single products doc.
	assertDocCount(t, o3.TemplateName+"*", 1)

	// ── fanout + teardown permutation ──
	// Pre-warm clones (paratest workers). Clone index prefixes nest under
	// the source prefix, so the single source-prefix DropMatching on
	// teardown reaps source + clones together. A distinct slug (fresh
	// worktree, unedited dump) keeps these indices off the assertions above.
	t.Run("fanout_teardown", func(t *testing.T) {
		wt2 := t.TempDir()
		copyTree(t, "fixtures", filepath.Join(wt2, "fixtures"))
		fcfg := buildConfig()
		fcfg.Databases[0].KeyPrefix = "wtfan_{slug}_"
		fcfg.Databases[0].TestClones = &config.TestClonesSpec{
			Clones:       config.ClonesSetting{Fixed: 2},
			NameTemplate: "wtfan_{slug}_w{n}_",
		}
		env2 := harness.NewEnv(t, wt2)
		fo := harness.AssertOutcome(t, env2.RunPrepare(t, fcfg), "elasticsearch", false)
		if len(fo.Clones) != 2 {
			t.Fatalf("fanout: got %d clones, want 2 (%v)", len(fo.Clones), fo.Clones)
		}
		for _, c := range fo.Clones {
			assertDocCount(t, c+"*", 3) // each clone: products(2) + orders(1)
		}

		if err := prepare.TeardownDatabases(env2.Ctx, fcfg, env2.Slug.Value, env2.RepoID, env2.WTID, env2.Store); err != nil {
			t.Fatalf("TeardownDatabases: %v", err)
		}
		assertDocCount(t, fo.SourceDB+"*", 0) // source + nested clones all dropped
		for _, c := range fo.Clones {
			assertDocCount(t, c+"*", 0)
		}
		assertDocCount(t, fo.TemplateName+"*", 3) // cache survives
	})
}

func buildConfig() *config.Config {
	return &config.Config{
		Connections: config.ConnectionsConfig{
			Elasticsearch: &config.EsConn{URL: "http://127.0.0.1:19200"},
		},
		Databases: []config.DatabaseConfig{
			{
				Engine:       "elasticsearch",
				NameTemplate: "treeman_e2e_{slug}",
				KeyPrefix:    "wte2e_{slug}_",
				Dump:         &config.DumpSpec{Path: "fixtures/dump.ndjson"},
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
		_ = os.WriteFile(d, body, 0o644)
	}
}

func assertDocCount(t *testing.T, index string, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET",
		"http://127.0.0.1:19200/"+index+"/_count", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("count %s: %v", index, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("count %s: %s: %s", index, resp.Status, string(body))
	}
	var out struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("parse count: %v: %s", err, string(body))
	}
	if out.Count != want {
		t.Errorf("%s doc count = %d, want %d", index, out.Count, want)
	}
}
