package es

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/dumpload"
)

// DispatchRestore loads a `_bulk`-format NDJSON dump into `sourcePrefix`
// using the fastest available path. Strategy is selected in order:
//
//  1. `<container-engine> exec -i CID curl …` POSTing the bulk body to
//     the in-container ES (localhost:9200). Skips the host→container
//     network leg.
//  2. Wire-protocol: the host process POSTs to ES over HTTP via the
//     existing `Driver.Restore` (always available).
//
// There is no separate "native CLI" path for Elasticsearch: ES ships no
// widely-installed bulk-load CLI (`elasticdump` is third-party). The
// host's `curl` would post to the same TCP endpoint our Go HTTP client
// already uses, so we don't add it.
//
// Returns the strategy that actually ran. Fast-path FAILURES (exec ran
// but exited non-zero, or `errors: true` in the bulk response) are
// downgraded to a warn log + fall-through to the wire path, mirroring
// the MySQL/Postgres/Mongo dispatchers.
func (d *Driver) DispatchRestore(ctx context.Context, conn *config.EsConn, sourcePrefix, dumpPath string) (dumpload.LoadStrategy, error) {
	if ok, err := tryDockerExecESBulk(ctx, conn, sourcePrefix, dumpPath); ok {
		if rerr := d.refresh(ctx, sourcePrefix+"*"); rerr != nil {
			return "", fmt.Errorf("es refresh %s*: %w", sourcePrefix, rerr)
		}
		return dumpload.StrategyDockerExec, nil
	} else if err != nil {
		slog.Warn("es restore fast path (docker exec) failed; falling through", "error", err)
	}
	return dumpload.StrategyWire, d.Restore(ctx, sourcePrefix, dumpPath)
}

// tryDockerExecESBulk pipes the (decompressed, {target_db}-substituted)
// NDJSON body into curl inside the ES container. ES doesn't have a
// chunking concept at the curl level, so we slurp the whole substituted
// body into memory and POST it in one go — fine for the dev seed sizes
// treeman handles, and matches the existing host-side path's behaviour
// for ≤10 MiB inputs.
func tryDockerExecESBulk(ctx context.Context, conn *config.EsConn, sourcePrefix, dumpPath string) (bool, error) {
	if conn == nil || (conn.Container == "" && conn.ComposeService == "") {
		return false, nil
	}
	opts := containerip.Opts{
		Container:      conn.Container,
		ComposeService: conn.ComposeService,
		ComposeProject: conn.ComposeProject,
		Engine:         conn.ContainerEngine,
		Network:        conn.Network,
	}
	cid, cerr := containerip.ContainerID(ctx, opts)
	if cerr != nil {
		return false, nil //nolint:nilerr // container not resolvable; fall through to wire HTTP
	}
	engineBin := opts.Engine
	if engineBin == "" {
		engineBin = "docker"
	}
	if _, err := exec.LookPath(engineBin); err != nil {
		return false, nil //nolint:nilerr // engine binary missing; fall through
	}
	body, err := readBulkBody(dumpPath, sourcePrefix)
	if err != nil {
		return false, err
	}
	args := []string{
		"exec", "-i", cid,
		"curl", "-s", "-S", "-X", "POST",
		"-H", "Content-Type: application/x-ndjson",
		"--data-binary", "@-",
		"http://localhost:9200/_bulk",
	}
	cmd := exec.CommandContext(ctx, engineBin, args...)
	cmd.Stdin = bytes.NewReader(body)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if rerr := cmd.Run(); rerr != nil {
		return false, fmt.Errorf("%s exec curl _bulk: %w (stderr: %s)", engineBin, rerr, stderr.String())
	}
	// `curl -s -S` only writes to stderr on transport failures; a 4xx
	// from ES comes back as a non-zero exit only with `--fail`. We
	// don't pass `--fail` so we can read the per-item errors flag from
	// the response body — same check as the wire path.
	var hdr struct {
		Errors bool `json:"errors"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &hdr); err != nil {
		return false, fmt.Errorf("parse _bulk response: %w (body=%s)", err, truncate(stdout.String(), 256))
	}
	if hdr.Errors {
		return false, errors.New("_bulk reported per-item errors")
	}
	return true, nil
}

// readBulkBody opens the dump (auto-decompressed), substitutes every
// literal `{target_db}` token with sourcePrefix, and returns the
// resulting NDJSON bytes. Identical substitution logic to the wire
// path's per-line replace, just folded into one buffer.
func readBulkBody(dumpPath, sourcePrefix string) ([]byte, error) {
	rc, _, err := dumpload.OpenDump(dumpPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	var raw bytes.Buffer
	if _, err := raw.ReadFrom(rc); err != nil {
		return nil, err
	}
	return bytes.ReplaceAll(raw.Bytes(), []byte("{target_db}"), []byte(sourcePrefix)), nil
}
