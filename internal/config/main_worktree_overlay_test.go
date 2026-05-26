package config

import (
	"strings"
	"testing"
)

func uint32Ptr(v uint32) *uint32 { return &v }

func TestApplyMainWorktreeOverlayEmptyIsNoOp(t *testing.T) {
	cfg := &Config{
		Databases: []DatabaseConfig{{
			Engine:       "mysql",
			NameTemplate: "app_{slug}",
			TestClones:   &TestClonesSpec{Clones: ClonesSetting{Fixed: 8}, NameTemplate: "app_{slug}_t{n}"},
		}},
	}
	ApplyMainWorktreeOverlay(cfg)
	if cfg.Databases[0].NameTemplate != "app_{slug}" {
		t.Errorf("name template mutated by empty overlay: %q", cfg.Databases[0].NameTemplate)
	}
	if cfg.Databases[0].TestClones.Clones.Fixed != 8 {
		t.Errorf("test clones mutated by empty overlay: %+v", cfg.Databases[0].TestClones)
	}
}

func TestApplyMainWorktreeOverlayOverridesNameTemplate(t *testing.T) {
	cfg := &Config{
		Databases: []DatabaseConfig{{
			Engine:       "mysql",
			NameTemplate: "app_{slug}",
		}},
		MainWorktree: MainWorktreeConfig{
			Databases: []DatabaseOverlay{{NameTemplate: "app_dev_{slug}"}},
		},
	}
	ApplyMainWorktreeOverlay(cfg)
	if cfg.Databases[0].NameTemplate != "app_dev_{slug}" {
		t.Errorf("expected overridden template, got %q", cfg.Databases[0].NameTemplate)
	}
}

func TestApplyMainWorktreeOverlayDisablesFanout(t *testing.T) {
	cfg := &Config{
		Databases: []DatabaseConfig{{
			Engine:       "mysql",
			NameTemplate: "app_{slug}",
			TestClones:   &TestClonesSpec{Clones: ClonesSetting{Fixed: 8}, NameTemplate: "app_{slug}_t{n}"},
		}},
		MainWorktree: MainWorktreeConfig{
			Databases: []DatabaseOverlay{{
				TestClones: &TestClonesSpec{Clones: ClonesSetting{Fixed: 0}, NameTemplate: "ignored"},
			}},
		},
	}
	ApplyMainWorktreeOverlay(cfg)
	if cfg.Databases[0].TestClones == nil {
		t.Fatal("test clones unexpectedly nil")
	}
	if cfg.Databases[0].TestClones.Clones.Fixed != 0 {
		t.Errorf("expected clones=0, got %d", cfg.Databases[0].TestClones.Clones.Fixed)
	}
}

func TestApplyMainWorktreeOverlaySparseLeavesUntouched(t *testing.T) {
	cfg := &Config{
		Databases: []DatabaseConfig{
			{Engine: "mysql", NameTemplate: "first_{slug}"},
			{Engine: "postgres", NameTemplate: "second_{slug}"},
		},
		MainWorktree: MainWorktreeConfig{
			Databases: []DatabaseOverlay{{NameTemplate: "first_dev_{slug}"}},
		},
	}
	ApplyMainWorktreeOverlay(cfg)
	if cfg.Databases[0].NameTemplate != "first_dev_{slug}" {
		t.Errorf("index 0 not overridden: %q", cfg.Databases[0].NameTemplate)
	}
	if cfg.Databases[1].NameTemplate != "second_{slug}" {
		t.Errorf("index 1 should be untouched, got %q", cfg.Databases[1].NameTemplate)
	}
}

func TestApplyMainWorktreeOverlayFanoutZeroIsExplicit(t *testing.T) {
	cfg := &Config{
		Databases: []DatabaseConfig{{
			Engine:       "mysql",
			NameTemplate: "app_{slug}",
			Fanout:       16,
		}},
		MainWorktree: MainWorktreeConfig{
			Databases: []DatabaseOverlay{{Fanout: uint32Ptr(0)}},
		},
	}
	ApplyMainWorktreeOverlay(cfg)
	if cfg.Databases[0].Fanout != 0 {
		t.Errorf("expected explicit Fanout=0 to override, got %d", cfg.Databases[0].Fanout)
	}
}

func TestApplyMainWorktreeOverlayNilCfgSafe(t *testing.T) {
	ApplyMainWorktreeOverlay(nil) // must not panic.
}

// TestApplyMainWorktreeOverlayDoesNotMutateSharedBacking simulates the
// resolve.cache pattern: a single backing array shared across two
// Config values. The overlay must NOT mutate the original; otherwise
// every cached Config in the daemon would see the main-wt-specific
// templates after one finalize call.
func TestApplyMainWorktreeOverlayDoesNotMutateSharedBacking(t *testing.T) {
	base := []DatabaseConfig{{Engine: "mysql", NameTemplate: "app_{slug}"}}
	cachedCfg := Config{Databases: base}

	// Mirror what loadResolvedCached does: callers receive a value
	// copy of the Config. The slice header is fresh but the backing
	// array is shared.
	mutated := cachedCfg
	mutated.MainWorktree = MainWorktreeConfig{
		Databases: []DatabaseOverlay{{NameTemplate: "app_dev_{slug}"}},
	}
	ApplyMainWorktreeOverlay(&mutated)

	if cachedCfg.Databases[0].NameTemplate != "app_{slug}" {
		t.Errorf("shared backing array poisoned: cached cfg now reads %q",
			cachedCfg.Databases[0].NameTemplate)
	}
	if mutated.Databases[0].NameTemplate != "app_dev_{slug}" {
		t.Errorf("overlay didn't land on the local copy: %q",
			mutated.Databases[0].NameTemplate)
	}
}

func TestValidateRejectsOverlayLongerThanDatabases(t *testing.T) {
	cfg := &Config{
		Databases: []DatabaseConfig{
			{Engine: "mysql", NameTemplate: "app_{slug}"},
		},
		MainWorktree: MainWorktreeConfig{
			Databases: []DatabaseOverlay{{}, {}},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for overlay longer than databases")
	}
	if !strings.Contains(err.Error(), "main_worktree.databases") {
		t.Errorf("error doesn't mention main_worktree.databases: %v", err)
	}
}

func TestValidateOverlayTemplate(t *testing.T) {
	cfg := &Config{
		Databases: []DatabaseConfig{
			{Engine: "mysql", NameTemplate: "app_{slug}"},
		},
		MainWorktree: MainWorktreeConfig{
			Databases: []DatabaseOverlay{
				{NameTemplate: "app_{slag}"}, // typo'd placeholder
			},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for bad overlay template")
	}
}
