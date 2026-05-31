// Fast clone path for the MongoDB driver: when treeman has a
// ContainerRef for the mongo server AND mongodump/mongorestore are
// present in that container, a single in-container
// `mongodump … --archive | mongorestore … --archive --nsFrom/--nsTo`
// pipe copies a whole database server-side, no documents crossing the
// wire to the daemon. Falls back to the $out wire-protocol clone
// (cloneViaOut) whenever the container or the tools are unavailable, or
// the pipe fails — so behaviour is identical everywhere, just faster
// where the tools exist.

package mongo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/stubbedev/treeman/internal/db/containerip"
)

// errCloneToolsUnavailable signals that the dump/restore preconditions
// aren't met (no ContainerRef, container unresolvable, tools absent) so
// cloneDatabase falls back to $out silently rather than logging a warn.
var errCloneToolsUnavailable = errors.New("mongo dump/restore tools unavailable")

// cloneDatabase copies `source` → `dest`. Prefers the in-container
// mongodump|mongorestore pipe; falls back to the $out wire-protocol
// clone when the tools aren't usable or the pipe fails.
func (d *Driver) cloneDatabase(ctx context.Context, source, dest string) error {
	err := d.cloneViaDumpRestore(ctx, source, dest)
	if err == nil {
		return nil
	}
	if !errors.Is(err, errCloneToolsUnavailable) {
		// Tools were present but the pipe failed — fall back, but make
		// the failure visible since it's unexpected.
		slog.Warn("mongo dump/restore failed; falling back to $out clone",
			"source", source, "dest", dest, "error", err)
	}
	return d.cloneViaOut(ctx, source, dest)
}

// cloneViaDumpRestore runs one in-container mongodump|mongorestore pipe.
// Returns errCloneToolsUnavailable (wrapped) when the preconditions
// aren't met so the caller falls back without noise.
func (d *Driver) cloneViaDumpRestore(ctx context.Context, source, dest string) error {
	cid, engineBin, err := d.resolveContainer(ctx)
	if err != nil {
		return err
	}
	if !d.dumpToolsAvailable(ctx, engineBin, cid) {
		return fmt.Errorf("%w: mongodump/mongorestore not on PATH in %s", errCloneToolsUnavailable, cid)
	}

	uri := d.inContainerURI()
	// mongorestore --drop drops each destination collection before
	// restoring it, so a re-run is idempotent. --nsFrom/--nsTo remap the
	// dumped `source.*` namespaces onto `dest.*`. --archive with no path
	// streams over the pipe, so no scratch file is written.
	script := fmt.Sprintf(
		"mongodump --uri=%s --db=%s --archive | mongorestore --uri=%s --archive --nsInclude=%s --nsFrom=%s --nsTo=%s --drop",
		shQuote(uri), shQuote(source), shQuote(uri),
		shQuote(source+".*"), shQuote(source+".*"), shQuote(dest+".*"),
	)
	full := []string{"exec", cid, "sh", "-c", script}
	if out, cerr := exec.CommandContext(ctx, engineBin, full...).CombinedOutput(); cerr != nil {
		return fmt.Errorf("%s exec mongodump|mongorestore %s→%s: %w (%s)",
			engineBin, source, dest, cerr, strings.TrimSpace(string(out)))
	}
	return nil
}

// resolveContainer resolves the configured container id + engine binary.
// Returns errCloneToolsUnavailable when no ContainerRef is set or the
// container can't be resolved / the engine binary is missing.
func (d *Driver) resolveContainer(ctx context.Context) (cid, engineBin string, err error) {
	if d.cfg.Container == "" && d.cfg.ComposeService == "" {
		return "", "", fmt.Errorf("%w: no ContainerRef on connections.mongodb", errCloneToolsUnavailable)
	}
	opts := containerip.Opts{
		Container:      d.cfg.Container,
		ComposeService: d.cfg.ComposeService,
		ComposeProject: d.cfg.ComposeProject,
		Engine:         d.cfg.ContainerEngine,
		Network:        d.cfg.Network,
	}
	cid, cerr := containerip.ContainerID(ctx, opts)
	if cerr != nil {
		return "", "", fmt.Errorf("%w: container not resolvable: %w", errCloneToolsUnavailable, cerr)
	}
	engineBin = opts.Engine
	if engineBin == "" {
		engineBin = "docker"
	}
	if _, lerr := exec.LookPath(engineBin); lerr != nil {
		return "", "", fmt.Errorf("%w: %s binary not on PATH", errCloneToolsUnavailable, engineBin)
	}
	return cid, engineBin, nil
}

// dumpToolsAvailable reports (once) whether both mongodump and
// mongorestore are on PATH inside the container.
func (d *Driver) dumpToolsAvailable(ctx context.Context, engineBin, cid string) bool {
	d.dumpToolsOnce.Do(func() {
		cmd := exec.CommandContext(ctx, engineBin, "exec", cid, "sh", "-c",
			"command -v mongodump >/dev/null 2>&1 && command -v mongorestore >/dev/null 2>&1")
		d.dumpToolsOK = cmd.Run() == nil
	})
	return d.dumpToolsOK
}

// inContainerURI rewrites the configured URI to address the mongod from
// inside its own container: host 127.0.0.1, port = the URI's port (the
// internal port, by the ContainerRef convention). Credentials and other
// URI options are preserved.
func (d *Driver) inContainerURI() string {
	return containerip.RewriteHostPortInURIWithPort(d.cfg.URI, "127.0.0.1", containerip.URIPort(d.cfg.URI, 27017))
}

// shQuote single-quote-escapes a string for safe use inside `sh -c`.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
