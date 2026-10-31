package wt

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/slug"
)

const ignoredDumpRel = "database/dumps/seed.sql"

func newIgnoredDumpRepo(t *testing.T, withMainDump bool) (repo, wtPath string) {
	t.Helper()
	repo = filepath.Join(t.TempDir(), "repo")
	gitRunT(t, "", "init", "-q", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("/database/dumps\n/data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunT(t, repo, "add", ".gitignore")
	gitRunT(t, repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "init")
	if withMainDump {
		dump := filepath.Join(repo, ignoredDumpRel)
		if err := os.MkdirAll(filepath.Dir(dump), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dump, []byte("CREATE TABLE seeded (id INTEGER);\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wtPath = filepath.Join(t.TempDir(), "feature")
	gitRunT(t, repo, "worktree", "add", "-q", "-b", "feature", wtPath)
	if err := os.MkdirAll(filepath.Join(wtPath, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	return repo, wtPath
}

func ignoredDumpConfig(optional bool) *config.Config {
	return &config.Config{Databases: []config.DatabaseConfig{{
		Engine:       "sqlite",
		NameTemplate: "data/app_{slug}.db",
		Dump:         config.DumpList{{Path: ignoredDumpRel, Optional: optional}},
	}}}
}

func runIgnoredDumpFinalize(t *testing.T, cfg *config.Config, repo, wtPath string) error {
	t.Helper()
	ctx := context.Background()
	st := withTempStore(t)
	repoID, err := st.EnsureRepo(ctx, repo, "repo")
	if err != nil {
		t.Fatal(err)
	}
	wtID, err := st.EnsureWorktree(ctx, repoID, wtPath, "feature", "feature")
	if err != nil {
		t.Fatal(err)
	}
	return RunLocalFinalize(ctx, cfg, repo, wtPath, slug.Slug{Value: "feature", Source: slug.SourceTicket},
		false, st, repoID, wtID, nil, false, nil)
}

func TestRunLocalFinalizeLinksGitignoredDumpFromMainCheckout(t *testing.T) {
	repo, wtPath := newIgnoredDumpRepo(t, true)
	if err := runIgnoredDumpFinalize(t, ignoredDumpConfig(false), repo, wtPath); err != nil {
		t.Fatalf("finalize --local with a gitignored dump only in the main checkout: %v", err)
	}
	target, err := os.Readlink(filepath.Join(wtPath, ignoredDumpRel))
	if err != nil {
		t.Fatalf("dump not linked into the worktree: %v", err)
	}
	if want := filepath.Join(repo, ignoredDumpRel); target != want {
		t.Errorf("dump links to %s, want %s", target, want)
	}
}

func TestRunLocalFinalizeNamesGitignoredDumpMissingFromMainCheckout(t *testing.T) {
	repo, wtPath := newIgnoredDumpRepo(t, false)
	err := runIgnoredDumpFinalize(t, ignoredDumpConfig(false), repo, wtPath)
	if err == nil {
		t.Fatal("want an error for a required gitignored dump absent everywhere")
	}
	if !strings.Contains(err.Error(), "databases[0].dump[0] "+ignoredDumpRel+" is gitignored") {
		t.Errorf("err = %v, want it to name the gitignored dump", err)
	}
}

func TestLinkPathsSkipsMissingOptionalAndTrackedDumps(t *testing.T) {
	repo, wtPath := newIgnoredDumpRepo(t, false)
	if err := os.WriteFile(filepath.Join(wtPath, "tracked.sql"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := ignoredDumpConfig(true)
	cfg.Databases[0].Dump = append(cfg.Databases[0].Dump, config.DumpSpec{Path: "tracked.sql"})
	cfg.Worktrees.Links = []string{"vendor"}
	got, err := LinkPaths(context.Background(), cfg, repo, wtPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"vendor"}; !slices.Equal(got, want) {
		t.Errorf("LinkPaths = %v, want %v", got, want)
	}
}
