package redis

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os/exec"
	"strconv"
	"strings"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/dumpload"
)

// Restore loads a RESP-encoded dump (the format `redis-cli --pipe`
// accepts) into the given target prefix. Strategy is selected in order:
//
//  1. `<container-engine> exec -i CID redis-cli --pipe …` when the
//     connection's ContainerRef resolves to a running container.
//  2. Host `redis-cli --pipe` on PATH, piping the RESP stream over
//     stdin.
//  3. Pure-Go wire-protocol fallback: parses the RESP stream itself
//     and pipelines each command via the go-redis client. Always
//     available — selected when neither CLI path is.
//
// Compression (gzip/zstd/bzip2/xz) is auto-detected from the dump's
// magic bytes.
//
// Unlike the ES NDJSON loader, redis dumps are passed through
// VERBATIM with NO `{target_db}` token substitution: RESP is a
// length-prefixed binary protocol, so changing key bytes would desync
// the `$<N>` length headers and corrupt the stream. If you need per-
// worktree key prefixes, embed them at dump-generation time.
//
// Returns the strategy that actually ran. Fast-path FAILURES (CLI ran
// but exited non-zero) are downgraded to a warn log + fall-through, so
// a dev box missing redis-cli can never block a cold build.
func (d *Driver) Restore(ctx context.Context, conn *config.RedisConn, targetPrefix, dumpPath string) (dumpload.LoadStrategy, error) {
	if ok, err := tryDockerExecRedisCLI(ctx, conn, targetPrefix, dumpPath); ok {
		return dumpload.StrategyDockerExec, nil
	} else if err != nil {
		slog.Warn("redis restore fast path (docker exec) failed; falling through", "error", err)
	}
	if ok, err := tryNativeCLIRedisCLI(ctx, conn, targetPrefix, dumpPath); ok {
		return dumpload.StrategyNativeCLI, nil
	} else if err != nil {
		slog.Warn("redis restore fast path (native CLI) failed; falling through", "error", err)
	}
	return dumpload.StrategyWire, d.restoreViaDriver(ctx, targetPrefix, dumpPath)
}

// tryDockerExecRedisCLI runs `<engine> exec -i CID redis-cli --pipe`,
// piping the (decompressed + substituted) RESP bytes over stdin.
func tryDockerExecRedisCLI(ctx context.Context, conn *config.RedisConn, targetPrefix, dumpPath string) (bool, error) {
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
		return false, nil //nolint:nilerr // container not resolvable; fall through
	}
	engineBin := opts.Engine
	if engineBin == "" {
		engineBin = "docker"
	}
	if _, err := exec.LookPath(engineBin); err != nil {
		return false, nil //nolint:nilerr // engine binary missing; fall through
	}
	body, err := readDump(dumpPath, targetPrefix)
	if err != nil {
		return false, err
	}
	args := []string{"exec", "-i", cid, "redis-cli", "--pipe"}
	if pw := passwordFromURL(conn.URL); pw != "" {
		args = append(args, "-a", pw)
	}
	cmd := exec.CommandContext(ctx, engineBin, args...)
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if rerr := cmd.Run(); rerr != nil {
		return false, fmt.Errorf("%s exec redis-cli --pipe: %w (stderr: %s)", engineBin, rerr, stderr.String())
	}
	return true, nil
}

// tryNativeCLIRedisCLI runs the host's `redis-cli --pipe` when present.
func tryNativeCLIRedisCLI(ctx context.Context, conn *config.RedisConn, targetPrefix, dumpPath string) (bool, error) {
	if conn == nil {
		return false, nil
	}
	if _, err := exec.LookPath("redis-cli"); err != nil {
		return false, nil //nolint:nilerr // CLI not on PATH; fall through to wire fallback
	}
	host, port := hostPortFromURL(conn.URL)
	args := []string{"-h", host, "-p", strconv.Itoa(port), "--pipe"}
	if pw := passwordFromURL(conn.URL); pw != "" {
		args = append(args, "-a", pw)
	}
	body, err := readDump(dumpPath, targetPrefix)
	if err != nil {
		return false, err
	}
	cmd := exec.CommandContext(ctx, "redis-cli", args...)
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if rerr := cmd.Run(); rerr != nil {
		return false, fmt.Errorf("native redis-cli --pipe: %w (stderr: %s)", rerr, stderr.String())
	}
	return true, nil
}

