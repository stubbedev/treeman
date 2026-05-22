package resolve

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/stubbedev/treeman/internal/config"
)

// cacheEntry holds a previously-loaded resolved config plus the
// mtime fingerprints of every YAML file that contributed. A cache
// hit only fires when every file's mtime matches; any mismatch
// invalidates the entry and forces a reparse.
type cacheEntry struct {
	cfg    config.Config
	stamps map[string]int64
}

// configCache memoises LoadResolved / LoadResolvedForWorktree by
// (mainRoot, wtRoot). The daemon hits these paths from every RPC
// dispatch (finalize, teardown, watcher fire) — without the cache
// each call reparses three YAML files plus env-file resolution. For
// a host with 10 active worktrees and frequent watcher pings the
// reparse cost dominates daemon CPU.
//
// Callers are trusted not to mutate the returned config. The struct
// is returned by value but holds maps/slices internally; cache
// pollution would surface as cross-call drift. None of the current
// callers mutate, audited at the time this was written.
var (
	configCacheMu sync.RWMutex
	configCache   = map[string]*cacheEntry{}
)

func configCacheKey(mainRoot, wtRoot string) string {
	return mainRoot + "\x00" + wtRoot
}

// candidateYAMLFiles returns every YAML path that LoadLayered* may
// consume for the given (mainRoot, wtRoot). Order is irrelevant —
// the caller stats each and compares mtimes against the cached
// fingerprint. A file's absence is recorded as mtime 0; cache stays
// valid as long as the file remains absent.
func candidateYAMLFiles(mainRoot, wtRoot string) []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".config", "treeman", "config.yaml"))
	}
	if mainRoot != "" {
		out = append(out,
			filepath.Join(mainRoot, ".treeman.yaml"),
			filepath.Join(mainRoot, ".treeman.local.yaml"),
		)
	}
	if wtRoot != "" && wtRoot != mainRoot {
		out = append(out, filepath.Join(wtRoot, ".treeman.local.yaml"))
	}
	return out
}

// stampYAMLFiles returns the current mtime fingerprint for the
// candidate set. Missing files contribute 0 — that's a valid state
// that should not generate spurious cache misses.
func stampYAMLFiles(paths []string) map[string]int64 {
	stamps := make(map[string]int64, len(paths))
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			stamps[p] = fi.ModTime().UnixNano()
		} else {
			stamps[p] = 0
		}
	}
	return stamps
}

// stampsEqual reports whether two fingerprints describe the same
// on-disk state. Used as the cache-hit gate.
func stampsEqual(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// loadResolvedCached is the cache-fronted implementation shared by
// LoadResolved and LoadResolvedForWorktree. `loader` rebuilds from
// scratch on miss.
func loadResolvedCached(mainRoot, wtRoot string, loader func() (config.Config, error)) (config.Config, error) {
	key := configCacheKey(mainRoot, wtRoot)
	paths := candidateYAMLFiles(mainRoot, wtRoot)
	current := stampYAMLFiles(paths)

	configCacheMu.RLock()
	entry, ok := configCache[key]
	configCacheMu.RUnlock()
	if ok && stampsEqual(entry.stamps, current) {
		return entry.cfg, nil
	}

	cfg, err := loader()
	if err != nil {
		return cfg, err
	}
	configCacheMu.Lock()
	configCache[key] = &cacheEntry{cfg: cfg, stamps: current}
	configCacheMu.Unlock()
	return cfg, nil
}

// InvalidateConfigCache drops every cached resolved config. Tests
// and the config-write RPC call this so a config edit reflects on
// the next load instead of waiting for an mtime tick.
func InvalidateConfigCache() {
	configCacheMu.Lock()
	configCache = map[string]*cacheEntry{}
	configCacheMu.Unlock()
}
