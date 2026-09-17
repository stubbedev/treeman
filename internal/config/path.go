package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
)

var invocationConfigPath atomic.Pointer[string]

func ConfigureGlobalPath(path string) (func(), error) {
	if path == "" {
		path = os.Getenv("TREEMAN_CONFIG")
	}
	if path != "" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("config path %q: %w", path, err)
		}
		path = absolute
	}
	previous := invocationConfigPath.Swap(&path)
	restore := func() { invocationConfigPath.Store(previous) }
	if path != "" {
		if _, err := LoadGlobal(); err != nil {
			restore()
			return nil, err
		}
	}
	return restore, nil
}

func explicitGlobalConfigPath() string {
	if path := invocationConfigPath.Load(); path != nil {
		return *path
	}
	return os.Getenv("TREEMAN_CONFIG")
}