// restoreViaDriver is the pure-Go wire-protocol fallback. It parses
// the RESP stream itself and replays each command through the pooled
// go-redis client. Commands are batched into pipelines of 1000 to keep
// the round-trip cost down without unbounded memory growth.
func (d *Driver) restoreViaDriver(ctx context.Context, targetPrefix, dumpPath string) error {
	body, err := readDump(dumpPath, targetPrefix)
	if err != nil {
		return err
	}
	r := bufio.NewReader(bytes.NewReader(body))
	client := d.clientFor(-1)

	const batchSize = 1000
	pipe := client.Pipeline()
	pending := 0
	flush := func() error {
		if pending == 0 {
			return nil
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return fmt.Errorf("pipeline exec: %w", err)
		}
		pipe = client.Pipeline()
		pending = 0
		return nil
	}
	for {
		parts, perr := readRESPArray(r)
		if errors.Is(perr, io.EOF) {
			break
		}
		if perr != nil {
			return fmt.Errorf("parse RESP stream: %w", perr)
		}
		if len(parts) == 0 {
			continue
		}
		args := make([]any, len(parts))
		for i, p := range parts {
			args[i] = p
		}
		pipe.Do(ctx, args...)
		pending++
		if pending >= batchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// readDump opens dumpPath (auto-decompressed) and returns the raw RESP
// bytes verbatim. Unlike ES NDJSON, RESP is length-prefixed binary, so
// we deliberately do NOT substitute `{target_db}` tokens — doing so
// would desync the `$<N>` headers and corrupt the stream. Keys must be
// embedded at dump-generation time. The targetPrefix arg is retained
// for signature symmetry with the other engines' readers but ignored.
func readDump(dumpPath, _ string) ([]byte, error) {
	rc, _, err := dumpload.OpenDump(dumpPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	var raw bytes.Buffer
	if _, err := raw.ReadFrom(rc); err != nil {
		return nil, err
	}
	return raw.Bytes(), nil
}

// readRESPArray reads one `*N\r\n$L\r\nDATA\r\n…` array from r and
// returns the element bytes. Returns io.EOF cleanly at end of stream.
// Only the array+bulk-string forms used by `redis-cli --pipe` output
// are supported — inline / integer / simple-string / error replies
// don't appear in pipe input.
func readRESPArray(r *bufio.Reader) ([][]byte, error) {
	first, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if first != '*' {
		return nil, fmt.Errorf("expected '*' at array start, got %q", first)
	}
	nstr, err := r.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("array length line: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimRight(nstr, "\r\n"))
	if err != nil {
		return nil, fmt.Errorf("array length parse %q: %w", nstr, err)
	}
	if n < 0 {
		return nil, nil
	}
	parts := make([][]byte, n)
	for i := range n {
		head, herr := r.ReadByte()
		if herr != nil {
			return nil, herr
		}
		if head != '$' {
			return nil, fmt.Errorf("expected '$' at bulk-string start, got %q", head)
		}
		lstr, lerr := r.ReadString('\n')
		if lerr != nil {
			return nil, fmt.Errorf("bulk-string length line: %w", lerr)
		}
		l, perr := strconv.Atoi(strings.TrimRight(lstr, "\r\n"))
		if perr != nil {
			return nil, fmt.Errorf("bulk-string length parse %q: %w", lstr, perr)
		}
		if l < 0 {
			parts[i] = nil
			continue
		}
		buf := make([]byte, l)
		if _, rerr := io.ReadFull(r, buf); rerr != nil {
			return nil, fmt.Errorf("bulk-string body: %w", rerr)
		}
		// Consume trailing \r\n.
		var tail [2]byte
		if _, rerr := io.ReadFull(r, tail[:]); rerr != nil {
			return nil, fmt.Errorf("bulk-string trailer: %w", rerr)
		}
		parts[i] = buf
	}
	return parts, nil
}

// hostPortFromURL extracts host + port from a `redis://[:pw@]host:port[/db]`
// URL. Falls back to (127.0.0.1, 6379) on parse failure so the CLI can
// still attempt loopback.
func hostPortFromURL(rawURL string) (string, int) {
	if rawURL == "" {
		return "127.0.0.1", 6379
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "127.0.0.1", 6379
	}
	host := u.Hostname()
	if host == "" {
		host = "127.0.0.1"
	}
	port := 6379
	if p := u.Port(); p != "" {
		if v, err := strconv.Atoi(p); err == nil {
			port = v
		}
	}
	return host, port
}

// passwordFromURL extracts the password component of a redis URL, or
// "" when none is set.
func passwordFromURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.User == nil {
		return ""
	}
	pw, _ := u.User.Password()
	return pw
}
