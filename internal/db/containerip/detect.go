// detect.go — answers "are we running inside a container?" and
// "is the container engine actually usable from here?".
package containerip

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
)

var (
	insideOnce   sync.Once
	insideResult bool

	engineOnce sync.Map // map[engine]bool
)

// InsideContainer returns true when treeman itself is running
// inside a container (devcontainer, plain docker, podman, etc.).
// Result is cached for the process lifetime.
//
// Detection probes the standard markers in order of reliability:
//   - /.dockerenv (docker / nerdctl / finch)
//   - /run/.containerenv (podman)
//   - REMOTE_CONTAINERS=true (VS Code devcontainer feature)
//   - DEVCONTAINER=true (devcontainer.json env injection)
//   - /proc/1/cgroup contains "docker"/"containerd"/"libpod"
func InsideContainer() bool {
	insideOnce.Do(func() {
		insideResult = detectInsideContainer()
	})
	return insideResult
}

func detectInsideContainer() bool {
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	if os.Getenv("REMOTE_CONTAINERS") == "true" || os.Getenv("DEVCONTAINER") == "true" {
		return true
	}
	if b, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(b)
		for _, m := range []string{"docker", "containerd", "libpod", "kubepods"} {
			if strings.Contains(s, m) {
				return true
			}
		}
	}
	return false
}

// engineSocketReachable returns true when the configured engine
// binary exists and `<engine> info` succeeds — meaning the engine
// socket (or remote API) is wired up. Result is cached.
//
// Used to gate inspect-based lookups so a devcontainer with no
// docker socket mounted doesn't spend 30s timing out per
// connection attempt.
func engineSocketReachable(engine string) bool {
	if v, ok := engineOnce.Load(engine); ok {
		cached, _ := v.(bool)
		return cached
	}
	ok := func() bool {
		if _, err := exec.LookPath(engine); err != nil {
			return false
		}
		// `info` is cheap and proves the daemon socket answers.
		// Output discarded — exit code is the signal.
		return exec.CommandContext(context.Background(), engine, "info", "--format", "ok").Run() == nil
	}()
	engineOnce.Store(engine, ok)
	return ok
}
