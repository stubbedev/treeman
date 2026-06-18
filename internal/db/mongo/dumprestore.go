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
	"sync"

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

// mongoArchive is the once-per-template dump cache: a fan-out dumps the
// immutable template to an archive file once, then every clone restores
// from it. Exactly one of err / (path set) is meaningful after `once`.
type mongoArchive struct {
	once sync.Once
	path string
	err  error
}

// cloneViaDumpRestore copies `source` → `dest` using the in-container
// tools. For a fingerprint-named snapshot template (immutable) it dumps
// to a cached archive once and restores N clones from it; for any other
// source it runs a single mongodump|mongorestore pipe. Returns
// errCloneToolsUnavailable (wrapped) when preconditions aren't met so
// the caller falls back to $out without noise.
func (d *Driver) cloneViaDumpRestore(ctx context.Context, source, dest string) error {
	cid, engineBin, err := d.resolveContainer(ctx)
	if err != nil {
		return err
	}
	if !d.dumpToolsAvailable(ctx, engineBin, cid) {
		return fmt.Errorf("%w: mongodump/mongorestore not on PATH in %s", errCloneToolsUnavailable, cid)
	}
	uri := d.inContainerURI()

	if isSnapshotTemplate(source) {
		return d.cloneViaArchive(ctx, cid, engineBin, uri, source, dest)
	}

	// Non-template source (e.g. building the template itself): single
	// streamed pipe, no scratch file. --drop makes re-runs idempotent.
	script := fmt.Sprintf(
		"mongodump --uri=%s --db=%s --archive | mongorestore --uri=%s --archive --nsInclude=%s --nsFrom=%s --nsTo=%s --drop",
		shQuote(uri), shQuote(source), shQuote(uri),
		shQuote(source+".*"), shQuote(source+".*"), shQuote(dest+".*"),
	)
	return dockerExecSh(ctx, engineBin, cid, script,
		fmt.Sprintf("mongodump|mongorestore %s→%s", source, dest))
}

// cloneViaArchive dumps `source` to a cached archive once (guarded by
// sync.Once) then restores it into `dest`, remapping the source.*
// namespaces onto dest.*. The N-clone fan-out thus pays a single dump.
func (d *Driver) cloneViaArchive(ctx context.Context, cid, engineBin, uri, source, dest string) error {
	v, _ := d.archives.LoadOrStore(source, &mongoArchive{})
	a, _ := v.(*mongoArchive)
	a.once.Do(func() {
		path := mongoStageDir + "/" + source + ".archive"
		dump := fmt.Sprintf("mkdir -p %s && mongodump --uri=%s --db=%s --archive=%s",
			shQuote(mongoStageDir), shQuote(uri), shQuote(source), shQuote(path))
		if err := dockerExecSh(ctx, engineBin, cid, dump, "mongodump "+source); err != nil {
			a.err = err
			return
		}
		a.path = path
	})
	if a.err != nil {
		return a.err
	}
	restore := fmt.Sprintf(
		"mongorestore --uri=%s --archive=%s --nsInclude=%s --nsFrom=%s --nsTo=%s --drop",
		shQuote(uri), shQuote(a.path), shQuote(source+".*"), shQuote(source+".*"), shQuote(dest+".*"),
	)
	return dockerExecSh(ctx, engineBin, cid, restore,
		fmt.Sprintf("mongorestore %s→%s", source, dest))
}

// mongoStageDir is the in-container scratch dir for cached dump archives.
const mongoStageDir = "/tmp/_tm_mongo_stage"

// isSnapshotTemplate reports whether `name` is a fingerprint-keyed
// snapshot template — content-addressed and therefore immutable, so its
// dump archive is safe to cache and reuse across the fan-out.
func isSnapshotTemplate(name string) bool {
	return strings.HasPrefix(name, "_tm")
}

// dropArchive best-effort removes a template's cached dump archive and
// forgets the in-memory entry. Silent on every failure — the archive is
// a cache, not state.
func (d *Driver) dropArchive(ctx context.Context, template string) {
	d.archives.Delete(template)
	cid, engineBin, err := d.resolveContainer(ctx)
	if err != nil {
		return
	}
	_ = dockerExecSh(ctx, engineBin, cid,
		"rm -f "+shQuote(mongoStageDir+"/"+template+".archive"), "rm archive")
}

// dockerExecSh runs `<engine> exec <cid> sh -c <script>` and wraps any
// failure with `what` for context.
func dockerExecSh(ctx context.Context, engineBin, cid, script, what string) error {
	if out, err := exec.CommandContext(ctx, engineBin, "exec", cid, "sh", "-c", script).CombinedOutput(); err != nil {
		return fmt.Errorf("%s exec %s: %w (%s)", engineBin, what, err, strings.TrimSpace(string(out)))
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
