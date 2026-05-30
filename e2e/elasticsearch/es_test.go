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
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/treeman/e2e/harness"
	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/store"
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

// TestCrossWorktreeCacheReuseRestoresSource pins engine parity (issue #9):
// on a cache hit, the user-facing source prefix must be repopulated from
// the template the same way mysql/postgres/redis do — not just the
// clones. Two worktrees with identical content share one template (post-
// v5 fingerprint); the second is a cache hit AND its bare source prefix
// holds the same data as the first. Pre-parity this test would fail at
// the wt2 assertDocCount: ES cache-hit only fanned out clones, leaving
// the bare prefix empty.
func TestCrossWorktreeCacheReuseRestoresSource(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "es:19200", 120*time.Second, func() error {
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

	wt1 := t.TempDir()
	wt2 := t.TempDir()
	copyTree(t, "fixtures", filepath.Join(wt1, "fixtures"))
	copyTree(t, "fixtures", filepath.Join(wt2, "fixtures"))

	// Shared store so wt2 can see wt1's snapshot row and cache-hit off it.
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	env1 := harness.NewEnvShared(t, st, wt1, "feature-one")
	env2 := harness.NewEnvShared(t, st, wt2, "feature-two")

	// wt1: cold build seeds source + clones from the dump.
	o1 := harness.AssertOutcome(t, env1.RunPrepare(t, buildConfig()), "elasticsearch", false)
	t.Logf("wt1 cold: source=%s template=%s", o1.SourceDB, o1.TemplateName)
	assertDocCount(t, o1.SourceDB+"products", 2)
	assertDocCount(t, o1.SourceDB+"orders", 1)

	// wt2: identical content → cache hit. With the parity fix the bare
	// source prefix is repopulated as part of the fan-out, so its
	// indices match wt1's.
	o2 := harness.AssertOutcome(t, env2.RunPrepare(t, buildConfig()), "elasticsearch", true)
	t.Logf("wt2 hit:  source=%s template=%s", o2.SourceDB, o2.TemplateName)
	if o2.Fingerprint != o1.Fingerprint {
		t.Errorf("identical inputs must share a fingerprint: %s vs %s", o1.Fingerprint[:12], o2.Fingerprint[:12])
	}
	if o2.TemplateName != o1.TemplateName {
		t.Errorf("identical inputs must share a template: %s vs %s", o1.TemplateName, o2.TemplateName)
	}
	if o2.SourceDB == o1.SourceDB {
		t.Errorf("distinct worktrees must keep distinct source prefixes: both %s", o2.SourceDB)
	}
	// Parity proof: wt2's own bare source prefix is populated.
	assertDocCount(t, o2.SourceDB+"products", 2)
	assertDocCount(t, o2.SourceDB+"orders", 1)
}

// TestDumpLoadViaDockerExec proves the ES dump-load dispatcher picks
// the docker-exec fast path when ContainerRef is set: `docker exec -i
// CID curl -X POST http://localhost:9200/_bulk` from inside the
// container instead of host→TCP HTTP. Asserts strategy=docker-exec on
// the dump-load phase event AND that the bulk-loaded indices carry
// the expected doc counts.
func TestDumpLoadViaDockerExec(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "es:19200", 120*time.Second, func() error {
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
	// In-container URL; containerip rewrites host to the bridge IP and
	// 9200 is what ES listens on inside the container.
	cfg.Connections.Elasticsearch.URL = "http://127.0.0.1:9200"
	cfg.Connections.Elasticsearch.ContainerRef = config.ContainerRef{
		Container: "treeman-e2e-es",
	}
	env := harness.NewEnv(t, wt)
	o := harness.AssertOutcome(t, env.RunPrepare(t, cfg), "elasticsearch", false)

	evs, err := env.Store.QueryEvents(env.Ctx, store.EventFilter{
		WorktreeID: env.WTID,
		EventTypes: []string{"prepare_phase"},
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
		t.Errorf("expected strategy=docker-exec — ContainerRef should have selected curl-in-container")
	}
	assertDocCount(t, o.SourceDB+"products", 2)
	assertDocCount(t, o.SourceDB+"orders", 1)
}

// TestIncrementalAncestorBuild proves task #4 wired to elasticsearch:
// adding a NEW file under an `inputs:` glob (an extending vector, never
// an edit) makes the next prep build from the cached ancestor template
// — clone every index via the native `_clone` API, skip the NDJSON dump
// reload, just register the new snapshot — instead of dropping the
// source prefix and re-streaming the bulk file.
func TestIncrementalAncestorBuild(t *testing.T) {
	harness.SkipIfNoDocker(t)
	t.Cleanup(harness.ComposeUp(t, harness.MustAbs(".")))
	harness.WaitForReady(t, "es:19200", 120*time.Second, func() error {
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
	o1 := harness.AssertOutcome(t, env.RunPrepare(t, cfg), "elasticsearch", false)
	if o1.IncrementalBase != "" {
		t.Fatalf("pass 1 should be cold; got base=%s", o1.IncrementalBase)
	}
	assertDocCount(t, o1.SourceDB+"products", 2)
	assertDocCount(t, o1.SourceDB+"orders", 1)
	t.Logf("pass1 cold: fp=%s tmpl=%s", o1.Fingerprint[:12], o1.TemplateName)

	if err := os.WriteFile(filepath.Join(extrasDir, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	o2 := harness.AssertOutcome(t, env.RunPrepare(t, cfg), "elasticsearch", false)
	if o2.IncrementalBase != o1.Fingerprint {
		t.Errorf("pass 2 should build incrementally from pass 1; got IncrementalBase=%q want %q",
			o2.IncrementalBase, o1.Fingerprint)
	}
	if o2.Fingerprint == o1.Fingerprint {
		t.Errorf("pass 2 must produce a NEW fingerprint: still %s", o1.Fingerprint[:12])
	}
	t.Logf("pass2 incremental: fp=%s base=%s", o2.Fingerprint[:12], o2.IncrementalBase[:12])
	// Source prefix carries the loaded indices from the ancestor template
	// (the dump did NOT re-stream — we skipped it via incremental).
	assertDocCount(t, o2.SourceDB+"products", 2)
	assertDocCount(t, o2.SourceDB+"orders", 1)
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
				Dump:         config.DumpList{{Path: "fixtures/dump.ndjson"}},
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
