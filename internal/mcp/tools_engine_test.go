package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/engine"
)

// TestDropTemplateRejectsNonReservedNames is the trust-boundary
// guard on snapshot_drop. The MCP tool feeds rec.TemplateName into
// the prefix-LIKE / prefix-scan DropMatching / DropPrefix calls, so a
// hand-crafted SQLite row (or any client that synthesises a name)
// could otherwise reap real app databases. dropTemplate must refuse
// anything that doesn't carry one of treeman's reserved markers.
func TestDropTemplateRejectsNonReservedNames(t *testing.T) {
	cfg := &config.Config{}
	bad := []string{
		"kontainer", "kontainer_testing", "app_data",
		"_tmx_abc", // close to a reserved marker but not a match
		"tmbs",     // missing trailing _
		"",         // empty
	}
	for _, name := range bad {
		t.Run(name, func(t *testing.T) {
			err := dropTemplate(context.Background(), cfg, "mysql", name)
			if err == nil || !strings.Contains(err.Error(), "treeman-reserved") {
				t.Fatalf("name %q: want reserved-prefix rejection, got %v", name, err)
			}
		})
	}
}

// TestDropTemplateAcceptsReservedPrefixes confirms the guard isn't
// too strict — every legitimate reserved marker should pass the name
// check and reach the engine connect step (which then fails for
// unconfigured connections — that's the expected next failure).
func TestDropTemplateAcceptsReservedPrefixes(t *testing.T) {
	cfg := &config.Config{}
	ok := []string{"_tm_abc", "_tmbs_abc", "tm_abc", "tmbs_abc"}
	for _, name := range ok {
		t.Run(name, func(t *testing.T) {
			err := dropTemplate(context.Background(), cfg, "mysql", name)
			if err == nil {
				t.Fatalf("nil cfg should not return nil err")
			}
			if strings.Contains(err.Error(), "treeman-reserved") {
				t.Fatalf("name %q should pass reserved-marker check, got %v", name, err)
			}
			if !strings.Contains(err.Error(), "not configured") {
				t.Fatalf("name %q: expected 'not configured' connect-time error, got %v", name, err)
			}
		})
	}
}

// TestDropTemplateCoversEveryKnownEngine asserts the MCP-side
// dropTemplate handles every engine alias treeman accepts. Same
// regression-guard pattern as snapshot/gc_test.go: alias drift
// between validate.go's allow list and this switch is what caused
// the original `eviction: unsupported engine "mongodb"` incident.
func TestDropTemplateCoversEveryKnownEngine(t *testing.T) {
	cfg := &config.Config{}
	for _, eng := range engine.Known {
		t.Run(eng, func(t *testing.T) {
			err := dropTemplate(context.Background(), cfg, eng, "_tm_x")
			if err == nil {
				t.Fatalf("nil cfg should return an error")
			}
			if strings.Contains(err.Error(), "unsupported engine family") ||
				strings.Contains(err.Error(), "unknown engine") {
				t.Fatalf("engine %q falls into unsupported-engine arm: %v", eng, err)
			}
		})
	}
}
