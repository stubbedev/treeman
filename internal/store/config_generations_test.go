package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// TestConfigGenerationsRoundTrip exercises the per-repo config history
// that replaced the `.treeman.yaml.bak.*` files: each SnapshotConfig
// assigns the next monotonic generation, List returns them newest-first,
// and Get fetches exact bytes by number.
func TestConfigGenerationsRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	repo := "/tmp/repoA"
	cfg := filepath.Join(repo, ".treeman.yaml")

	g1, err := st.SnapshotConfig(ctx, repo, cfg, []byte("v1"))
	if err != nil {
		t.Fatal(err)
	}
	g2, err := st.SnapshotConfig(ctx, repo, cfg, []byte("v2"))
	if err != nil {
		t.Fatal(err)
	}
	if g1 != 1 || g2 != 2 {
		t.Fatalf("expected generations 1,2 got %d,%d", g1, g2)
	}

	gens, err := st.ListConfigGenerations(ctx, repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(gens) != 2 {
		t.Fatalf("expected 2 generations, got %d", len(gens))
	}
	if gens[0].Generation != 2 || string(gens[0].Content) != "v2" {
		t.Errorf("newest-first ordering broken: %+v", gens[0])
	}

	got, err := st.GetConfigGeneration(ctx, repo, cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Content) != "v1" {
		t.Errorf("gen 1 content = %q, want v1", got.Content)
	}

	if _, err := st.GetConfigGeneration(ctx, repo, cfg, 99); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("missing generation should return sql.ErrNoRows, got %v", err)
	}
}

// TestConfigGenerationsPerRepoIsolation confirms generations are keyed
// by repo: two repos each count from 1 and don't see each other's rows.
func TestConfigGenerationsPerRepoIsolation(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "treeman.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	a := filepath.Join("/tmp/repoA", ".treeman.yaml")
	b := filepath.Join("/tmp/repoB", ".treeman.yaml")
	if _, err := st.SnapshotConfig(ctx, "/tmp/repoA", a, []byte("a1")); err != nil {
		t.Fatal(err)
	}
	gen, err := st.SnapshotConfig(ctx, "/tmp/repoB", b, []byte("b1"))
	if err != nil {
		t.Fatal(err)
	}
	if gen != 1 {
		t.Errorf("repoB first generation = %d, want 1 (must not share repoA's counter)", gen)
	}
	gensA, _ := st.ListConfigGenerations(ctx, "/tmp/repoA", a)
	if len(gensA) != 1 {
		t.Errorf("repoA should have exactly 1 generation, got %d", len(gensA))
	}
}
