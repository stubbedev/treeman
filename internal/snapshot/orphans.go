package snapshot

import (
	"context"
	"fmt"
	"regexp"
	"sort"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/engineconn"
	"github.com/stubbedev/treeman/internal/engine"
)

// Orphan is one engine-side template namespace no snapshots row
// references. It can never be cache-hit, restored from, or collected
// by the row-driven GC sweeps — a crash between the engine-side
// SnapshotCreate and the SQLite RecordSnapshot strands it forever, so
// the doctor probe surfaces them and DropOrphans reclaims the space.
type Orphan struct {
	Family engine.Family
	// Name is the orphaned template namespace: a database name for
	// name-scoped engines, an index-name prefix for ES.
	Name string
}

// nameScopedTemplate matches the database names treeman derives for
// cached templates on name-scoped engines: the current `_tm_<hex16>`
// form, the legacy `_tm_tmpl_<engine>_<hex16>` form, and either's
// pre-warmed `_spare<n>` family members. Capture group 1 is the base
// template name (spare suffix stripped).
var nameScopedTemplate = regexp.MustCompile(`^(_tm_(?:tmpl_[a-z]+_)?[0-9a-f]{16})(?:_spare[0-9]+)?$`)

// esTemplatePrefix matches index names under an ES template prefix
// (`tm_<hex16>_<index>`); capture group 1 is the row-recorded prefix.
var esTemplatePrefix = regexp.MustCompile(`^(tm_[0-9a-f]{16}_)`)

// orphanFamilies is the probe's engine coverage. Redis is excluded:
// its templates are key prefixes whose enumeration needs a full SCAN
// (see engineconn.Conn.ListMatching), and stranded prefix keys cost
// bytes, not databases.
var orphanFamilies = []engine.Family{
	engine.FamilyMySQL, engine.FamilyPostgres, engine.FamilyMongo, engine.FamilyES,
}

// templateNameSource is the store subset FindOrphans needs.
type templateNameSource interface {
	ListAllTemplateNames(ctx context.Context) (map[string]bool, error)
}

// FindOrphans lists engine-side template namespaces with no snapshots
// row, for every configured engine. Only names matching treeman's own
// derived template shapes are considered — user databases can never be
// flagged, no matter what they're called. Engines that are configured
// but unreachable contribute an error (not silence) so the caller can
// distinguish "clean" from "couldn't look".
func FindOrphans(ctx context.Context, cfg *config.Config, st templateNameSource) ([]Orphan, error) {
	known, err := st.ListAllTemplateNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("list snapshot rows: %w", err)
	}
	var out []Orphan
	for _, fam := range orphanFamilies {
		if !engineconn.Configured(cfg, fam) {
			continue
		}
		conn, _, cerr := engineconn.Connect(ctx, cfg, fam)
		if cerr != nil {
			return out, fmt.Errorf("connect %s: %w", fam, cerr)
		}
		names, lerr := listTemplates(ctx, conn, fam)
		_ = conn.Close()
		if lerr != nil {
			return out, fmt.Errorf("list %s templates: %w", fam, lerr)
		}
		for _, name := range names {
			if !known[name] {
				out = append(out, Orphan{Family: fam, Name: name})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Family != out[j].Family {
			return out[i].Family < out[j].Family
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// listTemplates returns the engine's template namespaces in the same
// form the snapshots rows record them: base database names (spares
// folded into their template) for name-scoped engines, index-name
// prefixes for ES. Results are deduped.
func listTemplates(ctx context.Context, conn engineconn.Conn, fam engine.Family) ([]string, error) {
	prefix, re := "_tm_", nameScopedTemplate
	if fam == engine.FamilyES {
		prefix, re = "tm_", esTemplatePrefix
	}
	names, err := conn.ListMatching(ctx, prefix)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		m := re.FindStringSubmatch(n)
		if m == nil || seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		out = append(out, m[1])
	}
	return out, nil
}

// spareNamePattern matches one pre-warmed spare database name;
// capture group 1 is the owning template.
var spareNamePattern = regexp.MustCompile(`^(_tm_(?:tmpl_[a-z]+_)?[0-9a-f]{16})_spare[0-9]+$`)

// SpareCounts returns the number of pre-warmed spare databases per
// template name, as they exist engine-side right now. Postgres-only
// today (the only engine prewarm supports); returns an empty map
// without error when postgres isn't configured. Feeds the doctor's
// snapshots probe and the `snapshots list` SPARES column — the only
// surfaces that make the otherwise-anonymous pool observable.
func SpareCounts(ctx context.Context, cfg *config.Config) (map[string]int, error) {
	if !engineconn.Configured(cfg, engine.FamilyPostgres) {
		return map[string]int{}, nil
	}
	conn, _, err := engineconn.Connect(ctx, cfg, engine.FamilyPostgres)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	names, err := conn.ListMatching(ctx, "_tm_")
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, name := range names {
		if m := spareNamePattern.FindStringSubmatch(name); m != nil {
			out[m[1]]++
		}
	}
	return out, nil
}

// DropOrphans reclaims the given orphans: each template is dropped
// along with its pre-warmed `_spare` family (DropMatching covers both
// the exact name-scoped DB and ES's prefix-indexed family). Continues
// past per-orphan failures and returns them joined so one wedged drop
// doesn't strand the rest.
func DropOrphans(ctx context.Context, cfg *config.Config, orphans []Orphan) (dropped int, errs []error) {
	conns := map[engine.Family]engineconn.Conn{}
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for _, o := range orphans {
		conn, ok := conns[o.Family]
		if !ok {
			c, configured, cerr := engineconn.Connect(ctx, cfg, o.Family)
			if !configured || cerr != nil {
				errs = append(errs, fmt.Errorf("connect %s: configured=%v err=%w", o.Family, configured, cerr))
				continue
			}
			conns[o.Family] = c
			conn = c
		}
		if err := conn.DropSnapshot(ctx, o.Name); err != nil {
			errs = append(errs, fmt.Errorf("drop %s %s: %w", o.Family, o.Name, err))
			continue
		}
		if _, err := conn.DropMatching(ctx, o.Name+PrewarmSuffix); err != nil {
			errs = append(errs, fmt.Errorf("drop %s spare family %s%s*: %w", o.Family, o.Name, PrewarmSuffix, err))
			continue
		}
		dropped++
	}
	return dropped, errs
}
