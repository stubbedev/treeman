package wt

import (
	"testing"

	"github.com/stubbedev/treeman/internal/config"
)

// TestCopiesCoveringPatches — frontloading patches at create time has to
// bring in the `copies:` entries that supply a patch target first (a
// `.env` can't be patched before it's copied), and only those: the point
// of leaving the rest to the daemon is that the CLI never blocks on a
// vendor/ or node_modules/ copy.
func TestCopiesCoveringPatches(t *testing.T) {
	cases := []struct {
		name   string
		copies []string
		files  []string
		want   []string
	}{
		{"exact match", []string{".env", "node_modules"}, []string{".env"}, []string{".env"}},
		{"glob match", []string{".env*", "vendor"}, []string{".env.testing"}, []string{".env*"}},
		{"dir entry containing the target", []string{"config"}, []string{"config/app.json"}, []string{"config"}},
		{"doublestar match", []string{"**/*.ini"}, []string{"deploy/app.ini"}, []string{"**/*.ini"}},
		{"nothing relevant", []string{"vendor", "node_modules"}, []string{".env"}, []string{}},
		{"no copies at all", nil, []string{".env"}, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Worktrees.Copies = c.copies
			for _, f := range c.files {
				cfg.Patches = append(cfg.Patches, config.Patch{File: f})
			}
			got := copiesCoveringPatches(cfg)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}
