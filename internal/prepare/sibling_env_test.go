package prepare

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
)

// TestRunStepEnvResolvesSiblingPerContext: a step that migrates a sibling
// engine too must point that engine at the sibling's namespace in the
// current context — the main-worktree overlay name on main, the slugged
// name on a linked worktree — never the dev namespace the `.env`
// fallback would reach (#132).
func TestRunStepEnvResolvesSiblingPerContext(t *testing.T) {
	cases := []struct {
		name string
		main bool
		want string
	}{
		{name: "linked worktree", want: "data/logs_testing_wtslug.db"},
		{name: "main worktree overlay", main: true, want: "data/logs_testing.db"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tm.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			wt := t.TempDir()
			repoID, err := st.EnsureRepo(ctx, wt, "repo")
			if err != nil {
				t.Fatal(err)
			}
			wtID, err := st.EnsureWorktree(ctx, repoID, wt, "wtslug", "develop")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(wt, "data"), 0o755); err != nil {
				t.Fatal(err)
			}
			got := filepath.Join(wt, "sibling.out")
			cfg := &config.Config{
				Databases: []config.DatabaseConfig{
					{
						Engine:       "sqlite",
						NameTemplate: "data/app_testing_{slug}.db",
						Migrate: &config.Step{
							Run: `printf %s "$LOGS_DB" > ` + got + ` && touch "$TREEMAN_TARGET_DB"`,
							Env: map[string]string{"LOGS_DB": "{db:1}"},
						},
					},
					{
						Engine:       "sqlite",
						NameTemplate: "data/logs_testing_{slug}.db",
						Migrate:      &config.Step{Run: `touch "$TREEMAN_TARGET_DB"`},
					},
				},
				MainWorktree: config.MainWorktreeConfig{
					Enabled:   true,
					Databases: []config.DatabaseOverlay{{}, {NameTemplate: "data/logs_testing.db"}},
				},
			}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			if c.main {
				config.ApplyMainWorktreeOverlay(cfg)
			}

			if _, err := Run(ctx, cfg, wt, slug.Slug{Value: "wtslug", Source: slug.SourceTicket}, st, repoID, wtID, nil); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(got)
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(wt, c.want); string(b) != want {
				t.Fatalf("LOGS_DB = %q, want %q", b, want)
			}
		})
	}
}
