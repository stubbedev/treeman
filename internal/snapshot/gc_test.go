package snapshot

import (
	"context"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/engine"
	"github.com/stubbedev/treeman/internal/store"
)

// TestDropTemplateCoversEveryKnownEngine asserts that dropTemplate
// has a case for every alias treeman accepts under
// `databases[].engine`. Regression guard: the production incident
// was gc.go's switch handling only mysql + postgres while
// validate.go accepted mongo / es / redis / opensearch / mariadb
// / tidb / postgresql; cached templates for the missing engines
// looped forever in the eviction sweeps emitting WARN spam.
//
// Drives dropTemplate with a Config whose ConnectionsConfig has
// every block nil. A handled engine returns "connections.<x> not
// configured" (proves the switch arm fired and tried to connect);
// an unhandled engine returns "unsupported engine" (the
// regression). Any new alias added to engine.Known without a
// matching arm here fails this test.
func TestDropTemplateCoversEveryKnownEngine(t *testing.T) {
	cfg := &config.Config{}
	for _, eng := range engine.Known {
		t.Run(eng, func(t *testing.T) {
			c := store.SnapshotEvictionCandidate{Engine: eng, TemplateName: "_tm_x"}
			err := dropTemplate(context.Background(), cfg, c)
			if err == nil {
				t.Fatalf("nil cfg: expected an error, got none")
			}
			if strings.Contains(err.Error(), "unsupported engine") {
				t.Fatalf("engine %q falls into unsupported-engine default: %v", eng, err)
			}
			if !strings.Contains(err.Error(), "not configured") {
				t.Fatalf("engine %q: expected 'not configured' error, got: %v", eng, err)
			}
		})
	}
}

// TestDropTemplateRejectsUnknownEngine confirms that genuinely
// unknown engine strings still produce the loud "unsupported"
// error — the audit shouldn't mask real typos.
func TestDropTemplateRejectsUnknownEngine(t *testing.T) {
	cfg := &config.Config{}
	c := store.SnapshotEvictionCandidate{Engine: "sqlite", TemplateName: "_tm_x"}
	err := dropTemplate(context.Background(), cfg, c)
	if err == nil || !strings.Contains(err.Error(), "unsupported engine") {
		t.Fatalf("unknown engine: want 'unsupported engine' error, got %v", err)
	}
}
