package config

// ApplyMainWorktreeOverlay merges the `main_worktree.databases[]`
// overlay onto `databases[]`. Index-aligned; entries past the
// overlay's length are left as-is. Set fields on the overlay replace
// the matching field on the base; unset (zero / nil) fields inherit.
//
// The merge allocates a fresh `cfg.Databases` slice. resolve.cache
// hands out Config values that share their slice backing array — an
// in-place mutation here would poison every subsequent cache reader
// with this run's main-wt-specific values. Reslicing isolates the
// mutation to this caller.
//
// Caller is responsible for invoking this only when the finalize /
// prepare run targets the main worktree — applying it unconditionally
// would silently rewrite linked-wt DB names.
//
// The overlay shape is validated by Config.Validate at load time, so
// callers can assume `len(overlay) <= len(databases)` here. The
// loop guards against the inverse anyway: a hand-mutated Config
// (e.g. tests poking at fields directly) won't panic, it just skips
// out-of-bounds overlay entries.
func ApplyMainWorktreeOverlay(cfg *Config) {
	if cfg == nil {
		return
	}
	overlay := cfg.MainWorktree.Databases
	if len(overlay) == 0 || len(cfg.Databases) == 0 {
		return
	}
	// Fresh backing array so any cached Config sharing the previous
	// slice stays untouched. Only the entries the overlay covers need
	// copying — past that, we reuse the original DatabaseConfig
	// values (still by value, but each value's own nested pointers
	// like Migrate / Seed are unmodified so aliasing them is safe).
	out := make([]DatabaseConfig, len(cfg.Databases))
	copy(out, cfg.Databases)
	for i, ov := range overlay {
		if i >= len(out) {
			break
		}
		if ov.NameTemplate != "" {
			out[i].NameTemplate = ov.NameTemplate
		}
		if ov.KeyPrefix != "" {
			out[i].KeyPrefix = ov.KeyPrefix
		}
		if ov.TestClones != nil {
			// Full-spec replacement: the overlay either keeps fanout
			// shaped exactly like the base or completely redefines it.
			// Partial sub-field merge would create surprising
			// half-overrides (NameTemplate from base, Clones from
			// overlay, both fragile to reorder).
			tc := *ov.TestClones
			out[i].TestClones = &tc
		}
		if ov.Fanout != nil {
			out[i].Fanout = *ov.Fanout
		}
	}
	cfg.Databases = out
}
