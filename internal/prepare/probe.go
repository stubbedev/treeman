package prepare

import (
	"context"
	"fmt"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/engineconn"
	"github.com/stubbedev/treeman/internal/engine"
)

// ProbeDatabases connects once to the engine of each listed
// cfg.Databases index, the way prepare would, and returns the first
// failure. It does not wait for an engine that is still starting unless
// ctx asks for that (reachability.WithStartupWait).
func ProbeDatabases(ctx context.Context, cfg *config.Config, dbIdx []int) error {
	for _, i := range dbIdx {
		if i < 0 || i >= len(cfg.Databases) {
			return fmt.Errorf("probe databases[%d]: the config declares %d databases", i, len(cfg.Databases))
		}
		d := cfg.Databases[i]
		fam, ok := engine.Canonical(d.Engine)
		if !ok {
			return fmt.Errorf("probe databases[%d]: unknown engine %q", i, d.Engine)
		}
		conn, configured, err := engineconn.Connect(ctx, cfg, fam, d.Connection)
		if err != nil {
			return fmt.Errorf("probe databases[%d] (%s): %w", i, d.Engine, err)
		}
		if configured {
			_ = conn.Close()
		}
	}
	return nil
}
