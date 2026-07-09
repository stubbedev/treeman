package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/reachability"
	"github.com/stubbedev/treeman/internal/resolve"
)

// crashLoopUptime is the uptime under which a container that has already
// restarted (RestartCount > 0) reads as crash-looping. A healthy engine
// started once and stays up; one that restarted seconds ago is mid-loop.
// ponytail: single-snapshot heuristic (can't see "rising" from one
// inspect); a longer poll would catch a slower loop but needs daemon
// state — add that only if a real slow-loop slips through.
const crashLoopUptime = 60 * time.Second

// checkEngines probes every configured engine: a TCP reachability dial
// plus, for container-referenced engines, a `docker inspect` liveness
// read (running? restart count? uptime?). It exists because doctor was
// blind to a crash-looping mongod (RestartCount=536, dying every ~29s)
// while every other check reported ok — see issue #16. Aggregated into
// one result: worst per-engine outcome wins.
func checkEngines(ctx context.Context, repoRoot string) doctorResult {
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return doctorResult{Name: "engines", Status: "skip", Detail: "config not loadable: " + err.Error()}
	}
	probes := engineProbes(&cfg)
	if len(probes) == 0 {
		return doctorResult{Name: "engines", Status: "skip", Detail: "no engine connections configured"}
	}

	worst := "ok"
	lines := make([]string, 0, len(probes))
	var hints []string
	for _, p := range probes {
		status, line, hint := probeOneEngine(ctx, p)
		lines = append(lines, line)
		worst = worseStatus(worst, status)
		if hint != "" {
			hints = append(hints, hint)
		}
	}
	return doctorResult{
		Name:   "engines",
		Status: worst,
		Detail: strings.Join(lines, "; "),
		Hint:   strings.Join(hints, " "),
	}
}

// engineProbe describes how to reach one configured engine: its
// container ref (for the inspect) and a dial target (URL for URI-shaped
// engines, host/port for the SQL ones).
type engineProbe struct {
	name string // display + reachability engine label
	ref  config.ContainerRef
	url  string // probe via ProbeURLCtx when set
	host string // else probe host:port
	port uint16
}

func engineProbes(cfg *config.Config) []engineProbe {
	var out []engineProbe
	c := cfg.Connections
	if m := c.Mysql; m != nil {
		out = append(
			out,
			engineProbe{name: "mysql", ref: m.ContainerRef, host: orDefault(m.Host, "127.0.0.1"), port: orDefaultPort(m.Port, 3306)},
		)
	}
	if p := c.Postgres; p != nil {
		out = append(
			out,
			engineProbe{name: "postgres", ref: p.ContainerRef, host: orDefault(p.Host, "127.0.0.1"), port: orDefaultPort(p.Port, 5432)},
		)
	}
	if mg := c.Mongodb; mg != nil {
		out = append(out, engineProbe{name: "mongodb", ref: mg.ContainerRef, url: mg.URI})
	}
	if r := c.Redis; r != nil {
		out = append(out, engineProbe{name: "redis", ref: r.ContainerRef, url: r.URL})
	}
	if es := c.Elasticsearch; es != nil {
		out = append(out, engineProbe{name: "elasticsearch", ref: es.ContainerRef, url: es.URL})
	}
	if s := c.S3; s != nil {
		out = append(out, engineProbe{name: "s3", ref: s.ContainerRef, url: s.Endpoint})
	}
	return out
}

// probeOneEngine returns (status, detail-line, hint) for one engine.
// Container liveness takes priority (a dead/crash-looping container is
// the actionable root cause); TCP reachability is the fallback signal.
func probeOneEngine(ctx context.Context, p engineProbe) (status, line, hint string) {
	h, herr := containerip.ContainerHealth(ctx, containerip.Opts{
		Container:      p.ref.Container,
		ComposeService: p.ref.ComposeService,
		ComposeProject: p.ref.ComposeProject,
		Engine:         p.ref.ContainerEngine,
		Network:        p.ref.Network,
	})
	if herr == nil && h.Found {
		if !h.Running {
			return "fail", p.name + ": container not running",
				"start the " + p.name + " container (`docker start`) or check `docker logs`"
		}
		if h.RestartCount > 0 && h.Uptime > 0 && h.Uptime < crashLoopUptime {
			return "warn", fmt.Sprintf("%s: crash-looping (restarts=%d, up %s)", p.name, h.RestartCount, h.Uptime.Round(time.Second)),
				p.name + " keeps restarting — inspect `docker logs` for OOM/segfault before creating worktrees"
		}
	}

	if perr := probeReach(ctx, p); perr != nil {
		return "fail", fmt.Sprintf("%s: unreachable (%v)", p.name, perr), ""
	}

	detail := p.name + ": reachable"
	if herr == nil && h.Found && h.Running {
		detail = fmt.Sprintf("%s: up %s, restarts=%d", p.name, h.Uptime.Round(time.Second), h.RestartCount)
	}
	return "ok", detail, ""
}

func probeReach(ctx context.Context, p engineProbe) error {
	if p.url != "" {
		return reachability.ProbeURLCtx(ctx, p.name, p.url)
	}
	if p.port != 0 {
		return reachability.ProbeCtx(ctx, p.name, p.host, p.port)
	}
	return nil // nothing dialable (e.g. AWS S3 with no explicit endpoint)
}

// worseStatus returns the more-severe of two doctor statuses along the
// ok < warn < fail ordering (skip never appears here).
func worseStatus(a, b string) string {
	rank := map[string]int{"ok": 0, "warn": 1, "fail": 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func orDefaultPort(p, def uint16) uint16 {
	if p == 0 {
		return def
	}
	return p
}
